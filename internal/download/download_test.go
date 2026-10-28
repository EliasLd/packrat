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
)

// archive holds test file content keyed by id.
var archive = map[string][]byte{
	"one":   []byte("content-of-file-one-and-some-more-bytes-\r\n\t"),
	"two":   []byte("another-file-content-with-a-different-length"),
	"fresh": []byte("the-relink-successor-content!"),
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
		name  string
		id    string
		setup func(t *testing.T, dir string)
		want  []byte
	}{
		{
			name: "downloads content into the target directory",
			id:   "one",
			want: archive["one"],
		},
		{
			name: "skips an already downloaded final file",
			id:   "one",
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
			name: "resumes from a partial temp file",
			id:   "two",
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
			summary := download.New(download.Options{OutDir: dir}).Run(context.Background(), []download.Target{tgt})

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

	summary := download.New(download.Options{OutDir: dir}).Run(context.Background(), []download.Target{tgt})

	if summary.Skipped != 1 || summary.Done != 0 {
		t.Fatalf("summary = %+v, want skipped=1 done=0", summary)
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

	summary := download.New(download.Options{OutDir: dir, Retries: 3}).
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

	dl := download.New(download.Options{
		OutDir: dir,
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

	summary := download.New(download.Options{OutDir: dir, Retries: 2}).
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

	ctx, cancel := context.WithCancel(context.Background())
	summaryCh := make(chan download.Summary, 1)
	go func() {
		summaryCh <- download.New(download.Options{OutDir: dir}).Run(ctx, []download.Target{tgt})
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
	if _, err := os.Stat(finalPath(dir, tgt)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final file must not exist after cancellation, stat error: %v", err)
	}
	if _, err := os.Stat(partPath(dir, tgt)); err != nil {
		t.Fatalf("partial temp must survive for resume, stat error: %v", err)
	}
}
