package host_test

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/EliasLd/packrat/internal/host"
)

// fakeHost is a minimal Host implementation driving registry behavior tests.
type fakeHost struct {
	name     string
	domains  []string
	resolved host.DirectLink
	err      error
}

func (f *fakeHost) Name() string { return f.name }

func (f *fakeHost) Matches(u *url.URL) bool {
	for _, d := range f.domains {
		if u != nil && u.Host == d {
			return true
		}
	}
	return false
}

func (f *fakeHost) Resolve(ctx context.Context, pageURL *url.URL) (host.DirectLink, error) {
	if f.err != nil {
		return host.DirectLink{}, f.err
	}
	return f.resolved, nil
}

func TestLookup(t *testing.T) {
	alpha := &fakeHost{name: "alpha", domains: []string{"alpha.example"}}
	beta := &fakeHost{name: "beta", domains: []string{"beta.example", "shared.example"}}

	tests := []struct {
		name    string
		hosts   []host.Host
		rawURL  string
		want    string // expected host name, empty means error expected
		wantErr error
	}{
		{
			name:   "finds matching host",
			hosts:  []host.Host{alpha, beta},
			rawURL: "https://beta.example/page#file.part01.rar",
			want:   "beta",
		},
		{
			name:   "first registered host wins on shared domains",
			hosts:  []host.Host{beta, alpha},
			rawURL: "https://alpha.example/page",
			want:   "alpha",
		},
		{
			name:    "unknown domain fails with ErrUnknownHost",
			hosts:   []host.Host{alpha},
			rawURL:  "https://unknown.example/page",
			wantErr: host.ErrUnknownHost,
		},
		{
			name:    "empty registry fails with ErrUnknownHost",
			hosts:   nil,
			rawURL:  "https://alpha.example/page",
			wantErr: host.ErrUnknownHost,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := host.NewRegistry(tt.hosts...)

			u, err := url.Parse(tt.rawURL)
			if err != nil {
				t.Fatalf("invalid test url: %v", err)
			}

			got, lookupErr := registry.Lookup(u)
			if tt.wantErr != nil {
				if !errors.Is(lookupErr, tt.wantErr) {
					t.Fatalf("Lookup() error = %v, want %v", lookupErr, tt.wantErr)
				}
				if got != nil {
					t.Fatalf("Lookup() = %v, want nil host on error", got.Name())
				}
				return
			}
			if lookupErr != nil {
				t.Fatalf("Lookup() unexpected error: %v", lookupErr)
			}
			if got.Name() != tt.want {
				t.Fatalf("Lookup() = %q, want %q", got.Name(), tt.want)
			}
		})
	}
}

func TestLookupNilURL(t *testing.T) {
	registry := host.NewRegistry(&fakeHost{name: "alpha", domains: []string{"alpha.example"}})

	if _, err := registry.Lookup(nil); !errors.Is(err, host.ErrUnknownHost) {
		t.Fatalf("Lookup(nil) error = %v, want ErrUnknownHost", err)
	}
}
