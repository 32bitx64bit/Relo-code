package ctxeng

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/id"
)

type Engine struct {
	mu       sync.Mutex
	agents   map[string]*state
	findings map[string]Finding
	now      func() time.Time
}

type state struct {
	ID                string
	TaskID            string
	Role              string
	Phase             Phase
	Budget            int
	Strategy          string
	Scope             *Scope
	DependencyTaskIDs []string
	Pool              []Item
	Generations       []*Generation
	Metrics           Metrics
}

func New() *Engine {
	return &Engine{
		agents:   map[string]*state{},
		findings: map[string]Finding{},
		now:      func() time.Time { return time.Now().UTC() },
	}
}

func (e *Engine) Bind(agentID, taskID, role string, budget int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(agentID)
	st.TaskID = taskID
	st.Role = role
	if budget > 0 {
		st.Budget = budget
	}
}

func (e *Engine) SetPhase(agentID string, phase Phase) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensure(agentID).Phase = phase
}

func (e *Engine) SetStrategy(agentID, name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensure(agentID).Strategy = name
}

// hold reports whether the pool already has it, and keeps the stronger of the two flags when it does. Callers seed
// the same facts on every turn (the Primary's memory and objective, for one); each copy would be sent again in
// every call, and a run that started with 18 recalled entries carried 57 of them by its fifth turn.
// Conversation history is exempt: repeating it is a signal the loop detector counts.
func (st *state) hold(it Item) bool {
	if it.Class == ClassHistory || it.Class == ClassEphemeral {
		return false
	}
	for i := range st.Pool {
		have := &st.Pool[i]
		if have.Class != it.Class || have.Source != it.Source || have.Title != it.Title || have.Content != it.Content || have.Kind != it.Kind {
			continue
		}
		have.Pinned = have.Pinned || it.Pinned
		have.Verified = have.Verified || it.Verified
		return true
	}
	return false
}

func (e *Engine) Ingest(agentID string, items []Item) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(agentID)
	now := e.now()
	for _, it := range items {
		if it.ID == "" {
			it.ID = id.New("ctx")
		}
		if it.AgentID == "" {
			it.AgentID = agentID
		}
		if it.TaskID == "" {
			it.TaskID = st.TaskID
		}
		if it.Created.IsZero() {
			it.Created = now
		}
		EnsureTokens(&it)
		if st.hold(it) {
			continue
		}
		st.Pool = append(st.Pool, it)
		if it.Class == ClassHistory || it.Class == ClassEphemeral {
			key := fingerprint(it.Content)
			if st.Metrics.Repeats == nil {
				st.Metrics.Repeats = map[string]int{}
			}
			st.Metrics.Repeats[key]++
			if st.Metrics.Repeats[key] > 1 {
				st.Metrics.RepeatedActions++
			}
		}
		if it.Kind == KindBug || strings.Contains(strings.ToLower(it.Content), "edit failed") {
			st.Metrics.FailedEdits++
		}
		if Durable(it) && it.Class != ClassHistory {
			st.Metrics.LastProgress = now
		}
	}
}

func (e *Engine) Build(req BuildRequest) (*Generation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.buildLocked(req, false)
}

func (e *Engine) Refresh(agentID, reason string) (*Generation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(agentID)
	st.Metrics.Refreshes++
	st.Metrics.RefreshReasons = append(st.Metrics.RefreshReasons, reason)
	e.gcPool(st)
	return e.buildLocked(BuildRequest{
		AgentID:  agentID,
		TaskID:   st.TaskID,
		Role:     st.Role,
		Phase:    st.Phase,
		Budget:   st.Budget,
		Reason:   reason,
		Strategy: st.Strategy,
	}, true)
}

