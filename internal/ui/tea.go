package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/EliasLd/packrat/internal/event"
)

// IsTerminal reports whether f is attached to a terminal character device.
func IsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// InteractiveOptions configures the interactive renderer.
type InteractiveOptions struct {
	Out io.Writer // program output; keys are read from stdin by default

	// Cancel performs a graceful stop (q or ctrl+c): cancels the run
	// context so parts stop and keep their partial temp files.
	Cancel func()

	// Toggle flips the pause state (p); the caller wires it to the
	// download gate and reports RunPaused/RunResumed events, which the
	// model consumes like any other event.
	Toggle func()
}

// Interactive renders the run through bubbletea in inline mode (no alternate
// screen): splash art, per-part animated bars, a global stats line and a
// keybinding footer. Rows exist only while their part is working; the
// global stats line is the memory of the run.
//
// It returns the event Observer plus a wait func; call wait once the
// pipeline has returned so the final frame is flushed before exiting.
func Interactive(opt InteractiveOptions) (event.Observer, func()) {
	if opt.Out == nil {
		opt.Out = io.Discard
	}

	ch := make(chan event.Event, 256)
	prog := tea.NewProgram(newInteractiveModel(opt),
		tea.WithOutput(opt.Out), tea.WithoutSignalHandler())
	progDone := make(chan struct{})
	go func() {
		defer close(progDone)
		_, _ = prog.Run()
	}()
	go func() {
		for e := range ch {
			prog.Send(e)
		}
	}()

	obs := func(e event.Event) {
		if e.Kind == event.Progress {
			select {
			case ch <- e:
			default: // progress is a display hint: dropping a few is fine
			}
			return
		}
		select {
		case ch <- e:
		case <-progDone: // program gone; the run is over anyway
		}
	}
	wait := func() {
		close(ch)
		select {
		case <-progDone:
		case <-time.After(2 * time.Second): // RunDone missed: force exit
			prog.Quit()
			<-progDone
		}
	}
	return obs, wait
}

type rowState uint8

const (
	rowWaiting rowState = iota // planned, not started (never rendered)
	rowResolving
	rowQueued // resolved, waiting for a download slot
	rowDownloading
	rowHidden // terminal state: never rendered
)

type row struct {
	part, group string
	name        string
	state       rowState
	done        int64
	total       int64
	resumeAt    int64
	attempt     int
	speed       float64 // displayed speed, republished once per speedInterval
	speedDelta  int64   // bytes moved since the last speed sample
	speedAt     time.Time
	bar         progress.Model
}

type failedPart struct {
	name, reason string
}

type interactive struct {
	opt     InteractiveOptions
	now     func() time.Time // injectable clock; speed and elapsed read on it
	width   int
	rows    map[string]*row
	order   []string // plan order for stable rendering
	groups  []string // plan order of group names
	start   time.Time
	stats   *event.Stats
	paused  bool
	failure []failedPart

	partsTotal, groupsTotal int
	resolved                int
	downloadsStarted        int
	done, skipped, failed   int

	bytesTotal int64 // bytes moved over the whole run
	aggDelta   int64 // bytes moved since the last speed sample
	aggAt      time.Time
	aggSpeed   float64
}

func newInteractiveModel(opt InteractiveOptions) *interactive {
	m := &interactive{opt: opt, rows: map[string]*row{}}
	m.now = time.Now
	return m
}

func (m *interactive) Init() tea.Cmd { return nil }

func (m *interactive) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		for _, r := range m.rows {
			if r.bar.Width != 0 {
				r.bar.Width = m.barWidth()
			}
		}
	case tea.KeyMsg:
		// p pauses/resumes downloads; q and ctrl+c stop the run. Both are
		// wired by the caller (Cancel/Toggle) and reported back as events.
		switch msg.String() {
		case "q", "ctrl+c":
			if m.opt.Cancel != nil {
				m.opt.Cancel()
			}
		case "p":
			if m.opt.Toggle != nil {
				m.opt.Toggle()
			}
		}
	case progress.FrameMsg:
		// animation frames address one specific bar; every row bar gets a
		// chance to react and only the addressed one keeps the chain alive
		var cmds []tea.Cmd
		for _, part := range m.order {
			r := m.rows[part]
			if r == nil || r.state != rowDownloading {
				continue
			}
			bar, cmd := r.bar.Update(msg)
			if next, ok := bar.(progress.Model); ok {
				r.bar = next
			}
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)
	case event.Event:
		return m.apply(msg)
	}
	return m, nil
}

