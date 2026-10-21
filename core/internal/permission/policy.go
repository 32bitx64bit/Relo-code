package permission

import (
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

type Action string

const (
	Read        Action = "read"
	Write       Action = "write"
	Delete      Action = "delete"
	Test        Action = "test"
	Build       Action = "build"
	Install     Action = "install"
	GitCommit   Action = "git.commit"
	GitPush     Action = "git.push"
	Network     Action = "network"
	Outside     Action = "outside"
	Destructive Action = "destructive"
)

type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
)

type Policy struct {
	mu    sync.Mutex
	rules map[Action]Decision
	yolo  bool
}

func Default() *Policy {
	return &Policy{rules: map[Action]Decision{
		Read: Allow, Write: Allow, Test: Allow, Build: Allow,
		Delete: Ask, Install: Ask, GitCommit: Ask, GitPush: Ask,
		Network: Ask, Outside: Ask, Destructive: Ask,
	}}
}

func (p *Policy) Set(a Action, d Decision) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules[a] = d
}

// SetYolo turns auto-approval on or off. While it is on, every action the rules would ask about is allowed.
func (p *Policy) SetYolo(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.yolo = on
}

func (p *Policy) Yolo() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.yolo
}

// Get is the decision for an action. YOLO mode turns Ask into Allow; an action the user set to Deny stays denied,
// since refusing it is a decision, not a prompt.
func (p *Policy) Get(a Action) Decision {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.rules[a]
	if !ok {
		d = Ask
	}
	if d == Ask && p.yolo {
		return Allow
	}
	return d
}

func Classify(title, command, path, workspace string) Action {
	if pipeToShell.MatchString(command) {
		return Destructive
	}
	args, ok := parseCommand(command)
	if !ok {
		return Destructive
	}
	if path != "" && workspace != "" && !within(path, workspace) {
		return Outside
	}
	lowerTitle := strings.ToLower(title)
	var action Action
	for _, argv := range args {
		if len(argv) == 0 {
			continue
		}
		cmd := strings.ToLower(filepath.Base(argv[0]))
		joined := strings.ToLower(strings.Join(argv, " "))
		switch {
		case cmd == "sudo" || cmd == "mkfs" || strings.HasPrefix(cmd, "mkfs.") || cmd == "dd" || (cmd == "chmod" && hasOptionR(argv[1:])):
			return Destructive
		case cmd == "rm" && hasOptionR(argv[1:]) && hasForce(argv[1:]):
			return Destructive
		case cmd == "git" && len(argv) > 1 && strings.EqualFold(argv[1], "push"):
			if hasForce(argv[2:]) {
				return Destructive
			}
			action = GitPush
		case cmd == "git" && len(argv) > 1 && strings.EqualFold(argv[1], "commit"):
			action = GitCommit
		case cmd == "curl" || cmd == "wget" || strings.Contains(joined, "http://") || strings.Contains(joined, "https://"):
			action = Network
		case cmd == "rm" || cmd == "rmdir" || strings.Contains(lowerTitle, "delete"):
			action = Delete
		case (cmd == "npm" && hasArg(argv[1:], "install")) || (cmd == "pnpm" && hasArg(argv[1:], "add")) || (cmd == "yarn" && hasArg(argv[1:], "add")) || (cmd == "go" && hasArg(argv[1:], "get")) || (cmd == "pip" && hasArg(argv[1:], "install")) || (cmd == "cargo" && hasArg(argv[1:], "install")):
			action = Install
		case hasWord(argv, "test") || hasWord(argv, "lint") || hasWord(argv, "pytest"):
			action = Test
		case hasWord(argv, "build") || hasWord(argv, "compile"):
			action = Build
		}
	}
	if action != "" {
		return action
	}
	return Read
}

func parseCommand(command string) ([][]string, bool) {
	var commands [][]string
	var args []string
	var word strings.Builder
	quote := byte(0)
	escaped, started := false, false
	flushWord := func() {
		if started {
			args = append(args, word.String())
			word.Reset()
			started = false
		}
	}
	flushCommand := func() {
		flushWord()
		if len(args) > 0 {
			commands = append(commands, args)
			args = nil
		}
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		if escaped {
			word.WriteByte(c)
			escaped, started = false, true
			continue
		}
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			} else {
				word.WriteByte(c)
			}
			continue
		}
		if c == '\\' && quote != '\'' {
			escaped, started = true, true
			continue
		}
		if quote != 0 {
			if quote == '"' && (c == '`' || c == '$' && i+1 < len(command) && command[i+1] == '(') {
				return nil, false
			}
			if c == quote {
				quote = 0
			} else {
				word.WriteByte(c)
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote, started = c, true
			continue
		}
		if c == '`' || (c == '$' && i+1 < len(command) && command[i+1] == '(') {
			return nil, false
		}
		if c == ';' || c == '|' || c == '&' || c == '\n' {
			flushCommand()
			if (c == '|' || c == '&') && i+1 < len(command) && command[i+1] == c {
				i++
			}
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' {
			flushWord()
			continue
		}
		word.WriteByte(c)
		started = true
	}
	if escaped || quote != 0 {
		return nil, false
	}
	flushCommand()
	return commands, true
}

func hasOptionR(args []string) bool {
	for _, arg := range args {
		if arg == "--recursive" || strings.HasPrefix(arg, "-") && (strings.Contains(arg[1:], "r") || strings.Contains(arg[1:], "R")) {
			return true
		}
	}
	return false
}

func hasForce(args []string) bool {
	for _, arg := range args {
		if arg == "--force" || strings.HasPrefix(arg, "--force=") || arg == "--force-with-lease" || arg == "--hard" || arg == "-f" || strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "f") {
			return true
		}
	}
	return false
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func hasWord(args []string, want string) bool {
	for _, arg := range args {
		if strings.EqualFold(filepath.Base(arg), want) || strings.EqualFold(arg, want) {
			return true
		}
	}
	return false
}

func within(path, root string) bool {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolvedRoot, err = filepath.Abs(root)
		if err != nil {
			return false
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(resolvedRoot, path)
	}
	resolvedPath, err := resolvePath(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolvePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	probe := path
	var suffix []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(probe)
		if resolveErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", resolveErr
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}

var pipeToShell = regexp.MustCompile(`(?i)\b(?:curl|wget)\b[^;\n]*\|\s*(?:sh|bash|zsh|dash)\b`)

// MapReply maps Ask to OpenCode's "once" response; local policy prompts are unavailable here.
func MapReply(d Decision) string {
	switch d {
	case Allow:
		return "once"
	case Ask, Deny:
		return "reject"
	default:
		return "reject"
	}
}
