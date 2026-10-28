package fuckingfast_test

import (
	"context"
	_ "embed"
	"errors"
	"io"
	"net/url"
	"strings"
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

const (
	pageURLText  = "https://fuckingfast.co/abc123"
	endpointText = "https://fuckingfast.co/f/abc123/go"
	directText   = "https://dl.fuckingfast.co/dl/token1234"
	titleText    = "My.Archive.multi.part01.rar"
)

// fakeClient responds to requests via a configurable handler and counts calls.
type fakeClient struct {
	do    func(req *bhttp.Request) (*bhttp.Response, error)
	calls int
}

func (f *fakeClient) Do(req *bhttp.Request) (*bhttp.Response, error) {
	f.calls++
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
		if handler.calls != 6 { // 3 page fetches + 3 download posts
			t.Fatalf("calls = %d, want 6", handler.calls)
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
		if handler.calls != 1 {
			t.Fatalf("calls = %d, want 1 (no retry on permanent error)", handler.calls)
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
		if handler.calls != 6 {
			t.Fatalf("calls = %d, want 6", handler.calls)
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
		if handler.calls != 0 {
			t.Fatalf("calls = %d, want 0", handler.calls)
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
