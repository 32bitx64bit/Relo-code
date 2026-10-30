package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/relocode/relo/internal/web/challenge"
	"github.com/relocode/relo/internal/web/extract"
	"github.com/relocode/relo/internal/web/netguard"
	"github.com/relocode/relo/internal/web/page"
	"github.com/relocode/relo/internal/web/search"
)

const (
	navigationTimeout = 45 * time.Second
	maxResponseBytes  = 8 << 20
	maxSearchHits     = 50
)

type searchState struct {
	url  string
	hits []Hit
}

type actionEnv struct {
	agent            *agentState
	settings         Settings
	notes            []string
	tier             Tier
	challengeClicked bool
}

type challengeStop struct {
	kind string
	url  string
	tier Tier
}

func (e *challengeStop) Error() string { return "web: challenge needs the user" }

func normalizeSettings(settings Settings) (Settings, error) {
	if settings.Mode == "" {
		settings.Mode = ModeAuto
	}
	switch settings.Mode {
	case ModeAuto, ModeHidden, ModeBackground, ModeVisible:
	default:
		return Settings{}, fmt.Errorf("web: unsupported mode %q", settings.Mode)
	}
	if settings.Provider == "" {
		settings.Provider = ProviderDuckDuckGo
	}
	if settings.CustomResultKind == "" {
		settings.CustomResultKind = KindHTML
	}
	if settings.MaxTabs < 0 || settings.RateLimitPerAgent < 0 || settings.RateLimitGlobal < 0 {
		return Settings{}, errors.New("web: tab and rate limits cannot be negative")
	}
	if settings.MaxTabs == 0 {
		settings.MaxTabs = defaultMaxTabs
	}
	if settings.RateLimitPerAgent == 0 {
		settings.RateLimitPerAgent = defaultAgentRate
	}
	if settings.RateLimitGlobal == 0 {
		settings.RateLimitGlobal = defaultGlobalRate
	}
	if settings.StateDir == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			settings.StateDir = filepath.Join(dir, "relo-code", "opencode")
		} else {
			settings.StateDir = filepath.Join(os.TempDir(), "relo-code", "opencode")
		}
	}
	return settings, nil
}

func configuredProvider(settings Settings) (Provider, error) { return searchProviderFor(settings) }

func providerOrigin(provider Provider, _ Settings) (netguard.Origin, error) {
	if provider.Name != ProviderCustom || strings.TrimSpace(provider.Origin) == "" {
		return netguard.Origin{}, nil
	}
	origin, err := netguard.ParseOrigin(provider.Origin)
	if err != nil {
		return netguard.Origin{}, err
	}
	probe, err := search.QueryURL(provider, "origin-check")
	if err != nil {
		return netguard.Origin{}, err
	}
	u, err := url.Parse(probe)
	if err != nil {
		return netguard.Origin{}, err
	}
	templateOrigin, err := netguard.ParseOrigin(u.Scheme + "://" + u.Host)
	if err != nil {
		return netguard.Origin{}, err
	}
	if origin.String() != templateOrigin.String() {
		return netguard.Origin{}, errors.New("web: custom origin must exactly match the custom search template origin")
	}
	return origin, nil
}

func allowOrigin(settings Settings) (string, error) {
	provider, err := configuredProvider(settings)
	if err != nil {
		return "", err
	}
	origin, err := providerOrigin(provider, settings)
	if err != nil || origin.IsZero() {
		return "", err
	}
	return origin.String(), nil
}

func checkURL(settings Settings, raw string) error {
	origin, err := allowOrigin(settings)
	if err != nil {
		return err
	}
	return CheckURL(raw, origin)
}

func (e *Engine) guardURL(settings Settings, raw string) error {
	if e.urlCheck != nil {
		return e.urlCheck(settings, raw)
	}
	return checkURL(settings, raw)
}

func (e *Engine) execute(ctx context.Context, agent *agentState, settings Settings, req Request) (Result, error) {
	if err := e.global.wait(ctx); err != nil {
		return Result{}, err
	}
	if err := agent.limiter.wait(ctx); err != nil {
		return Result{}, err
	}
	env := &actionEnv{agent: agent, settings: settings}
	result, err := e.runAction(ctx, env, req)
	var stop *challengeStop
	if errors.As(err, &stop) {
		return Result{State: StateNeedsUser, Tier: stop.tier, URL: stop.url,
			Challenge: &Challenge{Kind: stop.kind, URL: stop.url}, Notes: env.notes}, nil
	}
	result.Notes = append(env.notes, result.Notes...)
	return result, err
}

