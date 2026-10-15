package mailru

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"universal-bypass-tool/utils"
)

const browserProfileLimit = 131072

var errBrowserAuth = errors.New("Mail.ru requires fresh browser cookies")
var cookieName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// Only a request to our fixed editor endpoint is replayed. Secrets are never
// forwarded to the document WebSocket host, redirects, logs or chat messages.
type browserProfile struct {
	Schema    int                        `json:"schema"`
	Document  string                     `json:"document"`
	Cookies   map[string]string          `json:"cookies"`
	Headers   map[string]string          `json:"headers"`
	Body      map[string]json.RawMessage `json:"body"`
	Query     url.Values                 `json:"query"`
	Bootstrap bool                       `json:"bootstrap,omitempty"`
}

var browserHeaders = map[string]bool{
	"User-Agent": true, "Accept": true, "Accept-Language": true,
	"Sec-Ch-Ua": true, "Sec-Ch-Ua-Mobile": true, "Sec-Ch-Ua-Platform": true,
	"X-Api-Version": true, "X-Email": true, "X-Csrf-Token": true,
	"X-Request-Token": true, "Authorization": true,
}

func (t *MailruDocsTransport) SetBrowserProfile(path string) error {
	if t.IsRunning() {
		return errors.New("configure the Mail.ru browser profile before Start")
	}
	t.profilePath = path
	_, err := t.reloadBrowserProfile()
	return err
}

func (t *MailruDocsTransport) reloadBrowserProfile() (bool, error) {
	t.authMu.Lock()
	defer t.authMu.Unlock()
	if t.profilePath == "" {
		return false, nil
	}
	f, err := os.Open(t.profilePath)
	var data []byte
	if err == nil {
		defer f.Close()
		st, e := f.Stat()
		if e != nil || !st.Mode().IsRegular() || runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0 {
			return false, errors.New("Mail.ru browser profile must be a private regular file (0600)")
		}
		data, err = io.ReadAll(io.LimitReader(f, browserProfileLimit+1))
	} else if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return false, errors.New("cannot read Mail.ru browser profile")
	}
	if len(data) > browserProfileLimit {
		return false, errors.New("Mail.ru browser profile is too large")
	}
	sum := sha256.Sum256(data)
	if t.profileLoaded && sum == t.profileSum {
		return false, nil
	}
	var profile *browserProfile
	if len(data) != 0 {
		profile = new(browserProfile)
		if json.Unmarshal(data, profile) != nil || profile.Schema != 1 || profile.Document != "https://cloud.mail.ru/public/"+t.weblink || len(profile.Cookies) == 0 {
			return false, errors.New("invalid Mail.ru browser profile or document mismatch")
		}
		for name, value := range profile.Cookies {
			if !cookieName.MatchString(name) || strings.ContainsAny(value, "\r\n\x00;") || (&http.Cookie{Name: name, Value: value}).Valid() != nil {
				return false, errors.New("invalid Mail.ru cookie")
			}
		}
		for name, value := range profile.Headers {
			if !browserHeaders[http.CanonicalHeaderKey(name)] || strings.ContainsAny(value, "\r\n\x00") || len(value) > 8192 {
				return false, errors.New("invalid Mail.ru browser header")
			}
		}
		var public string
		if json.Unmarshal(profile.Body["public"], &public) != nil || public != "/"+t.weblink {
			return false, errors.New("Mail.ru request document mismatch")
		}
	}
	t.profile, t.profileSum, t.profileLoaded = profile, sum, true
	t.browserToken = ""
	t.authBlocked = false
	// Changing the import replaces all previous session cookies, including ones
	// from a different account. Normal Set-Cookie updates stay in this in-memory jar.
	t.cookieJar, _ = cookiejar.New(nil)
	if profile != nil {
		u, _ := url.Parse(editAPI)
		cookies := make([]*http.Cookie, 0, len(profile.Cookies))
		for name, value := range profile.Cookies {
			cookies = append(cookies, &http.Cookie{Name: name, Value: value, Path: "/", Secure: u.Scheme == "https"})
		}
		t.cookieJar.SetCookies(u, cookies)
	}
	return true, nil
}

func (t *MailruDocsTransport) blockBrowserAuth(sum [32]byte) {
	t.authMu.Lock()
	current := sum == t.profileSum
	if current {
		t.authBlocked = true
	}
	t.authMu.Unlock()
	if current {
		t.statusThrottled("auth-required", time.Minute, "[M-AUTH] AUTH_BLOCKED: Mail.ru requires fresh browser cookies; waiting for a new import")
	}
}

func (t *MailruDocsTransport) browserAuthBlocked() bool {
	t.authMu.Lock()
	defer t.authMu.Unlock()
	return t.authBlocked
}

