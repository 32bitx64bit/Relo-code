package vws

import (
	"os"
	"path/filepath"
	"strings"
)

// Compat says whether a repository can be viewed without a checkout. A view returns the bytes Git stores. When
// Git rewrites content on the way in or out (core.autocrlf, text and eol attributes, clean and smudge filters such
// as LFS), a checkout holds different bytes from the blobs, so a view and a worktree would disagree about the
// same file and a three-way merge of one against the other would invent conflicts. Such repositories stay on
// worktrees.
type Compat struct {
	OK      bool     `json:"ok"`
	Reasons []string `json:"reasons,omitempty"`
}

// checkCompat reads the repository-wide settings. Attributes in the tree are checked per base by attributeRisk.
func checkCompat(r *repo) Compat {
	var reasons []string
	if out, err := r.git("config", "--get-regexp", `^(core\.(autocrlf|eol|attributesfile)|filter\..+\.(clean|smudge|process))$`); err == nil {
		for _, line := range strings.Split(out, "\n") {
			key, val, _ := strings.Cut(strings.TrimSpace(line), " ")
			key = strings.ToLower(key)
			val = strings.TrimSpace(val)
			switch {
			case key == "core.autocrlf":
				if v := strings.ToLower(val); v == "true" || v == "input" {
					reasons = append(reasons, "core.autocrlf="+val)
				}
			case key == "core.eol":
				if strings.EqualFold(val, "crlf") {
					reasons = append(reasons, "core.eol=crlf")
				}
			case key == "core.attributesfile":
				path := val
				if strings.HasPrefix(path, "~/") {
					if home, err := os.UserHomeDir(); err == nil {
						path = filepath.Join(home, path[2:])
					}
				}
				if data, err := os.ReadFile(path); err == nil {
					reasons = append(reasons, attributeReasons(path, string(data))...)
				}
			case strings.HasPrefix(key, "filter."):
				reasons = append(reasons, key+" is configured")
			}
		}
	}
	if gitPath, err := r.git("rev-parse", "--git-path", "info/attributes"); err == nil {
		if !filepath.IsAbs(gitPath) {
			gitPath = filepath.Join(r.root, gitPath)
		}
		if data, err := os.ReadFile(gitPath); err == nil {
			reasons = append(reasons, attributeReasons("info/attributes", string(data))...)
		}
	}
	return Compat{OK: len(reasons) == 0, Reasons: reasons}
}

// attributeRisk lists the reasons the attribute files inside a base tree rewrite content. The result is cached per
// tree, since many agents share one base.
func (e *Engine) attributeRisk(baseTree string) []string {
	e.attrMu.Lock()
	if reasons, ok := e.attrSeen[baseTree]; ok {
		e.attrMu.Unlock()
		return reasons
	}
	e.attrMu.Unlock()
	var reasons []string
	out, err := e.repo.gitRaw("ls-tree", "-r", "-z", "--name-only", baseTree)
	if err == nil {
		for _, name := range strings.Split(string(out), "\x00") {
			if name != ".gitattributes" && !strings.HasSuffix(name, "/.gitattributes") {
				continue
			}
			parts := strings.Split(name, "/")
			entry, found, lerr := e.repo.lookup(baseTree, parts)
			if lerr != nil || !found || !entry.isFile() {
				continue
			}
			if data, rerr := e.repo.readBlob(entry.OID); rerr == nil {
				reasons = append(reasons, attributeReasons(name, string(data))...)
			}
		}
	}
	e.attrMu.Lock()
	e.attrSeen[baseTree] = reasons
	e.attrMu.Unlock()
	return reasons
}

// attributeReasons reports each line of a gitattributes file that makes Git rewrite content.
func attributeReasons(file, text string) []string {
	var reasons []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "[attr]") {
			continue
		}
		for _, attr := range fields[1:] {
			name, _, _ := strings.Cut(attr, "=")
			switch name {
			case "filter", "eol", "ident", "working-tree-encoding", "text", "crlf":
				// "-text" and "!text" carry their sign in the name, so they never match.
				reasons = append(reasons, file+": "+line)
			}
		}
	}
	return reasons
}
