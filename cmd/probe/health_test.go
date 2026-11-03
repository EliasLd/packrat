package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	bhttp "github.com/bogdanfinn/fhttp"

	"github.com/EliasLd/packrat/internal/host"
)

func TestClassNames(t *testing.T) {
	tests := []struct {
		c    Class
		want string
	}{
		{ClassOK, "ok"},
		{ClassStale, "stale"},
		{ClassBroken, "broken"},
		{ClassInfra, "infra"},
	}
	for _, tt := range tests {
		if got := tt.c.String(); got != tt.want {
			t.Fatalf("Class(%d).String() = %q, want %q", tt.c, got, tt.want)
		}
	}
}

func TestClassExitCodes(t *testing.T) {
	tests := []struct {
		c    Class
		want int
	}{
		{ClassOK, 0},
		{ClassBlocked, 0},
		{ClassBroken, 1},
		{ClassStale, 2},
		{ClassInfra, 3},
	}
	for _, tt := range tests {
		if got := tt.c.exitCode(); got != tt.want {
			t.Fatalf("Class(%d).exitCode() = %d, want %d", tt.c, got, tt.want)
		}
	}
}

func TestClassifyResolve(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Class
	}{
		{"dead file", host.ErrFileGone, ClassStale},
		{"wrapped dead file stays stale", fmt.Errorf("resolve after 3 attempt(s): %w", host.ErrFileGone), ClassStale},
		{"bot challenge", host.ErrChallengeRequired, ClassBlocked},
		{"wrapped bot challenge stays blocked", fmt.Errorf("resolve after 3 attempt(s): fetch page: %w", host.ErrChallengeRequired), ClassBlocked},
		{"dial failure", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, ClassInfra},
		{"timeout", &timeoutErr{}, ClassInfra},
		{"deadline", context.DeadlineExceeded, ClassInfra},
		{"rate limited", host.ErrRateLimited, ClassBroken},
		{"layout change", errors.New("download button markup not found"), ClassBroken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyResolve(tt.err); got != tt.want {
				t.Fatalf("classifyResolve() = %v, want %v", got, tt.want)
			}
		})
	}
}