func (e *Engine) Replace(agentID string, items []Item, reason string) (*Generation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(agentID)
	var keep []Item
	for _, it := range st.Pool {
		if it.Pinned {
			keep = append(keep, it)
		}
	}
	st.Pool = keep
	now := e.now()
	for _, it := range items {
		if it.ID == "" {
			it.ID = id.New("ctx")
		}
		if it.Created.IsZero() {
			it.Created = now
		}
		EnsureTokens(&it)
		st.Pool = append(st.Pool, it)
	}
	st.Metrics.Refreshes++
	st.Metrics.RefreshReasons = append(st.Metrics.RefreshReasons, reason)
	return e.buildLocked(BuildRequest{
		AgentID: agentID,
		TaskID:  st.TaskID,
		Role:    st.Role,
		Phase:   st.Phase,
		Budget:  st.Budget,
		Reason:  reason,
	}, true)
}

func (e *Engine) Compress(agentID string) (*Generation, error) {
	return e.Refresh(agentID, "compress")
}

func (e *Engine) Fork(agentID string) (string, *Generation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	src := e.ensure(agentID)
	dstID := id.New("agent")
	cp := *src
	cp.ID = dstID
	cp.Generations = append([]*Generation{}, src.Generations...)
	cp.Pool = append([]Item{}, src.Pool...)
	cp.Metrics = cloneMetrics(src.Metrics)
	e.agents[dstID] = &cp
	gen, err := e.buildLocked(BuildRequest{
		AgentID: dstID,
		TaskID:  cp.TaskID,
		Role:    cp.Role,
		Phase:   cp.Phase,
		Budget:  cp.Budget,
		Reason:  "fork",
	}, false)
	return dstID, gen, err
}

func (e *Engine) Merge(dst, src string) (*Generation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.ensure(dst)
	b := e.ensure(src)
	for _, it := range b.Pool {
		if it.Class == ClassHistory || it.Class == ClassEphemeral {
			continue
		}
		if it.Class == ClassReport || Durable(it) {
			sum := ParentSummary(it)
			sum.ID = id.New("ctx")
			a.Pool = append(a.Pool, sum)
		}
	}
	return e.buildLocked(BuildRequest{
		AgentID: dst,
		TaskID:  a.TaskID,
		Role:    a.Role,
		Phase:   a.Phase,
		Budget:  a.Budget,
		Reason:  "merge:" + src,
	}, false)
}

func (e *Engine) Pin(agentID, itemID string) error {
	return e.flag(agentID, itemID, func(it *Item) { it.Pinned = true; it.Durability = 1 })
}

func (e *Engine) Unpin(agentID, itemID string) error {
	return e.flag(agentID, itemID, func(it *Item) { it.Pinned = false })
}

func (e *Engine) Drop(agentID, itemID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(agentID)
	kept := st.Pool[:0]
	found := false
	for _, it := range st.Pool {
		if it.ID == itemID {
			found = true
			if it.Pinned {
				kept = append(kept, it)
				continue
			}
			continue
		}
		kept = append(kept, it)
	}
	st.Pool = kept
	if !found {
		return fmt.Errorf("context item %s not found", itemID)
	}
	return nil
}

func (e *Engine) PromoteFinding(f Finding) Finding {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f.ID == "" {
		f.ID = id.New("find")
	}
	if f.Created.IsZero() {
		f.Created = e.now()
	}
	e.findings[f.ID] = f
	st := e.ensure(f.AgentID)
	it := Item{
		ID:         id.New("ctx"),
		Class:      ClassFinding,
		Source:     sourceForFinding(f),
		Content:    findingContent(f),
		AgentID:    f.AgentID,
		TaskID:     f.TaskID,
		Created:    f.Created,
		Confidence: f.Confidence,
		Relevance:  0.8,
		Durability: 0.9,
		Verified:   f.Verified,
		Files:      f.Files,
		Symbols:    f.Symbols,
		Kind:       f.Kind,
		Title:      f.Summary,
		FindingID:  f.ID,
		Phase:      st.Phase,
	}
	EnsureTokens(&it)
	st.Pool = append(st.Pool, it)
	return f
}

