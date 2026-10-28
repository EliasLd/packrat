package link

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Part is a link assigned to its archive group and target location.
type Part struct {
	Link
	DirName  string // target directory, named after the archive
	FileName string // target file name
	num      int    // part number extracted from the file name, 0 when unnumbered
}

// Group is a named set of parts forming one archive.
type Group struct {
	Name  string
	Parts []*Part
}

var partRe = regexp.MustCompile(`(?i)^(.+)\.part(\d+)\.rar$`)

// Groups assigns each link the directory and file name it must be downloaded
// to, keeping parts of the same archive together. Groups appear in
// first-appearance order; parts inside a group are ordered by part number
// first, then file name.
func Groups(links []Link) []Group {
	var groups []Group
	index := map[string]int{}

	for i := range links {
		p := &Part{Link: links[i]}
		var base string
		if m := partRe.FindStringSubmatch(links[i].Name); m != nil {
			base, p.FileName = m[1], links[i].Name
			p.num = toInt(m[2])
		} else if links[i].Name != "" {
			base, p.FileName = strings.TrimSuffix(links[i].Name, filepath.Ext(links[i].Name)), links[i].Name
		} else {
			base, p.FileName = "unknown", links[i].ID
		}
		p.DirName, p.FileName = sanitize(base), sanitize(p.FileName)

		idx, ok := index[p.DirName]
		if !ok {
			idx = len(groups)
			index[p.DirName] = idx
			groups = append(groups, Group{Name: p.DirName})
		}
		groups[idx].Parts = append(groups[idx].Parts, p)
	}

	for _, g := range groups {
		sort.Slice(g.Parts, func(i, j int) bool {
			if g.Parts[i].num != g.Parts[j].num {
				return g.Parts[i].num < g.Parts[j].num
			}
			return g.Parts[i].FileName < g.Parts[j].FileName
		})
	}
	return groups
}

var sanitizeRe = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)

// sanitize makes a name safe as a file system path element on any platform.
func sanitize(s string) string {
	return strings.TrimSpace(sanitizeRe.ReplaceAllString(s, "_"))
}

func toInt(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
