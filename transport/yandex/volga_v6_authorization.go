//go:build volga

package yandex

// Experimental V6 authorization. Uses the v4.0.5 cookie-store format, while
// keeping editor cookies separate from each physical Volga session's jar.
// No production CLI selects this provider yet.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"universal-bypass-tool/transport"
)

type volgaV6Bootstrap struct {
	Schema   string      `json:"schema"`
	Editor   string      `json:"editor_url"`
	Action   string      `json:"action_url"`
	Access   string      `json:"access_token"`
	TTL      json.Number `json:"access_token_ttl"`
	Resource string      `json:"resource_url"`
	DocID    string      `json:"document_id"`
}

func (b volgaV6Bootstrap) expires() time.Time {
	n, _ := strconv.ParseInt(b.TTL.String(), 10, 64)
	return time.UnixMilli(n)
}

func volgaV6AllowedURL(raw string, editor bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	if editor {
		return u.Hostname() == "disk.yandex.ru" || u.Hostname() == "docs.yandex.ru"
	}
	return u.Hostname() == "volga.yandex.ru"
}

func (b volgaV6Bootstrap) validate() error {
	u, _ := url.Parse(b.Action)
	if b.Schema != "openflux-volga-bootstrap-v1" || !volgaV6AllowedURL(b.Editor, true) || !volgaV6AllowedURL(b.Action, false) || u.Path != "/session/main/auth/initial" || b.Access == "" || b.Resource == "" || b.DocID == "" {
		return errors.New("volga bootstrap: invalid fields or endpoint")
	}
	if _, err := strconv.ParseInt(b.TTL.String(), 10, 64); err != nil {
		return errors.New("volga bootstrap: invalid expiry")
	}
	return nil
}

type volgaV6AuthDocument struct {
	gate        chan struct{}
	seed        volgaV6Bootstrap
	blocked     error
	blockedFlag atomic.Bool
	cookies     string
}

type volgaV6AuthProvider struct {
	mu             sync.Mutex
	docs           map[string]*volgaV6AuthDocument
	cookiePath     string
	browserHeaders http.Header
	// Tests replace HTTP with a deterministic in-memory server; URLs still
	// undergo the same allow-list checks before any credentials are attached.
	roundTripper http.RoundTripper
	now          func() time.Time
}

func newVolgaV6AuthProvider(cookiePath string) *volgaV6AuthProvider {
	return &volgaV6AuthProvider{docs: make(map[string]*volgaV6AuthDocument), cookiePath: cookiePath, now: time.Now}
}

func (p *volgaV6AuthProvider) anyBlocked() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.docs {
		if d.blockedFlag.Load() {
			return true
		}
	}
	return false
}

// blockedCookiesChanged reports that a document latched on a CAPTCHA or login
// wall now has other cookies in the store, so a renewal can succeed without
// waiting for its spaced retry. A document whose bootstrap is running is
// skipped: that bootstrap compares the cookies itself.
func (p *volgaV6AuthProvider) blockedCookiesChanged() bool {
	p.mu.Lock()
	docs := make(map[string]*volgaV6AuthDocument, len(p.docs))
	for doc, d := range p.docs {
		docs[doc] = d
	}
	p.mu.Unlock()
	for doc, d := range docs {
		if !d.blockedFlag.Load() {
			continue
		}
		values, err := p.browserCookies(doc)
		if err != nil {
			continue
		}
		select {
		case d.gate <- struct{}{}:
		default:
			continue
		}
		changed := cookieMapToHeader(values) != d.cookies
		<-d.gate
		if changed {
			return true
		}
	}
	return false
}

func (p *volgaV6AuthProvider) document(doc string) *volgaV6AuthDocument {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.docs[doc]
	if d == nil {
		d = &volgaV6AuthDocument{gate: make(chan struct{}, 1)}
		p.docs[doc] = d
	}
	return d
}

