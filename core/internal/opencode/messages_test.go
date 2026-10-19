package opencode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/relocode/relo/internal/runtime"
)

type recordedRequest struct {
	Method  string
	Path    string
	Query   string
	Auth    string
	Escaped string
	Body    string
}

// recordingServer answers every request with 204 and records what it received, authenticated or not.
func recordingServer(t *testing.T) (*httptest.Server, *[]recordedRequest, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var seen []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/mcp" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"name":"relo","status":{"status":"connected"}}]}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		user, pass, _ := r.BasicAuth()
		mu.Lock()
		seen = append(seen, recordedRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Auth: user + ":" + pass, Escaped: r.URL.EscapedPath(), Body: string(b),
		})
		mu.Unlock()
		if user != "opencode" || pass != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen, &mu
}

func TestConfigureMessagesPutsRemoteReloMCP(t *testing.T) {
	srv, seen, mu := recordingServer(t)
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	dir := "/tmp/work dir/&x=1"
	err := c.ConfigureMessages(context.Background(), dir, runtime.MessageTools{URL: "http://127.0.0.1:9/mcp", Secret: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(*seen))
	}
	got := (*seen)[0]
	if got.Method != http.MethodPut || got.Path != "/api/experimental/mcp/relo" {
		t.Fatalf("request = %s %s, want PUT /api/experimental/mcp/relo", got.Method, got.Path)
	}
	wantQuery := url.Values{"location[directory]": {dir}}.Encode()
	if got.Query != wantQuery {
		t.Fatalf("query = %q, want %q", got.Query, wantQuery)
	}
	if u, _ := url.ParseQuery(got.Query); u.Get("location[directory]") != dir {
		t.Fatalf("directory round-trip = %q, want %q", u.Get("location[directory]"), dir)
	}
	if got.Auth != "opencode:secret" {
		t.Fatalf("basic auth = %q", got.Auth)
	}
	wantBody := `{"config":{"codemode":false,"headers":{"Authorization":"Bearer s3cret"},"oauth":false,"type":"remote","url":"http://127.0.0.1:9/mcp"}}`
	if got.Body != wantBody {
		t.Fatalf("body =\n%s\nwant\n%s", got.Body, wantBody)
	}
}

func TestConfigureMessagesSurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "bad mcp")
	}))
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	if err := c.ConfigureMessages(context.Background(), "/w", runtime.MessageTools{URL: "u", Secret: "s"}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v, want HTTP 400 error", err)
	}
}

