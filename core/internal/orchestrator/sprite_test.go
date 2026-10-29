package orchestrator

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/art"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/sprite"
	"github.com/relocode/relo/internal/taskgraph"
)

var (
	spriteKey    = color.NRGBA{255, 0, 255, 255}
	spriteOrange = color.NRGBA{230, 120, 20, 255}
)

// writeDiscs writes a picture of n orange discs on a magenta background, each in its own 48 pixel cell.
func writeDiscs(t *testing.T, path string, n int) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 48*n, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 48*n; x++ {
			dx, dy := x%48-24, y-24
			if dx*dx+dy*dy <= 14*14 {
				img.SetNRGBA(x, y, spriteOrange)
			} else {
				img.SetNRGBA(x, y, spriteKey)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readPNG(t *testing.T, path string) *image.NRGBA {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := sprite.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// spriteFixture is a bound art worker. The agent's own workspace is not the session's, as with a virtual workspace, so
// a test can tell where a read comes from and where a write lands.
type spriteFixture struct {
	c       *Coord
	key     string
	session string // the session workspace, where project files live
	work    string // the agent's workspace, where writes land
}

func newSpriteFixture(t *testing.T, role agent.Role, model string, art bool) *spriteFixture {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(&peerRuntime{Mock: runtime.NewMock()})
	c.UpdateSettings(func(s *Settings) { s.Models, s.ModelsKnown = catalog(), true })
	c.SetArtEnabled(true)
	session, work := t.TempDir(), t.TempDir()
	c.CreateSession(session, "make the iron sprite", ModeBuild)
	task := c.Graph.Add(taskgraph.Task{Title: "Iron sprite", Role: string(role), Workspace: work, Art: art, Vision: art})
	a := c.Agents.Create(agent.Agent{ID: "artist", Role: role, Workspace: work, TaskID: task.ID, Status: agent.Running, Model: model})
	c.runtimeSessions[a.ID] = &runtimeSession{id: "runtime-artist"}
	key, err := c.bindPeer(a, "runtime-artist")
	if err != nil {
		t.Fatal(err)
	}
	return &spriteFixture{c: c, key: key, session: session, work: work}
}

func (f *spriteFixture) call(req agentmsg.SpriteRequest) (agentmsg.ImageResult, error) {
	if req.Key == "" {
		req.Key = f.key
	}
	return f.c.spriteRequest(context.Background(), req)
}

func spriteMeta(t *testing.T, res agentmsg.ImageResult) map[string]any {
	t.Helper()
	return imageMetadata(t, agentmsg.ImageResult{Text: res.Text})
}

// ironOps is the pipeline an agent runs for a typical item sprite.
func ironOps() []sprite.Op {
	return []sprite.Op{
		{Op: "matte"},
		{Op: "fit", Size: []int{32, 32}, Margin: 2},
		{Op: "check", Rules: &sprite.Rules{Width: 32, Height: 32, Alpha: true}},
	}
}

func TestSpriteRequestMakesAndWritesASprite(t *testing.T) {
	f := newSpriteFixture(t, agent.Worker, "p/seeing", true)
	writeDiscs(t, filepath.Join(f.session, "art", "raw", "iron.png"), 1)

	res, err := f.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "art/raw/iron.png"}, Ops: ironOps(), Output: "Textures/Iron.png", Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	meta := spriteMeta(t, res)
	wrote, _ := meta["wrote"].([]any)
	if len(wrote) != 1 || wrote[0].(map[string]any)["path"] != "Textures/Iron.png" {
		t.Fatalf("wrote = %v", meta["wrote"])
	}
	if meta["failed"] == true || meta["aborted"] != nil {
		t.Fatalf("a good sprite failed its checks: %v", meta)
	}
	out := readPNG(t, filepath.Join(f.work, "Textures", "Iron.png"))
	if out.Rect.Dx() != 32 || out.Rect.Dy() != 32 {
		t.Fatalf("size = %v", out.Rect)
	}
	if out.NRGBAAt(0, 0).A != 0 || out.NRGBAAt(16, 16).A != 255 {
		t.Fatalf("corner alpha %d, centre alpha %d: the background was not removed", out.NRGBAAt(0, 0).A, out.NRGBAAt(16, 16).A)
	}
	if c := out.NRGBAAt(16, 16); c.R < 200 || c.B > 60 {
		t.Fatalf("centre colour changed to %v", c)
	}
	if _, err := os.Stat(filepath.Join(f.session, "Textures", "Iron.png")); err == nil {
		t.Fatal("the sprite was written to the session workspace instead of the agent's own")
	}
	if len(res.Images) != 1 || res.Images[0].MIME == "" || res.Images[0].Data == "" {
		t.Fatalf("preview = %+v", res.Images)
	}
}

func TestSpriteRequestWithoutOpsInspects(t *testing.T) {
	f := newSpriteFixture(t, agent.Worker, "p/seeing", true)
	writeDiscs(t, filepath.Join(f.work, "a.png"), 1)
	res, err := f.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "a.png"}})
	if err != nil {
		t.Fatal(err)
	}
	meta := spriteMeta(t, res)
	if in := meta["input"].(map[string]any); in["format"] != "png" || in["width"] != float64(48) {
		t.Fatalf("input = %v", in)
	}
	if len(res.Images) != 0 {
		t.Fatal("a picture came back without preview")
	}
	if entries, _ := os.ReadDir(f.work); len(entries) != 1 {
		t.Fatalf("an inspect wrote files: %v", entries)
	}
}

func TestSpriteRequestRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*spriteFixture) agentmsg.SpriteRequest
		role  agent.Role
		model string
		art   bool
		want  string
	}{
		{name: "disabled", want: "not enabled", role: agent.Worker, model: "p/seeing", art: true,
			setup: func(f *spriteFixture) agentmsg.SpriteRequest {
				f.c.SetArtEnabled(false)
				return agentmsg.SpriteRequest{}
			}},
		{name: "primary", want: "Primary", role: agent.Primary, model: "p/seeing", art: true},
		{name: "not an art task", want: "make art", role: agent.Worker, model: "p/seeing", art: false},
		{name: "text only model", want: "cannot see images", role: agent.Worker, model: "p/blind", art: true},
		{name: "unknown key", want: "sprite key", role: agent.Worker, model: "p/seeing", art: true,
			setup: func(*spriteFixture) agentmsg.SpriteRequest { return agentmsg.SpriteRequest{Key: "not-a-key"} }},
		{name: "reviewer cannot write", want: "cannot write", role: agent.Reviewer, model: "p/seeing", art: true,
			setup: func(*spriteFixture) agentmsg.SpriteRequest { return agentmsg.SpriteRequest{Output: "out.png"} }},
		{name: "plan mode cannot write", want: "build mode", role: agent.Worker, model: "p/seeing", art: true,
			setup: func(f *spriteFixture) agentmsg.SpriteRequest {
				f.c.SetMode(ModePlan)
				return agentmsg.SpriteRequest{Output: "out.png"}
			}},
		{name: "input outside the workspace", want: "outside", role: agent.Worker, model: "p/seeing", art: true,
			setup: func(*spriteFixture) agentmsg.SpriteRequest {
				outside := filepath.Join(t.TempDir(), "secret.png")
				writeDiscs(t, outside, 1)
				return agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: outside}}
			}},
		{name: "input missing", want: "nope.png", role: agent.Worker, model: "p/seeing", art: true,
			setup: func(*spriteFixture) agentmsg.SpriteRequest {
				return agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "nope.png"}}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSpriteFixture(t, tc.role, tc.model, tc.art)
			writeDiscs(t, filepath.Join(f.work, "a.png"), 1)
			req := agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "a.png"}}
			if tc.setup != nil {
				if r := tc.setup(f); r.Key != "" || r.Output != "" || r.Input.Path != "" {
					req = r
					if req.Input.Path == "" {
						req.Input.Path = "a.png"
					}
				}
			}
			_, err := f.call(req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if entries, _ := os.ReadDir(f.work); len(entries) != 1 {
				t.Fatalf("a refused call wrote files: %v", entries)
			}
		})
	}
}

func TestSpriteRequestWritesOnlyInsideTheWorkspace(t *testing.T) {
	f := newSpriteFixture(t, agent.Worker, "p/seeing", true)
	writeDiscs(t, filepath.Join(f.work, "a.png"), 1)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(f.work, "link")); err != nil {
		t.Skip("symlinks are not available:", err)
	}
	for name, output := range map[string]string{
		"git":             ".git/hooks/x.png",
		"relo":            ".relo/art/x.png",
		"nested git":      "sub/.GIT/x.png",
		"parent":          "../x.png",
		"deep parent":     "a/../../x.png",
		"absolute":        filepath.Join(outside, "x.png"),
		"not a png":       "x.jpg",
		"no extension":    "x",
		"through symlink": "link/x.png",
	} {
		_, err := f.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "a.png"}, Ops: []sprite.Op{{Op: "trim"}}, Output: output})
		if err == nil {
			t.Errorf("%s: %q was written", name, output)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("a file escaped the workspace: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.work), "x.png")); err == nil {
		t.Fatal("a file was written above the workspace")
	}
}

