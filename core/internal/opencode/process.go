package opencode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/relocode/relo/internal/runtime"
)

type Process struct {
	Path   string
	Dir    string
	cmd    *exec.Cmd
	done   chan struct{}
	client *Client
	url    string
}

func Look(path string) (string, error) {
	if path != "" {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path, nil
		}
		if p, err := exec.LookPath(path); err == nil {
			return p, nil
		}
	}
	for _, name := range []string{"opencode", "relo"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	home, _ := os.UserHomeDir()
	guess := []string{
		filepath.Join(home, ".opencode", "bin", "opencode"),
		filepath.Join(home, ".local", "bin", "opencode"),
		"/usr/local/bin/opencode",
	}
	for _, g := range guess {
		if st, err := os.Stat(g); err == nil && !st.IsDir() {
			return g, nil
		}
	}
	return "", fmt.Errorf("OpenCode is not installed. Install it from https://opencode.ai/docs and set relo.openCode.path if it is not on PATH")
}

// Start launches `opencode serve`. extraEnv (KEY=VALUE) is appended after the inherited environment, so it wins
// on duplicates; it carries provider config and API keys and must never be logged.
// Isolation returns the environment that keeps a Relo-owned OpenCode apart from the user's own install.
// root is Relo's private directory: OpenCode's config, data (sessions, auth, logs), cache and state all live
// under it, so Relo never reads or writes ~/.config/opencode, ~/.local/share/opencode and friends, and an
// opencode.json in the workspace cannot add providers (Relo's providers arrive only through
// OPENCODE_CONFIG_CONTENT). Returns nil when root is empty (isolation off).
func Isolation(root string) []string {
	if root == "" {
		return nil
	}
	return []string{
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"XDG_DATA_HOME=" + filepath.Join(root, "data"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"XDG_STATE_HOME=" + filepath.Join(root, "state"),
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_AUTOUPDATE=1",
	}
}

// childEnv is the inherited environment minus anything extraEnv overrides, plus extraEnv.
func childEnv(inherited, extraEnv []string) []string {
	overridden := map[string]bool{"OPENCODE_SERVER_PASSWORD": true}
	for _, env := range extraEnv {
		if k, _, ok := strings.Cut(env, "="); ok {
			overridden[k] = true
			if k == "XDG_CONFIG_HOME" { // isolated: the user's pointers at their own config must not leak in
				overridden["OPENCODE_CONFIG"] = true
				overridden["OPENCODE_CONFIG_DIR"] = true
			}
		}
	}
	out := []string{}
	for _, env := range inherited {
		k, _, _ := strings.Cut(env, "=")
		if !overridden[k] {
			out = append(out, env)
		}
	}
	return append(out, extraEnv...)
}

func Start(ctx context.Context, bin, dir string, extraEnv []string) (*Process, error) {
	var p *Process
	for attempt := 0; attempt < 5; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		addr := fmt.Sprintf("http://127.0.0.1:%d", port)
		cmd := exec.CommandContext(ctx, bin, "serve", "--hostname", "127.0.0.1", "--port", fmt.Sprintf("%d", port))
		cmd.Dir = dir
		cmd.SysProcAttr = childAttr()
		cmd.Env = childEnv(os.Environ(), extraEnv)
		cmd.Env = append(cmd.Env, "OPENCODE_SERVER_PASSWORD="+hex.EncodeToString(secret))
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("failed to start OpenCode: %w", err)
		}
		p = &Process{Path: bin, Dir: dir, cmd: cmd, done: make(chan struct{}), url: addr, client: NewClientWithAuth(addr, serverUser, hex.EncodeToString(secret))}
		process := p
		go func() {
			_ = cmd.Wait()
			close(process.done)
		}()
		go func() {
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				_ = sc.Text()
			}
		}()
		deadline := time.NewTimer(20 * time.Second)
		ticker := time.NewTicker(200 * time.Millisecond)
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				deadline.Stop()
				_ = p.Stop()
				return nil, ctx.Err()
			case <-p.done:
				ticker.Stop()
				deadline.Stop()
				if attempt == 4 {
					return nil, fmt.Errorf("OpenCode exited before listening on %s after 5 port attempts", addr)
				}
				p = nil
				goto retry
			case <-deadline.C:
				ticker.Stop()
				_ = p.Stop()
				return nil, fmt.Errorf("OpenCode did not become healthy on %s", addr)
			case <-ticker.C:
				if p.client.Health(ctx).Healthy {
					ticker.Stop()
					deadline.Stop()
					return p, nil
				}
			}
		}
	retry:
	}
	return nil, fmt.Errorf("OpenCode failed to start")
}

func (p *Process) Client() *Client { return p.client }
func (p *Process) URL() string     { return p.url }

func (p *Process) Stop() error {
	if p.client != nil {
		_ = p.client.Close()
	}
	if p.cmd == nil || p.cmd.Process == nil || p.done == nil {
		return nil
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
		return nil
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		return nil
	}
}

func (p *Process) Runtime() runtime.AgentRuntime {
	return &Adapter{Client: p.client}
}

type Adapter struct {
	Client *Client
}

func (a *Adapter) StartSession(ctx context.Context, cfg runtime.SessionConfig) (runtime.SessionID, error) {
	return a.Client.StartSession(ctx, cfg)
}
func (a *Adapter) SendTask(ctx context.Context, session runtime.SessionID, task runtime.AgentTask) error {
	return a.Client.SendTask(ctx, session, task)
}
func (a *Adapter) Events(ctx context.Context, session runtime.SessionID) (<-chan runtime.Event, error) {
	return a.Client.Events(ctx, session)
}
func (a *Adapter) Cancel(ctx context.Context, session runtime.SessionID) error {
	return a.Client.Cancel(ctx, session)
}
func (a *Adapter) CloseSession(ctx context.Context, session runtime.SessionID) error {
	return a.Client.CloseSession(ctx, session)
}
func (a *Adapter) Models(ctx context.Context) ([]runtime.Model, error) {
	return a.Client.Models(ctx)
}
func (a *Adapter) Providers(ctx context.Context) ([]runtime.Provider, error) {
	return a.Client.Providers(ctx)
}
func (a *Adapter) Health(ctx context.Context) runtime.Health {
	return a.Client.Health(ctx)
}
func (a *Adapter) Close() error { return a.Client.Close() }
