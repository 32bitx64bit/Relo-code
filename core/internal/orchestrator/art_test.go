package orchestrator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/art"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/sprite"
)

const artTestKey = "sk-test-SECRET0123456789"

// imageAPI is a fake OpenAI-compatible image endpoint: it answers every request with n pictures and usage.
type imageAPI struct {
	srv   *httptest.Server
	discs [][]byte // pictures of different widths, so none is a copy of another
	mu    sync.Mutex
	paths []string
	fail  int // answer with this status instead, when set
}

func newImageAPI(t *testing.T) *imageAPI {
	t.Helper()
	a := &imageAPI{}
	for i := 1; i <= 4; i++ {
		p := filepath.Join(t.TempDir(), "disc.png")
		writeDiscs(t, p, i)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		a.discs = append(a.discs, data)
	}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		n, _ := strconv.Atoi(r.FormValue("n"))
		if r.Header.Get("Content-Type") == "application/json" {
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			if v, ok := m["n"].(float64); ok {
				n = int(v)
			}
		}
		a.mu.Lock()
		a.paths = append(a.paths, r.URL.Path)
		fail := a.fail
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail != 0 {
			w.WriteHeader(fail)
			fmt.Fprintf(w, `{"error":{"message":"refused for %s"}}`, artTestKey)
			return
		}
		var data []map[string]string
		for i := 0; i < min(4, max(1, n)); i++ {
			data = append(data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(a.discs[i])})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"input_tokens": 300, "output_tokens": 1056,
			"input_tokens_details": map[string]int{"text_tokens": 100, "image_tokens": 200}}})
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *imageAPI) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.paths...)
}

// artFixture is a bound art worker with picture generation switched on against a fake endpoint, and a user who answers
// the approval prompt.
type artFixture struct {
	*spriteFixture
	api      *imageAPI
	mu       sync.Mutex
	answer   string // once, always, reject, or hold to leave the question open
	events   []string
	payloads []string
	asked    []map[string]any
	provider string
}

func newArtFixture(t *testing.T, role agent.Role, model string, isArt bool, set func(*art.Settings)) *artFixture {
	t.Helper()
	f := &artFixture{spriteFixture: newSpriteFixture(t, role, model, isArt), api: newImageAPI(t), answer: "once"}
	t.Cleanup(f.c.CloseArt)
	f.c.SetArtEndpoint(func(provider string) (art.Endpoint, error) {
		f.mu.Lock()
		f.provider = provider
		f.mu.Unlock()
		return art.Endpoint{BaseURL: f.api.srv.URL + "/v1", Key: artTestKey}, nil
	})
	f.c.SetEmitter(func(name string, payload any) {
		b, _ := json.Marshal(payload)
		f.mu.Lock()
		f.events = append(f.events, name)
		f.payloads = append(f.payloads, string(b))
		answer := f.answer
		f.mu.Unlock()
		if name == "permission.asked" {
			m := payload.(map[string]any)
			f.mu.Lock()
			f.asked = append(f.asked, m)
			f.mu.Unlock()
			if answer != "hold" {
				f.c.RespondPermission(m["id"].(string), answer)
			}
		}
	})
	s := art.Settings{Enabled: true, Provider: "openai", Count: 2, SessionJobLimit: 8, BudgetUSD: 3, Approval: "session"}
	if set != nil {
		set(&s)
	}
	f.c.SetArtSettings(s)
	return f
}

func (f *artFixture) call(req agentmsg.ArtRequest) (agentmsg.ImageResult, error) {
	if req.Key == "" {
		req.Key = f.key
	}
	return f.c.artRequest(context.Background(), req)
}

func (f *artFixture) mustCall(t *testing.T, req agentmsg.ArtRequest) agentmsg.ImageResult {
	t.Helper()
	res, err := f.call(req)
	if err != nil {
		t.Fatalf("%s: %v", req.Action, err)
	}
	return res
}

func (f *artFixture) answerWith(a string) { f.mu.Lock(); f.answer = a; f.mu.Unlock() }

func (f *artFixture) askedCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.asked) }

