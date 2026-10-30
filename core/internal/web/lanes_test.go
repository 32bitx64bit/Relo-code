package web

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/relocode/relo/internal/web/browser"
)

// stubLauncher replaces launchFunc for one test and records every attempt.
type stubLauncher struct {
	t    *testing.T
	mu   sync.Mutex
	logs []string
	// calls records the options of every launch attempt.
	calls []browser.Options
	// angleFails fails only attempts without the software GL opt-out.
	angleFails bool
	// failAlways fails every attempt, including the software GL retry.
	failAlways bool
	canceled   bool
}

func (s *stubLauncher) recorded() []browser.Options {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]browser.Options(nil), s.calls...)
}

func (s *stubLauncher) reset() {
	s.mu.Lock()
	s.calls = nil
	s.mu.Unlock()
}

func (s *stubLauncher) log(msg string) {
	s.mu.Lock()
	s.logs = append(s.logs, msg)
	s.mu.Unlock()
}

func (s *stubLauncher) joinedLogs() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.logs, "\n")
}

func (s *stubLauncher) hooks() Hooks { return Hooks{Log: s.log} }

// stubLaunch replaces the package launch function and restores it afterwards.
func stubLaunch(t *testing.T, stub *stubLauncher) *stubLauncher {
	t.Helper()
	stub.t = t
	previous := launchFunc
	launchFunc = func(_ context.Context, opts browser.Options) (*browser.Process, error) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		stub.calls = append(stub.calls, opts)
		if stub.canceled {
			return nil, context.Canceled
		}
		if stub.failAlways || (stub.angleFails && !opts.SoftwareGL) {
			return nil, errors.New("browser: chromium did not answer over the pipe: EOF")
		}
		// A stub process has no pipe, but only the options are under test here.
		return fakeProcess(), nil
	}
	t.Cleanup(func() { launchFunc = previous })
	return stub
}

// noCloseLane stands in for a launched lane in tests that never open a tab.
type noCloseLane struct{}

func (*noCloseLane) Open(context.Context) (browserTab, error) {
	return nil, errors.New("noCloseLane: no tab in this test")
}
func (*noCloseLane) Close() error { return nil }

func TestLaunchLaneRetriesOnceWithSoftwareGL(t *testing.T) {
	stub := stubLaunch(t, &stubLauncher{angleFails: true})
	process, software, err := launchLaneProcess(context.Background(), TierHidden,
		Settings{StateDir: t.TempDir()}, "http://127.0.0.1:1", stub.hooks(), false)
	if err != nil || process == nil {
		t.Fatalf("launch = %v, %v", process, err)
	}
	if !software {
		t.Fatal("software = false; want true after the software GL retry")
	}
	calls := stub.recorded()
	if len(calls) != 2 {
		t.Fatalf("launch calls = %d, want 2", len(calls))
	}
	if calls[0].SoftwareGL || !calls[1].SoftwareGL {
		t.Fatalf("SoftwareGL flags = %t, %t; want false then true", calls[0].SoftwareGL, calls[1].SoftwareGL)
	}
	if calls[1].Lane != browser.LaneHidden || calls[1].StateDir != calls[0].StateDir {
		t.Fatalf("retry options = %+v", calls[1])
	}
	logged := stub.joinedLogs()
	if !strings.Contains(logged, "ANGLE launch failed") || !strings.Contains(logged, "software GL") {
		t.Fatalf("logs = %q; want the ANGLE failure and the software GL choice", logged)
	}
}

func TestLaunchLaneSoftwareGLRetriesOnlyOnce(t *testing.T) {
	stub := stubLaunch(t, &stubLauncher{failAlways: true})
	_, _, err := launchLaneProcess(context.Background(), TierHidden, Settings{StateDir: t.TempDir()}, "", Hooks{}, false)
	if err == nil {
		t.Fatal("launch succeeded, want an error")
	}
	if calls := stub.recorded(); len(calls) != 2 {
		t.Fatalf("launch calls = %d, want exactly 2", len(calls))
	}
}

