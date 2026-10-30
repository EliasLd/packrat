package fuckingfast_test

import (
	"context"
	_ "embed"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bhttp "github.com/bogdanfinn/fhttp"

	"github.com/EliasLd/packrat/internal/host"
	"github.com/EliasLd/packrat/internal/host/fuckingfast"
)

//go:embed testdata/page_valid.html
var pageValid string

//go:embed testdata/page_broken.html
var pageBroken string

//go:embed testdata/page_rate_limit.html
var pageRateLimit string

const (
	pageURLText  = "https://fuckingfast.co/abc123"
	endpointText = "https://fuckingfast.co/f/abc123/go"
	directText   = "https://dl.fuckingfast.co/dl/token1234"
	titleText    = "My.Archive.multi.part01.rar"
)

// fakeClient responds to requests via a configurable handler and counts calls.
type fakeClient struct {
	do    func(req *bhttp.Request) (*bhttp.Response, error)
	calls atomic.Int64 // safe for concurrent Do, as the resolver fans out
}

func (f *fakeClient) Do(req *bhttp.Request) (*bhttp.Response, error) {
	f.calls.Add(1)
	return f.do(req)
}

func makeResponse(status int, headers map[string][]string, body string) *bhttp.Response {
	return &bhttp.Response{
		StatusCode: status,
		Header:     bhttp.Header(headers),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// happyHandler serves a valid page then a valid download redirect.
func happyHandler(t *testing.T) *fakeClient {
	t.Helper()
	return &fakeClient{do: func(req *bhttp.Request) (*bhttp.Response, error) {
		switch req.Method {
		case bhttp.MethodGet:
			return makeResponse(200, nil, pageValid), nil
		case bhttp.MethodPost:
			if got := req.URL.String(); got != endpointText {
				t.Errorf("POST url = %q, want %q", got, endpointText)
			}
			return makeResponse(200, map[string][]string{"Hx-Redirect": {directText}}, ""), nil
		default:
			return makeResponse(400, nil, "unexpected method"), nil
		}
	}}
}

func resolvedHost(t *testing.T, client *fakeClient) host.Host {
	t.Helper()
	h, err := fuckingfast.New(
		fuckingfast.WithHTTPClient(client),
		fuckingfast.WithBaseDelay(time.Millisecond),
		// pacing disabled: this helper must keep the existing tests instant
		fuckingfast.WithMinRequestInterval(0),
	)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return h
}

func TestResolve(t *testing.T) {
	pageURL, err := url.Parse(pageURLText)
	if err != nil {
		t.Fatalf("test url: %v", err)
	}

	t.Run("happy path returns the direct link with the page title as filename", func(t *testing.T) {
		client := happyHandler(t)
		h := resolvedHost(t, client)

		got, err := h.Resolve(context.Background(), pageURL)
		if err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		if got.URL.String() != directText {
			t.Fatalf("Resolve() URL = %q, want %q", got.URL, directText)
		}
		if got.Filename != titleText {
			t.Fatalf("Resolve() Filename = %q, want %q", got.Filename, titleText)
		}
		if got.Size != 0 {
			t.Fatalf("Resolve() Size = %d, want 0 (unknown upfront)", got.Size)
		}
	})

	t.Run("transient failures are retried until exhaustion", func(t *testing.T) {
		handler := &fakeClient{do: func(req *bhttp.Request) (*bhttp.Response, error) {
			if req.Method == bhttp.MethodGet {
				return makeResponse(200, nil, pageValid), nil
			}
			return makeResponse(500, nil, "boom"), nil
		}}
		h := resolvedHost(t, handler)

		_, err := h.Resolve(context.Background(), pageURL)
		if err == nil {
			t.Fatal("Resolve() expected error")
		}
		if !strings.Contains(err.Error(), "resolve after 3 attempt(s)") {
			t.Fatalf("Resolve() error = %v, want attempts exhausted", err)
		}
		if handler.calls.Load() != 6 { // 3 page fetches + 3 download posts
			t.Fatalf("calls = %d, want 6", handler.calls.Load())
		}
	})

	t.Run("layout change fails immediately without retry", func(t *testing.T) {
		handler := &fakeClient{do: func(req *bhttp.Request) (*bhttp.Response, error) {
			return makeResponse(200, nil, pageBroken), nil
		}}
		h := resolvedHost(t, handler)

		_, err := h.Resolve(context.Background(), pageURL)
		if !errors.Is(err, fuckingfast.ErrLayoutChanged) {
			t.Fatalf("Resolve() error = %v, want ErrLayoutChanged", err)
		}
		if handler.calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1 (no retry on permanent error)", handler.calls.Load())
		}
	})

	t.Run("missing direct link is retried then wrapped", func(t *testing.T) {
		handler := &fakeClient{do: func(req *bhttp.Request) (*bhttp.Response, error) {
			if req.Method == bhttp.MethodGet {
				return makeResponse(200, nil, pageValid), nil
			}
			return makeResponse(200, nil, "flash message: try again"), nil
		}}
		h := resolvedHost(t, handler)

		_, err := h.Resolve(context.Background(), pageURL)
		if !errors.Is(err, fuckingfast.ErrNoDirectLink) {
			t.Fatalf("Resolve() error = %v, want ErrNoDirectLink", err)
		}
		if handler.calls.Load() != 6 {
			t.Fatalf("calls = %d, want 6", handler.calls.Load())
		}
	})

	t.Run("canceled context stops before any request", func(t *testing.T) {
		handler := &fakeClient{}
		h := resolvedHost(t, handler)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := h.Resolve(ctx, pageURL)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Resolve() error = %v, want context.Canceled", err)
		}
		if handler.calls.Load() != 0 {
			t.Fatalf("calls = %d, want 0", handler.calls.Load())
		}
	})
}

