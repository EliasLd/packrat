package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/EliasLd/packrat/internal/event"
)

// the interactive model must satisfy the bubbletea model interface
var _ tea.Model = (*interactive)(nil)

// apply feeds events into the model and returns it plus the last emitted
// command (nil when none of the events produced one).
func apply(t *testing.T, m *interactive, events ...event.Event) (*interactive, tea.Cmd) {
	t.Helper()
	var last tea.Cmd
	for _, e := range events {
		next, cmd := m.Update(e)
		var ok bool
		m, ok = next.(*interactive)
		if !ok {
			t.Fatal("Update() did not return the interactive model")
		}
		if cmd != nil {
			last = cmd
		}
	}
	return m, last
}

var testPlan = []event.PlanEntry{
	{Group: "Game", Part: "part1"},
	{Group: "Game", Part: "part2"},
	{Group: "Extra", Part: "bonus"},
}

func TestInteractiveRowsFollowThePlan(t *testing.T) {
	m, _ := apply(t, newInteractiveModel(InteractiveOptions{}), event.Event{
		Kind:  event.RunStart,
		Stats: &event.Stats{PartsTotal: 3, GroupsTotal: 2},
		Plan:  testPlan,
	})

	if len(m.order) != 3 || m.partsTotal != 3 || len(m.groups) != 2 {
		t.Fatalf("plan layout = order %v groups %v total %d", m.order, m.groups, m.partsTotal)
	}
	// planned-but-idle rows are not rendered yet: splash + resolve counter
	if view := m.View(); strings.Contains(view, "part1") || !strings.Contains(view, "resolving 0/3") {
		t.Fatalf("idle view = %q, want splash and resolve line only", view)
	}
}

func TestInteractiveRowsVanishWhenPartsFinish(t *testing.T) {
	m, _ := apply(t, newInteractiveModel(InteractiveOptions{}), event.Event{
		Kind:  event.RunStart,
		Stats: &event.Stats{PartsTotal: 3, GroupsTotal: 2},
		Plan:  testPlan,
	})

	m, _ = apply(t, m,
		event.Event{Kind: event.ResolveStart, Part: "part1"},
		event.Event{Kind: event.ResolveDone, Part: "part1", Filename: "Game.part1.rar"},
		event.Event{Kind: event.DownloadStart, Part: "part1", Filename: "Game.part1.rar"},
		event.Event{Kind: event.Progress, Part: "part1", Done: 2048, Total: 4096},
	)

	if view := m.View(); !strings.Contains(view, "Game.part1.rar") {
		t.Fatalf("view = %q, want the active bar row", view)
	}
	view := m.View()
	if !strings.Contains(view, "\nGame\n") {
		t.Fatalf("view = %q, want the group heading", view)
	}

	// finishing the part removes its row and the empty group heading
	m, _ = apply(t, m,
		event.Event{Kind: event.DownloadDone, Part: "part1", Filename: "Game.part1.rar", Size: 4096},
	)
	view = m.View()
	if strings.Contains(view, "Game.part1.rar") || strings.Contains(view, "Game") {
		t.Fatalf("view = %q, want the finished row gone", view)
	}
}

func TestInteractiveBarsAnimateBetweenFrames(t *testing.T) {
	m, _ := apply(t, newInteractiveModel(InteractiveOptions{}), event.Event{
		Kind:  event.RunStart,
		Stats: &event.Stats{PartsTotal: 1, GroupsTotal: 1},
		Plan:  []event.PlanEntry{{Group: "Game", Part: "part1"}},
	})

	m, anim := apply(t, m,
		event.Event{Kind: event.ResolveStart, Part: "part1"},
		event.Event{Kind: event.ResolveDone, Part: "part1", Filename: "Game.part1.rar"},
		event.Event{Kind: event.DownloadStart, Part: "part1", Filename: "Game.part1.rar"},
		event.Event{Kind: event.Progress, Part: "part1", Done: 300, Total: 900},
	)
	r := m.rows["part1"]
	if r == nil || anim == nil {
		t.Fatalf("progress must schedule an animation frame (row %v cmd %v)", r, anim)
	}
	empty := r.bar.View() // percentShown is still 0: no frame was routed

	// the anim command yields FrameMsg values; routing them back into the
	// model must keep the spring chain alive until it settles
	for i := 0; i < 90 && anim != nil; i++ {
		msg := anim()
		next, cmd := m.Update(msg)
		var ok bool
		if m, ok = next.(*interactive); !ok {
			t.Fatal("Update() did not return the interactive model")
		}
		anim = cmd
	}
	if m.rows["part1"].bar.View() == empty {
		t.Fatal("bar never moved although frames were routed")
	}
}

