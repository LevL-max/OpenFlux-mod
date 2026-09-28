//go:build volga

package yandex

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

type volgaV6AuthRoundTripper func(*http.Request) (*http.Response, error)

func (f volgaV6AuthRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func volgaV6AuthResponse(r *http.Request, status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func volgaV6TestBootstrap(now time.Time) volgaV6Bootstrap {
	return volgaV6Bootstrap{Schema: "openflux-volga-bootstrap-v1", Editor: "https://disk.yandex.ru/edit/test", Action: "https://volga.yandex.ru/session/main/auth/initial", Access: "secret-access", TTL: json.Number(strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10)), Resource: "test-resource", DocID: "test-document"}
}
func volgaV6ConfigHTML(b volgaV6Bootstrap) string {
	j, _ := json.Marshal(map[string]any{"officeActionData": map[string]any{"action_url": b.Action, "access_token": b.Access, "access_token_ttl": b.TTL, "resource_url": b.Resource}, "editorParams": map[string]any{"idDoc": b.DocID}})
	return "<script id=\"client-config\">\n" + string(j) + "\n</script>"
}

func TestVolgaV6AuthorizationRefreshConcurrentExpiry(t *testing.T) {
	now := time.Now()
	p := newVolgaV6AuthProvider("")
	p.now = func() time.Time { return now }
	var fetch atomic.Int32
	p.roundTripper = volgaV6AuthRoundTripper(func(r *http.Request) (*http.Response, error) {
		fetch.Add(1)
		return volgaV6AuthResponse(r, 200, volgaV6ConfigHTML(volgaV6TestBootstrap(now)), nil), nil
	})
	doc := "https://disk.yandex.ru/i/test"
	check := func() {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				b, e := p.bootstrap(context.Background(), doc, false)
				if e != nil || b.Access == "" {
					t.Errorf("bootstrap failed: %v", e)
				}
			}()
		}
		wg.Wait()
	}
	check()
	if fetch.Load() != 1 {
		t.Fatalf("concurrent refresh requests: %d", fetch.Load())
	}
	now = now.Add(2 * time.Hour)
	check()
	if fetch.Load() != 2 {
		t.Fatalf("expiry did not refresh once: %d", fetch.Load())
	}
}

func TestVolgaV6AuthorizationBlockedUntilCookiesChange(t *testing.T) {
	doc := "https://disk.yandex.ru/i/test"
	path := filepath.Join(t.TempDir(), "cookies.json")
	p := newVolgaV6AuthProvider(path)
	var calls atomic.Int32
	p.roundTripper = volgaV6AuthRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Header.Get("Cookie") != "Session_id=new" {
			return volgaV6AuthResponse(r, 302, "", http.Header{"Location": {"/showcaptcha?secret=hidden"}}), nil
		}
		return volgaV6AuthResponse(r, 200, volgaV6ConfigHTML(volgaV6TestBootstrap(time.Now())), http.Header{"Set-Cookie": {"updated=value; Path=/; Secure"}}), nil
	})
	for i := 0; i < 3; i++ {
		_, e := p.bootstrap(context.Background(), doc, false)
		if !errors.Is(e, ErrCaptchaRequired) {
			t.Fatalf("expected CAPTCHA latch: %v", e)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("CAPTCHA retried %d times", calls.Load())
	}
	s, _ := transport.NewCookieStore(path)
	if e := s.Save(doc, map[string]string{"Session_id": "new"}); e != nil {
		t.Fatal(e)
	}
	if _, e := p.bootstrap(context.Background(), doc, false); e != nil {
		t.Fatal(e)
	}
	reloaded, _ := transport.NewCookieStore(path)
	if reloaded.Load(doc)["updated"] != "value" {
		t.Fatal("server cookie not persisted")
	}
	if _, e := p.bootstrap(context.Background(), doc, false); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected extra refresh: %d", calls.Load())
	}
}

