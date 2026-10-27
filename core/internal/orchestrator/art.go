package orchestrator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/art"
	"github.com/relocode/relo/internal/vision"
)

const (
	// maxArtPerMinute bounds how often one agent may call relo_art. Most calls only read state; the ones that spend
	// are limited far more tightly by the session's job cap and budget.
	maxArtPerMinute = 60
	// defaultArtWait is how long one wait blocks when the agent does not say.
	defaultArtWait = 25
)

// artGuidance is the turn's line about the art tool.
const artGuidance = "relo_art makes pictures with an image model through the user's own API account: every picture costs real money and the user set the limits for the session (they may have set none, which is not a reason to spend carelessly). " +
	"Call it with action status first (is it set up, the model, what is left) and action playbook (prompts, references, transparency, sprites, how much to spend); both are free. " +
	"Work asynchronously: action generate returns at once with a job id, and action wait (up to 45 seconds a call) returns a contact sheet of numbered pictures when the job is ready. " +
	"The user may be asked to approve the spend first: keep waiting, and never start a second job for the same picture. " +
	"Look at the sheet before you ask for more, change one thing at a time, and stop when the sprite passes the brief. To refine a picture, store it as a reference (action refs) and generate again with a prompt that says what to change. " +
	"A picture you choose is a file in .relo/art/raw/<job>/<n>.png (the result gives the path): pass it to relo_sprite as input.path to matte, fit and check it. " +
	"When a call says a limit is reached, stop, use what you have, and tell the user what is missing. Pictures are made by a third party from your prompt and references: put nothing private in either."

// SetArtSettings applies the user's picture settings. Managers that exist pick them up at once; whether the tools are
// offered at all follows Enabled, as SetArtEnabled.
func (c *Coord) SetArtSettings(s art.Settings) {
	c.art.mu.Lock()
	c.art.settings = s
	managers := make([]*art.Manager, 0, len(c.art.managers))
	for _, m := range c.art.managers {
		managers = append(managers, m)
	}
	c.art.mu.Unlock()
	for _, m := range managers {
		m.SetSettings(s)
	}
	c.SetArtEnabled(s.Enabled)
	if s.Enabled && c.Current() != nil {
		c.ResumeArt()
	}
}

// SetArtEndpoint installs the function that says where image calls go and with which key. The key stays in the app: the
// function is called when a job runs, and nothing here stores or logs what it returns.
func (c *Coord) SetArtEndpoint(fn func(provider string) (art.Endpoint, error)) {
	c.art.mu.Lock()
	c.art.endpoint = fn
	c.art.mu.Unlock()
}

func (c *Coord) artEndpoint(provider string) (art.Endpoint, error) {
	c.art.mu.Lock()
	fn := c.art.endpoint
	c.art.mu.Unlock()
	if fn == nil {
		return art.Endpoint{}, errors.New("no image provider is configured")
	}
	return fn(provider)
}

// ResumeArt opens the art manager of the current workspace, which picks up what an earlier run left in flight: a job that
// was waiting for approval. Without it nothing would ask until an agent first needed the manager.
func (c *Coord) ResumeArt() {
	if !c.artOn() {
		return
	}
	if root := c.artRoot(); root != "" {
		if _, err := c.artManager(root); err != nil {
			c.logWarn("resume art jobs", err)
		}
	}
}

// CloseArt stops the background work of every art manager.
func (c *Coord) CloseArt() {
	c.art.mu.Lock()
	managers := c.art.managers
	c.art.managers = nil
	c.art.mu.Unlock()
	for _, m := range managers {
		m.Close()
	}
}

// artManager returns the manager for the workspace at root, making it, and resuming what it left in flight, the first
// time.
func (c *Coord) artManager(root string) (*art.Manager, error) {
	if root == "" {
		return nil, errors.New("there is no workspace to keep pictures in")
	}
	root = filepath.Clean(root)
	c.art.mu.Lock()
	if m := c.art.managers[root]; m != nil {
		c.art.mu.Unlock()
		return m, nil
	}
	settings := c.art.settings
	c.art.mu.Unlock()

	m, err := art.NewManager(filepath.Join(root, ".relo", "art"), art.Hooks{
		Endpoint: c.artEndpoint,
		Approve:  c.artApprove,
		Yolo:     func() bool { p := c.permissions(); return p != nil && p.Yolo() },
	})
	if err != nil {
		return nil, err
	}
	m.SetSettings(settings)

	c.art.mu.Lock()
	if existing := c.art.managers[root]; existing != nil { // another call made it first
		c.art.mu.Unlock()
		m.Close()
		return existing, nil
	}
	if c.art.managers == nil {
		c.art.managers = map[string]*art.Manager{}
	}
	c.art.managers[root] = m
	c.art.mu.Unlock()
	m.Recover()
	return m, nil
}

