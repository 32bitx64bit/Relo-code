package art

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Defaults and limits. The settings clamp to these, and a request that names its own quality or size is checked
// against the same lists.
const (
	DefaultModel   = "gpt-image-2.5-flare"
	maxRefsPerJob  = 6
	maxPictures    = 4
	maxPromptBytes = 4000
	// The session limits are the user's to raise: 0 means no limit at all, and the ceilings only stop a typo.
	DefaultBudgetUSD   = 10.0
	DefaultJobLimit    = 25
	maxBudgetUSD       = 100000.0
	maxSessionJobLimit = 100000
	// callSlots bounds the image calls in flight at once, across every agent.
	callSlots = 2
)

var (
	qualities   = []string{"low", "medium", "high", "auto"}
	sizes       = []string{"1024x1024", "1536x1024", "1024x1536", "auto"}
	backgrounds = []string{"auto", "transparent", "opaque"}
)

// Settings are the user's choices, applied live.
type Settings struct {
	Enabled bool
	// Provider is the ID of the Relo provider whose endpoint and key are used; empty means the first OpenAI one.
	Provider string
	Model    string
	Quality  string
	Size     string
	// Count is how many pictures a job asks for unless the agent says otherwise.
	Count           int
	BudgetUSD       float64 // the most one session may spend; 0 means no limit
	SessionJobLimit int     // the most jobs one session may start; 0 means no limit
	Approval        string  // session or each
}

func normalize(s Settings) Settings {
	s.Provider = strings.TrimSpace(s.Provider)
	if s.Model = strings.TrimSpace(s.Model); s.Model == "" {
		s.Model = DefaultModel
	}
	if !contains(qualities, s.Quality) {
		s.Quality = "medium"
	}
	if !contains(sizes, s.Size) {
		s.Size = "1024x1024"
	}
	if s.Count < 1 || s.Count > maxPictures {
		s.Count = 2
	}
	if s.BudgetUSD < 0 {
		s.BudgetUSD = DefaultBudgetUSD
	}
	s.BudgetUSD = min(maxBudgetUSD, s.BudgetUSD)
	if s.SessionJobLimit < 0 {
		s.SessionJobLimit = DefaultJobLimit
	}
	s.SessionJobLimit = min(maxSessionJobLimit, s.SessionJobLimit)
	if s.Approval != "each" {
		s.Approval = "session"
	}
	return s
}

// Approval is a request to the user to let a job spend money.
type Approval struct {
	JobID     string
	SessionID string
	AgentID   string
	Title     string
	Detail    string
	USD       float64
}

// Hooks connect the manager to the rest of Relo.
type Hooks struct {
	// Endpoint returns where to send image calls for the named provider (empty for the default). Its error says what is
	// missing and is shown to the agent, so it must not contain the key.
	Endpoint func(provider string) (Endpoint, error)
	// Approve asks the user and returns "once", "always" or "reject". It blocks until they answer or ctx ends. A nil
	// Approve approves everything, which only tests and an embedder without a UI want.
	Approve func(ctx context.Context, a Approval) (string, error)
	// Yolo reports whether the user switched on automatic approval. It never lifts the budget.
	Yolo func() bool
	Now  func() time.Time
	// Client sends the image calls; nil uses a client with the usual timeout.
	Client *http.Client
}

// Budget is what a session has spent and may still spend.
//
// A limit the user turned off is not shown as a limit of zero: its figures are left out of the JSON and its Unlimited
// flag is true, so an agent is never told it has $0 left when it has no limit. A limit that is set always shows, even
// at zero remaining.
type Budget struct {
	SpentUSD        float64 `json:"spentUsd"`
	LimitUSD        float64 `json:"limitUsd"`
	RemainingUSD    float64 `json:"remainingUsd"`
	BudgetUnlimited bool    `json:"budgetUnlimited"`
	ProjectUSD      float64 `json:"projectUsd"`
	SessionJobs     int     `json:"sessionJobs"`
	SessionJobLimit int     `json:"sessionJobLimit"`
	JobsUnlimited   bool    `json:"jobsUnlimited"`
}

// MarshalJSON leaves out the figures of a limit that is off. The outer fields shadow the embedded ones of the same name.
func (b Budget) MarshalJSON() ([]byte, error) {
	type plain Budget // without the method, so this does not call itself
	out := struct {
		plain
		LimitUSD        *float64 `json:"limitUsd,omitempty"`
		RemainingUSD    *float64 `json:"remainingUsd,omitempty"`
		SessionJobLimit *int     `json:"sessionJobLimit,omitempty"`
	}{plain: plain(b)}
	if !b.BudgetUnlimited {
		out.LimitUSD, out.RemainingUSD = &b.LimitUSD, &b.RemainingUSD
	}
	if !b.JobsUnlimited {
		out.SessionJobLimit = &b.SessionJobLimit
	}
	return json.Marshal(out)
}

