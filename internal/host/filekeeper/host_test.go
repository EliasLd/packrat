package filekeeper_test

import (
	"context"
	_ "embed"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EliasLd/packrat/internal/event"
	"github.com/EliasLd/packrat/internal/host"
	"github.com/EliasLd/packrat/internal/host/filekeeper"
)

//go:embed testdata/page_valid.html
var pageValid string

//go:embed testdata/page_captcha.html
var pageCaptcha string

//go:embed testdata/page_password.html
var pagePassword string

//go:embed testdata/page_broken.html
var pageBroken string

//go:embed testdata/page_dead.html
var pageDead string

//go:embed testdata/page_rate_limit.html
var pageRateLimit string

const (
	pagePath   = "/abc123/My.Archive.multi.part01.rar"
	landedPath = "/download"
	directPath = "/edge/file?sig=x"
)

// sleepRecorder stands in for real waiting and records every wait the
// resolver schedules. Resolves fan out concurrently, so it is locked.
type sleepRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleepRecorder) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration{}, s.waits...)
}

// counts tallies request methods across a mux so tests can assert how many
// requests a flow really made.
type counts struct {
	get  atomic.Int64
	post atomic.Int64
}

// resolver builds a resolver against srv with instant backoff and recorded
// sleeps; the waits surface through the returned recorder.
func resolver(t *testing.T, srv *httptest.Server, opts ...filekeeper.Option) (*filekeeper.Host, *sleepRecorder) {
	t.Helper()
	s := &sleepRecorder{}
	h := filekeeper.New(append([]filekeeper.Option{
		filekeeper.WithBaseDelay(time.Millisecond),
		filekeeper.WithSleep(s.sleep),
	}, opts...)...)
	t.Cleanup(srv.Close)
	return h, s
}

func pageURL(t *testing.T, srv *httptest.Server, path string) *url.URL {
	t.Helper()
	u, err := url.Parse(srv.URL + path)
	if err != nil {
		t.Fatalf("test url: %v", err)
	}
	return u
}

// cookie reads the file_code cookie value the resolver forges.
func cookie(r *http.Request) string {
	if c, err := r.Cookie("file_code"); err == nil {
		return c.Value
	}
	return ""
}

// serveWithCookie asserts every request carries the forged file_code cookie
// before delegating to the handler.
func serveWithCookie(t *testing.T, want string, next http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if got := cookie(r); got != want {
			t.Errorf("%s %s: Cookie = %q, want the forged file_code=%s", r.Method, r.URL.Path, got, want)
		}
		next(w, r)
	}
}