func (e *Engine) searchFlow(ctx context.Context, env *actionEnv, query string) (Result, error) {
	provider, err := configuredProvider(env.settings)
	if err != nil {
		return Result{}, err
	}
	hits, err := e.searchOne(ctx, env, provider, query)
	if err != nil && provider.Name == ProviderCustom && !env.settings.DisableFallback {
		var stop *challengeStop
		if errors.As(err, &stop) {
			return Result{}, err
		}
		customErr := err
		for _, name := range []ProviderName{ProviderDuckDuckGo, ProviderBrave, ProviderBing} {
			fallback, providerErr := search.DefaultProvider(name)
			if providerErr != nil {
				continue
			}
			hits, err = e.searchOne(ctx, env, fallback, query)
			if err == nil {
				provider = fallback
				env.notes = appendNote(env.notes, "custom search provider failed; used "+string(name)+" fallback")
				break
			}
			if errors.As(err, &stop) {
				return Result{}, err
			}
		}
		if err != nil {
			return Result{}, fmt.Errorf("web: custom search failed: %v; fallbacks failed: %w", customErr, err)
		}
	} else if err != nil {
		return Result{}, err
	}

	rawURL, _ := search.QueryURL(provider, query)
	result := Result{Tier: env.tier, URL: rawURL, Results: convertHits(hits)}
	if env.tier != TierHTTP {
		e.mu.Lock()
		env.agent.lastSearch = &searchState{url: rawURL, hits: result.Results}
		e.mu.Unlock()
	}
	return result, nil
}

func (e *Engine) searchOne(ctx context.Context, env *actionEnv, provider Provider, query string) ([]search.Hit, error) {
	tab, err := e.ensureTab(ctx, env.agent, env.settings, preferredTier(env.settings.Mode))
	if err == nil {
		env.tier = tab.tier
		fetcher := &browserFetcher{engine: e, env: env, tab: tab, kind: provider.ResultKind}
		hits, searchErr := search.Search(ctx, fetcher, provider, query)
		return hits, searchErr
	}
	if !canUseHTTPFallback(err) {
		return nil, err
	}
	env.notes = appendNote(env.notes, "browser unavailable; used plain Go HTTP")
	fetcher := &plainFetcher{engine: e, env: env}
	return search.Search(ctx, fetcher, provider, query)
}

func convertHits(hits []search.Hit) []Hit {
	if len(hits) > maxSearchHits {
		hits = hits[:maxSearchHits]
	}
	out := make([]Hit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, Hit{Title: hit.Title, URL: hit.URL, Snippet: hit.Snippet})
	}
	return out
}

func canUseHTTPFallback(err error) bool {
	return !errors.Is(err, errTooManyTabs) && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, errEngineClosed)
}

type browserFetcher struct {
	engine *Engine
	env    *actionEnv
	tab    *agentTab
	kind   ResultKind
}

func (f *browserFetcher) Fetch(ctx context.Context, rawURL string) (int, string, []byte, error) {
	if err := f.engine.navigateDirect(ctx, f.env, f.tab, rawURL, ""); err != nil {
		return 0, "", nil, err
	}
	active, err := f.engine.currentTab(f.env.agent)
	if err != nil {
		return 0, "", nil, err
	}
	f.tab = active
	var body string
	if f.kind == "" || f.kind == KindHTML {
		body, err = f.tab.tab.HTML(ctx)
	} else {
		// Browser XML/JSON viewers report their text, sometimes with prose or fences.
		body, err = f.tab.tab.Text(ctx)
	}
	return 200, "text/html; charset=utf-8", []byte(body), err
}

type plainFetcher struct {
	engine *Engine
	env    *actionEnv
}

