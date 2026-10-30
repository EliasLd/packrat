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
	"strconv"
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

// UserAgent is the browser user agent sent with requests to this host,
// covering both landing page fetches and direct downloads.
const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

const hostName = "fuckingfast"

var domains = map[string]bool{"fuckingfast.co": true, "fuckingfast.net": true}

var (
	hxPostRe = regexp.MustCompile(`hx-post\s*=\s*"([^"]+)"`)
	titleRe  = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
)

// limitPhrases are the markers a rate-limit or IP-block page leaves in its
// body even when it answers status 200. Deliberately short and specific.
var limitPhrases = []string{"rate limit", "rate-limit", "ratelimit", "too many requests"}

// parseRetryAfter reads a Retry-After header; only the seconds form is
// supported, HTTP-date responses count as unknown.
func parseRetryAfter(v string) (time.Duration, bool) {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// hasLimitPhrase reports whether the body looks like a rate-limit page.
func hasLimitPhrase(body []byte) bool {
	lower := strings.ToLower(string(body))
	for _, phrase := range limitPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// rateLimited reports a "back off" signal from a failed exchange. The
// response carrying a Retry-After header always wins, then the explicit
// limit statuses, then the body sniff as the last resort for app-level
// limits that answer status 200.
func rateLimited(status int, retryAfter string, body []byte) *host.RateLimitedError {
	if ra, ok := parseRetryAfter(retryAfter); ok {
		return &host.RateLimitedError{RetryAfter: ra, Detail: fmt.Sprintf("status %d", status)}
	}
	if status == bhttp.StatusTooManyRequests || status == bhttp.StatusForbidden || status == bhttp.StatusServiceUnavailable {
		return &host.RateLimitedError{Detail: fmt.Sprintf("status %d", status)}
	}
	if len(body) > 0 && hasLimitPhrase(body) {
		return &host.RateLimitedError{Detail: "page suggests rate limiting"}
	}
	return nil
}

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
	limiter   *requestLimiter
	baseDelay time.Duration
}

// New builds a Host with the default browser-fingerprinted client.
func New(opts ...Option) (*Host, error) {
	h := &Host{retries: 3, baseDelay: time.Second, limiter: newRequestLimiter()}
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
	page, err := h.fetchPage(ctx, pageURL)
	if err != nil {
		return host.DirectLink{}, err
	}
	endpoint, filename, err := parsePage(page, pageURL)
	if err != nil {
		return host.DirectLink{}, err
	}
	directURL, err := h.triggerDownload(ctx, endpoint, pageURL)
	if err != nil {
		return host.DirectLink{}, err
	}
	direct, err := url.Parse(directURL)
	if err != nil || direct.Scheme == "" || direct.Host == "" {
		return host.DirectLink{}, fmt.Errorf("%w: %q", ErrNoDirectLink, directURL)
	}
	return host.DirectLink{URL: direct, Filename: filename}, nil
}

func (h *Host) fetchPage(ctx context.Context, pageURL *url.URL) ([]byte, error) {
	if err := h.limiter.wait(ctx); err != nil {
		return nil, err
	}
	req, err := bhttp.NewRequestWithContext(ctx, bhttp.MethodGet, pageURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build page request: %w", err)
	}
	req.Header = bhttp.Header{
		"User-Agent": {UserAgent},
		"Accept":     {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch page: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read page: %w", err)
	}
	if resp.StatusCode != bhttp.StatusOK {
		if limited := rateLimited(resp.StatusCode, resp.Header.Get("Retry-After"), body); limited != nil {
			return nil, limited
		}
		return nil, fmt.Errorf("download page status %d", resp.StatusCode)
	}
	return body, nil
}

// parsePage extracts the download endpoint and the file name. This host puts
// the file name in the page title.
//
// When the button is missing the body is checked for rate-limit markers
// first: a block page answers status 200 just like a healthy landing page,
// and misreading it as a permanent layout change would make the engine give
// up on every part while the block is merely asking for patience.
func parsePage(body []byte, pageURL *url.URL) (endpoint, filename string, err error) {
	m := hxPostRe.FindSubmatch(body)
	if m == nil {
		if hasLimitPhrase(body) {
			return "", "", &host.RateLimitedError{Detail: "page suggests rate limiting"}
		}
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

func (h *Host) triggerDownload(ctx context.Context, endpoint string, referer *url.URL) (string, error) {
	if err := h.limiter.wait(ctx); err != nil {
		return "", err
	}
	req, err := bhttp.NewRequestWithContext(ctx, bhttp.MethodPost, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build download request: %w", err)
	}
	req.Header = bhttp.Header{
		"User-Agent": {UserAgent},
		"HX-Request": {"true"},
		"Referer":    {referer.String()},
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if direct := resp.Header.Get("Hx-Redirect"); direct != "" {
		return direct, nil
	}
	body, _ := io.ReadAll(resp.Body)
	if limited := rateLimited(resp.StatusCode, resp.Header.Get("Retry-After"), body); limited != nil {
		return "", limited
	}
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
