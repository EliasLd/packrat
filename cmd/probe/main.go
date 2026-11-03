// Command probe runs the resolution flow of the registered hosts against
// live landing page URLs and reports the result, then verifies that the
// direct download server accepts both plain Go TLS and the fingerprinted
// client with a 1 MiB ranged read.
//
// With --links it instead resolves every landing page listed in a links
// file, classifies each host as ok, stale, broken or infra, prints one
// machine-readable "probe: <host>=<state>" line per host and exits with
// the worst outcome (0 ok, 1 broken, 2 stale, 3 infra). The CI provider
// health workflow greps those lines and reads the exit code.
//
// Use it when download resolution breaks, to see which side changed: the
// host pages, the download host, or nothing.
//
// Usage: probe <landing-page-url>...  or:  probe --links <links-file>
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	bhttp "github.com/bogdanfinn/fhttp"

	"github.com/EliasLd/packrat/internal/host"
	"github.com/EliasLd/packrat/internal/host/filekeeper"
	"github.com/EliasLd/packrat/internal/host/fuckingfast"
)

const (
	userAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	probeBytes = 1 << 20
)

func main() {
	linksFile := flag.String("links", "", "links file whose landing pages are resolved and classified per host")
	flag.Parse()
	if flag.NArg() == 0 && *linksFile == "" {
		fmt.Fprintln(os.Stderr, "usage: probe <landing-page-url>...  or:  probe --links <links-file>")
		os.Exit(2)
	}
	ctx := context.Background()
	registry, err := buildRegistry()
	if err != nil {
		fatal(err)
	}
	fingerprinted, err := fuckingfast.NewTLSClient()
	if err != nil {
		fatal(err)
	}

	if *linksFile != "" {
		if flag.NArg() > 0 {
			fmt.Fprintln(os.Stderr, "probe: --links cannot be combined with landing page URLs")
			os.Exit(2)
		}
		os.Exit(runLinks(ctx, registry, fingerprinted, *linksFile))
	}

	failed := false
	for _, arg := range flag.Args() {
		u, err := url.Parse(arg)
		if err != nil {
			fatal(fmt.Errorf("bad url %q: %w", arg, err))
		}
		u.Fragment = "" // resolution targets the landing page, not the fragment

		resolver, err := registry.Lookup(u)
		if err != nil {
			fmt.Printf("  [FAIL] %v\n", err)
			failed = true
			continue
		}
		fmt.Printf("resolving %s via %s\n", u, resolver.Name())
		direct, err := resolver.Resolve(ctx, u)
		if err != nil {
			fmt.Printf("  [FAIL] %v\n", err)
			failed = true
			continue
		}
		fmt.Printf("  [OK]   filename: %s\n", direct.Filename)
		fmt.Printf("         direct:   %s\n", direct.URL)

		probeDownload(ctx, &http.Client{}, direct.URL, "stdlib")
		probeDownloadTLS(ctx, fingerprinted, direct.URL)
	}
	if failed {
		os.Exit(1)
	}
}

// buildRegistry wires the probe's hosts, in the same order packrat uses.
func buildRegistry() (*host.Registry, error) {
	ff, err := fuckingfast.New()
	if err != nil {
		return nil, err
	}
	return host.NewRegistry(filekeeper.New(), ff), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "probe:", err)
	os.Exit(1)
}

func probeDownload(ctx context.Context, client *http.Client, direct *url.URL, label string) readResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, direct.String(), nil)
	if err != nil {
		fmt.Printf("         [%s] bad request: %v\n", label, err)
		return readResult{}
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", probeBytes-1))
	req.Header.Set("User-Agent", userAgent)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("         [%s] FAIL: %v\n", label, err)
		return readResult{}
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		fmt.Printf("         [%s] FAIL: read body: %v\n", label, err)
		return readResult{}
	}
	fmt.Printf("         [%s] %d %s  content-range=%q  in %dms\n",
		label, resp.StatusCode, resp.Proto, resp.Header.Get("Content-Range"),
		time.Since(start).Milliseconds())
	return readResult{ok: isDownloadStatus(resp.StatusCode), answered: true}
}

func probeDownloadTLS(ctx context.Context, client bhttpClient, direct *url.URL) readResult {
	req, err := bhttp.NewRequestWithContext(ctx, bhttp.MethodGet, direct.String(), nil)
	if err != nil {
		fmt.Printf("         [tls-client] bad request: %v\n", err)
		return readResult{}
	}
	req.Header = bhttp.Header{
		"User-Agent": {userAgent},
		"Range":      {fmt.Sprintf("bytes=0-%d", probeBytes-1)},
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("         [tls-client] FAIL: %v\n", err)
		return readResult{}
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		fmt.Printf("         [tls-client] FAIL: read body: %v\n", err)
		return readResult{}
	}
	fmt.Printf("         [tls-client] %d %s  content-range=%q  in %dms\n",
		resp.StatusCode, resp.Proto, resp.Header.Get("Content-Range"),
		time.Since(start).Milliseconds())
	return readResult{ok: isDownloadStatus(resp.StatusCode), answered: true}
}

// bhttpClient is the shared Do signature accepted by the TLS probe.
type bhttpClient interface {
	Do(req *bhttp.Request) (*bhttp.Response, error)
}