func (p *volgaV6AuthProvider) seed(doc, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("volga bootstrap: cannot read file")
	}
	if len(data) > 1<<20 {
		return errors.New("volga bootstrap: file too large")
	}
	var b volgaV6Bootstrap
	if json.Unmarshal(data, &b) != nil {
		return errors.New("volga bootstrap: invalid JSON")
	}
	if err := b.validate(); err != nil {
		return err
	}
	d := p.document(doc)
	d.gate <- struct{}{}
	defer func() { <-d.gate }()
	d.seed = b
	return nil
}

func (p *volgaV6AuthProvider) browserCookies(doc string) (map[string]string, error) {
	if p.cookiePath == "" {
		return nil, nil
	}
	s, err := transport.NewCookieStore(p.cookiePath)
	if err != nil {
		return nil, errors.New("volga cookie store: cannot read")
	}
	return s.Load(doc), nil
}

func (p *volgaV6AuthProvider) saveCookies(doc string, jar http.CookieJar) error {
	if p.cookiePath == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, err := transport.NewCookieStore(p.cookiePath)
	if err != nil {
		return errors.New("volga cookie store: cannot read")
	}
	values := map[string]string{}
	for _, host := range []string{"disk.yandex.ru", "docs.yandex.ru"} {
		u := &url.URL{Scheme: "https", Host: host, Path: "/"}
		for _, c := range jar.Cookies(u) {
			values[c.Name] = c.Value
		}
	}
	if len(values) == 0 {
		return nil
	}
	if s.Save(doc, values) != nil {
		return errors.New("volga cookie store: cannot save")
	}
	return nil
}

