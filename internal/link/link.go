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
	ID      string // file code: first path segment on path-named hosts, last segment elsewhere
	Name    string // target file name: the fragment when present, else the URL path file name on path-named hosts
}

// Issue is a non-fatal input problem: the offending line was skipped, the
// rest of the file is still parsed. Callers report issues to the user.
type Issue struct {
	Line   int // 1-based line number in the input
	Reason string
}

var markdownRe = regexp.MustCompile(`\[([^\]]*)\]\((https?://[^)\s]+)\)`)

// pathNamedHosts address files by code first and file name last in the URL
// path, carrying no fragment on pasted links (repack pastes are
// browser-encrypted, so their links never have one). For these hosts the
// code is the file's identity: the first path segment names the link and
// two URLs with the same code are the same link even if one spells the
// file name and the other does not.
var pathNamedHosts = map[string]bool{
	"filekeeper.net": true,
	"datanodes.to":   true,
}

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
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			issues = append(issues, Issue{Line: lineNo, Reason: "not a usable http(s) file URL"})
			continue
		}

		var id, name string
		if pathNamedHosts[parsed.Host] {
			segments := pathSegments(parsed.Path)
			if len(segments) == 0 {
				issues = append(issues, Issue{Line: lineNo, Reason: "not a usable http(s) file URL"})
				continue
			}
			id = segments[0]
			name = parsed.Fragment
			if name == "" && len(segments) > 1 {
				name = segments[len(segments)-1]
			}
		} else {
			id = parsed.Path[strings.LastIndex(parsed.Path, "/")+1:]
			if id == "" {
				issues = append(issues, Issue{Line: lineNo, Reason: "not a usable http(s) file URL"})
				continue
			}
			name = parsed.Fragment
		}

		page := parsed.Scheme + "://" + parsed.Host + parsed.Path
		key := page
		if pathNamedHosts[parsed.Host] {
			key = parsed.Host + "/" + id
		}
		if seen[key] {
			issues = append(issues, Issue{Line: lineNo, Reason: "duplicate link"})
			continue
		}
		seen[key] = true

		links = append(links, Link{PageURL: page, ID: id, Name: name})
	}
	return links, issues
}

// pathSegments lists the non-empty elements of a URL path.
func pathSegments(p string) []string {
	var segments []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	return segments
}
