package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"

	"github.com/EliasLd/packrat/internal/host"
	"github.com/EliasLd/packrat/internal/link"
)

// The links-file mode (probe --links) resolves a fixture of real landing
// pages and condenses the outcome into one state per host, so a CI health
// workflow can tell four problems apart without reading the whole log:
//
//	ok     at least one link flowed end to end
//	blocked the host's bot front challenges the CI egress; no action
//	       exists, the handler is fine, so the run stays green
//	stale  the fixture's codes died: dead files only, handler fine
//	broken the page or download flow changed: the handler needs a fix
//	infra  transport failure: requests never got an answer

// Class ranks how badly one link or one host fared. Ascending order is
// the aggregation order between hosts. Within one host, a single working
// link covers the environmental classes (the codes died, the egress was
// challenged) because the handler is proven fine; a broken or infra link
// decides regardless. See hostState.
type Class int

const (
	ClassOK Class = iota
	ClassBlocked
	ClassStale
	ClassBroken
	ClassInfra
)

// String renders the state name used in machine-readable state lines.
func (c Class) String() string {
	switch c {
	case ClassOK:
		return "ok"
	case ClassBlocked:
		return "blocked"
	case ClassStale:
		return "stale"
	case ClassBroken:
		return "broken"
	case ClassInfra:
		return "infra"
	}
	return "unknown"
}

// exitCode maps a run's worst class onto the workflow contract: 0 green,
// 1 broken, 2 stale, 3 infra. Green covers ok and blocked alike: a bot
// challenge is about the CI egress, nothing in the repo can answer it,
// and a failing run is the only channel that notifies. Infra is the only
// class a retry can cure, so it is the only one the workflow retries on.
func (c Class) exitCode() int {
	switch c {
	case ClassOK, ClassBlocked:
		return 0
	case ClassBroken:
		return 1
	case ClassStale:
		return 2
	default:
		return 3
	}
}

// readResult condenses one ranged-read probe into what classification
// needs.
type readResult struct {
	ok       bool // a valid download answer arrived
	answered bool // the server answered at all, good or bad
}

// isDownloadStatus reports whether the answer carries download bytes: a
// full 200 when the server ignores the range, or the requested 206.
func isDownloadStatus(status int) bool {
	return status == http.StatusOK || status == http.StatusPartialContent
}

// classifyResolve sorts a resolve failure: dead codes are the fixture's
// problem (stale), a bot challenge is the egress's problem (blocked),
// silent transports are infra, everything else means the page or the flow
// changed.
func classifyResolve(err error) Class {
	switch {
	case errors.Is(err, host.ErrFileGone):
		return ClassStale
	case errors.Is(err, host.ErrChallengeRequired):
		return ClassBlocked
	case isTransport(err):
		return ClassInfra
	default:
		return ClassBroken
	}
}

// isTransport reports failures that never got an answer: DNS, dial, TLS
// and timeout errors. Unrecognized shapes read as broken on purpose — a
// page the classifier cannot name still deserves a human.
func isTransport(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// classifyRead condenses the two ranged-read results: one readable client
// proves the download side works, reads that never answered mean transport
// trouble, and any clean refusal means the download edge changed.
func classifyRead(std, tls readResult) Class {
	if std.ok || tls.ok {
		return ClassOK
	}
	if !std.answered && !tls.answered {
		return ClassInfra
	}
	return ClassBroken
}

// runLinks executes the links-file mode and returns the process exit code.
// Links resolve sequentially, so every host's own pacing applies.
func runLinks(ctx context.Context, reg *host.Registry, fingerprinted bhttpClient, path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		return 1
	}
	links, issues := link.Parse(data)
	for _, issue := range issues {
		fmt.Fprintf(os.Stderr, "  [SKIP] line %d: %s\n", issue.Line, issue.Reason)
	}
	if len(links) == 0 {
		fmt.Fprintln(os.Stderr, "probe: no usable links in", path)
		return 1
	}

	// Bucket the fixture's landing pages per host, keeping the registration
	// order for the state lines and the file order for the probes.
	bucketed := map[string][]*url.URL{}
	strays := 0
	for _, l := range links {
		u, err := url.Parse(l.PageURL)
		if err == nil {
			var h host.Host
			h, err = reg.Lookup(u)
			if err == nil {
				bucketed[h.Name()] = append(bucketed[h.Name()], u)
				continue
			}
		}
		fmt.Printf("  [FAIL] %v\n", err)
		strays++
	}

	var states []Class
	for _, h := range reg.Hosts() {
		pages := bucketed[h.Name()]
		state := ClassBroken // a host without fixture links goes unchecked
		if len(pages) > 0 {
			classes := make([]Class, 0, len(pages))
			for _, u := range pages {
				classes = append(classes, probeLink(ctx, h, u, fingerprinted))
			}
			state = hostState(classes)
		} else {
			fmt.Printf("  [WARN] no %s links in %s\n", h.Name(), path)
		}
		fmt.Printf("probe: %s=%s\n", h.Name(), state)
		states = append(states, state)
	}

	// Links no host claimed are a fixture problem, not a handler one; they
	// still fail the run through their own pseudo-state line.
	if strays > 0 {
		fmt.Printf("probe: unknown=broken\n")
		states = append(states, ClassBroken)
	}
	return runState(states).exitCode()
}

// hostState collapses one host's link outcomes. A broken (flow misbehaved)
// or infra (nothing answered) link decides the host whatever its siblings
// did — infra ranks above broken because it is the only class a retry can
// cure. Without one, a single working link proves the handler, so the
// environmental classes (dead fixture codes, bot challenges) stay in the
// log instead of the verdict; dead codes outrank challenges there, as the
// fixture refresh they demand is the more pressing errand.
func hostState(classes []Class) Class {
	var actionable Class
	working := false
	for _, c := range classes {
		switch c {
		case ClassOK:
			working = true
		case ClassBroken, ClassInfra:
			if c > actionable {
				actionable = c
			}
		}
	}
	if actionable != ClassOK {
		return actionable
	}
	if working {
		return ClassOK
	}
	for _, c := range classes {
		if c == ClassStale {
			return ClassStale
		}
	}
	return ClassBlocked
}

// runState collapses the hosts' states into the run's verdict. The worst
// state wins as usual, except one shape: when EVERY host got challenged
// the run checked nothing and a green verdict would fake coverage, so the
// egress degradation reads as broken and pages.
func runState(states []Class) Class {
	if len(states) > 0 {
		all := true
		for _, s := range states {
			all = all && s == ClassBlocked
		}
		if all {
			return ClassBroken
		}
	}
	worst := ClassOK
	for _, s := range states {
		if s > worst {
			worst = s
		}
	}
	return worst
}

// probeLink resolves one landing page and ranged-reads the direct download
// on both clients, mirroring the positional mode's output.
func probeLink(ctx context.Context, h host.Host, u *url.URL, fingerprinted bhttpClient) Class {
	fmt.Printf("resolving %s via %s\n", u, h.Name())
	direct, err := h.Resolve(ctx, u)
	if err != nil {
		fmt.Printf("  [FAIL] %v\n", err)
		return classifyResolve(err)
	}
	fmt.Printf("  [OK]   filename: %s\n", direct.Filename)
	fmt.Printf("         direct:   %s\n", direct.URL)

	std := probeDownload(ctx, &http.Client{}, direct.URL, "stdlib")
	tls := probeDownloadTLS(ctx, fingerprinted, direct.URL)
	return classifyRead(std, tls)
}
