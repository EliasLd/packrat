// Package filekeeper implements the host.Host contract for FileKeeper
// (filekeeper.net), resolved over plain HTTP with the standard library: the
// landing link redirects to a countdown page, the announced countdown is
// waited out exactly (the server validates it), and one form post answers
// with the signed direct download URL in a redirect Location header.
//
// No cookie jar is shared. The only state the flow needs, the file_code
// cookie, is known from the link itself and forged on every request, so
// concurrent resolves cannot overwrite each other's session.
package filekeeper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/EliasLd/packrat/internal/event"
	"github.com/EliasLd/packrat/internal/host"
)

// ErrFileGone means the file no longer exists (or the code never matched
// one). It carries the shared host.ErrFileGone sentinel, so every host's
// dead file reads the same to callers.
var ErrFileGone = host.ErrFileGone

// ErrCAPTCHARequired means this particular file asks for a captcha. packrat
// never solves captchas, so the file fails permanently with a clear message.
var ErrCAPTCHARequired = errors.New("file requires a captcha")

// ErrPasswordProtected means this particular file needs a password that only
// its owner knows. Permanent for the same reason.
var ErrPasswordProtected = errors.New("file is password protected")

// ErrLayoutChanged means the download page no longer exposes the expected
// countdown markup. It is permanent: retrying cannot help until the site
// changes back.
var ErrLayoutChanged = errors.New("download countdown markup not found")

// ErrNoDirectLink means the download endpoint did not return a direct link.
// It is treated as transient and retried.
var ErrNoDirectLink = errors.New("no direct download link returned")

// UserAgent is the browser user agent sent with requests to this host,
// covering both landing page fetches and the download handoff.
const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// Rate-limit answers route through the shared wait policy carried by
// internal/host; this host keeps the tuned defaults.
var limitWaitPolicy = host.DefaultWaitPolicy()

const (
	hostName       = "filekeeper"
	requestTimeout = 45 * time.Second
	// defaultMethod is what the page's own script submits when the countdown
	// element carries no method: the free tier's button label.
	defaultMethod = "Free download"
	// defaultTitlePrefix is what this host puts before the file name in the
	// page title.
	defaultTitlePrefix = "Download "
)

var domains = map[string]bool{"filekeeper.net": true}

// The countdown element spreads over one open tag spanning several lines,
// attrs placed either side of the id, so the whole tag is captured first and
// the individual attrs are read out of it afterwards.
var (
	countdownTagRe   = regexp.MustCompile(`<\w+\b[^>]*\bid="download-countdown"[^>]*>`)
	dataCountdownRe  = regexp.MustCompile(`data-countdown\s*=\s*"(\d+)"`)
	dataCodeRe       = regexp.MustCompile(`data-code\s*=\s*"([^"]*)"`)
	dataRandRe       = regexp.MustCompile(`data-rand\s*=\s*"([^"]*)"`)
	dataRefererRe    = regexp.MustCompile(`data-referer\s*=\s*"([^"]*)"`)
	dataMethodRe     = regexp.MustCompile(`data-method\s*=\s*"([^"]*)"`)
	dataHasCaptchaRe = regexp.MustCompile(`data-has-captcha\s*=\s*"([^"]*)"`)
	dataHasPassword  = regexp.MustCompile(`data-has-password\s*=\s*"([^"]*)"`)
	titleRe          = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
	fileGoneRe       = regexp.MustCompile(`(?i)file\s+not\s+found`)
)

// HTTPClient abstracts the underlying transport so tools and tests can swap
// the real clients for fakes or proxies.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Option customizes a Host.
type Option func(*Host)

// WithHTTPClient replaces both transports — the redirect-following page
// fetcher and the stop-at-first-redirect download poster (diagnostics and
// tests).
func WithHTTPClient(c HTTPClient) Option { return func(h *Host) { h.pages, h.posts = c, c } }

// WithRetries sets how often Resolve retries transient failures (default 3).
func WithRetries(n int) Option { return func(h *Host) { h.retries = n } }

// WithBaseDelay sets the base delay between attempts; the wait grows per
// attempt and receives jitter (default 1s).
func WithBaseDelay(d time.Duration) Option { return func(h *Host) { h.baseDelay = d } }

// WithSleep replaces how waits are performed: the validated countdown and
// the rate-limit pauses (tests).
func WithSleep(s host.Sleeper) Option { return func(h *Host) { h.sleep = s } }

// WithObserve relays events from the resolver: the cooldowns a rate-limited
// host imposes, so renderers can show them alongside the pipeline's own
// events. Nil (default) emits nothing.
func WithObserve(o event.Observer) Option { return func(h *Host) { h.observe = o } }

// Host resolves FileKeeper landing links into direct download links.
type Host struct {
	pages     HTTPClient // redirects followed: landing link to countdown page
	posts     HTTPClient // redirects stopped: the handoff's Location is the link
	retries   int
	sleep     host.Sleeper
	observe   event.Observer
	baseDelay time.Duration
}