func (f *artFixture) manager(t *testing.T) *art.Manager {
	t.Helper()
	m, err := f.c.artManager(f.session)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// waitState polls the job until it reaches a state.
func (f *artFixture) waitState(t *testing.T, id string, states ...string) art.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, _ := f.manager(t).Get(id)
		for _, s := range states {
			if j.State == s {
				return j
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s is %s (%s), wanted %v", id, j.State, j.Message, states)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func jobIDOf(t *testing.T, res agentmsg.ImageResult) string {
	t.Helper()
	meta := imageMetadata(t, agentmsg.ImageResult{Text: res.Text})
	id, _ := meta["job"].(string)
	if id == "" {
		t.Fatalf("no job in %v", meta)
	}
	return id
}

// readyJob generates and waits until the job has its pictures.
func (f *artFixture) readyJob(t *testing.T, req agentmsg.ArtRequest) string {
	t.Helper()
	req.Action = "generate"
	if req.Prompt == "" {
		req.Prompt = "an iron ingot on a flat magenta background"
	}
	id := jobIDOf(t, f.mustCall(t, req))
	f.waitState(t, id, art.StateReady)
	return id
}

func TestArtRequestRefusals(t *testing.T) {
	cases := []struct {
		name  string
		role  agent.Role
		model string
		art   bool
		setup func(*artFixture)
		req   agentmsg.ArtRequest
		want  string
	}{
		{name: "disabled", role: agent.Worker, model: "p/seeing", art: true, want: "not enabled",
			setup: func(f *artFixture) { f.c.SetArtSettings(art.Settings{}) }},
		{name: "primary", role: agent.Primary, model: "p/seeing", art: true, want: "Primary"},
		{name: "not an art task", role: agent.Worker, model: "p/seeing", art: false, want: "make art"},
		{name: "text only model", role: agent.Worker, model: "p/blind", art: true, want: "cannot see images"},
		{name: "unknown key", role: agent.Worker, model: "p/seeing", art: true, want: "art key", req: agentmsg.ArtRequest{Key: "nope"}},
		{name: "reviewer", role: agent.Reviewer, model: "p/seeing", art: true, want: "cannot spend"},
		{name: "plan mode", role: agent.Worker, model: "p/seeing", art: true, want: "build mode",
			setup: func(f *artFixture) { f.c.SetMode(ModePlan) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newArtFixture(t, tc.role, tc.model, tc.art, nil)
			if tc.setup != nil {
				tc.setup(f)
			}
			req := tc.req
			req.Action = "status"
			if _, err := f.call(req); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestArtStatusAndPlaybook(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	meta := imageMetadata(t, agentmsg.ImageResult{Text: f.mustCall(t, agentmsg.ArtRequest{Action: "status"}).Text})
	if meta["enabled"] != true || meta["ready"] != true || meta["model"] != art.DefaultModel || meta["picturesPerJob"] != float64(2) {
		t.Fatalf("status = %v", meta)
	}
	budget := meta["budget"].(map[string]any)
	if budget["limitUsd"] != float64(3) || budget["sessionJobLimit"] != float64(8) || budget["remainingUsd"] != float64(3) {
		t.Fatalf("budget = %v", budget)
	}
	if topics, _ := meta["playbookTopics"].([]any); len(topics) != 9 {
		t.Fatalf("topics = %v", meta["playbookTopics"])
	}
	for _, leak := range []string{"SECRET", "api.openai", f.api.srv.URL} {
		if b, _ := json.Marshal(meta); strings.Contains(string(b), leak) {
			t.Fatalf("status shows %q", leak)
		}
	}

	if text, _ := f.mustCall(t, agentmsg.ArtRequest{Action: "playbook"}).Text.(string); !strings.Contains(text, "Order of work") {
		t.Fatalf("playbook = %q", text)
	}
	if text, _ := f.mustCall(t, agentmsg.ArtRequest{Action: "playbook", Topic: "sprites"}).Text.(string); !strings.Contains(text, "transparent") {
		t.Fatalf("sprites topic = %q", text)
	}
}

func TestArtStatusSaysWhenNoProviderCanMakePictures(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	f.c.SetArtEndpoint(func(string) (art.Endpoint, error) {
		return art.Endpoint{}, fmt.Errorf("no OpenAI-style provider is configured")
	})
	// A manager made earlier keeps working against the function it was built with, so ask a new workspace.
	f.c.CloseArt()
	meta := imageMetadata(t, agentmsg.ImageResult{Text: f.mustCall(t, agentmsg.ArtRequest{Action: "status"}).Text})
	if meta["ready"] != false || !strings.Contains(meta["problem"].(string), "no OpenAI-style provider") {
		t.Fatalf("status = %v", meta)
	}
	if _, err := f.call(agentmsg.ArtRequest{Action: "generate", Prompt: "an ingot"}); err == nil || !strings.Contains(err.Error(), "no OpenAI-style provider") {
		t.Fatalf("generate without a provider: %v", err)
	}
}

func TestArtLoopFromReferenceToSprite(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	writeDiscs(t, filepath.Join(f.session, "art", "ref", "style.png"), 1)

	refs := imageMetadata(t, agentmsg.ImageResult{Text: f.mustCall(t, agentmsg.ArtRequest{Action: "refs", Sources: []agentmsg.ArtSource{{Path: "art/ref/style.png"}}}).Text})
	list := refs["refs"].([]any)
	if len(list) != 1 {
		t.Fatalf("refs = %v", refs)
	}
	refID := list[0].(map[string]any)["id"].(string)

	started := f.mustCall(t, agentmsg.ArtRequest{Action: "generate", Prompt: "match image 1: a single iron ingot, flat solid magenta background", Refs: []agentmsg.ArtRef{{ID: refID, Role: "style"}}, Quality: "low"})
	id := jobIDOf(t, started)
	if meta := imageMetadata(t, agentmsg.ImageResult{Text: started.Text}); meta["references"] != float64(1) || meta["estimateUsd"].(float64) <= 0 || meta["budget"] == nil {
		t.Fatalf("started = %v", meta)
	}
	job := f.waitState(t, id, art.StateReady)
	if f.askedCount() != 1 {
		t.Fatalf("the user was asked %d times", f.askedCount())
	}
	if got := f.api.calls(); len(got) != 1 || got[0] != "/v1/images/edits" {
		t.Fatalf("a job with a reference should be an edit, calls = %v", got)
	}
	if !strings.HasPrefix(job.Dir, filepath.Join(f.session, ".relo", "art")) {
		t.Fatalf("pictures are kept in %s, not under the session workspace", job.Dir)
	}

	ready := f.mustCall(t, agentmsg.ArtRequest{Action: "wait", Job: id})
	rm := imageMetadata(t, agentmsg.ImageResult{Text: ready.Text})
	if rm["state"] != art.StateReady || len(ready.Images) != 1 || rm["costUsd"].(float64) <= 0 {
		t.Fatalf("ready = %v, images %d", rm, len(ready.Images))
	}
	if v, ok := rm["costFromEstimate"]; !ok || v != false {
		t.Fatalf("costFromEstimate = %v (present %v), want false for a job the endpoint priced", v, ok)
	}
	pics := rm["files"].([]any)
	if len(pics) != 2 || pics[1].(map[string]any)["path"] != ".relo/art/raw/"+id+"/2.png" {
		t.Fatalf("files = %v", pics)
	}

	// The agent takes picture 1 through the sprite pipeline: it is readable from the session workspace, and the sprite
	// carries a record of where it came from.
	res, err := f.spriteFixture.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: ".relo/art/raw/" + id + "/1.png"}, Ops: ironOps(), Output: "Textures/Iron.png"})
	if err != nil {
		t.Fatal(err)
	}
	if sm := spriteMeta(t, res); sm["provenance"] != "art/provenance/Iron.json" {
		t.Fatalf("sprite result = %v", sm)
	}
	data, err := os.ReadFile(filepath.Join(f.work, "art", "provenance", "Iron.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prov struct {
		Job    string
		Index  int
		Prompt string
		Model  string
		Refs   []string
		SHA256 string
		Output []struct{ Path string }
	}
	if err := json.Unmarshal(data, &prov); err != nil {
		t.Fatal(err)
	}
	if prov.Job != id || prov.Index != 1 || !strings.Contains(prov.Prompt, "iron ingot") || prov.Model != art.DefaultModel || len(prov.Refs) != 1 || prov.SHA256 == "" || len(prov.Output) != 1 || prov.Output[0].Path != "Textures/Iron.png" {
		t.Fatalf("provenance = %s", data)
	}
	// A sprite made from a picture nobody generated has no record.
	writeDiscs(t, filepath.Join(f.session, "art", "own.png"), 1)
	res, err = f.spriteFixture.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "art/own.png"}, Ops: ironOps(), Output: "Textures/Own.png"})
	if err != nil {
		t.Fatal(err)
	}
	if spriteMeta(t, res)["provenance"] != nil {
		t.Fatal("provenance recorded for a picture that was not generated")
	}
	if _, err := os.Stat(filepath.Join(f.work, "art", "provenance", "Own.json")); err == nil {
		t.Fatal("a provenance file exists for Own.png")
	}
}

func TestArtTheKeyNeverLeavesTheCore(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	f.readyJob(t, agentmsg.ArtRequest{})
	f.api.mu.Lock()
	f.api.fail = 401
	f.api.mu.Unlock()
	id := jobIDOf(t, f.mustCall(t, agentmsg.ArtRequest{Action: "generate", Prompt: "second"}))
	failed := f.waitState(t, id, art.StateFailed)
	res := f.mustCall(t, agentmsg.ArtRequest{Action: "wait", Job: id})
	f.mu.Lock()
	seen := append([]string(nil), f.payloads...)
	f.mu.Unlock()
	b, _ := json.Marshal(res.Text)
	seen = append(seen, string(b), failed.Message)
	for _, s := range seen {
		if strings.Contains(s, "SECRET") {
			t.Fatalf("the key reached an agent or an event: %s", s)
		}
	}
	if !strings.Contains(failed.Message, "rejected the API key") {
		t.Fatalf("message = %q", failed.Message)
	}
}

func TestArtApprovalRefusedEndsTheJobAndCostsNothing(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	f.answerWith("reject")
	id := jobIDOf(t, f.mustCall(t, agentmsg.ArtRequest{Action: "generate", Prompt: "an ingot"}))
	f.waitState(t, id, art.StateCancelled)
	if b := f.manager(t).Budget(f.c.SessionID()); b.SpentUSD != 0 {
		t.Fatalf("a refused job cost $%.2f", b.SpentUSD)
	}
	if len(f.api.calls()) != 0 {
		t.Fatal("a declined job reached the API")
	}
	res := f.mustCall(t, agentmsg.ArtRequest{Action: "wait", Job: id})
	if m := imageMetadata(t, agentmsg.ImageResult{Text: res.Text}); m["state"] != art.StateCancelled || !strings.Contains(m["next"].(string), "cancelled") {
		t.Fatalf("wait = %v", m)
	}
}

func TestArtApprovalAlwaysCoversTheSession(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	f.answerWith("always")
	f.readyJob(t, agentmsg.ArtRequest{Prompt: "one"})
	f.readyJob(t, agentmsg.ArtRequest{Prompt: "two"})
	if f.askedCount() != 1 {
		t.Fatalf("asked %d times", f.askedCount())
	}
	asked := f.asked[0]
	if !strings.Contains(asked["title"].(string), "$") || !strings.Contains(asked["command"].(string), "one") || !strings.Contains(asked["command"].(string), "budget") {
		t.Fatalf("approval prompt = %v", asked)
	}
}

func TestArtYoloSkipsTheQuestionButNotTheCaps(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, func(s *art.Settings) { s.SessionJobLimit = 1; s.Approval = "each" })
	f.c.permissions().SetYolo(true)
	f.readyJob(t, agentmsg.ArtRequest{Prompt: "one"})
	if f.askedCount() != 0 {
		t.Fatal("yolo still asked")
	}
	if _, err := f.call(agentmsg.ArtRequest{Action: "generate", Prompt: "two"}); err == nil || !strings.Contains(err.Error(), "may") {
		t.Fatalf("yolo lifted the session cap: %v", err)
	}
}