func TestResolve(t *testing.T) {
	t.Run("happy path: follow the landing redirect, wait the countdown, take the Location link", func(t *testing.T) {
		var (
			srv    *httptest.Server
			counts counts
		)
		srvURL := func() string { return srv.URL }

		mux := http.NewServeMux()
		mux.HandleFunc(pagePath, serveWithCookie(t, "abc123", func(w http.ResponseWriter, r *http.Request) {
			counts.get.Add(1)
			http.Redirect(w, r, landedPath, http.StatusFound)
		}))
		mux.HandleFunc(landedPath, serveWithCookie(t, "abc123", func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				counts.get.Add(1)
				_, _ = w.Write([]byte(pageValid))
			case http.MethodPost:
				counts.post.Add(1)
				assertFreeDownloadForm(t, r, srvURL()+landedPath)
				http.Redirect(w, r, srvURL()+directPath, http.StatusFound)
			default:
				t.Errorf("unexpected method %q", r.Method)
				http.Error(w, "unexpected method", http.StatusBadRequest)
			}
		}))
		mux.HandleFunc("/edge/file", func(w http.ResponseWriter, r *http.Request) {
			// the direct link must travel in the Location header only; the
			// resolver must never follow it
			t.Errorf("the resolver followed the download redirect to %q", r.URL)
			http.Error(w, "must not be followed", http.StatusInternalServerError)
		})
		srv = httptest.NewServer(mux)
		h, sleeps := resolver(t, srv)

		got, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
		if err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		if got.URL.String() != srvURL()+directPath {
			t.Fatalf("Resolve() URL = %q, want %q", got.URL, srvURL()+directPath)
		}
		if got.Filename != "My.Archive.multi.part01.rar" {
			t.Fatalf("Resolve() Filename = %q, want the title without its Download prefix", got.Filename)
		}
		if got.Size != 0 {
			t.Fatalf("Resolve() Size = %d, want 0 (unknown upfront)", got.Size)
		}
		if waits := sleeps.all(); len(waits) != 1 || waits[0] != 6*time.Second {
			t.Fatalf("waits = %v, want the exact 5s countdown plus one spare", sleeps.all())
		}
		if counts.get.Load() != 2 || counts.post.Load() != 1 {
			t.Fatalf("requests: get=%d post=%d, want 2 page fetches (landing + redirect) and 1 post",
				counts.get.Load(), counts.post.Load())
		}
	})

	t.Run("code-only links resolve from the first path segment", func(t *testing.T) {
		var srv *httptest.Server
		srvURL := func() string { return srv.URL }

		mux := http.NewServeMux()
		mux.HandleFunc("/abc123", serveWithCookie(t, "abc123", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, landedPath, http.StatusFound)
		}))
		mux.HandleFunc(landedPath, serveWithCookie(t, "abc123", func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_, _ = w.Write([]byte(pageValid))
			case http.MethodPost:
				assertFreeDownloadForm(t, r, srvURL()+landedPath)
				http.Redirect(w, r, srvURL()+"/edge/abc123", http.StatusFound)
			}
		}))
		mux.HandleFunc("/edge/abc123", func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the resolver followed the download redirect to %q", r.URL)
			http.Error(w, "must not be followed", http.StatusInternalServerError)
		})
		srv = httptest.NewServer(mux)
		h, _ := resolver(t, srv)

		got, err := h.Resolve(context.Background(), pageURL(t, srv, "/abc123"))
		if err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		if got.URL.String() != srvURL()+"/edge/abc123" {
			t.Fatalf("Resolve() URL = %q, want %q", got.URL, srvURL()+"/edge/abc123")
		}
	})

	t.Run("cancelled context stops before any request", func(t *testing.T) {
		srv := httptest.NewServer(http.NewServeMux())
		h, _ := resolver(t, srv)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := h.Resolve(ctx, pageURL(t, srv, pagePath)); !errors.Is(err, context.Canceled) {
			t.Fatalf("Resolve() error = %v, want context.Canceled", err)
		}
	})
}

// assertFreeDownloadForm checks the request the free-download button makes:
// the form fields verbatim, the landed page as Referer.
func assertFreeDownloadForm(t *testing.T, r *http.Request, landedURL string) {
	t.Helper()
	for field, want := range map[string]string{
		"op":          "download2",
		"id":          "abc123",
		"rand":        "",
		"referer":     "",
		"method_free": "Free download",
		"down_direct": "1",
	} {
		if got := r.FormValue(field); got != want {
			t.Errorf("form %s = %q, want %q", field, got, want)
		}
	}
	if got := r.Referer(); got != landedURL {
		t.Errorf("Referer = %q, want the landed page %q", got, landedURL)
	}
}

// TestResolveForgesItsCookiePerRequest pins the no-jar design: every request
// a resolve makes carries the cookie of its own link, so simultaneous
// resolves cannot hand each other the wrong file (the shared-jar race).
func TestResolveForgesItsCookiePerRequest(t *testing.T) {
	mux := http.NewServeMux()
	for _, code := range []string{"c1", "c2"} {
		mux.HandleFunc("/"+code, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, landedPath, http.StatusFound)
		})
	}
	mux.HandleFunc(landedPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if cookie(r) == "" {
				t.Errorf("download page requested without a file_code cookie")
				http.NotFound(w, r)
				return
			}
			// the page announces the code its own cookie names
			_, _ = w.Write([]byte(strings.Replace(pageValid, "abc123", cookie(r), 1)))
		case http.MethodPost:
			if cookie(r) != r.FormValue("id") {
				t.Errorf("download post id %q does not match its own cookie %q", r.FormValue("id"), cookie(r))
			}
			http.Redirect(w, r, "/edge/"+cookie(r), http.StatusFound)
		}
	})
	srv := httptest.NewServer(mux)
	h, _ := resolver(t, srv)

	type result struct {
		page string
		url  string
		err  error
	}
	results := make(chan result, 2)
	for _, code := range []string{"c1", "c2"} {
		go func(code string) {
			direct, err := h.Resolve(context.Background(), pageURL(t, srv, "/"+code))
			results <- result{page: code, url: direct.URL.String(), err: err}
		}(code)
	}
	for range []string{"c1", "c2"} {
		r := <-results
		if r.err != nil {
			t.Fatalf("Resolve(%s) error: %v", r.page, r.err)
		}
		if want := srv.URL + "/edge/" + r.page; r.url != want {
			t.Fatalf("Resolve(%s) URL = %q, want %q (its own file)", r.page, r.url, want)
		}
	}
}

