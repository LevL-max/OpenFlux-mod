package mailru

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

func saveBrowserFixture(t *testing.T, path, cookie string) {
	t.Helper()
	p := browserProfile{Schema: 1, Document: "https://cloud.mail.ru/public/test/doc", Cookies: map[string]string{"Mpop": cookie}, Headers: map[string]string{"user-agent": "FixtureBrowser", "x-csrf-token": "fixture-header"}, Body: map[string]json.RawMessage{"public": json.RawMessage(`"/test/doc"`), "token": json.RawMessage(`"fixture-body"`)}, Query: map[string][]string{"token": {"fixture-query"}}}
	b, _ := json.Marshal(p)
	tmp := path + ".new"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func editorFixture(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"api":"https://editor.example.test","token":"fixture","document":{"key":"fixture","fileType":"docx"},"editorConfig":{"user":{"id":"same-account"}}}`))
}

func TestBrowserCredentialsReloadAfterRefusalWithoutRetryStorm(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		cookie, _ := r.Cookie("Mpop")
		if cookie == nil || cookie.Value != "fresh" {
			w.WriteHeader(401)
			return
		}
		if r.UserAgent() != "FixtureBrowser" || r.Header.Get("X-Csrf-Token") != "fixture-header" || r.URL.Query().Get("token") != "fixture-query" {
			t.Error("browser request headers/query lost")
		}
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["token"] != "fixture-body" || body["public"] != "/test/doc" {
			t.Error("browser request body lost")
		}
		editorFixture(w)
	}))
	defer srv.Close()
	old := editAPI
	editAPI = srv.URL
	defer func() { editAPI = old }()
	path := filepath.Join(t.TempDir(), "browser.json")
	saveBrowserFixture(t, path, "expired")
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	defer tr.Stop()
	if err := tr.SetBrowserProfile(path); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := tr.fetchDocInfo("test/doc"); !errors.Is(err, errBrowserAuth) {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("refused credentials were retried")
	}
	tr.BaseTransport.Start()
	done := make(chan struct{})
	go func() { tr.browserProfileLoop(); close(done) }()
	saveBrowserFixture(t, path, "fresh")
	select {
	case <-tr.authChanged:
	case <-time.After(3 * time.Second):
		t.Fatal("import did not wake authorization")
	}
	info, err := tr.fetchDocInfo("test/doc")
	if err != nil || !info.Authenticated || tr.browserAuthBlocked() {
		t.Fatalf("fresh profile not accepted: %v", err)
	}
	tr.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("profile watcher survived Stop")
	}
}

func TestBrowserCookiesNeverFollowRedirects(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }))
	defer srv.Close()
	old := editAPI
	editAPI = srv.URL
	defer func() { editAPI = old }()
	path := filepath.Join(t.TempDir(), "browser.json")
	saveBrowserFixture(t, path, "secret")
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	defer tr.Stop()
	if err := tr.SetBrowserProfile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.fetchDocInfo("test/doc"); err == nil {
		t.Fatal("redirect accepted")
	}
	if leaked.Load() {
		t.Fatal("browser request followed redirect")
	}
}

func TestStaleBrowserRefusalCannotBlockNewImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "browser.json")
	saveBrowserFixture(t, path, "old")
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	defer tr.Stop()
	if err := tr.SetBrowserProfile(path); err != nil {
		t.Fatal(err)
	}
	old := tr.profileSum
	saveBrowserFixture(t, path, "new")
	if _, err := tr.reloadBrowserProfile(); err != nil {
		t.Fatal(err)
	}
	tr.blockBrowserAuth(old)
	if tr.browserAuthBlocked() {
		t.Fatal("stale request blocked new credentials")
	}
	wrong := NewMailruDocsTransport("other/doc", transport.DefaultConfig())
	defer wrong.Stop()
	if err := wrong.SetBrowserProfile(path); err == nil {
		t.Fatal("other document profile accepted")
	}
}

func TestAuthenticatedChatSeparatesSessionsWithSameAccount(t *testing.T) {
	local, peer := strings.Repeat("A", 26), strings.Repeat("B", 26)
	for _, sender := range []string{local, peer} {
		encoded := encodeChatData([]byte("payload"), sender)
		var parts []json.RawMessage
		var msg map[string]string
		_ = json.Unmarshal(encoded[2:], &parts)
		_ = json.Unmarshal(parts[1], &msg)
		b, _ := json.Marshal([]any{"message", map[string]any{"type": "message", "messages": []map[string]string{{"message": msg["message"], "useridoriginal": "same-account"}}}})
		got := decodeSessionChatData(append([]byte("42"), b...), local, "same-account")
		if sender == local && len(got) != 0 {
			t.Fatal("self echo delivered")
		}
		if sender == peer && (len(got) != 1 || string(got[0]) != "payload") {
			t.Fatal("peer with same account was discarded")
		}
	}
}

func TestPublicPageBootstrapRetainsCookiesAndRenewsExpiredToken(t *testing.T) {
	var pages, edits atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/public/test/doc", func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		cookie, err := r.Cookie("Mpop")
		if err != nil || cookie.Value != "login" {
			t.Error("login missing on bootstrap")
		}
		http.SetCookie(w, &http.Cookie{Name: "oid", Value: "bootstrap-session", Path: "/"})
		_, _ = w.Write([]byte(`<html><script>var state={"tokens":{"csrf":"fresh-token"},"other":{}};</script></html>`))
	})
	mux.HandleFunc("/edit", func(w http.ResponseWriter, r *http.Request) {
		n := edits.Add(1)
		cookie, err := r.Cookie("oid")
		if err != nil || cookie.Value != "bootstrap-session" {
			t.Error("bootstrap Set-Cookie lost")
		}
		if r.URL.Query().Get("token") != "fresh-token" || r.Header.Get("X-Csrf-Token") != "fresh-token" {
			t.Error("CSRF token missing")
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["token"] != "fresh-token" || body["public"] != "/test/doc" {
			t.Error("bootstrap body incorrect")
		}
		if n == 2 {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"token"}`))
			return
		}
		editorFixture(w)
	})
	oldAPI, oldPage := editAPI, publicPageBase
	editAPI = srv.URL + "/edit"
	publicPageBase = srv.URL + "/public/"
	defer func() { editAPI = oldAPI; publicPageBase = oldPage }()
	path := filepath.Join(t.TempDir(), "browser.json")
	saveBrowserFixture(t, path, "login")
	b, _ := os.ReadFile(path)
	var profile browserProfile
	_ = json.Unmarshal(b, &profile)
	profile.Bootstrap = true
	b, _ = json.Marshal(profile)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	defer tr.Stop()
	if err := tr.SetBrowserProfile(path); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := tr.fetchDocInfo("test/doc"); err != nil {
			t.Fatal(err)
		}
	}
	if pages.Load() != 2 || edits.Load() != 3 || tr.browserAuthBlocked() {
		t.Fatal("token renewal failed or retried excessively")
	}
}