func (e *Engine) VerifyFinding(id string) (Finding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f, ok := e.findings[id]
	if !ok {
		return Finding{}, fmt.Errorf("finding %s not found", id)
	}
	f.Verified = true
	e.findings[id] = f
	for _, st := range e.agents {
		for i := range st.Pool {
			if st.Pool[i].FindingID == f.ID {
				st.Pool[i].Verified = true
				st.Pool[i].Source = SourceVerified
			}
		}
	}
	return f, nil
}

func (e *Engine) Findings() []Finding {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Finding, 0, len(e.findings))
	for _, f := range e.findings {
		out = append(out, f)
	}
	return out
}

func (e *Engine) Inspect(agentID string) Inspection {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(agentID)
	ins := Inspection{AgentID: agentID, Budget: st.Budget, Phase: st.Phase, Metrics: cloneMetrics(st.Metrics), Composition: map[string]int{}}
	if len(st.Generations) == 0 {
		return ins
	}
	gen := st.Generations[len(st.Generations)-1]
	ins.Generation = gen.Number
	ins.Used = gen.Tokens
	ins.Included = gen.Items
	ins.Reason = gen.Reason
	for _, it := range gen.Items {
		ins.Composition[string(it.Class)] += it.Tokens
	}
	for _, d := range gen.Dropped {
		ins.Excluded = append(ins.Excluded, d.Item)
	}
	return ins
}

func (e *Engine) Generation(agentID string, n int) *Generation {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(agentID)
	for _, g := range st.Generations {
		if g.Number == n {
			cp := *g
			return &cp
		}
	}
	if n <= 0 && len(st.Generations) > 0 {
		cp := *st.Generations[len(st.Generations)-1]
		return &cp
	}
	return nil
}

func (e *Engine) Generations(agentID string) []*Generation {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(agentID)
	out := make([]*Generation, len(st.Generations))
	copy(out, st.Generations)
	return out
}

func (e *Engine) RestoreGeneration(g Generation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(g.AgentID)
	if st.TaskID == "" {
		st.TaskID = g.TaskID
	}
	for _, existing := range st.Generations {
		if existing.Number == g.Number {
			return
		}
	}
	cp := g
	st.Generations = append(st.Generations, &cp)
	sort.SliceStable(st.Generations, func(i, j int) bool { return st.Generations[i].Number < st.Generations[j].Number })
	seen := make(map[string]bool, len(st.Pool))
	for _, it := range st.Pool {
		if it.ID != "" {
			seen[it.ID] = true
		}
	}
	for _, it := range g.Items {
		if it.ID != "" {
			if seen[it.ID] {
				continue
			}
			seen[it.ID] = true
		}
		st.Pool = append(st.Pool, it)
	}
	st.Phase, st.Budget, st.Strategy = g.Phase, g.Budget, g.Strategy
	st.Metrics.Generations = len(st.Generations)
}

func (e *Engine) Metrics(agentID string) Metrics {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneMetrics(e.ensure(agentID).Metrics)
}

// cloneMetrics returns a copy that shares no mutable state with m.
func cloneMetrics(m Metrics) Metrics {
	if m.Repeats != nil {
		repeats := make(map[string]int, len(m.Repeats))
		for k, v := range m.Repeats {
			repeats[k] = v
		}
		m.Repeats = repeats
	}
	m.RefreshReasons = append([]string(nil), m.RefreshReasons...)
	return m
}

func (e *Engine) Render(agentID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(agentID)
	if len(st.Generations) == 0 {
		return ""
	}
	return Render(st.Generations[len(st.Generations)-1])
}

func (e *Engine) Unhealthy(agentID string, threshold float64) (bool, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.unhealthyLocked(e.ensure(agentID), threshold)
}

func (e *Engine) SnapshotPool(agentID string) []Item {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Item{}, e.ensure(agentID).Pool...)
}

func (e *Engine) CurrentNumber(agentID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return nextNumber(e.ensure(agentID)) - 1
}

