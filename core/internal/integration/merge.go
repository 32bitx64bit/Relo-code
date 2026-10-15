package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

type Conflict struct {
	Path   string   `json:"path"`
	Agents []string `json:"agents"`
}

type ConflictReport struct {
	Files  []string `json:"files"`
	Reason string   `json:"reason"`
}

type ConflictError struct {
	Report ConflictReport
}

func (e ConflictError) Error() string { return "integration conflict: " + e.Report.Reason }

type Result struct {
	Applied   []string        `json:"applied"`
	Conflicts []Conflict      `json:"conflicts"`
	Blocked   []string        `json:"blocked"`
	Conflict  *ConflictReport `json:"conflict,omitempty"`
}

func Overlap(a, b []string) []string {
	seen := map[string]bool{}
	for _, x := range a {
		seen[x] = true
	}
	var out []string
	for _, x := range b {
		if seen[x] {
			out = append(out, x)
		}
	}
	return out
}

func Detect(byAgent map[string][]string) []Conflict {
	file := map[string][]string{}
	for agent, files := range byAgent {
		for _, f := range files {
			file[f] = append(file[f], agent)
		}
	}
	var out []Conflict
	for f, agents := range file {
		if len(agents) > 1 {
			out = append(out, Conflict{Path: f, Agents: agents})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func Apply(root, patch string, protected []string) (Result, error) {
	return ApplyAgainst(root, patch, protected, "")
}

func ApplyAgainst(root, patch string, protected []string, base string) (Result, error) {
	res := Result{}
	if strings.TrimSpace(patch) == "" {
		return res, nil
	}
	files, err := patchFiles(patch)
	if err != nil {
		return conflict(res, nil, err.Error())
	}
	for _, file := range files {
		for _, p := range protected {
			if samePath(file, p) {
				res.Blocked = append(res.Blocked, file)
			}
		}
	}
	if len(res.Blocked) > 0 {
		res.Blocked = uniquePaths(res.Blocked)
		return conflict(res, res.Blocked, "path was dirty at session start")
	}
	var dirtyPaths []string
	if base != "" {
		dirty, statusErr := git(root, "diff", "--name-only", "-z", base, "--")
		if statusErr != nil {
			return res, statusErr
		}
		dirtyPaths = strings.Split(dirty, "\x00")
		staged, statusErr := git(root, "diff", "--cached", "--name-only", "-z", base, "--")
		if statusErr != nil {
			return res, statusErr
		}
		dirtyPaths = append(dirtyPaths, strings.Split(staged, "\x00")...)
		dirty, statusErr = git(root, "ls-files", "--others", "--exclude-standard", "-z")
		if statusErr != nil {
			return res, statusErr
		}
		dirtyPaths = append(dirtyPaths, strings.Split(dirty, "\x00")...)
	} else {
		dirty, statusErr := git(root, "status", "--porcelain", "-z")
		if statusErr != nil {
			return res, statusErr
		}
		dirtyPaths = statusPaths(dirty)
	}
	for _, path := range dirtyPaths {
		for _, file := range files {
			if samePath(file, path) {
				res.Blocked = append(res.Blocked, file)
			}
		}
	}
	if len(res.Blocked) > 0 {
		res.Blocked = uniquePaths(res.Blocked)
		return conflict(res, res.Blocked, "path has local modifications")
	}
	for _, file := range files {
		folded := file
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			folded = strings.ToLower(folded)
		}
		if strings.HasPrefix(folded, ".git/") || folded == ".git" || strings.HasPrefix(folded, ".relo/") || folded == ".relo" {
			return conflict(res, []string{file}, "protected metadata path")
		}
		if err := checkSymlinkPath(root, file); err != nil {
			return conflict(res, []string{file}, err.Error())
		}
	}
	tmp, err := os.CreateTemp("", "relo-*.patch")
	if err != nil {
		return res, err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.WriteString(patch); err != nil {
		_ = tmp.Close()
		return res, err
	}
	if err = tmp.Close(); err != nil {
		return res, err
	}
	check := exec.Command("git", "apply", "--check", tmp.Name())
	check.Dir = root
	if out, checkErr := check.CombinedOutput(); checkErr != nil {
		return conflict(res, files, "git apply --check failed: "+strings.TrimSpace(string(out)))
	}
	apply := exec.Command("git", "apply", tmp.Name())
	apply.Dir = root
	if out, applyErr := apply.CombinedOutput(); applyErr != nil {
		return conflict(res, files, "git apply failed: "+strings.TrimSpace(string(out)))
	}
	res.Applied = files
	return res, nil
}

func uniquePaths(paths []string) []string {
	sort.Strings(paths)
	out := paths[:0]
	for _, path := range paths {
		if len(out) == 0 || out[len(out)-1] != path {
			out = append(out, path)
		}
	}
	return out
}

func conflict(res Result, files []string, reason string) (Result, error) {
	report := ConflictReport{Files: append([]string(nil), files...), Reason: reason}
	res.Conflict = &report
	return res, ConflictError{Report: report}
}

func git(root string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func statusPaths(status string) []string {
	fields := strings.Split(status, "\x00")
	var paths []string
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			continue
		}
		path := entry[3:]
		paths = append(paths, path)
		if strings.Contains(entry[:2], "R") || strings.Contains(entry[:2], "C") {
			i++
			if i < len(fields) && fields[i] != "" {
				paths = append(paths, fields[i])
			}
		}
	}
	return paths
}

func patchFiles(patch string) ([]string, error) {
	seen := map[string]bool{}
	var files []string
	for _, line := range strings.Split(patch, "\n") {
		var raw string
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			if at := strings.Index(line, "\t"); at >= 0 {
				line = line[:at]
			}
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			old, next, err := diffHeaderPaths(strings.TrimPrefix(line, "diff --git "))
			if err != nil {
				return nil, err
			}
			for _, path := range []string{old, next} {
				path, err = normalizePath(strings.TrimPrefix(strings.TrimPrefix(path, "a/"), "b/"))
				if err != nil {
					return nil, err
				}
				if !seen[path] {
					files = append(files, path)
					seen[path] = true
				}
			}
			continue
		case strings.HasPrefix(line, "+++ "):
			raw = strings.TrimPrefix(line, "+++ ")
			prefix := "b/"
			if strings.HasPrefix(raw, "\"") {
				decoded, err := strconv.Unquote(raw)
				if err != nil {
					return nil, fmt.Errorf("invalid quoted patch path %q", raw)
				}
				raw = decoded
			} else if !strings.HasPrefix(raw, prefix) && raw != "/dev/null" {
				continue
			}
			raw = strings.TrimPrefix(raw, prefix)
		case strings.HasPrefix(line, "--- "):
			raw = strings.TrimPrefix(line, "--- ")
			prefix := "a/"
			if strings.HasPrefix(raw, "\"") {
				decoded, err := strconv.Unquote(raw)
				if err != nil {
					return nil, fmt.Errorf("invalid quoted patch path %q", raw)
				}
				raw = decoded
			} else if !strings.HasPrefix(raw, prefix) && raw != "/dev/null" {
				continue
			}
			raw = strings.TrimPrefix(raw, prefix)
		case strings.HasPrefix(line, "rename from ") || strings.HasPrefix(line, "rename to ") || strings.HasPrefix(line, "copy from ") || strings.HasPrefix(line, "copy to "):
			var err error
			raw, err = decodePath(strings.TrimSpace(line[strings.IndexByte(line, ' ')+1:]))
			if err != nil {
				return nil, err
			}
		default:
			continue
		}
		if raw == "/dev/null" {
			continue
		}
		path, err := normalizePath(raw)
		if err != nil {
			return nil, err
		}
		if !seen[path] {
			files = append(files, path)
			seen[path] = true
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("patch contains no file paths")
	}
	return files, nil
}

func diffHeaderPaths(value string) (string, string, error) {
	var paths []string
	for len(strings.TrimSpace(value)) > 0 {
		value = strings.TrimLeft(value, " \t")
		token := value
		if strings.HasPrefix(value, "\"") {
			end := 1
			for end < len(value) {
				if value[end] == '\\' {
					end += 2
					continue
				}
				if value[end] == '"' {
					end++
					break
				}
				end++
			}
			if end > len(value) || value[end-1] != '"' {
				return "", "", fmt.Errorf("invalid quoted diff path")
			}
			token, value = value[:end], value[end:]
		} else if fields := strings.Fields(value); len(fields) > 0 {
			token, value = fields[0], strings.TrimPrefix(value[len(fields[0]):], " ")
		}
		path, err := decodePath(token)
		if err != nil {
			return "", "", err
		}
		paths = append(paths, path)
	}
	if len(paths) != 2 {
		return "", "", fmt.Errorf("invalid diff header paths")
	}
	return paths[0], paths[1], nil
}

func decodePath(path string) (string, error) {
	if strings.HasPrefix(path, "\"") {
		decoded, err := strconv.Unquote(path)
		if err != nil {
			return "", fmt.Errorf("invalid quoted patch path %q", path)
		}
		path = decoded
	}
	return path, nil
}

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

func samePath(a, b string) bool {
	a, errA := normalizePath(a)
	b, errB := normalizePath(b)
	if errA != nil || errB != nil {
		return false
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
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

func CopyFile(fromRoot, toRoot, rel string) error {
	normalized, err := normalizePath(rel)
	if err != nil {
		return err
	}
	src := filepath.Join(fromRoot, filepath.FromSlash(normalized))
	dst := filepath.Join(toRoot, filepath.FromSlash(normalized))
	if err := checkSymlinkPath(fromRoot, normalized); err != nil {
		return err
	}
	if err := checkSymlinkPath(toRoot, normalized); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
