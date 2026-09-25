package yandex

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"universal-bypass-tool/transport"
)

func TestLoadBrowserCookies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, []byte("Cookie: a=1; b=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tr := NewYandexDocsTransport("https://disk.yandex.ru/i/test", transport.DefaultConfig())
	if err := tr.LoadBrowserCookies(path, "TestBrowser/1.0"); err != nil {
		t.Fatalf("LoadBrowserCookies failed: %v", err)
	}

	if tr.browserCookie != "a=1; b=2" {
		t.Fatalf("unexpected cookie header: %q", tr.browserCookie)
	}
	if tr.browserUserAgent != "TestBrowser/1.0" {
		t.Fatalf("unexpected user agent: %q", tr.browserUserAgent)
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
