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
// server serves the matching content; selected ids can fail on purpose, and
// limitIDs makes Resolve answer with the typed rate-limit error.
type fakeHost struct {
	serverURL string
	failIDs   map[string]bool
	limitIDs  map[string]bool
}

func (f *fakeHost) Name() string { return "fake" }

func (f *fakeHost) Matches(u *url.URL) bool { return u.Host == "host.example" }

func (f *fakeHost) Resolve(ctx context.Context, pageURL *url.URL) (host.DirectLink, error) {
	id := pageURL.Path[strings.LastIndex(pageURL.Path, "/")+1:]
	if f.limitIDs[id] {
		return host.DirectLink{}, &host.RateLimitedError{Detail: "status 429"}
	}
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

func TestEngineNothingToDownloadWhenEveryResolveFails(t *testing.T) {
	server := serveContent(t)
	dir := t.TempDir()
	registry := host.NewRegistry(&fakeHost{
		serverURL: server.URL,
		failIDs:   map[string]bool{"part1": true, "part2": true, "bonus": true},
	})

	col := &collector{}
	eng := engine.New(registry, engine.Options{OutDir: dir, Observe: col.observe})

	err := eng.Run(context.Background(), testGroups())
	if err == nil || !strings.Contains(err.Error(), "3 part(s) failed to resolve, nothing to download") {
		t.Fatalf("Run() error = %v, want the nothing-to-download message", err)
	}
	if len(col.of(event.ResolveFail)) != 3 {
		t.Fatalf("resolve failure events = %d, want one per part", len(col.of(event.ResolveFail)))
	}
	if len(col.of(event.DownloadStart)) != 0 {
		t.Fatal("no downloads may start when every resolve fails")
	}
	dones := col.of(event.RunDone)
	if len(dones) != 1 || dones[0].Stats.Done != 0 || dones[0].Stats.Failed != 3 {
		t.Fatalf("run done events = %+v, want done=0 failed=3", dones)
	}
}

// oneGroup returns a single group with n identically-shaped parts so
// streak tests can dial the abort threshold precisely.
func oneGroup(n int) []link.Group {
	parts := make([]*link.Part, 0, n)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("part%d", i)
		parts = append(parts, &link.Part{
			Link:     link.Link{PageURL: "https://host.example/" + id, ID: id},
			DirName:  "G",
			FileName: "G." + id + ".rar",
		})
	}
	return []link.Group{{Name: "G", Parts: parts}}
}

func TestEngineAbortsResolveAfterSustainedRateLimits(t *testing.T) {
	dir := t.TempDir() // nothing resolves, so no downloads must run
	const links = 6
	limited := map[string]bool{}
	for i := 1; i <= links; i++ {
		limited[fmt.Sprintf("part%d", i)] = true
	}
	registry := host.NewRegistry(&fakeHost{limitIDs: limited})

	col := &collector{}
	eng := engine.New(registry, engine.Options{OutDir: dir, Observe: col.observe})

	err := eng.Run(context.Background(), oneGroup(links))
	if err == nil || !strings.Contains(err.Error(), "host is rate limiting") {
		t.Fatalf("Run() error = %v, want the rate-limit bail-out", err)
	}
	failed := col.of(event.ResolveFail)
	if len(failed) != links {
		t.Fatalf("resolve failures = %d, want all %d parts failed fast", len(failed), links)
	}
	for _, e := range failed {
		if !errors.Is(e.Err, host.ErrRateLimited) {
			t.Fatalf("resolve failure = %v, want the typed rate-limit reason", e.Err)
		}
	}
	if len(col.of(event.DownloadStart)) != 0 {
		t.Fatal("nothing may download when the resolve phase bailed out")
	}
	dones := col.of(event.RunDone)
	if len(dones) != 1 || dones[0].Stats.Failed != links {
		t.Fatalf("run done events = %+v, want failed=%d", dones, links)
	}
}

func TestEngineRateLimitStreakResetsOnSuccess(t *testing.T) {
	// a server with content for every part id, so only the two limiting
	// parts fail at all
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := "content for " + strings.TrimPrefix(r.URL.Path, "/")
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	// only the first two parts limit: two hits in a row stay below the
	// abort threshold, and the healthy results reset whatever follows
	registry := host.NewRegistry(&fakeHost{
		serverURL: server.URL,
		limitIDs:  map[string]bool{"part1": true, "part2": true},
	})

	col := &collector{}
	eng := engine.New(registry, engine.Options{OutDir: dir, Observe: col.observe})

	err := eng.Run(context.Background(), oneGroup(6))
	if err == nil || !strings.Contains(err.Error(), "2 part(s) failed") {
		t.Fatalf("Run() error = %v, want the plain failure count", err)
	}
	if len(col.of(event.ResolveFail)) != 2 {
		t.Fatalf("resolve failures = %d, want the two limiting parts only", len(col.of(event.ResolveFail)))
	}
	if len(col.of(event.DownloadStart)) != 4 {
		t.Fatalf("downloads started = %d, want the healthy four", len(col.of(event.DownloadStart)))
	}
	dones := col.of(event.RunDone)
	if len(dones) != 1 || dones[0].Stats.Done != 4 || dones[0].Stats.Failed != 2 {
		t.Fatalf("run done events = %+v, want done=4 failed=2", dones)
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