// artApprove asks the user, through the same prompt as a tool permission, whether a job may spend money. The answer is
// once, always (for the rest of the session) or reject.
func (c *Coord) artApprove(ctx context.Context, ap art.Approval) (string, error) {
	c.art.mu.Lock()
	c.art.askSeq++
	id := "art-" + ap.JobID + "-" + strconv.Itoa(c.art.askSeq)
	c.art.mu.Unlock()
	session := c.SessionID()
	p := &PendingPerm{ID: id, Session: session, AgentID: ap.AgentID, Title: ap.Title,
		Command: ap.Detail + "\nAlways approves the rest of this session's picture jobs, within the limits set for the session.", Reply: make(chan string, 1)}
	c.mu.Lock()
	c.pending[id] = p
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	c.emit("permission.asked", map[string]any{"id": id, "title": p.Title, "command": p.Command, "agentId": ap.AgentID, "session": session})
	select {
	case reply := <-p.Reply:
		return reply, nil
	case <-ctx.Done():
		return "reject", ctx.Err()
	}
}

// artRoot is the workspace the art tools work in: the session's, or the project's when there is none yet.
func (c *Coord) artRoot() string {
	if s := c.Current(); s != nil && s.Workspace != "" {
		return s.Workspace
	}
	if w := c.workspace(); w != nil {
		return w.Root()
	}
	return ""
}

// artRequest services one relo_art call. The key is checked like every hub call: bound to the live session and the
// agent's live runtime, with the agent still active. The Primary, a task that is not an art task, a model that cannot
// see, a role that cannot write and a session that is not in build mode are refused.
func (c *Coord) artRequest(ctx context.Context, req agentmsg.ArtRequest) (agentmsg.ImageResult, error) {
	if !c.artOn() {
		return agentmsg.ImageResult{}, errors.New("relo_art is not enabled")
	}
	a, sessionWorkspace, err := c.imageCaller(req.Key)
	if err != nil {
		return agentmsg.ImageResult{}, errors.New(strings.Replace(err.Error(), "image key", "art key", 1))
	}
	if a.Role == agent.Primary {
		return agentmsg.ImageResult{}, errors.New("relo_art is not available to the Primary; ask a worker to make the picture")
	}
	if t, ok := c.Graph.Get(a.TaskID); !ok || !t.Art {
		return agentmsg.ImageResult{}, errors.New("relo_art is only available to tasks that make art")
	}
	if status := c.settings().VisionFor(a.Model); status == vision.No {
		return agentmsg.ImageResult{}, fmt.Errorf("your model (%s) cannot see images, so you could not judge what was generated. Report that the art work needs a model that sees", a.Model)
	}
	if !agent.Mutating(a.Role) {
		return agentmsg.ImageResult{}, fmt.Errorf("the %s role cannot spend the user's money on pictures", a.Role)
	}
	if s := c.Current(); s == nil || s.Mode != ModeBuild {
		return agentmsg.ImageResult{}, errors.New("relo_art is available only in build mode")
	}
	if err := c.reserveCall("art:"+a.ID, maxArtPerMinute, "art rate limit reached; wait on the job you have instead of asking again"); err != nil {
		return agentmsg.ImageResult{}, err
	}
	mgr, err := c.artManager(sessionWorkspace)
	if err != nil {
		return agentmsg.ImageResult{}, err
	}
	session := c.SessionID()

	switch req.Action {
	case "status":
		return agentmsg.ImageResult{Text: artStatusResult{Status: mgr.Status(session), Topics: art.PlaybookTopics(),
			Note: "Read the playbook (action playbook) before your first job. Jobs run asynchronously: generate, then wait."}}, nil
	case "playbook":
		text, _ := art.Playbook(req.Topic)
		return agentmsg.ImageResult{Text: text}, nil
	case "jobs":
		return c.artJobsResult(mgr, session, req.Limit), nil
	case "refs":
		return c.artRefs(ctx, mgr, a, sessionWorkspace, req.Sources)
	case "generate":
		job, err := mgr.Generate(ctx, art.GenerateInput{SessionID: session, AgentID: a.ID, Prompt: req.Prompt, Refs: req.Refs,
			Quality: req.Quality, Size: req.Size, Background: req.Background, Count: req.Count})
		if err != nil {
			return agentmsg.ImageResult{}, err
		}
		return agentmsg.ImageResult{Text: c.artStarted(mgr, job)}, nil
	case "wait":
		return c.artWait(ctx, mgr, req)
	case "cancel":
		job, ok := mgr.Get(req.Job)
		if !ok {
			return agentmsg.ImageResult{}, fmt.Errorf("no job %q", req.Job)
		}
		if job.SessionID != session {
			return agentmsg.ImageResult{}, fmt.Errorf("job %s belongs to another session; only the user can cancel it", req.Job)
		}
		out, err := mgr.Cancel(req.Job)
		if err != nil {
			return agentmsg.ImageResult{}, err
		}
		return agentmsg.ImageResult{Text: artJobView(out, "")}, nil
	}
	return agentmsg.ImageResult{}, fmt.Errorf("unknown action %q", req.Action)
}

