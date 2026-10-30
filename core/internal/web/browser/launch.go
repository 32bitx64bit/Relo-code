package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/web/cdp"
)

const (
	launchTimeout = 30 * time.Second
	shutdownGrace = 5 * time.Second
	stderrKeep    = 8 << 10
)

// Options configures Launch.
type Options struct {
	Lane Lane
	// StateDir is the Relo state directory. The profile is created under
	// StateDir/web/profiles/<lane>.
	StateDir string
	// BrowserPath overrides browser discovery when non-empty.
	BrowserPath string
	// Proxy is passed as --proxy-server when non-empty.
	Proxy string
	// ExtraArgs are appended after the built-in flags. Used for measured experiments.
	ExtraArgs []string
	// SoftwareGL drops the measured ANGLE flags, for hosts where they do not
	// start. The WebGL renderer then falls back to the software rasteriser.
	SoftwareGL bool
}

// Process is a running browser controlled over the CDP pipe.
type Process struct {
	Conn       *cdp.Conn
	Lane       Lane
	Browser    Installed
	ProfileDir string
	// Version is the product string from Browser.getVersion, e.g. "HeadlessChrome/154...".
	Version string

	cmd       *exec.Cmd
	exited    chan struct{}
	waitErr   error
	stderr    *tailBuffer
	closeOnce sync.Once
	closeErr  error
}

// Launch starts the browser for opts.Lane and verifies the pipe with Browser.getVersion.
// The process is killed if verification fails.
func Launch(ctx context.Context, opts Options) (*Process, error) {
	if opts.Lane != LaneHidden && opts.Lane != LaneWindow {
		return nil, fmt.Errorf("browser: unknown lane %q", opts.Lane)
	}
	if opts.StateDir == "" {
		return nil, errors.New("browser: StateDir is required for the agent profile")
	}
	inst, err := Find(opts.BrowserPath)
	if err != nil {
		return nil, err
	}
	profileDir := filepath.Join(opts.StateDir, "web", "profiles", string(opts.Lane))
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return nil, fmt.Errorf("browser: create profile: %w", err)
	}

	args := append(buildArgs(opts.Lane, profileDir, opts.Proxy, opts.SoftwareGL), opts.ExtraArgs...)
	p, err := start(inst, opts.Lane, profileDir, args)
	if err != nil {
		return nil, err
	}

	vctx, cancel := context.WithTimeout(ctx, launchTimeout)
	defer cancel()
	var ver struct {
		Product string `json:"product"`
	}
	if err := p.Conn.Call(vctx, "", "Browser.getVersion", nil, &ver); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("browser: %s did not answer over the pipe: %w (stderr: %s)", inst.Family, err, p.stderr.String())
	}
	p.Version = ver.Product
	return p, nil
}

// start runs inst with args over the CDP pipe and returns the running Process.
// The browser is not verified here: the caller checks Browser.getVersion and
// must Close the Process.
func start(inst Installed, lane Lane, profileDir string, args []string) (*Process, error) {
	// fd 3: parent writes commands, child reads. fd 4: child writes, parent reads.
	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("browser: command pipe: %w", err)
	}
	respR, respW, err := os.Pipe()
	if err != nil {
		closeAll(cmdR, cmdW)
		return nil, fmt.Errorf("browser: response pipe: %w", err)
	}

	stderr := &tailBuffer{max: stderrKeep}
	cmd := exec.Command(inst.Path, args...)
	// A Chromium helper can outlive the browser process and hold the inherited
	// stderr open; without this bound cmd.Wait would block on it forever.
	cmd.WaitDelay = shutdownGrace
	cmd.ExtraFiles = []*os.File{cmdR, respW}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		closeAll(cmdR, cmdW, respR, respW)
		return nil, fmt.Errorf("browser: start %s: %w", inst.Path, err)
	}
	// The child owns its copies now; keep only the parent ends.
	_ = cmdR.Close()
	_ = respW.Close()

	p := &Process{
		Conn:       cdp.NewConn(cmdW, respR),
		Lane:       lane,
		Browser:    inst,
		ProfileDir: profileDir,
		cmd:        cmd,
		exited:     make(chan struct{}),
		stderr:     stderr,
	}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// Close shuts the browser down: closing the pipe asks Chromium to exit, and the
