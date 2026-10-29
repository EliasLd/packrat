// Package download fetches direct download URLs into files under an output
// root, with worker-pool concurrency, HTTP range resume and retries.
//
// Streaming writes go to "<name>.part" temporary files that are atomically
// renamed once the size reported by the server is reached, so an interrupted
// run can be resumed and a present final file is always complete.
package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EliasLd/packrat/internal/event"
)

// ErrLinkExpired reports a 401/403/410 from the download server: the direct
// URL most likely has a limited lifetime or single use. Wire a Relinker to
// have such targets re-resolved automatically.
var ErrLinkExpired = errors.New("direct link expired or refused")

// Target is one file to download into <OutDir>/<DirName>/<FileName>.
type Target struct {
	URL      *url.URL // direct download URL
	PageURL  *url.URL // landing page it was resolved from, for Relink
	Part     string   // stable identity on the event stream (engine-owned)
	DirName  string   // archive group the part belongs to
	FileName string
}

// Relinker re-resolves a target whose direct URL was refused.
type Relinker func(ctx context.Context, t Target) (*url.URL, error)

// Options configures a Downloader. Zero values fall back to the defaults
// documented per field.
type Options struct {
	OutDir    string         // output root directory
	Workers   int            // parallel downloads (default 8)
	Retries   int            // attempts per target (default 5)
	UserAgent string         // sent with download requests; empty sends Go's default
	Relink    Relinker       // optional recovery hook for expired links
	Observe   event.Observer // progress events; nil emits nothing
}

// Failure records one target that could not be downloaded.
type Failure struct {
	Target Target
	Err    error
}

// Summary reports the outcome of a Run.
type Summary struct {
	Total    int
	Done     int
	Skipped  int
	Bytes    int64
	Failures []Failure
}

type Downloader struct {
	opt     Options
	client  *http.Client
	bytes   atomic.Int64
	observe event.Observer
}

// New builds a Downloader with defaults applied for zero option values.
func New(opt Options) *Downloader {
	if opt.Workers <= 0 {
		opt.Workers = 8
	}
	if opt.Retries <= 0 {
		opt.Retries = 5
	}
	if opt.Observe == nil {
		opt.Observe = event.Nop
	}
	return &Downloader{opt: opt, client: &http.Client{}, observe: opt.Observe}
}

// Run downloads all targets with the configured worker pool. A canceled
// context stops new attempts; finished files are kept and partial ones stay
// as "<name>.part" so a rerun can resume where it stopped.
func (d *Downloader) Run(ctx context.Context, targets []Target) Summary {
	summary := Summary{Total: len(targets)}

	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		doneCount    atomic.Int64
		skippedCount atomic.Int64
	)
	record := func(t Target, err error) {
		mu.Lock()
		summary.Failures = append(summary.Failures, Failure{Target: t, Err: err})
		mu.Unlock()
		fail := eventFor(event.DownloadFail, t)
		fail.Err = err
		d.observe(fail)
	}

	sem := make(chan struct{}, d.opt.Workers)
	for _, t := range targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				record(t, fmt.Errorf("interrupted: %w", ctx.Err()))
				return
			}
			defer func() { <-sem }()
			skipped, err := d.one(ctx, t)
			switch {
			case err != nil:
				record(t, err)
			case skipped:
				skippedCount.Add(1)
			default:
				doneCount.Add(1)
			}
		}(t)
	}
	wg.Wait()
	summary.Done = int(doneCount.Load())
	summary.Skipped = int(skippedCount.Load())
	summary.Bytes = d.bytes.Load()
	return summary
}

// one downloads a single target with retries. skipped is true when the
// target file already existed.
func (d *Downloader) one(ctx context.Context, t Target) (bool, error) {
	target := filepath.Join(d.opt.OutDir, t.DirName, t.FileName)
	temp := target + ".part"
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return false, err
	}
	if fi, err := os.Stat(target); err == nil && fi.Size() > 0 {
		skip := eventFor(event.DownloadSkipped, t)
		skip.Size = fi.Size()
		d.observe(skip)
		return true, nil
	}
	resumeAt := fileSize(temp)
	start := eventFor(event.DownloadStart, t)
	start.ResumeAt = resumeAt
	d.observe(start)

	pace := &progressPacer{observe: d.observe, group: t.DirName, part: t.Part}

	var lastErr error
	for attempt := 1; attempt <= d.opt.Retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("interrupted: %w", err)
		}
		written, total, err := d.stream(ctx, &t, temp, pace)
		if err == nil && written >= total {
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
			if err := os.Rename(temp, target); err != nil {
				return false, err
			}
			done := eventFor(event.DownloadDone, t)
			done.Size = total
			d.observe(done)
			return false, nil
		}
		if err != nil {
			lastErr = err
			if errors.Is(err, ErrLinkExpired) && d.opt.Relink != nil {
				if fresh, relinkErr := d.opt.Relink(ctx, t); relinkErr == nil {
					t.URL = fresh
				} else {
					lastErr = fmt.Errorf("re-resolve link: %w", relinkErr)
				}
			}
		} else {
			lastErr = fmt.Errorf("short read (%s/%s)", event.HumanBytes(written), event.HumanBytes(total))
		}
		if attempt < d.opt.Retries {
			retry := eventFor(event.DownloadRetry, t)
			retry.Attempt, retry.Err = attempt, lastErr
			d.observe(retry)
		}
		if waitErr := sleepContext(ctx, time.Duration(attempt)*2*time.Second); waitErr != nil {
			return false, fmt.Errorf("interrupted: %w", waitErr)
		}
	}
	return false, fmt.Errorf("giving up after %d attempt(s): %w", d.opt.Retries, lastErr)
}

