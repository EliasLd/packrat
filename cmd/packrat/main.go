// Command packrat fetches multi-part file downloads from file hosting
// services: it resolves landing pages to direct links and downloads every
// part concurrently, with resume support.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/EliasLd/packrat/internal/link"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	dryRun := flag.Bool("dry-run", false, "parse the links file and print the planned download layout")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("packrat " + version)
		return
	}
	if *dryRun {
		if flag.NArg() != 1 {
			usage()
			os.Exit(2)
		}
		preview(flag.Arg(0))
		return
	}
	if flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "packrat: the download engine is not wired up yet (roadmap in README)")
		os.Exit(2)
	}
	usage()
	os.Exit(2)
}

// preview parses the links file and prints the download layout that would be
// performed, without resolving or downloading anything.
func preview(linkFile string) {
	data, err := os.ReadFile(linkFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "packrat:", err)
		os.Exit(1)
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