func (f *plainFetcher) Fetch(ctx context.Context, rawURL string) (int, string, []byte, error) {
	response, err := f.engine.plainFetch(ctx, f.env, rawURL)
	if err != nil {
		return 0, "", nil, err
	}
	f.env.tier = TierHTTP
	body := response.body
	reader, decodeErr := charset.NewReader(bytes.NewReader(body), response.contentType)
	if decodeErr == nil {
		if decoded, readErr := io.ReadAll(reader); readErr == nil {
			body = decoded
		}
	}
	return response.status, response.contentType, body, nil
}

type plainResponse struct {
	status      int
	contentType string
	body        []byte
	finalURL    string
}

func (e *Engine) plainFetch(ctx context.Context, env *actionEnv, rawURL string) (*plainResponse, error) {
	if err := e.guardURL(env.settings, rawURL); err != nil {
		return nil, err
	}
	if err := e.pacer.wait(ctx, rawURL); err != nil {
		return nil, err
	}
	provider, err := configuredProvider(env.settings)
	if err != nil {
		return nil, err
	}
	origin, err := providerOrigin(provider, env.settings)
	if err != nil {
		return nil, err
	}
	client := (netguard.GuardedClient{AllowOrigin: origin}).NewClient()
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		body = body[:maxResponseBytes]
	}
	return &plainResponse{status: response.StatusCode, contentType: response.Header.Get("Content-Type"),
		body: body, finalURL: response.Request.URL.String()}, nil
}

func (e *Engine) openFlow(ctx context.Context, env *actionEnv, rawURL string, keep bool, maxChars int) (Result, error) {
	if err := e.guardURL(env.settings, rawURL); err != nil {
		return Result{}, err
	}
	tab, err := e.ensureTab(ctx, env.agent, env.settings, preferredTier(env.settings.Mode))
	if err != nil {
		if !canUseHTTPFallback(err) {
			return Result{}, err
		}
		env.notes = appendNote(env.notes, "browser unavailable; used plain Go HTTP")
		response, fetchErr := e.plainFetch(ctx, env, rawURL)
		if fetchErr != nil {
			return Result{}, fetchErr
		}
		if response.status >= 400 {
			return Result{}, fmt.Errorf("web: fetch %s: status %d", rawURL, response.status)
		}
		env.tier = TierHTTP
		return extractResult(response.body, response.finalURL, TierHTTP, maxChars)
	}
	env.tier = tab.tier
	if !keep {
		defer func() { _ = e.closeAgentTab(env.agent) }()
	}
	if err := e.navigateOpen(ctx, env, tab, rawURL); err != nil {
		return Result{}, err
	}
	tab, err = e.currentTab(env.agent)
	if err != nil {
		return Result{}, err
	}
	currentURL, err := tab.tab.URL(ctx)
	if err != nil {
		return Result{}, err
	}
	body, err := tab.tab.HTML(ctx)
	if err != nil {
		return Result{}, err
	}
	return extractResult([]byte(body), currentURL, tab.tier, maxChars)
}

func extractResult(body []byte, rawURL string, tier Tier, maxChars int) (Result, error) {
	base, err := url.Parse(rawURL)
	if err != nil {
		return Result{}, err
	}
	extracted, err := extract.HTML(body, extract.Options{BaseURL: base, MaxChars: maxChars})
	if err != nil {
		return Result{}, err
	}
	result := Result{Tier: tier, URL: rawURL, Title: extracted.Title, Content: untrustedEnvelope(extracted.Content)}
	if extracted.Truncated {
		result.Notes = append(result.Notes, "content truncated at the requested character limit")
	}
	return result, nil
}

func (e *Engine) navigateDirect(ctx context.Context, env *actionEnv, tab *agentTab, rawURL, referrer string) error {
	if err := e.guardURL(env.settings, rawURL); err != nil {
		return err
	}
	if err := e.pacer.wait(ctx, rawURL); err != nil {
		return err
	}
	navCtx, cancel := context.WithTimeout(ctx, navigationTimeout)
	defer cancel()
	if err := tab.tab.Navigate(navCtx, rawURL, referrer); err != nil {
		return err
	}
	currentURL, err := tab.tab.URL(navCtx)
	if err != nil {
		return err
	}
	if err := e.guardURL(env.settings, currentURL); err != nil {
		return err
	}
	return e.handleChallenge(ctx, env, tab, currentURL)
}

