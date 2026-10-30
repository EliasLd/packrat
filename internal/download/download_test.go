package download_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliasLd/packrat/internal/download"
	"github.com/EliasLd/packrat/internal/event"
)

// archive holds test file content keyed by id.
var archive = map[string][]byte{
	"one":   []byte("content-of-file-one-and-some-more-bytes-\r\n\t"),
	"two":   []byte("another-file-content-with-a-different-length"),
	"fresh": []byte("the-relink-successor-content!"),
}

// collector records events for assertions; emitters call it from concurrent
// goroutines, so it is locked.
type collector struct {
	mu     sync.Mutex
	events []event.Event
}

func (c *collector) observe(e event.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

// of gives the events of one kind, in arrival order.
func (c *collector) of(kind event.Kind) []event.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []event.Event
	for _, e := range c.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func serveFiles(t *testing.T, requests *int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests++
		id := strings.TrimPrefix(r.URL.Path, "/")
		content, ok := archive[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, id, time.Time{}, strings.NewReader(string(content)))
	}))
	t.Cleanup(server.Close)
	return server
}

// truncateHandler serves partial bytes then aborts the connection on every
// request that has not reached the resume threshold yet.
func truncateHandler(t *testing.T, requests *int, resumeThreshold int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests++
		content := archive["one"]
		start := 0
		if r.Header.Get("Range") != "" {
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start); err != nil {
				t.Errorf("bad range request: %v", err)
				return
			}
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
			w.WriteHeader(http.StatusPartialContent)
			if start < resumeThreshold {
				_, _ = w.Write(content[start:resumeThreshold])
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				panic(http.ErrAbortHandler) // force an unexpected EOF client-side
			}
			_, _ = w.Write(content[start:])
			return
		}
		// fresh request: promise the full size but abort after the threshold,
		// forcing the caller into the resumed-retry path
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		_, _ = w.Write(content[:resumeThreshold])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(server.Close)
	return server
}

