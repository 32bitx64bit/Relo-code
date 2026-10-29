package art

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Job states. ready is not final: a ready job's pictures can still be used as references for the next one.
const (
	StateQueued    = "queued" // made, and waiting for the user to approve the spend
	StateRunning   = "running"
	StateReady     = "ready"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// Ref is a reference picture attached to a job. File is a normalized copy under the store's refs folder.
type Ref struct {
	ID   string `json:"id"`
	File string `json:"file"`
}

// Candidate is one picture a job produced, stored as <number>.png in the job's folder.
type Candidate struct {
	Index  int    `json:"index"`
	File   string `json:"file"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	SHA256 string `json:"sha256"`
}

// Job is one image generation or edit. The JSON form is the file form.
type Job struct {
	ID          string      `json:"id"`
	SessionID   string      `json:"sessionId"`
	AgentID     string      `json:"agentId"`
	State       string      `json:"state"`
	Prompt      string      `json:"prompt"`
	Model       string      `json:"model"`
	Quality     string      `json:"quality"`
	Size        string      `json:"size"`
	Background  string      `json:"background"`
	AspectRatio string      `json:"aspectRatio,omitempty"`
	Resolution  string      `json:"resolution,omitempty"`
	Seed        *int64      `json:"seed,omitempty"`
	Count       int         `json:"count"`
	Refs        []Ref       `json:"refs"`
	Candidates  []Candidate `json:"candidates"`
	Dir         string      `json:"dir"`
	Message     string      `json:"message,omitempty"`
	CreatedAt   time.Time   `json:"createdAt"`
	UpdatedAt   time.Time   `json:"updatedAt"`

	// EstimateUSD is the conservative price worked out before the call. CostUSD is what the call cost, from the usage
	// the endpoint reported; it is nil while that is unknown (a call that was cut off may or may not have been billed),
	// and until it is known the estimate is what counts against the session's budget.
	EstimateUSD float64  `json:"estimateUsd"`
	CostUSD     *float64 `json:"costUsd,omitempty"`
	// CostFromEstimate is true when the endpoint reported no usage, so CostUSD is the estimate for the call.
	CostFromEstimate bool `json:"costFromEstimate,omitempty"`

	// Approval is pending, approved or declined.
	Approval string `json:"approval,omitempty"`
}

// Active reports whether the job may still produce pictures.
func (j Job) Active() bool { return j.State == StateQueued || j.State == StateRunning }

// Spent is what the job counts for against a budget: its cost when known, otherwise its estimate, unless it never
// reached the endpoint.
func (j Job) Spent() float64 {
	if j.CostUSD != nil {
		return *j.CostUSD
	}
	if j.Approval == "declined" {
		return 0
	}
	return j.EstimateUSD
}

func (j Job) clone() Job {
	c := j
	c.Refs = append([]Ref(nil), j.Refs...)
	c.Candidates = append([]Candidate(nil), j.Candidates...)
	if j.Seed != nil {
		v := *j.Seed
		c.Seed = &v
	}
	if j.CostUSD != nil {
		v := *j.CostUSD
		c.CostUSD = &v
	}
	return c
}

// Store keeps jobs in memory and on disk under root (<workspace>/.relo/art):
//
//	jobs/<id>/job.json   the record
//	raw/<id>/<n>.png     the candidates
//	refs/<id>.png        normalized reference pictures
type Store struct {
	root string

	mu       sync.Mutex
	jobs     map[string]*Job
	next     int
	watchers map[string]map[chan struct{}]struct{}
	onChange func(Job)
}

// OpenStore loads the jobs under root. A folder that does not exist yet is fine.
func OpenStore(root string) (*Store, error) {
	s := &Store{root: root, jobs: map[string]*Job{}, watchers: map[string]map[chan struct{}]struct{}{}, next: 1}
	entries, err := os.ReadDir(filepath.Join(root, "jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, "jobs", e.Name(), "job.json"))
		if err != nil {
			continue
		}
		var j Job
		if json.Unmarshal(data, &j) != nil || j.ID != e.Name() {
			continue
		}
		switch j.State {
		case StateQueued, StateRunning, StateReady, StateFailed, StateCancelled:
		default: // written by a version of Relo that generated pictures another way
			j.State, j.Message = StateCancelled, "Made by an earlier version of Relo."
		}
		s.place(&j)
		s.jobs[j.ID] = &j
		if n, err := strconv.Atoi(strings.TrimPrefix(j.ID, "j")); err == nil && n >= s.next {
			s.next = n + 1
		}
	}
	return s, nil
}

// place sets the paths in a job from where the store is now, so a workspace that moved still works.
func (s *Store) place(j *Job) {
	j.Dir = s.JobDir(j.ID)
	for i := range j.Refs {
		j.Refs[i].File = filepath.Join(s.RefsDir(), j.Refs[i].ID+".png")
	}
}

func (s *Store) Root() string            { return s.root }
func (s *Store) JobDir(id string) string { return filepath.Join(s.root, "raw", id) }
func (s *Store) RefsDir() string         { return filepath.Join(s.root, "refs") }

// OnChange installs the function called, outside the lock, after every create and update.
func (s *Store) OnChange(fn func(Job)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// Create stores a new job, giving it an ID (j1, j2, ...) and its folders.
func (s *Store) Create(j Job) (Job, error) {
	s.mu.Lock()
	j = j.clone()
	j.ID = "j" + strconv.Itoa(s.next)
	s.next++
	now := time.Now().UTC()
	j.CreatedAt, j.UpdatedAt = now, now
	if j.State == "" {
		j.State = StateQueued
	}
	if j.Candidates == nil {
		j.Candidates = []Candidate{}
	}
	if j.Refs == nil {
		j.Refs = []Ref{}
	}
	s.place(&j)
	if err := os.MkdirAll(j.Dir, 0o755); err != nil {
		s.mu.Unlock()
		return Job{}, fmt.Errorf("create job folder: %w", err)
	}
	if err := s.save(&j); err != nil {
		s.mu.Unlock()
		return Job{}, err
	}
	s.jobs[j.ID] = &j
	out, fn := j.clone(), s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn(out)
	}
	return out, nil
}

// Get returns a copy of a job.
func (s *Store) Get(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, false
	}
	return j.clone(), true
}

// Update changes a job under the lock and saves it. A failed save leaves the job as it was.
func (s *Store) Update(id string, fn func(*Job)) (Job, error) {
	s.mu.Lock()
	cur, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return Job{}, fmt.Errorf("no job %s", id)
	}
	next := cur.clone()
	fn(&next)
	next.ID, next.Dir = cur.ID, cur.Dir
	next.UpdatedAt = time.Now().UTC()
	if err := s.save(&next); err != nil {
		s.mu.Unlock()
		return Job{}, err
	}
	*cur = next
	out, onChange := next.clone(), s.onChange
	for ch := range s.watchers[id] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
	if onChange != nil {
		onChange(out)
	}
	return out, nil
}

// List returns copies of all jobs, oldest first.
func (s *Store) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j.clone())
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].CreatedAt.Equal(out[b].CreatedAt) {
			return out[a].CreatedAt.Before(out[b].CreatedAt)
		}
		return jobNumber(out[a].ID) < jobNumber(out[b].ID)
	})
	return out
}

func jobNumber(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "j"))
	return n
}

// Watch returns a channel that receives a value after each update of a job, and a function that stops the watch.
func (s *Store) Watch(id string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	if s.watchers[id] == nil {
		s.watchers[id] = map[chan struct{}]struct{}{}
	}
	s.watchers[id][ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.watchers[id], ch)
		s.mu.Unlock()
	}
}

// save writes the job file beside its destination and renames it into place.
func (s *Store) save(j *Job) error {
	dir := filepath.Join(s.root, "jobs", j.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "job.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write job: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "job.json")); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write job: %w", err)
	}
	return nil
}
