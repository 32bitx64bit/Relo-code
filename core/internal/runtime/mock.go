package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/id"
)

type Mock struct {
	mu         sync.Mutex
	sessions   map[string]*mockSess
	models     []Model
	delay      time.Duration
	systemUses map[string]int
}

type mockSess struct {
	id        string
	events    chan Event
	n         int
	system    string
	cacheRead int
	wg        sync.WaitGroup
}

func NewMock() *Mock {
	return &Mock{
		sessions:   map[string]*mockSess{},
		systemUses: map[string]int{},
		delay:      15 * time.Millisecond,
		models: []Model{
			{ID: "relo-primary", Provider: "mock", Name: "Relo Primary (mock)"},
			{ID: "relo-tier1", Provider: "mock", Name: "Relo Tier 1 (mock)"},
			{ID: "relo-tier2", Provider: "mock", Name: "Relo Tier 2 (mock)"},
		},
	}
}

func (m *Mock) StartSession(ctx context.Context, cfg SessionConfig) (SessionID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := id.New("oc")
	m.sessions[id] = &mockSess{id: id, events: make(chan Event, 64)}
	return SessionID(id), nil
}

func (m *Mock) SendTask(ctx context.Context, session SessionID, task AgentTask) error {
	m.mu.Lock()
	s, ok := m.sessions[string(session)]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown session")
	}
	s.n++
	n := s.n
	cacheWrite := 0
	cacheRead := 0
	if task.System != "" {
		prefixTokens := max(1, len(task.System)/4)
		uses := m.systemUses[task.System]
		if uses == 0 {
			cacheWrite = prefixTokens
		} else {
			cacheRead = uses * prefixTokens
		}
		m.systemUses[task.System] = uses + 1
	}
	s.system = task.System
	usage := &Usage{Input: max(1, (len(task.System)+len(task.Prompt))/4), Output: 32, CacheRead: cacheRead, CacheWrite: cacheWrite}
	usage.Context = usage.PromptTokens()
	s.wg.Add(1)
	m.mu.Unlock()
	go m.play(s, task, n, usage)
	return nil
}

func (m *Mock) play(s *mockSess, task AgentTask, n int, usage *Usage) {
	defer s.wg.Done()
	ch := s.events
	emit := func(ev Event) {
		select {
		case ch <- ev:
		case <-time.After(2 * time.Second):
		}
	}
	time.Sleep(m.delay)
	role := strings.ToLower(task.Role)
	body := m.response(role, task, n)
	emit(Event{Type: "session.reasoning.started", Phase: "reasoning"})
	emit(Event{Type: "message.part.updated", Delta: body[:min(40, len(body))], PartType: "text", Phase: "writing", Text: body})
	if role == "worker" || role == "architect" || role == "debug" {
		noise := strings.Repeat("exploratory grep hit in vendor cache\n", 30)
		emit(Event{Type: "message.part.updated", PartType: "tool", Phase: "tool", Tool: "grep", ToolID: "t1", Text: noise})
		emit(Event{Type: "session.tool.success", Phase: "model", ToolID: "t1", ToolDone: true})
	}
	emit(Event{Type: "session.idle", Idle: true, Done: true, JSON: body, Text: body, Usage: usage})
}

func (m *Mock) response(role string, task AgentTask, n int) string {
	obj := extractObjective(task)
	switch role {
	case "primary", "head":
		return primaryJSON(obj, n)
	case "probe":
		return probeJSON(obj)
	case "reviewer":
		return reportJSON("completed", "Independent review found no blocking defects.", []string{"Changes match acceptance criteria."}, nil, []string{"go test ./..."}, []string{"timeout integration untested"}, "")
	case "test":
		return reportJSON("completed", "Verification commands passed.", nil, nil, []string{"go test ./... passed"}, nil, "")
	case "integration":
		return reportJSON("completed", "Compatible patches applied without overwriting user files.", nil, []string{"internal/auth/session.go"}, []string{"go test ./..."}, nil, "")
	case "debug":
		return reportJSON("completed", "Fixed remaining test failure.", []string{"Race on token refresh."}, []string{"internal/auth/session.go"}, []string{"go test -race ./internal/auth/... passed"}, nil, "two concurrent refresh requests could replace the same token")
	default:
		if n == 1 && (role == "worker" || role == "architect") {
			return reportJSON("completed", "Implemented the assigned subsystem from curated context.", []string{"Used verified probe findings rather than raw history."}, []string{"internal/auth/session.go", "internal/auth/session_test.go"}, []string{"go test ./internal/auth/... passed"}, []string{"provider timeout behavior has not been integration tested"}, "two concurrent refresh requests could replace the same token")
		}
		return reportJSON("completed", "Continued from refreshed context without losing task state.", []string{"Pinned constraints survived."}, []string{"internal/auth/session.go"}, []string{"go test ./internal/auth/... passed"}, nil, "")
	}
}