// stream performs one ranged HTTP fetch, appending to temp. It returns the
// bytes now present in temp and the total size reported by the server.
// A nil error means temp holds the complete file.
func (d *Downloader) stream(ctx context.Context, t *Target, temp string, pace *progressPacer) (written int64, total int64, err error) {
	writtenPrior := fileSize(temp)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL.String(), nil)
	if err != nil {
		return 0, 0, err
	}
	if writtenPrior > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", writtenPrior))
	}
	if d.opt.UserAgent != "" {
		req.Header.Set("User-Agent", d.opt.UserAgent)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK: // server ignored Range: restart from scratch
		if writtenPrior > 0 {
			if err := os.Remove(temp); err != nil {
				return 0, 0, err
			}
			writtenPrior = 0
		}
		total = resp.ContentLength
	case http.StatusPartialContent:
		var ok bool
		total, ok = parseContentRangeTotal(resp.Header.Get("Content-Range"))
		if !ok {
			return 0, 0, errors.New("206 response with unparseable Content-Range")
		}
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusGone:
		return 0, 0, fmt.Errorf("%w (status %d)", ErrLinkExpired, resp.StatusCode)
	default:
		return 0, 0, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	if total <= 0 {
		return 0, 0, errors.New("download server reported no size")
	}
	if writtenPrior >= total {
		return writtenPrior, total, nil // temp already complete
	}

	f, err := os.OpenFile(temp, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()

	// stall watchdog: when nothing arrives for 60s, kill the body so the
	// stream fails and the retry loop can resume from the current offset.
	var lastAct atomic.Int64
	lastAct.Store(time.Now().Unix())
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				_ = resp.Body.Close()
				return
			case <-ticker.C:
				if time.Since(time.Unix(lastAct.Load(), 0)) > 60*time.Second {
					_ = resp.Body.Close()
					return
				}
			}
		}
	}()

	buf := make([]byte, 512<<10)
	var n int64
	for {
		read, rerr := resp.Body.Read(buf)
		if read > 0 {
			if _, werr := f.Write(buf[:read]); werr != nil {
				return writtenPrior, total, werr
			}
			n += int64(read)
			d.bytes.Add(int64(read))
			pace.send(writtenPrior+n, total, time.Now())
			lastAct.Store(time.Now().Unix())
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return writtenPrior + n, total, nil // clean end of stream
			}
			return writtenPrior + n, total, rerr
		}
		if ctx.Err() != nil {
			return writtenPrior + n, total, ctx.Err()
		}
	}
}

// eventFor seeds a download event with the identity fields of its target.
func eventFor(kind event.Kind, t Target) event.Event {
	return event.Event{Kind: kind, Group: t.DirName, Part: t.Part, Filename: t.FileName}
}

// progressPacer emits at most one Progress event per target: every 100 ms or
// on a 1% size step, whichever comes first. Progress events are display
// hints, so pacing exists only to keep the event stream cheap.
type progressPacer struct {
	observe  event.Observer
	group    string
	part     string
	lastSent time.Time
	lastDone int64
}

func (p *progressPacer) send(done, total int64, now time.Time) {
	if p.observe == nil || total <= 0 {
		return
	}
	if now.Sub(p.lastSent) < 100*time.Millisecond && done-p.lastDone < total/100 {
		return
	}
	p.lastSent, p.lastDone = now, done
	p.observe(event.Event{Kind: event.Progress, Group: p.group, Part: p.part, Done: done, Total: total})
}

func parseContentRangeTotal(contentRange string) (int64, bool) {
	i := strings.LastIndex(contentRange, "/")
	if i < 0 {
		return 0, false
	}
	var total int64
	if _, err := fmt.Sscanf(contentRange[i+1:], "%d", &total); err != nil || total <= 0 {
		return 0, false
	}
	return total, true
}

func fileSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

func sleepContext(ctx context.Context, d time.Duration) error {
	jitter := time.Duration(rand.Int63n(int64(d/4 + 1)))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d + jitter):
		return nil
	}
}