// New builds a Host over the standard library clients.
func New(opts ...Option) *Host {
	h := &Host{retries: 3, baseDelay: time.Second, sleep: host.SleepFor}
	h.pages = &http.Client{Timeout: requestTimeout}
	h.posts = &http.Client{Timeout: requestTimeout, CheckRedirect: stopAtRedirect}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// stopAtRedirect keeps the download handoff one-legged: the post's redirect
// Location is the link itself, following it would start the real download.
func stopAtRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (h *Host) Name() string { return hostName }

// Matches reports whether the URL is a landing link of a known FileKeeper
// domain pointing at a file code.
func (h *Host) Matches(u *url.URL) bool {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return domains[u.Hostname()] && fileCode(u) != ""
}

// Resolve turns a landing link into a direct download link. Transient
// failures are retried with growing delays; the permanent errors return
// immediately because retrying cannot help.
func (h *Host) Resolve(ctx context.Context, pageURL *url.URL) (host.DirectLink, error) {
	if pageURL == nil {
		return host.DirectLink{}, errors.New("page URL is nil")
	}
	var lastErr error
	var limitStreak int
	for attempt := 1; attempt <= h.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return host.DirectLink{}, err
		}
		direct, err := h.try(ctx, pageURL)
		if err == nil {
			return direct, nil
		}
		if permanent(err) {
			return host.DirectLink{}, err
		}
		lastErr = err

		// ordinary transient failures keep the small per-attempt backoff;
		// rate limit answers replace it with the host-mandated pause and
		// may end the retries outright (see the shared WaitPolicy). A
		// streak resets on any non-limit failure so an unrelated blip
		// cannot trigger it.
		wait := h.baseDelay * time.Duration(attempt)
		var limited *host.RateLimitedError
		if errors.As(err, &limited) {
			limitStreak++
			var giveUp bool
			if wait, giveUp = limitWaitPolicy.Wait(limited.RetryAfter, attempt, limitStreak); giveUp {
				return host.DirectLink{}, fmt.Errorf("resolve after %d attempt(s): %w", attempt, err)
			}
			if h.observe != nil {
				h.observe(event.Event{Kind: event.Cooling, Err: limited, Wait: wait})
			}
		} else {
			limitStreak = 0
			wait = jitterUp(wait) // the ordinary backoff keeps its breathing fuzz
		}
		if waitErr := h.sleep(ctx, wait); waitErr != nil {
			return host.DirectLink{}, waitErr
		}
	}
	return host.DirectLink{}, fmt.Errorf("resolve after %d attempt(s): %w", h.retries, lastErr)
}

// permanent reports failures retrying cannot fix: the file is gone, asks
// for something packrat will not do, changed its page, or the bot front
// demands a browser.
func permanent(err error) bool {
	return errors.Is(err, ErrFileGone) ||
		errors.Is(err, ErrCAPTCHARequired) ||
		errors.Is(err, ErrPasswordProtected) ||
		errors.Is(err, ErrLayoutChanged) ||
		errors.Is(err, host.ErrChallengeRequired)
}

// jitterUp widens a wait by up to 25 percent, upward only. The ordinary
// per-attempt backoff uses it so waits never repeat a uniform rhythm.
func jitterUp(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int63n(int64(d/4+1)))
}

// countdownWait is the pause the server validates: the announced seconds
// plus one spare. A countdown is a floor — never jittered down.
func countdownWait(countdown int) time.Duration {
	return time.Duration(countdown+1) * time.Second
}

func (h *Host) try(ctx context.Context, pageURL *url.URL) (host.DirectLink, error) {
	landed, body, err := h.fetchPage(ctx, pageURL)
	if err != nil {
		return host.DirectLink{}, err
	}
	form, filename, err := parsePage(body, pageURL)
	if err != nil {
		return host.DirectLink{}, err
	}
	if err := h.sleep(ctx, countdownWait(form.countdown)); err != nil {
		return host.DirectLink{}, err
	}
	direct, err := h.submitDownload(ctx, landed, form, fileCode(pageURL))
	if err != nil {
		return host.DirectLink{}, err
	}
	return host.DirectLink{URL: direct, Filename: filename}, nil
}

// fetchPage loads the page the link lands on: the landing URL redirects to
// the countdown page, whose URL is where the download request then goes.
// The file_code cookie is forged from the link's own code on every request —
// the landing redirect reaches the same host, so the header carries over.
func (h *Host) fetchPage(ctx context.Context, pageURL *url.URL) (*url.URL, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL.String(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build page request: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Cookie", "file_code="+fileCode(pageURL))

	resp, err := h.pages.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch page: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read page: %w", err)
	}
	landed := pageURL
	if resp.Request != nil && resp.Request.URL != nil {
		landed = resp.Request.URL
	}
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return nil, nil, ErrFileGone
	}
	if resp.StatusCode != http.StatusOK {
		if challenge := host.ClassifyChallenge(body); challenge != nil {
			return nil, nil, fmt.Errorf("fetch page: %w", challenge)
		}
		if limited := host.ClassifyLimit(resp.StatusCode, resp.Header.Get("Retry-After"), body); limited != nil {
			return nil, nil, fmt.Errorf("fetch page: %w", limited)
		}
		return nil, nil, fmt.Errorf("download page status %d", resp.StatusCode)
	}
	return landed, body, nil
}

