package netguard

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// proxiedClient answers an http.Client whose every request goes through the guard proxy, the way a browser is pointed
// at it, with the resolver and dialer the test chose.
func proxiedClient(t *testing.T, p *Proxy) *http.Client {
	t.Helper()
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	proxyURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true},
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // a redirect is the caller's to follow, as through the proxy again
		},
	}
}

// plainServer answers a plain-http site on a loopback httptest server.
func plainServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// hostOf answers the host of a test server's URL, without its port.
func hostOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %s: %v", srv.URL, err)
	}
	return u.Hostname()
}

// portOf answers the port of a test server's URL.
func portOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %s: %v", srv.URL, err)
	}
	return u.Port()
}

func TestProxyCarriesAllowedPublicSite(t *testing.T) {
	site := plainServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "" {
			t.Error("upstream request lost its Host")
		}
		if got := r.Header.Get("Proxy-Connection"); got != "" {
			t.Errorf("Proxy-Connection reached the site: %q", got)
		}
		fmt.Fprintf(w, "hello from %s", r.URL.Path)
	})
	// The injected resolver answers a public address for the site's name, and the injected dialer sends it to the
	// loopback test server, so no live network is touched.
	var dialled []string
	resolver := fakeResolver{"site.example": {"93.184.216.34"}}
	p := &Proxy{
		Resolver: resolver,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialled = append(dialled, address)
			return (&net.Dialer{}).DialContext(ctx, network, site.Listener.Addr().String())
		},
	}
	client := proxiedClient(t, p)
	resp, err := client.Get("http://site.example/page")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "hello from /page" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if len(dialled) != 1 || dialled[0] != "93.184.216.34:80" {
		t.Fatalf("the guard dialled %v, not the address it checked", dialled)
	}
}

func TestProxyRefusesPrivateAddress(t *testing.T) {
	site := plainServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a request to a private address reached its site")
	})
	var mu sync.Mutex
	dialled := 0
	p := &Proxy{
		Resolver: fakeResolver{"site.example": {"93.184.216.34"}},
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			dialled++
			mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, site.Listener.Addr().String())
		},
	}
	client := proxiedClient(t, p)
	cases := []string{
		site.URL, // a literal loopback address
		"http://localhost" + site.URL[len("http://"+site.Listener.Addr().String()):],
	}
	for _, target := range cases {
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("GET %s answered %d, not 403", target, resp.StatusCode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if dialled != 0 {
		t.Fatalf("a refused request was dialled %d times", dialled)
	}
}

func TestProxyRefusesNameThatResolvesToPrivateAddress(t *testing.T) {
	p := &Proxy{Resolver: fakeResolver{"rebind.example": {"93.184.216.34", "10.0.0.5"}}}
	client := proxiedClient(t, p)
	resp, err := client.Get("http://rebind.example/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a name with a private answer answered %d, not 403", resp.StatusCode)
	}
}

func TestProxyRefusesRedirectToPrivateAddress(t *testing.T) {
	// The private site answers if reached; the guard must stop the browser before it is.
	private := plainServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a redirect reached the private address")
	})
	public := plainServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, private.URL+"/secret", http.StatusFound)
	})
	resolver := fakeResolver{"site.example": {"93.184.216.34"}}
	p := &Proxy{
		Resolver: resolver,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, public.Listener.Addr().String())
		},
	}
	client := proxiedClient(t, p)
	// A browser follows the redirect itself, through the proxy again.
	resp, err := client.Get("http://site.example/start")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("the site answered %d, not a redirect", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	resp, err = client.Get(location)
	if err != nil {
		t.Fatalf("GET redirect: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the redirect to %s answered %d, not 403", location, resp.StatusCode)
	}
}