func (t *MailruDocsTransport) browserProfileLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
		}
		changed, err := t.reloadBrowserProfile()
		if err != nil {
			t.statusThrottled("profile-invalid", time.Minute, "[M-AUTH] browser profile unavailable or invalid; import it again")
			continue
		}
		if !changed {
			continue
		}
		t.refusals.Store(0)
		utils.Statusf("[M-AUTH] browser profile changed; retrying authorization")
		select {
		case t.authChanged <- struct{}{}:
		default:
		}
		t.Mu.RLock()
		session, opening := t.session, t.opening
		t.Mu.RUnlock()
		if session != nil {
			t.dropConnection(session)
		}
		if opening != nil && opening.Conn != nil {
			_ = opening.Conn.Close()
		}
	}
}

func (t *MailruDocsTransport) editorRequest() (*http.Request, *http.Client, [32]byte, bool, error) {
	// The watcher is the only live reload owner. A metadata refresh must not
	// consume a change before the watcher wakes a blocked reconnect.
	t.authMu.Lock()
	profile, jar, sum, blocked := t.profile, t.cookieJar, t.profileSum, t.authBlocked
	t.authMu.Unlock()
	if blocked {
		return nil, nil, sum, false, errBrowserAuth
	}
	body := map[string]json.RawMessage{}
	for k, v := range map[string]string{"x-email": "anonym", "public": "/" + t.weblink, "platform": "desktop_web"} {
		body[k], _ = json.Marshal(v)
	}
	u, err := url.Parse(editAPI)
	if err != nil {
		return nil, nil, sum, false, errors.New("invalid editor endpoint")
	}
	if profile != nil {
		for key, value := range profile.Body {
			body[key] = value
		}
		u.RawQuery = profile.Query.Encode()
	}
	req, err := http.NewRequestWithContext(t.ctx, "POST", u.String(), nil)
	if err != nil {
		return nil, nil, sum, false, errors.New("cannot create editor request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("User-Agent", mailruUserAgent)
	req.Header.Set("X-Api-Version", "4")
	req.Header.Set("Origin", "https://cloud.mail.ru")
	req.Header.Set("Referer", fmt.Sprintf("https://cloud.mail.ru/public/%s?weblink=%s", t.weblink, t.weblink))
	if profile != nil {
		for k, v := range profile.Headers {
			req.Header.Set(k, v)
		}
	}
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if profile != nil && profile.Bootstrap {
		token, e := t.bootstrapBrowserToken(client, req.Header, sum)
		if e != nil {
			return nil, nil, sum, true, e
		}
		body["token"], _ = json.Marshal(token)
		q := req.URL.Query()
		q.Set("token", token)
		req.URL.RawQuery = q.Encode()
		req.Header.Set("X-Csrf-Token", token)
	}
	data, _ := json.Marshal(body)
	req.Body = io.NopCloser(strings.NewReader(string(data)))
	req.ContentLength = int64(len(data))
	return req, client, sum, profile != nil, nil
}

var publicPageBase = "https://cloud.mail.ru/public/"
var bootstrapTokens = regexp.MustCompile(`"tokens"\s*:\s*`)

func (t *MailruDocsTransport) clearBrowserToken(sum [32]byte) bool {
	t.authMu.Lock()
	defer t.authMu.Unlock()
	if sum != t.profileSum || t.profile == nil || !t.profile.Bootstrap {
		return false
	}
	t.browserToken = ""
	return true
}

func (t *MailruDocsTransport) bootstrapBrowserToken(client *http.Client, headers http.Header, sum [32]byte) (string, error) {
	t.authMu.Lock()
	token := t.browserToken
	current := sum == t.profileSum
	t.authMu.Unlock()
	if token != "" && current {
		return token, nil
	}
	req, err := http.NewRequestWithContext(t.ctx, "GET", publicPageBase+t.weblink, nil)
	if err != nil {
		return "", errors.New("cannot create Mail.ru page request")
	}
	req.Header = headers.Clone()
	req.Header.Del("Content-Type")
	req.Header.Del("X-Csrf-Token")
	req.Header.Set("Accept", "text/html")
	resp, err := client.Do(req)
	if err != nil {
		return "", safeRequestError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode >= 300 && resp.StatusCode < 400 {
		t.blockBrowserAuth(sum)
		return "", errBrowserAuth
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Mail.ru page returned status %d", resp.StatusCode)
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(page) > 4<<20 {
		return "", errors.New("cannot read Mail.ru document page")
	}
	for _, location := range bootstrapTokens.FindAllIndex(page, 8) {
		var tokens struct {
			CSRF string `json:"csrf"`
		}
		if json.NewDecoder(strings.NewReader(string(page[location[1]:]))).Decode(&tokens) != nil || tokens.CSRF == "" || len(tokens.CSRF) > 4096 || strings.ContainsAny(tokens.CSRF, "\r\n\x00") {
			continue
		}
		t.authMu.Lock()
		if sum == t.profileSum {
			t.browserToken = tokens.CSRF
		}
		t.authMu.Unlock()
		return tokens.CSRF, nil
	}
	return "", errors.New("Mail.ru page has no browser token; copy the editor POST request instead")
}

// Keep URL query tokens out of errors while preserving network classification.
type safeRequestError struct{ cause error }

func (e safeRequestError) Error() string { return "Mail.ru editor request failed" }
func (e safeRequestError) Unwrap() error { return e.cause }