func TestResolveDeadFile(t *testing.T) {
	t.Run("ErrFileGone is the shared host sentinel", func(t *testing.T) {
		if !errors.Is(filekeeper.ErrFileGone, host.ErrFileGone) {
			t.Fatal("filekeeper.ErrFileGone must alias host.ErrFileGone")
		}
	})

	t.Run("a dead link fails without retrying", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "File Not Found", http.StatusNotFound)
		})
		srv := httptest.NewServer(mux)
		h, _ := resolver(t, srv)

		_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
		if !errors.Is(err, filekeeper.ErrFileGone) {
			t.Fatalf("Resolve() error = %v, want ErrFileGone", err)
		}
	})

	t.Run("a dead file on the download step is permanent too", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc(pagePath, serveWithCookie(t, "abc123", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, landedPath, http.StatusFound)
		}))
		mux.HandleFunc(landedPath, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_, _ = w.Write([]byte(pageValid))
			case http.MethodPost:
				_, _ = w.Write([]byte(pageDead))
			}
		})
		srv := httptest.NewServer(mux)
		h, _ := resolver(t, srv)

		_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
		if !errors.Is(err, filekeeper.ErrFileGone) {
			t.Fatalf("Resolve() error = %v, want ErrFileGone", err)
		}
	})
}