func TestVolgaV6AuthorizationIndependentSessionsAndNoCookieLeak(t *testing.T) {
	p := newVolgaV6AuthProvider("")
	b := volgaV6TestBootstrap(time.Now())
	var initial atomic.Int32
	var configurations atomic.Int32
	seenTokens := make(map[string]bool)
	p.roundTripper = volgaV6AuthRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "disk.yandex.ru" {
			fresh := b
			fresh.Access = fmt.Sprintf("fresh-%d", configurations.Add(1))
			return volgaV6AuthResponse(r, 200, volgaV6ConfigHTML(fresh), nil), nil
		}
		if r.Method == "POST" {
			if r.Header.Get("Cookie") != "" {
				t.Error("session cookies reused for another lane")
			}
			if r.Header.Get("Referer") != b.Editor {
				t.Error("missing editor referer")
			}
			i := initial.Add(1)
			_ = r.ParseForm()
			access := r.Form.Get("access_token")
			if seenTokens[access] {
				t.Error("bootstrap reused across independent sessions")
			}
			seenTokens[access] = true
			if r.Form.Get("access_token_ttl") != b.TTL.String() {
				t.Error("TTL precision lost")
			}
			j, _ := json.Marshal(map[string]any{"sessionId": fmt.Sprint(i), "xiva": map[string]any{"user": i, "sign": "secret-sign", "ts": "123"}})
			q := url.Values{"token": {"secret-token"}, "request-path": {fmt.Sprint(i)}, "json": {string(j)}}
			return volgaV6AuthResponse(r, 302, "", http.Header{"Location": {"https://volga.yandex.ru/document/test?" + q.Encode()}, "Set-Cookie": {fmt.Sprintf("lane=%d; Path=/; Secure", i)}}), nil
		}
		if r.Header.Get("Cookie") == "" {
			t.Error("activation cookie missing")
		}
		return volgaV6AuthResponse(r, 200, "ok", nil), nil
	})
	a, e := p.authorize(context.Background(), b.Editor)
	if e != nil {
		t.Fatal(e)
	}
	c, e := p.authorize(context.Background(), b.Editor)
	if e != nil {
		t.Fatal(e)
	}
	if a.Session.Jar == c.Session.Jar || a.SessionID == c.SessionID || a.RequestPath == c.RequestPath {
		t.Fatal("lanes share identity")
	}
	if len(a.Cookies) != 1 || a.Cookies[0].Value != "1" || c.Cookies[0].Value != "2" {
		t.Fatal("cookie contamination")
	}
	if configurations.Load() != 2 {
		t.Fatal("each physical session needs fresh configuration")
	}
}

func TestVolgaV6AuthorizationRejectExpiredAndUntrustedEndpoints(t *testing.T) {
	p := newVolgaV6AuthProvider("")
	calls := 0
	p.roundTripper = volgaV6AuthRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("secret-url-and-cookie")
	})
	b := volgaV6TestBootstrap(time.Now())
	b.TTL = "1"
	if _, e := p.authorizeBootstrap(context.Background(), b); e == nil {
		t.Fatal("expired token accepted")
	}
	b = volgaV6TestBootstrap(time.Now())
	b.Action = "https://attacker.invalid/session/main/auth/initial"
	if _, e := p.authorizeBootstrap(context.Background(), b); e == nil {
		t.Fatal("foreign endpoint accepted")
	}
	if calls != 0 {
		t.Fatal("credentials sent before validation")
	}
	b = volgaV6TestBootstrap(time.Now())
	_, e := p.authorizeBootstrap(context.Background(), b)
	if e == nil || strings.Contains(e.Error(), "secret") {
		t.Fatal("underlying URL error leaked")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := p.document(b.Editor)
	d.gate <- struct{}{}
	_, e = p.bootstrap(ctx, b.Editor, false)
	<-d.gate
	if !errors.Is(e, context.Canceled) {
		t.Fatal("waiting refresh ignores cancellation")
	}
}

func TestVolgaV6AuthorizationBlinkUsesActualOrigin(t *testing.T) {
	p := newVolgaV6AuthProvider("")
	p.roundTripper = volgaV6AuthRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "disk.yandex.ru" || r.Header.Get("Origin") != "https://disk.yandex.ru" {
			t.Error("challenge sent to wrong origin")
		}
		if r.URL.RawQuery != "a=1&b=2" {
			t.Error("HTML form action not decoded")
		}
		if r.Header.Get("Cookie") != "challenge=value" {
			t.Error("challenge lost scoped cookie")
		}
		return volgaV6AuthResponse(r, 302, "", http.Header{"Location": {"/edit/test"}}), nil
	})
	c := p.client(map[string]string{"challenge": "value"})
	ssr := base64.StdEncoding.EncodeToString([]byte(`{"uniqueKey":"test","pow":{"complexity":0,"prefix":"00"}}`))
	body := []byte(`<script>window.__SSR_DATA__ = JSON.parse(atob("` + ssr + `"))</script><form id="tmgrdfrend-form" action="/check?a=1&amp;b=2">`)
	if e := volgaV6SolveBlink(context.Background(), c, "https://disk.yandex.ru/showcaptchafast", body); e != nil {
		t.Fatal(e)
	}
}

func TestVolgaV6AuthorizationDecodesCompressedHTML(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip" {
			t.Error("HTTP transport not managing compression")
		}
		w.Header().Set("Content-Encoding", "gzip")
		z := gzip.NewWriter(w)
		_, _ = z.Write([]byte("configuration"))
		_ = z.Close()
	}))
	defer s.Close()
	c := newVolgaV6AuthProvider("").client(nil)
	defer c.CloseIdleConnections()
	_, b, e := volgaV6AuthRequest(context.Background(), c, "GET", s.URL, nil)
	if e != nil || string(b) != "configuration" {
		t.Fatalf("compressed HTML not decoded: %v", e)
	}
}

