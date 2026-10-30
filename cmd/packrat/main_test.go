package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// canceledCtx gives a context that already is done, like after ctrl-c.
func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestExitCodesFollowTheRunOutcome(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want int
	}{
		{"clean run", context.Background(), nil, 0},
		{"failed parts", context.Background(), errors.New("2 part(s) failed"), 1},
		{"setup failure", context.Background(), errors.New("open links.txt: no such file or directory"), 1},
		{"nothing to download", context.Background(), errors.New("3 part(s) failed to resolve, nothing to download"), 1},
		{"interrupted downloads", canceledCtx(), errors.New("2 part(s) failed"), 130},
		{"interrupted resolve", canceledCtx(), fmt.Errorf("%w: resolve phase interrupted", context.Canceled), 130},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(tt.ctx, tt.err); got != tt.want {
				t.Fatalf("exitCode(%v, %v) = %d, want %d", tt.ctx, tt.err, got, tt.want)
			}
		})
	}
}