func target(serverURL, id string) download.Target {
	u, err := url.Parse(serverURL + "/" + id)
	if err != nil {
		panic(err)
	}
	return download.Target{
		URL:      u,
		PageURL:  u,
		Part:     id,
		DirName:  "archive",
		FileName: id + ".bin",
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func finalPath(dir string, t download.Target) string {
	return filepath.Join(dir, t.DirName, t.FileName)
}

func partPath(dir string, t download.Target) string {
	return finalPath(dir, t) + ".part"
}

func TestDownload(t *testing.T) {
	tests := []struct {
		name        string
		id          string
		setup       func(t *testing.T, dir string)
		want        []byte
		skip        bool  // expected to be skipped without a request
		startResume int64 // expected DownloadStart resume offset
	}{
		{
			name: "downloads content into the target directory",
			id:   "one",
			want: archive["one"],
		},
		{
			name: "skips an already downloaded final file",
			id:   "one",
			skip: true,
			setup: func(t *testing.T, dir string) {
				path := filepath.Join(dir, "archive", "one.bin")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, archive["one"], 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: archive["one"],
		},
		{
			name:        "resumes from a partial temp file",
			id:          "two",
			startResume: 5,
			setup: func(t *testing.T, dir string) {
				path := filepath.Join(dir, "archive", "two.bin.part")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, archive["two"][:5], 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: archive["two"],
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests int
			server := serveFiles(t, &requests)
			dir := t.TempDir()
			if tt.setup != nil {
				tt.setup(t, dir)
			}

			tgt := target(server.URL, tt.id)
			col := &collector{}
			summary := download.New(download.Options{OutDir: dir, Observe: col.observe}).
				Run(context.Background(), []download.Target{tgt})

			if len(summary.Failures) != 0 {
				t.Fatalf("unexpected failure: %v", summary.Failures[0].Err)
			}
			got := readFile(t, filepath.Join(dir, tgt.DirName, tgt.FileName))
			if string(got) != string(tt.want) {
				t.Fatalf("file content = %q, want %q", got, tt.want)
			}
			if _, err := os.Stat(partPath(dir, tgt)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temp file should be gone, stat error: %v", err)
			}

			// event contract: lifecycle kinds, no retries or failures
			dones, skips, starts := col.of(event.DownloadDone), col.of(event.DownloadSkipped), col.of(event.DownloadStart)
			if tt.skip {
				if len(skips) != 1 || len(starts) != 0 || len(dones) != 0 {
					t.Fatalf("skip case events: done=%d skipped=%d started=%d", len(dones), len(skips), len(starts))
				}
			} else {
				if len(starts) != 1 || starts[0].ResumeAt != tt.startResume {
					t.Fatalf("start events = %+v, want one with resume offset %d", starts, tt.startResume)
				}
				if len(dones) != 1 || dones[0].Size != int64(len(tt.want)) {
					t.Fatalf("done events = %+v, want one with size %d", dones, len(tt.want))
				}
				if len(col.of(event.Progress)) == 0 {
					t.Fatal("a streamed download must emit progress events")
				}
			}
			if r := col.of(event.DownloadRetry); len(r) != 0 {
				t.Fatalf("unexpected retry events: %+v", r)
			}
		})
	}
}

func TestDownloadSkipsWithoutRequest(t *testing.T) {
	var requests int
	server := serveFiles(t, &requests)
	dir := t.TempDir()
	tgt := target(server.URL, "one")
	if err := os.MkdirAll(filepath.Join(dir, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "archive", "one.bin"), archive["one"], 0o644); err != nil {
		t.Fatal(err)
	}

	col := &collector{}
	summary := download.New(download.Options{OutDir: dir, Observe: col.observe}).
		Run(context.Background(), []download.Target{tgt})

	if summary.Skipped != 1 || summary.Done != 0 {
		t.Fatalf("summary = %+v, want skipped=1 done=0", summary)
	}
	if skips := col.of(event.DownloadSkipped); len(skips) != 1 || skips[0].Size != int64(len(archive["one"])) {
		t.Fatalf("skipped events = %+v, want one with the file size", skips)
	}
	if requests != 0 {
		t.Fatalf("server saw %d requests, want 0", requests)
	}
}

func TestDownloadRetriesAfterTruncatedResponse(t *testing.T) {
	var requests int
	server := truncateHandler(t, &requests, len(archive["one"])/2)
	dir := t.TempDir()
	tgt := target(server.URL, "one")
	col := &collector{}

	summary := download.New(download.Options{OutDir: dir, Retries: 3, Observe: col.observe}).
		Run(context.Background(), []download.Target{tgt})

	if len(summary.Failures) != 0 {
		t.Fatalf("unexpected failure: %v", summary.Failures[0].Err)
	}
	got := readFile(t, filepath.Join(dir, tgt.DirName, tgt.FileName))
	if string(got) != string(archive["one"]) {
		t.Fatal("retried download produced wrong content")
	}
	if requests < 2 {
		t.Fatalf("truncation did not trigger a resumed retry, requests=%d", requests)
	}
	retries := col.of(event.DownloadRetry)
	if len(retries) != 1 || retries[0].Attempt != 1 {
		t.Fatalf("retry events = %+v, want exactly the failed attempt 1", retries)
	}
}

func TestDownloadRelinksExpiredLinks(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case "gone-id": // the direct URL ran out of validity
			http.Error(w, "gone", http.StatusGone)
		case "fresh":
			w.Header().Set("Content-Length", fmt.Sprint(len(archive["fresh"])))
			_, _ = w.Write(archive["fresh"])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	tgt := target(server.URL, "gone-id")
	relinked := 0
	col := &collector{}

	dl := download.New(download.Options{
		OutDir:  dir,
		Observe: col.observe,
		Relink: func(ctx context.Context, t download.Target) (*url.URL, error) {
			relinked++
			return url.Parse(server.URL + "/fresh")
		},
	})
	summary := dl.Run(context.Background(), []download.Target{tgt})

	if len(summary.Failures) != 0 {
		t.Fatalf("unexpected failure: %v", summary.Failures[0].Err)
	}
	if relinked != 1 {
		t.Fatalf("relink calls = %d, want 1", relinked)
	}
	retries := col.of(event.DownloadRetry)
	if len(retries) != 1 || retries[0].Attempt != 1 || !errors.Is(retries[0].Err, download.ErrLinkExpired) {
		t.Fatalf("retry events = %+v, want one ErrLinkExpired retry for attempt 1", retries)
	}
	got := readFile(t, filepath.Join(dir, tgt.DirName, tgt.FileName))
	if string(got) != string(archive["fresh"]) {
		t.Fatal("content does not match the re-resolved file")
	}
}

func TestDownloadReportsExpiredWithoutRelinker(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "gone", http.StatusGone)
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	tgt := target(server.URL, "one")
	col := &collector{}

	summary := download.New(download.Options{OutDir: dir, Retries: 2, Observe: col.observe}).
		Run(context.Background(), []download.Target{tgt})

	if len(summary.Failures) != 1 {
		t.Fatalf("summary = %+v, want exactly 1 failure", summary)
	}
	if !errors.Is(summary.Failures[0].Err, download.ErrLinkExpired) {
		t.Fatalf("failure error = %v, want ErrLinkExpired", summary.Failures[0].Err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2 (one retry)", requests)
	}
	fails := col.of(event.DownloadFail)
	if len(fails) != 1 || !errors.Is(fails[0].Err, download.ErrLinkExpired) {
		t.Fatalf("failure events = %+v, want one ErrLinkExpired failure", fails)
	}
}

func TestDownloadHonorsCanceledContext(t *testing.T) {
	started := make(chan struct{})
	var closeOnce sync.Once
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1200")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 100; i++ {
			if i == 1 {
				closeOnce.Do(func() { close(started) })
			}
			_, _ = w.Write([]byte("chunk-chunk-"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-time.After(30 * time.Millisecond)
		}
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	dir := t.TempDir()
	tgt := target(server.URL, "one")
	col := &collector{}

	ctx, cancel := context.WithCancel(context.Background())
	summaryCh := make(chan download.Summary, 1)
	go func() {
		summaryCh <- download.New(download.Options{OutDir: dir, Observe: col.observe}).Run(ctx, []download.Target{tgt})
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never produced its first chunks")
	}
	cancel()

	summary := <-summaryCh
	if len(summary.Failures) != 1 || !errors.Is(summary.Failures[0].Err, context.Canceled) {
		t.Fatalf("summary = %+v, want exactly one context.Canceled failure", summary)
	}
	fails := col.of(event.DownloadFail)
	if len(fails) != 1 || !errors.Is(fails[0].Err, context.Canceled) {
		t.Fatalf("failure events = %+v, want one context.Canceled failure", fails)
	}
	if _, err := os.Stat(finalPath(dir, tgt)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final file must not exist after cancellation, stat error: %v", err)
	}
	if _, err := os.Stat(partPath(dir, tgt)); err != nil {
		t.Fatalf("partial temp must survive for resume, stat error: %v", err)
	}
}

// slowServer serves the content in numChunks writes with a pause between
// chunks, so tests can interleave with the stream.
func slowServer(t *testing.T, id, content string, numChunks int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+id {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		for i := 0; i < numChunks; i++ {
			from := i * len(content) / numChunks
			to := (i + 1) * len(content) / numChunks
			_, _ = w.Write([]byte(content[from:to]))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(30 * time.Millisecond)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestGateBlocksStartupUntilResumed(t *testing.T) {
	var requests int
	server := serveFiles(t, &requests)
	dir := t.TempDir()
	gate := download.NewGate()
	gate.Pause()
	col := &collector{}

	summaryCh := make(chan download.Summary, 1)
	go func() {
		summaryCh <- download.New(download.Options{OutDir: dir, Observe: col.observe, Gate: gate}).
			Run(context.Background(), []download.Target{target(server.URL, "one")})
	}()

	select {
	case summary := <-summaryCh:
		t.Fatalf("run finished while paused: %+v", summary)
	case <-time.After(300 * time.Millisecond):
	}

	gate.Resume()
	summary := <-summaryCh
	if len(summary.Failures) != 0 || summary.Done != 1 {
		t.Fatalf("summary = %+v, want one completed download", summary)
	}
	if requests == 0 {
		t.Fatal("no requests seen after resume")
	}
}

func TestGatePausesAndResumesMidStream(t *testing.T) {
	content := strings.Repeat("packrat-chunk-", 40) // 560 bytes
	server := slowServer(t, "one", content, 10)
	dir := t.TempDir()
	gate := download.NewGate()
	tgt := target(server.URL, "one")

	summaryCh := make(chan download.Summary, 1)
	go func() {
		summaryCh <- download.New(download.Options{OutDir: dir, Gate: gate}).
			Run(context.Background(), []download.Target{tgt})
	}()

	// pause once the stream has written something, then check nothing
	// moves while paused
	var pausedSize int64
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(partPath(dir, tgt)); err == nil && fi.Size() > 0 {
			gate.Pause()
			pausedSize = fi.Size()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pausedSize == 0 {
		t.Fatal("never observed partial progress before pausing")
	}
	time.Sleep(300 * time.Millisecond)
	if fi, err := os.Stat(partPath(dir, tgt)); err != nil || fi.Size() > pausedSize+int64(120) {
		// a slow chunk may race the pause check; growth is only allowed
		// for one in-flight chunk
		t.Fatalf("download kept moving while paused: %d -> %d", pausedSize, sizeOf(partPath(dir, tgt)))
	}

	gate.Resume()
	summary := <-summaryCh
	if len(summary.Failures) != 0 {
		t.Fatalf("unexpected failure after resume: %v", summary.Failures[0].Err)
	}
	got := readFile(t, finalPath(dir, tgt))
	if string(got) != content {
		t.Fatalf("resumed download content mismatch: %d bytes, want %d", len(got), len(content))
	}
}

func sizeOf(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

func TestGatePausesTheRetryBackoff(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "not available", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	gate := download.NewGate()
	col := &collector{}
	tgt := target(server.URL, "one")

	summaryCh := make(chan download.Summary, 1)
	go func() {
		summaryCh <- download.New(download.Options{
			OutDir: dir, Retries: 2, Backoff: time.Hour, Observe: col.observe, Gate: gate,
		}).Run(context.Background(), []download.Target{tgt})
	}()

	// wait for the first attempt to fail, then pause: the backoff sleep
	// must not schedule the second attempt
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(col.of(event.DownloadRetry)) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(col.of(event.DownloadRetry)) == 0 {
		t.Fatal("first attempt never failed")
	}
	gate.Pause()
	time.Sleep(200 * time.Millisecond)
	if requests != 1 {
		t.Fatalf("requests = %d while the backoff is paused, want 1", requests)
	}

	gate.Resume()
	summary := <-summaryCh
	if len(summary.Failures) != 1 || requests != 2 {
		t.Fatalf("summary = %+v requests = %d, want 1 failure across 2 attempts", summary, requests)
	}
}
