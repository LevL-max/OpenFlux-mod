package mailru

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

func TestRefusalBackoffAfterThreeQuickTries(t *testing.T) {
	for n := 0; n <= 3; n++ {
		if d := refusalBackoff(n); d != 0 {
			t.Fatalf("refusals %d: backoff %v, want 0", n, d)
		}
	}
	for n, want := range map[int]time.Duration{4: 30 * time.Second, 5: time.Minute, 8: 8 * time.Minute, 9: 10 * time.Minute, 40: 10 * time.Minute} {
		if d := refusalBackoff(n); d < want || d > want+want/4 {
			t.Fatalf("refusals %d: backoff %v, want %v..%v", n, d, want, want+want/4)
		}
	}
}

func TestDocumentRefusalIsNotANetworkFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()
	old := editAPI
	defer func() { editAPI = old }()

	tr := NewMailruDocsTransport("AbCd/EfGh", transport.DefaultConfig())
	editAPI = server.URL
	if _, err := tr.fetchDocInfo("AbCd/EfGh"); err == nil || isNetworkFailure(err) {
		t.Fatalf("HTTP 404: err %v; want a refusal, not a network failure", err)
	}

	// Nothing listens on a closed listener's port: the network failed.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	editAPI = "http://" + l.Addr().String()
	l.Close()
	if _, err := tr.fetchDocInfo("AbCd/EfGh"); err == nil || !isNetworkFailure(err) {
		t.Fatalf("closed port: err %v; want a network failure", err)
	}
}
