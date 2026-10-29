package engine_test

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

	"github.com/EliasLd/packrat/internal/engine"
	"github.com/EliasLd/packrat/internal/event"
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

	col := &collector{}
	eng := engine.New(registry, engine.Options{OutDir: dir, UserAgent: "test-agent", Observe: col.observe})

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

	starts := col.of(event.RunStart)
	if len(starts) != 1 || starts[0].Stats.PartsTotal != 3 || starts[0].Stats.GroupsTotal != 2 {
		t.Fatalf("run start events = %+v, want one start for 3 parts in 2 groups", starts)
	}
	if len(col.of(event.ResolveDone)) != 3 || len(col.of(event.ResolveFail)) != 0 {
		t.Fatalf("resolve events: done=%d fail=%d, want 3 done and none failed",
			len(col.of(event.ResolveDone)), len(col.of(event.ResolveFail)))
	}
	if len(col.of(event.DownloadDone)) != 3 || len(col.of(event.DownloadFail)) != 0 {
		t.Fatalf("download events: done=%d fail=%d, want 3 done and none failed",
			len(col.of(event.DownloadDone)), len(col.of(event.DownloadFail)))
	}
	dones := col.of(event.RunDone)
	if len(dones) != 1 || dones[0].Stats.Done != 3 || dones[0].Stats.Failed != 0 {
		t.Fatalf("run done events = %+v, want one with done=3 failed=0", dones)
	}
}

func TestEnginePartIdentityCarriesToDownloads(t *testing.T) {
	server := serveContent(t)
	dir := t.TempDir()
	registry := host.NewRegistry(&fakeHost{serverURL: server.URL})

	col := &collector{}
	eng := engine.New(registry, engine.Options{OutDir: dir, Observe: col.observe})
	if err := eng.Run(context.Background(), testGroups()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	for _, part := range []string{"part1", "part2", "bonus"} {
		var resolved bool
		for _, e := range col.of(event.ResolveDone) {
			if e.Part == part {
				resolved = true
			}
		}
		if !resolved {
			t.Fatalf("part %s never resolved", part)
		}
		started := col.of(event.DownloadStart)
		var found bool
		for _, e := range started {
			if e.Part == part {
				found = true
			}
		}
		if !found {
			t.Fatalf("download events lost the part identity %s", part)
		}
	}
}

func TestEngineKeepsGoingWhenResolveFails(t *testing.T) {
	server := serveContent(t)
	dir := t.TempDir()
	registry := host.NewRegistry(&fakeHost{serverURL: server.URL, failIDs: map[string]bool{"part1": true}})

	col := &collector{}
	eng := engine.New(registry, engine.Options{OutDir: dir, Observe: col.observe})

	err := eng.Run(context.Background(), testGroups())
	if err == nil {
		t.Fatal("Run() expected an error when parts fail")
	}
	if !strings.Contains(err.Error(), "1 part(s) failed") {
		t.Fatalf("Run() error = %v, want failure count", err)
	}
	fails := col.of(event.ResolveFail)
	if len(fails) != 1 || fails[0].Part != "part1" {
		t.Fatalf("resolve failure events = %+v, want part1", fails)
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
	col := &collector{}
	eng := engine.New(registry, engine.Options{OutDir: dir, Observe: col.observe})
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
	// the resolve event must carry the host-provided name for display
	dones := col.of(event.ResolveDone)
	if len(dones) != 1 || dones[0].Filename != "titled-part1.bin" {
		t.Fatalf("resolve done events = %+v, want the host filename", dones)
	}
}

func TestEngineEmptyInput(t *testing.T) {
	registry := host.NewRegistry(&fakeHost{serverURL: "http://127.0.0.1:1"})
	col := &collector{}
	eng := engine.New(registry, engine.Options{Observe: col.observe})
	if err := eng.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run(nil groups) must be a no-op success, got %v", err)
	}
	dones := col.of(event.RunDone)
	if len(dones) != 1 || dones[0].Stats.PartsTotal != 0 {
		t.Fatalf("run done events = %+v, want one empty summary", dones)
	}
	if len(col.of(event.RunStart)) != 0 {
		t.Fatal("an empty run must not start")
	}
}
