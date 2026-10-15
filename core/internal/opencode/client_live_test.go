package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/runtime"
)

func TestLiveOpenCodeV2(t *testing.T) {
	if os.Getenv("RELO_LIVE_OPENCODE") != "1" {
		t.Skip("set RELO_LIVE_OPENCODE=1 to run live OpenCode smoke test")
	}
	bin, err := Look(os.Getenv("OPENCODE_BIN"))
	if err != nil {
		t.Fatal(err)
	}
	root := "/tmp/relo-live"
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "session-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	password := fmt.Sprintf("%x", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "serve", "--hostname", "127.0.0.1", "--port", fmt.Sprint(port))
	cmd.Dir = dir
	cmd.Env = []string{}
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "OPENCODE_SERVER_PASSWORD=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "OPENCODE_SERVER_PASSWORD="+password)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
		}
		_ = cmd.Wait()
	}()
	c := NewClientWithAuth(fmt.Sprintf("http://127.0.0.1:%d", port), "opencode", password)
	defer c.Close()
	ready := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if c.Health(ctx).Healthy {
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		t.Fatal("OpenCode v2 did not become healthy")
	}
	models, err := c.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("listed %d enabled models", len(models))
	var defaultModel struct {
		Data struct {
			ID         string `json:"id"`
			ModelID    string `json:"modelID"`
			ProviderID string `json:"providerID"`
		} `json:"data"`
	}
	defaultErr := c.get(ctx, "/api/model/default", &defaultModel)
	if defaultErr != nil {
		t.Logf("default model lookup failed: %v", defaultErr)
	}
	var integrations struct {
		Data []struct {
			ID          string `json:"id"`
			Connections []any  `json:"connections"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/integration", &integrations); err != nil {
		t.Fatal(err)
	}
	connectedProviders := map[string]bool{}
	for _, integration := range integrations.Data {
		if len(integration.Connections) > 0 {
			connectedProviders[integration.ID] = true
		}
	}
	var connected runtime.Model
	if defaultErr == nil {
		modelID := defaultModel.Data.ModelID
		if modelID == "" {
			modelID = defaultModel.Data.ID
		}
		if modelID != "" && defaultModel.Data.ProviderID != "" {
			connected = runtime.Model{ID: modelID, Provider: defaultModel.Data.ProviderID, Name: "default"}
			t.Logf("default model=%s/%s", connected.Provider, connected.ID)
		}
	}
	for _, model := range models {
		if connected.ID != "" {
			break
		}
		if connectedProviders[model.Provider] {
			var payload struct {
				Data []struct {
					ModelID    string `json:"modelID"`
					ProviderID string `json:"providerID"`
				} `json:"data"`
			}
			if err := c.get(ctx, "/api/model", &payload); err != nil {
				t.Fatal(err)
			}
			for _, info := range payload.Data {
				if info.ProviderID == model.Provider && info.ModelID == model.ID {
					connected = runtime.Model{ID: info.ModelID, Provider: info.ProviderID, Name: model.Name}
					break
				}
			}
			if connected.ID != "" {
				break
			}
		}
	}
	id, err := c.StartSession(ctx, runtime.SessionConfig{Title: "Relo v2 smoke", Directory: filepath.Clean(dir), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseSession(context.Background(), id) }()
	for _, path := range []string{"/api/session/" + string(id) + "/context", "/api/experimental/session/" + string(id) + "/instructions/entries"} {
		var probe json.RawMessage
		if err := c.get(ctx, path, &probe); err != nil {
			t.Logf("overhead probe %s unavailable: %v", path, err)
		} else {
			t.Logf("overhead probe %s bytes=%d approximateTokens=%d payload=%s", path, len(probe), len(probe)/4, probe)
		}
	}
	if connected.ID == "" {
		t.Log("no usable model found; verified health, model list, session creation; prompt skipped")
		return
	}
	ctxLive, cancelLive := context.WithTimeout(ctx, 45*time.Second)
	defer cancelLive()
	system := "Reply with exactly the requested text. Keep this instruction unchanged."
	task := runtime.AgentTask{Provider: connected.Provider, Model: connected.ID, System: system, Prompt: "Reply with exactly: OK", Directory: dir, Tools: map[string]bool{"read": true, "write": false, "edit": false, "bash": false}}
	first, err := livePrompt(ctxLive, c, id, task, 1, t)
	if err != nil {
		if unavailableModel(err) {
			t.Logf("prompt unavailable; exact error: %v; prompt portion skipped", err)
			return
		}
		t.Fatal(err)
	}
	t.Logf("turn 1 usage.input=%d cacheRead=%d cacheWrite=%d output=%d", first.Input, first.CacheRead, first.CacheWrite, first.Output)
	t.Logf("fixed per-session overhead estimate: %d input tokens (first near-empty prompt)", first.Input)
	second, err := livePrompt(ctxLive, c, id, task, 2, t)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("turn 2 same-session usage.input=%d cacheRead=%d cacheWrite=%d output=%d", second.Input, second.CacheRead, second.CacheWrite, second.Output)
	other, err := c.StartSession(ctxLive, runtime.SessionConfig{Title: "Relo v2 cache smoke", Directory: filepath.Clean(dir), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseSession(context.Background(), other) }()
	third, err := livePrompt(ctxLive, c, other, task, 3, t)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("turn 3 fresh-session usage.input=%d cacheRead=%d cacheWrite=%d output=%d", third.Input, third.CacheRead, third.CacheWrite, third.Output)
}

func livePrompt(ctx context.Context, c *Client, session runtime.SessionID, task runtime.AgentTask, turn int, t *testing.T) (runtime.Usage, error) {
	events, err := c.Events(ctx, session)
	if err != nil {
		return runtime.Usage{}, err
	}
	if err := c.SendTask(ctx, session, task); err != nil {
		return runtime.Usage{}, err
	}
	var terminal runtime.Event
	for !terminal.Done && terminal.Error == "" {
		select {
		case ev, ok := <-events:
			if !ok {
				return runtime.Usage{}, fmt.Errorf("turn %d event stream closed", turn)
			}
			if ev.Delta != "" {
				t.Logf("turn %d delta: %q", turn, ev.Delta)
			}
			if ev.Done || ev.Error != "" {
				terminal = ev
			}
		case <-ctx.Done():
			return runtime.Usage{}, ctx.Err()
		}
	}
	if terminal.Error != "" {
		return runtime.Usage{}, fmt.Errorf("turn %d: %s", turn, terminal.Error)
	}
	if terminal.Text == "" || terminal.Usage == nil {
		return runtime.Usage{}, fmt.Errorf("turn %d terminal missing full text or usage: %+v", turn, terminal)
	}
	t.Logf("turn %d final=%q", turn, terminal.Text)
	return *terminal.Usage, nil
}

func unavailableModel(err error) bool {
	s := strings.ToLower(err.Error())
	for _, needle := range []string{"connection refused", "http 401", "unauthorized", "no credentials", "not authenticated", "no provider", "api key", "missing credentials", "context deadline exceeded"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