func TestConfigureMessagesDoesNotReconnectUnchangedEndpoint(t *testing.T) {
	srv, seen, mu := recordingServer(t)
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.ConfigureMessages(context.Background(), "/w", runtime.MessageTools{URL: "http://127.0.0.1:9/mcp", Secret: "s"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := c.ConfigureMessages(context.Background(), "/w", runtime.MessageTools{URL: "http://127.0.0.1:10/mcp", Secret: "next"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*seen) != 2 {
		t.Fatalf("registration requests=%d, want initial plus changed endpoint only", len(*seen))
	}
}

func TestConfigureMessagesWaitsForConnectionBeforeReturning(t *testing.T) {
	var checks int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		checks++
		state := "pending"
		if checks > 1 {
			state = "connected"
		}
		_, _ = io.WriteString(w, `{"data":[{"name":"relo","status":{"status":"`+state+`"}}]}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	if err := c.ConfigureMessages(context.Background(), "/w", runtime.MessageTools{URL: "http://127.0.0.1:9/mcp", Secret: "s"}); err != nil {
		t.Fatal(err)
	}
	if checks != 2 {
		t.Fatalf("readiness checks=%d, want pending then connected", checks)
	}
}

func TestConfigureMessagesCancelsCatalogWaitAndDoesNotCacheIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registrations := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			registrations++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"name":"relo","status":{"status":"connected"}}]}`)
		if registrations == 1 {
			cancel()
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	tools := runtime.MessageTools{URL: "http://127.0.0.1:9/mcp", Secret: "s"}
	if err := c.ConfigureMessages(ctx, "/w", tools); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled catalog wait = %v", err)
	}
	if err := c.ConfigureMessages(context.Background(), "/w", tools); err != nil {
		t.Fatal(err)
	}
	if registrations != 2 {
		t.Fatalf("registration requests = %d, want failed wait retried", registrations)
	}
}

func TestDeliverMessagePostsSyntheticSteerOnly(t *testing.T) {
	srv, seen, mu := recordingServer(t)
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	ctx := context.Background()
	// A turn timer and a claimed turn must survive delivery untouched.
	c.startTurnTimer("ses_m", 3600)
	c.mu.Lock()
	c.turnDone["ses_m"] = true
	timer := c.timers["ses_m"]
	c.mu.Unlock()

	if err := c.DeliverMessage(ctx, runtime.SessionID("ses_m"), "msg_1", "hello peer"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*seen) != 1 {
		t.Fatalf("requests = %+v, want exactly one (synthetic only)", *seen)
	}
	got := (*seen)[0]
	if got.Method != http.MethodPost || got.Path != "/api/session/ses_m/synthetic" || got.Query != "" {
		t.Fatalf("request = %s %s?%s", got.Method, got.Path, got.Query)
	}
	if got.Auth != "opencode:secret" {
		t.Fatalf("basic auth = %q", got.Auth)
	}
	wantBody := `{"delivery":"steer","description":"Relo peer coordination","metadata":{"reloPeerMessage":"msg_1"},"resume":false,"text":"hello peer"}`
	if got.Body != wantBody {
		t.Fatalf("body =\n%s\nwant\n%s", got.Body, wantBody)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.turnDone["ses_m"] || c.timers["ses_m"] != timer || timer == nil {
		t.Fatalf("delivery changed turn state: turnDone=%v timer kept=%v", c.turnDone["ses_m"], c.timers["ses_m"] == timer)
	}
	if timer != nil {
		timer.Stop()
	}
}

func TestDeliverMessageEscapesSessionID(t *testing.T) {
	srv, seen, mu := recordingServer(t)
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	if err := c.DeliverMessage(context.Background(), runtime.SessionID("a/b"), "m", "t"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	// The session ID must reach the server as one escaped segment, so a "/" inside it cannot reroute the request.
	if len(*seen) != 1 || (*seen)[0].Escaped != "/api/session/a%2Fb/synthetic" {
		t.Fatalf("escaped path = %+v", *seen)
	}
}

// The permission PATCH for Primary must allow relo_agents after the "*" deny, and must not allow anything else.
func TestPrimaryPermissionsAllowReloAgentsAndCanvasAfterWildcardDeny(t *testing.T) {
	srv, seen, mu := recordingServer(t)
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	tools := runtime.WithCanvas(runtime.ToolsFor(runtime.RolePrimary, "build", false), true)
	if err := c.setTools(context.Background(), "ses_p", tools); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	body := (*seen)[0].Body
	wildcardDeny := strings.Index(body, `{"action":"*","effect":"deny","resource":"*"}`)
	for _, name := range []string{runtime.ReloAgentsTool, runtime.ReloCanvasTool} {
		index := strings.Index(body, `{"action":"`+name+`","effect":"allow","resource":"*"}`)
		if wildcardDeny < 0 || index < 0 || index < wildcardDeny {
			t.Fatalf("want %s allow after * deny in %s", name, body)
		}
	}
	if strings.Count(body, `"effect":"allow"`) != 3 { // the base allow-all plus the two Relo tools only
		t.Fatalf("unexpected allow rules in %s", body)
	}
}

// Non-primary roles have no wildcard deny, so relo_agents needs no extra rule and their request body is unchanged.
func TestWorkerPermissionsUnchangedByReloAgents(t *testing.T) {
	srv, seen, mu := recordingServer(t)
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	if err := c.setTools(context.Background(), "ses_w", runtime.ToolsFor("worker", "build", true)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains((*seen)[0].Body, "relo_agents") {
		t.Fatalf("worker permissions must not mention relo_agents: %s", (*seen)[0].Body)
	}
}

var _ runtime.MessageRuntime = (*Adapter)(nil)