func (e *Engine) navigateOpen(ctx context.Context, env *actionEnv, tab *agentTab, rawURL string) error {
	if err := e.guardURL(env.settings, rawURL); err != nil {
		return err
	}
	e.mu.Lock()
	last := env.agent.lastSearch
	e.mu.Unlock()
	if last != nil {
		navCtx, cancel := context.WithTimeout(ctx, navigationTimeout)
		currentURL, err := tab.tab.URL(navCtx)
		var elements []page.Element
		if err == nil && currentURL == last.url {
			elements, err = tab.tab.Snapshot(navCtx)
		}
		cancel()
		if err == nil {
			if ref := findSearchResultRef(elements, last.hits, rawURL); ref > 0 {
				if err := e.pacer.wait(ctx, rawURL); err != nil {
					return err
				}
				clickCtx, clickCancel := context.WithTimeout(ctx, navigationTimeout)
				defer clickCancel()
				if err := tab.tab.Click(clickCtx, ref); err != nil {
					return err
				}
				currentURL, err = tab.tab.URL(clickCtx)
				if err != nil {
					return err
				}
				if err := e.guardURL(env.settings, currentURL); err != nil {
					return err
				}
				e.mu.Lock()
				env.agent.lastSearch = nil
				e.mu.Unlock()
				return e.handleChallenge(ctx, env, tab, currentURL)
			}
		}
	}
	if err := e.navigateDirect(ctx, env, tab, rawURL, ""); err != nil {
		return err
	}
	e.mu.Lock()
	env.agent.lastSearch = nil
	e.mu.Unlock()
	return nil
}

// findSearchResultRef matches direct result links first, then a unique title.
// A missing or ambiguous match deliberately falls back to direct navigation.
func findSearchResultRef(elements []page.Element, hits []Hit, rawURL string) int {
	wantTitle := ""
	found := false
	for _, hit := range hits {
		if hit.URL == rawURL {
			wantTitle = hit.Title
			found = true
			break
		}
	}
	if !found {
		return 0
	}
	for _, element := range elements {
		if element.Href == rawURL && !element.Disabled {
			return element.Index
		}
	}
	if wantTitle == "" {
		return 0
	}
	match := 0
	for _, element := range elements {
		if element.Name == wantTitle && !element.Disabled {
			if match != 0 {
				return 0
			}
			match = element.Index
		}
	}
	return match
}

func (e *Engine) handleChallenge(ctx context.Context, env *actionEnv, tab *agentTab, currentURL string) error {
	result, err := e.runChallenge(ctx, env, tab)
	if err != nil || result.Outcome == challenge.Solved {
		return err
	}
	if tab.tier == TierHidden && env.settings.Mode == ModeAuto {
		cookies, cookieErr := tab.tab.GetCookies(ctx)
		if cookieErr != nil {
			return cookieErr
		}
		cookies = cookiesForDomain(currentURL, cookies)
		if err := e.closeAgentTab(env.agent); err != nil {
			return err
		}
		window, err := e.ensureTab(ctx, env.agent, env.settings, TierWindow)
		if err != nil {
			return err
		}
		env.tier = TierWindow
		env.notes = appendNote(env.notes, "hidden lane challenged; escalated once to the window lane")
		if err := window.tab.SetCookies(ctx, cookies); err != nil {
			return err
		}
		if err := e.navigateWindowAfterEscalation(ctx, env, window, currentURL); err != nil {
			return err
		}
		tab = window
		result, err = e.runChallenge(ctx, env, tab)
		if err != nil || result.Outcome == challenge.Solved {
			return err
		}
	}
	return e.handoffChallenge(ctx, env, tab, currentURL, result)
}

// navigateWindowAfterEscalation avoids recursively escalating the window lane.
func (e *Engine) navigateWindowAfterEscalation(ctx context.Context, env *actionEnv, tab *agentTab, rawURL string) error {
	if err := e.pacer.wait(ctx, rawURL); err != nil {
		return err
	}
	navCtx, cancel := context.WithTimeout(ctx, navigationTimeout)
	defer cancel()
	if err := tab.tab.Navigate(navCtx, rawURL, ""); err != nil {
		return err
	}
	currentURL, err := tab.tab.URL(navCtx)
	if err != nil {
		return err
	}
	return e.guardURL(env.settings, currentURL)
}

