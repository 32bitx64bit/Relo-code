package web

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/relocode/relo/internal/web/browser"
	"github.com/relocode/relo/internal/web/netguard"
	"github.com/relocode/relo/internal/web/search"
)

// ErrNotImplemented remains for source compatibility with the original skeleton.
var ErrNotImplemented = errors.New("web: not implemented")

var (
	errEngineClosed = errors.New("web: engine is closed")
	errTooManyTabs  = errors.New("web: global tab limit reached")
)

// MaxWait bounds one Wait call. It stays under the 65s agent tool timeout.
const MaxWait = 40 * time.Second

// Status describes the engine and the browser it would use, without starting either.
type Status struct {
	Ready       bool         `json:"ready"`
	Mode        Mode         `json:"mode,omitempty"`
	BrowserPath string       `json:"browserPath,omitempty"`
	Provider    ProviderName `json:"provider,omitempty"`
	Tabs        int          `json:"tabs"`
	Agents      int          `json:"agents"`
	// Notes carry engine remarks, such as software rendering after an ANGLE
	// launch failure.
	Notes []string `json:"notes,omitempty"`
}

type agentState struct {
	id         string
	ctx        context.Context
	cancel     context.CancelFunc
	limiter    *rateLimiter
	tab        *agentTab
	current    string
	lastSearch *searchState
	forgotten  bool
}

type agentTab struct {
	tier    Tier
	tab     browserTab
	refusal *guardRefusal // a refused page still reported by the next read of this tab
}

// Engine is the web capability. Browser processes are shared by lane, while
// each agent has its own tab, operation context and rate limiter.
type Engine struct {
	mu       sync.Mutex
	settings Settings
	hooks    Hooks
	ctx      context.Context
	cancel   context.CancelFunc
	closed   bool

	agents map[string]*agentState
	ops    map[string]*operation
	lanes  map[Tier]*laneSlot
	proxy  *guardProxy
	opSeq  uint64
	tabs   int

	global      *rateLimiter
	pacer       *domainPacer
	laneFactory laneFactory // tests may replace this before any lane is opened
	urlCheck    func(Settings, string) error

	// softwareGL holds the tiers whose ANGLE launch failed; later launches for
	// that tier skip the measured ANGLE flags.
	softwareGL map[Tier]bool
}

// softwareGLFor reports whether tier has been switched to software rendering.
// Tests use it to feed the real launch decision through a fake lane factory.
func (e *Engine) softwareGLFor(tier Tier) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.softwareGL[tier]
}

// rememberSoftwareGL records that tier needs the software GL opt-out.
func (e *Engine) rememberSoftwareGL(tier Tier) {
	e.mu.Lock()
	if e.softwareGL == nil {
		e.softwareGL = make(map[Tier]bool)
	}
	e.softwareGL[tier] = true
	e.mu.Unlock()
}

// New returns an engine for the given settings and hooks. It starts no browser
// and no listener; both are lazy.
func New(settings Settings, hooks Hooks) (*Engine, error) {
	normalized, err := normalizeSettings(settings)
	if err != nil {
		return nil, err
	}
	provider, err := configuredProvider(normalized)
	if err != nil {
		return nil, err
	}
	if _, err := providerOrigin(provider, normalized); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{
		settings:   normalized,
		hooks:      hooks,
		ctx:        ctx,
		cancel:     cancel,
		agents:     make(map[string]*agentState),
		ops:        make(map[string]*operation),
		lanes:      make(map[Tier]*laneSlot),
		softwareGL: make(map[Tier]bool),
		global:     newRateLimiter(normalized.RateLimitGlobal),
		pacer:      newDomainPacer(defaultDomainInterval),
	}, nil
}

// SetSettings replaces the configuration. Open tabs and their lanes are kept.
func (e *Engine) SetSettings(settings Settings) error {
	normalized, err := normalizeSettings(settings)
	if err != nil {
		return err
	}
	provider, err := configuredProvider(normalized)
	if err != nil {
		return err
	}
	origin, err := providerOrigin(provider, normalized)
	if err != nil {
		return err
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errEngineClosed
	}
	e.settings = normalized
	e.global.setRate(normalized.RateLimitGlobal)
	for _, agent := range e.agents {
		agent.limiter.setRate(normalized.RateLimitPerAgent)
	}
	proxy := e.proxy
	e.mu.Unlock()
	if proxy != nil {
		proxy.setOrigin(origin)
	}
	return nil
}