// nextNumber returns max(existing Number, count)+1 so restored generations never collide.
func nextNumber(st *state) int {
	n := len(st.Generations)
	for _, g := range st.Generations {
		if g.Number > n {
			n = g.Number
		}
	}
	return n + 1
}

func (e *Engine) buildLocked(req BuildRequest, boundary bool) (*Generation, error) {
	st := e.ensure(req.AgentID)
	if req.TaskID != "" {
		st.TaskID = req.TaskID
	}
	if req.Role != "" {
		st.Role = req.Role
	}
	if req.Phase != "" {
		st.Phase = req.Phase
	}
	if req.Budget > 0 {
		st.Budget = req.Budget
	}
	if st.Budget <= 0 {
		st.Budget = 24000
	}
	if req.Strategy != "" {
		st.Strategy = req.Strategy
	}
	now := e.now()
	for _, it := range req.Seed {
		if it.ID == "" {
			it.ID = id.New("ctx")
		}
		if it.Created.IsZero() {
			it.Created = now
		}
		EnsureTokens(&it)
		if st.hold(it) {
			continue
		}
		st.Pool = append(st.Pool, it)
	}
	applyScope := boundary || len(st.Generations) == 0
	if req.Scope != nil && applyScope {
		scope := *req.Scope
		if req.Scope.Classes != nil {
			scope.Classes = append([]Class{}, req.Scope.Classes...)
		}
		st.Scope = &scope
		st.DependencyTaskIDs = append([]string(nil), req.DependencyTaskIDs...)
	}
	scope := st.Scope
	dependencies := st.DependencyTaskIDs
	var scopeDropped []Dropped
	items := st.Pool
	if scope != nil {
		items, scopeDropped = applyScopeToItems(items, scope, dependencies)
	}
	pool := Retrieve(items, req.Query)
	if len(pool) == 0 {
		pool = items
	}
	strat := StrategyByName(st.Strategy)
	sel := strat.Select(pool, st.Budget, st.Role, st.Phase, now)
	sel.Dropped = append(sel.Dropped, scopeDropped...)
	var deferred []string
	if len(st.Generations) > 0 {
		prior := st.Generations[len(st.Generations)-1]
		if !boundary {
			sel.Included = appendNewInOrder(prior.Items, sel.Included)
			deferred = carryDeferred(prior, sel.Included)
		} else {
			previous := prior.Items
			if scope != nil {
				previous, _ = applyScopeToItems(previous, scope, dependencies)
			}
			sel.Included = retainWithinHysteresis(previous, sel.Included, st.Budget)
		}
	}
	gen := &Generation{
		Number:         nextNumber(st),
		AgentID:        st.ID,
		TaskID:         st.TaskID,
		Phase:          st.Phase,
		Reason:         req.Reason,
		Strategy:       strat.Name(),
		Items:          sel.Included,
		Dropped:        sel.Dropped,
		Compressed:     sel.Compressed,
		Deferred:       deferred,
		BuiltAt:        now,
		Tokens:         sel.Tokens,
		Budget:         st.Budget,
		OverflowTokens: sel.OverflowTokens,
	}
	if gen.Reason == "" {
		gen.Reason = "build"
	}
	gen.Tokens = Tokens(Render(gen))
	for i := 0; i < 8; i++ {
		n := Tokens(Render(gen))
		overflow := maxInt(0, n-gen.Budget)
		if n == gen.Tokens && overflow == gen.OverflowTokens {
			break
		}
		gen.Tokens = n
		gen.OverflowTokens = overflow
	}
	gen.OverflowTokens = maxInt(0, gen.Tokens-gen.Budget)
	st.Generations = append(st.Generations, gen)
	st.Metrics.Generations = len(st.Generations)
	st.Metrics.InputTokens = gen.Tokens
	if st.Budget > 0 {
		st.Metrics.Utilization = float64(gen.Tokens) / float64(st.Budget)
	}
	return gen, nil
}

