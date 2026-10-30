// Package web contains the dependency-free search provider model shared by the
// engine and its search implementation.
package web

// ProviderName names the search backend.
type ProviderName string

// Supported search providers.
const (
	ProviderDuckDuckGo ProviderName = "duckduckgo"
	ProviderBrave      ProviderName = "brave"
	ProviderBing       ProviderName = "bing"
	ProviderCustom     ProviderName = "custom"
)

// ResultKind is the document a search template returns.
type ResultKind string

// Supported search result documents.
const (
	KindRSS  ResultKind = "rss"
	KindAtom ResultKind = "atom"
	KindJSON ResultKind = "json"
	KindHTML ResultKind = "html"
)

// SourceKind says where a custom provider's template came from.
type SourceKind string

// Supported provider discovery sources.
const (
	SourceOpenSearch SourceKind = "opensearch"
	SourceForm       SourceKind = "form"
	SourceUser       SourceKind = "user"
)

// Provider describes a search backend, built-in or custom.
type Provider struct {
	Name       ProviderName `json:"name"`
	Template   string       `json:"template,omitempty"`
	ResultKind ResultKind   `json:"resultKind,omitempty"`
	Source     SourceKind   `json:"source,omitempty"`
	Origin     string       `json:"origin,omitempty"`
}