// Status is what an agent is told about the setup before it starts.
type Status struct {
	Enabled bool `json:"enabled"`
	// Ready is false when no pictures can be made yet; Problem says why.
	Ready      bool     `json:"ready"`
	Problem    string   `json:"problem,omitempty"`
	Model      string   `json:"model"`
	Quality    string   `json:"quality"`
	Size       string   `json:"size"`
	Count      int      `json:"picturesPerJob"`
	Qualities  []string `json:"qualities"`
	Sizes      []string `json:"sizes"`
	Backgrouds []string `json:"backgrounds"`
	Budget     Budget   `json:"budget"`
	Note       string   `json:"note,omitempty"`
}

// Manager runs jobs for one workspace.
type Manager struct {
	store  *Store
	hooks  Hooks
	client *http.Client
	// approvalWait is how long the user has to answer.
	approvalWait time.Duration

	mu       sync.Mutex
	set      Settings
	approved map[string]bool
	cancels  map[string]context.CancelFunc
	closed   bool
	slots    chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewManager opens the workspace's art folder at root (<workspace>/.relo/art).
func NewManager(root string, hooks Hooks) (*Manager, error) {
	store, err := OpenStore(root)
	if err != nil {
		return nil, err
	}
	if hooks.Now == nil {
		hooks.Now = time.Now
	}
	client := hooks.Client
	if client == nil {
		client = newClient()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{store: store, hooks: hooks, client: client, approvalWait: 10 * time.Minute, set: normalize(Settings{BudgetUSD: DefaultBudgetUSD, SessionJobLimit: DefaultJobLimit}),
		approved: map[string]bool{}, cancels: map[string]context.CancelFunc{}, slots: make(chan struct{}, callSlots), ctx: ctx, cancel: cancel}, nil
}

// Recover deals with what was in flight when Relo last stopped. Call it once, after Settings are applied. A job that was
// running may or may not have reached the provider, so it is failed and keeps counting at its estimate; one that was
// waiting for the user's approval asks again.
func (m *Manager) Recover() {
	for _, j := range m.store.List() {
		switch {
		case j.State == StateRunning:
			m.fail(j.ID, "Relo stopped while this job was running. The provider may still have made and billed the pictures: check its usage page before running it again.", nil)
		case j.State == StateQueued && j.Approval == "pending":
			m.process(j.ID)
		case j.State == StateQueued:
			m.cancelJob(j.ID, "Relo stopped before this job started.")
		}
	}
}

// Close stops the background work. A job waiting for approval stays queued and asks again after the restart.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) Store() *Store { return m.store }

// SetSettings applies the user's choices.
func (m *Manager) SetSettings(s Settings) {
	m.mu.Lock()
	m.set = normalize(s)
	m.mu.Unlock()
}

func (m *Manager) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.set
}

// Status describes the setup. It checks that an endpoint is configured but never reveals it.
func (m *Manager) Status(sessionID string) Status {
	s := m.Settings()
	st := Status{Enabled: s.Enabled, Ready: s.Enabled, Model: s.Model, Quality: s.Quality, Size: s.Size, Count: s.Count,
		Qualities: qualities, Sizes: sizes, Backgrouds: backgrounds, Budget: m.Budget(sessionID),
		Note: "Prices are estimates until a job finishes, then they are worked out from the usage the provider reports."}
	if _, err := m.endpoint(s); err != nil {
		st.Ready, st.Problem = false, err.Error()
	}
	return st
}

func (m *Manager) endpoint(s Settings) (Endpoint, error) {
	if m.hooks.Endpoint == nil {
		return Endpoint{}, errors.New("no image provider is configured")
	}
	return m.hooks.Endpoint(s.Provider)
}

// Budget is the money a session has used, counting each job at its real cost when known and its estimate otherwise.
func (m *Manager) Budget(sessionID string) Budget {
	s := m.Settings()
	var b Budget
	b.LimitUSD, b.SessionJobLimit = s.BudgetUSD, s.SessionJobLimit
	b.BudgetUnlimited, b.JobsUnlimited = s.BudgetUSD == 0, s.SessionJobLimit == 0
	for _, j := range m.store.List() {
		b.ProjectUSD += j.Spent()
		if j.SessionID == sessionID && j.Approval != "declined" {
			b.SpentUSD += j.Spent()
			b.SessionJobs++
		}
	}
	b.SpentUSD, b.ProjectUSD = round4(b.SpentUSD), round4(b.ProjectUSD)
	if !b.BudgetUnlimited {
		b.RemainingUSD = round4(max(0, b.LimitUSD-b.SpentUSD))
	}
	return b
}

// GenerateInput is a request for new pictures. Refs are ids from Store.AddRef; with any, the call is an edit that
// uses them as the pictures to start from. Quality, Size, Background and Count fall back to the user's settings.
type GenerateInput struct {
	SessionID  string
	AgentID    string
	Prompt     string
	Refs       []string
	Quality    string
	Size       string
	Background string
	Count      int
}

// Generate starts a job. It checks everything it can before anything is spent, and returns while the user is still
// being asked: the job is queued until they answer.
func (m *Manager) Generate(ctx context.Context, in GenerateInput) (Job, error) {
	s := m.Settings()
	if !s.Enabled {
		return Job{}, errors.New("picture generation is switched off")
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" || len(prompt) > maxPromptBytes {
		return Job{}, fmt.Errorf("prompt must be 1 to %d bytes of text", maxPromptBytes)
	}
	quality, size, background, count := orDefault(in.Quality, s.Quality), orDefault(in.Size, s.Size), orDefault(in.Background, "auto"), in.Count
	if count == 0 {
		count = s.Count
	}
	switch {
	case !contains(qualities, quality):
		return Job{}, fmt.Errorf("quality %q must be one of %s", quality, strings.Join(qualities, ", "))
	case !contains(sizes, size):
		return Job{}, fmt.Errorf("size %q must be one of %s", size, strings.Join(sizes, ", "))
	case !contains(backgrounds, background):
		return Job{}, fmt.Errorf("background %q must be one of %s", background, strings.Join(backgrounds, ", "))
	case count < 1 || count > maxPictures:
		return Job{}, fmt.Errorf("count must be 1 to %d pictures", maxPictures)
	case len(in.Refs) > maxRefsPerJob:
		return Job{}, fmt.Errorf("attach at most %d reference pictures", maxRefsPerJob)
	}
	var refs []Ref
	seen := map[string]bool{}
	for i, id := range in.Refs {
		if !m.store.HasRef(id) {
			return Job{}, fmt.Errorf("refs[%d]: no stored reference %q; add the picture with the refs action first", i, id)
		}
		if seen[id] {
			return Job{}, fmt.Errorf("refs[%d] repeats %s", i, id)
		}
		seen[id] = true
		refs = append(refs, Ref{ID: id, File: filepath.Join(m.store.RefsDir(), id+".png")})
	}
	if _, err := m.endpoint(s); err != nil {
		return Job{}, err
	}
	b := m.Budget(in.SessionID)
	if !b.JobsUnlimited && b.SessionJobs >= s.SessionJobLimit {
		return Job{}, fmt.Errorf("this session has already started %d of the %d jobs it may; stop here, use what you have, and tell the user what is still missing", b.SessionJobs, s.SessionJobLimit)
	}
	estimate := EstimateUSD(s.Model, quality, size, count, len(refs), len(prompt))
	if !b.BudgetUnlimited && estimate > b.RemainingUSD {
		return Job{}, fmt.Errorf("not enough budget: this job could cost up to $%.2f and $%.2f of the session's $%.2f is left. Do not retry at the same size; ask for fewer pictures or a lower quality, or stop and tell the user", estimate, b.RemainingUSD, b.LimitUSD)
	}
	created, err := m.store.Create(Job{SessionID: in.SessionID, AgentID: in.AgentID, State: StateQueued, Prompt: prompt, Model: s.Model, Quality: quality, Size: size,
		Background: background, Count: count, Refs: refs, EstimateUSD: estimate, Approval: "pending", Message: "Waiting for the user to approve the spend."})
	if err != nil {
		return Job{}, err
	}
	m.process(created.ID)
	return created, nil
}

func orDefault(v, def string) string {
	if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
		return v
	}
	return def
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// process asks for approval if it is needed, then runs the job. It works in the background so the agent's call returns
// at once.
func (m *Manager) process(id string) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancels[id] = cancel
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer func() {
			cancel()
			m.mu.Lock()
			delete(m.cancels, id)
			m.mu.Unlock()
		}()
		j, ok := m.store.Get(id)
		if !ok || j.State != StateQueued {
			return
		}
		if m.needsApproval(j) {
			answer, err := m.ask(ctx, j)
			if err != nil && m.ctx.Err() != nil {
				return // Relo is shutting down: the job stays queued and is asked about again after the restart
			}
			if err != nil && ctx.Err() != nil {
				return // cancelled while the user was being asked
			}
			if err != nil || answer == "reject" {
				msg := "The user declined this job."
				if err != nil {
					msg = "Nobody answered the approval request in time, so the job was dropped."
				}
				_, _ = m.store.Update(id, func(x *Job) { x.Approval = "declined" })
				m.cancelJob(id, msg)
				return
			}
			if answer == "always" {
				m.mu.Lock()
				m.approved[j.SessionID] = true
				m.mu.Unlock()
			}
		}
		if cur, ok := m.store.Get(id); !ok || cur.State != StateQueued {
			return
		}
		_, _ = m.store.Update(id, func(x *Job) { x.Approval = "approved"; x.Message = "" })
		m.run(ctx, id)
	}()
}