// carryDeferred returns the deferred IDs for a non-boundary build: items already deferred by
// prior stay deferred, and stable items newly added by this build are deferred.
func carryDeferred(prior *Generation, items []Item) []string {
	priorIDs := make(map[string]bool, len(prior.Items))
	for _, it := range prior.Items {
		priorIDs[it.ID] = true
	}
	var out []string
	for _, it := range items {
		if prior.isDeferred(it.ID) || (!priorIDs[it.ID] && stableItem(it)) {
			out = append(out, it.ID)
		}
	}
	return out
}

func applyScopeToItems(items []Item, scope *Scope, dependencyTaskIDs []string) ([]Item, []Dropped) {
	allowed := make(map[Class]bool, len(scope.Classes))
	for _, class := range scope.Classes {
		allowed[class] = true
	}
	dependencies := make(map[string]bool, len(dependencyTaskIDs))
	for _, taskID := range dependencyTaskIDs {
		dependencies[taskID] = true
	}
	kept := make([]Item, 0, len(items))
	var dropped []Dropped
	for _, it := range items {
		if scope.Classes != nil && !allowed[it.Class] {
			dropped = append(dropped, Dropped{Item: it, Reason: "scope_class"})
			continue
		}
		if it.Class == ClassMemory && !scope.Memory {
			dropped = append(dropped, Dropped{Item: it, Reason: "scope_memory"})
			continue
		}
		if scope.DependencyReportsOnly && (it.Class == ClassReport || it.Source == SourceParent) && !dependencies[it.TaskID] {
			dropped = append(dropped, Dropped{Item: it, Reason: "scope_dependency"})
			continue
		}
		EnsureTokens(&it)
		if scope.MaxItemTokens > 0 && it.Tokens > scope.MaxItemTokens && !it.Pinned && Priority(it) != 1 {
			it.Content, it.Tokens = clippedExcerpt(it.Content, it.Files, it.Symbols, it.Tokens, scope.MaxItemTokens)
		}
		kept = append(kept, it)
	}
	return kept, dropped
}

