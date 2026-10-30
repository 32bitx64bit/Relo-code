package netguard

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeResolver answers the names a test lists and refuses the rest, so no test looks anything up.
type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	answers, ok := f[h]
	if !ok {
		answers, ok = f["*"+h]
	}
	if !ok {
		return nil, fmt.Errorf("fake resolver: no answer for %s", host)
	}
	addrs := make([]netip.Addr, 0, len(answers))
	for _, a := range answers {
		addr, err := netip.ParseAddr(a)
		if err != nil {
			return nil, err
		}
		addrs = append(addrs, addr)
	}
	return addrs, nil
}

// noNetworkResolver answers nothing and records being asked, so a test proves no lookup was made.
type noNetworkResolver struct {
	t         *testing.T
	numLookup int32
}

func (n *noNetworkResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	atomic.AddInt32(&n.numLookup, 1)
	n.t.Errorf("the check resolved %q through a resolver; a refused name must not need one", host)
	return nil, fmt.Errorf("no network in a test")
}

func TestCheckURLRefusesEveryPrivateAddressClass(t *testing.T) {
	resolver := &noNetworkResolver{t: t}
	// file and odd schemes never reach a name, so they are refused before any lookup.
	odd := []string{
		"file:///etc/passwd",
		"file://localhost/etc/passwd",
		"ftp://example.com/",
		"javascript:alert(1)",
		"data:text/html,hi",
		"blob:https://example.com/x",
		"ws://example.com/",
		"/relative/path",
		"//example.com/",
		// ports that cannot be a service
		"http://127.0.0.1:0/",
		"http://8.8.8.8:70000/",
		"http://8.8.8.8:https/",
		// no host, or a user and password hiding one
		"http:///path",
		"http:example.com",
		"http://user:secret@8.8.8.8/",
	}
	for _, raw := range odd {
		t.Run(raw, func(t *testing.T) {
			if err := CheckURLWithResolver(raw, Origin{}, resolver); err == nil {
				t.Fatalf("CheckURL(%q) accepted a private or odd URL", raw)
			}
		})
	}
	for _, c := range addressClassCases {
		t.Run(c.url, func(t *testing.T) {
			if err := CheckURLWithResolver(c.url, Origin{}, resolver); err == nil {
				t.Fatalf("CheckURL(%q) accepted a private address", c.url)
			}
			if err := CheckRemoteIP(net.ParseIP(c.ip)); err == nil {
				t.Fatalf("CheckRemoteIP(%s) accepted the same address CheckURL refused", c.ip)
			}
		})
	}
	// Names that only mean something inside a machine or its network are refused by their text, so none is looked up.
	for _, raw := range []string{
		"http://localhost/",
		"http://LOCALHOST./",
		"http://api.localhost/",
		"http://printer.local/",
		"http://router.lan/",
		"http://service.corp/",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://intranet/",
		"http://router/",
	} {
		t.Run(raw, func(t *testing.T) {
			if err := CheckURLWithResolver(raw, Origin{}, resolver); err == nil {
				t.Fatalf("CheckURL(%q) accepted a private name", raw)
			}
		})
	}
	// Odd numeric host forms: the resolver reads these as 127.0.0.1, so they need a lookup; it answers, and the
	// answer is refused. No live resolver is asked.
	for _, raw := range []string{
		"http://0x7f.1/",
		"http://127.1/",
	} {
		t.Run(raw, func(t *testing.T) {
			numeric := fakeResolver{"*": {"127.0.0.1"}}
			if err := CheckURLWithResolver(raw, Origin{}, numeric); err == nil {
				t.Fatalf("CheckURL(%q) accepted an odd form of loopback", raw)
			}
		})
	}
	// The cases judged above from their text made no lookup.
	if n := atomic.LoadInt32(&resolver.numLookup); n != 0 {
		t.Fatalf("%d lookups were made; the refused cases must need no resolver", n)
	}
}

func TestCheckURLAcceptsPublicAddresses(t *testing.T) {
	// Every name in this table is answered by the resolver below, so no test looks anything up.
	resolver := fakeResolver{
		"*example.com":     {"93.184.216.34"},
		"127.1.2.3.nip.io": {"127.0.0.1"}, // the name is public; its answer is not
	}
	cases := []string{
		"https://example.com/",
		"http://example.com:8080/page?q=1#top",
		"https://93.184.216.34/",
		"https://8.8.8.8/",
		"http://[2606:4700:4700::1111]/",
		"https://example.com./",
		"http://EXAMPLE.com/",
		"https://100.63.255.255/", // just under CGNAT
		"https://172.32.0.1/",     // just above private
		"https://[64:ff9b::808:808]/",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if _, err := checkURL(raw, Origin{}, resolver); err != nil {
				t.Fatalf("checkURL(%q) refused a public URL: %v", raw, err)
			}
		})
	}
	// A public name whose answer is private is refused by the answer, not by the name.
	if _, err := checkURL("http://127.1.2.3.nip.io/", Origin{}, resolver); err == nil {
		t.Fatal("a public name answering loopback was accepted")
	}
}