func TestSpriteRequestAcceptsAnAbsolutePathInsideTheWorkspace(t *testing.T) {
	f := newSpriteFixture(t, agent.Worker, "p/seeing", true)
	writeDiscs(t, filepath.Join(f.work, "a.png"), 1)
	target := filepath.Join(f.work, "out", "iron.png")
	if _, err := f.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "a.png"}, Ops: []sprite.Op{{Op: "trim"}}, Output: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
}

func TestSpriteRequestOverwriteNeedsPermission(t *testing.T) {
	f := newSpriteFixture(t, agent.Worker, "p/seeing", true)
	writeDiscs(t, filepath.Join(f.work, "a.png"), 1)
	target := filepath.Join(f.work, "iron.png")
	req := agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "a.png"}, Ops: []sprite.Op{{Op: "trim"}}, Output: "iron.png"}
	if _, err := f.call(req); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(target)
	req.Ops = []sprite.Op{{Op: "matte"}, {Op: "trim"}}
	if _, err := f.call(req); err == nil || !strings.Contains(err.Error(), "overwrite") {
		t.Fatalf("an existing file was replaced without overwrite: %v", err)
	}
	if again, _ := os.ReadFile(target); !bytes.Equal(first, again) {
		t.Fatal("a refused write changed the file")
	}
	req.Overwrite = true
	if _, err := f.call(req); err != nil {
		t.Fatal(err)
	}
	if replaced, _ := os.ReadFile(target); bytes.Equal(first, replaced) {
		t.Fatal("overwrite did not replace the file")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(f.work, "*.relo-tmp")); len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestSpriteRequestWritesEveryPictureOfASlice(t *testing.T) {
	f := newSpriteFixture(t, agent.Worker, "p/seeing", true)
	writeDiscs(t, filepath.Join(f.work, "sheet.png"), 3)
	ops := []sprite.Op{{Op: "matte"}, {Op: "slice", Cols: 3, Rows: 1}, {Op: "fit", Size: []int{16, 16}}}

	if _, err := f.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "sheet.png"}, Ops: ops, Output: "out/iron.png"}); err == nil || !strings.Contains(err.Error(), "{n}") {
		t.Fatalf("three pictures went to one name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.work, "out")); err == nil {
		t.Fatal("a rejected set left a folder behind")
	}

	res, err := f.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "sheet.png"}, Ops: ops, Output: "out/iron_{n}.png"})
	if err != nil {
		t.Fatal(err)
	}
	if wrote := spriteMeta(t, res)["wrote"].([]any); len(wrote) != 3 {
		t.Fatalf("wrote %d files, want 3", len(wrote))
	}
	for _, name := range []string{"iron_1.png", "iron_2.png", "iron_3.png"} {
		if img := readPNG(t, filepath.Join(f.work, "out", name)); img.Rect.Dx() != 16 {
			t.Fatalf("%s is %v", name, img.Rect)
		}
	}
}

func TestSpriteRequestStrictFailureWritesNothing(t *testing.T) {
	f := newSpriteFixture(t, agent.Worker, "p/seeing", true)
	writeDiscs(t, filepath.Join(f.work, "a.png"), 1)
	ops := []sprite.Op{{Op: "matte"}, {Op: "fit", Size: []int{32, 32}}, {Op: "check", Rules: &sprite.Rules{Width: 64}, Strict: true}}
	res, err := f.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "a.png"}, Ops: ops, Output: "iron.png"})
	if err != nil {
		t.Fatal(err)
	}
	meta := spriteMeta(t, res)
	if meta["aborted"] == nil || meta["wrote"] != nil || !strings.Contains(meta["note"].(string), "Nothing was written") {
		t.Fatalf("meta = %v", meta)
	}
	if _, err := os.Stat(filepath.Join(f.work, "iron.png")); err == nil {
		t.Fatal("a strict failure still wrote the file")
	}
}

func TestSpriteRequestLenientFailureWritesAndSaysSo(t *testing.T) {
	f := newSpriteFixture(t, agent.Worker, "p/seeing", true)
	writeDiscs(t, filepath.Join(f.work, "a.png"), 1)
	ops := []sprite.Op{{Op: "matte"}, {Op: "fit", Size: []int{32, 32}}, {Op: "check", Rules: &sprite.Rules{Width: 64}}}
	res, err := f.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "a.png"}, Ops: ops, Output: "iron.png"})
	if err != nil {
		t.Fatal(err)
	}
	meta := spriteMeta(t, res)
	if meta["failed"] != true || !strings.Contains(meta["note"].(string), "check failed") {
		t.Fatalf("meta = %v", meta)
	}
	if _, err := os.Stat(filepath.Join(f.work, "iron.png")); err != nil {
		t.Fatal("a lenient check should still write the file")
	}
}