func clippedExcerpt(content string, files, symbols []string, originalTokens, limit int) (string, int) {
	runes := []rune(content)
	var preserved []string
	for _, line := range strings.Split(content, "\n") {
		for _, value := range append(append([]string(nil), files...), symbols...) {
			if value != "" && strings.Contains(line, value) && !containsString(preserved, line) {
				preserved = append(preserved, line)
			}
		}
	}
	for _, value := range append(append([]string(nil), files...), symbols...) {
		if value != "" && !strings.Contains(content, value) && !containsString(preserved, value) {
			preserved = append(preserved, value)
		}
	}
	preserve := strings.Join(preserved, "\n")
	build := func(keep int) (string, int) {
		head, tail := keep/2, keep-keep/2
		excerpt := string(runes[:head])
		if tail > 0 {
			excerpt += "\n\n" + string(runes[len(runes)-tail:])
		}
		body := excerpt
		if preserve != "" {
			body += "\n" + preserve
		}
		clipped := maxInt(1, originalTokens-Tokens(excerpt))
		result := body + fmt.Sprintf("\n[clipped %d tokens]", clipped)
		return result, Tokens(result)
	}
	best, bestTokens := build(0)
	lo, hi := 0, len(runes)
	for lo <= hi {
		keep := (lo + hi) / 2
		candidate, count := build(keep)
		if count <= limit {
			best, bestTokens = candidate, count
			lo = keep + 1
		} else {
			hi = keep - 1
		}
	}
	return best, bestTokens
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func (e *Engine) ensure(agentID string) *state {
	if agentID == "" {
		agentID = id.New("agent")
	}
	st, ok := e.agents[agentID]
	if ok {
		return st
	}
	st = &state{
		ID:     agentID,
		Budget: 24000,
		Phase:  PhaseExplore,
		Metrics: Metrics{
			Repeats:      map[string]int{},
			LastProgress: e.now(),
		},
	}
	e.agents[agentID] = st
	return st
}

func (e *Engine) flag(agentID, itemID string, fn func(*Item)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(agentID)
	for i := range st.Pool {
		if st.Pool[i].ID == itemID {
			fn(&st.Pool[i])
			return nil
		}
	}
	return fmt.Errorf("context item %s not found", itemID)
}

func (e *Engine) gcPool(st *state) {
	kept := st.Pool[:0]
	for _, it := range st.Pool {
		if it.Pinned || Durable(it) {
			if it.Class == ClassEphemeral && !it.Pinned {
				continue
			}
			if it.Class == ClassHistory && !it.Pinned && strings.ToLower(st.Strategy) != "full_history" {
				continue
			}
			if it.Phase != "" && it.Phase != st.Phase && it.Class == ClassEphemeral {
				continue
			}
			kept = append(kept, it)
			continue
		}
		if it.Class == ClassEphemeral || it.Class == ClassHistory {
			continue
		}
		kept = append(kept, it)
	}
	st.Pool = kept
}

func fingerprint(s string) string {
	sum := sha1.Sum([]byte(strings.ToLower(strings.TrimSpace(s))))
	return hex.EncodeToString(sum[:6])
}

func appendNewInOrder(prior, selected []Item) []Item {
	out := append([]Item{}, prior...)
	seen := make(map[string]bool, len(out))
	for _, it := range out {
		seen[it.ID] = true
	}
	add := make([]Item, 0, len(selected))
	for _, it := range selected {
		if !seen[it.ID] {
			add = append(add, it)
			seen[it.ID] = true
		}
	}
	sort.Slice(add, func(i, j int) bool {
		pi, pj := Priority(add[i]), Priority(add[j])
		if pi != pj {
			return pi < pj
		}
		return itemKey(add[i]) < itemKey(add[j])
	})
	return append(out, add...)
}

func retainWithinHysteresis(prior, selected []Item, budget int) []Item {
	limit := budget + budget/10
	if SumTokens(prior) > limit {
		return selected
	}
	out := append([]Item{}, prior...)
	used := SumTokens(out)
	seen := make(map[string]bool, len(out))
	for _, it := range out {
		seen[it.ID] = true
	}
	add := make([]Item, 0, len(selected))
	for _, it := range selected {
		if !seen[it.ID] {
			add = append(add, it)
			seen[it.ID] = true
		}
	}
	sort.Slice(add, func(i, j int) bool {
		pi, pj := Priority(add[i]), Priority(add[j])
		if pi != pj {
			return pi < pj
		}
		return itemKey(add[i]) < itemKey(add[j])
	})
	for _, it := range add {
		if used+it.Tokens > limit && !it.Pinned && Priority(it) != 1 {
			break
		}
		out = append(out, it)
		used += it.Tokens
	}
	return out
}

func sourceForFinding(f Finding) Source {
	if f.Verified {
		return SourceVerified
	}
	if f.Confidence < 0.6 {
		return SourceHypothesis
	}
	return SourceWorker
}

func findingContent(f Finding) string {
	return CompressReport(f.Summary, f.Files, nil, nil, f.RootCause) + "\n\n" + strings.TrimSpace(f.Detail)
}

func Render(gen *Generation) string {
	if gen == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(RenderStable(gen))
	b.WriteString(RenderDynamic(gen))
	b.WriteString(RenderVolatile(gen))
	fmt.Fprintf(&b, "\n## Generation status\nGeneration: %d\nPhase: %s\nStrategy: %s\nBudget: %d tokens used of %d\nOverflow: %d tokens\nReason: %s\n", gen.Number, gen.Phase, gen.Strategy, gen.Tokens, gen.Budget, gen.OverflowTokens, gen.Reason)
	return b.String()
}

func RenderStable(gen *Generation) string {
	if gen == nil {
		return ""
	}
	var b strings.Builder
	stable := make([]Item, 0, len(gen.Items))
	for _, it := range gen.Items {
		if stableItem(it) && !gen.isDeferred(it.ID) {
			stable = append(stable, it)
		}
	}
	sortItems(stable)
	if len(stable) == 0 {
		return ""
	}
	b.WriteString("# Session-stable context\n\n")
	for _, it := range stable {
		renderItem(&b, it)
	}
	return b.String()
}

func RenderDynamic(gen *Generation) string {
	if gen == nil {
		return ""
	}
	var b strings.Builder
	lastClass := Class("")
	for _, it := range gen.Items {
		if (stableItem(it) && !gen.isDeferred(it.ID)) || volatileItem(it) {
			continue
		}
		if lastClass != it.Class {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "## %s\n", it.Class)
			lastClass = it.Class
		}
		renderItem(&b, it)
	}
	return b.String()
}

