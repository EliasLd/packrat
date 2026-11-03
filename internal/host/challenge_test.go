package host_test

import (
	"errors"
	"testing"

	"github.com/EliasLd/packrat/internal/host"
)

func TestClassifyChallenge(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{
			name: "the interstitial names itself in its title",
			body: `<!DOCTYPE html><html lang="en-US"><head><title>Just a moment...</title></head>`,
			want: host.ErrChallengeRequired,
		},
		{
			name: "the firewall block page names itself too",
			body: `<head><title>Attention Required! | Cloudflare</title></head>`,
			want: host.ErrChallengeRequired,
		},
		{
			name: "the captched POST verdict rides in the body",
			body: `{"error": true, "msg": "captcha verification failed"}`,
			want: host.ErrChallengeRequired,
		},
		{
			name: "ordinary copy mentioning a moment is not a challenge",
			body: `<html><head><title>Some.Repack.part01.rar</title></head><body><p>Please wait, just a moment</p></body></html>`,
			want: nil,
		},
		{
			name: "a legitimate turnstile embed is not a refusal",
			body: `<html><head><title>Some.Repack.part01.rar</title></head><body><script src="/challenge-platform/scripts/turnstile.js"></script><div class="cf-turnstile"></div></body></html>`,
			want: nil,
		},
		{
			name: "a healthy landing page is not a challenge",
			body: `<!DOCTYPE html><html><head><title>RDR2_--_fitgirl-repacks.site_--_.part001.rar</title></head><body hx-post="/f/abc123/go"></body></html>`,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := host.ClassifyChallenge([]byte(tt.body))
			if tt.want == nil {
				if got != nil {
					t.Fatalf("ClassifyChallenge() = %v on ordinary body, want nil", got)
				}
				return
			}
			if !errors.Is(got, tt.want) {
				t.Fatalf("ClassifyChallenge() = %v, want %v", got, tt.want)
			}
		})
	}
}
