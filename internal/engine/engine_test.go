package engine_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EliasLd/packrat/internal/engine"
	"github.com/EliasLd/packrat/internal/host"
	"github.com/EliasLd/packrat/internal/link"
)

// fakeHost resolves every landing page to "<server>/<id>" where the test
// server serves the matching content; selected ids can fail on purpose.
type fakeHost struct {
	serverURL string
	failIDs   map[string]bool
}

func (f *fakeHost) Name() string { return "fake" }

func (f *fakeHost) Matches(u *url.URL) bool { return u.Host == "host.example" }

func (f *fakeHost) Resolve(ctx context.Context, pageURL *url.URL) (host.DirectLink, error) {
	id := pageURL.Path[strings.LastIndex(pageURL.Path, "/")+1:]
	if f.failIDs[id] {
		return host.DirectLink{}, errors.New("host says no")
	}
	direct, err := url.Parse(f.serverURL + "/" + id)
	if err != nil {
		return host.DirectLink{}, err
	}
	return host.DirectLink{URL: direct, Filename: "titled-" + id + ".bin"}, nil
}

func files() map[string]string {
	return map[string]string{
		"part1": "content for part one",
		"part2": "part two has different content lengths!!",
		"bonus": "optional bonus bytes",
	}
}

// resolve testdata: two fragments in one archive, one standalone, one with no
// fragment (filename must then come from the host).
func testGroups() []link.Group {
	return []link.Group{
		{
			Name: "Game-Archive",
			Parts: []*link.Part{
				{Link: link.Link{PageURL: "https://host.example/part1", ID: "part1", Name: "Game-Archive.part1.rar"}, DirName: "Game-Archive", FileName: "Game-Archive.part1.rar"},
				{Link: link.Link{PageURL: "https://host.example/part2", ID: "part2", Name: "Game-Archive.part2.rar"}, DirName: "Game-Archive", FileName: "Game-Archive.part2.rar"},
			},
		},
		{
			Name: "Extra",
			Parts: []*link.Part{
				{Link: link.Link{PageURL: "https://host.example/bonus", ID: "bonus", Name: "Extra.bonus.rar"}, DirName: "Extra", FileName: "Extra.bonus.rar"},
			},
		},
	}
}

func serveContent(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/")
		content, ok := files()[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestEngineRun(t *testing.T) {
	server := serveContent(t)
	dir := t.TempDir()
	registry := host.NewRegistry(&fakeHost{serverURL: server.URL})

	var out, warn strings.Builder
	eng := engine.New(registry, engine.Options{OutDir: dir, UserAgent: "test-agent"}, &out, &warn)

	if err := eng.Run(context.Background(), testGroups()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// parts with a fragment keep it as the target file name; direct URLs map
	// to the test content by id
	wants := []struct {
		rel     string
		content string
	}{
		{filepath.Join("Game-Archive", "Game-Archive.part1.rar"), files()["part1"]},
		{filepath.Join("Game-Archive", "Game-Archive.part2.rar"), files()["part2"]},
		{filepath.Join("Extra", "Extra.bonus.rar"), files()["bonus"]},
	}
	for _, want := range wants {
		target := filepath.Join(dir, want.rel)
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read %s: %v", target, err)
		}
		if string(got) != want.content {
			t.Fatalf("%s content mismatch: %q != %q", target, got, want.content)
		}
	}
	if !strings.Contains(out.String(), "Game-Archive.part1.rar OK") || !strings.Contains(out.String(), "done: 3/3") {
		t.Fatalf("output missing event lines:\n%s", out.String())
	}
	if warn.String() != "" {
		t.Fatalf("unexpected warnings:\n%s", warn.String())
	}
}

func TestEngineKeepsGoingWhenResolveFails(t *testing.T) {
	server := serveContent(t)
	dir := t.TempDir()
	registry := host.NewRegistry(&fakeHost{serverURL: server.URL, failIDs: map[string]bool{"part1": true}})

	var out, warn strings.Builder
	eng := engine.New(registry, engine.Options{OutDir: dir}, &out, &warn)

	err := eng.Run(context.Background(), testGroups())
	if err == nil {
		t.Fatal("Run() expected an error when parts fail")
	}
	if !strings.Contains(err.Error(), "1 part(s) failed") {
		t.Fatalf("Run() error = %v, want failure count", err)
	}
	if !strings.Contains(warn.String(), "[resolve FAIL] part1") {
		t.Fatalf("warnings missing resolve failure:\n%s", warn.String())
	}
	// the two healthy parts must still be on disk, named after their fragments
	for _, rel := range []string{filepath.Join("Game-Archive", "Game-Archive.part2.rar"), filepath.Join("Extra", "Extra.bonus.rar")} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Fatalf("healthy part %s missing: %v", rel, err)
		}
	}
}

func TestEngineUsesHostFilenameWithoutFragment(t *testing.T) {
	server := serveContent(t)
	dir := t.TempDir()
	registry := host.NewRegistry(&fakeHost{serverURL: server.URL})

	groups := []link.Group{
		{
			Name: "unknown",
			Parts: []*link.Part{
				{Link: link.Link{PageURL: "https://host.example/part1", ID: "part1"}, DirName: "unknown", FileName: "part1"},
			},
		},
	}
	var out, warn strings.Builder
	eng := engine.New(registry, engine.Options{OutDir: dir}, &out, &warn)
	if err := eng.Run(context.Background(), groups); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "unknown", "titled-part1.bin"))
	if err != nil {
		t.Fatalf("host-provided filename not used: %v", err)
	}
	if string(got) != files()["part1"] {
		t.Fatal("content mismatch")
	}
}

// ensure the resolve phase really runs concurrently and stops cleanly
func TestEngineEmptyInput(t *testing.T) {
	registry := host.NewRegistry(&fakeHost{serverURL: "http://127.0.0.1:1"})
	var out strings.Builder
	eng := engine.New(registry, engine.Options{}, &out, io.Discard)
	if err := eng.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run(nil groups) must be a no-op success, got %v", err)
	}
	if !strings.Contains(out.String(), "no links to process") {
		t.Fatalf("unexpected output: %q", out.String())
	}
}
