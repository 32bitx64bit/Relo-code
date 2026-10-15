package app

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/orchestrator"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/taskgraph"
)

func newMockApp(t *testing.T) (*App, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	logs := &bytes.Buffer{}
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(dir, "relo.db"), Runtime: "mock"}, logging.New(logs, logging.Debug))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	return core, logs
}

func TestSettingsApplyReadsRoutingContextAndEfforts(t *testing.T) {
	core, _ := newMockApp(t)
	core.Coord.UpdateSettings(func(s *orchestrator.Settings) {
		s.Models = map[string]runtime.Model{"p/big": {ID: "big", Provider: "p", Context: 1_000_000, Efforts: []string{"high"}}}
	})
	got, err := core.Handle("settings.apply", json.RawMessage(`{"routingMode":"advanced","contextLimit":350000,"modelEfforts":{"p/big":"high"},"byDifficulty":{"hard":"p/z"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if m := got.(map[string]any); m["applied"] != true {
		t.Fatalf("result = %v", m)
	}
	s := core.Coord.SettingsSnapshot()
	if s.RoutingMode != routing.ModeAdvanced || s.ContextLimit != 350000 || s.ModelEfforts["p/big"] != "high" {
		t.Fatalf("settings = %+v", s)
	}
	if core.Coord.Route.Mode != routing.ModeAdvanced {
		t.Fatalf("route mode = %q", core.Coord.Route.Mode)
	}
	if got := s.Window("p/big"); got != 350000 {
		t.Fatalf("window = %d", got)
	}
	if _, err := core.Handle("settings.apply", json.RawMessage(`{"routingMode":"bogus"}`)); err != nil {
		t.Fatal(err)
	}
	if core.Coord.SettingsSnapshot().RoutingMode != routing.ModeSimple {
		t.Fatal("unknown routing mode must normalize to simple")
	}
	if _, err := core.Handle("settings.apply", json.RawMessage(`{"contextLimit":0}`)); err != nil || core.Coord.SettingsSnapshot().ContextLimit != 0 {
		t.Fatalf("contextLimit 0 must clear the cap: %v %d", err, core.Coord.SettingsSnapshot().ContextLimit)
	}
}

func TestProvidersValidateAndKeysNeverLeak(t *testing.T) {
	core, logs := newMockApp(t)
	payload := `{"providers":[{"id":"acme","kind":"openai","baseURL":"https://api.acme.test/v1","models":[{"id":"m","context":64000}]}],"providerKeys":{"acme":"sk-very-secret"}}`
	if _, err := core.Handle("settings.apply", json.RawMessage(payload)); err != nil {
		t.Fatal(err)
	}
	if !core.providersChanged() {
		t.Fatal("new providers must differ from the running (empty) config")
	}
	env, err := core.providerEnv()
	if err != nil || !strings.Contains(strings.Join(env, "\n"), "RELO_KEY_ACME=sk-very-secret") {
		t.Fatalf("env = %v %v", env, err)
	}
	st, _ := json.Marshal(core.backendStatus())
	snap, _ := json.Marshal(core.Coord.Snapshot())
	settings, _ := json.Marshal(core.Coord.SettingsSnapshot())
	for name, blob := range map[string]string{"status": string(st), "snapshot": string(snap), "settings": string(settings), "logs": logs.String()} {
		if strings.Contains(blob, "sk-very-secret") {
			t.Fatalf("API key leaked into %s", name)
		}
	}
	// An invalid provider list is dropped whole, not half-applied.
	bad := `{"providers":[{"id":"Bad Id","kind":"openai","baseURL":"https://x.test"}]}`
	if _, err := core.Handle("settings.apply", json.RawMessage(bad)); err != nil {
		t.Fatal(err)
	}
	core.provMu.Lock()
	n := len(core.providers)
	core.provMu.Unlock()
	if n != 0 {
		t.Fatalf("invalid providers kept: %d", n)
	}
}

func TestSettingsApplyDefersRestartWhileBusy(t *testing.T) {
	core, _ := newMockApp(t)
	core.Coord.Agents.Create(agent.Agent{Role: agent.Primary, Title: "Primary", Status: agent.Running})
	payload := `{"providers":[{"id":"acme","kind":"openai","baseURL":"https://api.acme.test/v1"}]}`
	got, err := core.Handle("settings.apply", json.RawMessage(payload))
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if m["restartPending"] != true || m["restarted"] != false {
		t.Fatalf("busy apply must defer the restart: %v", m)
	}
	if _, err := core.Handle("backend.restart", json.RawMessage(`{}`)); err == nil {
		t.Fatal("backend.restart while busy must refuse, not orphan running agents")
	}
}

func TestProvidersDiscoverReturnsErrorInPayload(t *testing.T) {
	core, _ := newMockApp(t)
	got, err := core.Handle("providers.discover", json.RawMessage(`{"kind":"gemini","baseURL":"ftp://nope"}`))
	if err != nil {
		t.Fatalf("a bad endpoint is a payload error, not an RPC failure: %v", err)
	}
	m := got.(map[string]any)
	if m["error"] == "" || m["error"] == nil {
		t.Fatalf("want error text, got %v", m)
	}
	if models, ok := m["models"].([]any); ok && len(models) != 0 {
		t.Fatalf("models = %v", models)
	}
}

func TestBackendStatusAndSnapshotCarryRouting(t *testing.T) {
	core, _ := newMockApp(t)
	if _, err := core.Handle("settings.apply", json.RawMessage(`{"routingMode":"advanced","contextLimit":200000}`)); err != nil {
		t.Fatal(err)
	}
	st := core.backendStatus()
	if st["routingMode"] != "advanced" || st["contextLimit"] != 200000 {
		t.Fatalf("status = %v", st)
	}
	r := core.Coord.Snapshot()["routing"].(map[string]any)
	if r["mode"] != "advanced" || r["contextLimit"] != 200000 {
		t.Fatalf("snapshot routing = %v", r)
	}
}

// A provider or model edit made while agents run is stored, not applied. Nothing applied it afterwards, so a model
// added mid-run stayed out of the pickers until the window was reloaded.
func TestADeferredRestartIsAppliedOnceNothingIsRunning(t *testing.T) {
	core, _ := newMockApp(t)
	primary := core.Coord.Agents.Create(agent.Agent{Role: agent.Primary, Title: "Primary", Status: agent.Running})
	payload := `{"providers":[{"id":"acme","kind":"openai","baseURL":"https://api.acme.test/v1","models":[{"id":"new-model"}]}]}`
	got, err := core.Handle("settings.apply", json.RawMessage(payload))
	if err != nil {
		t.Fatal(err)
	}
	if got.(map[string]any)["restartPending"] != true {
		t.Fatalf("the edit was not deferred: %v", got)
	}

	core.applyPendingRestart()
	if !core.isRestartPending() {
		t.Fatal("restarted while an agent was running")
	}

	core.Coord.Agents.Update(primary.ID, func(a *agent.Agent) { a.Status = agent.Completed })
	task := core.Coord.Graph.Add(taskgraph.Task{Title: "next", Role: string(agent.Worker)})
	core.applyPendingRestart()
	if !core.isRestartPending() {
		t.Fatal("restarted between one task finishing and the next starting, which would cut it off")
	}

	if _, err := core.Coord.Graph.Update(task.ID, func(x *taskgraph.Task) { x.Status = taskgraph.Done }); err != nil {
		t.Fatal(err)
	}
	core.applyPendingRestart()
	if core.isRestartPending() {
		t.Fatal("the deferred restart was never applied")
	}
}