func TestArtTheBudgetStopsAJobBeforeAnythingIsSpent(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, func(s *art.Settings) { s.BudgetUSD = 0.2 })
	_, err := f.call(agentmsg.ArtRequest{Action: "generate", Prompt: "an ingot", Quality: "high", Count: 4})
	if err == nil || !strings.Contains(err.Error(), "not enough budget") {
		t.Fatalf("err = %v", err)
	}
	if f.askedCount() != 0 || len(f.api.calls()) != 0 {
		t.Fatal("a job over the budget asked the user or reached the API")
	}
}

// Turning both limits off lets an agent start more jobs than the old cap and a single job that the old budget would
// have refused, and tells it so in plain words, with no "$0 left" to misread.
func TestArtUnlimitedLimitsLetTheWorkContinue(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, func(s *art.Settings) { s.SessionJobLimit, s.BudgetUSD = 0, 0 })
	meta := imageMetadata(t, agentmsg.ImageResult{Text: f.mustCall(t, agentmsg.ArtRequest{Action: "status"}).Text})
	budget := meta["budget"].(map[string]any)
	if budget["budgetUnlimited"] != true || budget["jobsUnlimited"] != true {
		t.Fatalf("budget = %v", budget)
	}
	for _, key := range []string{"limitUsd", "remainingUsd", "sessionJobLimit"} {
		if _, there := budget[key]; there {
			t.Errorf("%s is shown for a limit that is off: %v", key, budget)
		}
	}
	// The art package's tests start more jobs than any cap; here it is enough that the tool path starts jobs, including
	// one that the tiny budget in TestArtTheBudgetStopsAJobBeforeAnythingIsSpent would have refused.
	f.readyJob(t, agentmsg.ArtRequest{Prompt: "an ingot", Quality: "low", Count: 1})
	f.readyJob(t, agentmsg.ArtRequest{Prompt: "a large ingot", Quality: "high", Count: 4})
	after := imageMetadata(t, agentmsg.ImageResult{Text: f.mustCall(t, agentmsg.ArtRequest{Action: "status"}).Text})["budget"].(map[string]any)
	if after["sessionJobs"] != float64(2) || after["spentUsd"].(float64) <= 0 {
		t.Fatalf("spend should still be counted: %v", after)
	}
}

