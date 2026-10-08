package yandex

import (
	"context"
	"net"
	"testing"
	"time"
)

func acceptAndClose(t *testing.T, ln net.Listener) {
	t.Helper()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
}

// requireDualStackLocalhost skips unless "localhost" resolves to an IPv4 and
// an IPv6 loopback address, which the family-order tests need.
func requireDualStackLocalhost(t *testing.T) {
	t.Helper()
	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), "localhost")
	if err != nil {
		t.Skip("cannot resolve localhost:", err)
	}
	var v4, v6 bool
	for _, a := range addrs {
		if a.IP.To4() != nil {
			v4 = true
		} else {
			v6 = true
		}
	}
	if !v4 || !v6 {
		t.Skipf("localhost resolves to %v; both families are needed", addrs)
	}
}

func listenIPv6Loopback(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback:", err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return ln, port
}

// A host with both families is reached over IPv4, the family Yandex saw the
// cookies come from. A plain dual-stack dial prefers ::1 for localhost.
func TestIPv4FirstPrefersIPv4(t *testing.T) {
	requireDualStackLocalhost(t)
	ln6, port := listenIPv6Loopback(t)
	acceptAndClose(t, ln6)
	ln4, err := net.Listen("tcp4", "127.0.0.1:"+port)
	if err != nil {
		t.Skipf("port %s is taken on IPv4: %v", port, err)
	}
	acceptAndClose(t, ln4)

	conn, err := ipv4First(&net.Dialer{Timeout: 3 * time.Second})(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.RemoteAddr().(*net.TCPAddr).IP.To4() == nil {
		t.Fatalf("connected to %s, want the IPv4 address", conn.RemoteAddr())
	}
}

// IPv6 is only the fallback: nothing answers on IPv4, IPv6 does.
func TestIPv4FirstFallsBackToIPv6(t *testing.T) {
	requireDualStackLocalhost(t)
	ln6, port := listenIPv6Loopback(t)
	acceptAndClose(t, ln6)
	probe, err := net.Listen("tcp4", "127.0.0.1:"+port)
	if err != nil {
		t.Skipf("port %s is in use on IPv4: %v", port, err)
	}
	_ = probe.Close() // nothing listens on the IPv4 side

	conn, err := ipv4First(&net.Dialer{Timeout: 5 * time.Second})(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.RemoteAddr().(*net.TCPAddr).IP.To4() != nil {
		t.Fatalf("connected to %s, want the IPv6 fallback", conn.RemoteAddr())
	}
}

func TestIPv4FirstDialsLiteralAddresses(t *testing.T) {
	dial := ipv4First(&net.Dialer{Timeout: 3 * time.Second})
	ln4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("no IPv4 loopback:", err)
	}
	acceptAndClose(t, ln4)
	conn, err := dial(context.Background(), "tcp", ln4.Addr().String())
	if err != nil {
		t.Fatalf("IPv4 literal: %v", err)
	}
	_ = conn.Close()

	ln6, _ := listenIPv6Loopback(t)
	acceptAndClose(t, ln6)
	conn, err = dial(context.Background(), "tcp", ln6.Addr().String())
	if err != nil {
		t.Fatalf("IPv6 literal: %v", err)
	}
	_ = conn.Close()
}

// Every Legacy connection to Yandex goes through the IPv4-first dial: a plain
// http.Client{} or websocket.Dialer{} would quietly bring dual-stack back.
func TestLegacyDialsIPv4First(t *testing.T) {
	if yandexHTTPTransport.DialContext == nil {
		t.Fatal("the Legacy HTTP transport has no IPv4-first dial")
	}
	if yandexHTTPTransport.Proxy == nil || yandexHTTPTransport.TLSHandshakeTimeout == 0 {
		t.Fatal("the Legacy HTTP transport lost http.DefaultTransport's settings")
	}
	if docWSDialer().NetDialContext == nil {
		t.Fatal("the document WebSocket dialer has no IPv4-first dial")
	}
}
