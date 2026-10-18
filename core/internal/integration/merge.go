package integration

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ConflictReport is the user-facing account of a refused three-way merge.
type ConflictReport struct {
	Files  []string `json:"files"`
	Reason string   `json:"reason"`
}

// ConflictError says a file was refused and nothing was written.
type ConflictError struct {
	Report ConflictReport
}

func (e ConflictError) Error() string { return "integration conflict: " + e.Report.Reason }

func normalizePath(raw string) (string, error) {
	if strings.ContainsAny(raw, "\x00\n\r") || filepath.IsAbs(raw) || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, `\\`) || (len(raw) > 1 && raw[1] == ':') {
		return "", fmt.Errorf("invalid patch path %q", raw)
	}
	canonical := strings.ReplaceAll(strings.ReplaceAll(raw, `\`, "/"), "\\", "/")
	for _, part := range strings.Split(canonical, "/") {
		if part == ".." {
			return "", fmt.Errorf("patch path contains parent traversal: %q", raw)
		}
	}
	clean := filepath.Clean(filepath.FromSlash(canonical))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("patch path escapes workspace: %q", raw)
	}
	return filepath.ToSlash(clean), nil
}

func checkSymlinkPath(root, rel string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	probe := target
	var suffix []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(probe)
		if resolveErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			resolved, err = filepath.Abs(resolved)
			if err != nil {
				return err
			}
			relPath, relErr := filepath.Rel(root, resolved)
			if relErr != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
				return fmt.Errorf("patch path escapes workspace through symlink")
			}
			return nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return resolveErr
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}
