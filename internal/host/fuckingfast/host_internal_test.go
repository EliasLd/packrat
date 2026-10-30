package fuckingfast

import (
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{name: "seconds", raw: "30", want: 30 * time.Second},
		{name: "padded seconds", raw: " 7 ", want: 7 * time.Second},
		{name: "http date counts as unknown", raw: "Wed, 21 Oct 2026 07:28:00 GMT", want: 0},
		{name: "garbage", raw: "soon", want: 0},
		{name: "negative", raw: "-5", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := parseRetryAfter(tt.raw)
			if got != tt.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
