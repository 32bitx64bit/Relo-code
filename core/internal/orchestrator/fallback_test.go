package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/routing"
	rt "github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/taskgraph"
)

type fallbackRuntime struct {
	*dispatchRuntime
	id       rt.SessionID
	failures int
	err      error
}

func (r *fallbackRuntime) StartSession(context.Context, rt.SessionConfig) (rt.SessionID, error) {
	r.starts++
	return r.id, nil
}

func (r *fallbackRuntime) SendTask(_ context.Context, _ rt.SessionID, task rt.AgentTask) error {
	r.tasks = append(r.tasks, task)
	if r.err != nil {
		return r.err
	}
	if r.failures > 0 {
		r.failures--
		r.events <- rt.Event{Done: true, Error: "quota exhausted"}
	} else {
		r.events <- rt.Event{Done: true, JSON: `{"status":"completed","summary":"Finished."}`}
	}
	return nil
}

func (r *fallbackRuntime) ConfigureMessages(context.Context, string, rt.MessageTools) error {
	return nil
}
func (r *fallbackRuntime) DeliverMessage(context.Context, rt.SessionID, string, string) error {
	return nil
}

func TestWorkerFallbackWithinLaunchAndNextTaskUsesMain(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	main := &fallbackRuntime{dispatchRuntime: newDispatchRuntime(), id: "main", failures: 1}
	backup := &fallbackRuntime{dispatchRuntime: newDispatchRuntime(), id: "backup:1"}
	c.SetRuntime(rt.NewMux(main, rt.Route{Provider: "backup", Prefix: "backup:", Runtime: backup}))
	t.Cleanup(c.CloseMessages)
	s := c.CreateSession(t.TempDir(), "fallback", ModePlan)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Route = routing.Config{Tier1BackupModel: "backup/model"}
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	var events captured
	c.SetEmitter(events.emit)
	for _, id := range []string{"first", "second"} {
		task := c.Graph.Add(taskgraph.Task{ID: id, Title: id, Role: string(agent.Probe), Difficulty: "medium", Model: "main/model", Attempt: 1})
		a := c.spawn(task, agent.Probe)
		if err := c.execWorker(context.Background(), task, a); err != nil {
			t.Fatal(err)
		}
		got, _ := c.Graph.Get(id)
		if got.Status != taskgraph.Done || got.Attempt != 1 || got.Model != "main/model" {
			t.Fatalf("task = %+v", got)
		}
		worker, _ := c.Agents.Get(a.ID)
		if id == "first" && (worker.Model != "backup/model" || worker.FallbackFrom != "main/model" || worker.FallbackClass != "quota") {
			t.Fatalf("fallback agent = %+v", worker)
		}
		if id == "second" && (worker.Model != "main/model" || worker.FallbackFrom != "") {
			t.Fatalf("next agent = %+v", worker)
		}
	}
	if len(main.tasks) != 2 || len(backup.tasks) != 1 || main.starts != 2 || backup.starts != 1 {
		t.Fatalf("main tasks/starts=%d/%d backup tasks/starts=%d/%d", len(main.tasks), main.starts, len(backup.tasks), backup.starts)
	}
	if backup.tasks[0].Provider != "backup" || backup.tasks[0].Model != "model" || !strings.Contains(backup.tasks[0].System, "first") || !strings.Contains(backup.tasks[0].Prompt, "first") {
		t.Fatalf("backup task missing route or context pack: %+v", backup.tasks[0])
	}
	if len(events.named("agent.fallback")) != 1 {
		t.Fatalf("fallback events: %v", events.named("agent.fallback"))
	}
}

func TestWorkerFallbackGuardsAndSessionPersistence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		role      agent.Role
		backup    string
		failures  int
		wantCalls int
		wantError bool
	}{
		{"missing", agent.Worker, "", 1, 1, true},
		{"primary", agent.Primary, "backup/model", 1, 1, true},
		{"same model", agent.Worker, "main/model", 1, 1, true},
		{"backup fails once", agent.Worker, "backup/model", 2, 2, true},
		{"backup retained", agent.Worker, "backup/model", 1, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(logging.New(nil, logging.Error))
			r := &fallbackRuntime{dispatchRuntime: newDispatchRuntime(), id: "main", failures: tc.failures}
			c.SetRuntime(r)
			t.Cleanup(c.CloseMessages)
			s := c.CreateSession(t.TempDir(), "test", ModePlan)
			c.Route = routing.Config{Tier1BackupModel: tc.backup}
			a := c.Agents.Create(agent.Agent{Role: tc.role, Model: "main/model", Difficulty: "medium", Workspace: s.Workspace})
			parts := prompt.Parts{Static: "pack", Dynamic: "context"}
			_, err := c.runAgent(context.Background(), a, parts, "task", nil)
			if (err != nil) != tc.wantError || len(r.tasks) != tc.wantCalls {
				t.Fatalf("err=%v calls=%d", err, len(r.tasks))
			}
			if tc.name == "backup retained" {
				if _, err = c.runAgent(context.Background(), a, parts, "next turn", nil); err != nil || r.tasks[2].Provider != "backup" || r.starts != 2 {
					t.Fatalf("backup turn err=%v tasks=%+v starts=%d", err, r.tasks, r.starts)
				}
				r.failures = 1
				if _, err = c.runAgent(context.Background(), a, parts, "another turn", nil); err == nil || len(r.tasks) != 4 {
					t.Fatalf("backup failure err=%v calls=%d", err, len(r.tasks))
				}
			}
			c.closeAgentSession(a.ID)
		})
	}
	var old agent.Agent
	if err := json.Unmarshal([]byte(`{"id":"old","model":"main/model"}`), &old); err != nil || old.FallbackFrom != "" || old.FallbackClass != "" {
		t.Fatalf("legacy agent=%+v err=%v", old, err)
	}
}