func RenderVolatile(gen *Generation) string {
	if gen == nil {
		return ""
	}
	items := make([]Item, 0)
	for _, it := range gen.Items {
		if volatileItem(it) {
			items = append(items, it)
		}
	}
	var b strings.Builder
	if len(items) > 0 {
		sortItems(items)
		b.WriteString("## Current user input and failures\n")
		for _, it := range items {
			renderItem(&b, it)
		}
	}
	if line := excludedSummary(gen.Dropped); line != "" {
		b.WriteString("## Intentionally excluded\n")
		b.WriteString(line)
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}

// excludedSummary says how much was left out and why, in one line. It used to list every dropped item with its
// first line, which for an agent whose scope keeps only its task was most of what it was sent: a probe received
// 3.7k tokens describing eighteen memory entries it was not meant to see.
func excludedSummary(dropped []Dropped) string {
	if len(dropped) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, d := range dropped {
		counts[fmt.Sprintf("%s (%s)", d.Item.Class, d.Reason)]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	return "Left out of this context: " + strings.Join(parts, "; ") + "."
}

func stableItem(it Item) bool {
	// Volatile items stay out of the cached block even when pinned.
	if volatileItem(it) {
		return false
	}
	if it.Class == ClassMemory && it.Kind == KindArchitecture {
		return true
	}
	if it.Class != ClassTask {
		return it.Pinned
	}
	return true
}

func volatileItem(it Item) bool {
	title := strings.ToLower(it.Title)
	return it.Source == SourceTest || strings.Contains(title, "latest failure") || strings.Contains(title, "test failure") || strings.Contains(title, "latest user") || (it.Class == ClassTask && (title == "redirect" || (it.Pinned && it.Title == "" && it.Kind != KindConstraint)))
}

func renderItem(b *strings.Builder, it Item) {
	fmt.Fprintf(b, "### %s [%s]\n", label(it), it.Source)
	if it.Pinned {
		b.WriteString("PINNED - must survive compression.\n")
	}
	if it.Verified {
		b.WriteString("Verified.\n")
	} else if it.Source == SourceHypothesis {
		b.WriteString("Unverified hypothesis - do not treat as fact.\n")
	}
	b.WriteString(it.Content)
	b.WriteString("\n\n")
}

func sortItems(items []Item) {
	sort.Slice(items, func(i, j int) bool {
		pi, pj := Priority(items[i]), Priority(items[j])
		if pi != pj {
			return pi < pj
		}
		return stableItemKey(items[i]) < stableItemKey(items[j])
	})
}

func stableItemKey(it Item) string {
	value := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%t\x00%t", it.Class, it.Source, it.Title, it.Content, it.Pinned, it.Verified)
	sum := sha256.Sum256([]byte(value))
	// Kind is not hashed so existing sort order is preserved; it only breaks ties between otherwise identical items.
	return hex.EncodeToString(sum[:]) + value + "\x00kind\x00" + it.Kind
}

func itemKey(it Item) string { return stableItemKey(it) }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func label(it Item) string {
	if it.Title != "" {
		return it.Title
	}
	if it.Kind != "" {
		return it.Kind
	}
	return string(it.Class)
}
