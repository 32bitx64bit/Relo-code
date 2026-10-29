package art

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestArtStatusCapabilitiesAndRecoveredParameters(t *testing.T) {
	var received = make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			fmt.Fprint(w, `{"data":[{"id":"`+DefaultModel+`","supported_parameters":{"aspect_ratio":{"type":"enum","values":["3:2","16:9"]},"resolution":{"type":"enum","values":["1K","2K"]},"seed":{"type":"boolean"}}}]}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		received <- body
		fmt.Fprint(w, `{"data":[{"b64_json":"YQ==","media_type":"image/svg+xml"}]}`)
	}))
	defer server.Close()
	mod := func(s *Settings) { s.Provider = "openrouter" }
	r := newRig(t, mod)
	r.endpoint = func(string) (Endpoint, error) { return Endpoint{BaseURL: server.URL, Provider: "openrouter"}, nil }
	status := r.m.Status("s1")
	params, ok := status.SupportedParameters.([]string)
	if !ok || !contains(params, "seed") {
		t.Fatalf("status capabilities=%v", status.SupportedParameters)
	}
	r.m.hooks.Approve = func(ctx context.Context, a Approval) (string, error) { <-ctx.Done(); return "reject", ctx.Err() }
	seed := int64(0)
	j := r.generate(GenerateInput{AspectRatio: "3:2", Resolution: "2K", Seed: &seed, Count: 1})
	r.m.Close()
	r.open(mod)
	t.Cleanup(r.m.Close)
	r.endpoint = func(string) (Endpoint, error) { return Endpoint{BaseURL: server.URL, Provider: "openrouter"}, nil }
	r.m.Recover()
	done := r.finish(j.ID)
	if done.State != StateFailed || !strings.Contains(done.Message, "image/svg+xml") || done.Seed == nil || *done.Seed != 0 || done.AspectRatio != "3:2" || done.Resolution != "2K" {
		t.Fatalf("recovered job=%+v", done)
	}
	body := <-received
	if body["aspect_ratio"] != "3:2" || body["resolution"] != "2K" || body["seed"] != float64(0) {
		t.Fatal("recovered parameters not sent")
	}
	r.endpoint = func(string) (Endpoint, error) {
		return Endpoint{BaseURL: "http://127.0.0.1:1", Provider: "openrouter"}, nil
	}
	if got := r.m.Status("s1").SupportedParameters; got != "unknown" {
		t.Fatalf("failed listing capabilities=%v", got)
	}
}

func TestArtNewParameterValidation(t *testing.T) {
	r := newRig(t, nil)
	negative := int64(-1)
	zero := int64(0)
	for _, tc := range []struct {
		input GenerateInput
		want  string
	}{
		{GenerateInput{Prompt: "p", AspectRatio: "1/1"}, "aspect_ratio"},
		{GenerateInput{Prompt: "p", Resolution: "too-long-resolution"}, "resolution"},
		{GenerateInput{Prompt: "p", Seed: &negative}, "seed"},
		{GenerateInput{Prompt: "p", Seed: &zero}, "only supported for OpenRouter"},
	} {
		_, err := r.m.Generate(context.Background(), tc.input)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("expected %s: %v", tc.want, err)
		}
	}
}

// rig is a manager wired to a fake API.
type rig struct {
	t    *testing.T
	api  *fakeAPI
	m    *Manager
	root string

	mu       sync.Mutex
	asked    []Approval
	answer   string
	endpoint func(provider string) (Endpoint, error)
}

func newRig(t *testing.T, mod func(*Settings)) *rig {
	t.Helper()
	r := &rig{t: t, api: newFakeAPI(t), root: filepath.Join(t.TempDir(), ".relo", "art"), answer: "once"}
	r.open(mod)
	t.Cleanup(r.m.Close)
	return r
}

func (r *rig) open(mod func(*Settings)) {
	r.endpoint = func(string) (Endpoint, error) { return r.api.endpoint(), nil }
	m, err := NewManager(r.root, Hooks{
		Endpoint: func(p string) (Endpoint, error) { return r.endpoint(p) },
		Approve: func(ctx context.Context, a Approval) (string, error) {
			r.mu.Lock()
			r.asked = append(r.asked, a)
			answer := r.answer
			r.mu.Unlock()
			return answer, nil
		},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	set := Settings{Enabled: true, Count: 2, Quality: "medium", Size: "1024x1024", BudgetUSD: 3, SessionJobLimit: 8}
	if mod != nil {
		mod(&set)
	}
	m.SetSettings(set)
	r.m = m
}

func (r *rig) asks() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asked)
}

func (r *rig) generate(in GenerateInput) Job {
	r.t.Helper()
	if in.SessionID == "" {
		in.SessionID = "s1"
	}
	if in.Prompt == "" {
		in.Prompt = "an iron ingot"
	}
	j, err := r.m.Generate(context.Background(), in)
	if err != nil {
		r.t.Fatal(err)
	}
	return j
}

func (r *rig) finish(id string) Job {
	r.t.Helper()
	j, timedOut, err := r.m.Wait(context.Background(), id, 5*time.Second)
	if err != nil || timedOut {
		r.t.Fatalf("job %s did not finish: %+v timedOut=%v err=%v", id, j, timedOut, err)
	}
	return j
}

func TestAJobMakesPicturesAndRecordsWhatTheyCost(t *testing.T) {
	r := newRig(t, nil)
	j := r.finish(r.generate(GenerateInput{}).ID)
	if j.State != StateReady || len(j.Candidates) != 2 || j.Count != 2 || j.Model != DefaultModel {
		t.Fatalf("job = %+v", j)
	}
	if c := r.api.last(); c.Path != "/v1/images/generations" || c.N != 2 || c.Fields["quality"] != "medium" || c.Fields["size"] != "1024x1024" {
		t.Fatalf("the call = %+v", c)
	}
	// 100 text, 200 image in and 1056 out at $5, $8 and $30 per million.
	if j.CostUSD == nil || !near(*j.CostUSD, 0.0338) || j.CostFromEstimate {
		t.Fatalf("cost = %v (from estimate %v)", j.CostUSD, j.CostFromEstimate)
	}
	if j.EstimateUSD < *j.CostUSD {
		t.Fatalf("the estimate %v is below what it cost %v", j.EstimateUSD, *j.CostUSD)
	}
	for _, c := range j.Candidates {
		if _, err := os.Stat(filepath.Join(j.Dir, c.File)); err != nil {
			t.Fatal(err)
		}
	}
	if got := RelPath(j.ID, 2); got != ".relo/art/raw/"+j.ID+"/2.png" {
		t.Fatalf("relpath = %s", got)
	}
	if b := r.m.Budget("s1"); !near(b.SpentUSD, 0.0338) || b.SessionJobs != 1 || !near(b.RemainingUSD, 3-0.0338) {
		t.Fatalf("budget = %+v", b)
	}
	if b := r.m.Budget("another-session"); b.SpentUSD != 0 || !near(b.ProjectUSD, 0.0338) {
		t.Fatalf("another session starts at nothing, the project keeps the total: %+v", b)
	}
	if prov, ok := r.m.ProvenanceFor(j.ID, 1); !ok || prov.Prompt != "an iron ingot" || prov.SHA256 == "" || prov.Model != DefaultModel {
		t.Fatalf("provenance = %+v %v", prov, ok)
	}
	if data, err := r.m.CandidateData(j.ID, 1); err != nil || len(data) == 0 {
		t.Fatalf("candidate data: %v", err)
	}
}

func TestReferencesTurnTheJobIntoAnEdit(t *testing.T) {
	r := newRig(t, nil)
	ref, err := r.m.Store().AddRef(discPNG(t, 50, 50, color.NRGBA{255, 255, 255, 255}))
	if err != nil {
		t.Fatal(err)
	}
	j := r.finish(r.generate(GenerateInput{Refs: []string{ref.ID}, Count: 1, Quality: "low", Background: "transparent"}).ID)
	c := r.api.last()
	if j.State != StateReady || c.Path != "/v1/images/edits" || c.Images != 1 || c.Fields["quality"] != "low" || c.Fields["background"] != "transparent" {
		t.Fatalf("state %s, call %+v", j.State, c)
	}
	if len(j.Refs) != 1 || j.Refs[0].ID != ref.ID {
		t.Fatalf("refs = %+v", j.Refs)
	}
	if prov, _ := r.m.ProvenanceFor(j.ID, 1); len(prov.Refs) != 1 || prov.Refs[0] != ref.ID {
		t.Fatalf("provenance refs = %+v", prov.Refs)
	}
}

func TestTheUserIsAskedOnceAYesForTheSessionCoversTheRest(t *testing.T) {
	r := newRig(t, nil)
	r.answer = "always"
	r.finish(r.generate(GenerateInput{Count: 1}).ID)
	r.finish(r.generate(GenerateInput{Count: 1}).ID)
	if r.asks() != 1 {
		t.Fatalf("asked %d times, want once", r.asks())
	}
	a := r.asked[0]
	for _, want := range []string{"an iron ingot", "medium quality", DefaultModel, "budget"} {
		if !strings.Contains(a.Detail, want) {
			t.Errorf("the question does not say %q:\n%s", want, a.Detail)
		}
	}
	if !strings.Contains(a.Title, "$") || a.USD <= 0 {
		t.Fatalf("the question does not show the price: %+v", a)
	}
	// Another session has not been approved.
	r.finish(r.generate(GenerateInput{Count: 1, SessionID: "s2"}).ID)
	if r.asks() != 2 {
		t.Fatalf("a new session should be asked: %d", r.asks())
	}
}

func TestApprovalEachAsksEveryTime(t *testing.T) {
	r := newRig(t, func(s *Settings) { s.Approval = "each" })
	r.answer = "always"
	r.finish(r.generate(GenerateInput{Count: 1}).ID)
	r.finish(r.generate(GenerateInput{Count: 1}).ID)
	if r.asks() != 2 {
		t.Fatalf("asked %d times, want each time", r.asks())
	}
}

func TestDecliningCostsNothingAndNeverCallsTheAPI(t *testing.T) {
	r := newRig(t, nil)
	r.answer = "reject"
	j := r.finish(r.generate(GenerateInput{}).ID)
	if j.State != StateCancelled || j.CostUSD == nil || *j.CostUSD != 0 || r.api.count() != 0 || !strings.Contains(j.Message, "declined") {
		t.Fatalf("job = %+v, calls %d", j, r.api.count())
	}
	if b := r.m.Budget("s1"); b.SpentUSD != 0 || b.SessionJobs != 0 {
		t.Fatalf("a declined job still counts: %+v", b)
	}
}

func TestYoloSkipsTheQuestionButNotTheBudget(t *testing.T) {
	r := newRig(t, func(s *Settings) { s.BudgetUSD = 0.1 })
	r.m.hooks.Yolo = func() bool { return true }
	r.finish(r.generate(GenerateInput{Count: 1, Quality: "low"}).ID)
	if r.asks() != 0 {
		t.Fatal("Yolo should not ask")
	}
	_, err := r.m.Generate(context.Background(), GenerateInput{SessionID: "s1", Prompt: "p", Count: 4, Quality: "high"})
	if err == nil || !strings.Contains(err.Error(), "not enough budget") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheBudgetCountsRealCostAfterwardsSoMoreFits(t *testing.T) {
	// Two medium pictures are estimated at about $0.11, over a $0.12 budget once the first job is counted at its
	// estimate, but each really costs about $0.034.
	r := newRig(t, func(s *Settings) { s.BudgetUSD = 0.12 })
	first := r.generate(GenerateInput{Count: 1})
	if _, err := r.m.Generate(context.Background(), GenerateInput{SessionID: "s1", Prompt: "p", Count: 2}); err == nil {
		t.Fatal("a job that cannot fit beside the one in flight was accepted")
	}
	r.finish(first.ID)
	if _, err := r.m.Generate(context.Background(), GenerateInput{SessionID: "s1", Prompt: "p", Count: 2, Quality: "low"}); err != nil {
		t.Fatalf("after the first job finished cheaply, a low-quality pair should fit: %v", err)
	}
}

func TestTheSessionJobLimitStopsTheLoop(t *testing.T) {
	r := newRig(t, func(s *Settings) { s.SessionJobLimit = 2 })
	r.finish(r.generate(GenerateInput{Count: 1}).ID)
	r.finish(r.generate(GenerateInput{Count: 1}).ID)
	_, err := r.m.Generate(context.Background(), GenerateInput{SessionID: "s1", Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "2 of the 2 jobs") {
		t.Fatalf("err = %v", err)
	}
}

func TestBadRequestsAreRefusedBeforeAnythingIsMade(t *testing.T) {
	r := newRig(t, nil)
	ref, _ := r.m.Store().AddRef(discPNG(t, 40, 40, color.NRGBA{255, 255, 255, 255}))
	for name, in := range map[string]GenerateInput{
		"empty prompt":   {Prompt: "   "},
		"long prompt":    {Prompt: strings.Repeat("x", maxPromptBytes+1)},
		"quality":        {Prompt: "p", Quality: "ultra"},
		"size":           {Prompt: "p", Size: "99x99"},
		"background":     {Prompt: "p", Background: "see-through"},
		"too many":       {Prompt: "p", Count: 5},
		"unknown ref":    {Prompt: "p", Refs: []string{"r_nope"}},
		"repeated ref":   {Prompt: "p", Refs: []string{ref.ID, ref.ID}},
		"too many refs":  {Prompt: "p", Refs: []string{"r_1", "r_2", "r_3", "r_4", "r_5", "r_6", "r_7"}},
		"path as an id":  {Prompt: "p", Refs: []string{"../../etc/passwd"}},
		"negative count": {Prompt: "p", Count: -1},
	} {
		in.SessionID = "s1"
		if _, err := r.m.Generate(context.Background(), in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(r.m.List(0)) != 0 || r.api.count() != 0 {
		t.Fatal("a refused request left a job or reached the API")
	}
}

func TestSwitchedOffRefusesEverything(t *testing.T) {
	r := newRig(t, func(s *Settings) { s.Enabled = false })
	if _, err := r.m.Generate(context.Background(), GenerateInput{SessionID: "s1", Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "switched off") {
		t.Fatalf("err = %v", err)
	}
}

func TestAMissingProviderIsReportedBeforeAJobExists(t *testing.T) {
	r := newRig(t, nil)
	r.endpoint = func(p string) (Endpoint, error) {
		return Endpoint{}, errors.New("no OpenAI-style provider is configured")
	}
	if _, err := r.m.Generate(context.Background(), GenerateInput{SessionID: "s1", Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "no OpenAI-style provider") {
		t.Fatalf("err = %v", err)
	}
	if len(r.m.List(0)) != 0 {
		t.Fatal("a job was made with nothing to run it")
	}
	if st := r.m.Status("s1"); st.Ready || st.Problem == "" || !st.Enabled {
		t.Fatalf("status = %+v", st)
	}
}

func TestTheProviderIsPassedThrough(t *testing.T) {
	var got atomic.Value
	r := newRig(t, func(s *Settings) { s.Provider = " openai-main " })
	r.endpoint = func(p string) (Endpoint, error) { got.Store(p); return r.api.endpoint(), nil }
	r.finish(r.generate(GenerateInput{Count: 1}).ID)
	if got.Load() != "openai-main" {
		t.Fatalf("provider = %v", got.Load())
	}
}

func TestAProviderRefusalIsFreeAndAFailureTheAgentCanRead(t *testing.T) {
	r := newRig(t, nil)
	r.api.setHandler(func(w http.ResponseWriter, req *http.Request, c call) {
		apiError(w, 400, "Invalid value: 'transparent' is not supported for this model. Key "+testKey)
	})
	j := r.finish(r.generate(GenerateInput{Background: "transparent"}).ID)
	if j.State != StateFailed || j.CostUSD == nil || *j.CostUSD != 0 || !strings.Contains(j.Message, "transparent") {
		t.Fatalf("job = %+v", j)
	}
	if strings.Contains(j.Message, "SECRET") {
		t.Fatalf("the key reached the job: %q", j.Message)
	}
	if b := r.m.Budget("s1"); b.SpentUSD != 0 {
		t.Fatalf("a refused call was charged: %+v", b)
	}
}

func TestACallThatWasCutOffStillCountsAtItsEstimate(t *testing.T) {
	r := newRig(t, nil)
	r.api.setHandler(func(w http.ResponseWriter, req *http.Request, c call) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	})
	j := r.finish(r.generate(GenerateInput{}).ID)
	if j.State != StateFailed || j.CostUSD != nil || !strings.Contains(j.Message, "billed") {
		t.Fatalf("job = %+v", j)
	}
	if b := r.m.Budget("s1"); !near(b.SpentUSD, j.EstimateUSD) {
		t.Fatalf("a cut-off call should count at its estimate %v, got %+v", j.EstimateUSD, b)
	}
}

func TestWithoutUsageTheEstimateStandsInForTheCost(t *testing.T) {
	r := newRig(t, nil)
	r.api.setHandler(func(w http.ResponseWriter, req *http.Request, c call) { r.api.respond(w, c.N, false) })
	j := r.finish(r.generate(GenerateInput{}).ID)
	if j.State != StateReady || j.CostUSD == nil || !j.CostFromEstimate || !near(*j.CostUSD, j.EstimateUSD) {
		t.Fatalf("job = %+v", j)
	}
}

func TestCancellingARunningJobStopsTheCallAndKeepsItsEstimate(t *testing.T) {
	r := newRig(t, nil)
	started := make(chan struct{})
	var once sync.Once
	r.api.setHandler(func(w http.ResponseWriter, req *http.Request, c call) {
		once.Do(func() { close(started) })
		<-req.Context().Done()
	})
	j := r.generate(GenerateInput{})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never started")
	}
	out, err := r.m.Cancel(j.ID)
	if err != nil || out.State != StateCancelled {
		t.Fatalf("cancel = %+v %v", out, err)
	}
	final := r.finish(j.ID)
	if final.State != StateCancelled || final.CostUSD != nil {
		t.Fatalf("a call that was out when it was cancelled may still be billed: %+v", final)
	}
	if _, err := r.m.Cancel(j.ID); err == nil {
		t.Fatal("cancelled a job twice")
	}
	if _, err := r.m.Cancel("j99"); err == nil {
		t.Fatal("cancelled a job that does not exist")
	}
}

func TestCancellingAJobWaitingForApprovalCostsNothing(t *testing.T) {
	r := newRig(t, nil)
	block := make(chan struct{})
	r.m.hooks.Approve = func(ctx context.Context, a Approval) (string, error) {
		select {
		case <-block:
			return "once", nil
		case <-ctx.Done():
			return "reject", ctx.Err()
		}
	}
	j := r.generate(GenerateInput{})
	out, err := r.m.Cancel(j.ID)
	if err != nil || out.State != StateCancelled || out.CostUSD == nil || *out.CostUSD != 0 {
		t.Fatalf("cancel = %+v %v", out, err)
	}
	time.Sleep(50 * time.Millisecond)
	if r.api.count() != 0 {
		t.Fatal("a cancelled job reached the API")
	}
	if cur, _ := r.m.Get(j.ID); cur.State != StateCancelled {
		t.Fatalf("state = %s", cur.State)
	}
	close(block)
}

func TestAJobWaitingForApprovalSurvivesAShutdown(t *testing.T) {
	r := newRig(t, nil)
	r.m.hooks.Approve = func(ctx context.Context, a Approval) (string, error) { <-ctx.Done(); return "reject", ctx.Err() }
	j := r.generate(GenerateInput{})
	time.Sleep(30 * time.Millisecond)
	r.m.Close()
	if cur, _ := r.m.Get(j.ID); cur.State != StateQueued || cur.Approval != "pending" {
		t.Fatalf("the job should wait for the restart: %+v", cur)
	}

	r.open(nil) // a new run of Relo on the same folder
	t.Cleanup(r.m.Close)
	r.m.Recover()
	done := r.finish(j.ID)
	if done.State != StateReady || r.asks() != 1 {
		t.Fatalf("after the restart: %+v, asked %d", done, r.asks())
	}
}

func TestAJobThatWasRunningWhenReloStoppedIsFailedAndKeepsCounting(t *testing.T) {
	r := newRig(t, nil)
	j, err := r.m.Store().Create(Job{SessionID: "s1", State: StateRunning, Prompt: "p", EstimateUSD: 0.2, Approval: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	r.m.Recover()
	got, _ := r.m.Get(j.ID)
	if got.State != StateFailed || got.CostUSD != nil || !strings.Contains(got.Message, "billed") || r.api.count() != 0 {
		t.Fatalf("job = %+v, calls %d", got, r.api.count())
	}
	if b := r.m.Budget("s1"); !near(b.SpentUSD, 0.2) {
		t.Fatalf("budget = %+v", b)
	}
}

func TestTheKeyNeverReachesDiskOrWhatAgentsSee(t *testing.T) {
	r := newRig(t, nil)
	j := r.finish(r.generate(GenerateInput{}).ID)
	bad := r.finish(func() string {
		r.api.setHandler(func(w http.ResponseWriter, req *http.Request, c call) {
			apiError(w, 401, "Incorrect API key provided: "+testKey)
		})
		return r.generate(GenerateInput{}).ID
	}())
	seen := []string{bad.Message}
	if b, err := json.Marshal(r.m.Status("s1")); err == nil {
		seen = append(seen, string(b))
	}
	if b, err := json.Marshal(j); err == nil {
		seen = append(seen, string(b))
	}
	for _, s := range seen {
		if strings.Contains(s, "SECRET") {
			t.Errorf("the key leaked: %s", s)
		}
	}
	filepath.Walk(r.root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".json") {
			if data, _ := os.ReadFile(path); strings.Contains(string(data), "SECRET") {
				t.Errorf("the key is on disk in %s", path)
			}
		}
		return nil
	})
}

func TestWaitReturnsWhileAJobIsStillWorking(t *testing.T) {
	r := newRig(t, nil)
	release := make(chan struct{})
	r.api.setHandler(func(w http.ResponseWriter, req *http.Request, c call) { <-release; r.api.respond(w, c.N, true) })
	j := r.generate(GenerateInput{Count: 1})
	got, timedOut, err := r.m.Wait(context.Background(), j.ID, 50*time.Millisecond)
	if err != nil || !timedOut || !got.Active() {
		t.Fatalf("wait = %+v timedOut=%v err=%v", got, timedOut, err)
	}
	close(release)
	if done := r.finish(j.ID); done.State != StateReady {
		t.Fatalf("job = %+v", done)
	}
	if _, _, err := r.m.Wait(context.Background(), "j99", time.Millisecond); err == nil {
		t.Fatal("waited for a job that does not exist")
	}
}

func TestSettingsAreClampedToWhatIsSafe(t *testing.T) {
	got := normalize(Settings{Quality: "ultra", Size: "1x1", Count: 99, BudgetUSD: 1e9, SessionJobLimit: 1 << 30, Approval: "whatever"})
	if got.Model != DefaultModel || got.Quality != "medium" || got.Size != "1024x1024" || got.Count != 2 || got.BudgetUSD != maxBudgetUSD || got.SessionJobLimit != maxSessionJobLimit || got.Approval != "session" {
		t.Fatalf("settings = %+v", got)
	}
	// A negative limit is a mistake and gets the default; zero is the user asking for no limit.
	if got := normalize(Settings{BudgetUSD: -1, SessionJobLimit: -3}); got.BudgetUSD != DefaultBudgetUSD || got.SessionJobLimit != DefaultJobLimit {
		t.Fatalf("negative limits should default, got %+v", got)
	}
	if got := normalize(Settings{}); got.BudgetUSD != 0 || got.SessionJobLimit != 0 {
		t.Fatalf("zero should stay zero (unlimited), got %+v", got)
	}
}

func TestUnlimitedMeansNoBudgetAndNoJobCap(t *testing.T) {
	r := newRig(t, func(s *Settings) { s.BudgetUSD, s.SessionJobLimit = 0, 0 })
	// Far more than the default job cap, and a single job priced far over the default budget.
	for i := 0; i < DefaultJobLimit+3; i++ {
		r.finish(r.generate(GenerateInput{Count: 1, Quality: "low"}).ID)
	}
	r.finish(r.generate(GenerateInput{Count: 4, Quality: "high"}).ID)
	b := r.m.Budget("s1")
	if !b.BudgetUnlimited || !b.JobsUnlimited || b.RemainingUSD != 0 || b.LimitUSD != 0 || b.SessionJobs != DefaultJobLimit+4 {
		t.Fatalf("budget = %+v", b)
	}
	raw, _ := json.Marshal(b)
	for _, bad := range []string{"limitUsd", "remainingUsd", "sessionJobLimit"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("an agent would read %q as a limit of zero: %s", bad, raw)
		}
	}
	if !strings.Contains(string(raw), `"budgetUnlimited":true`) || !strings.Contains(string(raw), `"jobsUnlimited":true`) {
		t.Fatalf("the unlimited flags are missing: %s", raw)
	}
	// What was spent is still counted and shown.
	if b.SpentUSD <= 0 || b.ProjectUSD != b.SpentUSD {
		t.Fatalf("spend should still be tracked: %+v", b)
	}
}

// A limit that is set must always show, also when nothing is left, or an agent could read the gap as "no limit".
func TestASetLimitAlwaysShowsEvenWhenSpent(t *testing.T) {
	raw, err := json.Marshal(Budget{SpentUSD: 3, LimitUSD: 3, RemainingUSD: 0, SessionJobs: 8, SessionJobLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"limitUsd":3`, `"remainingUsd":0`, `"sessionJobLimit":8`, `"budgetUnlimited":false`, `"jobsUnlimited":false`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %s in %s", want, raw)
		}
	}
}