func TestCheckURLRefusesNamesThatResolveToPrivateAddresses(t *testing.T) {
	private := []string{"127.0.0.1", "10.1.2.3", "192.168.1.1", "169.254.169.254", "::1", "::ffff:127.0.0.1",
		"fd00::1", "fe80::1", "100.64.0.1", "64:ff9b::a9fe:a9fe", "0.0.0.0"}
	for _, answer := range private {
		t.Run(answer, func(t *testing.T) {
			resolver := fakeResolver{"private.example": {answer}}
			if _, err := checkURL("http://private.example/", Origin{}, resolver); err == nil {
				t.Fatalf("a name resolving to %s was accepted", answer)
			}
		})
	}
	// One private answer among public ones is enough to refuse.
	t.Run("one private among public", func(t *testing.T) {
		resolver := fakeResolver{"mixed.example": {"93.184.216.34", "10.0.0.7"}}
		if _, err := checkURL("http://mixed.example/", Origin{}, resolver); err == nil {
			t.Fatal("a name with one private answer was accepted")
		}
	})
	t.Run("public answers", func(t *testing.T) {
		resolver := fakeResolver{"public.example": {"93.184.216.34", "2606:4700:4700::1111"}}
		if _, err := checkURL("http://public.example/", Origin{}, resolver); err != nil {
			t.Fatalf("a public name was refused: %v", err)
		}
	})
	t.Run("no answer", func(t *testing.T) {
		if _, err := checkURL("http://nobody.example/", Origin{}, fakeResolver{}); err == nil {
			t.Fatal("a name with no answer was accepted")
		}
	})
}

func TestExemptOriginMatchesOnlyItself(t *testing.T) {
	exempt, err := ParseOrigin("https://search.example.com")
	if err != nil {
		t.Fatalf("ParseOrigin: %v", err)
	}
	cases := []struct {
		url   string
		allow bool
	}{
		{"https://search.example.com/", true},
		{"https://search.example.com:443/", true},
		{"https://SEARCH.example.com/?q=x", true},
		{"https://search.example.com./", true},
		{"https://search.example.com:8443/", false},
		{"http://search.example.com/", false},
		{"http://search.example.com:80/", false},
		{"https://other.example.com/", false},
		{"https://sub.search.example.com/", false},
		{"https://search.example.com.evil.test/", false},
		{"https://search.example.com@evil.test/", false},
	}
	for _, c := range cases {
		t.Run(c.url, func(t *testing.T) {
			// The name answers a private address, as a provider on the local network would.
			resolver := fakeResolver{
				"search.example.com":     {"192.168.0.10"},
				"other.example.com":      {"192.168.0.11"},
				"sub.search.example.com": {"192.168.0.12"},
			}
			_, err := checkURL(c.url, exempt, resolver)
			if c.allow && err != nil {
				t.Fatalf("%q should be exempt: %v", c.url, err)
			}
			if !c.allow && err == nil {
				t.Fatalf("%q should have been refused", c.url)
			}
		})
	}
}

func TestParseOriginRefusesWhatIsNotAnOrigin(t *testing.T) {
	for _, raw := range []string{"", "example.com", "ftp://example.com", "https://example.com/path", "https://example.com/?q=1",
		"https://user@example.com", "https://example.com/#x", "https://example.com:0"} {
		if _, err := ParseOrigin(raw); err == nil {
			t.Fatalf("ParseOrigin(%q) accepted something that is not an origin", raw)
		}
	}
	o, err := ParseOrigin("http://127.0.0.1:8080/")
	if err != nil {
		t.Fatalf("a provider on loopback should parse: %v", err)
	}
	if o.String() != "http://127.0.0.1:8080" {
		t.Fatalf("Origin.String() = %q", o.String())
	}
}

