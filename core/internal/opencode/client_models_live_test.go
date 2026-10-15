package opencode

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/provider"
)

// TestLiveModelsCarryConfiguredLimit starts a throwaway OpenCode server with a custom provider whose
// model declares limit.context 64000, then checks Models() reports it. No model turn is run.
// Guarded by RELO_LIVE_OPENCODE=1.
func TestLiveModelsCarryConfiguredLimit(t *testing.T) {
	if os.Getenv("RELO_LIVE_OPENCODE") != "1" {
		t.Skip("set RELO_LIVE_OPENCODE=1 to run live OpenCode model-catalog probe")
	}
	bin, err := Look(os.Getenv("OPENCODE_BIN"))
	if err != nil {
		t.Fatal(err)
	}
	ps := []provider.Provider{{ID: "relolive", Name: "Relo Live", Kind: provider.KindOpenAI, BaseURL: "http://127.0.0.1:9/v1",
		Models: []provider.Model{{ID: "tiny-ctx", Name: "Tiny", Context: 64000, Output: 4000, Reasoning: true}}}}
	cfg, err := provider.BuildConfig(ps, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "serve", "--hostname", "127.0.0.1", "--port", fmt.Sprint(port))
	cmd.Dir = dir
	cmd.Env = []string{}
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "OPENCODE_SERVER_PASSWORD=") && !strings.HasPrefix(env, "OPENCODE_CONFIG_CONTENT=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "OPENCODE_SERVER_PASSWORD=livepw", "OPENCODE_CONFIG_CONTENT="+cfg)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	}()
	c := NewClientWithAuth(fmt.Sprintf("http://127.0.0.1:%d", port), "opencode", "livepw")
	defer c.Close()
	ready := false
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		if c.Health(ctx).Healthy {
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		t.Fatal("OpenCode did not become healthy")
	}
	models, err := c.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.Provider == "relolive" && m.ID == "tiny-ctx" {
			t.Logf("tiny-ctx: context=%d output=%d efforts=%v", m.Context, m.Output, m.Efforts)
			if m.Context != 64000 {
				t.Fatalf("Context = %d, want 64000", m.Context)
			}
			return
		}
	}
	t.Fatalf("relolive/tiny-ctx not listed among %d models", len(models))
}
