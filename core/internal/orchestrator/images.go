package orchestrator

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/vision"
)

const (
	// maxImagePerMinute bounds how often one agent may call relo_image. Each call can put megabytes in its context.
	maxImagePerMinute = 30
	// maxImageConcurrent bounds the images decoded at once across the Coord: a large file can take a few hundred MB.
	maxImageConcurrent = 2
	// maxImageCallBytes bounds the pictures one call returns, under the hub's own cap.
	maxImageCallBytes = 12 << 20
)

// imageGuidance is the turn's line about images, and whether relo_image is offered with it.
func imageGuidance(status string) (string, bool) {
	const view = "relo_image shows you image files from your workspace, such as screenshots, reference designs and rendered output: pass your key and 1-4 paths, and a region to zoom in. It does not take screenshots, so write one to a file first (Canvas capture, or the project's own screenshot command). When your task has a visual reference, view your result next to it and fix the differences before you report."
	switch status {
	case vision.No:
		return "Your model cannot see images, so you cannot check a UI or a screenshot by eye. Never claim a visual match: report that visual verification was unavailable and say what a person or a vision model should check.", false
	case vision.Yes:
		return view, true
	default:
		return view + " Relo does not know whether your model accepts images; if none arrives, report that visual verification was unavailable.", true
	}
}

// imageEntry describes one requested file in the text part of a relo_image result, in the order of the image blocks.
type imageEntry struct {
	Index        int            `json:"index,omitempty"` // 1-based position among the image blocks; 0 when not shown
	Path         string         `json:"path"`
	Error        string         `json:"error,omitempty"`
	Format       string         `json:"format,omitempty"`
	Source       *imageSize     `json:"source,omitempty"`
	Sent         *imageSize     `json:"sent,omitempty"`
	Crop         *vision.Region `json:"crop,omitempty"`
	Scale        float64        `json:"scale,omitempty"` // source pixels per sent pixel
	ApproxTokens int            `json:"approxTokens,omitempty"`
	Note         string         `json:"note,omitempty"`
}

type imageSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// imageRequest services one relo_image call. The key is checked like every hub call: bound to the live session and
// the agent's live runtime, with the agent still active. The Primary and agents on a text-only model are refused, so
// a model never receives pixels Relo knows it cannot use.
func (c *Coord) imageRequest(ctx context.Context, req agentmsg.ImageRequest) (agentmsg.ImageResult, error) {
	a, sessionWorkspace, err := c.imageCaller(req.Key)
	if err != nil {
		return agentmsg.ImageResult{}, err
	}
	if a.Role == agent.Primary {
		return agentmsg.ImageResult{}, errors.New("relo_image is not available to the Primary; ask a worker to look")
	}
	status := c.settings().VisionFor(a.Model)
	if status == vision.No {
		return agentmsg.ImageResult{}, fmt.Errorf("your model (%s) cannot see images, so no image was sent. Report that visual verification was unavailable", a.Model)
	}
	if err := c.reserveImage(a.ID); err != nil {
		return agentmsg.ImageResult{}, err
	}
	opts := vision.Options{MaxEdge: req.MaxEdge}
	if r := req.Region; r != nil {
		opts.Region = &vision.Region{X: r.X, Y: r.Y, Width: r.Width, Height: r.Height}
	}
	roots := []string{a.Workspace}
	if sessionWorkspace != a.Workspace {
		roots = append(roots, sessionWorkspace)
	}

	var result agentmsg.ImageResult
	entries := make([]imageEntry, 0, len(req.Paths))
	total := 0
	for _, path := range req.Paths {
		entry, pic, err := c.prepareImage(ctx, roots, path, opts)
		if err != nil {
			entries = append(entries, imageEntry{Path: path, Error: err.Error()})
			continue
		}
		if total+len(pic.Data) > maxImageCallBytes {
			entry.Error = "not shown: this call already carries as much image data as one result may; view it in another call"
			entries = append(entries, entry)
			continue
		}
		total += len(pic.Data)
		result.Images = append(result.Images, agentmsg.Image{MIME: pic.MIME, Data: base64.StdEncoding.EncodeToString(pic.Data)})
		entry.Index = len(result.Images)
		entries = append(entries, entry)
	}
	if len(result.Images) == 0 {
		msg := "no image could be shown"
		for _, e := range entries {
			msg += "; " + e.Path + ": " + e.Error
		}
		return agentmsg.ImageResult{}, errors.New(msg)
	}
	text := map[string]any{"images": entries}
	if status == vision.Unknown {
		text["note"] = "Relo does not know whether your model accepts images. If you cannot see them, report that visual verification was unavailable instead of describing them."
	}
	result.Text = text
	return result, nil
}

