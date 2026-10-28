// Package fuckingfast implements the host.Host contract for the FuckingFast
// file hosting service.
//
// Resolution replays the page's download mechanism with a client whose TLS
// fingerprint impersonates a real browser: GET the landing page, read the
// hx-post endpoint of the download button, POST to it, and pick up the direct
// download URL from the HX-Redirect response header.
package fuckingfast

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"regexp"
	"strings"
	"time"

	bhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"github.com/EliasLd/packrat/internal/host"
)

// ErrLayoutChanged means the landing page no longer exposes the expected
// download button. It is permanent: retrying cannot help until the site
// changes back.
var ErrLayoutChanged = errors.New("download button markup not found")

// ErrNoDirectLink means the download endpoint did not return a direct link.
// It is treated as transient and retried.
var ErrNoDirectLink = errors.New("no direct download link returned")

const (
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	hostName  = "fuckingfast"
)

var domains = map[string]bool{"fuckingfast.co": true, "fuckingfast.net": true}

var (
	hxPostRe = regexp.MustCompile(`hx-post\s*=\s*"([^"]+)"`)
	titleRe  = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
)

// HTTPClient abstracts the underlying transport so tools and tests can swap
// the fingerprinted client for fakes or alternate fingerprints.
type HTTPClient interface {
	Do(req *bhttp.Request) (*bhttp.Response, error)
}

// Option customizes a Host.
type Option func(*Host)

// WithHTTPClient replaces the transport (diagnostics and tests).
func WithHTTPClient(c HTTPClient) Option { return func(h *Host) { h.client = c } }

// WithRetries sets how often Resolve retries transient failures (default 3).
func WithRetries(n int) Option { return func(h *Host) { h.retries = n } }

// WithBaseDelay sets the base delay between attempts; the wait grows per
// attempt and receives jitter (default 1s).
func WithBaseDelay(d time.Duration) Option { return func(h *Host) { h.baseDelay = d } }

// Host resolves FuckingFast landing pages into direct download links.
type Host struct {
	client    HTTPClient
	retries   int
	baseDelay time.Duration
}

// New builds a Host with the default browser-fingerprinted client.
func New(opts ...Option) (*Host, error) {
	h := &Host{retries: 3, baseDelay: time.Second}
	for _, opt := range opts {
		opt(h)
	}
	if h.client == nil {
		client, err := NewTLSClient()
		if err != nil {
			return nil, err
		}
		h.client = client
	}
	return h, nil
}

// NewTLSClient returns the browser-fingerprinted HTTP client used for host
// pages and download servers. Exported for diagnostics tooling.
func NewTLSClient() (tls_client.HttpClient, error) {
	jar := tls_client.NewCookieJar()
	return tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeoutSeconds(45),
		tls_client.WithClientProfile(profiles.Chrome_152),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithCookieJar(jar))
}

func (h *Host) Name() string { return hostName }

// Matches reports whether the URL is a landing page of a known FuckingFast
// domain pointing at a file id.
func (h *Host) Matches(u *url.URL) bool {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if !domains[u.Hostname()] {
		return false
	}
	return u.Path[strings.LastIndex(u.Path, "/")+1:] != ""
}

// Resolve turns a landing page URL into a direct download link.
// Transient failures are retried with growing delays; ErrLayoutChanged is
// returned immediately because retrying cannot help.
func (h *Host) Resolve(ctx context.Context, pageURL *url.URL) (host.DirectLink, error) {
	if pageURL == nil {
		return host.DirectLink{}, errors.New("page URL is nil")
	}
	var lastErr error
	for attempt := 1; attempt <= h.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return host.DirectLink{}, err
		}
		direct, err := h.try(ctx, pageURL)
		if err == nil {
			return direct, nil
		}
		if errors.Is(err, ErrLayoutChanged) {
			return host.DirectLink{}, err
		}
		lastErr = err
		if waitErr := sleepContext(ctx, h.baseDelay*time.Duration(attempt)); waitErr != nil {
			return host.DirectLink{}, waitErr
		}
	}
	return host.DirectLink{}, fmt.Errorf("resolve after %d attempt(s): %w", h.retries, lastErr)
}

func (h *Host) try(ctx context.Context, pageURL *url.URL) (host.DirectLink, error) {
	page, err := fetchPage(ctx, h.client, pageURL)
	if err != nil {
		return host.DirectLink{}, err
	}
	endpoint, filename, err := parsePage(page, pageURL)
	if err != nil {
		return host.DirectLink{}, err
	}
	directURL, err := triggerDownload(ctx, h.client, endpoint, pageURL)
	if err != nil {
		return host.DirectLink{}, err
	}
	direct, err := url.Parse(directURL)
	if err != nil || direct.Scheme == "" || direct.Host == "" {
		return host.DirectLink{}, fmt.Errorf("%w: %q", ErrNoDirectLink, directURL)
	}
	return host.DirectLink{URL: direct, Filename: filename}, nil
}

func fetchPage(ctx context.Context, client HTTPClient, pageURL *url.URL) ([]byte, error) {
	req, err := bhttp.NewRequestWithContext(ctx, bhttp.MethodGet, pageURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build page request: %w", err)
	}
	req.Header = bhttp.Header{
		"User-Agent": {userAgent},
		"Accept":     {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch page: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read page: %w", err)
	}
	if resp.StatusCode != bhttp.StatusOK {
		return nil, fmt.Errorf("download page status %d", resp.StatusCode)
	}
	return body, nil
}

// parsePage extracts the download endpoint and the file name. This host puts
// the file name in the page title.
func parsePage(body []byte, pageURL *url.URL) (endpoint, filename string, err error) {
	m := hxPostRe.FindSubmatch(body)
	if m == nil {
		return "", "", fmt.Errorf("%w (page %s)", ErrLayoutChanged, pageURL.Redacted())
	}
	endpoint = string(m[1])
	if strings.HasPrefix(endpoint, "/") {
		endpoint = pageURL.Scheme + "://" + pageURL.Host + endpoint
	}
	if t := titleRe.FindSubmatch(body); t != nil {
		filename = strings.TrimSpace(string(t[1]))
	}
	return endpoint, filename, nil
}

func triggerDownload(ctx context.Context, client HTTPClient, endpoint string, referer *url.URL) (string, error) {
	req, err := bhttp.NewRequestWithContext(ctx, bhttp.MethodPost, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build download request: %w", err)
	}
	req.Header = bhttp.Header{
		"User-Agent": {userAgent},
		"HX-Request": {"true"},
		"Referer":    {referer.String()},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if direct := resp.Header.Get("Hx-Redirect"); direct != "" {
		return direct, nil
	}
	body, _ := io.ReadAll(resp.Body)
	return "", fmt.Errorf("%w: status %d, body %.80s", ErrNoDirectLink, resp.StatusCode, body)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	jitter := time.Duration(rand.Int63n(int64(d/4 + 1)))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d + jitter):
		return nil
	}
}
