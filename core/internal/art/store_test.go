package art

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".relo", "art")
	s, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func TestStoreNumbersJobsAndSurvivesARestart(t *testing.T) {
	s, root := newStore(t)
	a, err := s.Create(Job{SessionID: "s1", Prompt: "iron", Size: "1024x1024", Refs: []Ref{{ID: "ref_abc"}}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Create(Job{SessionID: "s1", Prompt: "gold"})
	if a.ID != "j1" || b.ID != "j2" || a.State != StateQueued || a.Dir != filepath.Join(root, "raw", "j1") {
		t.Fatalf("jobs = %+v %+v", a, b)
	}
	if info, err := os.Stat(a.Dir); err != nil || !info.IsDir() {
		t.Fatal("the job folder was not made")
	}
	if _, err := s.Update("j1", func(j *Job) {
		j.State = StateReady
		j.Candidates = append(j.Candidates, Candidate{Index: 1, File: "1.png", Width: 64, Height: 64})
	}); err != nil {
		t.Fatal(err)
	}

	moved := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Rename(filepath.Dir(filepath.Dir(root)), moved); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(filepath.Join(moved, ".relo", "art"))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get("j1")
	if !ok || got.State != StateReady || len(got.Candidates) != 1 || got.Size != "1024x1024" {
		t.Fatalf("reloaded job = %+v", got)
	}
	if got.Dir != filepath.Join(moved, ".relo", "art", "raw", "j1") || got.Refs[0].File != filepath.Join(moved, ".relo", "art", "refs", "ref_abc.png") {
		t.Fatalf("paths were not re-rooted: %q %q", got.Dir, got.Refs[0].File)
	}
	c, _ := reopened.Create(Job{})
	if c.ID != "j3" {
		t.Fatalf("next id after a restart = %s", c.ID)
	}
	if list := reopened.List(); len(list) != 3 || list[0].ID != "j1" || list[2].ID != "j3" {
		t.Fatalf("list = %+v", list)
	}
}

func TestStoreHandsOutCopies(t *testing.T) {
	s, _ := newStore(t)
	cost := 0.5
	j, _ := s.Create(Job{CostUSD: &cost, Refs: []Ref{{ID: "r"}}})
	*j.CostUSD = 99
	j.Refs[0].ID = "changed"
	got, _ := s.Get(j.ID)
	if *got.CostUSD != 0.5 || got.Refs[0].ID != "r" {
		t.Fatalf("a caller changed the stored job: %+v", got)
	}
}

func TestStoreUpdateIsAtomicAndNotifies(t *testing.T) {
	s, _ := newStore(t)
	var changes []string
	var mu sync.Mutex
	s.OnChange(func(j Job) { mu.Lock(); changes = append(changes, j.ID+":"+j.State); mu.Unlock() })
	j, _ := s.Create(Job{})
	watch, stop := s.Watch(j.ID)
	defer stop()
	if _, err := s.Update(j.ID, func(x *Job) { x.State = StateRunning; x.ID = "hijack"; x.Dir = "/elsewhere" }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-watch:
	case <-time.After(time.Second):
		t.Fatal("a watcher was not told")
	}
	got, _ := s.Get(j.ID)
	if got.ID != j.ID || got.Dir != j.Dir || got.State != StateRunning || !got.UpdatedAt.After(j.CreatedAt.Add(-time.Second)) {
		t.Fatalf("job = %+v", got)
	}
	if _, err := s.Update("nope", func(*Job) {}); err == nil {
		t.Fatal("updated a job that does not exist")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(changes) != 2 || changes[0] != "j1:queued" || changes[1] != "j1:running" {
		t.Fatalf("changes = %v", changes)
	}
}

func TestStoreSkipsJobFilesItCannotTrust(t *testing.T) {
	s, root := newStore(t)
	s.Create(Job{})
	for name, body := range map[string]string{"j7": "{not json", "j8": `{"id":"someone-else","state":"ready"}`} {
		dir := filepath.Join(root, "jobs", name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "job.json"), []byte(body), 0o644)
	}
	reopened, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.List()) != 1 {
		t.Fatalf("loaded %d jobs, want only the good one", len(reopened.List()))
	}
}

func TestStoreCancelsJobsFromAnEarlierVersion(t *testing.T) {
	s, root := newStore(t)
	s.Create(Job{})
	dir := filepath.Join(root, "jobs", "j9")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "job.json"), []byte(`{"id":"j9","state":"awaiting_user","prompt":"x --ar 1:1"}`), 0o644)
	reopened, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	old, ok := reopened.Get("j9")
	if !ok || old.State != StateCancelled || old.Active() || old.Message == "" {
		t.Fatalf("an old job should be set aside, got %+v", old)
	}
}

func TestJobSpentCountsTheEstimateUntilTheCostIsKnown(t *testing.T) {
	cost := 0.04
	for name, tc := range map[string]struct {
		j    Job
		want float64
	}{
		"known":    {Job{EstimateUSD: 0.2, CostUSD: &cost}, 0.04},
		"unknown":  {Job{EstimateUSD: 0.2}, 0.2},
		"declined": {Job{EstimateUSD: 0.2, Approval: "declined"}, 0},
	} {
		if got := tc.j.Spent(); got != tc.want {
			t.Errorf("%s: spent %v, want %v", name, got, tc.want)
		}
	}
}
