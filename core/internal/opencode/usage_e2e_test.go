package opencode

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/relocode/relo/internal/provider"
	"github.com/relocode/relo/internal/runtime"
)

func TestProviderPromptUsage(t *testing.T) {
	if os.Getenv("RELO_E2E_OPENCODE") == "" {
		t.Skip("set RELO_E2E_OPENCODE=1 to run against the installed opencode")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode is not installed")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"claude-fake\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"claude-fake\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":69144,\"completion_tokens\":8034,\"total_tokens\":77178,\"prompt_tokens_details\":{\"cached_tokens\":66457,\"cache_creation_tokens\":2685}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	env, err := provider.Env([]provider.Provider{{ID: "fake", Kind: provider.KindOpenAI, BaseURL: srv.URL + "/v1", Models: []provider.Model{{ID: "claude-fake", Context: 200000, Output: 16000}}}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	proc, err := Start(ctx, bin, t.TempDir(), append(Isolation(t.TempDir()), env...))
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Stop()
	rt := proc.Runtime()
	sid, err := rt.StartSession(ctx, runtime.SessionConfig{Title: "usage", Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	events, err := rt.Events(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.SendTask(ctx, sid, runtime.AgentTask{Model: "claude-fake", Provider: "fake", Prompt: "Reply OK"}); err != nil {
		t.Fatal(err)
	}
	sawStep := false
	for ev := range events {
		if ev.Type == "session.step.ended" {
			sawStep = true
			if ev.ContextTokens != 69144 {
				t.Fatalf("step context=%d, want prompt context 69144", ev.ContextTokens)
			}
		}
		if ev.Done {
			if ev.Error != "" {
				t.Fatal(ev.Error)
			}
			if ev.Usage == nil || ev.Usage.Context != 69144 {
				t.Fatalf("usage=%+v, want prompt context 69144", ev.Usage)
			}
			if !sawStep {
				t.Fatal("no per-request step usage")
			}
			return
		}
	}
	t.Fatal("no terminal event")
}