func TestProxyExemptsExactlyTheProviderOrigin(t *testing.T) {
	provider := plainServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "results")
	})
	host := hostOf(t, provider)
	port := portOf(t, provider)
	// The provider listens on a loopback address, as a provider on the local network would on its own address.
	exempt, err := ParseOrigin(provider.URL)
	if err != nil {
		t.Fatalf("ParseOrigin: %v", err)
	}
	p := &Proxy{
		AllowOrigin: exempt,
		Resolver:    fakeResolver{},
	}
	client := proxiedClient(t, p)
	resp, err := client.Get(provider.URL + "/search?q=x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the exempt provider answered %d", resp.StatusCode)
	}
	// A different port, a different scheme, a name for the same address and another name are not exempt, and all
	// resolve private.
	for _, target := range []string{
		"http://" + host + ":1/",
		fmt.Sprintf("http://127.0.0.2:%s/", port),
		"http://localhost:" + port + "/",
	} {
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s answered %d, not 403", target, resp.StatusCode)
		}
	}
}

func TestSplitConnectTarget(t *testing.T) {
	cases := []struct{ in, host, port string }{
		{"example.com:443", "example.com", "443"},
		{"example.com", "example.com", "443"},
		{"[::1]:8443", "::1", "8443"},
		{"[::1]", "::1", "443"},
	}
	for _, c := range cases {
		host, port, err := splitConnectTarget(c.in)
		if err != nil {
			t.Fatalf("splitConnectTarget(%q): %v", c.in, err)
		}
		if host != c.host || port != c.port {
			t.Fatalf("splitConnectTarget(%q) = %q, %q", c.in, host, port)
		}
	}
	for _, in := range []string{"", ":443", "example.com:0", "example.com:x"} {
		if _, _, err := splitConnectTarget(in); err == nil {
			t.Fatalf("splitConnectTarget(%q) accepted a malformed target", in)
		}
	}
}

// connect sends a CONNECT line to the guard and answers the line it replies with.
func connect(t *testing.T, proxyAddr, target string) (string, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial guard: %v", err)
	}
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		conn.Close()
		t.Fatalf("read guard reply: %v", err)
	}
	return strings.TrimSpace(line), conn
}

func TestProxyConnectRefusesPrivateAddress(t *testing.T) {
	guard := httptest.NewServer(&Proxy{})
	defer guard.Close()
	line, conn := connect(t, guard.Listener.Addr().String(), "127.0.0.1:443")
	defer conn.Close()
	if !strings.Contains(line, "403") {
		t.Fatalf("CONNECT to loopback answered %q, not 403", line)
	}
}

func TestProxyConnectTunnelsToCheckedAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn)
	}()
	addr := listener.Addr().String()
	// The tunnel target is a public name; the injected dialer sends it to the loopback echo server.
	guard := httptest.NewServer(&Proxy{
		Resolver: fakeResolver{"secure.example": {"93.184.216.34"}},
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "93.184.216.34:443" {
				return nil, fmt.Errorf("dialled %s, not the checked address", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	})
	defer guard.Close()
	line, conn := connect(t, guard.Listener.Addr().String(), "secure.example:443")
	defer conn.Close()
	if !strings.Contains(line, "200") {
		t.Fatalf("CONNECT answered %q", line)
	}
	// The tunnel carries what the client sends, in both directions.
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write tunnel: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("tunnel read %q, %v", buf, err)
	}
}

// upgradeSite answers a plain-http site on loopback that agrees to an upgrade, writes its handler's own reply line,
// and then echoes what it receives, so a test can tell a tunnel from an HTTP message read as a response.
func upgradeSite(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wantsUpgrade(r) || r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "no upgrade", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Proxy-Connection"); got != "" {
			t.Errorf("Proxy-Connection reached the site: %q", got)
		}
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")); err != nil {
			return
		}
		rest := make([]byte, 4)
		if _, err := io.ReadFull(buffered.Reader, rest); err == nil {
			conn.Write(append([]byte("echo:"), rest...))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// upgradeTunnel is a connection to the guard and the reader that holds its reply, which carries whatever the guard
// sent after the reply line.
type upgradeTunnel struct {
	conn net.Conn
	br   *bufio.Reader
}

// read answers n bytes from the guard, after any it sent past the reply.
func (u *upgradeTunnel) read(t *testing.T, n int) string {
	t.Helper()
	u.conn.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(u.br, buf); err != nil {
		t.Fatalf("read from the tunnel: %v", err)
	}
	return string(buf)
}

// upgradeThroughGuard sends an Upgrade request through the guard's plain-http forward path and answers the reply.
func upgradeThroughGuard(t *testing.T, proxyAddr, target string) (*http.Response, *upgradeTunnel) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial guard: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	u, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse %s: %v", target, err)
	}
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", target, u.Host)
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read guard reply: %v", err)
	}
	return resp, &upgradeTunnel{conn: conn, br: br}
}

