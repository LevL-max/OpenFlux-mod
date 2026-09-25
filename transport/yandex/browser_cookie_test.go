package yandex

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"universal-bypass-tool/transport"
)

func TestLoadBrowserCookiesPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "cookies.txt")
	store := filepath.Join(dir, "cookies.json")

	if err := os.WriteFile(seed, []byte("Cookie: b=2; a=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tr := NewYandexDocsTransport("https://disk.yandex.ru/i/test", transport.DefaultConfig())
	if err := tr.SetCookieStore(store); err != nil {
		t.Fatalf("SetCookieStore failed: %v", err)
	}
	if err := tr.LoadBrowserCookies(seed, "TestBrowser/1.0"); err != nil {
		t.Fatalf("LoadBrowserCookies failed: %v", err)
	}

	if tr.browserCookie != "a=1; b=2" {
		t.Fatalf("unexpected cookie header: %q", tr.browserCookie)
	}
	if tr.browserUserAgent != "TestBrowser/1.0" {
		t.Fatalf("unexpected user agent: %q", tr.browserUserAgent)
	}

	info, err := os.Stat(store)
	if err != nil {
		t.Fatalf("cookie store missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cookie store mode = %o, want 600", info.Mode().Perm())
	}

	tr2 := NewYandexDocsTransport("https://disk.yandex.ru/i/test", transport.DefaultConfig())
	if err := tr2.SetCookieStore(store); err != nil {
		t.Fatalf("reload SetCookieStore failed: %v", err)
	}
	if tr2.browserCookie != "a=1; b=2" {
		t.Fatalf("reloaded cookie header: %q", tr2.browserCookie)
	}
}

func TestMergeCookieHeader(t *testing.T) {
	got := mergeCookieHeader(
		"a=1; b=two=2",
		[]*http.Cookie{
			{Name: "b", Value: "3"},
			{Name: "c", Value: "4"},
		},
	)
	want := "a=1; b=3; c=4"
	if got != want {
		t.Fatalf("mergeCookieHeader() = %q, want %q", got, want)
	}
}

func TestParseCookieHeaderKeepsEqualsInValue(t *testing.T) {
	got := parseCookieHeader("a=1; token=abc==; empty=")
	if got["a"] != "1" || got["token"] != "abc==" || got["empty"] != "" {
		t.Fatalf("unexpected parsed cookies: %#v", got)
	}
}


func TestCookieStoreFingerprintChanges(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "cookies.json")

	before, err := cookieStoreFingerprint(store)
	if err != nil {
		t.Fatalf("initial fingerprint failed: %v", err)
	}
	if before != "missing" {
		t.Fatalf("initial fingerprint = %q, want missing", before)
	}

	if err := os.WriteFile(store, []byte(`{"one":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := cookieStoreFingerprint(store)
	if err != nil {
		t.Fatalf("updated fingerprint failed: %v", err)
	}
	if after == before || after == "" {
		t.Fatalf("fingerprint did not change: before=%q after=%q", before, after)
	}
}

func TestReloadCookieStorePicksUpExternalUpdate(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cookies.json")
	docURL := "https://disk.yandex.ru/i/test"

	tr := NewYandexDocsTransport(docURL, transport.DefaultConfig())
	if err := tr.SetCookieStore(storePath); err != nil {
		t.Fatalf("SetCookieStore failed: %v", err)
	}

	external, err := transport.NewCookieStore(storePath)
	if err != nil {
		t.Fatalf("NewCookieStore failed: %v", err)
	}
	if err := external.Save(docURL, map[string]string{"fresh": "2", "token": "abc=="}); err != nil {
		t.Fatalf("external Save failed: %v", err)
	}

	if err := tr.reloadCookieStore(); err != nil {
		t.Fatalf("reloadCookieStore failed: %v", err)
	}
	if tr.browserCookie != "fresh=2; token=abc==" {
		t.Fatalf("reloaded cookie header = %q", tr.browserCookie)
	}
}
