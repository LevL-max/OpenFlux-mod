package yandex

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// challengeServer redirects the document to a first-tier challenge. page
// answers the challenge page, check the posted answer.
func challengeServer(t *testing.T, page, check http.HandlerFunc) string {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/i/doc":
			http.Redirect(w, r, server.URL+"/showcaptchafast?d=1", http.StatusFound)
		case "/showcaptchafast":
			page(w, r)
		case "/checkcaptchafast":
			check(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL + "/i/doc"
}

func challengePage(w http.ResponseWriter, _ *http.Request) {
	ssr := base64.StdEncoding.EncodeToString([]byte(`{"uniqueKey":"key-1","pow":{"complexity":0,"prefix":"00"},"timestamp":1}`))
	fmt.Fprintf(w, `<script>window.__SSR_DATA__ = JSON.parse(atob("%s"))</script><form id="tmgrdfrend-form" action="/checkcaptchafast?d=1">`, ssr)
}

// A first-tier challenge that Yandex refuses, or presents in a form the solver
// cannot read, needs fresh browser cookies like SmartCaptcha (upstream #170).
func TestRefusedFirstTierChallengeNeedsFreshCookies(t *testing.T) {
	cases := map[string]string{
		"challenge page 403": challengeServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }, nil),
		"unreadable page":    challengeServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>new challenge</html>") }, nil),
		"answer refused":     challengeServer(t, challengePage, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := (&YandexDocsTransport{}).fetchDocInfo(doc, "0000000001")
			if !errors.Is(err, ErrCaptchaRequired) {
				t.Fatalf("want ErrCaptchaRequired, got %v", err)
			}
		})
	}
}

// A connection that breaks while answering is a network failure: it is retried
// with the same cookies and does not latch AUTH_BLOCKED.
func TestBrokenChallengeConnectionIsRetried(t *testing.T) {
	doc := challengeServer(t, challengePage, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	_, err := (&YandexDocsTransport{}).fetchDocInfo(doc, "0000000001")
	if err == nil || errors.Is(err, ErrCaptchaRequired) || errors.Is(err, ErrLoginRequired) {
		t.Fatalf("want a transient error, got %v", err)
	}
	if !isNetworkFailure(err) {
		t.Fatalf("a broken connection must count as a network failure: %v", err)
	}
}