func (m *Mock) Events(ctx context.Context, session SessionID) (<-chan Event, error) {
	m.mu.Lock()
	s, ok := m.sessions[string(session)]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown session")
	}
	return s.events, nil
}

func (m *Mock) Cancel(ctx context.Context, session SessionID) error {
	return nil
}

func (m *Mock) CloseSession(ctx context.Context, session SessionID) error {
	m.mu.Lock()
	s, ok := m.sessions[string(session)]
	if ok {
		delete(m.sessions, string(session))
	}
	m.mu.Unlock()
	if ok {
		s.wg.Wait()
		close(s.events)
	}
	return nil
}

func (m *Mock) Models(ctx context.Context) ([]Model, error) { return m.models, nil }

func (m *Mock) Providers(ctx context.Context) ([]Provider, error) {
	return []Provider{{ID: "mock", Name: "Mock", Models: m.models}}, nil
}

func (m *Mock) Health(ctx context.Context) Health {
	return Health{Healthy: true, Version: "mock-1", Kind: "mock"}
}

func (m *Mock) Close() error { return nil }

func primaryJSON(obj string, n int) string {
	if n > 1 {
		return `{
  "status": "completed",
  "userMessage": "Objective complete. Probe findings were promoted, workers ran in isolated worktrees, context was refreshed when utilization grew, verification passed, and the parent received compressed reports rather than raw transcripts.",
  "objective": ` + jq(obj) + `,
  "refreshSelf": false,
  "tasks": [],
  "spawns": [],
  "decisions": ["Treat worker reports as context, not transcripts."],
  "risks": ["provider timeout integration remains untested"]
}`
	}
	return `{
  "status": "working",
  "userMessage": "I split this into reconnaissance, then parallel implementation, then verification. Workers will receive curated context rather than full transcripts.",
  "objective": ` + jq(obj) + `,
  "refreshSelf": false,
  "decisions": ["Probe first", "Parallelize independent implementation"],
  "risks": [],
  "tasks": [
    {"id": "probe-auth", "title": "Investigate existing auth architecture", "role": "probe", "description": "Find where authentication lives and how sessions work.", "acceptance": ["Named files and symbols", "Structured findings"], "priority": 10, "deps": []},
    {"id": "impl-backend", "title": "Implement backend OAuth", "role": "worker", "description": "Implement server-side OAuth using probe findings.", "acceptance": ["Google and GitHub login endpoints", "Tests for refresh race"], "priority": 8, "deps": ["probe-auth"]},
    {"id": "impl-frontend", "title": "Implement frontend OAuth UI", "role": "worker", "description": "Add login buttons and callback handling.", "acceptance": ["Google and GitHub buttons render"], "priority": 8, "deps": ["probe-auth"]},
    {"id": "verify", "title": "Verify changes", "role": "test", "description": "Run project tests and report failures structurally.", "acceptance": ["Tests pass or failures are structured"], "priority": 6, "deps": ["impl-backend", "impl-frontend"]},
    {"id": "review", "title": "Independent review", "role": "reviewer", "description": "Review correctness, security, and acceptance criteria.", "acceptance": ["Review report produced"], "priority": 5, "deps": ["verify"]}
  ],
  "spawns": []
}`
}

func probeJSON(obj string) string {
	return `{
  "status": "completed",
  "summary": "Authentication currently lives in internal/auth with a session refresh race.",
  "findings": [
    "OAuth callbacks should be added beside internal/auth/session.go",
    "Token refresh is not serialized by session ID"
  ],
  "filesChanged": [],
  "verification": [],
  "remainingRisks": ["No existing Google/GitHub providers"],
  "recommendedNextActions": ["Implement serialized refresh", "Add OAuth providers"],
  "rootCause": "two concurrent refresh requests could replace the same token",
  "structuredFindings": [
    {"kind": "architecture", "summary": "Auth subsystem is internal/auth", "files": ["internal/auth/session.go"], "confidence": 0.9, "verified": true},
    {"kind": "bug", "summary": "Session refresh race", "files": ["internal/auth/session.go"], "confidence": 0.85, "verified": true, "rootCause": "two concurrent refresh requests could replace the same token"}
  ]
}`
}

func reportJSON(status, summary string, findings, files, verify, risks []string, cause string) string {
	v := []map[string]string{}
	for _, c := range verify {
		v = append(v, map[string]string{"command": c, "status": "passed"})
	}
	payload := map[string]any{
		"status":                 status,
		"summary":                summary,
		"findings":               findings,
		"filesChanged":           files,
		"verification":           v,
		"remainingRisks":         risks,
		"recommendedNextActions": []string{},
		"rootCause":              cause,
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	return string(b)
}

func extractObjective(task AgentTask) string {
	s := task.Prompt + "\n" + task.System
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(strings.ToLower(line), "objective") && len(line) > 12 {
			return strings.TrimSpace(line)
		}
	}
	if task.Prompt != "" {
		return first(task.Prompt, 200)
	}
	return "complete the assigned engineering task"
}

func jq(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func first(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
