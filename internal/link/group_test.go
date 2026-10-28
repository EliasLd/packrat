package link_test

import (
	"reflect"
	"testing"

	"github.com/EliasLd/packrat/internal/link"
)

func groupedNames(g link.Group) []string {
	out := make([]string, 0, len(g.Parts))
	for _, p := range g.Parts {
		out = append(out, p.FileName)
	}
	return out
}

func TestGroup(t *testing.T) {
	tests := []struct {
		name      string
		links     []link.Link
		wantNames []string // group names, in expected order
		wantParts map[string][]string
	}{
		{
			name: "multi-file input produces one group per archive, in appearance order",
			links: []link.Link{
				{PageURL: "https://h.example/a", ID: "a", Name: "Game--x.part01.rar"},
				{PageURL: "https://h.example/b", ID: "b", Name: "Optional.bonus.part1.rar"},
				{PageURL: "https://h.example/c", ID: "c", Name: "Game--x.part02.rar"},
				{PageURL: "https://h.example/d", ID: "d", Name: "Optional.bonus.part2.rar"},
			},
			wantNames: []string{"Game--x", "Optional.bonus"},
			wantParts: map[string][]string{
				"Game--x":        {"Game--x.part01.rar", "Game--x.part02.rar"},
				"Optional.bonus": {"Optional.bonus.part1.rar", "Optional.bonus.part2.rar"},
			},
		},
		{
			name: "parts are ordered numerically across widths",
			links: []link.Link{
				{PageURL: "https://h.example/1", ID: "1", Name: "game.part10.rar"},
				{PageURL: "https://h.example/2", ID: "2", Name: "game.part2.rar"},
				{PageURL: "https://h.example/3", ID: "3", Name: "game.part1.rar"},
				{PageURL: "https://h.example/4", ID: "4", Name: "game.part03.rar"},
			},
			wantNames: []string{"game"},
			wantParts: map[string][]string{
				"game": {"game.part1.rar", "game.part2.rar", "game.part03.rar", "game.part10.rar"},
			},
		},
		{
			name:      "single file without part numbering",
			links:     []link.Link{{PageURL: "https://h.example/s", ID: "s", Name: "Manual.pdf"}},
			wantNames: []string{"Manual"},
			wantParts: map[string][]string{"Manual": {"Manual.pdf"}},
		},
		{
			name:      "missing fragment falls back to the id in an unknown group",
			links:     []link.Link{{PageURL: "https://h.example/abc99", ID: "abc99"}},
			wantNames: []string{"unknown"},
			wantParts: map[string][]string{"unknown": {"abc99"}},
		},
		{
			name:      "hostile file names are sanitized",
			links:     []link.Link{{PageURL: "https://h.example/s", ID: "s", Name: "Game: Rebirth?*.part01.rar"}},
			wantNames: []string{"Game_ Rebirth__"},
			wantParts: map[string][]string{"Game_ Rebirth__": {"Game_ Rebirth__.part01.rar"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups := link.Groups(tt.links)
			names := make([]string, len(groups))
			for i, g := range groups {
				names[i] = g.Name
				if want, ok := tt.wantParts[g.Name]; ok && !reflect.DeepEqual(groupedNames(g), want) {
					t.Fatalf("Group() parts of %q = %v, want %v", g.Name, groupedNames(g), want)
				}
			}
			if !reflect.DeepEqual(names, tt.wantNames) {
				t.Fatalf("Group() groups = %v, want %v", names, tt.wantNames)
			}
		})
	}
}
