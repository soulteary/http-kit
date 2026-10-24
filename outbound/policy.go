// Derived from OwlCron. See ../LICENSE-OWLCRON and ../NOTICE.

package outbound

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrBlocked reports an invalid or prohibited destination. It never includes
// the URL, hostname, or address. Use errors.Is to classify wrapped client errors.
var ErrBlocked = errors.New("outbound destination is prohibited by security policy")

// ErrInvalidOptions reports malformed configuration without repeating its input.
var ErrInvalidOptions = errors.New("invalid outbound policy options")

// ErrDNSResolution reports a failed DNS lookup without exposing resolver details.
var ErrDNSResolution = errors.New("outbound DNS resolution failed")

// ErrNoAddresses reports a successful lookup that returned no addresses.
var ErrNoAddresses = errors.New("outbound DNS returned no addresses")

// Options configures exceptions to the default public-HTTPS policy. Entries do
// not restrict access to other public HTTPS destinations. Wildcards, URL schemes,
// ports, trailing dots, scoped IPv6 addresses and IPv4-mapped CIDRs are rejected.
// Use exact DNS hostnames, bare IP literals, or CIDRs respectively.
// Metadata and other always-prohibited addresses remain blocked by any exception.
// NewPolicy copies and normalizes these slices; later caller mutations have no effect.
type Options struct {
	AllowedHosts []string
	AllowedCIDRs []string
}

// Policy is an immutable destination policy, safe for concurrent requests.
// Its zero value permits public HTTPS and no other destinations.
type Policy struct {
	hosts    map[string]bool
	networks []netip.Prefix
	lookup   func(context.Context, string) ([]net.IPAddr, error)
	dial     func(context.Context, string, string) (net.Conn, error)
}

// NewPolicy validates the options and returns an independent policy. An invalid
// entry rejects the entire configuration; errors never contain the supplied entry.
func NewPolicy(options Options) (*Policy, error) {
	p := &Policy{hosts: make(map[string]bool)}
	for _, host := range options.AllowedHosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" || strings.ContainsAny(host, "/@?#*%\\ \t\r\n") || strings.HasSuffix(host, ".") {
			return nil, ErrInvalidOptions
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			host = ip.Unmap().String()
		} else if strings.Contains(host, ":") || !validDNSHost(host) {
			return nil, ErrInvalidOptions
		}
		p.hosts[host] = true
	}
	for _, raw := range options.AllowedCIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Addr().Is4In6() {
			return nil, ErrInvalidOptions
		}
		p.networks = append(p.networks, prefix.Masked())
	}
	return p, nil
}

func validDNSHost(host string) bool {
	if len(host) > 253 || host == "" {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}
func parseURL(target string) (*url.URL, error) {
	u, err := url.Parse(target)
	if err != nil || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" || strings.Contains(target, "#") || strings.ContainsAny(u.Host, "%\\ \t\r\n") {
		return nil, ErrBlocked
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.HasSuffix(host, ".") {
		return nil, ErrBlocked
	}
	if _, err := netip.ParseAddr(host); err != nil && !validDNSHost(host) {
		return nil, ErrBlocked
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, ErrBlocked
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return nil, ErrBlocked
		}
	}
	return u, nil
}

// Special-use ranges are denied by default, including metadata and transition networks.
var specialNetworks = func() []netip.Prefix {
	var result []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "::/96", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20", "fc00::/7", "fe80::/10", "ff00::/8"} {
		result = append(result, netip.MustParsePrefix(s))
	}
	return result
}()

// Transition and translation ranges can conceal an otherwise forbidden IPv4 target.
// Explicit trust never enables these ranges or cloud metadata service endpoints.
var alwaysDeniedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

func (p *Policy) allowedAddress(host string, addr netip.Addr) (bool, bool) {
	addr = addr.Unmap()
	// These destinations remain forbidden even when a broad allowlist is configured.
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || (addr == netip.MustParseAddr("100.100.100.200") || addr == netip.MustParseAddr("168.63.129.16") || addr == netip.MustParseAddr("fd00:ec2::254")) {
		return false, false
	}
	for _, prefix := range alwaysDeniedNetworks {
		if prefix.Contains(addr) {
			return false, false
		}
	}
	if addr.Is6() && netip.MustParsePrefix("::/96").Contains(addr) && !addr.IsLoopback() {
		return false, false
	}
	explicit := p.hosts[host]
	for _, prefix := range p.networks {
		if prefix.Contains(addr) {
			explicit = true
		}
	}
	if explicit {
		return true, true
	}
	if !addr.IsGlobalUnicast() || (addr.Is6() && !netip.MustParsePrefix("2000::/3").Contains(addr)) {
		return false, false
	}
	for _, prefix := range specialNetworks {
		if prefix.Contains(addr) {
			return false, false
		}
	}
	return true, false
}
func (p *Policy) resolve(ctx context.Context, host string) ([]netip.Addr, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if p == nil {
		return nil, false, ErrBlocked
	}
	host = strings.ToLower(host)
	var addresses []net.IPAddr
	if addr, err := netip.ParseAddr(host); err == nil {
		addresses = []net.IPAddr{{IP: net.IP(addr.AsSlice())}}
	} else {
		var err error
		lookup := p.lookup
		if lookup == nil {
			lookup = net.DefaultResolver.LookupIPAddr
		}
		addresses, err = lookup(ctx, host)
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			return nil, false, ErrDNSResolution
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if len(addresses) == 0 {
		return nil, false, ErrNoAddresses
	}
	result := make([]netip.Addr, 0, len(addresses))
	allExplicit := true
	for _, address := range addresses {
		ip, ok := netip.AddrFromSlice(address.IP)
		if !ok || address.Zone != "" {
			return nil, false, ErrBlocked
		}
		allowed, explicit := p.allowedAddress(host, ip)
		if !allowed {
			return nil, false, ErrBlocked
		}
		allExplicit = allExplicit && explicit
		result = append(result, ip.Unmap())
	}
	return result, allExplicit, nil
}

// Validate checks a URL and all addresses it currently resolves to. DNS changes
// after this call are possible: use Client for the actual request as well.
// A nil Policy fails closed. A canceled context returns its context error.
func (p *Policy) Validate(ctx context.Context, target string) error {
	if p == nil {
		return ErrBlocked
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	u, err := parseURL(target)
	if err != nil {
		return err
	}
	_, explicit, err := p.resolve(ctx, u.Hostname())
	if err != nil {
		return err
	}
	if u.Scheme != "https" && !explicit {
		return ErrBlocked
	}
	return nil
}

func (p *Policy) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrBlocked
	}
	ips, explicit, err := p.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	if required, _ := ctx.Value(requireExplicitTrustKey{}).(bool); required && !explicit {
		return nil, ErrBlocked
	}
	for _, ip := range ips {
		dial := p.dial
		if dial == nil {
			dial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		}
		conn, dialErr := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		err = dialErr
	}
	return nil, err
}
