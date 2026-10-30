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

	"github.com/EliasLd/packrat/internal/download"
	"github.com/EliasLd/packrat/internal/engine"
	"github.com/EliasLd/packrat/internal/event"
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
	plain := flag.Bool("plain", false, "force the plain text renderer even on a terminal")
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
		if err := preview(flag.Arg(0)); err != nil {
			fatal(err)
		}
	case flag.NArg() == 1:
		// ctrl-c cancels the pipeline; partial downloads stay as .part
		// files and a rerun resumes them. cancel is also exposed to the
		// interactive renderer for the stop key.
		outer, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		ctx, cancel := context.WithCancel(outer)
		defer cancel()

		err := run(ctx, cancel, flag.Arg(0), engine.Options{
			OutDir:              *outDir,
			ResolveConcurrency:  *resolveConc,
			DownloadConcurrency: *conc,
			Retries:             *retries,
			UserAgent:           fuckingfast.UserAgent,
		}, *plain)
		if code := exitCode(ctx, err); code != 0 {
			if code == 130 {
				fmt.Fprintln(os.Stderr, warnText("interrupted — rerun the same command to resume"))
			}
			os.Exit(code)
		}
	default:
		usage()
		os.Exit(2)
	}
}

// exitCode maps a run outcome to the documented process code: 0 ok, 1 part
// failures or setup errors, 130 interrupted. Usage errors never reach it:
// the flag package reports them with exit 2 around our styled usage.
func exitCode(ctx context.Context, err error) int {
	switch {
	case err == nil:
		return 0
	case ctx.Err() != nil:
		return 130
	default:
		return 1
	}
}

// styled reports whether stderr should carry ANSI accents.
func styled() bool {
	return ui.IsTerminal(os.Stderr) && ui.IsTerminal(os.Stdout)
}

// warnText / errorLine degrade to plain text off a terminal.
func warnText(s string) string {
	if styled() {
		return ui.WarnText(s)
	}
	return s
}

func errorLine(err error) string {
	if styled() {
		return ui.FailText("error") + ": " + err.Error()
	}
	return "error: " + err.Error()
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, errorLine(err))
	os.Exit(1)
}

func run(ctx context.Context, cancel context.CancelFunc, linkFile string, opt engine.Options, plain bool) error {
	data, err := os.ReadFile(linkFile)
	if err != nil {
		return err
	}

	links, issues := link.Parse(data)
	for _, issue := range issues {
		fmt.Fprintf(os.Stderr, "links:%d: %s\n", issue.Line, issue.Reason)
	}
	if len(links) == 0 {
		return fmt.Errorf("no usable links in %s", linkFile)
	}
	groups := link.Groups(links)

	resolver, err := fuckingfast.New()
	if err != nil {
		return err
	}
	registry := host.NewRegistry(resolver)

	var wait func()
	opt.Observe = ui.Plain(ui.PlainOptions{Out: os.Stdout, Warn: os.Stderr})
	if !plain && ui.IsTerminal(os.Stdout) {
		// the pause key toggles the download gate and reports the state
		// change as events, so both the gate and the view stay in sync
		gate := download.NewGate()
		opt.Gate = gate
		var obs event.Observer
		obs, wait = ui.Interactive(ui.InteractiveOptions{
			Out:    os.Stdout,
			Cancel: cancel,
			Toggle: func() {
				if gate.Paused() {
					gate.Resume()
					obs(event.Event{Kind: event.RunResumed})
				} else {
					gate.Pause()
					obs(event.Event{Kind: event.RunPaused})
				}
			},
		})
		opt.Observe = obs
	}

	eng := engine.New(registry, opt)
	err = eng.Run(ctx, groups)
	if wait != nil { // flush the final frame before reporting the outcome
		wait()
	}
	return err
}

// preview parses the links file and prints the download layout that would be
// performed, without resolving or downloading anything.
func preview(linkFile string) error {
	data, err := os.ReadFile(linkFile)
	if err != nil {
		return err
	}
	links, issues := link.Parse(data)
	for _, issue := range issues {
		fmt.Fprintf(os.Stderr, "links:%d: %s\n", issue.Line, issue.Reason)
	}
	if len(links) == 0 {
		return fmt.Errorf("no usable links in %s", linkFile)
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
	return nil
}

func usage() {
	accent, dim := plainWrap, plainWrap
	if styled() {
		accent, dim = ui.AccentText, ui.DimText
	}
	out := os.Stderr

	_, _ = fmt.Fprintf(out, "%s — fetch multi-part downloads from file hosts\n\n", accent("packrat"))
	_, _ = fmt.Fprint(out, "usage: packrat [options] links.txt\n")
	_, _ = fmt.Fprint(out, dim("options must come before the links file\n\n"))

	_, _ = fmt.Fprintln(out, "options:")
	row := func(name, desc string) {
		_, _ = fmt.Fprintf(out, "  %s  %s\n", accent(fmt.Sprintf("%-*s", 13, name)), desc)
	}
	row("-o dir", `output root directory (default "downloads")`)
	row("-c int", "parallel downloads (default 8)")
	row("-rc int", "parallel page resolutions (default 3)")
	row("-retries int", "attempts per part (default 5)")
	row("--plain", "never style output (pipes, CI, logs)")
	row("--dry-run", "print the planned layout, resolve and download nothing")
	row("--version", "print the version and exit")

	_, _ = fmt.Fprintln(out, "\nexamples:")
	example := func(cmd, note string) {
		_, _ = fmt.Fprintf(out, "  %s  %s\n", accent(fmt.Sprintf("%-*s", 35, cmd)), dim(note))
	}
	example("packrat links.txt", "fetch every part into ./downloads")
	example("packrat -o /mnt/archive links.txt", "pick another output root")
	example("packrat -c 2 -retries 8 links.txt", "gentler run for shaky connections")
	example("packrat --dry-run links.txt", "preview the layout first")
	example("packrat --plain links.txt | tee log", "scriptable output")

	_, _ = fmt.Fprint(out, dim("\nexit codes: 0 ok · 1 some part(s) failed · 2 bad usage · 130 interrupted\n"))
}

func plainWrap(s string) string { return s }
