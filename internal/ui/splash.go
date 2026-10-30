package ui

import (
	"fmt"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucasb-eyer/go-colorful"
)

// splashLines is the wordmark art; the gradient runs over each line from the
// light accent toward the base accent, left to right.
var splashLines = []string{
	"▄▄▄▄   ▄▄▄   ▄▄▄▄ ▄▄ ▄▄ ▄▄▄▄   ▄▄▄ ▄▄▄▄▄▄",
	"██▄█▀ ██▀██ ██▀▀▀ ██▄█▀ ██▄█▄ ██▀██  ██",
	"██    ██▀██ ▀████ ██ ██ ██ ██ ██▀██  ██",
}

var splashOnce struct {
	sync.Once
	splash string
}

// splash renders the wordmark with its gradient. It is rendered once and
// cached, because every View frame reuses it.
func splash() string {
	splashOnce.Do(func() {
		splashOnce.splash = gradient(splashLines)
	})
	return splashOnce.splash
}

// gradient colors whole lines from AccentGlow toward Accent, left to right.
func gradient(lines []string) string {
	from, err1 := colorful.Hex(AccentGlow)
	to, err2 := colorful.Hex(Accent)
	if err1 != nil || err2 != nil {
		// unreachable with the built-in palette; degrade to flat accent
		var b strings.Builder
		for _, line := range lines {
			b.WriteString(styleAccent.Render(line))
			b.WriteByte('\n')
		}
		return b.String()
	}

	var b strings.Builder
	for _, line := range lines {
		runes := []rune(line)
		for i, r := range runes {
			t := 0.0
			if len(runes) > 1 {
				t = float64(i) / float64(len(runes)-1)
			}
			blend := from.BlendLuv(to, t)
			b.WriteString(lipgloss.NewStyle().
				Foreground(lipgloss.Color(hex(blend))).
				Render(string(r)))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// hex formats a blended color as a hex string for lipgloss.
func hex(c colorful.Color) string {
	return fmt.Sprintf("#%02x%02x%02x",
		uint8(clamp01(c.R)*255), uint8(clamp01(c.G)*255), uint8(clamp01(c.B)*255))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
