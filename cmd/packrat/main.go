// Command packrat fetches multi-part file downloads from file hosting
// services: it resolves landing pages to direct links and downloads every
// part concurrently, with resume support.
package main

import (
	"flag"
	"fmt"
	"os"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("packrat " + version)
		return
	}
	if flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "packrat: the download engine is not wired up yet (roadmap in README)")
		os.Exit(2)
	}
	usage()
	os.Exit(2)
}

func usage() {
	fmt.Fprintln(os.Stderr, "packrat — fetch multi-part downloads from file hosts")
	fmt.Fprintln(os.Stderr, "\nusage: packrat [options] links.txt")
	fmt.Fprintln(os.Stderr, "\noptions:")
	flag.PrintDefaults()
}
