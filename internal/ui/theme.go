// Package ui renders pipeline event streams. Plain writes styled plain text
// for non-interactive terminals; the interactive bubbletea renderer lives in
// tea.go. Renderers only consume events, never touch the pipeline.
package ui

import "github.com/charmbracelet/lipgloss"

// Palette shared by every renderer.
const (
	Accent      = "#f59e0b" // base accent (rat gold)
	AccentGlow  = "#fbbf24" // light accent; gradient partner of Accent
	OkColor     = "#34d399"
	WarnColor   = "#fbbf24"
	FailColor   = "#f87171"
	DimColor    = "#6b7280"
	BrightColor = "#e5e7eb"
)

// Styles used by the plain and interactive renderers. The accent styles are
// reserved for the splash art and the aggregate line.
var (
	styleAccent = lipgloss.NewStyle().Foreground(lipgloss.Color(Accent)).Bold(true)
	styleOk     = lipgloss.NewStyle().Foreground(lipgloss.Color(OkColor))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color(WarnColor))
	styleFail   = lipgloss.NewStyle().Foreground(lipgloss.Color(FailColor)).Bold(true)
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color(DimColor))
	styleHeader = lipgloss.NewStyle().Foreground(lipgloss.Color(BrightColor)).Bold(true)
)

// Text helpers shape the entry point's usage and error strings with the same
// palette as the run views. lipgloss degrades to plain text on a
// non-terminal stdout and honors NO_COLOR, so callers stay unconditional.
func AccentText(s string) string { return styleAccent.Render(s) }
func DimText(s string) string    { return styleDim.Render(s) }
func WarnText(s string) string   { return styleWarn.Render(s) }
func FailText(s string) string   { return styleFail.Render(s) }
