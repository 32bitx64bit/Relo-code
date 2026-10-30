// Package netguard keeps Relo's web capability off the machine it runs on. CheckURL refuses addresses that point
// back into the host, its network or the cloud it runs on, and the guard proxy resolves names itself so a page cannot
// change what a name means between the check and the dial.
//
// The user may name one search provider, the only origin the guard lets through even when it resolves to a private
// address: a provider on the local network is a deliberate choice, not a trick.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// resolveTimeout bounds the lookup CheckURL makes, so a slow resolver cannot hold a caller past one check.
const resolveTimeout = 10 * time.Second

// Origin is one scheme, host and port a page may be fetched from. It is built from an absolute URL and an explicit
// port, so two URLs that differ in scheme, host or port never share an Origin.
type Origin struct {
	scheme string
	host   string
	port   string
}

// ParseOrigin reads a URL as an origin, such as "https://search.example.com". A URL with a query or fragment is
// refused, and a path is refused too: the exemption covers a provider's origin, not pages on it.
func ParseOrigin(raw string) (Origin, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Origin{}, fmt.Errorf("netguard: empty origin")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return Origin{}, fmt.Errorf("netguard: origin %q: %w", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return Origin{}, fmt.Errorf("netguard: origin %q: %s is not http or https", raw, scheme)
	}
	if u.User != nil {
		return Origin{}, fmt.Errorf("netguard: origin %q must not carry a user", raw)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return Origin{}, fmt.Errorf("netguard: origin %q must not carry a query or fragment", raw)
	}
	if p := u.EscapedPath(); p != "" && p != "/" {
		return Origin{}, fmt.Errorf("netguard: origin %q must not carry a path", raw)
	}
	host, port, err := hostAndPort(u)
	if err != nil {
		return Origin{}, fmt.Errorf("netguard: origin %q: %w", raw, err)
	}
	return Origin{scheme: scheme, host: host, port: port}, nil
}

// String renders the origin as its caller usually writes it: scheme and host, with the port only when it is not the
// scheme's default.
func (o Origin) String() string {
	if o.host == "" {
		return ""
	}
	if o.port == defaultPort(o.scheme) {
		return o.scheme + "://" + o.host
	}
	return o.scheme + "://" + net.JoinHostPort(o.host, o.port)
}

// IsZero reports whether no origin was named.
func (o Origin) IsZero() bool { return o.host == "" }

// matches reports whether u names exactly this origin. The port is compared as resolved, so ":443" and an absent
// port are the same https origin, and a host is compared case-insensitively and without its trailing dot.
func (o Origin) matches(u *url.URL) bool {
	if o.host == "" || u == nil {
		return false
	}
	host, port, err := hostAndPort(u)
	if err != nil {
		return false
	}
	return o.scheme == strings.ToLower(u.Scheme) && o.host == host && o.port == port
}

// CheckURL inspects a URL before it is fetched, answering nil only for a public http or https one. A URL on exactly
// allowOrigin is exempt; pass the zero Origin to exempt nothing. Names are resolved, so a name that points at a
// private address is refused like the address itself.
func CheckURL(raw string, allowOrigin Origin) error {
	return CheckURLWithResolver(raw, allowOrigin, nil)
}

// CheckURLWithResolver is CheckURL with the resolver named, so a caller that already holds one, such as the guard
// proxy, judges a URL exactly as it will dial it, and a test answers names without a network. A nil resolver means
// the system resolver.
func CheckURLWithResolver(raw string, allowOrigin Origin, resolver Resolver) error {
	_, err := checkURL(raw, allowOrigin, resolver)
	return err
}

// CheckURLString is CheckURL for a caller that holds only text, such as the web engine's API, where an unparsable
// allowOrigin is refused rather than silently exempting nothing.
func CheckURLString(raw, allowOrigin string) error {
	var exempt Origin
	if strings.TrimSpace(allowOrigin) != "" {
		parsed, err := ParseOrigin(allowOrigin)
		if err != nil {
			return err
		}
		exempt = parsed
	}
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && exempt.matches(u) {
		return nil
	}
	return CheckURL(raw, exempt)
}

// checkURL is CheckURL with the resolver named, so a test can answer with the addresses it wants.
func checkURL(raw string, allowOrigin Origin, resolver Resolver) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("netguard: empty URL")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("netguard: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https":
	case "file":
		return nil, fmt.Errorf("netguard: file URLs are not allowed")
	case "":
		return nil, fmt.Errorf("netguard: URL needs a scheme such as https")
	default:
		return nil, fmt.Errorf("netguard: %s is not http or https", scheme)
	}
	if u.Opaque != "" {
		return nil, fmt.Errorf("netguard: opaque URL is not allowed")
	}
	host, _, err := hostAndPort(u)
	if err != nil {
		return nil, err
	}
	if allowOrigin.matches(u) {
		return u, nil
	}
	if err := checkHost(host, resolver); err != nil {
		return nil, err
	}
	return u, nil
}

