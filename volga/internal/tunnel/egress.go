package tunnel

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

var ErrTargetDenied = errors.New("egress target denied")

type ipResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// TargetDialer is immutable after construction. Public mode validates every DNS
// answer and dials the numeric address from that same lookup, preventing a
// second lookup from rebinding an allowed hostname to an internal address.
type TargetDialer struct {
	mode       string
	allowed    map[string]bool
	denied     []netip.Prefix
	resolver   ipResolver
	interfaces func() ([]netip.Prefix, error)
	dial       func(context.Context, string, string) (net.Conn, error)
}

func NewTargetDialer(mode string, allowed, deniedCIDRs []string) (*TargetDialer, error) {
	if mode == "" {
		mode = "allowlist"
	}
	if mode != "allowlist" && mode != "public" {
		return nil, errors.New("egress_policy must be allowlist or public")
	}
	d := &TargetDialer{mode: mode, allowed: make(map[string]bool), resolver: net.DefaultResolver, interfaces: interfacePrefixes}
	d.dial = (&net.Dialer{Timeout: SetupTimeout}).DialContext
	for _, target := range allowed {
		canonical, host, _, err := canonicalTarget(target)
		if err != nil {
			return nil, err
		}
		if mode == "public" {
			if _, err := netip.ParseAddr(host); err != nil {
				return nil, errors.New("public-mode allowed_targets exceptions require literal IP:port")
			}
		}
		d.allowed[canonical] = true
	}
	for _, raw := range deniedCIDRs {
		p, err := netip.ParsePrefix(raw)
		if err != nil || p.Addr().Is4In6() {
			return nil, errors.New("invalid denied_cidrs prefix")
		}
		d.denied = append(d.denied, p.Masked())
	}
	return d, nil
}

func canonicalTarget(target string) (string, string, string, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil || host == "" || len(host) > 253 || strings.ContainsAny(host, "%\x00\r\n\t /\\") {
		return "", "", "", ErrTargetDenied
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", "", "", ErrTargetDenied
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	} else {
		host = strings.ToLower(strings.TrimSuffix(host, "."))
	}
	if host == "" {
		return "", "", "", ErrTargetDenied
	}
	port = strconv.Itoa(n)
	return net.JoinHostPort(host, port), host, port, nil
}

func interfacePrefixes() ([]netip.Prefix, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	result := make([]netip.Prefix, 0, len(addrs))
	for _, a := range addrs {
		p, err := netip.ParsePrefix(a.String())
		if err != nil {
			return nil, err
		}
		result = append(result, p.Masked())
	}
	return result, nil
}

// Conservative special-use exclusions based on the IANA IPv4/IPv6 Special-
// Purpose registries (reviewed 2026-09-28). Transition prefixes are denied too:
// translation/6to4/Teredo must not turn a public-looking IPv6 literal into LAN
// access. Some special anycast ranges are intentionally excluded.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/3"),
	netip.MustParsePrefix("168.63.129.16/32"), // Azure host/platform virtual address.
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

func publicIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() {
		return false
	}
	// Only native IPv6 global-unicast space; excludes ULA, NAT64, discard and
	// future/special allocations without interpreting embedded IPv4 addresses.
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

func (d *TargetDialer) DialContext(ctx context.Context, target string) (net.Conn, error) {
	canonical, host, port, err := canonicalTarget(target)
	if err != nil {
		return nil, err
	}
	exception := d.allowed[canonical]
	if d.mode == "allowlist" && !exception {
		return nil, ErrTargetDenied
	}
	ctx, cancel := context.WithTimeout(ctx, SetupTimeout)
	defer cancel()
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = d.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 || len(ips) > 32 {
		return nil, ErrTargetDenied
	}
	var local []netip.Prefix
	if d.mode == "public" && !exception {
		local, err = d.interfaces()
		if err != nil {
			return nil, ErrTargetDenied
		} // fail closed
	}
	for _, raw := range ips {
		ip := raw.Unmap()
		if !ip.IsValid() || ip.Zone() != "" {
			return nil, ErrTargetDenied
		}
		// Explicit deny CIDRs override even test exceptions.
		for _, p := range d.denied {
			if p.Contains(ip) {
				return nil, ErrTargetDenied
			}
		}
		if d.mode == "public" && !exception {
			if !publicIP(ip) {
				return nil, ErrTargetDenied
			}
			for _, p := range local {
				if p.Contains(ip) {
					return nil, ErrTargetDenied
				}
			}
		}
	}
	return d.dialPinned(ctx, ips, port)
}

// At most two racers, one per address family. All addresses have already passed
// policy; no DNS lookup happens here. A broken IPv6 route cannot consume the
// entire setup budget before a reachable IPv4 address is attempted (or vice versa).
func (d *TargetDialer) dialPinned(ctx context.Context, ips []netip.Addr, port string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var primary, fallback []netip.Addr
	firstIs4 := ips[0].Unmap().Is4()
	for _, ip := range ips {
		if ip.Unmap().Is4() == firstIs4 {
			primary = append(primary, ip)
		} else {
			fallback = append(fallback, ip)
		}
	}
	type result struct {
		conn net.Conn
		err  error
	}
	results := make(chan result)
	run := func(addresses []netip.Addr) {
		var r result
		for i, ip := range addresses {
			deadline, _ := ctx.Deadline()
			attempt, done := context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(addresses)-i))
			r.conn, r.err = d.dial(attempt, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
			done()
			if r.err == nil || ctx.Err() != nil {
				break
			}
		}
		select {
		case results <- r:
		case <-ctx.Done():
			if r.conn != nil {
				r.conn.Close()
			}
		}
	}
	go run(primary)
	pending := 1
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	startFallback := func() {
		if len(fallback) != 0 {
			go run(fallback)
			fallback = nil
			pending++
		}
	}
	var lastErr error
	for pending > 0 {
		select {
		case r := <-results:
			pending--
			if r.err == nil {
				return r.conn, nil
			}
			lastErr = r.err
			startFallback()
		case <-timer.C:
			startFallback()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}
