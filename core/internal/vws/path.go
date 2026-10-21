package vws

import (
	"errors"
	"path"
	"strings"
)

// SplitPath canonicalises a workspace-relative path into its components. It rejects what Relo's other path checks
// reject: absolute paths, drive letters, parent traversal, control characters and the metadata folders. An empty
// path or "." names the root and returns no components; callers that need a file reject that themselves.
func SplitPath(rel string) ([]string, error) {
	if rel == "" || rel == "." {
		return nil, nil
	}
	if strings.ContainsAny(rel, "\x00\n\r") {
		return nil, pathErr(rel, ErrInvalidPath)
	}
	canonical := strings.ReplaceAll(rel, `\`, "/")
	if strings.HasPrefix(canonical, "/") || (len(canonical) > 1 && canonical[1] == ':') {
		return nil, pathErr(rel, ErrInvalidPath)
	}
	for _, part := range strings.Split(canonical, "/") {
		if part == ".." {
			return nil, pathErr(rel, ErrInvalidPath)
		}
	}
	clean := path.Clean(canonical)
	if clean == "." {
		return nil, nil
	}
	parts := strings.Split(clean, "/")
	for i, part := range parts {
		if isDotGit(part) {
			return nil, pathErr(rel, ErrProtected)
		}
		if i == 0 && strings.EqualFold(part, ".relo") {
			return nil, pathErr(rel, ErrProtected)
		}
	}
	return parts, nil
}

// splitFile is SplitPath for a path that must name something below the root.
func splitFile(rel string) ([]string, error) {
	parts, err := SplitPath(rel)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, pathErr(rel, ErrInvalidPath)
	}
	return parts, nil
}

// isDotGit reports whether a path component is, or on some filesystem would collapse to, the .git folder.
func isDotGit(name string) bool {
	trimmed := strings.ToLower(strings.TrimRight(name, " ."))
	return trimmed == ".git" || trimmed == "git~1"
}

func join(parts []string) string { return strings.Join(parts, "/") }

// Protected reports whether rel names Git or Relo metadata, which no agent may read or write.
func Protected(rel string) bool {
	_, err := SplitPath(rel)
	return err != nil && isProtected(err)
}

func isProtected(err error) bool { return errors.Is(err, ErrProtected) }

// symlinkStaysInside reports whether a link stored at linkPath with the given target resolves inside the workspace.
// Targets are resolved lexically against the link's directory; an absolute target always escapes.
func symlinkStaysInside(linkPath, target string) bool {
	if target == "" || strings.ContainsAny(target, "\x00") {
		return false
	}
	t := strings.ReplaceAll(target, `\`, "/")
	if strings.HasPrefix(t, "/") || (len(t) > 1 && t[1] == ':') {
		return false
	}
	dir := path.Dir(linkPath)
	if dir == "." {
		dir = ""
	}
	resolved := path.Clean(path.Join(dir, t))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return false
	}
	if resolved != "." {
		for i, part := range strings.Split(resolved, "/") {
			if isDotGit(part) || (i == 0 && strings.EqualFold(part, ".relo")) {
				return false
			}
		}
	}
	return true
}