// apply folds one pipeline event into the model state.
func (m *interactive) apply(e event.Event) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch e.Kind {
	case event.RunStart:
		m.partsTotal, m.groupsTotal = e.Stats.PartsTotal, e.Stats.GroupsTotal
		m.start, m.aggAt = m.now(), m.now()
		for _, entry := range e.Plan {
			if _, ok := m.rows[entry.Part]; ok {
				continue
			}
			m.rows[entry.Part] = &row{part: entry.Part, group: entry.Group, name: entry.Part}
			m.order = append(m.order, entry.Part)
			if !m.hasGroup(entry.Group) {
				m.groups = append(m.groups, entry.Group)
			}
		}
	case event.ResolveStart:
		if r := m.rows[e.Part]; r != nil {
			r.state = rowResolving
		}
	case event.ResolveDone:
		if r := m.rows[e.Part]; r != nil {
			r.name = displayName(e)
			if r.state == rowWaiting || r.state == rowResolving {
				r.state = rowQueued
			}
		}
		m.resolved++
	case event.ResolveFail:
		if r := m.rows[e.Part]; r != nil {
			r.state = rowHidden
		}
		m.failed++
		m.failure = append(m.failure, failedPart{name: displayName(e), reason: errText(e.Err)})
	case event.DownloadStart:
		if r := m.rows[e.Part]; r != nil {
			r.name = displayName(e)
			r.state, r.resumeAt = rowDownloading, e.ResumeAt
			r.speedAt = m.now()
			r.bar = progress.New(progress.WithDefaultGradient())
			r.bar.ShowPercentage = false // the row already prints its own percent
			r.bar.Width = m.barWidth()
		}
		m.downloadsStarted++
	case event.Progress:
		r := m.rows[e.Part]
		if r == nil || r.state != rowDownloading {
			break
		}
		delta := max(e.Done-r.done, 0)
		r.done, r.total = e.Done, e.Total
		now := m.now()
		m.sampleRowSpeed(r, now, delta)
		m.sampleAggSpeed(now, delta)
		m.bytesTotal += delta
		if r.total > 0 {
			anim := r.bar.SetPercent(float64(r.done) / float64(r.total))
			cmds = append(cmds, anim)
		}
	case event.DownloadRetry:
		if r := m.rows[e.Part]; r != nil {
			r.attempt = e.Attempt
		}
	case event.DownloadDone:
		if r := m.rows[e.Part]; r != nil {
			r.state = rowHidden // rows vanish when their part is over
		}
		m.done++
	case event.DownloadSkipped:
		if r := m.rows[e.Part]; r != nil {
			r.state = rowHidden
		}
		m.skipped++
	case event.DownloadFail:
		if r := m.rows[e.Part]; r != nil {
			r.state = rowHidden
		}
		m.failed++
		m.failure = append(m.failure, failedPart{name: displayName(e), reason: errText(e.Err)})
	case event.RunPaused, event.RunResumed:
		m.paused = e.Kind == event.RunPaused
		if e.Kind == event.RunResumed {
			// paused time must not pollute the next speed window
			now := m.now()
			for _, r := range m.rows {
				if r.state == rowDownloading {
					r.speedDelta, r.speedAt = 0, now
				}
			}
			m.aggDelta, m.aggAt = 0, now
		}
	case event.RunDone:
		m.stats = e.Stats
		return m, tea.Quit
	}

	return m, tea.Batch(cmds...)
}

func (m *interactive) hasGroup(g string) bool {
	for _, name := range m.groups {
		if name == g {
			return true
		}
	}
	return false
}

// barWidth fits the bar between the name and metadata columns.
func (m *interactive) barWidth() int {
	w := m.width - 64
	switch {
	case w < 12:
		return 12
	case w > barMax:
		return barMax
	}
	return w
}

const barMax = 26

// speedInterval is how often the speed readouts are republished; progress
// arrives far more often and would be unreadable if displayed directly.
const speedInterval = time.Second

// sampleRowSpeed folds a bytes delta into a row's speed readout.
func (m *interactive) sampleRowSpeed(r *row, now time.Time, delta int64) {
	if delta <= 0 {
		return
	}
	r.speedDelta += delta
	if now.Sub(r.speedAt) < speedInterval {
		return
	}
	r.speed = nextSpeed(r.speed, float64(r.speedDelta)/now.Sub(r.speedAt).Seconds())
	r.speedDelta, r.speedAt = 0, now
}

// sampleAggSpeed folds a bytes delta into the global speed readout.
func (m *interactive) sampleAggSpeed(now time.Time, delta int64) {
	if delta <= 0 {
		return
	}
	m.aggDelta += delta
	if now.Sub(m.aggAt) < speedInterval {
		return
	}
	m.aggSpeed = nextSpeed(m.aggSpeed, float64(m.aggDelta)/now.Sub(m.aggAt).Seconds())
	m.aggDelta, m.aggAt = 0, now
}

// nextSpeed smooths a new window average into the displayed value; the very
// first window is published as-is instead of starting from zero.
func nextSpeed(current, instant float64) float64 {
	if current <= 0 {
		return instant
	}
	return 0.7*current + 0.3*instant
}