// prepareImage opens and prepares one file, holding one of the Coord's decode slots while it does.
func (c *Coord) prepareImage(ctx context.Context, roots []string, path string, opts vision.Options) (imageEntry, vision.Picture, error) {
	release, err := c.acquireImageSlot(ctx)
	if err != nil {
		return imageEntry{}, vision.Picture{}, err
	}
	defer release()
	f, rel, err := vision.Open(roots, path)
	if err != nil {
		return imageEntry{}, vision.Picture{}, err
	}
	defer f.Close()
	pic, err := vision.Prepare(f, opts)
	if err != nil {
		return imageEntry{}, vision.Picture{}, err
	}
	entry := imageEntry{
		Path:         rel,
		Format:       pic.Format,
		Source:       &imageSize{Width: pic.SourceWidth, Height: pic.SourceHeight},
		Sent:         &imageSize{Width: pic.Width, Height: pic.Height},
		Crop:         pic.Crop,
		Scale:        pic.Scale,
		ApproxTokens: pic.ApproxTokens(),
	}
	if pic.Animated {
		entry.Note = "animated GIF: only the first frame is shown"
	}
	return entry, pic, nil
}

// imageCaller resolves a capability key to the agent that holds it and the session workspace.
func (c *Coord) imageCaller(key string) (agent.Agent, string, error) {
	c.mu.Lock()
	identity, ok := c.peerKeys[key]
	valid := ok && c.sess != nil && c.sess.ID == identity.session
	var workspace string
	if valid {
		workspace = c.sess.Workspace
	}
	c.mu.Unlock()
	if !valid {
		return agent.Agent{}, "", errors.New("invalid or expired agent image key")
	}
	c.runtimeMu.Lock()
	live := c.runtimeSessions[identity.agent]
	active := live != nil && live.id == identity.runtime
	c.runtimeMu.Unlock()
	if !active {
		return agent.Agent{}, "", errors.New("agent runtime has ended")
	}
	a, ok := c.Agents.Get(identity.agent)
	if !ok || agent.Terminal(a.Status) {
		return agent.Agent{}, "", errors.New("agent is no longer active")
	}
	if a.Workspace == "" {
		a.Workspace = workspace
	}
	return a, workspace, nil
}

// reserveImage counts one call against the agent's per-minute limit.
func (c *Coord) reserveImage(agentID string) error {
	c.imageMu.Lock()
	defer c.imageMu.Unlock()
	if c.imageRate == nil {
		c.imageRate = map[string][]time.Time{}
	}
	now := time.Now()
	recent := c.imageRate[agentID][:0]
	for _, at := range c.imageRate[agentID] {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	if len(recent) >= maxImagePerMinute {
		c.imageRate[agentID] = recent
		return errors.New("image rate limit reached; look at what you already have, or crop to the part you need")
	}
	c.imageRate[agentID] = append(recent, now)
	return nil
}

// acquireImageSlot waits for one of the Coord's decode slots, or for ctx to end.
func (c *Coord) acquireImageSlot(ctx context.Context) (func(), error) {
	c.imageMu.Lock()
	if c.imageSlots == nil {
		c.imageSlots = make(chan struct{}, maxImageConcurrent)
	}
	slots := c.imageSlots
	c.imageMu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, errors.New("image request timed out waiting for another image to finish")
	}
}
