package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/art"
	"github.com/relocode/relo/internal/sprite"
	"github.com/relocode/relo/internal/vision"
)

const (
	// maxSpritePerMinute bounds how often one agent may run a sprite pipeline. Each call decodes a picture and may
	// resample it several times.
	maxSpritePerMinute = 20
	// maxSpriteWriteBytes bounds one PNG a pipeline writes into the workspace.
	maxSpriteWriteBytes = 8 << 20
	// maxSpriteWrites bounds the files one call writes.
	maxSpriteWrites = sprite.MaxFrames
)

// artState is the Coord's state for the art tools: whether they are offered at all, and the call rate per agent.
type artState struct {
	mu      sync.Mutex
	enabled bool
	rate    map[string][]time.Time // agent ID -> recent relo_sprite call times; "art:"+ID for relo_art

	// The picture generator (art.go): the user's settings, where its calls go, and one manager per workspace.
	settings art.Settings
	endpoint func(provider string) (art.Endpoint, error)
	managers map[string]*art.Manager
	askSeq   int
}

// SetArtEnabled records whether the art tools are switched on. A change closes the messaging server, so the hub's tool
// catalog is rebuilt, with or without relo_sprite, the next time an agent needs it.
func (c *Coord) SetArtEnabled(on bool) {
	c.art.mu.Lock()
	changed := c.art.enabled != on
	c.art.enabled = on
	c.art.mu.Unlock()
	if !changed {
		return
	}
	c.messageMu.Lock()
	s := c.messageServer
	c.messageServer = nil
	c.messageMu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

// artOn reports whether the art tools are offered to tasks that ask for them.
func (c *Coord) artOn() bool {
	c.art.mu.Lock()
	defer c.art.mu.Unlock()
	return c.art.enabled
}

// ArtEnabled reports whether the art tools are switched on.
func (c *Coord) ArtEnabled() bool { return c.artOn() }

// spriteGuidance is the turn's line about the sprite tool.
const spriteGuidance = "This task makes image assets. relo_sprite turns a picture in your workspace into a game-ready sprite, locally: pass your key, input.path, and ops that run in order (matte to remove a flat background, fit to size it with a margin, outline, check against the project's rules), and output to write the PNG into your workspace, where it shows up among your changes. " +
	"If the generator cannot give a transparent background, ask it for a solid colour the sprite does not contain, such as #ff00ff, and matte reads it from the border; check the alpha of what you get either way. " +
	"Work out the sprite specification from the project itself (existing textures, definitions, naming, sizes) and keep it in a short brief file in the workspace, so the next task starts from it; if the project does not settle it, say what you assumed in your report. " +
	"Always set preview, look at the result next to a reference with relo_image, and run check with the project's rules before you report. Do not claim a sprite is right on the strength of a passing check alone."

// spriteResult is the text part of a relo_sprite result.
type spriteResult struct {
	Input spriteInputInfo `json:"input"`
	sprite.Report
	Wrote []spriteWrite `json:"wrote,omitempty"`
	// Provenance is the record written beside the sprite when the pipeline started from a generated picture.
	Provenance string `json:"provenance,omitempty"`
	Preview    string `json:"preview,omitempty"`
	Note       string `json:"note,omitempty"`
}

type spriteInputInfo struct {
	Path   string `json:"path"`
	Format string `json:"format"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type spriteWrite struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// spriteRequest services one relo_sprite call. The key is checked like every hub call: bound to the live session and
// the agent's live runtime, with the agent still active. The Primary, a task that is not an art task, and a model that
// cannot see are refused; writing a file also needs a role that may mutate and a session in build mode.
func (c *Coord) spriteRequest(ctx context.Context, req agentmsg.SpriteRequest) (agentmsg.ImageResult, error) {
	if !c.artOn() {
		return agentmsg.ImageResult{}, errors.New("relo_sprite is not enabled")
	}
	a, sessionWorkspace, err := c.imageCaller(req.Key)
	if err != nil {
		return agentmsg.ImageResult{}, errors.New(strings.Replace(err.Error(), "image key", "sprite key", 1))
	}
	if a.Role == agent.Primary {
		return agentmsg.ImageResult{}, errors.New("relo_sprite is not available to the Primary; ask a worker to make the sprite")
	}
	if t, ok := c.Graph.Get(a.TaskID); !ok || !t.Art {
		return agentmsg.ImageResult{}, errors.New("relo_sprite is only available to tasks that make art")
	}
	if status := c.settings().VisionFor(a.Model); status == vision.No {
		return agentmsg.ImageResult{}, fmt.Errorf("your model (%s) cannot see images, so you could not check a sprite by eye. Report that the sprite work needs a model that sees", a.Model)
	}
	if req.Output != "" {
		if !agent.Mutating(a.Role) {
			return agentmsg.ImageResult{}, fmt.Errorf("the %s role cannot write files; leave output out and report what you found", a.Role)
		}
		if s := c.Current(); s == nil || s.Mode != ModeBuild {
			return agentmsg.ImageResult{}, errors.New("writing a sprite is available only in build mode")
		}
		if a.Workspace == "" {
			return agentmsg.ImageResult{}, errors.New("you have no workspace to write into")
		}
	}
	if err := c.reserveSprite(a.ID); err != nil {
		return agentmsg.ImageResult{}, err
	}
	roots := []string{a.Workspace}
	if sessionWorkspace != a.Workspace {
		roots = append(roots, sessionWorkspace)
	}
	release, err := c.acquireImageSlot(ctx)
	if err != nil {
		return agentmsg.ImageResult{}, errors.New("sprite request timed out waiting for another picture to finish")
	}
	defer release()

	session := c.SessionID()
	readFile := func(name string) ([]byte, error) { return c.readSpriteInput(session, sessionWorkspace, roots, name) }
	data, err := readFile(req.Input.Path)
	if err != nil {
		return agentmsg.ImageResult{}, err
	}
	img, format, err := sprite.Decode(data)
	if err != nil {
		return agentmsg.ImageResult{}, fmt.Errorf("%s: %w", req.Input.Path, err)
	}
	ops := req.Ops
	if len(ops) == 0 {
		ops = []sprite.Op{{Op: "inspect"}}
	}
	frames, report, err := sprite.Run(img, ops, sprite.Env{ReadFile: readFile, Output: req.Output})
	if err != nil {
		return agentmsg.ImageResult{}, err
	}
	result := spriteResult{
		Input:  spriteInputInfo{Path: req.Input.Path, Format: format, Width: img.Rect.Dx(), Height: img.Rect.Dy()},
		Report: *report,
	}
	var out agentmsg.ImageResult
	switch {
	case report.Aborted != "":
		result.Note = "Nothing was written. Fix what the check reported and run the pipeline again."
	case req.Output != "":
		wrote, err := writeSprites(a.Workspace, req.Output, req.Overwrite, frames)
		if err != nil {
			return agentmsg.ImageResult{}, err
		}
		result.Wrote = wrote
		result.Provenance = c.recordProvenance(a, sessionWorkspace, req.Input.Path, wrote)
		if report.Failed {
			result.Note = "The file was written, but a check failed. Fix it before you report the sprite as done."
		}
	}
	if (req.Preview || ops[len(ops)-1].Op == "sheet") && ctx.Err() == nil {
		pic, note, err := spritePreview(frames, ops[len(ops)-1].Op == "sheet")
		if err != nil {
			result.Preview = "no preview: " + err.Error()
		} else {
			out.Images = append(out.Images, agentmsg.Image{MIME: pic.MIME, Data: base64.StdEncoding.EncodeToString(pic.Data)})
			result.Preview = "image 1: " + note
		}
	}
	out.Text = result
	return out, nil
}

// spritePreview makes the picture an agent looks at: the sheet the pipeline ended in, or a sheet of its results.
func spritePreview(frames []*image.NRGBA, isSheet bool) (vision.Picture, string, error) {
	shown, note := frames[0], "the pipeline's contact sheet"
	if !isSheet {
		var err error
		shown, note, err = sprite.RenderSheet(frames, sprite.SheetOptions{Labels: true}, nil)
		if err != nil {
			return vision.Picture{}, "", err
		}
	}
	data, err := sprite.EncodePNG(shown)
	if err != nil {
		return vision.Picture{}, "", err
	}
	pic, err := vision.Prepare(bytes.NewReader(data), vision.Options{})
	if err != nil {
		return vision.Picture{}, "", err
	}
	return pic, note, nil
}

// readWorkspaceFile reads a file inside one of roots, which are searched in order.
func readWorkspaceFile(roots []string, name string) ([]byte, error) {
	f, _, err := vision.Open(roots, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, vision.MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: read failed", name)
	}
	if len(data) > vision.MaxFileBytes {
		return nil, fmt.Errorf("%s is larger than %d MiB", name, vision.MaxFileBytes>>20)
	}
	return data, nil
}

// readSpriteInput reads a picture a sprite pipeline names. A path under .relo is read only when it is a picture of one
// of the caller's own art jobs, in the form an art job returns it (.relo/art/raw/<job>/<n>.png). Any other .relo path
// is refused, and everything else is read from roots as before.
func (c *Coord) readSpriteInput(session, sessionWorkspace string, roots []string, name string) ([]byte, error) {
	rel, inRelo := spriteReloRel(roots, name)
	if !inRelo {
		return readWorkspaceFile(roots, name)
	}
	job, index, ok := artPictureRel(rel)
	if !ok {
		return nil, fmt.Errorf("%s: relo_sprite reads files under .relo only for pictures an art job returned (.relo/art/raw/<job>/<n>.png)", name)
	}
	mgr, err := c.artManager(sessionWorkspace)
	if err != nil {
		return nil, err
	}
	if _, err := artOwnJob(mgr, session, job); err != nil {
		return nil, err
	}
	data, err := mgr.CandidateData(job, index)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return data, nil
}

// spriteReloRel reports whether name, resolved in one of roots, lies under .relo, and returns that path relative to the
// root. It checks the path as written and with symlinks resolved, so a link into .relo does not get past the gate.
func spriteReloRel(roots []string, name string) (string, bool) {
	name = strings.TrimSpace(name)
	for _, root := range roots {
		if root == "" {
			continue
		}
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		resolved := filepath.Clean(path)
		if real, err := filepath.EvalSymlinks(path); err == nil {
			resolved = real
		}
		bases := []string{filepath.Clean(root)}
		if real, err := filepath.EvalSymlinks(root); err == nil {
			bases = append(bases, real)
		}
		for _, p := range []string{filepath.Clean(path), resolved} {
			for _, base := range bases {
				rel, err := filepath.Rel(base, p)
				if err != nil {
					continue
				}
				if rel = filepath.ToSlash(rel); rel == ".relo" || strings.HasPrefix(rel, ".relo/") {
					return rel, true
				}
			}
		}
	}
	return "", false
}

// artPictureRel parses a workspace-relative path of the form .relo/art/raw/<job>/<n>.png, the form an art job returns.
func artPictureRel(rel string) (string, int, bool) {
	rest, ok := strings.CutPrefix(rel, ".relo/art/raw/")
	if !ok {
		return "", 0, false
	}
	job, file, ok := strings.Cut(rest, "/")
	if !ok || job == "" || strings.Contains(file, "/") {
		return "", 0, false
	}
	stem, ok := strings.CutSuffix(file, ".png")
	if !ok {
		return "", 0, false
	}
	n, err := strconv.Atoi(stem)
	if err != nil || n < 1 || strconv.Itoa(n) != stem {
		return "", 0, false
	}
	return job, n, true
}

// writeSprites writes the pictures of a pipeline into root as PNG files. A single picture goes to output; several
// need {n} in it. Nothing is written unless every name is acceptable, so a bad one cannot leave half a set behind.
func writeSprites(root, output string, overwrite bool, frames []*image.NRGBA) ([]spriteWrite, error) {
	if len(frames) > maxSpriteWrites {
		return nil, fmt.Errorf("the pipeline made %d pictures; the most one call writes is %d", len(frames), maxSpriteWrites)
	}
	if len(frames) > 1 && !strings.Contains(output, "{n}") {
		return nil, fmt.Errorf("the pipeline ended with %d pictures, so output must contain {n}, which stands for each picture's number: for example %s", len(frames), strings.Replace(output, ".png", "_{n}.png", 1))
	}
	type target struct {
		rel  string
		data []byte
	}
	seen := map[string]bool{}
	targets := make([]target, 0, len(frames))
	for i, f := range frames {
		name := strings.ReplaceAll(output, "{n}", fmt.Sprint(i+1))
		rel, err := spritePath(root, name)
		if err != nil {
			return nil, err
		}
		if seen[rel] {
			return nil, fmt.Errorf("two pictures would be written to %s", rel)
		}
		seen[rel] = true
		data, err := sprite.EncodePNG(f)
		if err != nil {
			return nil, err
		}
		if len(data) > maxSpriteWriteBytes {
			return nil, fmt.Errorf("%s would be %d MiB; the most one file may be is %d MiB", rel, len(data)>>20, maxSpriteWriteBytes>>20)
		}
		targets = append(targets, target{rel, data})
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, errors.New("your workspace cannot be opened")
	}
	defer r.Close()
	if !overwrite {
		for _, t := range targets {
			if _, err := r.Lstat(t.rel); err == nil {
				return nil, fmt.Errorf("%s already exists; set overwrite to replace it, or choose another name", t.rel)
			}
		}
	}
	wrote := make([]spriteWrite, 0, len(targets))
	for _, t := range targets {
		if dir := filepath.Dir(t.rel); dir != "." {
			if err := r.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("%s: cannot create its folder inside your workspace", t.rel)
			}
		}
		// The file is written beside its destination and renamed into place, so a reader never sees half a PNG.
		tmp := t.rel + ".relo-tmp"
		if err := r.WriteFile(tmp, t.data, 0o644); err != nil {
			return nil, fmt.Errorf("%s: cannot be written inside your workspace", t.rel)
		}
		if err := r.Rename(tmp, t.rel); err != nil {
			_ = r.Remove(tmp)
			return nil, fmt.Errorf("%s: cannot be replaced inside your workspace", t.rel)
		}
		sum := sha256.Sum256(t.data)
		wrote = append(wrote, spriteWrite{Path: filepath.ToSlash(t.rel), Bytes: len(t.data), SHA256: hex.EncodeToString(sum[:])})
	}
	return wrote, nil
}

// spritePath turns the name an agent gave into a path relative to root, or says why it is not allowed. The name must
// be a .png inside the workspace and may not touch the repository's own folder or Relo's.
func spritePath(root, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsRune(name, 0) {
		return "", errors.New("output must name a file in your workspace")
	}
	rel := name
	if filepath.IsAbs(name) {
		bases := []string{filepath.Clean(root)}
		if real, err := filepath.EvalSymlinks(root); err == nil {
			bases = append(bases, real)
		}
		rel = ""
		for _, base := range bases {
			if r, err := filepath.Rel(base, filepath.Clean(name)); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				rel = r
				break
			}
		}
		if rel == "" {
			return "", fmt.Errorf("%s is outside your workspace", name)
		}
	}
	rel = filepath.Clean(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s is outside your workspace", name)
	}
	if !strings.EqualFold(filepath.Ext(rel), ".png") {
		return "", fmt.Errorf("%s: sprites are written as .png files", rel)
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.EqualFold(part, ".git") || strings.EqualFold(part, ".relo") {
			return "", fmt.Errorf("%s: files under %s belong to the repository or to Relo and cannot be written here", rel, part)
		}
	}
	return rel, nil
}

// reserveSprite counts one call against the agent's per-minute limit.
func (c *Coord) reserveSprite(agentID string) error {
	return c.reserveCall(agentID, maxSpritePerMinute, "sprite rate limit reached; put the steps of one sprite in a single call, and look at the result before you change it again")
}

// reserveCall counts one call against key's per-minute limit.
func (c *Coord) reserveCall(key string, limit int, message string) error {
	c.art.mu.Lock()
	defer c.art.mu.Unlock()
	if c.art.rate == nil {
		c.art.rate = map[string][]time.Time{}
	}
	now := time.Now()
	recent := c.art.rate[key][:0]
	for _, at := range c.art.rate[key] {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	if len(recent) >= limit {
		c.art.rate[key] = recent
		return errors.New(message)
	}
	c.art.rate[key] = append(recent, now)
	return nil
}