// Do starts one asynchronous web action. Its lifetime belongs to the agent,
// not the caller's ctx; Wait collects the result.
func (e *Engine) Do(_ context.Context, agentID string, req Request) (Result, error) {
	if agentID == "" {
		return Result{}, errors.New("web: agent id is required")
	}
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	if req.Action == ActionStatus {
		status, err := e.Status(context.Background())
		if err != nil {
			return Result{}, err
		}
		content := fmt.Sprintf("ready: %t\nmode: %s\nprovider: %s\nbrowser: %s\ntabs: %d\nagents: %d",
			status.Ready, status.Mode, status.Provider, status.BrowserPath, status.Tabs, status.Agents)
		for _, note := range status.Notes {
			content += "\nnote: " + note
		}
		return Result{State: StateDone, Outcome: OutcomeDone, Op: string(ActionStatus), Content: untrustedEnvelope(content)}, nil
	}

	e.mu.Lock()
	if e.closed || e.ctx == nil {
		e.mu.Unlock()
		return Result{}, errEngineClosed
	}
	settings := e.settings
	agent := e.agents[agentID]
	if agent == nil {
		agent = &agentState{id: agentID, limiter: newRateLimiter(settings.RateLimitPerAgent)}
		agent.ctx, agent.cancel = context.WithCancel(e.ctx)
		e.agents[agentID] = agent
	}
	e.mu.Unlock()
	return e.startOperation(agent, settings, req)
}

// Wait waits up to d for the op or, when op is empty, the agent's current op.
func (e *Engine) Wait(ctx context.Context, agentID string, opID string, d time.Duration) (Result, error) {
	e.mu.Lock()
	if opID == "" {
		if agent := e.agents[agentID]; agent != nil {
			opID = agent.current
		}
	}
	op := e.ops[opID]
	e.mu.Unlock()
	if op == nil {
		return Result{}, errNoOperation
	}
	if op.agentID != agentID {
		return Result{}, fmt.Errorf("web: operation %s belongs to another agent", opID)
	}
	return e.waitOperation(ctx, op, d)
}

// ForgetAgent cancels agentID's operations and closes its tab. Completed ops
// remain collectable by explicit id.
func (e *Engine) ForgetAgent(agentID string) {
	e.mu.Lock()
	agent := e.agents[agentID]
	if agent == nil {
		e.mu.Unlock()
		return
	}
	delete(e.agents, agentID)
	agent.forgotten = true
	cancel := agent.cancel
	var tab browserTab
	if agent.tab != nil {
		tab = agent.tab.tab
		agent.tab = nil
		e.tabs--
	}
	agent.lastSearch = nil
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if tab != nil {
		_ = tab.Close()
	}
}

// Status reports configuration without starting a browser.
func (e *Engine) Status(ctx context.Context) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	e.mu.Lock()
	settings := e.settings
	closed := e.closed
	ready := e.ctx != nil && !closed
	tabs := e.tabs
	agents := 0
	for _, agent := range e.agents {
		if agent.tab != nil {
			agents++
		}
	}
	var notes []string
	for _, tier := range []Tier{TierHidden, TierWindow} {
		if e.softwareGL[tier] {
			notes = appendNote(notes, string(tier)+" lane uses software rendering (ANGLE launch failed)")
		}
	}
	e.mu.Unlock()
	status := Status{Ready: ready, Mode: settings.Mode, Provider: settings.Provider,
		BrowserPath: settings.BrowserPath, Tabs: tabs, Agents: agents, Notes: notes}
	if installed, err := browser.Find(settings.BrowserPath); err == nil {
		status.BrowserPath = installed.Path
	}
	return status, nil
}

