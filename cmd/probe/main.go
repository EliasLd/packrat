// Command probe runs the FuckingFast resolution flow against live landing
// page URLs and reports the result, then verifies that the direct download
// server accepts both plain Go TLS and the fingerprinted client with a
// 1 MiB ranged read.
//
// Use it when download resolution breaks, to see which side changed: the
// host pages, the download host, or nothing.
//
// Usage: probe <landing-page-url>...
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	bhttp "github.com/bogdanfinn/fhttp"

	"github.com/EliasLd/packrat/internal/host/fuckingfast"
)

const (
	userAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	probeBytes = 1 << 20
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probe <landing-page-url>...")
		os.Exit(2)
	}
	ctx := context.Background()
	resolver, err := fuckingfast.New()
	if err != nil {
		fatal(err)
	}
	fingerprinted, err := fuckingfast.NewTLSClient()
	if err != nil {
		fatal(err)
	}

	failed := false
	for _, arg := range os.Args[1:] {
		u, err := url.Parse(arg)
		if err != nil {
			fatal(fmt.Errorf("bad url %q: %w", arg, err))
		}
		u.Fragment = "" // resolution targets the landing page, not the fragment

		fmt.Printf("resolving %s\n", u)
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

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "probe:", err)
	os.Exit(1)
}

func probeDownload(ctx context.Context, client *http.Client, direct *url.URL, label string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, direct.String(), nil)
	if err != nil {
		fmt.Printf("         [%s] bad request: %v\n", label, err)
		return
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", probeBytes-1))
	req.Header.Set("User-Agent", userAgent)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("         [%s] FAIL: %v\n", label, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	fmt.Printf("         [%s] %d %s  content-range=%q  in %dms\n",
		label, resp.StatusCode, resp.Proto, resp.Header.Get("Content-Range"),
		time.Since(start).Milliseconds())
}

func probeDownloadTLS(ctx context.Context, client bhttpClient, direct *url.URL) {
	req, err := bhttp.NewRequestWithContext(ctx, bhttp.MethodGet, direct.String(), nil)
	if err != nil {
		fmt.Printf("         [tls-client] bad request: %v\n", err)
		return
	}
	req.Header = bhttp.Header{
		"User-Agent": {userAgent},
		"Range":      {fmt.Sprintf("bytes=0-%d", probeBytes-1)},
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("         [tls-client] FAIL: %v\n", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	fmt.Printf("         [tls-client] %d %s  content-range=%q  in %dms\n",
		resp.StatusCode, resp.Proto, resp.Header.Get("Content-Range"),
		time.Since(start).Milliseconds())
}

// bhttpClient is the shared Do signature accepted by the TLS probe.
type bhttpClient interface {
	Do(req *bhttp.Request) (*bhttp.Response, error)
}