func (p *volgaV6AuthProvider) client(cookies map[string]string) *http.Client {
	jar, _ := cookiejar.New(nil)
	for _, host := range []string{"disk.yandex.ru", "docs.yandex.ru"} {
		u := &url.URL{Scheme: "https", Host: host, Path: "/"}
		for k, v := range cookies {
			jar.SetCookies(u, []*http.Cookie{{Name: k, Value: v, Path: "/", Secure: true}})
		}
	}
	rt := p.roundTripper
	if rt == nil {
		rt = &http.Transport{DialContext: ipv4First(&net.Dialer{}), MaxIdleConns: 4, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}
	}
	if len(p.browserHeaders) != 0 {
		rt = &volgaV6BrowserTransport{base: rt, headers: p.browserHeaders.Clone()}
	}
	return &http.Client{Jar: jar, Transport: rt, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// The browser profile accompanies only the editor cookies, never the Volga
// session. The whitelist deliberately excludes Cookie, Authorization and Host.
type volgaV6BrowserTransport struct {
	base    http.RoundTripper
	headers http.Header
}

func (t *volgaV6BrowserTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if volgaV6AllowedURL(r.URL.String(), true) {
		r = r.Clone(r.Context())
		r.Header = r.Header.Clone()
		for _, name := range []string{"User-Agent", "Accept", "Accept-Language", "Sec-CH-UA", "Sec-CH-UA-Mobile", "Sec-CH-UA-Platform"} {
			if v := t.headers.Get(name); v != "" {
				r.Header.Set(name, v)
			}
		}
	}
	return t.base.RoundTrip(r)
}
func (t *volgaV6BrowserTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// bootstrap serializes refreshes per document. A CAPTCHA/login rejection is
// latched until that document's persisted cookies change, like v4.0.5 Legacy.
// Expired imported tokens are never sent. Existing healthy sessions aren't
// disconnected merely because an editor bootstrap token has expired.
func (p *volgaV6AuthProvider) bootstrap(ctx context.Context, doc string, force bool) (volgaV6Bootstrap, error) {
	d := p.document(doc)
	select {
	case d.gate <- struct{}{}:
	case <-ctx.Done():
		return volgaV6Bootstrap{}, ctx.Err()
	}
	defer func() { <-d.gate }()
	values, err := p.browserCookies(doc)
	if err != nil {
		return volgaV6Bootstrap{}, err
	}
	fingerprint := cookieMapToHeader(values)
	if d.cookies != fingerprint {
		d.blocked = nil
		d.blockedFlag.Store(false)
		d.seed.Access = ""
		d.cookies = fingerprint
	}
	if d.blocked != nil {
		return volgaV6Bootstrap{}, d.blocked
	}
	if !force && d.seed.Access != "" && d.seed.expires().After(p.now().Add(time.Minute)) {
		return d.seed, nil
	}
	client := p.client(values)
	defer client.CloseIdleConnections()
	// A previously captured direct editor URL avoids the public-link redirect
	// chain, but must issue a fresh configuration; it is not a refresh token.
	entry := doc
	if d.seed.Editor != "" {
		entry = d.seed.Editor
	}
	b, err := p.fetchBootstrap(ctx, client, entry)
	if errors.Is(err, ErrCaptchaRequired) || errors.Is(err, ErrLoginRequired) {
		d.blocked = err
		d.blockedFlag.Store(true)
	}
	if err != nil {
		return volgaV6Bootstrap{}, err
	}
	if d.seed.Resource != "" && d.seed.Resource != b.Resource {
		return volgaV6Bootstrap{}, errors.New("volga refresh: document identity changed")
	}
	if err = p.saveCookies(doc, client.Jar); err != nil {
		return volgaV6Bootstrap{}, err
	}
	updated, err := p.browserCookies(doc)
	if err != nil {
		return volgaV6Bootstrap{}, err
	}
	d.cookies = cookieMapToHeader(updated)
	d.seed = b
	return b, nil
}

var volgaV6ClientConfig = regexp.MustCompile(`(?s)<script[^>]*id=["']client-config["'][^>]*>(.*?)</script>`)

func volgaV6BrowserHeaders(req *http.Request) {
	setBrowserHeaders(req, volgaUserAgent)
	// Let net/http negotiate and decode gzip. Manually advertising deflate
	// would leave compressed HTML unparsed by this client.
	req.Header.Del("Accept-Encoding")
}

func volgaV6AuthRequest(ctx context.Context, c *http.Client, method, raw string, body io.Reader) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, raw, body)
	if err != nil {
		return nil, nil, errors.New("volga auth: invalid request")
	}
	volgaV6BrowserHeaders(req)
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, errors.New("volga auth: HTTP request failed")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(b) > 4<<20 {
		return nil, nil, errors.New("volga auth: invalid response body")
	}
	return resp, b, nil
}

func (p *volgaV6AuthProvider) fetchBootstrap(ctx context.Context, c *http.Client, entry string) (volgaV6Bootstrap, error) {
	current := entry
	solved := false
	for i := 0; i < 10; i++ {
		u, err := url.Parse(current)
		if err == nil && u.Hostname() == "passport.yandex.ru" {
			return volgaV6Bootstrap{}, ErrLoginRequired
		}
		if !volgaV6AllowedURL(current, true) {
			return volgaV6Bootstrap{}, errors.New("volga refresh: unexpected redirect")
		}
		if strings.Contains(u.Path, "showcaptcha") && !strings.Contains(u.Path, "showcaptchafast") {
			return volgaV6Bootstrap{}, ErrCaptchaRequired
		}
		resp, body, err := volgaV6AuthRequest(ctx, c, "GET", current, nil)
		if err != nil {
			return volgaV6Bootstrap{}, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc, e := url.Parse(resp.Header.Get("Location"))
			if e != nil || resp.Header.Get("Location") == "" {
				return volgaV6Bootstrap{}, errors.New("volga refresh: missing redirect")
			}
			current = u.ResolveReference(loc).String()
			continue
		}
		if strings.Contains(u.Path, "showcaptchafast") {
			if solved {
				return volgaV6Bootstrap{}, ErrCaptchaRequired
			}
			solved = true
			if err = volgaV6SolveBlink(ctx, c, current, body); err != nil {
				return volgaV6Bootstrap{}, err
			}
			current = entry
			continue
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return volgaV6Bootstrap{}, ErrLoginRequired
		}
		if resp.StatusCode != 200 {
			return volgaV6Bootstrap{}, fmt.Errorf("volga refresh: HTTP %d", resp.StatusCode)
		}
		match := volgaV6ClientConfig.FindSubmatch(body)
		if len(match) != 2 {
			return volgaV6Bootstrap{}, errors.New("volga refresh: client-config missing")
		}
		var cfg struct {
			Office struct {
				Action   string      `json:"action_url"`
				Access   string      `json:"access_token"`
				TTL      json.Number `json:"access_token_ttl"`
				Resource string      `json:"resource_url"`
			} `json:"officeActionData"`
			Editor struct {
				ID string `json:"idDoc"`
			} `json:"editorParams"`
		}
		if json.Unmarshal(match[1], &cfg) != nil {
			return volgaV6Bootstrap{}, errors.New("volga refresh: invalid configuration")
		}
		b := volgaV6Bootstrap{Schema: "openflux-volga-bootstrap-v1", Editor: current, Action: cfg.Office.Action, Access: cfg.Office.Access, TTL: cfg.Office.TTL, Resource: cfg.Office.Resource, DocID: cfg.Editor.ID}
		if err = b.validate(); err != nil {
			return volgaV6Bootstrap{}, err
		}
		if !b.expires().After(p.now().Add(time.Minute)) {
			return volgaV6Bootstrap{}, errors.New("volga refresh: fresh token already expiring")
		}
		return b, nil
	}
	return volgaV6Bootstrap{}, errors.New("volga refresh: redirect limit")
}

// Reuse the reviewed Legacy parser/fingerprint, adding a bounded and cancellable
// PoW loop and endpoint validation. Interactive SmartCaptcha stays AUTH_BLOCKED.
func volgaV6SolveBlink(ctx context.Context, c *http.Client, raw string, body []byte) error {
	userAgent := volgaUserAgent
	if tr, ok := c.Transport.(*volgaV6BrowserTransport); ok && tr.headers.Get("User-Agent") != "" {
		userAgent = tr.headers.Get("User-Agent")
	}
	ssr, action, err := parseCaptchaHTML(string(body))
	if err != nil {
		return errors.New("volga blink: unsupported challenge")
	}
	u, _ := url.Parse(raw)
	// Legacy's parser resolves relative forms against docs.yandex.ru. Volga
	// also uses disk.yandex.ru; preserve the actual challenge origin instead.
	if match := reFormAction.FindSubmatch(body); len(match) == 2 {
		action = html.UnescapeString(string(match[1]))
	}
	a, err := url.Parse(action)
	if err != nil {
		return errors.New("volga blink: invalid action")
	}
	action = u.ResolveReference(a).String()
	if !volgaV6AllowedURL(action, true) {
		return errors.New("volga blink: unexpected action")
	}
	if ssr.Pow.Complexity < 0 || ssr.Pow.Complexity > 24 {
		return errors.New("volga blink: excessive complexity")
	}
	prefix, err := hexDecode(ssr.Pow.Prefix)
	if err != nil || len(prefix) == 0 {
		prefix = []byte(ssr.Pow.Prefix)
	}
	deadline := time.Now().Add(3 * time.Second)
	var nonce [16]byte
	solution := ""
	putU64LE(nonce[:8], uint64(time.Now().UnixMilli()))
	for n := uint64(0); n < 10_000_000; n++ {
		if n%1024 == 0 {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if time.Now().After(deadline) {
				break
			}
		}
		putU64LE(nonce[8:], n)
		h := sha256.New()
		h.Write(nonce[:])
		h.Write(prefix)
		if captchaCheckComplexity(h.Sum(nil), ssr.Pow.Complexity) {
			solution = hexEncode(nonce[:])
			break
		}
	}
	if solution == "" {
		return errors.New("volga blink: computation limit")
	}
	form := url.Values{"version": {"1.5.0"}, "uniquekey": {ssr.UniqueKey}, "chstate": {"ok"}, "fingerprint": {encodeCaptchaFingerprint(buildCaptchaFingerprint(solution, userAgent))}}
	req, _ := http.NewRequestWithContext(ctx, "POST", action, strings.NewReader(form.Encode()))
	volgaV6BrowserHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", u.Scheme+"://"+u.Host)
	req.Header.Set("Referer", raw)
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("volga blink: HTTP request failed")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return fmt.Errorf("volga blink: challenge rejected (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func (p *volgaV6AuthProvider) authorize(ctx context.Context, doc string) (*volgaAuth, error) {
	// A bootstrap is a session-creation credential, not a reusable pool token.
	// Reusing it can leave the old WebSocket alive while its relay returns 401.
	// Every physical lane therefore obtains a fresh editor configuration.
	b, err := p.bootstrap(ctx, doc, true)
	if err != nil {
		return nil, err
	}
	return p.authorizeBootstrap(ctx, b)
}

func (p *volgaV6AuthProvider) authorizeBootstrap(ctx context.Context, b volgaV6Bootstrap) (*volgaAuth, error) {
	if err := b.validate(); err != nil {
		return nil, err
	}
	if !b.expires().After(p.now()) {
		return nil, errors.New("volga auth: expired bootstrap")
	}
	// Volga gets a new jar for each lane; account/editor cookies are not copied.
	c := p.client(nil)
	defer c.CloseIdleConnections()
	form := url.Values{"access_token": {b.Access}, "access_token_ttl": {b.TTL.String()}}
	req, _ := http.NewRequestWithContext(ctx, "POST", b.Action, strings.NewReader(form.Encode()))
	volgaV6BrowserHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://disk.yandex.ru")
	req.Header.Set("Referer", b.Editor)
	req.Header.Set("Sec-Fetch-Dest", "iframe")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("volga auth: initial request failed")
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
	resp.Body.Close()
	if resp.StatusCode != 302 {
		return nil, fmt.Errorf("volga auth: initial HTTP %d", resp.StatusCode)
	}
	action, _ := url.Parse(b.Action)
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.Header.Get("Location") == "" {
		return nil, errors.New("volga auth: missing location")
	}
	loc = action.ResolveReference(loc)
	if !volgaV6AllowedURL(loc.String(), false) {
		return nil, errors.New("volga auth: unexpected location")
	}
	if strings.Contains(loc.Path, "/document/error/") {
		return nil, errors.New("volga auth: document rejected")
	}
	q := loc.Query()
	var data map[string]interface{}
	dec := json.NewDecoder(bytes.NewBufferString(q.Get("json")))
	dec.UseNumber()
	if dec.Decode(&data) != nil {
		return nil, errors.New("volga auth: invalid session")
	}
	xiva, _ := data["xiva"].(map[string]interface{})
	a := &volgaAuth{Session: c, AccessToken: b.Access, Token: q.Get("token"), RequestPath: q.Get("request-path"), ResourceURL: b.Resource, DocID: b.DocID, SessionID: getStr(data, "sessionId"), UserID: int(getFloat(data, "userId")), UserIDStr: getStr(xiva, "user"), Sign: getStr(xiva, "sign"), TS: getStr(xiva, "ts")}
	if a.Token == "" || a.RequestPath == "" || a.SessionID == "" || a.UserIDStr == "" || a.Sign == "" || a.TS == "" {
		return nil, errors.New("volga auth: incomplete session")
	}
	req, _ = http.NewRequestWithContext(ctx, "GET", loc.String(), nil)
	volgaV6BrowserHeaders(req)
	req.Header.Set("Referer", b.Action)
	resp, err = c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("volga auth: activation failed")
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("volga auth: activation HTTP %d", resp.StatusCode)
	}
	a.Cookies = c.Jar.Cookies(loc)
	return a, nil
}
