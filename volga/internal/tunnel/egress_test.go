package tunnel

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func TestPinnedFallbackSurvivesBlackholedFamily(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		d, _ := testPolicy(t, nil, nil)
		ips := []netip.Addr{netip.MustParseAddr("2606:4700:4700::1111"), netip.MustParseAddr("8.8.8.8")}
		if reverse {
			ips[0], ips[1] = ips[1], ips[0]
		}
		lookups := 0
		d.resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			lookups++
			return ips, nil
		})
		cancelled := make(chan struct{})
		d.dial = func(ctx context.Context, _, address string) (net.Conn, error) {
			if address == net.JoinHostPort(ips[0].String(), "443") {
				<-ctx.Done()
				close(cancelled)
				return nil, ctx.Err()
			}
			a, b := net.Pipe()
			b.Close()
			return a, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c, err := d.DialContext(ctx, "test.example:443")
		cancel()
		if err != nil {
			t.Fatal("working alternate family was starved", err)
		}
		c.Close()
		if lookups != 1 {
			t.Fatal("fallback repeated DNS lookup")
		}
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatal("losing dial was not cancelled")
		}
	}
}

func (f resolverFunc) LookupNetIP(c context.Context, n, h string) ([]netip.Addr, error) {
	return f(c, n, h)
}

func TestPublicAddressClasses(t *testing.T) {
	for _, address := range []string{"0.0.0.0", "0.1.2.3", "10.2.3.4", "100.100.100.200", "127.1.2.3", "169.254.169.254", "172.31.0.1", "192.168.2.1", "192.0.0.8", "192.0.2.1", "192.88.99.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255", "168.63.129.16", "::", "::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "fc00::1", "fd00:ec2::254", "fe80::1", "fe80::1%eth0", "ff02::1", "64:ff9b::a00:1", "64:ff9b:1::1", "100::1", "2001::1", "2001:db8::1", "2002:7f00:1::1", "3fff::1", "5f00::1"} {
		t.Run(address, func(t *testing.T) {
			if publicIP(netip.MustParseAddr(address)) {
				t.Fatal("accepted non-public address")
			}
		})
	}
	for _, address := range []string{"1.1.1.1", "8.8.8.8", "93.184.215.14", "::ffff:8.8.8.8", "2606:4700:4700::1111", "2001:4860:4860::8888"} {
		if !publicIP(netip.MustParseAddr(address)) {
			t.Fatalf("denied public address %s", address)
		}
	}
}

func testPolicy(t *testing.T, allowed, deny []string) (*TargetDialer, *[]string) {
	t.Helper()
	d, err := NewTargetDialer("public", allowed, deny)
	if err != nil {
		t.Fatal(err)
	}
	d.interfaces = func() ([]netip.Prefix, error) {
		return []netip.Prefix{netip.MustParsePrefix("9.9.9.0/24"), netip.MustParsePrefix("2606:4700:1234::/64")}, nil
	}
	called := []string{}
	d.dial = func(_ context.Context, network, address string) (net.Conn, error) {
		called = append(called, address)
		if network != "tcp" {
			t.Fatal(network)
		}
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}
	return d, &called
}

