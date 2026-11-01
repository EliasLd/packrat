package link_test

import (
	"reflect"
	"testing"

	"github.com/EliasLd/packrat/internal/link"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantLinks  []link.Link
		wantIssues []link.Issue
	}{
		{
			name:  "plain url with fragment",
			input: "https://downloads.example/abc123#Game.x--multi.part01.rar\n",
			wantLinks: []link.Link{
				{PageURL: "https://downloads.example/abc123", ID: "abc123", Name: "Game.x--multi.part01.rar"},
			},
		},
		{
			name:  "markdown bullet with label",
			input: "- [Game file](https://downloads.example/def456#Game.part01.rar)\n",
			wantLinks: []link.Link{
				{PageURL: "https://downloads.example/def456", ID: "def456", Name: "Game.part01.rar"},
			},
		},
		{
			name:  "star bullet with url as label",
			input: "*   [https://downloads.example/qwe#f.part01.rar](https://downloads.example/qwe#f.part01.rar)\n",
			wantLinks: []link.Link{
				{PageURL: "https://downloads.example/qwe", ID: "qwe", Name: "f.part01.rar"},
			},
		},
		{
			name:  "comments, blanks and CRLF line endings",
			input: "# header\r\n\r\n- [file](https://downloads.example/1#one.rar)\r\n",
			wantLinks: []link.Link{
				{PageURL: "https://downloads.example/1", ID: "1", Name: "one.rar"},
			},
		},
		{
			name:  "fragment is decoded",
			input: "https://downloads.example/ok#My%20Game.part01.rar\n",
			wantLinks: []link.Link{
				{PageURL: "https://downloads.example/ok", ID: "ok", Name: "My Game.part01.rar"},
			},
		},
		{
			name:  "trailing punctuation is trimmed",
			input: "https://downloads.example/id9#small.rar,\nhttps://downloads.example/id8#tiny.rar;\n",
			wantLinks: []link.Link{
				{PageURL: "https://downloads.example/id9", ID: "id9", Name: "small.rar"},
				{PageURL: "https://downloads.example/id8", ID: "id8", Name: "tiny.rar"},
			},
		},
		{
			name:      "empty input",
			input:     "",
			wantLinks: nil,
		},
		{
			name:  "path-named host names the link from its URL path",
			input: "https://filekeeper.net/l8e29ola677v/CoD_Modern_Warfare_II_--_fitgirl-repacks.site_--_.part01.rar\n",
			wantLinks: []link.Link{
				{PageURL: "https://filekeeper.net/l8e29ola677v/CoD_Modern_Warfare_II_--_fitgirl-repacks.site_--_.part01.rar", ID: "l8e29ola677v", Name: "CoD_Modern_Warfare_II_--_fitgirl-repacks.site_--_.part01.rar"},
			},
		},
		{
			name:  "code-only url on a path-named host keeps the name empty",
			input: "https://filekeeper.net/qywzzpq4hcwa\n",
			wantLinks: []link.Link{
				{PageURL: "https://filekeeper.net/qywzzpq4hcwa", ID: "qywzzpq4hcwa"},
			},
		},
		{
			name:  "second path-named host rides the same shape",
			input: "https://datanodes.to/abc123/Game.x--multi.part02.rar\n",
			wantLinks: []link.Link{
				{PageURL: "https://datanodes.to/abc123/Game.x--multi.part02.rar", ID: "abc123", Name: "Game.x--multi.part02.rar"},
			},
		},
		{
			name:  "a fragment outranks the path file name on a path-named host",
			input: "https://filekeeper.net/abc123/Some.File.part01.rar#My.Game.part02.rar\n",
			wantLinks: []link.Link{
				{PageURL: "https://filekeeper.net/abc123/Some.File.part01.rar", ID: "abc123", Name: "My.Game.part02.rar"},
			},
		},
		{
			name:      "a code-only and a named variant of one code are duplicates",
			input:     "https://filekeeper.net/abc123\nhttps://filekeeper.net/abc123/Game.part01.rar\n",
			wantLinks: []link.Link{{PageURL: "https://filekeeper.net/abc123", ID: "abc123"}},
			wantIssues: []link.Issue{
				{Line: 2, Reason: "duplicate link"},
			},
		},
		{
			name:  "distinct codes on a path-named host are kept apart",
			input: "https://filekeeper.net/aaa111/Game.part01.rar\nhttps://filekeeper.net/bbb222/Game.part02.rar\n",
			wantLinks: []link.Link{
				{PageURL: "https://filekeeper.net/aaa111/Game.part01.rar", ID: "aaa111", Name: "Game.part01.rar"},
				{PageURL: "https://filekeeper.net/bbb222/Game.part02.rar", ID: "bbb222", Name: "Game.part02.rar"},
			},
		},
		{
			name:  "regular hosts keep the fragment-only naming",
			input: "https://fuckingfast.co/abc123#Game.part01.rar\nhttps://other.example/def456#Game.part02.rar\n",
			wantLinks: []link.Link{
				{PageURL: "https://fuckingfast.co/abc123", ID: "abc123", Name: "Game.part01.rar"},
				{PageURL: "https://other.example/def456", ID: "def456", Name: "Game.part02.rar"},
			},
		},
		{
			name:      "url without file id is reported and skipped",
			input:     "just some prose\nhttps://downloads.example/ok#a.rar\n",
			wantLinks: []link.Link{{PageURL: "https://downloads.example/ok", ID: "ok", Name: "a.rar"}},
			wantIssues: []link.Issue{
				{Line: 1, Reason: "no URL found"},
			},
		},
		{
			name:       "url without file id is reported and skipped",
			input:      "https://downloads.example/\n",
			wantIssues: []link.Issue{{Line: 1, Reason: "not a usable http(s) file URL"}},
		},
		{
			name:       "non http(s) scheme is reported and skipped",
			input:      "ftp://downloads.example/xyz\n",
			wantIssues: []link.Issue{{Line: 1, Reason: "not a usable http(s) file URL"}},
		},
		{
			name:      "duplicate page keeps first occurrence",
			input:     "https://downloads.example/ok#a.rar\n- [alias](https://downloads.example/ok#b.rar)\n",
			wantLinks: []link.Link{{PageURL: "https://downloads.example/ok", ID: "ok", Name: "a.rar"}},
			wantIssues: []link.Issue{
				{Line: 2, Reason: "duplicate link"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			links, issues := link.Parse([]byte(tt.input))
			if !reflect.DeepEqual(links, tt.wantLinks) {
				t.Fatalf("Parse() links = %+v, want %+v", links, tt.wantLinks)
			}
			if !reflect.DeepEqual(issues, tt.wantIssues) {
				t.Fatalf("Parse() issues = %+v, want %+v", issues, tt.wantIssues)
			}
		})
	}
}