func TestSpriteRequestSheetOpReturnsThePicture(t *testing.T) {
	f := newSpriteFixture(t, agent.Worker, "p/seeing", true)
	writeDiscs(t, filepath.Join(f.work, "a.png"), 1)
	res, err := f.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "a.png"}, Ops: []sprite.Op{{Op: "matte"}, {Op: "sheet"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 {
		t.Fatalf("a sheet pipeline returned %d pictures", len(res.Images))
	}
}

func TestSpriteRequestIsRateLimited(t *testing.T) {
	f := newSpriteFixture(t, agent.Worker, "p/seeing", true)
	writeDiscs(t, filepath.Join(f.work, "a.png"), 1)
	req := agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "a.png"}}
	for i := 0; i < maxSpritePerMinute; i++ {
		if _, err := f.call(req); err != nil {
			t.Fatalf("call %d refused early: %v", i+1, err)
		}
	}
	if _, err := f.call(req); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("rate limit not applied: %v", err)
	}
}

func TestSpriteRequestRejectsPicturesThatAreNotImages(t *testing.T) {
	f := newSpriteFixture(t, agent.Worker, "p/seeing", true)
	if err := os.WriteFile(filepath.Join(f.work, "fake.png"), []byte("not a picture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "fake.png"}}); err == nil {
		t.Fatal("a file that is not an image was accepted")
	}
}

func TestSpriteRequestMaskIsReadFromTheWorkspaceToo(t *testing.T) {
	f := newSpriteFixture(t, agent.Worker, "p/seeing", true)
	writeDiscs(t, filepath.Join(f.work, "a.png"), 1)
	if _, err := f.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: "a.png"}, Ops: []sprite.Op{{Op: "matte", Mode: "mask", Mask: "../../etc/passwd"}}}); err == nil {
		t.Fatal("a mask outside the workspace was read")
	}
}

// spriteExposure runs one turn for a role and returns the prompt the model saw and the tool policy that turn carried.
func spriteExposure(t *testing.T, role agent.Role, model string, art, enabled bool, mode Mode) (string, map[string]bool) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	rt := &canvasExposureRuntime{Mock: runtime.NewMock()}
	c.SetRuntime(rt)
	c.UpdateSettings(func(s *Settings) { s.Models, s.ModelsKnown = catalog(), true })
	c.SetArtEnabled(enabled)
	work := t.TempDir()
	c.CreateSession(work, "make the iron sprite", mode)
	task := c.Graph.Add(taskgraph.Task{Title: "Iron sprite", Role: string(role), Workspace: work, Art: art})
	a := c.Agents.Create(agent.Agent{ID: "exposure-" + string(role), Role: role, Workspace: work, TaskID: task.ID, Status: agent.Running, Model: model})
	if _, err := c.runAgent(context.Background(), a, prompt.Parts{}, "do the work", nil); err != nil {
		t.Fatalf("runAgent(%s): %v", role, err)
	}
	last := rt.last()
	return last.Prompt, last.Tools
}

func TestSpriteToolIsOfferedOnlyToArtBuilders(t *testing.T) {
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
			if tools[runtime.ReloSpriteTool] != tc.offered {
				t.Fatalf("relo_sprite offered = %v, want %v", tools[runtime.ReloSpriteTool], tc.offered)
			}
			if got := strings.Contains(promptText, "relo_sprite turns a picture"); got != tc.offered {
				t.Fatalf("guidance present = %v, want %v:\n%s", got, tc.offered, promptText)
			}
		})
	}
}

func TestSetArtEnabledRebuildsTheHubCatalog(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	t.Cleanup(c.CloseMessages)
	if err := c.StartMessages(); err != nil {
		t.Fatal(err)
	}
	first := c.messageServer
	c.SetArtEnabled(false)
	if c.messageServer != first {
		t.Fatal("switching off something already off rebuilt the hub")
	}
	c.SetArtEnabled(true)
	if c.messageServer != nil || !c.ArtEnabled() {
		t.Fatal("switching art on kept the old hub, which has no relo_sprite")
	}
	if err := c.StartMessages(); err != nil {
		t.Fatal(err)
	}
	if c.messageServer == nil || c.messageServer == first {
		t.Fatal("the hub was not rebuilt")
	}
}

