package netguard

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Resolver answers which addresses a name stands for. Proxy and GuardedClient take one so a test can answer without
// a network; nil means the system resolver.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DialFunc opens the connection to an address the check has already passed. nil uses net.Dialer.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

const (
	connectTimeout        = 30 * time.Second
	tlsHandshakeTimeout   = 20 * time.Second
	responseHeaderTimeout = 30 * time.Second
	idleConnTimeout       = 90 * time.Second
	expectContinueTimeout = time.Second
)

// Proxy is the guard's HTTP proxy for the browser, on 127.0.0.1. It resolves each request's name itself, checks
// every address it gets, and dials the address it checked, so a name cannot change to a private address between the
// check and the dial. Redirects are new requests through the proxy, so a redirect to a private address is refused
// like any other.
type Proxy struct {
	// AllowOrigin is the user's search provider, the only origin a request to a private address may reach.
	AllowOrigin Origin
	// Resolver answers names; nil means net.DefaultResolver.
	Resolver Resolver
	// Dial opens connections; nil means net.Dialer.
	Dial DialFunc
}

// ServeHTTP serves one proxied request, plain http or a CONNECT tunnel.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.serveConnect(w, r)
		return
	}
	p.serveForward(w, r)
}

// serveForward carries a plain http request upstream. The browser sends absolute URLs, so both the URL check here and
// the address check in DialContext stand between the page and the host.
func (p *Proxy) serveForward(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || !r.URL.IsAbs() {
		http.Error(w, "netguard: proxy needs an absolute URL", http.StatusBadRequest)
		return
	}
	if strings.ToLower(r.URL.Scheme) != "http" {
		http.Error(w, "netguard: proxy carries plain http only; https goes through CONNECT", http.StatusBadRequest)
		return
	}
	if err := p.CheckRequest(r); err != nil {
		http.Error(w, "netguard: "+err.Error(), http.StatusForbidden)
		return
	}
	if wantsUpgrade(r) {
		p.serveUpgrade(w, r)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = "http"
	out.URL.User = nil
	removeHopHeaders(out.Header)
	out.Header.Del("Proxy-Connection")
	out.Close = false
	resp, err := p.upstream(exemptDialAddress(r.URL, p.AllowOrigin)).RoundTrip(out)
	if err != nil {
		http.Error(w, "netguard: upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	removeHopHeaders(resp.Header)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// upgradeTimeout bounds how long the upstream site may take to answer an upgrade request.
const upgradeTimeout = connectTimeout

// wantsUpgrade reports whether r asks to change protocol on its connection, as a websocket does. Such a request
// cannot be carried by an http.Client, which reads a 101 as the end of the response, so it is tunnelled instead.
func wantsUpgrade(r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("Upgrade")) != "" {
		return true
	}
	return headerHasToken(r.Header.Get("Connection"), "upgrade")
}

// serveUpgrade carries an Upgrade request, a websocket handshake on plain http, to an address the guard resolved and
// checked. r passed CheckRequest, and the address is checked again before it is dialled, so a name that changed
// between the two cannot reach the host. The request is written to that connection and the connection is then a
// tunnel, so the 101 and every frame after it cross the guard without being read as an HTTP message.
func (p *Proxy) serveUpgrade(w http.ResponseWriter, r *http.Request) {
	host, port, err := hostAndPort(r.URL)
	if err != nil {
		http.Error(w, "netguard: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), upgradeTimeout)
	defer cancel()
	var conn net.Conn
	if exempt := exemptDialAddress(r.URL, p.AllowOrigin); exempt != "" {
		conn, err = p.dialer()(ctx, "tcp", exempt)
	} else {
		conn, err = p.dialChecked(ctx, "tcp", host, port)
	}
	if err != nil {
		http.Error(w, "netguard: upstream: "+err.Error(), http.StatusForbidden)
		return
	}
	// The timeout belongs to the connect and the handshake, not to the tunnel that follows them.
	_ = conn.SetDeadline(time.Now().Add(upgradeTimeout))
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		conn.Close()
		http.Error(w, "netguard: connection cannot be upgraded", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		conn.Close()
		return
	}
	if err := writeUpstreamRequest(conn, r); err != nil {
		client.Close()
		conn.Close()
		return
	}
	_ = client.SetDeadline(time.Time{})
	_ = conn.SetDeadline(time.Time{})
	tunnel(client, buffered.Reader, conn)
}

// writeUpstreamRequest writes r to conn in origin form, the form a site reads, and leaves out the headers the
// guard owns.
func writeUpstreamRequest(conn net.Conn, r *http.Request) error {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.User = nil
	removeHopHeadersExceptUpgrade(out.Header)
	out.Header.Del("Proxy-Connection")
	out.Close = false
	if err := out.Write(conn); err != nil {
		return fmt.Errorf("netguard: write upgrade request: %w", err)
	}
	return nil
}

// serveConnect opens a CONNECT tunnel to an address that passed the check. The address is resolved once and dialled,
// so no second lookup can change where the tunnel goes.
func (p *Proxy) serveConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := splitConnectTarget(r.Host)
	if err != nil {
		http.Error(w, "netguard: "+err.Error(), http.StatusBadRequest)
		return
	}
	var conn net.Conn
	if p.AllowOrigin.matches(&url.URL{Scheme: "https", Host: net.JoinHostPort(host, port)}) {
		conn, err = p.dialer()(r.Context(), "tcp", net.JoinHostPort(host, port))
	} else {
		conn, err = p.dialChecked(r.Context(), "tcp", host, port)
	}
	if err != nil {
		http.Error(w, "netguard: upstream: "+err.Error(), http.StatusForbidden)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		conn.Close()
		http.Error(w, "netguard: connection cannot be upgraded", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		conn.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		conn.Close()
		return
	}
	tunnel(client, buffered.Reader, conn)
}

// tunnel copies both ways until either side is done, then closes both. What the browser sent past the CONNECT line
// and that bufio holds belongs to the tunnel as much as the rest.
func tunnel(client net.Conn, pending *bufio.Reader, upstream net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			client.Close()
			upstream.Close()
		})
	}
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		if pending != nil && pending.Buffered() > 0 {
			if _, err := io.Copy(upstream, pending); err != nil {
				closeBoth()
				return
			}
		}
		if _, err := io.Copy(upstream, client); err != nil {
			closeBoth()
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		if _, err := io.Copy(client, upstream); err != nil {
			closeBoth()
		}
	}()
	<-done
	closeBoth()
	<-done
}

// CheckRequest answers whether the proxy may carry r. DialContext refuses the same addresses again at connect time;
// this check refuses the request first, so a refused name never reaches the network.
func (p *Proxy) CheckRequest(r *http.Request) error {
	if r == nil || r.URL == nil {
		return fmt.Errorf("no URL")
	}
	if !r.URL.IsAbs() {
		return fmt.Errorf("proxy needs an absolute URL")
	}
	scheme := strings.ToLower(r.URL.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%s is not http or https", scheme)
	}
	if p.AllowOrigin.matches(r.URL) {
		return nil
	}
	host, _, err := hostAndPort(r.URL)
	if err != nil {
		return err
	}
	return checkHost(host, p.resolver())
}

// DialContext is the dialer for a request the proxy carries. The transport does not say which name a dial is for, so
// it checks the address alone: the request's name was checked by CheckRequest before the transport ran. Nothing is
// exempt here; serveForward names the one address an exempt origin may dial.
func (p *Proxy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return p.dialAddress(ctx, network, address, "")
}

// dialAddress dials address after the check, letting through the one address the exempt origin dials, if named.
func (p *Proxy) dialAddress(ctx context.Context, network, address, exempt string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("netguard: %w", err)
	}
	portNum, err := normalPort(port)
	if err != nil {
		return nil, err
	}
	host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(host, "]"), "["), ".")
	if exempt != "" && sameHostPort(exempt, host, port) {
		return p.dialer()(ctx, network, net.JoinHostPort(host, strconv.Itoa(portNum)))
	}
	return p.dialChecked(ctx, network, host, strconv.Itoa(portNum))
}