// downloadForm carries the fields the countdown page prepares for the
// download request.
type downloadForm struct {
	id        string
	rand      string
	referer   string
	method    string
	countdown int
}

// parsePage extracts the countdown form and the file name. This host
// announces the file name in the page title, behind a "Download " prefix.
//
// When the countdown element is missing, the body is classified first: dead
// files land on a "File Not Found" page, and block pages answer status 200 —
// mistaking either for a permanent layout change would burn retries on a
// dead file or drop recoverable links.
func parsePage(body []byte, pageURL *url.URL) (downloadForm, string, error) {
	if fileGoneRe.Match(body) {
		return downloadForm{}, "", ErrFileGone
	}
	tag := countdownTagRe.Find(body)
	if tag == nil {
		if challenge := host.ClassifyChallenge(body); challenge != nil {
			return downloadForm{}, "", fmt.Errorf("%w (page %s)", challenge, pageURL.Redacted())
		}
		if limited := host.ClassifyLimit(http.StatusOK, "", body); limited != nil {
			return downloadForm{}, "", limited
		}
		return downloadForm{}, "", fmt.Errorf("%w (page %s)", ErrLayoutChanged, pageURL.Redacted())
	}

	m := dataCountdownRe.FindSubmatch(tag)
	if m == nil {
		return downloadForm{}, "", fmt.Errorf("%w: countdown not readable", ErrLayoutChanged)
	}
	countdown, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return downloadForm{}, "", fmt.Errorf("%w: countdown %q is not a number", ErrLayoutChanged, m[1])
	}
	if attrTrue(dataHasCaptchaRe, tag) {
		return downloadForm{}, "", ErrCAPTCHARequired
	}
	if attrTrue(dataHasPassword, tag) {
		return downloadForm{}, "", ErrPasswordProtected
	}
	form := downloadForm{
		id:        attr(dataCodeRe, tag),
		rand:      attr(dataRandRe, tag),
		referer:   attr(dataRefererRe, tag),
		method:    methodValue(dataMethodRe, tag),
		countdown: countdown,
	}
	if form.id == "" {
		return downloadForm{}, "", fmt.Errorf("%w: file code missing", ErrLayoutChanged)
	}
	return form, pageName(body), nil
}

// submitDownload replays the free-download button: one urlencoded form post
// to the landed page. The host answers with a redirect whose Location is the
// signed direct download URL; the posts client never follows it.
func (h *Host) submitDownload(ctx context.Context, landed *url.URL, form downloadForm, code string) (*url.URL, error) {
	formData := url.Values{
		"op":          {"download2"},
		"id":          {form.id},
		"rand":        {form.rand},
		"referer":     {form.referer},
		"method_free": {form.method},
		"down_direct": {"1"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, landed.String(), strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", landed.String())
	req.Header.Set("Cookie", "file_code="+code)

	resp, err := h.posts.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		if location := resp.Header.Get("Location"); location != "" {
			ref, err := url.Parse(location)
			if err != nil {
				return nil, fmt.Errorf("%w: %q", ErrNoDirectLink, location)
			}
			return landed.ResolveReference(ref), nil
		}
	}
	body, _ := io.ReadAll(resp.Body)
	if fileGoneRe.Match(body) {
		return nil, ErrFileGone
	}
	if challenge := host.ClassifyChallenge(body); challenge != nil {
		return nil, fmt.Errorf("download request: %w", challenge)
	}
	if limited := host.ClassifyLimit(resp.StatusCode, resp.Header.Get("Retry-After"), body); limited != nil {
		return nil, fmt.Errorf("download request: %w", limited)
	}
	return nil, fmt.Errorf("%w: status %d, body %.80s", ErrNoDirectLink, resp.StatusCode, body)
}

// fileCode extracts the file code a landing link carries as its first path
// segment.
func fileCode(u *url.URL) string {
	for _, s := range strings.Split(u.Path, "/") {
		if s != "" {
			return s
		}
	}
	return ""
}

func attr(re *regexp.Regexp, tag []byte) string {
	if m := re.FindSubmatch(tag); m != nil {
		return string(m[1])
	}
	return ""
}

func attrTrue(re *regexp.Regexp, tag []byte) bool {
	return strings.EqualFold(attr(re, tag), "true")
}

func methodValue(re *regexp.Regexp, tag []byte) string {
	if m := attr(re, tag); m != "" {
		return m
	}
	return defaultMethod
}

// pageName extracts the file name this host announces in the page title,
// behind its "Download " prefix. A title that is just the prefix carries no
// name at all.
func pageName(body []byte) string {
	m := titleRe.FindSubmatch(body)
	if m == nil {
		return ""
	}
	name := strings.TrimSpace(string(m[1]))
	if strings.EqualFold(name, strings.TrimSpace(defaultTitlePrefix)) {
		return ""
	}
	if len(name) > len(defaultTitlePrefix) && strings.EqualFold(name[:len(defaultTitlePrefix)], defaultTitlePrefix) {
		name = strings.TrimSpace(name[len(defaultTitlePrefix):])
	}
	return name
}