func TestDNSPinnedAndMixedAnswersDenied(t *testing.T) {
	for _, addresses := range [][]string{{"8.8.8.8"}, {"8.8.8.8", "10.0.0.1"}, {"2606:4700:4700::1111", "::ffff:127.0.0.1"}, {"9.9.9.9"}, {"2606:4700:1234::2"}, {}} {
		d, called := testPolicy(t, nil, nil)
		lookups := 0
		d.resolver = resolverFunc(func(_ context.Context, network, host string) ([]netip.Addr, error) {
			lookups++
			if lookups > 1 {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			if host != "test.example" || network != "ip" {
				t.Fatal(host, network)
			}
			var result []netip.Addr
			for _, a := range addresses {
				result = append(result, netip.MustParseAddr(a))
			}
			return result, nil
		})
		c, err := d.DialContext(context.Background(), "Test.Example.:443")
		if len(addresses) == 1 && addresses[0] == "8.8.8.8" {
			if err != nil {
				t.Fatal(err)
			}
			c.Close()
			if len(*called) != 1 || (*called)[0] != "8.8.8.8:443" || lookups != 1 {
				t.Fatal("not pinned", *called, lookups)
			}
		} else if !errors.Is(err, ErrTargetDenied) || len(*called) != 0 {
			if c != nil {
				c.Close()
			}
			t.Fatal("unsafe answers reached dialer", addresses, err, *called)
		}
	}
}

func TestPublicExceptionsAndExplicitDeny(t *testing.T) {
	d, called := testPolicy(t, []string{"127.0.0.1:18765"}, []string{"8.8.8.0/24"})
	for _, target := range []string{"127.0.0.1:18766", "8.8.8.8:443", "9.9.9.9:443", "[::ffff:127.0.0.1]:22", "[fe80::1%eth0]:80"} {
		if c, err := d.DialContext(context.Background(), target); !errors.Is(err, ErrTargetDenied) {
			if c != nil {
				c.Close()
			}
			t.Fatal(target, err)
		}
	}
	if len(*called) != 0 {
		t.Fatal(*called)
	}
	c, err := d.DialContext(context.Background(), "127.0.0.1:18765")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if len(*called) != 1 || (*called)[0] != "127.0.0.1:18765" {
		t.Fatal(*called)
	}
	d, called = testPolicy(t, []string{"127.0.0.1:18765"}, []string{"127.0.0.0/8"})
	if _, err = d.DialContext(context.Background(), "127.0.0.1:18765"); !errors.Is(err, ErrTargetDenied) || len(*called) != 0 {
		t.Fatal("deny must override exception")
	}
}

func TestEgressFailsClosedAndFallbackStaysPinned(t *testing.T) {
	d, called := testPolicy(t, nil, nil)
	d.interfaces = func() ([]netip.Prefix, error) { return nil, errors.New("interfaces unavailable") }
	if _, err := d.DialContext(context.Background(), "8.8.8.8:443"); !errors.Is(err, ErrTargetDenied) || len(*called) != 0 {
		t.Fatal("interface failure did not fail closed")
	}
	d, called = testPolicy(t, nil, nil)
	d.resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")}, nil
	})
	okDial := d.dial
	d.dial = func(ctx context.Context, n, a string) (net.Conn, error) {
		if a == "8.8.8.8:443" {
			return nil, errors.New("unreachable")
		}
		return okDial(ctx, n, a)
	}
	c, err := d.DialContext(context.Background(), "test.example:443")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if len(*called) != 1 || (*called)[0] != "1.1.1.1:443" {
		t.Fatal(*called)
	}
}

func TestEgressConfigAndAllowlistCompatibility(t *testing.T) {
	for _, tc := range []struct {
		mode            string
		allowed, denied []string
	}{
		{"anything", nil, nil}, {"public", []string{"origin.example:80"}, nil}, {"public", []string{"127.0.0.1:0"}, nil},
		{"public", nil, []string{"bad"}}, {"public", nil, []string{"::ffff:10.0.0.0/104"}},
	} {
		if _, err := NewTargetDialer(tc.mode, tc.allowed, tc.denied); err == nil {
			t.Fatal("invalid config accepted", tc)
		}
	}
	d, err := NewTargetDialer("", []string{"origin.example:80"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	d.dial = func(_ context.Context, _, a string) (net.Conn, error) {
		if a != "127.0.0.1:80" {
			t.Fatal(a)
		}
		a1, b := net.Pipe()
		b.Close()
		return a1, nil
	}
	c, err := d.DialContext(context.Background(), "origin.example:80")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err = d.DialContext(context.Background(), "8.8.8.8:80"); !errors.Is(err, ErrTargetDenied) {
		t.Fatal(err)
	}
}
