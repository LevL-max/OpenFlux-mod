package yandex

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

func redirectChain(t *testing.T, hosts ...string) (*http.Request, []*http.Request) {
	t.Helper()
	var reqs []*http.Request
	for _, h := range hosts {
		r, err := http.NewRequest("GET", "https://"+h+"/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		reqs = append(reqs, r)
	}
	return reqs[len(reqs)-1], reqs[:len(reqs)-1]
}

func TestDocRedirectLoopThroughPassportIsLoginRequired(t *testing.T) {
	// A revoked session bounces between the document and Passport.
	var loop []string
	for i := 0; i < 11; i++ {
		loop = append(loop, map[bool]string{true: "docs.yandex.ru", false: "passport.yandex.ru"}[i%2 == 0])
	}
	req, via := redirectChain(t, loop...)
	err := docRedirectPolicy(req, via)
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("passport loop: %v, want ErrLoginRequired", err)
	}
	// It must survive the http.Client wrapping, which the caller sees.
	if !errors.Is(&url.Error{Op: "Get", URL: "https://disk.yandex.ru/i/x", Err: err}, ErrLoginRequired) {
		t.Fatal("ErrLoginRequired lost inside url.Error")
	}

	// A short hop through Passport, as a session refresh does, is followed.
	req, via = redirectChain(t, "disk.yandex.ru", "passport.yandex.ru", "docs.yandex.ru")
	if err := docRedirectPolicy(req, via); err != nil {
		t.Fatalf("short passport hop refused: %v", err)
	}

	// A loop elsewhere stays an ordinary refusal, retried with a growing pause.
	var docs []string
	for i := 0; i < 11; i++ {
		docs = append(docs, "docs.yandex.ru")
	}
	req, via = redirectChain(t, docs...)
	err = docRedirectPolicy(req, via)
	if err == nil || errors.Is(err, ErrLoginRequired) {
		t.Fatalf("docs-only loop: %v", err)
	}
	if isNetworkFailure(&url.Error{Op: "Get", URL: "https://disk.yandex.ru/i/x", Err: err}) {
		t.Fatal("a redirect loop counted as a network failure")
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestIsNetworkFailureSeparatesTheNetworkFromYandex(t *testing.T) {
	wrap := func(err error) error { return &url.Error{Op: "Get", URL: "https://disk.yandex.ru/i/x", Err: err} }
	network := []error{
		wrap(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}),
		wrap(&net.DNSError{Err: "no such host", Name: "disk.yandex.ru"}),
		wrap(timeoutError{}),
		fmt.Errorf("OnlyOffice build detection failed: %w", wrap(&net.OpError{Op: "read", Net: "tcp", Err: errors.New("reset")})),
	}
	for _, err := range network {
		if !isNetworkFailure(err) {
			t.Fatalf("%v: not a network failure", err)
		}
	}
	refused := []error{
		fmt.Errorf("config not found: looks like a login page (doc not public?)"),
		fmt.Errorf("officeActionData missing"),
		wrap(errors.New("stopped after 10 redirects (login required? doc not public?)")),
	}
	for _, err := range refused {
		if isNetworkFailure(err) {
			t.Fatalf("%v: counted as a network failure", err)
		}
	}
}

func TestRefusalBackoffGrowsToTenMinutes(t *testing.T) {
	for n := 0; n <= 3; n++ {
		if d := refusalBackoff(n); d != 0 {
			t.Fatalf("refusal %d waits %v; the first three keep the quick backoff", n, d)
		}
	}
	for n, base := range map[int]time.Duration{4: 30 * time.Second, 5: time.Minute, 6: 2 * time.Minute, 7: 4 * time.Minute, 8: 8 * time.Minute, 9: 10 * time.Minute, 50: 10 * time.Minute} {
		if d := refusalBackoff(n); d < base || d > base+base/4 {
			t.Fatalf("refusal %d waits %v, want %v plus up to 25%%", n, d, base)
		}
	}
}

func TestRetryPicksUpCookiesStoredByAnotherWriter(t *testing.T) {
	doc := "https://disk.yandex.ru/i/test"
	path := filepath.Join(t.TempDir(), "cookies.json")
	seed, err := transport.NewCookieStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Save(doc, map[string]string{"Session_id": "old"}); err != nil {
		t.Fatal(err)
	}
	tr := NewYandexDocsTransport(doc, transport.DefaultConfig())
	if err := tr.SetCookieStore(path); err != nil {
		t.Fatal(err)
	}

	// The exit's own write (Set-Cookie merged after a fetch) is not news.
	tr.refusals.Store(6)
	if err := tr.updateCookieState(map[string]string{"yandexuid": "1"}); err != nil {
		t.Fatal(err)
	}
	tr.reloadCookieStoreIfChanged()
	if tr.refusals.Load() != 6 {
		t.Fatal("the exit's own cookie write was treated as fresh cookies")
	}

	// The Disk recovery inbox stores fresh cookies while the exit retries.
	other, err := transport.NewCookieStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Save(doc, map[string]string{"Session_id": "new"}); err != nil {
		t.Fatal(err)
	}
	tr.reloadCookieStoreIfChanged()
	if cookie, _ := tr.currentBrowserState(); cookie != "Session_id=new" {
		t.Fatalf("after another writer: cookie header %q", cookie)
	}
	if tr.refusals.Load() != 0 {
		t.Fatal("fresh cookies must restore quick retries")
	}
}