func TestInteractiveQueuedPartsStayOneLine(t *testing.T) {
	m, _ := apply(t, newInteractiveModel(InteractiveOptions{}), event.Event{
		Kind:  event.RunStart,
		Stats: &event.Stats{PartsTotal: 3, GroupsTotal: 2},
		Plan:  testPlan,
	})

	// one part resolved and queued for a slot (part2), one downloading
	// (part1), one still unresolved (part3, counted in the stats line)
	m, _ = apply(t, m,
		event.Event{Kind: event.ResolveStart, Part: "part1"},
		event.Event{Kind: event.ResolveDone, Part: "part1", Filename: "Game.part1.rar"},
		event.Event{Kind: event.ResolveStart, Part: "part2"},
		event.Event{Kind: event.ResolveDone, Part: "part2", Filename: "Game.part2.rar"},
		event.Event{Kind: event.DownloadStart, Part: "part1", Filename: "Game.part1.rar"},
	)

	view := m.View()
	if strings.Contains(view, "[queued]") {
		t.Fatalf("view = %q, queued parts must not get their own rows", view)
	}
	if !strings.Contains(view, "1 more queued") {
		t.Fatalf("view = %q, want the queued summary line", view)
	}
	if strings.Contains(view, "Game.part2.rar") {
		t.Fatalf("view = %q, the queued part must stay anonymous", view)
	}
	if !strings.Contains(view, "Game.part1.rar") {
		t.Fatalf("view = %q, want the downloading row", view)
	}
}

func TestInteractiveFailuresKeepTheirReason(t *testing.T) {
	m, _ := apply(t, newInteractiveModel(InteractiveOptions{}), event.Event{
		Kind:  event.RunStart,
		Stats: &event.Stats{PartsTotal: 3, GroupsTotal: 2},
		Plan:  testPlan,
	})

	m, _ = apply(t, m,
		event.Event{Kind: event.ResolveStart, Part: "bonus"},
		event.Event{Kind: event.ResolveFail, Part: "bonus", Err: errors.New("host says no")},
	)

	view := m.View()
	if !strings.Contains(view, "[fail] bonus — host says no") {
		t.Fatalf("view = %q, want the persistent failure line", view)
	}
	if strings.Count(view, "[fail]") != 1 {
		t.Fatalf("view = %q, want exactly one failure line", view)
	}
}

func TestInteractiveStatsLineIsTheRunMemory(t *testing.T) {
	m, _ := apply(t, newInteractiveModel(InteractiveOptions{}), event.Event{
		Kind:  event.RunStart,
		Stats: &event.Stats{PartsTotal: 3, GroupsTotal: 2},
		Plan:  testPlan,
	})

	m, _ = apply(t, m,
		event.Event{Kind: event.ResolveStart, Part: "part1"},
		event.Event{Kind: event.ResolveDone, Part: "part1", Filename: "Game.part1.rar"},
	)

	if view := m.View(); !strings.Contains(view, "resolving 1/3 part(s)") {
		t.Fatalf("view = %q, want the resolve counter", view)
	}

	// once the download phase starts, the stats line tracks the run
	m, _ = apply(t, m,
		event.Event{Kind: event.DownloadStart, Part: "part1", Filename: "Game.part1.rar"},
		event.Event{Kind: event.Progress, Part: "part1", Done: 300, Total: 900},
		event.Event{Kind: event.DownloadDone, Part: "part1", Filename: "Game.part1.rar", Size: 900},
	)
	view := m.View()
	if !strings.Contains(view, "1/3 parts") {
		t.Fatalf("view = %q, want the parts counter on the stats line", view)
	}
	if !strings.Contains(view, "33%") {
		t.Fatalf("view = %q, want the global percentage", view)
	}
}

func TestInteractiveRunDoneQuotesTheStats(t *testing.T) {
	m, _ := apply(t, newInteractiveModel(InteractiveOptions{}), event.Event{
		Kind:  event.RunStart,
		Stats: &event.Stats{PartsTotal: 3, GroupsTotal: 2},
		Plan:  testPlan,
	})

	m, cmd := apply(t, m, event.Event{
		Kind: event.RunDone,
		Stats: &event.Stats{
			PartsTotal: 3, GroupsTotal: 2, Done: 1, Skipped: 1, Failed: 1,
			Bytes: 2048, Elapsed: 2_000_000_000,
		},
	})

	if m.stats == nil {
		t.Fatal("run done stats not stored")
	}
	if cmd == nil {
		t.Fatal("run done must quit the program")
	}
	view := m.View()
	for _, want := range []string{
		"done: 1/3 part(s) downloaded, 1 skipped, 1 failed",
		"2.0KiB in 2s @ 1.0KiB/s",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("view = %q, want %q", view, want)
		}
	}
}