// timeoutErr matches net.Error with Timeout on, without being an OpError.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func TestClassifyRead(t *testing.T) {
	tests := []struct {
		name string
		std  readResult
		tls  readResult
		want Class
	}{
		{"both clients read", readResult{ok: true, answered: true}, readResult{ok: true, answered: true}, ClassOK},
		{"one client read, other unanswered", readResult{ok: true, answered: true}, readResult{}, ClassOK},
		{"one client read, other refused", readResult{ok: true, answered: true}, readResult{answered: true}, ClassOK},
		{"both refused cleanly", readResult{answered: true}, readResult{answered: true}, ClassBroken},
		{"refused next to unanswered", readResult{answered: true}, readResult{}, ClassBroken},
		{"nothing answered", readResult{}, readResult{}, ClassInfra},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyRead(tt.std, tt.tls); got != tt.want {
				t.Fatalf("classifyRead() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHostState(t *testing.T) {
	tests := []struct {
		name    string
		classes []Class
		want    Class
	}{
		{"one working link covers dead codes", []Class{ClassStale, ClassOK}, ClassOK},
		{"one working link covers a challenge", []Class{ClassBlocked, ClassOK, ClassStale}, ClassOK},
		{"all dead codes are stale", []Class{ClassStale, ClassStale}, ClassStale},
		{"everything challenged is blocked", []Class{ClassBlocked}, ClassBlocked},
		{"dead codes beat challenges", []Class{ClassBlocked, ClassStale}, ClassStale},
		{"a broken link decides whatever the siblings did", []Class{ClassOK, ClassStale, ClassBlocked, ClassBroken}, ClassBroken},
		{"infra decides over everything", []Class{ClassOK, ClassBroken, ClassStale, ClassInfra}, ClassInfra},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostState(tt.classes); got != tt.want {
				t.Fatalf("hostState() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunState(t *testing.T) {
	tests := []struct {
		name   string
		states []Class
		want   int
	}{
		{"all hosts green", []Class{ClassOK, ClassOK}, 0},
		{"one host blocked", []Class{ClassOK, ClassBlocked}, 0},
		{"every host blocked checked nothing", []Class{ClassBlocked, ClassBlocked}, 1},
		{"one host stale", []Class{ClassOK, ClassStale}, 2},
		{"stale next to blocked", []Class{ClassBlocked, ClassStale}, 2},
		{"one host broken", []Class{ClassStale, ClassBroken}, 1},
		{"broken next to blocked", []Class{ClassBlocked, ClassBroken}, 1},
		{"one host infra", []Class{ClassOK, ClassInfra}, 3},
		{"broken next to infra", []Class{ClassBroken, ClassInfra}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runState(tt.states).exitCode(); got != tt.want {
				t.Fatalf("runState(%v) exits %d, want %d", tt.states, got, tt.want)
			}
		})
	}
}

// fakeHost serves as a registry host: it claims its domain and hands out
// either a direct link on a local server or an error, per landing page.
type fakeHost struct {
	name    string
	domain  string
	resolve func(u *url.URL) (host.DirectLink, error)
	calls   atomic.Int64
}

func (f *fakeHost) Name() string { return f.name }

func (f *fakeHost) Matches(u *url.URL) bool {
	return u != nil && u.Hostname() == f.domain
}

func (f *fakeHost) Resolve(_ context.Context, u *url.URL) (host.DirectLink, error) {
	f.calls.Add(1)
	return f.resolve(u)
}

// fakeTLSClient answers ranged reads with the configured status.
type fakeTLSClient struct{ status int }

func (f *fakeTLSClient) Do(_ *bhttp.Request) (*bhttp.Response, error) {
	return &bhttp.Response{
		StatusCode: f.status,
		Proto:      "HTTP/2.0",
		Header:     bhttp.Header{"Content-Range": {"bytes 0-1048575/4194304"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil
}

// downloadServer answers ranged reads with file bytes.
func downloadServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-1048575/4194304")
		w.WriteHeader(http.StatusPartialContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// directURL renders a direct download address on a local server.
func directURL(t *testing.T, base, name string) *url.URL {
	t.Helper()
	u, err := url.Parse(base + "/dl/" + name)
	if err != nil {
		t.Fatalf("direct url: %v", err)
	}
	return u
}

// captureStdout swaps os.Stdout for a pipe while fn runs, returning what
// was printed and fn's result.
func captureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	replaced := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	code := fn()
	_ = w.Close()
	os.Stdout = replaced
	var out bytes.Buffer
	_, _ = io.Copy(&out, r)
	return out.String(), code
}

func writeFixture(t *testing.T, lines ...string) string {
	t.Helper()
	path := t.TempDir() + "/links.txt"
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestRunLinksHealthyFixture(t *testing.T) {
	down := downloadServer(t)
	alpha := &fakeHost{name: "alpha", domain: "alpha.test", resolve: func(*url.URL) (host.DirectLink, error) {
		return host.DirectLink{URL: directURL(t, down.URL, "file.part001.rar"), Filename: "file.part001.rar"}, nil
	}}
	beta := &fakeHost{name: "beta", domain: "beta.test", resolve: func(*url.URL) (host.DirectLink, error) {
		return host.DirectLink{URL: directURL(t, down.URL, "file.part002.rar"), Filename: "file.part002.rar"}, nil
	}}
	fixture := writeFixture(t,
		"https://alpha.test/a1#file.part001.rar",
		"https://beta.test/b1/file.part002.rar",
	)

	out, code := captureStdout(t, func() int {
		return runLinks(context.Background(), host.NewRegistry(alpha, beta), &fakeTLSClient{status: 206}, fixture)
	})

	if code != 0 {
		t.Fatalf("runLinks exit = %d, want 0\n%s", code, out)
	}
	ai, bi := strings.Index(out, "probe: alpha=ok"), strings.Index(out, "probe: beta=ok")
	if ai < 0 || bi < 0 || ai > bi {
		t.Fatalf("state lines missing or out of registration order:\n%s", out)
	}
}

func TestRunLinksBlockedEgress(t *testing.T) {
	down := downloadServer(t)
	alpha := &fakeHost{name: "alpha", domain: "alpha.test", resolve: func(*url.URL) (host.DirectLink, error) {
		return host.DirectLink{URL: directURL(t, down.URL, "file.part001.rar"), Filename: "file.part001.rar"}, nil
	}}
	beta := &fakeHost{name: "beta", domain: "beta.test", resolve: func(*url.URL) (host.DirectLink, error) {
		return host.DirectLink{}, fmt.Errorf("resolve after 3 attempt(s): fetch page: %w", host.ErrChallengeRequired)
	}}
	fixture := writeFixture(t,
		"https://alpha.test/a1#file.part001.rar",
		"https://beta.test/b1#file.part002.rar",
	)

	out, code := captureStdout(t, func() int {
		return runLinks(context.Background(), host.NewRegistry(alpha, beta), &fakeTLSClient{status: 206}, fixture)
	})

	// a challenged egress must not alarm: green run, blocked state visible
	if code != 0 {
		t.Fatalf("runLinks exit = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "probe: beta=blocked") || !strings.Contains(out, "probe: alpha=ok") {
		t.Fatalf("blocked state line missing:\n%s", out)
	}
}

func TestRunLinksProvingLinksCoverDeadCodes(t *testing.T) {
	down := downloadServer(t)
	alpha := &fakeHost{name: "alpha", domain: "alpha.test", resolve: func(u *url.URL) (host.DirectLink, error) {
		if strings.HasSuffix(u.Path, "a2") { // one spare code died with the game
			return host.DirectLink{}, fmt.Errorf("resolve after 3 attempt(s): %w", host.ErrFileGone)
		}
		return host.DirectLink{URL: directURL(t, down.URL, "file.part001.rar"), Filename: "file.part001.rar"}, nil
	}}
	fixture := writeFixture(t,
		"https://alpha.test/a1#file.part001.rar",
		"https://alpha.test/a2#file.part002.rar",
	)

	out, code := captureStdout(t, func() int {
		return runLinks(context.Background(), host.NewRegistry(alpha), &fakeTLSClient{status: 206}, fixture)
	})

	// one proven link keeps the run green; the dead one stays in the log
	if code != 0 {
		t.Fatalf("runLinks exit = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "probe: alpha=ok") {
		t.Fatalf("a working link must cover its host:\n%s", out)
	}
}

func TestRunLinksStaleFixture(t *testing.T) {
	down := downloadServer(t)
	alpha := &fakeHost{name: "alpha", domain: "alpha.test", resolve: func(*url.URL) (host.DirectLink, error) {
		return host.DirectLink{URL: directURL(t, down.URL, "file.part001.rar"), Filename: "file.part001.rar"}, nil
	}}
	beta := &fakeHost{name: "beta", domain: "beta.test", resolve: func(*url.URL) (host.DirectLink, error) {
		return host.DirectLink{}, fmt.Errorf("resolve after 3 attempt(s): %w", host.ErrFileGone)
	}}
	fixture := writeFixture(t,
		"https://alpha.test/a1#file.part001.rar",
		"https://beta.test/b1#file.part002.rar",
	)

	out, code := captureStdout(t, func() int {
		return runLinks(context.Background(), host.NewRegistry(alpha, beta), &fakeTLSClient{status: 206}, fixture)
	})

	if code != 2 {
		t.Fatalf("runLinks exit = %d, want 2\n%s", code, out)
	}
	if !strings.Contains(out, "probe: beta=stale") {
		t.Fatalf("stale state line missing:\n%s", out)
	}
}

func TestRunLinksBrokenDownloadEdge(t *testing.T) {
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(rejecting.Close)
	alpha := &fakeHost{name: "alpha", domain: "alpha.test", resolve: func(*url.URL) (host.DirectLink, error) {
		return host.DirectLink{URL: directURL(t, rejecting.URL, "file.part001.rar"), Filename: "file.part001.rar"}, nil
	}}
	fixture := writeFixture(t, "https://alpha.test/a1#file.part001.rar")

	out, code := captureStdout(t, func() int {
		return runLinks(context.Background(), host.NewRegistry(alpha), &fakeTLSClient{status: 403}, fixture)
	})

	if code != 1 {
		t.Fatalf("runLinks exit = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "probe: alpha=broken") {
		t.Fatalf("broken state line missing:\n%s", out)
	}
}

func TestRunLinksFlagsMissingHostLinks(t *testing.T) {
	down := downloadServer(t)
	alpha := &fakeHost{name: "alpha", domain: "alpha.test", resolve: func(*url.URL) (host.DirectLink, error) {
		return host.DirectLink{URL: directURL(t, down.URL, "file.part001.rar"), Filename: "file.part001.rar"}, nil
	}}
	beta := &fakeHost{name: "beta", domain: "beta.test", resolve: func(*url.URL) (host.DirectLink, error) {
		return host.DirectLink{}, nil
	}}
	fixture := writeFixture(t, "https://alpha.test/a1#file.part001.rar")

	out, code := captureStdout(t, func() int {
		return runLinks(context.Background(), host.NewRegistry(alpha, beta), &fakeTLSClient{status: 206}, fixture)
	})

	if code != 1 {
		t.Fatalf("runLinks exit = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "probe: beta=broken") {
		t.Fatalf("host without links must read as broken:\n%s", out)
	}
}

func TestRunLinksFlagsUnclaimedLinks(t *testing.T) {
	down := downloadServer(t)
	alpha := &fakeHost{name: "alpha", domain: "alpha.test", resolve: func(*url.URL) (host.DirectLink, error) {
		return host.DirectLink{URL: directURL(t, down.URL, "file.part001.rar"), Filename: "file.part001.rar"}, nil
	}}
	fixture := writeFixture(t,
		"https://alpha.test/a1#file.part001.rar",
		"https://gamma.test/g1#file.part003.rar",
	)

	out, code := captureStdout(t, func() int {
		return runLinks(context.Background(), host.NewRegistry(alpha), &fakeTLSClient{status: 206}, fixture)
	})

	if code != 1 {
		t.Fatalf("runLinks exit = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "probe: unknown=broken") {
		t.Fatalf("unclaimed link state line missing:\n%s", out)
	}
}
