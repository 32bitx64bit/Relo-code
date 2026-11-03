package orchestrator

import (
	"bytes"
	"io/fs"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	ctxeng "github.com/relocode/relo/internal/context"
)

// The repository map is the Primary's picture of the project's shape: which areas exist and what they hold, listed by
// Relo from the tree. It carries no code, so the Primary can place work and name owners without reading a file, and
// it is a stable block, so a turn re-reads it from cache instead of paying to write it again.
const (
	repoMapMaxBytes  = 5000
	repoMapMaxAreas  = 48
	repoMapMaxFiles  = 30000
	repoMapRootFiles = 14
)

// repoMapSkip names directories that are never the project's own shape.
var repoMapSkip = map[string]bool{".git": true, ".relo": true, "node_modules": true, "vendor": true, "target": true, "dist": true, "build": true, "__pycache__": true, ".venv": true, ".next": true}

// listProjectFiles returns the workspace's files, relative and slash-separated: tracked and untracked-but-not-ignored
// when it is a Git repository, a plain walk otherwise.
func listProjectFiles(root string) []string {
	var files []string
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	if out, err := cmd.Output(); err == nil {
		for _, name := range bytes.Split(out, []byte{0}) {
			if len(name) > 0 {
				files = append(files, string(name))
			}
		}
	} else {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != root && (repoMapSkip[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if rel, err := filepath.Rel(root, p); err == nil {
				files = append(files, filepath.ToSlash(rel))
			}
			if len(files) >= repoMapMaxFiles {
				return filepath.SkipAll
			}
			return nil
		})
	}
	kept := files[:0]
	for _, f := range files {
		if skipped(f) {
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

func skipped(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if repoMapSkip[part] {
			return true
		}
	}
	return false
}

// buildRepoMap summarizes files as areas: directories deep enough to tell apart, shallow enough to fit. Each line
// gives a count and the commonest extensions. Root files are named, since a README or manifest says what the project is.
func buildRepoMap(files []string) string {
	if len(files) == 0 {
		return ""
	}
	var root []string
	for _, f := range files {
		if !strings.Contains(f, "/") {
			root = append(root, f)
		}
	}
	sort.Strings(root)
	type area struct {
		files int
		exts  map[string]int
	}
	group := func(depth int) (map[string]*area, []string) {
		areas := map[string]*area{}
		for _, f := range files {
			dir := path.Dir(f)
			if dir == "." {
				continue
			}
			parts := strings.Split(dir, "/")
			if len(parts) > depth {
				parts = parts[:depth]
			}
			key := strings.Join(parts, "/") + "/"
			a := areas[key]
			if a == nil {
				a = &area{exts: map[string]int{}}
				areas[key] = a
			}
			a.files++
			ext := path.Ext(f)
			if ext == "" {
				ext = path.Base(f)
			}
			a.exts[ext]++
		}
		keys := make([]string, 0, len(areas))
		for k := range areas {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return areas, keys
	}
	// Go as deep as the budget of areas allows, so a big tree is still shown at its top.
	areas, keys := group(1)
	for depth := 2; depth <= 4; depth++ {
		deeper, deeperKeys := group(depth)
		if len(deeperKeys) > repoMapMaxAreas {
			break
		}
		areas, keys = deeper, deeperKeys
	}

	var b strings.Builder
	b.WriteString("Listed by Relo from the tree: areas with file counts. It holds no code; for what a file contains, ask.\n")
	if len(root) > 0 {
		shown := root
		if len(shown) > repoMapRootFiles {
			shown = shown[:repoMapRootFiles]
		}
		b.WriteString("Root files: " + strings.Join(shown, ", "))
		if len(root) > len(shown) {
			b.WriteString(", and " + strconv.Itoa(len(root)-len(shown)) + " more")
		}
		b.WriteString("\n")
	}
	for i, k := range keys {
		a := areas[k]
		type kv struct {
			ext string
			n   int
		}
		var exts []kv
		for e, n := range a.exts {
			exts = append(exts, kv{e, n})
		}
		sort.Slice(exts, func(i, j int) bool {
			if exts[i].n != exts[j].n {
				return exts[i].n > exts[j].n
			}
			return exts[i].ext < exts[j].ext
		})
		if len(exts) > 3 {
			exts = exts[:3]
		}
		var parts []string
		for _, e := range exts {
			parts = append(parts, e.ext+" "+strconv.Itoa(e.n))
		}
		line := "- " + k + "  " + strconv.Itoa(a.files) + " files (" + strings.Join(parts, ", ") + ")\n"
		if b.Len()+len(line) > repoMapMaxBytes {
			b.WriteString("- ... " + strconv.Itoa(len(keys)-i) + " more areas\n")
			break
		}
		b.WriteString(line)
	}
	return strings.TrimSpace(b.String())
}

// setRepoMap puts the project's map in the Primary's stable context. An unchanged map leaves the context as it was.
func (c *Coord) setRepoMap(primaryID string) {
	s := c.Current()
	if s == nil || s.Workspace == "" {
		return
	}
	text := buildRepoMap(listProjectFiles(s.Workspace))
	var items []ctxeng.Item
	if text != "" {
		items = []ctxeng.Item{{Source: ctxeng.SourceRepo, Content: text, Durability: 1, Relevance: 1, Pinned: true}}
	}
	c.Engine.SetTaskState(primaryID, "Repository map", items)
}