// sameHostPort reports whether address, as the transport writes it, names host and port.
func sameHostPort(address, host, port string) bool {
	aHost, aPort, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	aHost = strings.TrimSuffix(strings.TrimPrefix(aHost, "["), "]")
	return strings.EqualFold(aHost, host) && aPort == port
}

// dialChecked resolves host, refuses it if any address is not public, and dials an address it checked.
func (p *Proxy) dialChecked(ctx context.Context, network, host, port string) (net.Conn, error) {
	portNum, err := normalPort(port)
	if err != nil {
		return nil, err
	}
	addrs, err := p.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i, addr := range addrs {
		conn, err := p.dialer()(ctx, network, net.JoinHostPort(addr.String(), strconv.Itoa(portNum)))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil || i == len(addrs)-1 {
			break
		}
	}
	return nil, fmt.Errorf("netguard: dial %s: %w", net.JoinHostPort(host, strconv.Itoa(portNum)), lastErr)
}

// resolve answers the checked addresses of host, refusing the name if any of them is not public.
func (p *Proxy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if isBlockedHostName(host) {
		return nil, fmt.Errorf("netguard: %s is not a public host", host)
	}
	if addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(host, "]"), "["), ".")); err == nil {
		checked, err := checkedLiteral(addr)
		if err != nil {
			return nil, err
		}
		return []netip.Addr{checked}, nil
	}
	addrs, err := p.resolver().LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("netguard: cannot resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("netguard: %s resolved to no address", host)
	}
	checked := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		a = a.Unmap()
		if err := checkAddr(a); err != nil {
			return nil, fmt.Errorf("netguard: %s: %w", host, err)
		}
		checked = append(checked, dialForm(a))
	}
	return checked, nil
}