// CheckRemoteIP checks one address a request would reach, such as a CONNECT target or a socket the browser already
// opened. It refuses everything CheckURL refuses, with no exemption: both judge an address with checkAddr, and both
// unwrap the forms that carry an IPv4 address before they do.
func CheckRemoteIP(ip net.IP) error {
	if len(ip) == 0 {
		return fmt.Errorf("netguard: empty address")
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return fmt.Errorf("netguard: not an IP address")
	}
	if err := checkAddr(addr); err != nil {
		return err
	}
	return nil
}

// checkHost refuses a host that is, or resolves to, an address inside the host or its network.
func checkHost(host string, resolver Resolver) error {
	if isBlockedHostName(host) {
		return fmt.Errorf("netguard: %q is not a public host", host)
	}
	if addr, err := netip.ParseAddr(strings.TrimSuffix(host, ".")); err == nil {
		if err := checkAddr(addr.Unmap()); err != nil {
			return fmt.Errorf("netguard: %w", err)
		}
		return nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("netguard: cannot resolve %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("netguard: %q resolved to no address", host)
	}
	for _, a := range addrs {
		if err := checkAddr(a.Unmap()); err != nil {
			return fmt.Errorf("netguard: %q: %w", host, err)
		}
	}
	return nil
}

// checkAddr is the list of addresses a page may not reach. An IPv4-mapped, IPv4-compatible, NAT64 or 6to4 form is
// unwrapped first, so an address is judged by the IPv4 address it really is.
func checkAddr(addr netip.Addr) error {
	if !addr.IsValid() {
		return fmt.Errorf("netguard: invalid address")
	}
	addr = addr.WithZone("").Unmap()
	if v4, ok := embeddedIPv4(addr); ok {
		addr = v4
	}
	switch {
	case addr.IsUnspecified():
		return fmt.Errorf("netguard: %s is unspecified", addr)
	case addr.IsLoopback():
		return fmt.Errorf("netguard: %s is loopback", addr)
	case addr.IsMulticast() || addr.IsInterfaceLocalMulticast():
		return fmt.Errorf("netguard: %s is multicast", addr)
	case addr.IsLinkLocalUnicast():
		return fmt.Errorf("netguard: %s is link-local", addr)
	case addr.IsPrivate():
		return fmt.Errorf("netguard: %s is private", addr)
	case addr.Is4() && isBlockedIPv4(addr):
		return fmt.Errorf("netguard: %s is not a public address", addr)
	case addr.Is6() && isBlockedIPv6(addr):
		return fmt.Errorf("netguard: %s is not a public address", addr)
	}
	return nil
}

// metadataIPv4 are the addresses cloud providers answer on from inside a machine. Some sit in ranges a page might
// otherwise reach, so they are refused by name too.
var metadataIPv4 = mustPrefixes([]string{
	"169.254.169.254/32", // AWS, GCP, Azure, DigitalOcean, OpenStack
	"169.254.170.2/32",   // AWS ECS task metadata
	"169.254.169.253/32", // AWS DNS
	"100.100.100.200/32", // Alibaba Cloud
	"192.0.0.192/32",     // Oracle Cloud
	"168.63.129.16/32",   // Azure WireServer
})

// blockedIPv4 are the IPv4 ranges beyond the private ones that no public site lives in: this-network, shared
// (CGNAT), documentation, benchmarking, carrier and the rest of link-local.
var blockedIPv4 = mustPrefixes([]string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",  // CGNAT, and the cloud's own networks
	"127.0.0.0/8",    // loopback
	"169.254.0.0/16", // link-local, including metadata
	"172.16.0.0/12",
	"192.0.0.0/24",   // IETF protocol assignments
	"192.0.2.0/24",   // documentation
	"192.88.99.0/24", // 6to4 relay
	"192.168.0.0/16",
	"198.18.0.0/15", // benchmarking
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4", // multicast
	"240.0.0.0/4", // reserved, including broadcast
})

// blockedIPv6 are the IPv6 ranges besides the private, loopback, link-local and multicast ones that a page may not
// reach. The ranges that carry an IPv4 address (64:ff9b::/96, ::ffff:0:0/96, 6to4 and IPv4-compatible) are judged by
// the address they carry, in embeddedIPv4.
var blockedIPv6 = mustPrefixes([]string{
	"64:ff9b:1::/48", // local-use NAT64
	"100::/64",       // discard-only
	"2001::/23",      // IETF protocol assignments, including Teredo and benchmarking
	"2001:db8::/32",  // documentation
	"2002::/16",      // 6to4
	"3fff::/20",      // documentation
	"5f00::/16",      // segment routing
	"fc00::/7",       // unique local
	"fe80::/10",      // link-local
	"fec0::/10",      // site-local, withdrawn
	"ff00::/8",       // multicast
})

// mustPrefixes parses a list of ranges at load time. Each is masked, because ParsePrefix keeps the address's host
// bits and a range with host bits set, such as 2001:db8::/32 written out, would contain nothing it should.
func mustPrefixes(list []string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(list))
	for _, raw := range list {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			panic("netguard: bad prefix " + raw + ": " + err.Error())
		}
		prefixes = append(prefixes, p.Masked())
	}
	return prefixes
}