func (e *Engine) runChallenge(ctx context.Context, env *actionEnv, tab *agentTab) (challenge.Result, error) {
	options := challenge.DefaultOptions()
	options.Log = func(attempt challenge.Attempt) {
		if e.hooks.Log != nil {
			e.hooks.Log(fmt.Sprintf("web challenge lane=%s stage=%s kind=%s mode=%s outcome=%s %s",
				tab.tier, attempt.Stage, attempt.Kind, attempt.Mode, attempt.Outcome, attempt.Detail))
		}
	}
	if env.challengeClicked || (env.settings.ChallengeClick != nil && !*env.settings.ChallengeClick) {
		options.DisableClick = true
	}
	result, err := tab.tab.RunChallenge(ctx, options)
	if result.Clicked {
		env.challengeClicked = true
	}
	return result, err
}

func challengeKind(result challenge.Result) string {
	for _, widget := range result.Widgets {
		if widget.Mode == challenge.ModeUserPuzzle {
			return string(widget.Kind)
		}
	}
	if len(result.Widgets) > 0 {
		return string(result.Widgets[0].Kind)
	}
	return "challenge"
}

func (e *Engine) handoffChallenge(ctx context.Context, env *actionEnv, tab *agentTab, rawURL string, detected challenge.Result) error {
	kind := challengeKind(detected)
	if e.hooks.Challenge != nil {
		resolved, err := e.hooks.Challenge(ctx, Handoff{AgentID: env.agent.id, URL: rawURL, Kind: kind})
		if err != nil {
			return err
		}
		if resolved {
			return nil
		}
	}
	return &challengeStop{kind: kind, url: rawURL, tier: tab.tier}
}

func cookiesForDomain(rawURL string, cookies []page.Cookie) []page.Cookie {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	var out []page.Cookie
	for _, cookie := range cookies {
		domain := strings.ToLower(strings.TrimPrefix(strings.TrimSuffix(cookie.Domain, "."), "."))
		if domain != "" && (host == domain || strings.HasSuffix(host, "."+domain)) {
			out = append(out, cookie)
		}
	}
	return out
}

// DiscoverProvider uses a browser document fetcher just like search, falling
// back to guarded Go HTTP only when no browser target can be opened.
func (e *Engine) DiscoverProvider(ctx context.Context, rawURL string) (Provider, error) {
	if strings.TrimSpace(rawURL) == "" {
		return Provider{}, errors.New("web: discovery URL is required")
	}
	e.mu.Lock()
	if e.closed || e.ctx == nil {
		e.mu.Unlock()
		return Provider{}, errEngineClosed
	}
	settings := e.settings
	if strings.Contains(rawURL, "%s") {
		// An explicit template needs no discovery pass; HTML is the default kind.
		provider, err := search.Provider(ProviderCustom, rawURL, settings.CustomResultKind, "")
		e.mu.Unlock()
		return provider, err
	}
	e.opSeq++
	id := fmt.Sprintf("settings-discovery-%d", e.opSeq)
	agent := &agentState{id: id, limiter: newRateLimiter(settings.RateLimitPerAgent)}
	agent.ctx, agent.cancel = context.WithCancel(e.ctx)
	e.agents[id] = agent
	e.mu.Unlock()
	defer e.ForgetAgent(id)
	if err := e.guardURL(settings, rawURL); err != nil {
		return Provider{}, err
	}
	env := &actionEnv{agent: agent, settings: settings}
	tab, err := e.ensureTab(ctx, agent, settings, preferredTier(settings.Mode))
	if err == nil {
		env.tier = tab.tier
		return search.Discover(ctx, &browserFetcher{engine: e, env: env, tab: tab, kind: KindHTML}, rawURL)
	}
	if !canUseHTTPFallback(err) {
		return Provider{}, err
	}
	env.notes = appendNote(env.notes, "browser unavailable; used plain Go HTTP")
	if e.hooks.Log != nil {
		e.hooks.Log("web provider discovery used plain Go HTTP because no browser target was available")
	}
	return search.Discover(ctx, &plainFetcher{engine: e, env: env}, rawURL)
}