// process is killed if it has not exited after a short grace period. Every call
// blocks until the process is gone and returns the error it exited with, so a
// browser that died is never reported as a clean shutdown.
func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		p.closeErr = p.Conn.Close()
		select {
		case <-p.exited:
		case <-time.After(shutdownGrace):
			_ = p.cmd.Process.Kill()
			<-p.exited
		}
	})
	// Callers that arrive while another Close is running wait here as well: the
	// profile is still locked until the process is reaped.
	<-p.exited
	if p.closeErr != nil {
		return p.closeErr
	}
	return p.waitErr
}

func closeAll(fs ...*os.File) {
	for _, f := range fs {
		_ = f.Close()
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append([]byte(nil), t.buf[over:]...)
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

const identityPage = "chrome://version/"

// uaProbeJS reads the page's own identity. It only reads; nothing is patched.
const uaProbeJS = `(async () => {
  const o = { ua: navigator.userAgent };
  const u = navigator.userAgentData;
  if (u) {
    o.brands = u.brands;
    o.mobile = u.mobile;
    Object.assign(o, await u.getHighEntropyValues(['platform','platformVersion','architecture','bitness','model','fullVersionList','wow64']));
  }
  return JSON.stringify(o);
})()`

type uaProbe struct {
	UA              string            `json:"ua"`
	Brands          []json.RawMessage `json:"brands"`
	FullVersionList []json.RawMessage `json:"fullVersionList"`
	Mobile          bool              `json:"mobile"`
	Platform        string            `json:"platform"`
	PlatformVersion string            `json:"platformVersion"`
	Architecture    string            `json:"architecture"`
	Bitness         string            `json:"bitness"`
	Model           string            `json:"model"`
	Wow64           bool              `json:"wow64"`
}

// SetUserAgent makes the session's User-Agent header, navigator.userAgent and
// User-Agent Client Hints agree, and drops the HeadlessChrome token. It is chosen
// over the --user-agent flag: the flag leaves high-entropy hints empty (measured
// uaFullVersion ""), while Emulation.setUserAgentOverride with metadata keeps them
// populated. Call it on a session before navigating.
func SetUserAgent(ctx context.Context, s *cdp.Session) error {
	// about:blank is not a secure context and has no userAgentData. chrome://version
	// is local to the browser (no network) and exposes the full hints.
	if err := s.Call(ctx, "Page.enable", nil, nil); err != nil {
		return fmt.Errorf("browser: enable page: %w", err)
	}
	sub := s.Subscribe()
	defer sub.Unsubscribe()
	if err := s.Call(ctx, "Page.navigate", map[string]any{"url": identityPage}, nil); err != nil {
		return fmt.Errorf("browser: open identity page: %w", err)
	}
	if _, err := cdp.WaitEvent(ctx, sub, "Page.loadEventFired"); err != nil {
		return fmt.Errorf("browser: wait identity page: %w", err)
	}
	raw, err := s.Evaluate(ctx, uaProbeJS)
	if err != nil {
		return fmt.Errorf("browser: read identity: %w", err)
	}
	var pr uaProbe
	if err := json.Unmarshal([]byte(raw), &pr); err != nil {
		return fmt.Errorf("browser: decode identity: %w", err)
	}
	if pr.UA == "" {
		return errors.New("browser: empty navigator.userAgent")
	}
	meta := map[string]any{
		"brands":          pr.Brands,
		"fullVersionList": pr.FullVersionList,
		"platform":        pr.Platform,
		"platformVersion": pr.PlatformVersion,
		"architecture":    pr.Architecture,
		"bitness":         pr.Bitness,
		"model":           pr.Model,
		"mobile":          pr.Mobile,
		"wow64":           pr.Wow64,
	}
	params := map[string]any{
		"userAgent":         strings.Replace(pr.UA, "HeadlessChrome/", "Chrome/", 1),
		"userAgentMetadata": meta,
	}
	if err := s.Call(ctx, "Emulation.setUserAgentOverride", params, nil); err != nil {
		return fmt.Errorf("browser: set user agent: %w", err)
	}
	return nil
}
