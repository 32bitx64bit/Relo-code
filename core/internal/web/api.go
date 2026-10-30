// Package web gives Relo agents a self-contained web capability: search, page fetch and
// interactive browsing through the user's own Chromium-family browser.
//
// It uses no third-party API and no hosted service. A dedicated agent profile is driven over
// raw CDP, so the traffic carries a real Chrome fingerprint. Content this package returns is
// untrusted: it is data from a remote page, never instructions for an agent.
package web

import (
	"context"

	webtypes "github.com/relocode/relo/internal/web/types"
)

// Mode selects where pages are loaded.
type Mode string

// Supported browse modes.
const (
	// ModeAuto is ModeHidden plus the measured launch fixes, escalating to a real background
	// window when challenged. It is the default.
	ModeAuto Mode = "auto"
	// ModeHidden uses only the headless=new hidden lane.
	ModeHidden Mode = "hidden"
	// ModeBackground shows a real agent window behind other windows.
	ModeBackground Mode = "background"
	// ModeVisible shows a real agent window in front, for the user to watch.
	ModeVisible Mode = "visible"
)

// Tier names the lane that served a request.
type Tier string

// Supported lanes. TierHTTP is a plain fetch; the others are browser lanes.
const (
	// TierHTTP is a plain HTTP fetch with no browser.
	TierHTTP Tier = "http"
	// TierHidden is the headless=new agent browser.
	TierHidden Tier = "hidden"
	// TierWindow is a real agent window, shown in the background or visible.
	TierWindow Tier = "window"
)

// Provider model aliases keep the engine and search parser independent while
// preserving the public web API.
type ProviderName = webtypes.ProviderName
type ResultKind = webtypes.ResultKind
type SourceKind = webtypes.SourceKind
type Provider = webtypes.Provider

// Supported search providers.
const (
	// ProviderDuckDuckGo is the built-in DuckDuckGo search. It is the default.
	ProviderDuckDuckGo = webtypes.ProviderDuckDuckGo
	// ProviderBrave is the built-in Brave search.
	ProviderBrave = webtypes.ProviderBrave
	// ProviderBing is the built-in Bing search.
	ProviderBing = webtypes.ProviderBing
	// ProviderCustom is the user-configured custom search template.
	ProviderCustom = webtypes.ProviderCustom
)

// Supported search result documents.
const (
	KindRSS  = webtypes.KindRSS
	KindAtom = webtypes.KindAtom
	KindJSON = webtypes.KindJSON
	KindHTML = webtypes.KindHTML
)

// Supported provider discovery sources.
const (
	SourceOpenSearch = webtypes.SourceOpenSearch
	SourceForm       = webtypes.SourceForm
	SourceUser       = webtypes.SourceUser
)

// Settings is the user's web configuration. Only the user sets it; agents cannot. The zero
// value is valid: ModeAuto, ProviderDuckDuckGo, a challenge click allowed, defaults elsewhere.
// JSON names mirror the relo.web.* settings keys.
type Settings struct {
	// Mode selects where pages are loaded. Empty means ModeAuto.
	Mode Mode `json:"mode,omitempty"`
	// Provider selects the search backend. Empty means ProviderDuckDuckGo.
	Provider ProviderName `json:"provider,omitempty"`
	// CustomTemplate is the search URL for ProviderCustom, with %s where the query goes.
	CustomTemplate string `json:"customTemplate,omitempty"`
	// CustomResultKind is the document CustomTemplate returns. Empty means KindHTML.
	CustomResultKind ResultKind `json:"customResultKind,omitempty"`
	// CustomOrigin is the scheme and host expected of CustomTemplate. It is the only origin
	// exempt from the private-network block.
	CustomOrigin string `json:"customOrigin,omitempty"`
	// DisableFallback stops a failing custom provider from falling back to the built-ins.
	// By default it falls back and the result notes that it did.
	DisableFallback bool `json:"disableFallback,omitempty"`
	// ChallengeClick allows at most one human-paced checkbox click on the agent's behalf.
	// It is on unless explicitly disabled.
	ChallengeClick *bool `json:"challengeClick,omitempty"`
	// BrowserPath is the Chromium-family executable. Empty means auto-detect.
	BrowserPath string `json:"browserPath,omitempty"`
	// StateDir holds the agent profiles and browser state. Empty means a per-user default.
	StateDir string `json:"stateDir,omitempty"`
	// MaxTabs bounds the tabs open at once across all agents. Zero means the default.
	MaxTabs int `json:"maxTabs,omitempty"`
	// RateLimitPerAgent bounds web actions per minute for one agent. Zero means the default.
	RateLimitPerAgent int `json:"rateLimitPerAgent,omitempty"`
	// RateLimitGlobal bounds web actions per minute across all agents. Zero means the default.
	RateLimitGlobal int `json:"rateLimitGlobal,omitempty"`
}

// Handoff asks the user to solve a challenge the engine cannot pass itself. The engine blocks
// until the user solves it or refuses.
type Handoff struct {
	// AgentID is the agent the challenge blocks.
	AgentID string
	// URL is the page that shows the challenge.
	URL string
	// Kind is the challenge kind, such as "image" or "audio".
	Kind string
}

