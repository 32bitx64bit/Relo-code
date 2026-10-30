package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// GuardedClient is the http.Client for the web capability's no-browser fallback. It checks a request's URL before it
// leaves, checks each redirect as a new request, and dials only addresses it resolved and checked itself, so a name
// cannot change to a private address between the check and the dial.
type GuardedClient struct {
	// AllowOrigin is the user's search provider, the only origin a request to a private address may reach.
	AllowOrigin Origin
	// Resolver answers names; nil means net.DefaultResolver.
	Resolver Resolver
	// Dial opens connections; nil means net.Dialer.
	Dial DialFunc
	// Timeout bounds a whole fetch, including its redirects; zero means DefaultTimeout.
	Timeout time.Duration
	// MaxRedirects bounds how many redirects are followed; zero means DefaultMaxRedirects.
	MaxRedirects int
}

const (
	// DefaultTimeout bounds a fetch with the guarded client.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxRedirects is how many redirects one fetch follows.
	DefaultMaxRedirects = 10
)

// NewClient answers the http.Client to fetch with: its CheckRedirect checks each hop and its transport checks each
// address it dials. A body larger than the caller wants is the caller's to cut with io.LimitReader.
func (c GuardedClient) NewClient() *http.Client {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxRedirects := c.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = DefaultMaxRedirects
	}
	p := Proxy{AllowOrigin: c.AllowOrigin, Resolver: c.Resolver, Dial: c.Dial}
	exempt := ""
	if !c.AllowOrigin.IsZero() {
		// CheckRequest authorises scheme and origin for every request and redirect;
		// the dial layer only needs the matching host:port to permit its address.
		exempt = net.JoinHostPort(c.AllowOrigin.host, c.AllowOrigin.port)
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return p.dialAddress(ctx, network, address, exempt)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("netguard: stopped after %d redirects", maxRedirects)
			}
			return c.CheckRequest(req)
		},
	}
}

// CheckRequest answers whether the client may fetch r, the same check the proxy makes. A request without an absolute
// URL or an https URL names no site the client knows how to reach and is refused.
func (c GuardedClient) CheckRequest(r *http.Request) error {
	if r == nil || r.URL == nil || !r.URL.IsAbs() {
		return fmt.Errorf("netguard: no absolute URL")
	}
	_, err := checkURL(r.URL.String(), c.AllowOrigin, c.Resolver)
	return err
}

// DialContext resolves a name, refuses it if any address is not public, and dials an address it checked.
func (c GuardedClient) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("netguard: %w", err)
	}
	portNum, err := normalPort(port)
	if err != nil {
		return nil, err
	}
	p := Proxy{AllowOrigin: c.AllowOrigin, Resolver: c.Resolver, Dial: c.Dial}
	return p.dialChecked(ctx, network, host, strconv.Itoa(portNum))
}
