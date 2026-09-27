package yandex

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

// The captcha form must be posted back to the host that served it, so .com
// documents are answered on docs.yandex.com rather than docs.yandex.ru.
func TestCaptchaPostsToServingHost(t *testing.T) {
	var origin, uniqueKey string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/i/doc":
			http.Redirect(w, r, server.URL+"/showcaptchafast?d=1", http.StatusFound)
		case "/showcaptchafast":
			ssr := base64.StdEncoding.EncodeToString([]byte(`{"uniqueKey":"key-1","pow":{"complexity":4,"prefix":"00"},"timestamp":1}`))
			fmt.Fprintf(w, `<script>window.__SSR_DATA__ = JSON.parse(atob("%s"))</script><form id="tmgrdfrend-form" action="/checkcaptchafast?d=1&amp;s=2">`, ssr)
		case "/checkcaptchafast":
			r.ParseForm()
			origin, uniqueKey = r.Header.Get("Origin"), r.PostForm.Get("uniquekey")
			http.Redirect(w, r, server.URL+"/edit/d/doc", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	jar, _ := cookiejar.New(nil)
	retpath, err := solveCaptcha(server.URL+"/i/doc", jar, "")
	if err != nil {
		t.Fatal(err)
	}
	if origin != server.URL || uniqueKey != "key-1" || retpath != server.URL+"/edit/d/doc" {
		t.Fatalf("origin=%q uniquekey=%q retpath=%q", origin, uniqueKey, retpath)
	}
}

func TestCaptchaCookieHosts(t *testing.T) {
	for doc, want := range map[string][]string{
		"https://disk.yandex.ru/i/abc":  {"disk.yandex.ru", "docs.yandex.ru"},
		"https://disk.yandex.com/i/abc": {"disk.yandex.com", "docs.yandex.com"},
	} {
		u, _ := url.Parse(doc)
		if got := captchaCookieHosts(u); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", doc, got, want)
		}
	}
}