func TestInteractivePauseTogglesAndFlipsState(t *testing.T) {
	toggled := 0
	m := newInteractiveModel(InteractiveOptions{Toggle: func() { toggled++ }})

	m, _ = apply(t, m, event.Event{
		Kind:  event.RunStart,
		Stats: &event.Stats{PartsTotal: 2, GroupsTotal: 1},
		Plan:  []event.PlanEntry{{Group: "Game", Part: "part1"}},
	})

	// pressing p reports the toggle; the model only flips state when the
	// RunPaused event comes back through the stream
	toggleMsg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}}
	next, cmd := m.Update(toggleMsg)
	m, _ = next.(*interactive)
	_ = cmd
	if toggled != 1 {
		t.Fatalf("toggle calls = %d, want 1", toggled)
	}
	if m.paused {
		t.Fatal("state must only flip when the event arrives")
	}

	m, _ = apply(t, m, event.Event{Kind: event.RunPaused})
	if !m.paused {
		t.Fatal("RunPaused must pause the view")
	}
	view := m.View()
	if !strings.Contains(view, "[paused]") {
		t.Fatalf("view = %q, want the paused indicator", view)
	}

	m, _ = apply(t, m, event.Event{Kind: event.RunResumed})
	next, _ = m.Update(toggleMsg)
	m, _ = next.(*interactive)
	if m.paused || toggled != 2 {
		t.Fatalf("resume state = %v, toggles = %d, want resumed with 2 toggles", m.paused, toggled)
	}
	if strings.Contains(m.View(), "[paused]") {
		t.Fatal("paused indicator must clear on resume")
	}
}

func TestInteractiveSpeedReadoutMovesOncePerSecond(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	m := newInteractiveModel(InteractiveOptions{})
	m.now = func() time.Time { return base }

	m, _ = apply(t, m, event.Event{
		Kind:  event.RunStart,
		Stats: &event.Stats{PartsTotal: 1, GroupsTotal: 1},
		Plan:  []event.PlanEntry{{Group: "Game", Part: "part1"}},
	})
	m, _ = apply(t, m,
		event.Event{Kind: event.ResolveStart, Part: "part1"},
		event.Event{Kind: event.ResolveDone, Part: "part1", Filename: "Game.part1.rar"},
		event.Event{Kind: event.DownloadStart, Part: "part1", Filename: "Game.part1.rar"},
	)

	// traffic inside the first window must not move the readout
	m.now = func() time.Time { return base.Add(300 * time.Millisecond) }
	m, _ = apply(t, m, event.Event{Kind: event.Progress, Part: "part1", Done: 2000, Total: 4000})
	if m.rows["part1"].speed != 0 || m.aggSpeed != 0 {
		t.Fatalf("speeds = %v/%v, want 0 within the first window",
			m.rows["part1"].speed, m.aggSpeed)
	}

	// the sample at the end of the window republishes once
	m.now = func() time.Time { return base.Add(1500 * time.Millisecond) }
	m, _ = apply(t, m, event.Event{Kind: event.Progress, Part: "part1", Done: 4000, Total: 4000})
	speed := m.rows["part1"].speed
	if speed <= 0 {
		t.Fatalf("row speed = %v, want the first window sample", speed)
	}
	if m.aggSpeed != speed {
		t.Fatalf("agg speed %v != row speed %v with a single row", m.aggSpeed, speed)
	}

	// further traffic inside the same window keeps the number where it was
	m.now = func() time.Time { return base.Add(1800 * time.Millisecond) }
	m, _ = apply(t, m, event.Event{Kind: event.Progress, Part: "part1", Done: 4100, Total: 4200})
	if m.rows["part1"].speed != speed {
		t.Fatalf("row speed = %v, want %v until the next sample", m.rows["part1"].speed, speed)
	}

	// a pause/resume cycle must not leak pause time into the next window
	m.now = func() time.Time { return base.Add(2 * time.Second) }
	m, _ = apply(t, m,
		event.Event{Kind: event.RunPaused},
		event.Event{Kind: event.RunResumed},
	)
	if !m.rows["part1"].speedAt.Equal(base.Add(2 * time.Second)) {
		t.Fatalf("resume did not reset the row speed window: %v", m.rows["part1"].speedAt)
	}
	if m.aggSpeed != speed {
		t.Fatalf("resume must keep the last readout, got %v, want %v", m.aggSpeed, speed)
	}
}

func TestInteractiveStableRowOrder(t *testing.T) {
	m, _ := apply(t, newInteractiveModel(InteractiveOptions{}), event.Event{
		Kind:  event.RunStart,
		Stats: &event.Stats{PartsTotal: 2, GroupsTotal: 1},
		Plan:  []event.PlanEntry{{Group: "Game", Part: "part1"}, {Group: "Game", Part: "part2"}},
	})

	m, _ = apply(t, m,
		// out-of-order arrival: part2 resolves before part1
		event.Event{Kind: event.ResolveStart, Part: "part2"},
		event.Event{Kind: event.ResolveStart, Part: "part1"},
		event.Event{Kind: event.ResolveDone, Part: "part2", Filename: "Game.part2.rar"},
		event.Event{Kind: event.ResolveDone, Part: "part1", Filename: "Game.part1.rar"},
		event.Event{Kind: event.DownloadStart, Part: "part1", Filename: "Game.part1.rar"},
		event.Event{Kind: event.DownloadStart, Part: "part2", Filename: "Game.part2.rar"},
	)

	view := m.View()
	if strings.Index(view, "Game.part1.rar") > strings.Index(view, "Game.part2.rar") {
		t.Fatalf("view rows are out of plan order:\n%s", view)
	}
}
