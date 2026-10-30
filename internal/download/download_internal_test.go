package download

import (
	"testing"
	"time"
)

func TestClampRetryWait(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "zero falls back to the floor", in: 0, want: 2 * time.Second},
		{name: "sub-second delays clamp up", in: 400 * time.Millisecond, want: 2 * time.Second},
		{name: "exact values pass through", in: 30 * time.Second, want: 30 * time.Second},
		{name: "long pauses clamp down", in: 25 * time.Minute, want: retryCap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampRetryWait(tt.in); got != tt.want {
				t.Fatalf("clampRetryWait(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