func (m *Manager) needsApproval(j Job) bool {
	if m.hooks.Approve == nil || (m.hooks.Yolo != nil && m.hooks.Yolo()) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.set.Approval == "each" || !m.approved[j.SessionID]
}

func (m *Manager) ask(ctx context.Context, j Job) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, m.approvalWait)
	defer cancel()
	b := m.Budget(j.SessionID)
	var detail strings.Builder
	detail.WriteString(j.Prompt)
	if n := len(j.Refs); n > 0 {
		fmt.Fprintf(&detail, "\nStarting from %d reference picture(s).", n)
	}
	fmt.Fprintf(&detail, "\n%d picture(s), %s quality, %s, model %s.", j.Count, j.Quality, j.Size, j.Model)
	if b.BudgetUnlimited {
		fmt.Fprintf(&detail, "\nAt most about $%.2f. This session has spent $%.2f and has no budget limit.", j.EstimateUSD, b.SpentUSD-j.EstimateUSD)
	} else {
		fmt.Fprintf(&detail, "\nAt most about $%.2f. This session has spent $%.2f of its $%.2f budget.", j.EstimateUSD, b.SpentUSD-j.EstimateUSD, b.LimitUSD)
	}
	title := fmt.Sprintf("Generate %d picture(s): up to about $%.2f", j.Count, j.EstimateUSD)
	return m.hooks.Approve(ctx, Approval{JobID: j.ID, SessionID: j.SessionID, AgentID: j.AgentID, Title: title, Detail: detail.String(), USD: j.EstimateUSD})
}

