// Command packrat fetches multi-part file downloads from file hosting
// services: it resolves landing pages to direct links and downloads every
// part concurrently, with resume support.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/EliasLd/packrat/internal/engine"
	"github.com/EliasLd/packrat/internal/host"
	"github.com/EliasLd/packrat/internal/host/fuckingfast"
	"github.com/EliasLd/packrat/internal/link"
	"github.com/EliasLd/packrat/internal/ui"
)

// version is overridden at build time via -ldflags "-X main.version=..."
var version = "dev"

func main() {
	outDir := flag.String("o", "downloads", "output root directory")
	conc := flag.Int("c", 8, "download concurrency")
	resolveConc := flag.Int("rc", 3, "resolve concurrency")
	retries := flag.Int("retries", 5, "attempts per link/part")
	showVersion := flag.Bool("version", false, "print version and exit")
	dryRun := flag.Bool("dry-run", false, "parse the links file and print the planned download layout")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("packrat " + version)
		return
	}

	switch {
	case *dryRun:
		if flag.NArg() != 1 {
			usage()
			os.Exit(2)
		}
		preview(flag.Arg(0))
	case flag.NArg() == 1:
		run(flag.Arg(0), engine.Options{
			OutDir:              *outDir,
			ResolveConcurrency:  *resolveConc,
			DownloadConcurrency: *conc,
			Retries:             *retries,
			UserAgent:           fuckingfast.UserAgent,
			Observe:             ui.Plain(ui.PlainOptions{Out: os.Stdout, Warn: os.Stderr}),
		})
	default:
		usage()
		os.Exit(2)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "packrat:", err)
	os.Exit(1)
}

func run(linkFile string, opt engine.Options) {
	data, err := os.ReadFile(linkFile)
	if err != nil {
		fatal(err)
	}

	links, issues := link.Parse(data)
	for _, issue := range issues {
		fmt.Fprintf(os.Stderr, "links:%d: %s\n", issue.Line, issue.Reason)
	}

	resolver, err := fuckingfast.New()
	if err != nil {
		fatal(err)
	}
	registry := host.NewRegistry(resolver)

	// ctrl-c cancels the pipeline; partial downloads stay as .part files
	// and a rerun resumes them
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	eng := engine.New(registry, opt)
	if err := eng.Run(ctx, link.Groups(links)); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "\ninterrupted — rerun the same command to resume")
		}
		fatal(err)
	}
}

// preview parses the links file and prints the download layout that would be
// performed, without resolving or downloading anything.
func preview(linkFile string) {
	data, err := os.ReadFile(linkFile)
	if err != nil {
		fatal(err)
	}
	links, issues := link.Parse(data)
	for _, issue := range issues {
		fmt.Fprintf(os.Stderr, "links:%d: %s\n", issue.Line, issue.Reason)
	}
	groups := link.Groups(links)
	fmt.Println("planned download layout:")
	for _, g := range groups {
		fmt.Printf("  %s/\n", g.Name)
		for _, p := range g.Parts {
			fmt.Printf("    %s\n", p.FileName)
		}
	}
	fmt.Printf("\n%d link(s) across %d archive group(s) — dry run, nothing resolved or downloaded\n",
		len(links), len(groups))
}

func usage() {
	fmt.Fprintln(os.Stderr, "packrat — fetch multi-part downloads from file hosts")
	fmt.Fprintln(os.Stderr, "\nusage: packrat [options] links.txt")
	fmt.Fprintln(os.Stderr, "\noptions:")
	flag.PrintDefaults()
}
