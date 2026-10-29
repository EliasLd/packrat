// Package engine wires parsed links through host resolution into concurrent
// downloads: resolve every part via the host registry, then hand the direct
// links to the download client. Progress is reported as events through an
// Observer; the engine never renders.
package engine

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EliasLd/packrat/internal/download"
	"github.com/EliasLd/packrat/internal/event"
	"github.com/EliasLd/packrat/internal/host"
	"github.com/EliasLd/packrat/internal/link"
)

// Options configures an Engine. Zero values fall back to the defaults
// documented per field.
type Options struct {
	OutDir              string         // output root directory (default "downloads")
	ResolveConcurrency  int            // parallel landing-page resolutions (default 3)
	DownloadConcurrency int            // parallel downloads (default 8)
	Retries             int            // attempts per part (default 5)
	UserAgent           string         // sent with download requests (empty = Go default)
	Observe             event.Observer // progress events; nil emits nothing
}

// Engine coordinates the whole pipeline for one run.
type Engine struct {
	registry *host.Registry
	opt      Options
	observe  event.Observer
}

// New builds an Engine; a nil Observer emits nothing.
func New(registry *host.Registry, opt Options) *Engine {
	if opt.OutDir == "" {
		opt.OutDir = "downloads"
	}
	if opt.ResolveConcurrency <= 0 {
		opt.ResolveConcurrency = 3
	}
	if opt.DownloadConcurrency <= 0 {
		opt.DownloadConcurrency = 8
	}
	if opt.Retries <= 0 {
		opt.Retries = 5
	}
	if opt.Observe == nil {
		opt.Observe = event.Nop
	}
	return &Engine{registry: registry, opt: opt, observe: opt.Observe}
}

// Run resolves every part and downloads it. It returns a non-nil error when
// at least one part failed; individual failures were already reported as
// events.
func (e *Engine) Run(ctx context.Context, groups []link.Group) error {
	start := time.Now()

	var parts []*link.Part
	for _, g := range groups {
		parts = append(parts, g.Parts...)
	}
	if len(parts) == 0 {
		e.observe(event.Event{Kind: event.RunDone, Stats: &event.Stats{}})
		return nil
	}
	e.observe(event.Event{Kind: event.RunStart,
		Stats: &event.Stats{PartsTotal: len(parts), GroupsTotal: len(groups)}})

	targets, resolveFails := e.resolveAll(ctx, parts)

	if ctx.Err() != nil {
		e.observe(event.Event{Kind: event.RunDone, Stats: finalStats(event.Stats{PartsTotal: len(parts), GroupsTotal: len(groups),
			Failed: int(resolveFails.Load())}, start)})
		return fmt.Errorf("%w: %s", context.Canceled, "resolve phase interrupted")
	}
	if len(targets) == 0 {
		e.observe(event.Event{Kind: event.RunDone, Stats: finalStats(event.Stats{PartsTotal: len(parts), GroupsTotal: len(groups),
			Failed: int(resolveFails.Load())}, start)})
		return fmt.Errorf("%d part(s) failed to resolve, nothing to download", resolveFails.Load())
	}

	downloader := download.New(download.Options{
		OutDir:    e.opt.OutDir,
		Workers:   e.opt.DownloadConcurrency,
		Retries:   e.opt.Retries,
		UserAgent: e.opt.UserAgent,
		Relink:    e.relink,
		Observe:   e.observe,
	})
	summary := downloader.Run(ctx, targets)

	stats := event.Stats{
		PartsTotal:  len(parts),
		GroupsTotal: len(groups),
		Done:        summary.Done,
		Skipped:     summary.Skipped,
		Failed:      len(summary.Failures) + int(resolveFails.Load()),
		Bytes:       summary.Bytes,
	}
	e.observe(event.Event{Kind: event.RunDone, Stats: finalStats(stats, start)})

	if stats.Failed > 0 {
		return fmt.Errorf("%d part(s) failed", stats.Failed)
	}
	return nil
}

// finalStats fills the timing fields shared by every RunDone event.
func finalStats(s event.Stats, start time.Time) *event.Stats {
	s.Elapsed = time.Since(start)
	return &s
}

// resolveAll turns every part into a download target, running the host
// registry with bounded concurrency. Parts that fail are counted and
// reported as ResolveFail events; the rest still download.
func (e *Engine) resolveAll(ctx context.Context, parts []*link.Part) ([]download.Target, *atomic.Int64) {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		targets []download.Target
		fails   atomic.Int64
	)

	sem := make(chan struct{}, e.opt.ResolveConcurrency)
	for _, p := range parts {
		wg.Add(1)
		go func(p *link.Part) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			pageURL, err := url.Parse(p.PageURL)
			if err != nil {
				fails.Add(1)
				e.observe(event.Event{Kind: event.ResolveFail, Part: p.ID, Err: err})
				return
			}
			direct, err := e.resolve(ctx, pageURL)
			if err != nil {
				fails.Add(1)
				e.observe(event.Event{Kind: event.ResolveFail, Part: p.ID, Err: err})
				return
			}
			name := e.finalName(p, direct.Filename)
			mu.Lock()
			targets = append(targets, download.Target{
				URL:      direct.URL,
				PageURL:  pageURL,
				Part:     p.ID,
				DirName:  p.DirName,
				FileName: name,
			})
			mu.Unlock()
			e.observe(event.Event{Kind: event.ResolveDone, Group: p.DirName, Part: p.ID, Filename: name})
		}(p)
	}
	wg.Wait()
	return targets, &fails
}

// resolve funnels one parsed landing page through the host registry.
func (e *Engine) resolve(ctx context.Context, pageURL *url.URL) (host.DirectLink, error) {
	resolvedHost, err := e.registry.Lookup(pageURL)
	if err != nil {
		return host.DirectLink{}, err
	}
	return resolvedHost.Resolve(ctx, pageURL)
}

// finalName picks the file name for a part: the archive fragment when the
// fragment was present, the host-provided name otherwise.
func (e *Engine) finalName(p *link.Part, hostProvided string) string {
	if p.Name == "" && hostProvided != "" {
		return link.Sanitize(hostProvided)
	}
	return p.FileName
}

// relink re-resolves a target whose direct link expired; wired into the
// download client as recovery hook.
func (e *Engine) relink(ctx context.Context, t download.Target) (*url.URL, error) {
	resolvedHost, err := e.registry.Lookup(t.PageURL)
	if err != nil {
		return nil, err
	}
	direct, err := resolvedHost.Resolve(ctx, t.PageURL)
	if err != nil {
		return nil, err
	}
	return direct.URL, nil
}