func TestParseTurnReadsTheArtFlag(t *testing.T) {
	turn, err := ParseTurn(`{"status":"working","userMessage":"x","tasks":[{"id":"a","title":"Iron sprite","role":"worker","art":true},{"id":"b","title":"Wire the def","role":"worker"}]}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Tasks) != 2 || !turn.Tasks[0].Art || turn.Tasks[1].Art {
		t.Fatalf("tasks = %+v", turn.Tasks)
	}
}

// An art task needs a model that sees what it makes, so the flag implies vision and routes like a vision task. The tool
// itself is the feature's to switch: with art off, the flag is ignored rather than trusted.
func TestApplyTurnArtFlagImpliesVisionAndFollowsTheFeature(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		c := New(logging.New(nil, logging.Error))
		c.UpdateSettings(func(s *Settings) {
			s.Models, s.ModelsKnown = catalog(), true
			s.PrimaryModel, s.Tier1Model, s.Tier2Model = "p/seeing", "p/blind", "p/blind"
			s.RoutingMode = routing.ModeSimple
		})
		c.SetArtEnabled(enabled)
		c.CreateSession(t.TempDir(), "", ModeBuild)
		c.applyTurn(agent.Agent{}, Turn{Tasks: []TurnTask{
			{ID: "a", Title: "Iron sprite", Role: "worker", Difficulty: routing.Medium, Art: true},
			{ID: "b", Title: "Wire the def", Role: "worker", Difficulty: routing.Medium},
		}})
		iron, wire := findTaskByTitle(t, c, "Iron sprite"), findTaskByTitle(t, c, "Wire the def")
		if !iron.Vision || iron.Model != "p/seeing" {
			t.Errorf("enabled=%v: art task vision=%v model=%q, want vision on a model that sees", enabled, iron.Vision, iron.Model)
		}
		if iron.Art != enabled {
			t.Errorf("enabled=%v: art task Art=%v", enabled, iron.Art)
		}
		if wire.Art || wire.Vision || wire.Model != "p/blind" {
			t.Errorf("enabled=%v: an ordinary task became %+v", enabled, wire)
		}
	}
}

// makeArtPicture stores a ready art job owned by session, with one picture, and returns the job id.
func makeArtPicture(t *testing.T, f *spriteFixture, session string) string {
	t.Helper()
	mgr, err := f.c.artManager(f.session)
	if err != nil {
		t.Fatal(err)
	}
	j, err := mgr.Store().Create(art.Job{SessionID: session, State: art.StateReady, Prompt: "iron ingot",
		Candidates: []art.Candidate{{Index: 1, File: "1.png"}}})
	if err != nil {
		t.Fatal(err)
	}
	writeDiscs(t, filepath.Join(j.Dir, "1.png"), 1)
	return j.ID
}

// TestSpriteRequestReadsOnlyTheCallersArtPictures: a job's own picture under .relo/art/raw is readable; every other
// .relo path, another session's job picture, and any route to them through .. or a symlink is refused.
func TestSpriteRequestReadsOnlyTheCallersArtPictures(t *testing.T) {
	f := newSpriteFixture(t, agent.Worker, "p/seeing", true)
	t.Cleanup(f.c.CloseArt)
	own := makeArtPicture(t, f, f.c.SessionID())
	other := makeArtPicture(t, f, "some-other-session")
	writeDiscs(t, filepath.Join(f.session, ".relo", "notes", "secret.png"), 1)
	if err := os.MkdirAll(filepath.Join(f.session, "art"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", ".relo", "notes"), filepath.Join(f.session, "art", "link")); err != nil {
		t.Fatal(err)
	}

	res, err := f.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: ".relo/art/raw/" + own + "/1.png"}, Ops: ironOps(), Output: "Textures/Own.png"})
	if err != nil {
		t.Fatalf("the job's own picture was refused: %v", err)
	}
	if meta := spriteMeta(t, res); meta["failed"] == true || meta["aborted"] != nil {
		t.Fatalf("own picture failed its checks: %v", meta)
	}
	readPNG(t, filepath.Join(f.work, "Textures", "Own.png"))

	refused := []string{
		".relo/notes/secret.png",
		".relo/art/raw/" + other + "/1.png",
		".relo/art/raw/" + own + "/2.png",
		".relo/art/jobs/" + own + "/job.json",
		".relo/art/raw/../../notes/secret.png",
		"art/link/secret.png",
		filepath.Join(f.session, ".relo", "notes", "secret.png"),
	}
	for _, p := range refused {
		if _, err := f.call(agentmsg.SpriteRequest{Input: agentmsg.SpriteInput{Path: p}}); err == nil {
			t.Errorf("relo_sprite read %s", p)
		}
	}
}
