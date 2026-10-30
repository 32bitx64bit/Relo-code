package web

import (
	"context"
	"testing"
	"time"
)

func TestConstantsStayWithinAgentTimeout(t *testing.T) {
	if MaxWait >= 65*time.Second {
		t.Fatalf("MaxWait = %v, must stay under the 65s agent tool timeout", MaxWait)
	}
}

func TestNewStartsReadyAndLazy(t *testing.T) {
	e, err := New(Settings{}, Hooks{})
	if err != nil || e == nil {
		t.Fatalf("New = %v, %v", e, err)
	}
	defer e.Close()
	status, err := e.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Ready || status.Mode != ModeAuto || status.Provider != ProviderDuckDuckGo || status.Tabs != 0 {
		t.Fatalf("Status = %+v", status)
	}
}

func TestInvalidCallsReturnErrors(t *testing.T) {
	ctx := context.Background()
	e := &Engine{}
	if _, err := e.Do(ctx, "a", Request{Action: ActionFetch}); err == nil {
		t.Error("Do accepted a fetch without a URL")
	}
	if _, err := e.Wait(ctx, "a", "", time.Second); err == nil {
		t.Error("Wait accepted an agent with no operation")
	}
	if err := CheckURL("file:///etc/passwd", ""); err == nil {
		t.Error("CheckURL accepted a file URL")
	}
}

func TestForgetAgentDoesNotPanic(t *testing.T) {
	(&Engine{}).ForgetAgent("a")
}

func TestStatusNeedsNoBrowser(t *testing.T) {
	e := &Engine{settings: Settings{Mode: ModeHidden, Provider: ProviderCustom, BrowserPath: "/bin/chrome"}}
	got, err := e.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Mode != ModeHidden || got.Provider != ProviderCustom || got.BrowserPath != "/bin/chrome" {
		t.Fatalf("Status = %+v", got)
	}
}