func TestLaunchLaneSoftwareGLFailsWithoutRetry(t *testing.T) {
	stub := stubLaunch(t, &stubLauncher{failAlways: true})
	_, _, err := launchLaneProcess(context.Background(), TierWindow, Settings{StateDir: t.TempDir()}, "", Hooks{}, true)
	if err == nil {
		t.Fatal("launch succeeded, want an error")
	}
	calls := stub.recorded()
	if len(calls) != 1 || !calls[0].SoftwareGL || calls[0].Lane != browser.LaneWindow {
		t.Fatalf("calls = %+v; want one software GL window attempt", calls)
	}
}

func TestLaunchLaneCanceledDoesNotRetry(t *testing.T) {
	stub := stubLaunch(t, &stubLauncher{canceled: true})
	_, _, err := launchLaneProcess(context.Background(), TierHidden, Settings{StateDir: t.TempDir()}, "", Hooks{}, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("launch = %v, want context.Canceled", err)
	}
	if calls := stub.recorded(); len(calls) != 1 {
		t.Fatalf("launch calls = %d, want 1", len(calls))
	}
}

// TestEngineRemembersSoftwareGL checks that a failing launch is retried once,
// the choice is logged, later launches skip the ANGLE attempt, and Status says
// so. The lane factory follows the engine's own launch decision but returns a
// no-op lane, so the stub process is never closed.
func TestEngineRemembersSoftwareGL(t *testing.T) {
	stub := stubLaunch(t, &stubLauncher{angleFails: true})
	settings := Settings{Mode: ModeHidden, StateDir: t.TempDir(),
		RateLimitPerAgent: 1000000000, RateLimitGlobal: 1000000000}
	engine, err := New(settings, stub.hooks())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.pacer.interval = 0
	engine.urlCheck = func(Settings, string) error { return nil }
	engine.laneFactory = func(ctx context.Context, tier Tier, settings Settings, proxy string) (browserLane, error) {
		if _, err := launchBrowserLaneWithRetry(ctx, tier, settings, proxy, engine.hooks,
			engine.softwareGLFor(tier), engine.rememberSoftwareGL); err != nil {
			return nil, err
		}
		return &noCloseLane{}, nil
	}

	tier := TierHidden
	first, err := engine.getLane(context.Background(), tier, engine.settings)
	if err != nil {
		t.Fatalf("first getLane: %v", err)
	}
	if calls := stub.recorded(); len(calls) != 2 {
		t.Fatalf("launch calls = %d, want 2", len(calls))
	}

	// Drop the slot so the next call launches again, as after a process crash.
	engine.mu.Lock()
	delete(engine.lanes, tier)
	engine.mu.Unlock()
	_ = first.Close()
	stub.reset()

	second, err := engine.getLane(context.Background(), tier, engine.settings)
	if err != nil {
		t.Fatalf("second getLane: %v", err)
	}
	defer second.Close()
	if calls := stub.recorded(); len(calls) != 1 || !calls[0].SoftwareGL {
		t.Fatalf("second launch calls = %+v; want one software GL attempt", calls)
	}

	status, err := engine.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(status.Notes, " ")
	if !strings.Contains(notes, string(tier)) || !strings.Contains(notes, "software rendering") {
		t.Fatalf("status notes = %q; want software rendering for the hidden lane", status.Notes)
	}
	result, err := engine.Do(context.Background(), "agent", Request{Action: ActionStatus})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "software rendering") {
		t.Fatalf("status content = %q", result.Content)
	}
	if !strings.Contains(stub.joinedLogs(), "software GL") {
		t.Fatalf("logs = %q; want the software GL choice", stub.joinedLogs())
	}
}

// fakeProcess is what the launch stub returns: an unstarted Process, because
// only the launch decision is under test. The tests avoid closing it.
func fakeProcess() *browser.Process { return new(browser.Process) }
