package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/vision"
)

// Look finds the Claude Code CLI: the configured path, then `claude` on PATH, then the native installer's locations,
// then the copy the Claude Code editor extension ships (the newest version wins).
func Look(configured string) (string, error) {
	if configured != "" {
		if isExecutable(configured) {
			return configured, nil
		}
		return "", fmt.Errorf("Claude Code not found at %s", configured)
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("Claude Code (claude) is not on PATH")
	}
	name := "claude"
	if goruntime.GOOS == "windows" {
		name = "claude.exe"
	}
	for _, p := range []string{filepath.Join(home, ".local", "bin", name), filepath.Join(home, ".claude", "local", name)} {
		if isExecutable(p) {
			return p, nil
		}
	}
	var bundled []string
	for _, editor := range []string{".vscode", ".vscode-oss", ".vscode-insiders", ".cursor", ".windsurf"} {
		matches, _ := filepath.Glob(filepath.Join(home, editor, "extensions", "anthropic.claude-code-*", "resources", "native-binary", name))
		for _, m := range matches {
			if isExecutable(m) {
				bundled = append(bundled, m)
			}
		}
	}
	if len(bundled) == 0 {
		return "", errors.New("Claude Code (claude) is not on PATH and no editor extension ships it")
	}
	sort.SliceStable(bundled, func(i, j int) bool { return newer(extensionVersion(bundled[i]), extensionVersion(bundled[j])) })
	return bundled[0], nil
}

func isExecutable(path string) bool {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	return goruntime.GOOS == "windows" || st.Mode().Perm()&0o111 != 0
}

// extensionVersion reads 2.1.304 out of .../anthropic.claude-code-2.1.304-linux-x64/resources/...
func extensionVersion(path string) []int {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		rest, ok := strings.CutPrefix(part, "anthropic.claude-code-")
		if !ok {
			continue
		}
		version, _, _ := strings.Cut(rest, "-")
		var out []int
		for _, n := range strings.Split(version, ".") {
			v, err := strconv.Atoi(n)
			if err != nil {
				return out
			}
			out = append(out, v)
		}
		return out
	}
	return nil
}

func newer(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return len(a) > len(b)
}

// Info is what Relo learns from the CLI before running anything on it.
type Info struct {
	Version string
	Plan    string // "Claude Max", "API key", ...: how the CLI is signed in, never the account
	Models  []runtime.Model
}

// Probe checks that the CLI is signed in and lists the models it offers. It makes no model call: the list comes from
// the CLI's initialize reply. Nothing about the account beyond the kind of sign-in is kept.
func Probe(ctx context.Context, bin string, env []string) (Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "auth", "status")
	cmd.Env = env
	out, err := cmd.Output()
	var auth struct {
		LoggedIn         bool   `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if jerr := json.Unmarshal(out, &auth); jerr != nil {
		if err == nil {
			err = jerr
		}
		return Info{}, fmt.Errorf("claude auth status: %w", err)
	}
	if !auth.LoggedIn {
		return Info{}, errors.New("Claude Code is not signed in; run `claude` once in a terminal to sign in")
	}
	info := Info{Plan: "API key"}
	if auth.SubscriptionType != "" {
		info.Plan = "Claude " + strings.ToUpper(auth.SubscriptionType[:1]) + auth.SubscriptionType[1:]
	}
	reply, err := initialize(ctx, bin, env)
	if err != nil {
		return Info{}, err
	}
	info.Version = reply.Version
	info.Models = catalog(reply.Models)
	if len(info.Models) == 0 {
		return Info{}, errors.New("Claude Code offered no models")
	}
	return info, nil
}

type initReply struct {
	Version string      `json:"claude_code_version"`
	Models  []initModel `json:"models"`
}

type initModel struct {
	Value         string   `json:"value"`
	ResolvedModel string   `json:"resolvedModel"`
	DisplayName   string   `json:"displayName"`
	Efforts       []string `json:"supportedEffortLevels"`
}

// initialize starts the CLI in stream mode just long enough to read its initialize reply.
func initialize(ctx context.Context, bin string, env []string) (initReply, error) {
	cmd := exec.CommandContext(ctx, bin, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--tools", "", "--safe-mode", "--strict-mcp-config", "--no-session-persistence")
	cmd.Env = env
	cmd.Dir = os.TempDir()
	cmd.SysProcAttr = childAttr()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return initReply{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return initReply{}, err
	}
	if err := cmd.Start(); err != nil {
		return initReply{}, err
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	if err := writeLine(stdin, controlRequest("relo_probe", map[string]any{"subtype": "initialize", "hooks": nil})); err != nil {
		return initReply{}, err
	}
	lines := newLineReader(stdout)
	for {
		line, err := lines.next()
		if err != nil {
			return initReply{}, fmt.Errorf("Claude Code did not answer initialize: %w", err)
		}
		var msg struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string    `json:"subtype"`
				RequestID string    `json:"request_id"`
				Error     string    `json:"error"`
				Response  initReply `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(line, &msg) != nil || msg.Type != "control_response" || msg.Response.RequestID != "relo_probe" {
			continue
		}
		if msg.Response.Subtype != "success" {
			return initReply{}, fmt.Errorf("Claude Code refused initialize: %s", msg.Response.Error)
		}
		return msg.Response.Response, nil
	}
}

// catalog turns the CLI's model list into Relo models, one per resolved model id. Aliases such as "default" resolve
// to a model listed under its own name too, and that entry's name is the one kept.
func catalog(models []initModel) []runtime.Model {
	var out []runtime.Model
	index := map[string]int{}
	for _, m := range models {
		id := m.ResolvedModel
		if id == "" {
			id = m.Value
		}
		if id == "" || id == "default" {
			continue
		}
		entry := runtime.Model{ID: id, Provider: ProviderID, Name: m.DisplayName, Efforts: append([]string(nil), m.Efforts...),
			Vision: vision.Yes, VisionSource: vision.SourceCatalog}
		if i, ok := index[id]; ok {
			if m.Value != "default" {
				out[i].Name = m.DisplayName
			}
			continue
		}
		if m.Value == "default" {
			entry.Name = ""
		}
		index[id] = len(out)
		out = append(out, entry)
	}
	for i := range out {
		if out[i].Name == "" {
			out[i].Name = out[i].ID
		}
	}
	return out
}
