package yandex

import (
	"context"
	"net"
	"net/http"
	"time"
)

// Yandex binds a passed check (the "spravka") and the session behind the
// cookies to the address it saw them from. The cookies come from a browser
// that reached Yandex over IPv4 and the tunnel exits are IPv4-only, so a node
// whose network gains an IPv6 route would show Yandex another address than
// its cookies were issued for and meet SmartCaptcha after every cookie handoff
// (upstream OpenFlux 0.4.1, commit 1e8d832). Every Yandex connection, Legacy
// and Volga, therefore tries IPv4 first and uses IPv6 only when IPv4 fails.

// ipv4First wraps d so that a "tcp" dial tries the host's IPv4 addresses
// first, with Go's own spreading of the timeout over them, and falls back to
// IPv6 only when none of them answers. Other networks pass through unchanged.
func ipv4First(d *net.Dialer) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" {
			return d.DialContext(ctx, network, address)
		}
		conn, err := d.DialContext(ctx, "tcp4", address)
		if err == nil || ctx.Err() != nil {
			return conn, err
		}
		if conn6, err6 := d.DialContext(ctx, "tcp6", address); err6 == nil {
			return conn6, nil
		}
		return nil, err
	}
}

// yandexHTTPTransport serves the Legacy document fetch and captcha requests:
// http.DefaultTransport's settings with the IPv4-first dial.
var yandexHTTPTransport = newYandexHTTPTransport()

func newYandexHTTPTransport() *http.Transport {
	dial := ipv4First(&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second})
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		t := dt.Clone()
		t.DialContext = dial
		return t
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}