func isBlockedIPv4(addr netip.Addr) bool {
	if containsPrefix(metadataIPv4, addr) || containsPrefix(blockedIPv4, addr) {
		return true
	}
	return false
}

func isBlockedIPv6(addr netip.Addr) bool {
	// A global unicast address in no blocked range is the only kind with a public site on it.
	if !addr.IsGlobalUnicast() {
		return true
	}
	return containsPrefix(blockedIPv6, addr)
}

func containsPrefix(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// blockedSuffixes are names that only mean something inside a machine or its network. A resolver outside it may
// still answer them, so they are refused by their text.
var blockedSuffixes = []string{
	".localhost",
	".local",
	".localdomain",
	".internal",
	".intranet",
	".lan",
	".home",
	".corp",
	".private",
}

// isBlockedHostName refuses the names that need no lookup to know they are not public. A bare single label such as
// "intranet" is only what a search domain in the machine's resolver makes it, so it is refused too. A host that names
// an address is not judged here: checkAddr reads it.
func isBlockedHostName(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" || h == "localhost" {
		return true
	}
	if net.ParseIP(h) == nil && !strings.Contains(h, ".") && !strings.Contains(h, ":") {
		return true
	}
	for _, suffix := range blockedSuffixes {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

// embeddedIPv4 answers the IPv4 address an IPv6 address carries, for the ways to write one.
func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	if !addr.Is6() {
		return netip.Addr{}, false
	}
	if addr.Is4In6() {
		return addr.Unmap(), true
	}
	b := addr.As16()
	// NAT64 well-known prefix, 64:ff9b::a.b.c.d, and the deprecated IPv4-compatible form, ::a.b.c.d.
	if b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b {
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	}
	if allZero(b[:12]) {
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	}
	// 6to4, 2002:aabb:ccdd::, carries the IPv4 address in its second and third groups.
	if b[0] == 0x20 && b[1] == 0x02 {
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// checkedLiteral answers the address a literal host stands for after the check, as it will be dialled: an embedded
// IPv4 address is dialled as that IPv4 address, so no dialer has to decide what an odd form means.
func checkedLiteral(addr netip.Addr) (netip.Addr, error) {
	addr = addr.WithZone("").Unmap()
	if err := checkAddr(addr); err != nil {
		return netip.Addr{}, fmt.Errorf("netguard: %w", err)
	}
	return dialForm(addr), nil
}

// dialForm answers the address to dial for a checked one.
func dialForm(addr netip.Addr) netip.Addr {
	if v4, ok := embeddedIPv4(addr); ok {
		return v4
	}
	return addr
}

// hostAndPort splits a URL's host into the lower-case name without brackets or trailing dot and the port it will
// dial, filling in the scheme's default.
func hostAndPort(u *url.URL) (string, string, error) {
	if u == nil {
		return "", "", fmt.Errorf("netguard: no URL")
	}
	if u.Opaque != "" {
		return "", "", fmt.Errorf("netguard: opaque URL is not allowed")
	}
	if u.User != nil || strings.Contains(u.Host, "@") {
		return "", "", fmt.Errorf("netguard: URL must not carry a user")
	}
	host, port, err := splitHostPort(u.Host)
	if err != nil {
		return "", "", err
	}
	if host == "" {
		return "", "", fmt.Errorf("netguard: URL needs a host")
	}
	if port == "" {
		port = defaultPort(strings.ToLower(u.Scheme))
	}
	if port == "" {
		return "", "", fmt.Errorf("netguard: URL needs a port")
	}
	if _, err := normalPort(port); err != nil {
		return "", "", err
	}
	return host, port, nil
}

// splitHostPort splits "host", "host:port", "[v6]" or "[v6]:port" without the strictness of net.SplitHostPort, which
// refuses a bare IPv6 literal. host is lower-cased and loses its brackets and trailing dot.
func splitHostPort(hostport string) (string, string, error) {
	host, port := hostport, ""
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		if end < 0 {
			return "", "", fmt.Errorf("netguard: malformed host %q", hostport)
		}
		host = hostport[1:end]
		rest := hostport[end+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return "", "", fmt.Errorf("netguard: malformed host %q", hostport)
			}
			port = rest[1:]
		}
	} else if i := strings.LastIndexByte(hostport, ':'); i >= 0 {
		host, port = hostport[:i], hostport[i+1:]
		if strings.Contains(host, ":") {
			return "", "", fmt.Errorf("netguard: IPv6 host %q must be in brackets", hostport)
		}
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if port != "" {
		if _, err := normalPort(port); err != nil {
			return "", "", err
		}
	}
	return host, port, nil
}

// normalPort reads a port number and refuses one that cannot be a service. A zero, out of range or named port is a
// parse problem, not a name the resolver can help with.
func normalPort(port string) (int, error) {
	if port == "" {
		return 0, fmt.Errorf("netguard: a port is required")
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("netguard: %q is not a numeric port", port)
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n == 0 || n > 65535 {
		return 0, fmt.Errorf("netguard: port %q is out of range", port)
	}
	return n, nil
}

func defaultPort(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}