// run makes the call and stores what came back.
func (m *Manager) run(ctx context.Context, id string) {
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		return
	}
	j, ok := m.store.Get(id)
	if !ok || j.State != StateQueued {
		return
	}
	ep, err := m.endpoint(m.Settings())
	if err != nil {
		m.fail(id, err.Error(), zero())
		return
	}
	call := imageCall{Model: j.Model, Prompt: j.Prompt, Size: j.Size, Quality: j.Quality, Background: j.Background, N: j.Count}
	for _, r := range j.Refs {
		data, err := readPicture(r.File)
		if err != nil {
			m.fail(id, fmt.Sprintf("reference %s cannot be read: %v", r.ID, err), zero())
			return
		}
		call.Refs = append(call.Refs, data)
	}
	if _, err := m.store.Update(id, func(x *Job) { x.State, x.Message = StateRunning, "" }); err != nil {
		return
	}
	reply, err := callImages(ctx, m.client, ep, call)
	if cur, ok := m.store.Get(id); !ok || cur.State != StateRunning {
		return // cancelled while the call was out
	}
	if err != nil {
		var ce *CallError
		if errors.As(err, &ce) && ce.Billed {
			m.fail(id, ce.Message+" The provider may still have made and billed the pictures: check its usage page.", nil)
			return
		}
		m.fail(id, err.Error(), zero())
		return
	}
	cost, fromEstimate := CostUSD(j.Model, reply.Usage), false
	if !reply.HasUsage {
		cost, fromEstimate = j.EstimateUSD, true
	}
	cost = round4(cost)
	added, err := m.store.attach(j, reply.Images)
	if err != nil {
		m.fail(id, "the provider's pictures could not be stored: "+err.Error(), &cost)
		return
	}
	_, _ = m.store.Update(id, func(x *Job) {
		x.State, x.Message = StateReady, ""
		x.Candidates = append(x.Candidates, added...)
		x.CostUSD, x.CostFromEstimate = &cost, fromEstimate
	})
}

func zero() *float64 { v := 0.0; return &v }