func TestRateLimitResponses(t *testing.T) {
	pageURL, err := url.Parse(pageURLText)
	if err != nil {
		t.Fatalf("test url: %v", err)
	}

	t.Run("a 200 page with limit text is a rate limit, not a layout change", func(t *testing.T) {
		handler := &fakeClient{do: func(*bhttp.Request) (*bhttp.Response, error) {
			return makeResponse(200, nil, pageRateLimit), nil
		}}
		h := resolvedHost(t, handler)

		_, err := h.Resolve(context.Background(), pageURL)
		if !errors.Is(err, host.ErrRateLimited) {
			t.Fatalf("Resolve() error = %v, want host.ErrRateLimited", err)
		}
		if errors.Is(err, fuckingfast.ErrLayoutChanged) {
			t.Fatal("a rate limit page must never be reported as a layout change")
		}
		if !strings.Contains(err.Error(), "page suggests rate limiting") {
			t.Fatalf("Resolve() error = %v, want the limiting detail", err)
		}
	})

	t.Run("status 429 carries the parsed Retry-After", func(t *testing.T) {
		handler := &fakeClient{do: func(*bhttp.Request) (*bhttp.Response, error) {
			return makeResponse(429, map[string][]string{"Retry-After": {"30"}}, ""), nil
		}}
		h := resolvedHost(t, handler)

		_, err := h.Resolve(context.Background(), pageURL)
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

	t.Run("status 503 on the download endpoint is limiting too", func(t *testing.T) {
		handler := &fakeClient{do: func(req *bhttp.Request) (*bhttp.Response, error) {
			if req.Method == bhttp.MethodGet {
				return makeResponse(200, nil, pageValid), nil
			}
			return makeResponse(503, nil, ""), nil
		}}
		h := resolvedHost(t, handler)

		_, err := h.Resolve(context.Background(), pageURL)
		if !errors.Is(err, host.ErrRateLimited) {
			t.Fatalf("Resolve() error = %v, want host.ErrRateLimited", err)
		}
		if handler.calls.Load() != 6 {
			t.Fatalf("calls = %d, want 6", handler.calls.Load())
		}
	})

	t.Run("a 200 download answer with limit text is limiting", func(t *testing.T) {
		handler := &fakeClient{do: func(req *bhttp.Request) (*bhttp.Response, error) {
			if req.Method == bhttp.MethodGet {
				return makeResponse(200, nil, pageValid), nil
			}
			return makeResponse(200, nil, "you hit the rate limit, calm down"), nil
		}}
		h := resolvedHost(t, handler)

		_, err := h.Resolve(context.Background(), pageURL)
		if !errors.Is(err, host.ErrRateLimited) {
			t.Fatalf("Resolve() error = %v, want host.ErrRateLimited", err)
		}
		if errors.Is(err, fuckingfast.ErrNoDirectLink) {
			t.Fatal("a limit message must not be confused with a missing direct link")
		}
	})

	t.Run("an unrecognized failure keeps the old plain error", func(t *testing.T) {
		handler := &fakeClient{do: func(*bhttp.Request) (*bhttp.Response, error) {
			return makeResponse(404, nil, "not found"), nil
		}}
		h := resolvedHost(t, handler)

		_, err := h.Resolve(context.Background(), pageURL)
		if err == nil || !strings.Contains(err.Error(), "download page status 404") {
			t.Fatalf("Resolve() error = %v, want the plain status error", err)
		}
		if errors.Is(err, host.ErrRateLimited) {
			t.Fatal("404 must not count as a rate limit")
		}
	})
}

func TestMatches(t *testing.T) {
	h, err := fuckingfast.New(fuckingfast.WithHTTPClient(&fakeClient{}))
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	tests := []struct {
		name  string
		raw   string
		match bool
	}{
		{name: "landing page with id", raw: "https://fuckingfast.co/abc123#Game.part01.rar", match: true},
		{name: "alternate domain", raw: "https://fuckingfast.net/abc123", match: true},
		{name: "unknown domain", raw: "https://other.example/abc123", match: false},
		{name: "root page without id", raw: "https://fuckingfast.co/", match: false},
		{name: "empty path", raw: "https://fuckingfast.co", match: false},
		{name: "non file url", raw: "ftp://fuckingfast.co/abc123", match: false},
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

func TestResolveNilURLFails(t *testing.T) {
	h := resolvedHost(t, &fakeClient{})
	if _, err := h.Resolve(context.Background(), nil); err == nil {
		t.Fatal("Resolve(nil) expected error")
	}
}