func TestResolveBotChallenge(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html lang="en-US"><head><title>Just a moment...</title></head>`))
	})
	srv := httptest.NewServer(mux)
	h, _ := resolver(t, srv)

	_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
	if !errors.Is(err, host.ErrChallengeRequired) {
		t.Fatalf("Resolve() error = %v, want ErrChallengeRequired", err)
	}
	if !strings.Contains(err.Error(), "fetch page:") {
		t.Fatalf("Resolve() error = %v, want the page fetch stage in the text", err)
	}
}

func TestResolveLayoutChanged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pageBroken))
	})
	srv := httptest.NewServer(mux)
	h, _ := resolver(t, srv)

	_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
	if !errors.Is(err, filekeeper.ErrLayoutChanged) {
		t.Fatalf("Resolve() error = %v, want ErrLayoutChanged", err)
	}
}

func TestResolveCaptcha(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pageCaptcha))
	})
	srv := httptest.NewServer(mux)
	h, _ := resolver(t, srv)

	_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
	if !errors.Is(err, filekeeper.ErrCAPTCHARequired) {
		t.Fatalf("Resolve() error = %v, want ErrCAPTCHARequired", err)
	}
}

func TestResolvePassword(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pagePassword))
	})
	srv := httptest.NewServer(mux)
	h, _ := resolver(t, srv)

	_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
	if !errors.Is(err, filekeeper.ErrPasswordProtected) {
		t.Fatalf("Resolve() error = %v, want ErrPasswordProtected", err)
	}
}

func TestResolveNilURLFails(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	h, _ := resolver(t, srv)
	if _, err := h.Resolve(context.Background(), nil); err == nil {
		t.Fatal("Resolve(nil) expected error")
	}
}

func TestRateLimitResponses(t *testing.T) {
	t.Run("status 429 carries the parsed Retry-After", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "slow down", http.StatusTooManyRequests)
		})
		srv := httptest.NewServer(mux)
		h, _ := resolver(t, srv)

		_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
		var limited *host.RateLimitedError
		if !errors.As(err, &limited) {
			t.Fatalf("Resolve() error = %v, want a *host.RateLimitedError", err)
		}
		if limited.RetryAfter != 30*time.Second {
			t.Fatalf("RetryAfter = %v, want 30s", limited.RetryAfter)
		}
		if !strings.Contains(err.Error(), "retry after 30s") {
			t.Fatalf("Resolve() error = %v, want the retry hint in the text", err)
		}
	})

	t.Run("a 200 page with limit text is a rate limit, not a layout change", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(pageRateLimit))
		})
		srv := httptest.NewServer(mux)
		h, _ := resolver(t, srv)

		_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
		if !errors.Is(err, host.ErrRateLimited) {
			t.Fatalf("Resolve() error = %v, want host.ErrRateLimited", err)
		}
		if errors.Is(err, filekeeper.ErrLayoutChanged) {
			t.Fatal("a rate limit page must never be reported as a layout change")
		}
	})

	t.Run("status 503 on the download endpoint is limiting too", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc(pagePath, serveWithCookie(t, "abc123", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, landedPath, http.StatusFound)
		}))
		mux.HandleFunc(landedPath, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_, _ = w.Write([]byte(pageValid))
			case http.MethodPost:
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			}
		})
		srv := httptest.NewServer(mux)
		h, _ := resolver(t, srv)

		_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
		if !errors.Is(err, host.ErrRateLimited) {
			t.Fatalf("Resolve() error = %v, want host.ErrRateLimited", err)
		}
	})

	t.Run("status 404 is a gone file, not a rate limit", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
		srv := httptest.NewServer(mux)
		h, _ := resolver(t, srv)

		_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
		if !errors.Is(err, filekeeper.ErrFileGone) {
			t.Fatalf("Resolve() error = %v, want ErrFileGone", err)
		}
		if errors.Is(err, host.ErrRateLimited) {
			t.Fatal("404 must not count as a rate limit")
		}
	})
}

// eventLog records events emitted by the resolver itself.
type eventLog struct {
	mu     sync.Mutex
	events []event.Event
}

func (l *eventLog) observe(e event.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) of(kind event.Kind) []event.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []event.Event
	for _, e := range l.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestResolverReportsCooldowns(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	log := &eventLog{}
	h, _ := resolver(t, srv, filekeeper.WithObserve(log.observe))

	_, resolveErr := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
	if !errors.Is(resolveErr, host.ErrRateLimited) {
		t.Fatalf("Resolve() error = %v, want host.ErrRateLimited", resolveErr)
	}
	cooldowns := log.of(event.Cooling)
	if len(cooldowns) != 2 { // one before each of the two retried waits
		t.Fatalf("cooldown events = %d, want one per imposed pause", len(cooldowns))
	}
	if cooldowns[0].Wait != 30*time.Second {
		t.Fatalf("cooldown waits = %v, want the host-mandated 30s", cooldowns[0].Wait)
	}
	if cooldowns[0].Err == nil || cooldowns[0].Err.Error() != `host rate limit (status 429; body: "slow down"): retry after 30s` {
		t.Fatalf("cooldown reason = %v, want the typed rate limit", cooldowns[0].Err)
	}
}

func TestRateLimitWaits(t *testing.T) {
	limited := func(w http.ResponseWriter, seconds string) {
		if seconds != "" {
			w.Header().Set("Retry-After", seconds)
		}
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}

	t.Run("a bounded Retry-After is waited out until the streak gives up", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { limited(w, "30") })
		srv := httptest.NewServer(mux)
		h, s := resolver(t, srv)

		_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
		if !errors.Is(err, host.ErrRateLimited) {
			t.Fatalf("Resolve() error = %v, want host.ErrRateLimited", err)
		}
		if !strings.Contains(err.Error(), "resolve after 3 attempt(s)") {
			t.Fatalf("Resolve() error = %v, want the streak give-up wrap", err)
		}
		waits := s.all()
		if len(waits) != 2 {
			t.Fatalf("waits = %v, want two sleeps between the tries", waits)
		}
		for _, w := range waits {
			if w != 30*time.Second {
				t.Fatalf("wait = %v, want the exact 30s the host asked for", w)
			}
		}
	})

	t.Run("an unbounded Retry-After gives up without camping", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { limited(w, "300") })
		srv := httptest.NewServer(mux)
		h, s := resolver(t, srv)

		_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
		if !errors.Is(err, host.ErrRateLimited) {
			t.Fatalf("Resolve() error = %v, want host.ErrRateLimited", err)
		}
		if !strings.Contains(err.Error(), "resolve after 1 attempt(s)") {
			t.Fatalf("Resolve() error = %v, want the give-up after one look", err)
		}
		if waits := s.all(); len(waits) != 0 {
			t.Fatalf("waits = %v, want no waiting at all", waits)
		}
	})

	t.Run("an unknown delay ladders 5s then 15s then gives up", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { limited(w, "") })
		srv := httptest.NewServer(mux)
		h, s := resolver(t, srv)

		_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
		if !errors.Is(err, host.ErrRateLimited) {
			t.Fatalf("Resolve() error = %v, want host.ErrRateLimited", err)
		}
		waits := s.all()
		if len(waits) != 2 || waits[0] != 5*time.Second || waits[1] != 15*time.Second {
			t.Fatalf("waits = %v, want the exact 5s/15s ladder", waits)
		}
	})

	t.Run("a non-limit failure resets the streak", func(t *testing.T) {
		mux := http.NewServeMux()
		var landings atomic.Int64
		mux.HandleFunc(pagePath, serveWithCookie(t, "abc123", func(w http.ResponseWriter, r *http.Request) {
			if landings.Add(1) == 1 { // limited once: a real wait, not the small backoff
				limited(w, "30")
				return
			}
			http.Redirect(w, r, landedPath, http.StatusFound)
		}))
		mux.HandleFunc(landedPath, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_, _ = w.Write([]byte(pageValid))
			case http.MethodPost:
				http.Error(w, "boom", http.StatusInternalServerError)
			}
		})
		srv := httptest.NewServer(mux)
		h, s := resolver(t, srv)

		_, err := h.Resolve(context.Background(), pageURL(t, srv, pagePath))
		if errors.Is(err, host.ErrRateLimited) {
			t.Fatalf("Resolve() error = %v, mixed failures end as transient", err)
		}
		if !strings.Contains(err.Error(), "no direct download link returned") {
			t.Fatalf("Resolve() error = %v, want the post failure text", err)
		}
		// one limiting pause, then each retried try re-pays the page's own
		// countdown (5s+1) before its post and keeps the small backoff
		waits := s.all()
		if len(waits) != 5 || waits[0] != 30*time.Second || waits[1] != 6*time.Second || waits[3] != 6*time.Second {
			t.Fatalf("waits = %v, want [30s limit, 6s countdown, backoff, 6s countdown, backoff]", waits)
		}
		for _, w := range []time.Duration{waits[2], waits[4]} {
			if w > 5*time.Millisecond {
				t.Fatalf("ordinary backoff = %v, want the 1ms base delay", w)
			}
		}
	})
}

func TestMatches(t *testing.T) {
	h := filekeeper.New()

	tests := []struct {
		name  string
		raw   string
		match bool
	}{
		{name: "landing link with code and file name", raw: "https://filekeeper.net/abc123/Game.part01.rar", match: true},
		{name: "code-only link", raw: "https://filekeeper.net/abc123", match: true},
		{name: "http scheme", raw: "http://filekeeper.net/abc123", match: true},
		{name: "unknown domain", raw: "https://other.example/abc123", match: false},
		{name: "root page without code", raw: "https://filekeeper.net/", match: false},
		{name: "empty path", raw: "https://filekeeper.net", match: false},
		{name: "non file url", raw: "ftp://filekeeper.net/abc123", match: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.raw)
			if err != nil {
				t.Fatalf("test url: %v", err)
			}
			if got := h.Matches(u); got != tt.match {
				t.Errorf("Matches(%q) = %v, want %v", tt.raw, got, tt.match)
			}
		})
	}
}
