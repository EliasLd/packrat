package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/EliasLd/packrat/internal/event"
)

// forceAscii strips all ANSI styling so assertions check plain text only.
func forceAscii(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

func TestPlainLifecycle(t *testing.T) {
	forceAscii(t)
	var out, warn strings.Builder
	obs := Plain(PlainOptions{Out: &out, Warn: &warn, ProgressInterval: -time.Second})

	for _, e := range []event.Event{
		{Kind: event.RunStart, Stats: &event.Stats{PartsTotal: 3, GroupsTotal: 2}},
		{Kind: event.ResolveDone, Part: "p1", Filename: "Game.part1.rar"},
		{Kind: event.ResolveFail, Part: "p2", Err: errors.New("host says no")},
		{Kind: event.DownloadStart, Part: "p1", Filename: "Game.part1.rar"},
		{Kind: event.DownloadStart, Part: "p3", Filename: "bonus.rar", ResumeAt: 10},
		{Kind: event.Progress, Part: "p1", Filename: "Game.part1.rar", Done: 1024, Total: 4096},
		{Kind: event.DownloadRetry, Part: "p3", Filename: "bonus.rar", Attempt: 2, Err: errors.New("expired")},
		{Kind: event.DownloadDone, Part: "p1", Filename: "Game.part1.rar", Size: 4096},
		{Kind: event.DownloadSkipped, Part: "px", Filename: "old.rar", Size: 8},
		{Kind: event.DownloadFail, Part: "p3", Filename: "bonus.rar", Err: errors.New("giving up")},
		{Kind: event.RunDone, Stats: &event.Stats{
			PartsTotal: 3, GroupsTotal: 1, Done: 1, Skipped: 1, Failed: 2,
			Bytes: 4104, Elapsed: 2 * time.Second}},
	} {
		obs(e)
	}

	text := out.String() + warn.String()
	for _, want := range []string{
		"resolving 3 part(s) across 2 archive group(s)",
		"[resolve] Game.part1.rar",
		"[resolve FAIL] p2: host says no",
		"[start] Game.part1.rar",
		"continuing at 10B",
		"[dl 0/3 parts] 1.0KiB @ 0B/s",
		"[retry] bonus.rar: expired (attempt 2)",
		"[done] Game.part1.rar (4.0KiB)",
		"[skip] old.rar already present (8B)",
		"[FAIL] bonus.rar: giving up",
		"done: 1/3 part(s) downloaded, 1 skipped, 2 failed",
		"4.0KiB in 2.0s @ 2.0KiB/s",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Fatal("the ascii profile must not emit ANSI codes")
	}
}

func TestPlainThrottlesAggregate(t *testing.T) {
	forceAscii(t)
	var out strings.Builder
	obs := Plain(PlainOptions{Out: &out, Warn: &out, ProgressInterval: time.Hour})

	obs(event.Event{Kind: event.RunStart, Stats: &event.Stats{PartsTotal: 2, GroupsTotal: 1}})
	for i := int64(1); i <= 5; i++ {
		obs(event.Event{Kind: event.Progress, Part: "p1", Done: i, Total: 10})
	}

	if n := strings.Count(out.String(), "[dl "); n != 1 {
		t.Fatalf("aggregate lines = %d, want 1 within the throttle window", n)
	}
}

func TestPlainRecapsFailedPartsGroupedByReason(t *testing.T) {
	forceAscii(t)
	var out, warn strings.Builder
	obs := Plain(PlainOptions{Out: &out, Warn: &warn, ProgressInterval: -time.Second})

	deadline := "giving up after 5 attempt(s): unexpected status 404"
	for _, e := range []event.Event{
		{Kind: event.RunStart, Stats: &event.Stats{PartsTotal: 4, GroupsTotal: 1}},
		{Kind: event.ResolveFail, Part: "p3", Err: errors.New("host says no")},
		{Kind: event.DownloadFail, Part: "p1", Filename: "f1.rar", Err: errors.New(deadline)},
		{Kind: event.DownloadFail, Part: "p2", Filename: "f2.rar", Err: errors.New(deadline)},
		{Kind: event.RunDone, Stats: &event.Stats{
			PartsTotal: 4, GroupsTotal: 1, Done: 1, Failed: 3, Bytes: 10, Elapsed: time.Second}},
	} {
		obs(e)
	}

	warned := warn.String()
	if !strings.Contains(warned, "failed parts:") {
		t.Fatalf("output = %q, want the recap heading", warned)
	}
	if !strings.Contains(warned, deadline+" — f1.rar, f2.rar") {
		t.Fatalf("output = %q, want the same-reason parts grouped", warned)
	}
	if !strings.Contains(warned, "host says no — p3") {
		t.Fatalf("output = %q, want the resolve failure in the recap", warned)
	}
	if strings.Count(warned, deadline+" —") != 1 {
		t.Fatalf("output = %q, want each reason exactly once in the recap", warned)
	}
}

func TestPlainEmptyRun(t *testing.T) {
	forceAscii(t)
	var out strings.Builder
	obs := Plain(PlainOptions{Out: &out, Warn: &out, ProgressInterval: -time.Second})

	obs(event.Event{Kind: event.RunDone, Stats: &event.Stats{}})

	if text := out.String(); !strings.Contains(text, "no links to process") {
		t.Fatalf("output = %q, want the empty-run message", text)
	}
}