func TestVolgaV6BrowserProfileStaysOnEditorHosts(t *testing.T) {
	p := newVolgaV6AuthProvider("")
	p.browserHeaders = http.Header{"User-Agent": {"browser-agent"}, "Accept-Language": {"en-GB"}, "Cookie": {"must-not-inject"}, "Authorization": {"must-not-inject"}}
	p.roundTripper = volgaV6AuthRoundTripper(func(r *http.Request) (*http.Response, error) {
		want := volgaUserAgent
		if r.URL.Host == "docs.yandex.ru" {
			want = "browser-agent"
		}
		if r.Header.Get("User-Agent") != want {
			t.Error("incorrect scoped User-Agent")
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("profile injected credentials")
		}
		return volgaV6AuthResponse(r, 200, "ok", nil), nil
	})
	c := p.client(nil)
	for _, raw := range []string{"https://docs.yandex.ru/edit/test", "https://volga.yandex.ru/document/test"} {
		if _, _, e := volgaV6AuthRequest(context.Background(), c, "GET", raw, nil); e != nil {
			t.Fatal(e)
		}
	}
}

func volgaV6DiagnosticAuth(t *testing.T, docs []string) *volgaV6AuthProvider {
	t.Helper()
	p := newVolgaV6AuthProvider(os.Getenv("OPENFLUX_V6_COOKIE_STORE"))
	if path := os.Getenv("OPENFLUX_V6_BROWSER_PROFILE"); path != "" {
		data, e := os.ReadFile(path)
		if e != nil {
			t.Fatal("cannot read browser profile")
		}
		var profile struct {
			Headers map[string]string `json:"headers"`
			Editors map[string]string `json:"editors"`
		}
		if json.Unmarshal(data, &profile) != nil {
			t.Fatal("invalid browser profile")
		}
		p.browserHeaders = make(http.Header)
		for k, v := range profile.Headers {
			p.browserHeaders.Set(k, v)
		}
		for _, doc := range docs {
			if editor := profile.Editors[doc]; editor != "" {
				if !volgaV6AllowedURL(editor, true) {
					t.Fatal("invalid editor URL")
				}
				p.document(doc).seed.Editor = editor
			}
		}
	}
	files := strings.Split(os.Getenv("OPENFLUX_V6_BOOTSTRAP_FILES"), ",")
	if len(files) == 1 && files[0] == "" {
		return p
	}
	if len(files) != 2 || len(docs) < 2 {
		t.Fatal("two bootstrap files required")
	}
	for i, path := range files {
		if e := p.seed(docs[i], path); e != nil {
			t.Fatal(e)
		}
	}
	return p
}

// A short network check with secret-free output; disabled in normal tests.
func TestVolgaV6AuthorizationProbe(t *testing.T) {
	if os.Getenv("OPENFLUX_V6_AUTH_PROBE") != "1" {
		t.Skip("explicit isolated auth check only")
	}
	docs := strings.Split(os.Getenv("OPENFLUX_V6_POOL"), ",")
	if len(docs) != 4 {
		t.Fatal("four pool entries required")
	}
	p := volgaV6DiagnosticAuth(t, docs)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for i, doc := range docs[:2] {
		b, e := p.bootstrap(ctx, doc, true)
		if e != nil {
			t.Fatalf("document %d refresh: %v", i, e)
		}
		fmt.Printf("V6AUTH document=%d fresh_bootstrap=true expires=%s\n", i, b.expires().UTC().Format(time.RFC3339))
	}
	if _, e := p.bootstrap(ctx, docs[0], true); e != nil {
		t.Fatalf("repeat configuration refresh: %v", e)
	}
	fmt.Println("V6AUTH repeat_configuration_refresh=true")
	cfg := defaultVolgaV6YandexConfig()
	cfg.authorize = p.authorize
	cfg.RelayEnvelope = "minimal"
	pool, e := livePoolFactory(docs, cfg)(1, func(volgaV6WireFrame) {})
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Stop()
	if e = pool.Start(ctx); e != nil {
		t.Fatal(e)
	}
	fmt.Println("V6AUTH four_sessions_connected=true")
	for round := 0; round < 2; round++ {
		for lane, c := range pool.(*liveCarrierPool).lanes {
			if e := c.SendVolgaV6(volgaV6WireFrame{Kind: volgaV6FrameAck, Ack: volgaV6Ack{Session: 1}}); e != nil {
				t.Fatalf("lane %d relay check: %v", lane, e)
			}
		}
		fmt.Printf("V6AUTH relay_round=%d all_four_lanes_accepted=true\n", round)
		if round == 0 {
			time.Sleep(2 * time.Second)
		}
	}
}