func TestCheckURLStringReadsTheAllowOriginText(t *testing.T) {
	// The exemption is applied to the URL itself, so a provider on loopback needs no lookup to pass.
	for raw, allow := range map[string]string{
		"http://127.0.0.1:8080/search": "http://127.0.0.1:8080",
		"http://[::1]:9000/x":          "http://[::1]:9000",
	} {
		if err := CheckURLString(raw, allow); err != nil {
			t.Fatalf("the exempt provider %s was refused: %v", raw, err)
		}
	}
	if err := CheckURLString("http://127.0.0.1:8081/search", "http://127.0.0.1:8080"); err == nil {
		t.Fatal("a different port was exempt")
	}
	if err := CheckURLString("https://127.0.0.1:8080/", "http://127.0.0.1:8080"); err == nil {
		t.Fatal("a different scheme was exempt")
	}
	if err := CheckURLString("https://8.8.8.8/", "not a url"); err == nil {
		t.Fatal("an unparsable exemption was accepted")
	}
}

// addressClassCases is the one table both CheckURL and CheckRemoteIP are judged by: each address, the URL that names
// it, and the class it belongs to. Every case here must be refused by both, so the two can never disagree about an
// address.
var addressClassCases = []struct{ name, ip, url string }{
	// IPv4 loopback, private, link-local, CGNAT and friends
	{"ipv4 loopback", "127.0.0.1", "http://127.0.0.1/"},
	{"ipv4 loopback last octet", "127.0.0.255", "http://127.0.0.255/"},
	{"ipv4 private /8", "10.0.0.1", "http://10.0.0.1/"},
	{"ipv4 private /8 high", "10.255.255.254", "http://10.255.255.254/"},
	{"ipv4 private /12", "172.16.0.1", "http://172.16.0.1/"},
	{"ipv4 private /12 high", "172.31.255.255", "http://172.31.255.255/"},
	{"ipv4 private /16", "192.168.0.1", "http://192.168.0.1/"},
	{"ipv4 private /16 high", "192.168.255.255", "http://192.168.255.255/"},
	{"ipv4 link-local", "169.254.1.1", "http://169.254.1.1/"},
	{"ipv4 cgnat", "100.64.0.1", "http://100.64.0.1/"},
	{"ipv4 cgnat high", "100.127.255.255", "http://100.127.255.255/"},
	{"ipv4 unspecified", "0.0.0.0", "http://0.0.0.0/"},
	{"ipv4 unspecified short", "0.0.0.0", "http://0/"},
	{"ipv4 multicast", "224.0.0.1", "http://224.0.0.1/"},
	{"ipv4 multicast ssdp", "239.255.255.250", "http://239.255.255.250/"},
	{"ipv4 broadcast", "255.255.255.255", "http://255.255.255.255/"},
	{"ipv4 reserved", "240.0.0.1", "http://240.0.0.1/"},
	{"ipv4 documentation 192.0.2", "192.0.2.1", "http://192.0.2.1/"},
	{"ipv4 documentation 198.51.100", "198.51.100.1", "http://198.51.100.1/"},
	{"ipv4 documentation 203.0.113", "203.0.113.1", "http://203.0.113.1/"},
	{"ipv4 benchmarking", "198.18.0.1", "http://198.18.0.1/"},
	{"ipv4 6to4 relay", "192.88.99.1", "http://192.88.99.1/"},
	{"ipv4 this-network", "0.1.2.3", "http://0.1.2.3/"},
	// cloud metadata
	{"metadata aws", "169.254.169.254", "http://169.254.169.254/latest/meta-data/"},
	{"metadata ecs", "169.254.170.2", "http://169.254.170.2/v2/credentials"},
	{"metadata alibaba", "100.100.100.200", "http://100.100.100.200/latest/meta-data/"},
	{"metadata oracle", "192.0.0.192", "http://192.0.0.192/opc/v1/instance/"},
	{"metadata azure", "168.63.129.16", "http://168.63.129.16/machine/?role=1"},
	// IPv6 loopback, private, link-local, multicast, unspecified, documentation and odd ranges
	{"ipv6 loopback", "::1", "http://[::1]/"},
	{"ipv6 unspecified", "::", "http://[::]/"},
	{"ipv6 unique local", "fc00::1", "http://[fc00::1]/"},
	{"ipv6 unique local fd", "fd12:3456:789a::1", "http://[fd12:3456:789a::1]/"},
	{"ipv6 link-local", "fe80::1", "http://[fe80::1]/"},
	{"ipv6 multicast", "ff02::1", "http://[ff02::1]/"},
	{"ipv6 site-local", "fec0::1", "http://[fec0::1]/"},
	{"ipv6 documentation", "2001:db8::1", "http://[2001:db8::1]/"},
	{"ipv6 ietf assignments", "2001::1", "http://[2001::1]/"},
	{"ipv6 documentation 3fff", "3fff::1", "http://[3fff::1]/"},
	{"ipv6 segment routing", "5f00::1", "http://[5f00::1]/"},
	{"ipv6 local nat64", "64:ff9b:1::a9fe:a9fe", "http://[64:ff9b:1::a9fe:a9fe]/"},
	{"ipv6 discard", "100::1", "http://[100::1]/"},
	// IPv4-mapped, IPv4-compatible, NAT64 and 6to4 forms of private and metadata addresses
	{"mapped loopback", "::ffff:127.0.0.1", "http://[::ffff:127.0.0.1]/"},
	{"mapped loopback hex", "::ffff:7f00:1", "http://[::ffff:7f00:1]/"},
	{"mapped private", "::ffff:10.0.0.1", "http://[::ffff:10.0.0.1]/"},
	{"mapped metadata", "::ffff:169.254.169.254", "http://[::ffff:169.254.169.254]/"},
	{"compatible loopback", "::127.0.0.1", "http://[::127.0.0.1]/"},
	{"compatible loopback hex", "::7f00:1", "http://[::7f00:1]/"},
	{"nat64 loopback", "64:ff9b::127.0.0.1", "http://[64:ff9b::127.0.0.1]/"},
	{"nat64 metadata", "64:ff9b::a9fe:a9fe", "http://[64:ff9b::a9fe:a9fe]/"},
	{"6to4 loopback", "2002:7f00:1::", "http://[2002:7f00:1::]/"},
	{"6to4 metadata", "2002:a9fe:a9fe::", "http://[2002:a9fe:a9fe::]/"},
}

