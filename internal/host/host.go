// Package host defines the contract every supported file host fulfills and
// the registry that dispatches landing page URLs to implementations.
package host

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

// ErrUnknownHost is returned by a Registry lookup when no host claims a URL.
// Use errors.Is to test for it.
var ErrUnknownHost = errors.New("no registered host matches this URL")

// DirectLink is a resolved, directly downloadable file with the metadata the
// downloader needs to save and verify it.
type DirectLink struct {
	URL      *url.URL // direct download URL
	Filename string   // target file name, empty when unknown
	Size     int64    // total size in bytes, 0 when unknown upfront
}

// Host resolves a landing page URL into a DirectLink.
//
// Implementations may hold browser-spoofing HTTP clients and their own retry
// policies, so Resolve must be safe for concurrent use.
type Host interface {
	// Name returns the host identifier used in logs and output.
	Name() string
	// Matches reports whether this host handles the given landing page URL.
	Matches(u *url.URL) bool
	// Resolve turns a landing page URL into a DirectLink. Implementations
	// should retry transient failures themselves; Resolve returning an error
	// means the link could not be obtained.
	Resolve(ctx context.Context, pageURL *url.URL) (DirectLink, error)
}

// Registry dispatches landing page URLs to the first registered host that
// claims them. Registration order defines precedence when several hosts
// match the same URL.
type Registry struct {
	hosts []Host
}

func NewRegistry(hosts ...Host) *Registry {
	return &Registry{hosts: hosts}
}

// Lookup returns the first registered host that handles u.
func (r *Registry) Lookup(u *url.URL) (Host, error) {
	for _, h := range r.hosts {
		if u != nil && h.Matches(u) {
			return h, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownHost, describeURL(u))
}

// describeURL renders a URL for error messages without leaking credentials.
func describeURL(u *url.URL) string {
	if u == nil {
		return "<nil>"
	}
	return u.Redacted()
}