func (p *Proxy) resolver() Resolver {
	if p.Resolver != nil {
		return p.Resolver
	}
	return net.DefaultResolver
}

func (p *Proxy) dialer() DialFunc {
	if p.Dial != nil {
		return p.Dial
	}
	return (&net.Dialer{Timeout: connectTimeout}).DialContext
}

// upstream is the transport every forwarded request goes through: no environment proxy (it would skip the check),
// no redirect following (a redirect is the browser's to make, through the proxy again), and a dialer that checks.
// upstream is the transport a forwarded request goes through: no environment proxy (it would skip the check), no
// redirect following (a redirect is the browser's to make, through the proxy again), and a dialer that checks. It is
// built per request, so the one address an exempt origin dials belongs to that request alone.
func (p *Proxy) upstream(exempt string) *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return p.dialAddress(ctx, network, address, exempt)
		},
		ForceAttemptHTTP2:     false,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
}

// exemptDialAddress answers the host:port a URL on the exempt origin dials, or empty when it is not exempt.
func exemptDialAddress(u *url.URL, allow Origin) string {
	if !allow.matches(u) {
		return ""
	}
	host, port, err := hostAndPort(u)
	if err != nil {
		return ""
	}
	return net.JoinHostPort(host, port)
}

// splitConnectTarget splits a CONNECT target into its host and its port, filling in 443 for a bare host.
func splitConnectTarget(hostport string) (string, string, error) {
	if hostport == "" {
		return "", "", fmt.Errorf("CONNECT needs a host")
	}
	host, port, err := splitHostPort(hostport)
	if err != nil {
		return "", "", err
	}
	if host == "" {
		return "", "", fmt.Errorf("CONNECT needs a host")
	}
	if port == "" {
		port = "443"
	}
	return host, port, nil
}

// removeHopHeaders drops the headers a proxy owns rather than the site, so a response describes the site's message
// and not the connection carrying it.
// removeHopHeadersExceptUpgrade drops the connection headers but keeps Upgrade, for a message that carries it.
func removeHopHeadersExceptUpgrade(h http.Header) {
	upgrade := h.Get("Upgrade")
	removeHopHeaders(h)
	if upgrade != "" {
		h.Set("Upgrade", upgrade)
	}
}

// headerHasToken reports whether a comma-separated header such as Connection names token, in any case.
func headerHasToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

func removeHopHeaders(h http.Header) {
	for _, name := range []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		h.Del(name)
	}
}
