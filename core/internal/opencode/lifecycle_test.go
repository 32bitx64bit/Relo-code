package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/relocode/relo/internal/runtime"
)

// lifecycleServer answers interrupt and delete with the given status codes.
func lifecycleServer(t *testing.T, interruptStatus, deleteStatus int) (*httptest.Server, *sync.Mutex, *int, *int) {
	t.Helper()
	var mu sync.Mutex
	interrupts, deletes := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session/ses_l/interrupt", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		mu.Lock()
		interrupts++
		mu.Unlock()
		w.WriteHeader(interruptStatus)
		_, _ = io.WriteString(w, `{"error":"gone"}`)
	})
	mux.HandleFunc("/api/session/ses_l", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		mu.Lock()
		deletes++
		mu.Unlock()
		w.WriteHeader(deleteStatus)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &mu, &interrupts, &deletes
}

func TestCancelNotFoundIsSuccess(t *testing.T) {
	srv, _, _, _ := lifecycleServer(t, http.StatusNotFound, http.StatusNoContent)
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	if err := c.Cancel(context.Background(), runtime.SessionID("ses_l")); err != nil {
		t.Fatalf("interrupt 404 should be nil, got %v", err)
	}
}

func TestCancelServerErrorIsError(t *testing.T) {
	srv, _, _, _ := lifecycleServer(t, http.StatusInternalServerError, http.StatusNoContent)
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	if err := c.Cancel(context.Background(), runtime.SessionID("ses_l")); err == nil {
		t.Fatal("interrupt 500 should return an error")
	}
}

func TestCloseSessionIsIdempotent(t *testing.T) {
	// Second delete returns 404 because the session is already gone.
	var mu sync.Mutex
	deletes := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session/ses_l", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		mu.Lock()
		deletes++
		n := deletes
		mu.Unlock()
		if n > 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	ctx := context.Background()
	if err := c.CloseSession(ctx, runtime.SessionID("ses_l")); err != nil {
		t.Fatalf("first CloseSession: %v", err)
	}
	if err := c.CloseSession(ctx, runtime.SessionID("ses_l")); err != nil {
		t.Fatalf("second CloseSession should be nil, got %v", err)
	}
}

func TestCloseSessionServerErrorIsError(t *testing.T) {
	srv, _, _, _ := lifecycleServer(t, http.StatusNoContent, http.StatusInternalServerError)
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	if err := c.CloseSession(context.Background(), runtime.SessionID("ses_l")); err == nil {
		t.Fatal("delete 500 should return an error")
	}
}

func TestCancelAfterCloseIsSafe(t *testing.T) {
	srv, _, _, _ := lifecycleServer(t, http.StatusNotFound, http.StatusNoContent)
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	ctx := context.Background()
	if err := c.CloseSession(ctx, runtime.SessionID("ses_l")); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if err := c.Cancel(ctx, runtime.SessionID("ses_l")); err != nil {
		t.Fatalf("Cancel after Close should be nil, got %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Cancel(ctx, runtime.SessionID("ses_l")); err != nil {
		t.Fatalf("Cancel after client Close should be nil on 404, got %v", err)
	}
}

func TestSetToolsDenyRulesAreDeterministic(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session/ses_p", func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(w, r) {
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClientWithAuth(srv.URL, "opencode", "secret")
	defer c.Close()
	tools := map[string]bool{"read": true, "write": false, "edit": false, "bash": false}
	for i := 0; i < 5; i++ {
		if err := c.setTools(context.Background(), "ses_p", tools); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatalf("permission body not deterministic:\n%s\n%s", bodies[0], b)
		}
	}
	var payload struct {
		Permissions []map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil {
		t.Fatal(err)
	}
	denied := map[string]bool{}
	for _, rule := range payload.Permissions {
		if rule["effect"] == "deny" {
			denied[rule["action"]] = true
		}
	}
	for _, want := range []string{"write", "edit", "bash", "shell"} {
		if !denied[want] {
			t.Fatalf("missing deny rule for %s in %v", want, payload.Permissions)
		}
	}
	// "shell" is what OpenCode v2 calls the command tool; "bash" alone left it open to a Primary with no tools.
	edits := 0
	for _, rule := range payload.Permissions {
		if rule["action"] == "edit" {
			edits++
		}
	}
	if edits != 1 {
		t.Fatalf("edit denied %d times, want once: %v", edits, payload.Permissions)
	}
}

func TestIsolationEnvAndChildEnv(t *testing.T) {
	if Isolation("") != nil {
		t.Fatal("empty root must mean no isolation")
	}
	iso := Isolation("/relo")
	joined := strings.Join(iso, "\n")
	for _, want := range []string{"XDG_CONFIG_HOME=/relo/config", "XDG_DATA_HOME=/relo/data", "XDG_CACHE_HOME=/relo/cache", "XDG_STATE_HOME=/relo/state", "OPENCODE_DISABLE_PROJECT_CONFIG=1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %v", want, iso)
		}
	}
	inherited := []string{"PATH=/bin", "HOME=/home/u", "XDG_CONFIG_HOME=/home/u/.config", "OPENCODE_CONFIG=/home/u/mine.json", "OPENCODE_CONFIG_DIR=/home/u/dir", "OPENCODE_SERVER_PASSWORD=old", "OPENCODE_CONFIG_CONTENT=stale"}
	got := childEnv(inherited, append(iso, "OPENCODE_CONFIG_CONTENT=fresh", "OPENCODE_SERVER_PASSWORD=new"))
	count := map[string]int{}
	val := map[string]string{}
	for _, e := range got {
		k, v, _ := strings.Cut(e, "=")
		count[k]++
		val[k] = v
	}
	for k, n := range count {
		if n != 1 {
			t.Errorf("%s appears %d times", k, n)
		}
	}
	if val["XDG_CONFIG_HOME"] != "/relo/config" || val["OPENCODE_CONFIG_CONTENT"] != "fresh" || val["OPENCODE_SERVER_PASSWORD"] != "new" {
		t.Fatalf("overrides lost: %v", val)
	}
	if _, ok := val["OPENCODE_CONFIG"]; ok {
		t.Fatal("user's OPENCODE_CONFIG must not reach an isolated child")
	}
	if _, ok := val["OPENCODE_CONFIG_DIR"]; ok {
		t.Fatal("user's OPENCODE_CONFIG_DIR must not reach an isolated child")
	}
	if val["PATH"] != "/bin" || val["HOME"] != "/home/u" {
		t.Fatalf("unrelated env must pass through: %v", val)
	}
	// Not isolated: the user's own pointers survive.
	plain := childEnv(inherited, []string{"OPENCODE_CONFIG_CONTENT=x"})
	if !strings.Contains(strings.Join(plain, "\n"), "OPENCODE_CONFIG=/home/u/mine.json") {
		t.Fatal("without isolation the inherited OPENCODE_CONFIG must pass through")
	}
}