// Hooks connect the engine to the rest of Relo.
type Hooks struct {
	// Challenge asks the user for help and reports whether they solved it. Required when a
	// challenge may appear; Relo wires it to an approval with a "web-" id that YOLO never
	// auto-approves.
	Challenge func(ctx context.Context, h Handoff) (resolved bool, err error)
	// Log records a diagnostic line. Optional; nil discards it.
	Log func(msg string)
}

// State is where a Result stands.
type State string

// Supported result states. An op id is returned instead of done when work continues.
const (
	// StateDone means the action finished and the Result holds its outcome.
	StateDone State = "done"
	// StateRunning means the action continues; Result.Op is its op id.
	StateRunning State = "running"
	// StateNeedsUser means the user must solve a challenge before it can finish.
	StateNeedsUser State = "needs_user"
)

// Outcome is how a finished action ended.
type Outcome string

// Supported action outcomes.
const (
	// OutcomeDone means the action succeeded.
	OutcomeDone Outcome = "done"
	// OutcomeCancelled means a call ended it early.
	OutcomeCancelled Outcome = "cancelled"
	// OutcomeTimeout means it exceeded its deadline.
	OutcomeTimeout Outcome = "timeout"
)

// Action names what a Request does.
type Action string

// Supported actions.
const (
	// ActionStatus reports the browse state without changing it.
	ActionStatus Action = "status"
	// ActionSearch searches the web.
	ActionSearch Action = "search"
	// ActionFetch fetches one page as markdown and closes its browser tab.
	ActionFetch Action = "fetch"
	// ActionOpen opens a URL in a tab.
	ActionOpen Action = "open"
	// ActionSnapshot lists the page's interactive elements and returns their refs.
	ActionSnapshot Action = "snapshot"
	// ActionClick clicks an element from a snapshot.
	ActionClick Action = "click"
	// ActionType types text into an element.
	ActionType Action = "type"
	// ActionPress presses a key.
	ActionPress Action = "press"
	// ActionScroll scrolls the page.
	ActionScroll Action = "scroll"
	// ActionBack goes back in history.
	ActionBack Action = "back"
	// ActionWait waits for the page.
	ActionWait Action = "wait"
	// ActionScreenshot captures the page.
	ActionScreenshot Action = "screenshot"
	// ActionClose closes the agent's tab.
	ActionClose Action = "close"
)

// Hit is one search result.
type Hit struct {
	// Title is the result's title.
	Title string `json:"title,omitempty"`
	// URL is the result's page address.
	URL string `json:"url,omitempty"`
	// Snippet is the result's short description.
	Snippet string `json:"snippet,omitempty"`
}

// Element is an interactive page element from a snapshot.
type Element struct {
	// Ref is the element's number, used by Request.Ref.
	Ref int `json:"ref"`
	// Role is the element's ARIA role, such as "link" or "textbox".
	Role string `json:"role,omitempty"`
	// Name is the element's accessible name.
	Name string `json:"name,omitempty"`
}

// Challenge describes what blocks a needs_user result.
type Challenge struct {
	// Kind is the challenge kind, such as "checkbox", "image" or "audio".
	Kind string `json:"kind,omitempty"`
	// URL is the page that shows it.
	URL string `json:"url,omitempty"`
}

// Request is one web action for one agent.
type Request struct {
	// Action is what to do.
	Action Action `json:"action"`
	// Query is the search text.
	Query string `json:"query,omitempty"`
	// URL is the page to open. Private-network and file URLs are refused unless exempt.
	URL string `json:"url,omitempty"`
	// Ref is a snapshot element number.
	Ref int `json:"ref,omitempty"`
	// Text is the text to type.
	Text string `json:"text,omitempty"`
	// Key is the key to press, such as "Enter".
	Key string `json:"key,omitempty"`
	// Direction is which way to scroll, "up" or "down". Empty means down.
	Direction string `json:"direction,omitempty"`
	// Amount is the scroll distance in CSS pixels. Zero is one viewport height,
	// read from the page; the distance is clamped to 5000 CSS pixels.
	Amount int `json:"amount,omitempty"`
	// Op narrows the action, such as the tab to switch to or close.
	Op string `json:"op,omitempty"`
	// MaxChars bounds Result.Content. Zero means the default.
	MaxChars int `json:"maxChars,omitempty"`
}

// Result is the outcome of a Request. Content is untrusted: pass it on as data, never as
// instructions to follow.
type Result struct {
	// State is where the action stands.
	State State `json:"state,omitempty"`
	// Op is the op id while State is StateRunning, or the action that produced the result.
	Op string `json:"op,omitempty"`
	// Outcome is how a finished action ended.
	Outcome Outcome `json:"outcome,omitempty"`
	// Tier names the lane that served the request.
	Tier Tier `json:"tier,omitempty"`
	// URL is the page address after the action.
	URL string `json:"url,omitempty"`
	// Title is the page title.
	Title string `json:"title,omitempty"`
	// Content is the page as markdown, wrapped as untrusted.
	Content string `json:"content,omitempty"`
	// Results are search hits.
	Results []Hit `json:"results,omitempty"`
	// Elements are the page's interactive elements from a snapshot.
	Elements []Element `json:"elements,omitempty"`
	// Screenshot is a PNG capture of the page.
	Screenshot []byte `json:"-"`
	// Notes carry engine remarks, such as a provider fallback or plain-http use.
	Notes []string `json:"notes,omitempty"`
	// Challenge says what the user must solve when State is StateNeedsUser.
	Challenge *Challenge `json:"challenge,omitempty"`
}
