// Package engine wires parsed links through host resolution into concurrent
// downloads: resolve every part via the host registry, then hand the direct
// links to the download client.
package engine

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/EliasLd/packrat/internal/download"
	"github.com/EliasLd/packrat/internal/host"
	"github.com/EliasLd/packrat/internal/link"
)

// Options configures an Engine. Zero values fall back to the defaults
// documented per field.
type Options struct {
	OutDir              string // output root directory (default "downloads")
	ResolveConcurrency  int    // parallel landing-page resolutions (default 3)
	DownloadConcurrency int    // parallel downloads (default 8)
	Retries             int    // attempts per part (default 5)
	UserAgent           string // sent with download requests (empty = Go default)
}

// Engine coordinates the whole pipeline for one run.
type Engine struct {
	registry *host.Registry
	opt      Options
	out      io.Writer // event and progress output
	warn     io.Writer // failures and warning output
	logMu    sync.Mutex
}

// New builds an Engine; nil writers are replaced with quiet sinks.
func New(registry *host.Registry, opt Options, out, warn io.Writer) *Engine {
	if opt.OutDir == "" {
		opt.OutDir = "downloads"
	}
	if opt.ResolveConcurrency <= 0 {
		opt.ResolveConcurrency = 3
	}
	if opt.DownloadConcurrency <= 0 {
		opt.DownloadConcurrency = 8
	}
	if opt.Retries <= 0 {
		opt.Retries = 5
	}
	if out == nil {
		out = io.Discard
	}
	if warn == nil {
		warn = io.Discard
	}
	return &Engine{registry: registry, opt: opt, out: out, warn: warn}
}

func (e *Engine) outf(format string, args ...any) {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	_, _ = fmt.Fprintf(e.out, format, args...)
}

func (e *Engine) warnf(format string, args ...any) {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	_, _ = fmt.Fprintf(e.warn, format, args...)
}

// Run resolves every part and downloads it. It returns a non-nil error when
// at least one part failed; individual failures were already reported.
func (e *Engine) Run(ctx context.Context, groups []link.Group) error {
	var parts []*link.Part
	for _, g := range groups {
		parts = append(parts, g.Parts...)
	}
	if len(parts) == 0 {
		e.outf("no links to process\n")
		return nil
	}
	e.outf("resolving %d part(s) across %d archive group(s)...\n", len(parts), len(groups))

	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		targets      []download.Target
		resolveFails atomic.Int64
	)
	sem := make(chan struct{}, e.opt.ResolveConcurrency)
	for i, p := range parts {
		wg.Add(1)
		go func(i int, p *link.Part) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			pageURL, err := url.Parse(p.PageURL)
			if err != nil {
				resolveFails.Add(1)
				e.warnf("  [resolve FAIL] %s: %v\n", p.ID, err)
				return
			}
			resolvedHost, err := e.registry.Lookup(pageURL)
			if err != nil {
				resolveFails.Add(1)
				e.warnf("  [resolve FAIL] %s: %v\n", p.ID, err)
				return
			}
			direct, err := resolvedHost.Resolve(ctx, pageURL)
			if err != nil {
				resolveFails.Add(1)
				e.warnf("  [resolve FAIL] %s: %v\n", p.ID, err)
				return
			}
			mu.Lock()
			targets = append(targets, download.Target{
				URL:      direct.URL,
				PageURL:  pageURL,
				DirName:  p.DirName,
				FileName: e.finalName(p, direct.Filename),
			})
			mu.Unlock()
			e.outf("  [%2d/%2d] %s OK\n", i+1, len(parts), e.finalName(p, direct.Filename))
		}(i, p)
	}
	wg.Wait()

	if ctx.Err() != nil {
		return fmt.Errorf("%w: %s", context.Canceled, "resolve phase interrupted")
	}
	if int64(len(targets)) == 0 {
		return fmt.Errorf("%d part(s) failed to resolve, nothing to download", resolveFails.Load())
	}
	if f := resolveFails.Load(); f > 0 {
		e.warnf("%d part(s) failed to resolve\n", f)
	}

	downloader := download.New(download.Options{
		OutDir:    e.opt.OutDir,
		Workers:   e.opt.DownloadConcurrency,
		Retries:   e.opt.Retries,
		UserAgent: e.opt.UserAgent,
		Relink:    e.relink,
		Output:    e.out,
	})
	summary := downloader.Run(ctx, targets)
	for _, f := range summary.Failures {
		e.warnf("  [FAIL] %s: %v\n", f.Target.FileName, f.Err)
	}
	e.outf("done: %d/%d part(s) downloaded, %d skipped, %d resolve failure(s)\n",
		summary.Done, len(targets), summary.Skipped, resolveFails.Load())

	if failed := len(summary.Failures) + int(resolveFails.Load()); failed > 0 {
		return fmt.Errorf("%d part(s) failed", failed)
	}
	return nil
}

// finalName picks the file name for a part: the archive fragment when the
// fragment was present, the host-provided name otherwise.
func (e *Engine) finalName(p *link.Part, hostProvided string) string {
	if p.Name == "" && hostProvided != "" {
		return link.Sanitize(hostProvided)
	}
	return p.FileName
}

// relink re-resolves a target whose direct link expired; wired into the
// download client as recovery hook.
func (e *Engine) relink(ctx context.Context, t download.Target) (*url.URL, error) {
	resolvedHost, err := e.registry.Lookup(t.PageURL)
	if err != nil {
		return nil, err
	}
	direct, err := resolvedHost.Resolve(ctx, t.PageURL)
	if err != nil {
		return nil, err
	}
	return direct.URL, nil
}