func TestArtRefusesBadRequestsBeforeSpending(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	for name, req := range map[string]agentmsg.ArtRequest{
		"unknown ref":    {Action: "generate", Prompt: "an ingot", Refs: []agentmsg.ArtRef{{ID: "r_missing"}}},
		"param flag":     {Action: "generate", Prompt: "an ingot --sref r_1"},
		"bad quality":    {Action: "generate", Prompt: "an ingot", Quality: "ultra"},
		"bad size":       {Action: "generate", Prompt: "an ingot", Size: "5000x5000"},
		"wait missing":   {Action: "wait", Job: "j99"},
		"cancel missing": {Action: "cancel", Job: "j99"},
	} {
		if _, err := f.call(req); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if b := f.manager(t).Budget(f.c.SessionID()); b.SpentUSD != 0 || b.SessionJobs != 0 {
		t.Fatalf("refusals cost something: %+v", b)
	}
	if f.askedCount() != 0 {
		t.Fatal("the user was asked about a request that was refused")
	}
}

func TestArtTheProviderSettingIsPassedOn(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	f.readyJob(t, agentmsg.ArtRequest{})
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.provider != "openai" {
		t.Fatalf("the endpoint was asked for provider %q", f.provider)
	}
}

func TestArtCancelIsLimitedToTheSessionsOwnJobs(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	f.answerWith("hold") // the question stays open, so the job stays queued
	id := jobIDOf(t, f.mustCall(t, agentmsg.ArtRequest{Action: "generate", Prompt: "an ingot"}))
	m := f.manager(t)
	if _, err := m.Store().Update(id, func(j *art.Job) { j.SessionID = "someone-else" }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(agentmsg.ArtRequest{Action: "cancel", Job: id}); err == nil || !strings.Contains(err.Error(), "another session") {
		t.Fatalf("cancelled another session's job: %v", err)
	}
	// Its own job it can cancel.
	own := jobIDOf(t, f.mustCall(t, agentmsg.ArtRequest{Action: "generate", Prompt: "second"}))
	res := f.mustCall(t, agentmsg.ArtRequest{Action: "cancel", Job: own})
	if meta := imageMetadata(t, agentmsg.ImageResult{Text: res.Text}); meta["state"] != art.StateCancelled {
		t.Fatalf("cancel = %v", meta)
	}
}

func TestArtRefsFromAJobsPicture(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	id := f.readyJob(t, agentmsg.ArtRequest{})
	res := f.mustCall(t, agentmsg.ArtRequest{Action: "refs", Sources: []agentmsg.ArtSource{{Job: id, Index: 1}}})
	if list := imageMetadata(t, agentmsg.ImageResult{Text: res.Text})["refs"].([]any); len(list) != 1 {
		t.Fatalf("refs = %v", list)
	}
	if _, err := f.call(agentmsg.ArtRequest{Action: "refs", Sources: []agentmsg.ArtSource{{Job: id, Index: 9}}}); err == nil {
		t.Fatal("stored a picture the job does not have")
	}
	if _, err := f.call(agentmsg.ArtRequest{Action: "refs", Sources: []agentmsg.ArtSource{{Path: "../../etc/passwd"}}}); err == nil {
		t.Fatal("read outside the workspace")
	}
}

func TestArtJobsListsOnlyTheSessionsJobs(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	a := f.readyJob(t, agentmsg.ArtRequest{Prompt: "one"})
	if _, err := f.manager(t).Store().Update(a, func(j *art.Job) { j.SessionID = "old" }); err != nil {
		t.Fatal(err)
	}
	b := f.readyJob(t, agentmsg.ArtRequest{Prompt: "two"})
	meta := imageMetadata(t, agentmsg.ImageResult{Text: f.mustCall(t, agentmsg.ArtRequest{Action: "jobs"}).Text})
	jobs := meta["jobs"].([]any)
	if len(jobs) != 1 || jobs[0].(map[string]any)["job"] != b {
		t.Fatalf("jobs = %v", jobs)
	}
}

func TestResumeArtAsksAgainAboutAJobThatWaitedForApproval(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	f.answerWith("hold")
	id := jobIDOf(t, f.mustCall(t, agentmsg.ArtRequest{Action: "generate", Prompt: "an ingot"}))
	deadline := time.Now().Add(5 * time.Second)
	for f.askedCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// Relo stops while the question is open, and starts again with a user who says yes.
	f.c.CloseArt()
	if j, _ := f.manager(t).Get(id); j.State != art.StateQueued {
		t.Fatalf("the job should wait for the restart, it is %s", j.State)
	}
	f.c.CloseArt()
	f.answerWith("once")
	f.c.ResumeArt()
	f.waitState(t, id, art.StateReady)
	if f.askedCount() < 2 {
		t.Fatalf("the user was asked %d times, want the question again", f.askedCount())
	}
}

func TestArtApproveEndsWhenTheContextDoes(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	f.c.SetEmitter(func(string, any) {}) // nobody answers
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got, err := f.c.artApprove(ctx, art.Approval{JobID: "j1", SessionID: "s", AgentID: "artist", Title: "t", Detail: "d", USD: 0.1})
	if got != "reject" || err == nil {
		t.Fatalf("%q %v", got, err)
	}
	f.c.mu.Lock()
	left := len(f.c.pending)
	f.c.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d approvals left pending", left)
	}
}

func TestArtToolIsOfferedWhereSpriteIs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		role    agent.Role
		model   string
		art     bool
		enabled bool
		mode    Mode
		offered bool
	}{
		{"art builder on a seeing model", agent.Worker, "p/seeing", true, true, ModeBuild, true},
		{"feature off", agent.Worker, "p/seeing", true, false, ModeBuild, false},
		{"task is not art", agent.Worker, "p/seeing", false, true, ModeBuild, false},
		{"text only model", agent.Worker, "p/blind", true, true, ModeBuild, false},
		{"plan mode", agent.Worker, "p/seeing", true, true, ModePlan, false},
		{"reviewer", agent.Reviewer, "p/seeing", true, true, ModeBuild, false},
		{"primary", agent.Primary, "p/seeing", true, true, ModeBuild, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			promptText, tools := spriteExposure(t, tc.role, tc.model, tc.art, tc.enabled, tc.mode)
			if tools[runtime.ReloArtTool] != tc.offered {
				t.Fatalf("relo_art offered = %v, want %v", tools[runtime.ReloArtTool], tc.offered)
			}
			if got := strings.Contains(promptText, "relo_art makes pictures"); got != tc.offered {
				t.Fatalf("guidance present = %v, want %v", got, tc.offered)
			}
		})
	}
}