// Close cancels every op and shuts down all resources the engine started.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	cancel := e.cancel
	var tabs []browserTab
	for _, agent := range e.agents {
		agent.forgotten = true
		if agent.tab != nil {
			tabs = append(tabs, agent.tab.tab)
			agent.tab = nil
		}
	}
	e.tabs = 0
	var lanes []browserLane
	for _, slot := range e.lanes {
		if slot.lane != nil {
			lanes = append(lanes, slot.lane)
		}
	}
	proxy := e.proxy
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	var errs []error
	for _, tab := range tabs {
		if err := tab.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, lane := range lanes {
		if err := lane.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if proxy != nil {
		if err := proxy.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) getLane(ctx context.Context, tier Tier, settings Settings) (browserLane, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errEngineClosed
	}
	if slot := e.lanes[tier]; slot != nil {
		e.mu.Unlock()
		select {
		case <-slot.ready:
			return slot.lane, slot.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	slot := &laneSlot{ready: make(chan struct{})}
	e.lanes[tier] = slot
	if e.proxy == nil {
		provider, err := configuredProvider(e.settings)
		if err != nil {
			delete(e.lanes, tier)
			slot.err = err
			close(slot.ready)
			e.mu.Unlock()
			return nil, err
		}
		origin, err := providerOrigin(provider, e.settings)
		if err != nil {
			delete(e.lanes, tier)
			slot.err = err
			close(slot.ready)
			e.mu.Unlock()
			return nil, err
		}
		proxy, err := newGuardProxy(origin)
		if err != nil {
			delete(e.lanes, tier)
			slot.err = err
			close(slot.ready)
			e.mu.Unlock()
			return nil, err
		}
		e.proxy = proxy
	}
	proxy := e.proxy
	factory := e.laneFactory
	hooks := e.hooks
	e.mu.Unlock()

	if factory == nil {
		factory = func(ctx context.Context, tier Tier, settings Settings, proxyAddress string) (browserLane, error) {
			return launchBrowserLaneWithRetry(ctx, tier, settings, proxyAddress, hooks,
				e.softwareGLFor(tier), e.rememberSoftwareGL)
		}
	}
	lane, err := factory(ctx, tier, settings, proxy.address)
	if err == nil && lane == nil {
		err = errors.New("web: lane factory returned no lane")
	}
	if err != nil && lane != nil {
		_ = lane.Close()
		lane = nil
	}

	e.mu.Lock()
	slot.lane, slot.err = lane, err
	if e.closed && lane != nil {
		slot.lane = nil
		slot.err = errEngineClosed
	}
	if err != nil {
		delete(e.lanes, tier)
	}
	close(slot.ready)
	closed := e.closed
	e.mu.Unlock()
	if closed && lane != nil {
		_ = lane.Close()
		return nil, errEngineClosed
	}
	return slot.lane, slot.err
}

func (e *Engine) ensureTab(ctx context.Context, agent *agentState, settings Settings, tier Tier) (*agentTab, error) {
	e.mu.Lock()
	if agent.tab != nil {
		tab := agent.tab
		e.mu.Unlock()
		return tab, nil
	}
	if agent.forgotten || e.closed {
		e.mu.Unlock()
		return nil, context.Canceled
	}
	maxTabs := e.settings.MaxTabs
	if e.tabs >= maxTabs {
		e.mu.Unlock()
		return nil, errTooManyTabs
	}
	e.tabs++ // reserve a global slot while the target is created
	e.mu.Unlock()

	fail := func(err error) (*agentTab, error) {
		e.mu.Lock()
		e.tabs--
		e.mu.Unlock()
		return nil, err
	}
	lane, err := e.getLane(ctx, tier, settings)
	if err != nil {
		return fail(err)
	}
	tab, err := lane.Open(ctx)
	if err != nil {
		return fail(err)
	}
	wrapped := &agentTab{tier: tier, tab: tab}
	e.mu.Lock()
	if agent.forgotten || e.closed {
		e.tabs--
		e.mu.Unlock()
		_ = tab.Close()
		return nil, context.Canceled
	}
	agent.tab = wrapped
	e.mu.Unlock()
	return wrapped, nil
}

func (e *Engine) currentTab(agent *agentState) (*agentTab, error) {
	e.mu.Lock()
	tab := agent.tab
	e.mu.Unlock()
	if tab == nil {
		return nil, errors.New("web: no open tab; use search or open first")
	}
	return tab, nil
}

func (e *Engine) closeAgentTab(agent *agentState) error {
	e.mu.Lock()
	var tab browserTab
	if agent.tab != nil {
		tab = agent.tab.tab
		agent.tab = nil
		e.tabs--
	}
	agent.lastSearch = nil
	e.mu.Unlock()
	if tab != nil {
		return tab.Close()
	}
	return nil
}

func preferredTier(mode Mode) Tier {
	if mode == ModeBackground || mode == ModeVisible {
		return TierWindow
	}
	return TierHidden
}

// CheckURL wraps the network guard. allowOrigin is the only exemption.
func CheckURL(raw, allowOrigin string) error {
	return netguard.CheckURLString(raw, allowOrigin)
}

// searchProviderFor is a small indirection used by tests with fake lanes.
var searchProviderFor = func(settings Settings) (Provider, error) {
	return search.Provider(settings.Provider, settings.CustomTemplate, settings.CustomResultKind, settings.CustomOrigin)
}
