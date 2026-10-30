package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/EliasLd/packrat/internal/event"
)

// PlainOptions configures the plain renderer.
type PlainOptions struct {
	Out  io.Writer // event lines
	Warn io.Writer // failure lines; falls back to Out when nil
	// ProgressInterval is the minimum delay between aggregate progress
	// lines, defaulting to 500 ms. A negative value prints one line per
	// Progress event, which is what tests want.
	ProgressInterval time.Duration
}

// Plain reports a run as styled plain text: one line per lifecycle event and
// a coarse aggregate progress line while parts stream. Safe for concurrent
// use, per the event.Observer contract.
func Plain(opt PlainOptions) event.Observer {
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	if opt.Warn == nil {
		opt.Warn = opt.Out
	}
	if opt.ProgressInterval == 0 {
		opt.ProgressInterval = 500 * time.Millisecond
	}
	return (&plain{opt: opt, active: map[string]event.Event{}}).observe
}

type plain struct {
	opt PlainOptions

	mu         sync.Mutex
	active     map[string]event.Event // part identity -> last Progress event
	done       int
	skipped    int
	failed     int
	partsTotal int
	failures   []failedPart // every resolve/download failure, for the recap

	aggregate   int64     // bytes summed over active parts
	aggregateAt time.Time // when the aggregate line was last printed
	speed       float64   // smoothed bytes per second, republished once per second
	speedAt     time.Time
	speedDelta  int64 // bytes moved since the last speed sample
	lineOpen    bool  // an aggregate line is pending a newline
}

func (p *plain) observe(e event.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch e.Kind {
	case event.RunStart:
		p.partsTotal = e.Stats.PartsTotal
		_, _ = fmt.Fprintf(p.opt.Out, "resolving %d part(s) across %d archive group(s)...\n",
			e.Stats.PartsTotal, e.Stats.GroupsTotal)
	case event.ResolveDone:
		p.endLine()
		_, _ = fmt.Fprintf(p.opt.Out, "  %s %s\n", styleOk.Render("[resolve]"), name(e))
	case event.ResolveFail:
		p.endLine()
		p.failures = append(p.failures, failedPart{name: name(e), reason: errText(e.Err)})
		_, _ = fmt.Fprintf(p.opt.Warn, "  %s %s: %v\n", styleFail.Render("[resolve FAIL]"), name(e), e.Err)
	case event.DownloadStart:
		p.endLine()
		if e.ResumeAt > 0 {
			_, _ = fmt.Fprintf(p.opt.Out, "  %s %s continuing at %s\n",
				styleDim.Render("[resum]"), name(e), event.HumanBytes(e.ResumeAt))
		} else {
			_, _ = fmt.Fprintf(p.opt.Out, "  %s %s\n", styleDim.Render("[start]"), name(e))
		}
	case event.Progress:
		if p.partsTotal == 0 {
			return // progress outside a run: nothing to aggregate
		}
		p.active[e.Part] = e
		p.printAggregate()
	case event.DownloadRetry:
		p.endLine()
		_, _ = fmt.Fprintf(p.opt.Warn, "  %s %s: %v (attempt %d)\n",
			styleWarn.Render("[retry]"), name(e), e.Err, e.Attempt)
	case event.DownloadDone:
		delete(p.active, e.Part)
		p.done++
		p.endLine()
		_, _ = fmt.Fprintf(p.opt.Out, "  %s %s (%s)\n", styleOk.Render("[done]"), name(e), event.HumanBytes(e.Size))
	case event.DownloadSkipped:
		delete(p.active, e.Part)
		p.skipped++
		p.endLine()
		_, _ = fmt.Fprintf(p.opt.Out, "  %s %s already present (%s)\n",
			styleDim.Render("[skip]"), name(e), event.HumanBytes(e.Size))
	case event.DownloadFail:
		delete(p.active, e.Part)
		p.failed++
		p.failures = append(p.failures, failedPart{name: name(e), reason: errText(e.Err)})
		p.endLine()
		_, _ = fmt.Fprintf(p.opt.Warn, "  %s %s: %v\n", styleFail.Render("[FAIL]"), name(e), e.Err)
	case event.Cooling:
		p.endLine()
		_, _ = fmt.Fprintf(p.opt.Warn, "  %s cooling %v: %s\n",
			styleWarn.Render("[rate limit]"), e.Wait, errText(e.Err))
	case event.RunPaused:
		p.endLine()
		_, _ = fmt.Fprintf(p.opt.Out, "  %s paused\n", styleWarn.Render("[pause]"))
	case event.RunResumed:
		p.endLine()
		_, _ = fmt.Fprintf(p.opt.Out, "  %s resumed\n", styleOk.Render("[resume]"))
	case event.RunDone:
		p.finish(e.Stats)
	}
}