func TestArtProvenancePathCannotEscapeOrCollide(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	a, _ := f.c.Agents.Get("artist")
	// Not a generated picture path, or a malformed one: no record, no error.
	for _, in := range []string{"art/raw/iron.png", ".relo/art/raw/j1", ".relo/art/raw/j1/x/2.png", ".relo/art/raw/j1/zero.png", ".relo/art/raw/j9/1.png"} {
		if got := f.c.recordProvenance(a, f.session, in, []spriteWrite{{Path: "Textures/A.png"}}); got != "" {
			t.Errorf("%s recorded %s", in, got)
		}
	}
	_ = sprite.MaxOps
}

func TestArtPromptStatesEachReferenceRoleAndWeight(t *testing.T) {
	prompt, ids := agentmsg.ArtPromptWithRefs("  an ingot  ", []agentmsg.ArtRef{{ID: "r_1", Role: "style", Weight: 0.5}, {ID: "r_2"}, {ID: "r_3", Role: "composition"}})
	want := "an ingot\nReferences: image 1: style reference (weight 0.5); image 2: reference; image 3: composition reference."
	if prompt != want || strings.Join(ids, ",") != "r_1,r_2,r_3" {
		t.Fatalf("prompt = %q ids = %v", prompt, ids)
	}
	if p, ids := agentmsg.ArtPromptWithRefs("an ingot", nil); p != "an ingot" || ids != nil {
		t.Fatalf("without refs prompt = %q ids = %v", p, ids)
	}
}