func TestCheckRemoteIPRefusesTheSameAddressesAsCheckURL(t *testing.T) {
	for _, c := range addressClassCases {
		t.Run(c.name, func(t *testing.T) {
			ip := net.ParseIP(c.ip)
			if ip == nil {
				t.Fatalf("%q is not an address", c.ip)
			}
			if err := CheckRemoteIP(ip); err == nil {
				t.Fatalf("CheckRemoteIP(%s) accepted %s", c.ip, c.name)
			}
			if err := CheckURLWithResolver(c.url, Origin{}, fakeResolver{}); err == nil {
				t.Fatalf("CheckURL(%s) accepted %s", c.url, c.name)
			}
		})
	}
	if err := CheckRemoteIP(nil); err == nil {
		t.Fatal("CheckRemoteIP(nil) accepted nothing as something")
	}
	if err := CheckRemoteIP(net.IP{1, 2, 3}); err == nil {
		t.Fatal("CheckRemoteIP accepted a malformed address")
	}
}

func TestCheckRemoteIPAcceptsPublicAddresses(t *testing.T) {
	for _, raw := range []string{"93.184.216.34", "8.8.8.8", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if err := CheckRemoteIP(net.ParseIP(raw)); err != nil {
			t.Fatalf("CheckRemoteIP(%s) refused a public address: %v", raw, err)
		}
	}
}

func TestBlockedRangesAreMasked(t *testing.T) {
	// A range written with host bits set must still contain the addresses it means, and not only the masked address.
	for _, p := range append(append([]netip.Prefix{}, blockedIPv4...), append(metadataIPv4, blockedIPv6...)...) {
		if p != p.Masked() {
			t.Fatalf("blocked range %s is not masked", p)
		}
	}
}

func TestSplitHostPort(t *testing.T) {
	cases := []struct{ in, host, port string }{
		{"example.com", "example.com", ""},
		{"Example.COM.:8443", "example.com", "8443"},
		{"[::1]", "::1", ""},
		{"[::1]:443", "::1", "443"},
		{"[::ffff:127.0.0.1]:80", "::ffff:127.0.0.1", "80"},
	}
	for _, c := range cases {
		host, port, err := splitHostPort(c.in)
		if err != nil {
			t.Fatalf("splitHostPort(%q): %v", c.in, err)
		}
		if host != c.host || port != c.port {
			t.Fatalf("splitHostPort(%q) = %q, %q", c.in, host, port)
		}
	}
	for _, in := range []string{"[::1", "::1", "example.com:0", "example.com:x"} {
		if _, _, err := splitHostPort(in); err == nil {
			t.Fatalf("splitHostPort(%q) accepted a malformed host", in)
		}
	}
}