func (m *interactive) View() string {
	var b strings.Builder

	b.WriteString(splash())

	queued := 0
	for _, group := range m.groups {
		var section strings.Builder
		shown := 0
		for _, part := range m.order {
			r := m.rows[part]
			if r == nil || r.group != group {
				continue
			}
			switch r.state {
			case rowResolving, rowDownloading:
				_, _ = fmt.Fprint(&section, m.rowView(r))
				shown++
			case rowQueued:
				queued++
			}
		}
		if shown > 0 {
			_, _ = fmt.Fprintf(&b, "\n%s\n", styleHeader.Render(group))
			b.WriteString(section.String())
		}
	}

	// resolved parts waiting for a download slot collapse into one line,
	// so the frame stays bounded no matter how big the links file is
	if queued > 0 {
		_, _ = fmt.Fprintf(&b, "  %s\n", styleDim.Render(fmt.Sprintf("%d more queued", queued)))
	}

	for _, f := range m.failure {
		_, _ = fmt.Fprintf(&b, "%s %s — %s\n", styleFail.Render("[fail]"), f.name, f.reason)
	}

	if line := m.statsLine(); line != "" {
		_, _ = fmt.Fprint(&b, line)
	}

	_, _ = fmt.Fprintf(&b, "\n%s\n", styleDim.Render("p pause/resume · q stop"))
	return b.String()
}

// rowView renders one active part row.
func (m *interactive) rowView(r *row) string {
	name := truncateName(r.name, nameMax)
	switch r.state {
	case rowResolving:
		return fmt.Sprintf("  %s %s\n", styleDim.Render("[resolve]"), name)
	case rowDownloading:
		if r.total == 0 {
			// first progress not known yet: show where it continued from
			if r.resumeAt > 0 {
				return fmt.Sprintf("  %s %s resuming at %s\n", styleDim.Render("[resum]"), name, event.HumanBytes(r.resumeAt))
			}
			return fmt.Sprintf("  %s %s starting…\n", styleDim.Render("[start]"), name)
		}
		retry := ""
		if r.attempt > 0 {
			retry = fmt.Sprintf("  %s retrying (attempt %d)", styleWarn.Render("[retry]"), r.attempt)
		}
		pct := float64(r.done) / float64(r.total) * 100
		return fmt.Sprintf("%s %3.0f%% %-22s %s / %s  %s/s%s\n",
			r.bar.View(), pct, name,
			event.HumanBytes(r.done), event.HumanBytes(r.total),
			event.HumanBytes(int64(r.speed)), retry)
	default:
		return ""
	}
}

const nameMax = 22

// statsLine is the run's memory under the bars: global percentage, parts
// completed over total, bytes, speed, elapsed time and failures.
func (m *interactive) statsLine() string {
	if m.partsTotal == 0 {
		return ""
	}

	paused := ""
	if m.paused {
		paused = "  " + styleWarn.Render("[paused]")
	}

	if m.stats == nil {
		if m.downloadsStarted == 0 {
			return fmt.Sprintf("\n%s resolving %d/%d part(s)…%s\n",
				styleAccent.Render("»"), m.resolved, m.partsTotal, paused)
		}
		partsDone := m.done + m.skipped
		pct := partsDone * 100 / m.partsTotal
		return fmt.Sprintf("\n%s %d/%d parts · %s @ %s/s · %s · %s%s\n",
			styleAccent.Render(fmt.Sprintf("%d%%", pct)),
			partsDone, m.partsTotal,
			event.HumanBytes(m.bytesTotal), event.HumanBytes(int64(m.aggSpeed)),
			elapsed(m.now(), m.start), m.failuresLine(), paused)
	}

	s := m.stats
	rate := int64(0)
	if s.Elapsed > 0 {
		rate = int64(float64(s.Bytes) / s.Elapsed.Seconds())
	}
	summary := fmt.Sprintf("\n%s %d/%d part(s) downloaded, %d skipped, %d failed · %s in %s @ %s/s\n",
		styleAccent.Render("done:"), s.Done, s.PartsTotal, s.Skipped, s.Failed,
		event.HumanBytes(s.Bytes), s.Elapsed.Truncate(time.Second), event.HumanBytes(rate))
	return summary
}

func (m *interactive) failuresLine() string {
	if m.failed == 0 {
		return styleDim.Render("0 failed")
	}
	return styleFail.Render(fmt.Sprintf("%d failed", m.failed))
}

// truncateName keeps the tail of the name (the part number end) within max.
func truncateName(name string, max int) string {
	runes := []rune(name)
	if len(runes) <= max {
		return string(runes)
	}
	return "…" + string(runes[len(runes)-max+1:])
}

// displayName prefers the file name and falls back to the part identity.
func displayName(e event.Event) string {
	if e.Filename != "" {
		return e.Filename
	}
	return e.Part
}

func errText(err error) string {
	if err == nil {
		return "unknown failure"
	}
	return err.Error()
}

func elapsed(now, since time.Time) string {
	if since.IsZero() {
		return "0s"
	}
	return now.Sub(since).Truncate(time.Second).String()
}