func TestTheQuestionSaysWhenThereIsNoBudgetLimit(t *testing.T) {
	r := newRig(t, func(s *Settings) { s.BudgetUSD = 0 })
	r.finish(r.generate(GenerateInput{Count: 1}).ID)
	if len(r.asked) != 1 || !strings.Contains(r.asked[0].Detail, "no budget limit") {
		t.Fatalf("question = %+v", r.asked)
	}
}

func TestListIsNewestFirst(t *testing.T) {
	r := newRig(t, nil)
	a := r.finish(r.generate(GenerateInput{Count: 1}).ID)
	b := r.finish(r.generate(GenerateInput{Count: 1}).ID)
	list := r.m.List(1)
	if len(list) != 1 || list[0].ID != b.ID || a.ID == b.ID {
		t.Fatalf("list = %+v", list)
	}
}

func TestAJobKeepsItsOwningSessionAcrossReopen(t *testing.T) {
	r := newRig(t, nil)
	j := r.finish(r.generate(GenerateInput{SessionID: "owner-1", Count: 1}).ID)
	r.m.Close()
	r.open(nil)
	t.Cleanup(r.m.Close)
	got, ok := r.m.Get(j.ID)
	if !ok || got.SessionID != "owner-1" {
		t.Fatalf("reopened job = %+v ok=%v, want SessionID owner-1", got, ok)
	}
}

// A limit nobody set (a fresh manager, or a negative value) is the default of 25 jobs, and the 26th is refused. Only an
// explicit 0 lifts the cap (TestUnlimitedMeansNoBudgetAndNoJobCap).
func TestUnsetOrNegativeJobLimitIsTwentyFive(t *testing.T) {
	for name, mod := range map[string]func(*Settings){
		"unset": func(s *Settings) {
			*s = normalize(Settings{Enabled: true, BudgetUSD: DefaultBudgetUSD, SessionJobLimit: DefaultJobLimit})
		},
		"negative": func(s *Settings) { s.BudgetUSD, s.SessionJobLimit = 1000, -1 },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, mod)
			if b := r.m.Budget("s1"); b.JobsUnlimited || b.SessionJobLimit != 25 {
				t.Fatalf("budget = %+v", b)
			}
			for i := 0; i < 25; i++ {
				r.finish(r.generate(GenerateInput{Count: 1, Quality: "low"}).ID)
			}
			if _, err := r.m.Generate(context.Background(), GenerateInput{SessionID: "s1", Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "25 of the 25 jobs") {
				t.Fatalf("the 26th job should be refused, err = %v", err)
			}
		})
	}
}