type artStatusResult struct {
	art.Status
	Topics []string `json:"playbookTopics"`
	Note   string   `json:"note,omitempty"`
}

// artJob is what an agent is told about a job.
type artJob struct {
	Job        string       `json:"job"`
	State      string       `json:"state"`
	Prompt     string       `json:"prompt,omitempty"`
	Model      string       `json:"model"`
	Quality    string       `json:"quality"`
	Size       string       `json:"size"`
	Background string       `json:"background"`
	Count      int          `json:"pictures"`
	Refs       int          `json:"references,omitempty"`
	EstimateUS float64      `json:"estimateUsd"`
	CostUSD    *float64     `json:"costUsd,omitempty"`
	Message    string       `json:"message,omitempty"`
	Found      []artPicture `json:"files,omitempty"`
	Next       string       `json:"next,omitempty"`
	Budget     *art.Budget  `json:"budget,omitempty"`
	Sheet      string       `json:"sheet,omitempty"`
	WaitedSecs int          `json:"waitedSeconds,omitempty"`
}

type artPicture struct {
	Index  int    `json:"index"`
	Path   string `json:"path"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func artJobView(j art.Job, next string) artJob {
	v := artJob{Job: j.ID, State: j.State, Model: j.Model, Quality: j.Quality, Size: j.Size, Background: j.Background, Count: j.Count, Refs: len(j.Refs),
		EstimateUS: j.EstimateUSD, CostUSD: j.CostUSD, Message: j.Message, Next: next}
	for _, p := range j.Candidates {
		v.Found = append(v.Found, artPicture{Index: p.Index, Path: art.RelPath(j.ID, p.Index), Width: p.Width, Height: p.Height})
	}
	return v
}

// artStarted describes a job that was just made.
func (c *Coord) artStarted(mgr *art.Manager, j art.Job) artJob {
	v := artJobView(j, "Call wait with this job to follow it. It may first wait for the user to approve the spend, and a picture can take a minute or two.")
	v.Prompt = j.Prompt
	b := mgr.Budget(j.SessionID)
	v.Budget = &b
	return v
}

func (c *Coord) artJobsResult(mgr *art.Manager, session string, limit int) agentmsg.ImageResult {
	if limit < 1 {
		limit = 10
	}
	var jobs []artJob
	for _, j := range mgr.List(0) {
		if j.SessionID != session {
			continue
		}
		jobs = append(jobs, artJobView(j, ""))
		if len(jobs) == limit {
			break
		}
	}
	return agentmsg.ImageResult{Text: map[string]any{"jobs": jobs, "budget": mgr.Budget(session)}}
}

// artWait blocks on a job and, when it is ready, shows the agent its pictures on one numbered sheet.
func (c *Coord) artWait(ctx context.Context, mgr *art.Manager, req agentmsg.ArtRequest) (agentmsg.ImageResult, error) {
	secs := req.WaitSeconds
	if secs < 1 {
		secs = defaultArtWait
	}
	started := time.Now()
	job, timedOut, err := mgr.Wait(ctx, req.Job, time.Duration(secs)*time.Second)
	if err != nil && !timedOut {
		return agentmsg.ImageResult{}, err
	}
	view := artJobView(job, "")
	view.WaitedSecs = int(time.Since(started).Seconds())
	b := mgr.Budget(job.SessionID)
	view.Budget = &b
	var out agentmsg.ImageResult
	switch job.State {
	case art.StateReady:
		view.Next = "Look at the sheet and pick a number; transparent areas show as the sheet's dark grey. Pass the file path above to relo_sprite. To change a picture, store it with action refs and generate again with a prompt that says what to change."
		release, err := c.acquireImageSlot(ctx)
		if err != nil {
			return agentmsg.ImageResult{}, errors.New("art request timed out waiting for another picture to finish")
		}
		img, note, serr := artSheet(job)
		release()
		if serr != nil {
			view.Sheet = "no sheet: " + serr.Error()
		} else {
			out.Images = append(out.Images, img)
			view.Sheet = "image 1: " + note
		}
	case art.StateQueued, art.StateRunning:
		view.Next = "Still " + job.State + ". Call wait again; do not start another job for the same picture."
	case art.StateCancelled:
		view.Next = "This job was cancelled (" + job.Message + "). Do not start it again; carry on with what you have and tell the user."
	default:
		view.Next = "This job failed. Read the message: if it names a setting the user must fix, tell them and stop; otherwise change one thing and try once more, within the budget."
	}
	out.Text = view
	return out, nil
}

// artSheet makes the picture an agent looks at: the job's candidates on one numbered sheet.
func artSheet(job art.Job) (agentmsg.Image, string, error) {
	data, err := art.CandidateSheet(job)
	if err != nil {
		return agentmsg.Image{}, "", err
	}
	pic, err := vision.Prepare(bytes.NewReader(data), vision.Options{})
	if err != nil {
		return agentmsg.Image{}, "", err
	}
	return agentmsg.Image{MIME: pic.MIME, Data: base64.StdEncoding.EncodeToString(pic.Data)}, "the job's pictures, each numbered in its corner", nil
}

type artRefResult struct {
	Refs []artRefInfo `json:"refs"`
	Note string       `json:"note,omitempty"`
}

type artRefInfo struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// artRefs stores reference pictures from workspace files or from earlier jobs.
func (c *Coord) artRefs(ctx context.Context, mgr *art.Manager, a agent.Agent, sessionWorkspace string, sources []agentmsg.ArtSource) (agentmsg.ImageResult, error) {
	roots := []string{a.Workspace}
	if sessionWorkspace != a.Workspace {
		roots = append(roots, sessionWorkspace)
	}
	release, err := c.acquireImageSlot(ctx)
	if err != nil {
		return agentmsg.ImageResult{}, errors.New("art request timed out waiting for another picture to finish")
	}
	defer release()
	var res artRefResult
	for i, src := range sources {
		var data []byte
		var label string
		switch {
		case src.Path != "":
			data, err = readWorkspaceFile(roots, src.Path)
			label = src.Path
		default:
			label = fmt.Sprintf("%s picture %d", src.Job, src.Index)
			data, err = mgr.CandidateData(src.Job, src.Index)
		}
		if err != nil {
			return agentmsg.ImageResult{}, fmt.Errorf("sources[%d] (%s): %w", i, label, err)
		}
		info, err := mgr.Store().AddRef(data)
		if err != nil {
			return agentmsg.ImageResult{}, fmt.Errorf("sources[%d] (%s): %w", i, label, err)
		}
		res.Refs = append(res.Refs, artRefInfo{ID: info.ID, Source: label, Width: info.Width, Height: info.Height})
	}
	res.Note = "Use these ids in generate's refs with a role: style, subject (one only) or composition."
	return agentmsg.ImageResult{Text: res}, nil
}

// recordProvenance notes where a sprite came from when a pipeline started from a generated picture. It writes
// one small file per sprite into the agent's workspace, so two agents never edit the same file, and reports the path.
// The record is a convenience for the user (for example to disclose AI use): a failure never fails the sprite call.
func (c *Coord) recordProvenance(a agent.Agent, sessionWorkspace, input string, wrote []spriteWrite) string {
	if len(wrote) == 0 || a.Workspace == "" {
		return ""
	}
	rel := filepath.ToSlash(filepath.Clean(input))
	if filepath.IsAbs(input) {
		if r, err := filepath.Rel(sessionWorkspace, input); err == nil {
			rel = filepath.ToSlash(r)
		}
	}
	rest, ok := strings.CutPrefix(rel, ".relo/art/raw/")
	if !ok {
		return ""
	}
	jobID, file, ok := strings.Cut(rest, "/")
	if !ok || strings.Contains(file, "/") {
		return ""
	}
	index, err := strconv.Atoi(strings.TrimSuffix(file, ".png"))
	if err != nil {
		return ""
	}
	mgr, err := c.artManager(sessionWorkspace)
	if err != nil {
		return ""
	}
	prov, ok := mgr.ProvenanceFor(jobID, index)
	if !ok {
		return ""
	}
	record := struct {
		art.Provenance
		Output []spriteWrite `json:"output"`
	}{prov, wrote}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return ""
	}
	stem := strings.TrimSuffix(path.Base(wrote[0].Path), path.Ext(wrote[0].Path))
	stem = strings.Map(func(r rune) rune {
		if r == '.' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, stem)
	name := "art/provenance/" + stem + ".json"
	root, err := os.OpenRoot(a.Workspace)
	if err != nil {
		return ""
	}
	defer root.Close()
	if err := root.MkdirAll("art/provenance", 0o755); err != nil {
		return ""
	}
	if err := root.WriteFile(name, append(data, '\n'), 0o644); err != nil {
		return ""
	}
	return name
}
