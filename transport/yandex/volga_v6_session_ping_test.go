//go:build volga

package yandex

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newPingTestCarrier(t *testing.T, rt volgaV6LocalRoundTripper) *volgaV6YandexCarrier {
	t.Helper()
	c := newVolgaV6YandexCarrier(1, "https://disk.yandex.ru/i/test", volgaV6YandexConfig{}, func(volgaV6WireFrame) {})
	c.auth = &volgaAuth{Token: "session-token", RequestPath: "rp-1", Cookies: []*http.Cookie{{Name: "volga", Value: "1"}}}
	c.http = &http.Client{Transport: rt}
	return c
}

func TestVolgaV6SessionPingMirrorsTheEditor(t *testing.T) {
	status := http.StatusNoContent
	var got *http.Request
	var body string
	c := newPingTestCarrier(t, func(req *http.Request) (*http.Response, error) {
		got = req
		if req.Body != nil {
			b, _ := io.ReadAll(req.Body)
			body = string(b)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: req}, nil
	})
	defer c.Stop()

	if err := c.sendSessionPing(); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || got.URL.String() != "https://volga.yandex.ru/session/main/rp-1/ping" {
		t.Fatalf("ping sent as %s %s", got.Method, got.URL)
	}
	if got.ContentLength != 0 || body != "" || got.Header.Get("Content-Type") != "" {
		t.Fatalf("ping carries a body: length=%d body=%q type=%q", got.ContentLength, body, got.Header.Get("Content-Type"))
	}
	for name, want := range map[string]string{"Authorization": "Bearer session-token", "Origin": "https://volga.yandex.ru", "Sec-Fetch-Site": "same-origin", "Cookie": "volga=1"} {
		if v := got.Header.Get(name); v != want {
			t.Fatalf("%s=%q, want %q", name, v, want)
		}
	}
	if s := c.Snapshot(); s.SessionPings != 1 || s.SessionPingFailures != 0 || s.SessionPingRejected != 0 {
		t.Fatalf("after 204: %+v", s)
	}

	// A refused ping means the editor session is gone.
	status = http.StatusUnauthorized
	if err := c.sendSessionPing(); err == nil {
		t.Fatal("401 reported as success")
	}
	// Other failures are counted but do not claim the session is gone.
	status = http.StatusInternalServerError
	if err := c.sendSessionPing(); err == nil {
		t.Fatal("500 reported as success")
	}
	if s := c.Snapshot(); s.SessionPings != 1 || s.SessionPingFailures != 2 || s.SessionPingRejected != 1 {
		t.Fatalf("after 401 and 500: pings=%d failures=%d rejected=%d", s.SessionPings, s.SessionPingFailures, s.SessionPingRejected)
	}
	if h := c.VolgaV6PhysicalHealth(time.Now()); h.SessionPingRejected != 1 || h.SessionPings != 1 {
		t.Fatalf("health does not carry the ping counters: %+v", h)
	}
}

func TestVolgaV6SessionPingLoopEndsWithTheCarrier(t *testing.T) {
	var pings atomic.Int64
	c := newPingTestCarrier(t, func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/ping") {
			pings.Add(1)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: req}, nil
	})
	go c.sessionPingLoop(5 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for pings.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if pings.Load() < 2 {
		t.Fatalf("%d pings in 2 s at a 5 ms interval", pings.Load())
	}
	c.Stop()
	time.Sleep(20 * time.Millisecond) // let an in-flight ping finish
	stopped := pings.Load()
	time.Sleep(50 * time.Millisecond)
	if n := pings.Load(); n != stopped {
		t.Fatalf("pings continued after Stop: %d -> %d", stopped, n)
	}
}

func TestVolgaV6PushReadDeadlineOutlastsTheServerPing(t *testing.T) {
	// push.yandex.ru pings an idle socket every 60 s; the deadline must not race it.
	if got := defaultVolgaV6YandexConfig().WSReadTimeout; got < 2*time.Minute {
		t.Fatalf("websocket read deadline %v races the 60 s server ping", got)
	}
}

func TestVolgaV6SessionPingDefaultsAndDisable(t *testing.T) {
	if got := newVolgaV6YandexCarrier(1, "https://disk.yandex.ru/i/test", volgaV6YandexConfig{}, nil).config.SessionPingInterval; got != time.Minute {
		t.Fatalf("default ping interval %v, want 1m", got)
	}
	if got := newVolgaV6YandexCarrier(1, "https://disk.yandex.ru/i/test", volgaV6YandexConfig{SessionPingInterval: -1}, nil).config.SessionPingInterval; got >= 0 {
		t.Fatalf("a negative interval must stay disabled, got %v", got)
	}
}