// name prefers the file name and falls back to the part identity.
func name(e event.Event) string {
	if e.Filename != "" {
		return e.Filename
	}
	return e.Part
}

// printAggregate refreshes the aggregate download line when the interval
// allows. It replaces the current line, leaving it open for the next pass.
func (p *plain) printAggregate() {
	now := time.Now()
	if p.opt.ProgressInterval > 0 && now.Sub(p.aggregateAt) < p.opt.ProgressInterval {
		return
	}
	var total int64
	for _, a := range p.active {
		total += a.Done
	}
	// the line refreshes every ProgressInterval, but the speed readout
	// only samples once a second or it is unreadable
	if delta := max(total-p.aggregate, 0); delta > 0 {
		p.speedDelta += delta
	}
	if p.speedAt.IsZero() {
		p.speedAt = now
	}
	if now.Sub(p.speedAt) >= time.Second {
		p.speed = nextSpeed(p.speed, float64(p.speedDelta)/now.Sub(p.speedAt).Seconds())
		p.speedDelta, p.speedAt = 0, now
	}
	p.aggregate, p.aggregateAt, p.lineOpen = total, now, true
	_, _ = fmt.Fprintf(p.opt.Out, "\r[dl %d/%d parts] %s @ %s/s  ",
		p.done+p.skipped+p.failed, p.partsTotal,
		event.HumanBytes(total), event.HumanBytes(int64(p.speed)))
}

// finish prints the final summary block and closes a pending aggregate line.
func (p *plain) finish(stats *event.Stats) {
	p.endLine()
	if stats == nil || stats.PartsTotal == 0 {
		_, _ = fmt.Fprintln(p.opt.Out, "no links to process")
		return
	}
	_, _ = fmt.Fprintf(p.opt.Out, "done: %d/%d part(s) downloaded, %d skipped, %d failed\n",
		stats.Done, stats.PartsTotal, stats.Skipped, stats.Failed)
	if stats.Bytes > 0 && stats.Elapsed > 0 && stats.Elapsed.Seconds() > 0 {
		rate := event.HumanBytes(int64(float64(stats.Bytes) / stats.Elapsed.Seconds()))
		_, _ = fmt.Fprintf(p.opt.Out, "      %s in %.1fs @ %s/s\n",
			event.HumanBytes(stats.Bytes), stats.Elapsed.Seconds(), rate)
	}
	p.recap()
}

// recap closes the run with the failures grouped by reason, so a log or CI
// job shows at a glance why a run ended partially. First-seen reason order
// is kept, and a reason with many parts lists a few names plus a count.
func (p *plain) recap() {
	if len(p.failures) == 0 {
		return
	}

	byReason := map[string][]string{}
	order := make([]string, 0, len(p.failures))
	for _, f := range p.failures {
		if _, seen := byReason[f.reason]; !seen {
			order = append(order, f.reason)
		}
		byReason[f.reason] = append(byReason[f.reason], f.name)
	}

	_, _ = fmt.Fprintln(p.opt.Warn, styleFail.Render("failed parts:"))
	for _, reason := range order {
		names := byReason[reason]
		more := ""
		if len(names) > 3 {
			more = fmt.Sprintf(", and %d more", len(names)-3)
			names = names[:3]
		}
		_, _ = fmt.Fprintf(p.opt.Warn, "  %s — %s%s\n", reason, strings.Join(names, ", "), more)
	}
}

// endLine closes a pending aggregate line so event lines start at column 0.
func (p *plain) endLine() {
	if p.lineOpen {
		_, _ = fmt.Fprintln(p.opt.Out)
		p.lineOpen = false
	}
}
