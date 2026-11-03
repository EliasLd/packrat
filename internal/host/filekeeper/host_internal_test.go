package filekeeper

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/EliasLd/packrat/internal/host"
)

func parseTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("test url: %v", err)
	}
	return u
}

func TestCountdownTagParses(t *testing.T) {
	tag := `<html>
	<title>Download My.Archive.multi.part01.rar</title>
	<div id="download-countdown" class="w-full"
	     data-countdown="5"
	     data-code="abc123"
	     data-referer=""
	     data-rand=""
	     data-method=""
	     data-has-password="false"
	     data-has-captcha="false">
	</div></html>`

	form, name, err := parsePage([]byte(tag), parseTestURL(t, "https://filekeeper.net/abc123"))
	if err != nil {
		t.Fatalf("parsePage() error: %v", err)
	}
	if form.id != "abc123" || form.countdown != 5 || form.method != "Free download" || form.rand != "" || form.referer != "" {
		t.Fatalf("parsePage() form = %+v, want the live page's defaults", form)
	}
	if name != "My.Archive.multi.part01.rar" {
		t.Fatalf("parsePage() name = %q, want the title without its prefix", name)
	}
}

func TestCountdownTagScrambledAttrs(t *testing.T) {
	// attributes may sit on either side of the id inside the open tag
	tag := `<div class="w-full" data-countdown="3" data-code="kk321" ` +
		`data-rand="rr" data-referer="up" data-method="Custom" id="download-countdown" ` +
		`data-has-captcha="false" data-has-password="false">`

	form, _, err := parsePage([]byte(tag), parseTestURL(t, "https://filekeeper.net/kk321"))
	if err != nil {
		t.Fatalf("parsePage() error: %v", err)
	}
	if form.id != "kk321" || form.countdown != 3 || form.rand != "rr" || form.referer != "up" || form.method != "Custom" {
		t.Fatalf("parsePage() form = %+v, want the scattered attrs read out", form)
	}
}

func TestCountdownTagClassifications(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{
			name: "captcha flag refuses permanently",
			body: `<div id="download-countdown" data-countdown="5" data-code="abc" data-has-captcha="true">`,
			want: ErrCAPTCHARequired,
		},
		{
			name: "password flag refuses permanently",
			body: `<div id="download-countdown" data-countdown="5" data-code="abc" data-has-password="true">`,
			want: ErrPasswordProtected,
		},
		{
			name: "empty file code is a layout change",
			body: `<div id="download-countdown" data-countdown="5" data-code="" data-has-captcha="false">`,
			want: ErrLayoutChanged,
		},
		{
			name: "unreadable countdown is a layout change",
			body: `<div id="download-countdown" data-countdown="" data-code="abc">`,
			want: ErrLayoutChanged,
		},
		{
			name: "a file-not-found body wins over the missing markup",
			body: `<h1>File Not Found</h1>`,
			want: ErrFileGone,
		},
		{
			name: "a cloudflare challenge page is not a layout change",
			body: `<!DOCTYPE html><html lang="en-US"><head><title>Just a moment...</title></head>`,
			want: host.ErrChallengeRequired,
		},
		{
			name: "a limit text body is a typed rate limit, not a layout change",
			body: `<p>Too many requests</p>`,
			want: nil, // asserted as host.ErrRateLimited below
		},
		{
			name: "an unrelated body is a layout change",
			body: `<p>Something unexpected happened</p>`,
			want: ErrLayoutChanged,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parsePage([]byte(tt.body), parseTestURL(t, "https://filekeeper.net/abc123"))
			if tt.want == nil {
				var limited *host.RateLimitedError
				if !errors.As(err, &limited) ||
					!strings.Contains(limited.Detail, "page suggests rate limiting") ||
					!strings.Contains(limited.Detail, "Too many requests") {
					t.Fatalf("parsePage() error = %v, want the typed rate limit with its body", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("parsePage() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestPageNameStripsThePrefix(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{name: "plain title", title: "<title>Download Game.rar</title>", want: "Game.rar"},
		{name: "extra spaces", title: "<title>Download   Game.rar </title>", want: "Game.rar"},
		{name: "lowercase prefix", title: "<title>download Game.rar</title>", want: "Game.rar"},
		{name: "no prefix stays as is", title: "<title>Game.rar</title>", want: "Game.rar"},
		{name: "only the prefix stays empty", title: "<title>Download </title>", want: ""},
		{name: "missing title", title: "<html></html>", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pageName([]byte(tt.title)); got != tt.want {
				t.Fatalf("pageName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCountdownWait(t *testing.T) {
	tests := []struct {
		countdown int
		want      time.Duration
	}{
		{countdown: 0, want: time.Second},
		{countdown: 5, want: 6 * time.Second},
		{countdown: 10, want: 11 * time.Second},
	}
	for _, tt := range tests {
		if got := countdownWait(tt.countdown); got != tt.want {
			t.Fatalf("countdownWait(%d) = %v, want %v", tt.countdown, got, tt.want)
		}
	}
}

func TestFileCode(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "https://filekeeper.net/abc123/Game.part01.rar", want: "abc123"},
		{raw: "https://filekeeper.net/abc123", want: "abc123"},
		{raw: "https://filekeeper.net/", want: ""},
		{raw: "https://filekeeper.net", want: ""},
	}
	for _, tt := range tests {
		u := parseTestURL(t, tt.raw)
		if got := fileCode(u); got != tt.want {
			t.Fatalf("fileCode(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}