func TestArtFailedRefsJobSaysStopAndReport(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	writeDiscs(t, filepath.Join(f.session, "art", "ref", "style.png"), 1)
	refs := imageMetadata(t, agentmsg.ImageResult{Text: f.mustCall(t, agentmsg.ArtRequest{Action: "refs", Sources: []agentmsg.ArtSource{{Path: "art/ref/style.png"}}}).Text})
	refID := refs["refs"].([]any)[0].(map[string]any)["id"].(string)

	f.api.mu.Lock()
	f.api.fail = 401
	f.api.mu.Unlock()
	id := jobIDOf(t, f.mustCall(t, agentmsg.ArtRequest{Action: "generate", Prompt: "an ingot", Refs: []agentmsg.ArtRef{{ID: refID, Role: "style"}}}))
	f.waitState(t, id, art.StateFailed)
	meta := imageMetadata(t, f.mustCall(t, agentmsg.ArtRequest{Action: "wait", Job: id}))
	if next, _ := meta["next"].(string); !strings.Contains(next, "Stop and report") || !strings.Contains(next, "never retry it text-only") {
		t.Fatalf("next = %q", next)
	}
}

func TestArtThirdIdenticalPromptIsRefused(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	f.api.mu.Lock()
	f.api.fail = 401
	f.api.mu.Unlock()
	gen := agentmsg.ArtRequest{Action: "generate", Prompt: "an ingot, flat solid magenta background"}
	for i := 1; i <= maxIdenticalArtPrompts; i++ {
		jobIDOf(t, f.mustCall(t, gen))
	}
	if _, err := f.call(gen); err == nil || !strings.Contains(err.Error(), "already sent") {
		t.Fatalf("third identical prompt: err = %v", err)
	}
	if _, err := f.call(agentmsg.ArtRequest{Action: "generate", Prompt: "a different ingot"}); err != nil {
		t.Fatalf("different prompt refused: %v", err)
	}
}