func TestProxyTunnelsUpgradeToCheckedAddress(t *testing.T) {
	site := upgradeSite(t)
	var dialled []string
	guard := httptest.NewServer(&Proxy{
		Resolver: fakeResolver{"ws.example": {"93.184.216.34"}},
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialled = append(dialled, address)
			return (&net.Dialer{}).DialContext(ctx, network, site.Listener.Addr().String())
		},
	})
	defer guard.Close()
	resp, tun := upgradeThroughGuard(t, guard.Listener.Addr().String(), "http://ws.example/socket")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the upgrade answered %d %s, not 101", resp.StatusCode, resp.Status)
	}
	if got := resp.Header.Get("Upgrade"); got != "websocket" {
		t.Fatalf("the 101 carried Upgrade %q", got)
	}
	// What follows the 101 is a tunnel: the browser's bytes reach the site and its reply reaches the browser.
	if _, err := tun.conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write tunnel: %v", err)
	}
	if got := tun.read(t, len("echo:ping")); got != "echo:ping" {
		t.Fatalf("tunnel read %q", got)
	}
	if len(dialled) != 1 || dialled[0] != "93.184.216.34:80" {
		t.Fatalf("the guard dialled %v, not the address it checked", dialled)
	}
}

func TestProxyRefusesUpgradeToPrivateAddress(t *testing.T) {
	site := upgradeSite(t)
	var mu sync.Mutex
	dialled := 0
	guard := httptest.NewServer(&Proxy{
		Resolver: fakeResolver{"ws.example": {"93.184.216.34", "10.0.0.5"}},
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			dialled++
			mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, site.Listener.Addr().String())
		},
	})
	defer guard.Close()
	for _, target := range []string{
		"http://ws.example/socket",                  // a name with a private answer
		"http://127.0.0.1:" + portOf(t, site) + "/", // a literal loopback address
	} {
		resp, _ := upgradeThroughGuard(t, guard.Listener.Addr().String(), target)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("the upgrade to %s answered %d, not 403", target, resp.StatusCode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if dialled != 0 {
		t.Fatalf("a refused upgrade was dialled %d times", dialled)
	}
}

func TestProxyTunnelsUpgradeToExemptProvider(t *testing.T) {
	site := upgradeSite(t)
	exempt, err := ParseOrigin(site.URL)
	if err != nil {
		t.Fatalf("ParseOrigin: %v", err)
	}
	// The provider is on loopback and the resolver knows nothing, as a provider on the local network would be.
	guard := httptest.NewServer(&Proxy{AllowOrigin: exempt, Resolver: fakeResolver{}})
	defer guard.Close()
	resp, _ := upgradeThroughGuard(t, guard.Listener.Addr().String(), site.URL+"/socket")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the exempt provider's upgrade answered %d %s, not 101", resp.StatusCode, resp.Status)
	}
}

func TestGuardedClientRefusesPrivateAndFollowsPublic(t *testing.T) {
	site := plainServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	resolver := fakeResolver{"site.example": {"93.184.216.34"}}
	c := GuardedClient{
		Resolver: resolver,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, site.Listener.Addr().String())
		},
	}
	client := c.NewClient()
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://site.example/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if _, err := client.Get(site.URL); err == nil {
		t.Fatal("the guarded client fetched a loopback address")
	}
	// A redirect to a private address is refused at the hop, not followed.
	redirecting := plainServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, site.URL, http.StatusFound)
	})
	c.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, redirecting.Listener.Addr().String())
	}
	client = c.NewClient()
	defer client.CloseIdleConnections()
	if _, err := client.Get("http://site.example/start"); err == nil {
		t.Fatal("the guarded client followed a redirect to a loopback address")
	}
}