func TestBackupSettingsReachBackupForThroughUpdateSettings(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.UpdateSettings(func(s *Settings) {
		s.Tier1BackupModel, s.PrimaryBackupModel = "backup/t1", "backup/p"
	})
	if got := c.routing().BackupFor(routing.Hard, "main/x", "main/x"); got != "backup/t1" {
		t.Fatalf("simple hard backup = %q, want backup/t1", got)
	}
	if got := c.routing().BackupFor(routing.Expert, "main/x", "main/x"); got != "backup/p" {
		t.Fatalf("simple expert backup = %q, want backup/p", got)
	}
	c.UpdateSettings(func(s *Settings) {
		s.RoutingMode = routing.ModeAdvanced
		s.BackupByDifficulty = map[string]string{routing.Hard: "backup/adv"}
	})
	if got := c.routing().BackupFor(routing.Hard, "main/x", "main/x"); got != "backup/adv" {
		t.Fatalf("advanced hard backup = %q, want backup/adv", got)
	}
	if got := c.routing().BackupFor(routing.Expert, "main/x", "main/x"); got != "backup/p" {
		t.Fatalf("advanced expert backup = %q, want backup/p", got)
	}
}

func TestSettingsWithoutBackupKeysLoadUnchanged(t *testing.T) {
	var s Settings
	if err := json.Unmarshal([]byte(`{"primaryModel":"main/x","tier1Model":"main/y"}`), &s); err != nil {
		t.Fatal(err)
	}
	if got := s.RouteConfig().BackupFor(routing.Hard, "main/y", "main/y"); got != "" || s.BackupByDifficulty != nil {
		t.Fatalf("legacy settings gained backup %q / %v", got, s.BackupByDifficulty)
	}
	out, err := json.Marshal(DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "Backup") || strings.Contains(string(out), "backupByDifficulty") {
		t.Fatalf("default settings serialize backup keys: %s", out)
	}
}

func TestWorkerFallbackRefusesBackupWithUnregisteredProvider(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	main := &fallbackRuntime{dispatchRuntime: newDispatchRuntime(), id: "main", failures: 1}
	backup := &fallbackRuntime{dispatchRuntime: newDispatchRuntime(), id: "backup:1"}
	c.SetRuntime(rt.NewMux(main, rt.Route{Provider: "backup", Prefix: "backup:", Runtime: backup}))
	t.Cleanup(c.CloseMessages)
	s := c.CreateSession(t.TempDir(), "test", ModePlan)
	c.Route = routing.Config{Tier1BackupModel: "ghost/model"}
	a := c.Agents.Create(agent.Agent{Role: agent.Worker, Model: "main/model", Difficulty: "medium", Workspace: s.Workspace})
	parts := prompt.Parts{Static: "pack", Dynamic: "context"}
	if _, err := c.runAgent(context.Background(), a, parts, "task", nil); err == nil {
		t.Fatal("want the main failure returned, not a silent switch")
	}
	if len(main.tasks) != 1 || len(backup.tasks) != 0 || backup.starts != 0 {
		t.Fatalf("main calls=%d backup calls=%d starts=%d", len(main.tasks), len(backup.tasks), backup.starts)
	}
	got, _ := c.Agents.Get(a.ID)
	if got.Model != "main/model" || got.FallbackFrom != "" {
		t.Fatalf("agent relabelled without a switch: %+v", got)
	}
}

func TestWorkerFallbackMissingUsesFailTaskAndStallDoesNotSwitch(t *testing.T) {
	for _, stall := range []bool{false, true} {
		c := New(logging.New(nil, logging.Error))
		r := &fallbackRuntime{dispatchRuntime: newDispatchRuntime(), id: "main", failures: 1}
		if stall {
			r.err = &stalledError{msg: "quota exhausted"}
			c.Route = routing.Config{Tier1BackupModel: "backup/model"}
		}
		c.SetRuntime(r)
		c.CreateSession(t.TempDir(), "test", ModePlan)
		c.Settings.AutoVerify, c.Settings.AutoReview = false, false
		task := c.Graph.Add(taskgraph.Task{ID: "failed", Title: "failed", Role: string(agent.Probe), Difficulty: "medium", Model: "main/model", Attempt: 1, MaxAttempts: 1})
		a := c.spawn(task, agent.Probe)
		err := c.execWorker(context.Background(), task, a)
		got, _ := c.Graph.Get(task.ID)
		worker, _ := c.Agents.Get(a.ID)
		if err == nil || got.Status != taskgraph.Failed || got.Attempt != 1 || worker.FallbackFrom != "" || len(r.tasks) != 1 {
			t.Fatalf("stall=%v err=%v task=%+v agent=%+v calls=%d", stall, err, got, worker, len(r.tasks))
		}
		c.CloseMessages()
	}
}