func TestArtWaitAndRefsAreLimitedToTheSessionsOwnJobs(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	id := f.readyJob(t, agentmsg.ArtRequest{})
	m := f.manager(t)
	own, _ := m.Get(id)
	for _, owner := range []string{"someone-else", ""} {
		if _, err := m.Store().Update(id, func(j *art.Job) { j.SessionID = owner }); err != nil {
			t.Fatal(err)
		}
		if _, err := f.call(agentmsg.ArtRequest{Action: "wait", Job: id}); err == nil || !strings.Contains(err.Error(), "belongs to another session") {
			t.Fatalf("waited on job with owner %q: %v", owner, err)
		}
		if _, err := f.call(agentmsg.ArtRequest{Action: "refs", Sources: []agentmsg.ArtSource{{Job: id, Index: 1}}}); err == nil || !strings.Contains(err.Error(), "belongs to another session") {
			t.Fatalf("stored a picture of job with owner %q: %v", owner, err)
		}
		if _, err := f.call(agentmsg.ArtRequest{Action: "cancel", Job: id}); err == nil || !strings.Contains(err.Error(), "belongs to another session") {
			t.Fatalf("cancelled job with owner %q: %v", owner, err)
		}
	}
	// Back to the caller's own session: the same job can be waited on again.
	if _, err := m.Store().Update(id, func(j *art.Job) { j.SessionID = own.SessionID }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(agentmsg.ArtRequest{Action: "wait", Job: id}); err != nil {
		t.Fatalf("own job refused: %v", err)
	}
}

func TestArtJobsHidesOwnerlessJobs(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	id := f.readyJob(t, agentmsg.ArtRequest{Prompt: "orphan"})
	if _, err := f.manager(t).Store().Update(id, func(j *art.Job) { j.SessionID = "" }); err != nil {
		t.Fatal(err)
	}
	meta := imageMetadata(t, agentmsg.ImageResult{Text: f.mustCall(t, agentmsg.ArtRequest{Action: "jobs"}).Text})
	if jobs, _ := meta["jobs"].([]any); len(jobs) != 0 {
		t.Fatalf("jobs = %v", jobs)
	}
}

func TestArtConcurrentIdenticalPromptsStayWithinTheCap(t *testing.T) {
	f := newArtFixture(t, agent.Worker, "p/seeing", true, nil)
	f.api.mu.Lock()
	f.api.fail = 401
	f.api.mu.Unlock()
	gen := agentmsg.ArtRequest{Action: "generate", Prompt: "an ingot, racing the cap"}
	const callers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.call(gen)
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !strings.Contains(err.Error(), "already sent") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != maxIdenticalArtPrompts {
		t.Fatalf("%d concurrent identical calls started jobs, want %d", ok, maxIdenticalArtPrompts)
	}
	jobs := f.manager(t).List(0)
	if len(jobs) != maxIdenticalArtPrompts {
		t.Fatalf("%d jobs recorded, want %d", len(jobs), maxIdenticalArtPrompts)
	}
}

// A Coord that was never sent the art settings must not read the zero value as "no limits": 0 is the user turning a limit
// off, so a limit nobody set has to be the default.
func TestArtManagerBeforeAnySettingsArriveKeepsTheDefaultLimits(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	t.Cleanup(c.CloseArt)
	m, err := c.artManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := m.Budget("s1")
	if b.BudgetUnlimited || b.JobsUnlimited || b.LimitUSD != art.DefaultBudgetUSD || b.SessionJobLimit != art.DefaultJobLimit {
		t.Fatalf("budget before settings = %+v, want the defaults %v and %d", b, art.DefaultBudgetUSD, art.DefaultJobLimit)
	}
}
