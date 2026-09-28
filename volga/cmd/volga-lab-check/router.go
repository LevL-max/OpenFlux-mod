package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var errSOCKSPolicyDenied = errors.New("SOCKS policy denied")

// Functional concurrency check only: no throughput target, rate calculation or
// tuning sweep. Open all 64 TCP connections before any payload is sent.
func routerCheck(ctx context.Context, proxy string) error {
	const count = 64
	const size = 128 << 10
	connections := make([]net.Conn, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := range connections {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := connect(proxy, "127.0.0.1:18765")
			if err == nil {
				c.SetDeadline(time.Now().Add(60 * time.Second))
				connections[i] = c
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	defer func() {
		for _, c := range connections {
			if c != nil {
				c.Close()
			}
		}
	}()
	for range count {
		if err := <-errs; err != nil {
			return fmt.Errorf("opening cohort: %w", err)
		}
	}
	event("router_connections_open", map[string]any{"simultaneous_connections": count})
	if c, err := connect(proxy, "127.0.0.1:18765"); err == nil {
		c.Close()
		return errors.New("65th SOCKS connection was admitted")
	}
	event("router_limit_rejection_pass", map[string]any{"limit": count})
	// Leave a bounded interval for independent resource sampling while all
	// connections are held open (the origin uses a 30-second header deadline).
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
	}
	for i, c := range connections {
		wg.Add(1)
		go func(i int, c net.Conn) {
			defer wg.Done()
			defer c.Close()
			direction, method := "download", "GET"
			var body io.Reader
			if i%2 == 1 {
				direction, method = "upload", "POST"
				body = payload(size)
			}
			req, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:18765/"+direction+"?bytes=131072", body)
			if err == nil {
				req.Close = true
				if body != nil {
					req.ContentLength = size
				}
				err = req.Write(c)
			}
			var got string
			if err == nil {
				var response *http.Response
				response, err = http.ReadResponse(bufio.NewReader(c), req)
				if err == nil {
					defer response.Body.Close()
					if response.StatusCode != 200 {
						err = fmt.Errorf("HTTP %d", response.StatusCode)
					} else if direction == "download" {
						h := sha256.New()
						var n int64
						n, err = io.Copy(h, io.LimitReader(response.Body, size+1))
						got = hex.EncodeToString(h.Sum(nil))
						if err == nil && n != size {
							err = errors.New("download size mismatch")
						}
					} else {
						var b []byte
						b, err = io.ReadAll(io.LimitReader(response.Body, 129))
						got = string(b)
					}
				}
			}
			if err == nil && got != digest(size) {
				err = errors.New("SHA-256 mismatch")
			}
			if err == nil {
				event("router_transfer", map[string]any{"index": i, "direction": direction, "bytes": size, "sha256": got, "integrity": true})
			}
			errs <- err
		}(i, c)
	}
	wg.Wait()
	for range count {
		if err := <-errs; err != nil {
			return err
		}
	}
	u, _ := url.Parse("socks5://" + proxy)
	ht := &http.Transport{Proxy: http.ProxyURL(u), DisableKeepAlives: true}
	defer ht.CloseIdleConnections()
	hc := &http.Client{Transport: ht, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// An HTTPS page exercises DNS and unlisted public egress through SOCKS.
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/", nil)
	response, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("public HTTPS: %w", err)
	}
	h := sha256.New()
	n, readErr := io.Copy(h, io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if readErr != nil || response.StatusCode != 200 || n == 0 || n >= 1<<20 {
		return errors.New("public HTTPS response failed")
	}
	event("router_public_https_pass", map[string]any{"host": "example.com", "status": response.StatusCode, "bytes": n, "sha256": hex.EncodeToString(h.Sum(nil)), "tls_verified": true})
	for _, target := range []string{"127.0.0.1:22", "169.254.169.254:80", "10.0.0.1:80", "[::ffff:127.0.0.1]:22"} {
		if c, err := connect(proxy, target); err == nil {
			c.Close()
			return fmt.Errorf("private target accepted: %s", target)
		} else if !errors.Is(err, errSOCKSPolicyDenied) {
			return fmt.Errorf("expected explicit policy denial for %s: %w", target, err)
		}
		event("router_private_denied", map[string]any{"target": target})
	}
	if err := lifecycle(proxy, hc); err != nil {
		return err
	}
	event("router_pass", map[string]any{"connections": count, "verified_bytes": count * size, "downloads": count / 2, "uploads": count / 2, "speed_benchmark": false})
	return nil
}
