// Package pathglob matches workspace-relative, slash-separated paths against the small glob syntax Relo's path
// settings use (ignorePaths, unionPaths, takeLatestPaths).
package pathglob

import (
	"path"
	"strings"
)

// Match reports whether rel matches pattern.
//
//   - A pattern without a slash matches any path whose base name matches it: "*.md" matches "docs/a.md".
//   - A pattern with a slash is anchored at the workspace root: "Mod/Assemblies/*.dll".
//   - "**" as a whole segment matches zero or more segments: "**/obj/**" matches "a/b/obj/x/y.dll" and "obj/x".
//   - "*" and "?" never cross a slash.
//
// An empty pattern matches nothing.
func Match(pattern, rel string) bool {
	pattern = strings.TrimSpace(pattern)
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	if pattern == "" || rel == "" {
		return false
	}
	if !strings.Contains(pattern, "/") {
		ok, err := path.Match(pattern, path.Base(rel))
		return err == nil && ok
	}
	return matchSegments(strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(rel, "/"))
}

// Any reports whether rel matches at least one of patterns.
func Any(patterns []string, rel string) bool {
	for _, p := range patterns {
		if Match(p, rel) {
			return true
		}
	}
	return false
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			if len(rest) == 0 {
				return true
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegments(rest, segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], segs[0])
		if err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