// fail ends a job without usable pictures. cost is what it is known to have cost: zero when the provider refused the
// call, nil when that cannot be known.
func (m *Manager) fail(id, message string, cost *float64) {
	_, _ = m.store.Update(id, func(x *Job) {
		x.State, x.Message, x.CostUSD = StateFailed, oneLine(message), cost
	})
}

// cancelJob ends a job that never reached the provider, at no cost.
func (m *Manager) cancelJob(id, message string) {
	_, _ = m.store.Update(id, func(x *Job) {
		x.State, x.Message, x.CostUSD = StateCancelled, oneLine(message), zero()
	})
}

// Get returns a job.
func (m *Manager) Get(id string) (Job, bool) { return m.store.Get(id) }

// List returns the most recent jobs, newest first.
func (m *Manager) List(limit int) []Job {
	all := m.store.List()
	sort.SliceStable(all, func(a, b int) bool { return all[a].CreatedAt.After(all[b].CreatedAt) })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}

// Wait returns the job once it is no longer working, or after max has passed. The second result is true when it timed
// out with the job still working.
func (m *Manager) Wait(ctx context.Context, id string, max time.Duration) (Job, bool, error) {
	watch, stop := m.store.Watch(id)
	defer stop()
	timer := time.NewTimer(max)
	defer timer.Stop()
	for {
		j, ok := m.store.Get(id)
		if !ok {
			return Job{}, false, fmt.Errorf("no job %q", id)
		}
		if !j.Active() {
			return j, false, nil
		}
		select {
		case <-watch:
		case <-timer.C:
			return j, true, nil
		case <-ctx.Done():
			return j, true, ctx.Err()
		}
	}
}

// Cancel stops a job that has not finished. One that is already out at the provider may still be billed, so it keeps
// counting at its estimate.
func (m *Manager) Cancel(id string) (Job, error) {
	j, ok := m.store.Get(id)
	if !ok {
		return Job{}, fmt.Errorf("no job %q", id)
	}
	if !j.Active() {
		return j, fmt.Errorf("job %s is %s and cannot be cancelled", id, j.State)
	}
	if j.State == StateRunning {
		_, _ = m.store.Update(id, func(x *Job) {
			x.State, x.Message, x.CostUSD = StateCancelled, "Cancelled while it was running; the provider may still bill it.", nil
		})
	} else {
		m.cancelJob(id, "Cancelled.")
	}
	m.mu.Lock()
	cancel := m.cancels[id]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	out, _ := m.store.Get(id)
	return out, nil
}

// Provenance describes where an accepted picture came from.
type Provenance struct {
	Job        string   `json:"job"`
	Index      int      `json:"index"`
	Prompt     string   `json:"prompt"`
	Model      string   `json:"model"`
	Quality    string   `json:"quality"`
	Size       string   `json:"size"`
	Background string   `json:"background"`
	Refs       []string `json:"refs,omitempty"`
	SHA256     string   `json:"sha256"`
	At         string   `json:"at"`
}

// ProvenanceFor returns the record for picture index of a job.
func (m *Manager) ProvenanceFor(id string, index int) (Provenance, bool) {
	j, ok := m.store.Get(id)
	if !ok {
		return Provenance{}, false
	}
	for _, c := range j.Candidates {
		if c.Index == index {
			p := Provenance{Job: j.ID, Index: index, Prompt: j.Prompt, Model: j.Model, Quality: j.Quality, Size: j.Size, Background: j.Background, SHA256: c.SHA256, At: j.CreatedAt.Format(time.RFC3339)}
			for _, r := range j.Refs {
				p.Refs = append(p.Refs, r.ID)
			}
			return p, true
		}
	}
	return Provenance{}, false
}

// RelPath is where a job's picture is, relative to the workspace, for the agent's tools.
func RelPath(id string, index int) string {
	return filepath.ToSlash(filepath.Join(".relo", "art", "raw", id, fmt.Sprintf("%d.png", index)))
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 32 {
			return ' '
		}
		return r
	}, s)), " ")
	if len(s) > 400 {
		s = s[:400]
	}
	return s
}

// CandidateData returns the stored picture number index of a job.
func (m *Manager) CandidateData(jobID string, index int) ([]byte, error) {
	j, ok := m.store.Get(jobID)
	if !ok {
		return nil, fmt.Errorf("no job %q", jobID)
	}
	for _, c := range j.Candidates {
		if c.Index == index {
			return readPicture(filepath.Join(j.Dir, c.File))
		}
	}
	return nil, fmt.Errorf("job %s has no picture %d", jobID, index)
}
