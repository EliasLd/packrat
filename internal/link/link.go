// Package link turns a links file (plain URLs or markdown bullet lists of
// landing page URLs) into structured links, and groups the multi-part links
// into one archive group per downloadable file set.
package link

import (
	"net/url"
	"regexp"
	"strings"
)

// Link is a parsed reference to a file host landing page.
type Link struct {
	PageURL string // landing page URL, without fragment
	ID      string // file identifier: last path segment
	Name    string // target file name from the URL fragment, empty when absent
}

// Issue is a non-fatal input problem: the offending line was skipped, the
// rest of the file is still parsed. Callers report issues to the user.
type Issue struct {
	Line   int // 1-based line number in the input
	Reason string
}

var markdownRe = regexp.MustCompile(`\[([^\]]*)\]\((https?://[^)\s]+)\)`)

// Parse converts the content of a links file into links. Every line is
// treated independently: unknown, malformed or duplicate lines are reported
// as issues and skipped instead of failing the whole file.
func Parse(data []byte) ([]Link, []Issue) {
	var (
		links  []Link
		issues []Issue
	)
	seen := map[string]bool{}

	for i, raw := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		line = strings.TrimSpace(strings.TrimPrefix(line, "-"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		urlText := ""
		if m := markdownRe.FindStringSubmatch(line); m != nil {
			urlText = m[2]
		} else {
			for _, field := range strings.Fields(line) {
				if strings.HasPrefix(field, "http://") || strings.HasPrefix(field, "https://") {
					urlText = strings.Trim(field, ",;")
					break
				}
			}
			// fall back to any URL-looking token so that non-http schemes
			// surface as a clear "not a usable URL" issue
			if urlText == "" {
				for _, field := range strings.Fields(line) {
					if strings.Contains(field, "://") {
						urlText = strings.Trim(field, ",;")
						break
					}
				}
			}
		}
		if urlText == "" {
			issues = append(issues, Issue{Line: lineNo, Reason: "no URL found"})
			continue
		}

		parsed, err := url.Parse(urlText)
		if err != nil {
			issues = append(issues, Issue{Line: lineNo, Reason: "unparseable URL"})
			continue
		}
		id := parsed.Path[strings.LastIndex(parsed.Path, "/")+1:]
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || id == "" {
			issues = append(issues, Issue{Line: lineNo, Reason: "not a usable http(s) file URL"})
			continue
		}

		page := parsed.Scheme + "://" + parsed.Host + parsed.Path
		if seen[page] {
			issues = append(issues, Issue{Line: lineNo, Reason: "duplicate link"})
			continue
		}
		seen[page] = true

		links = append(links, Link{PageURL: page, ID: id, Name: parsed.Fragment})
	}
	return links, issues
}
