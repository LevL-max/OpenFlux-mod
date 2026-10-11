package mailru

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"universal-bypass-tool/transport"
)

func TestDocumentKeyRefreshRejoinsNewRoom(t *testing.T) {
	var key atomic.Value
	key.Store("old")
	joined := make(chan string, 4)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/edit", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"api": "ws" + strings.TrimPrefix(srv.URL, "http"), "token": "test",
			"document":     map[string]interface{}{"key": key.Load(), "fileType": "docx"},
			"editorConfig": map[string]interface{}{"user": map[string]string{"id": "test"}},
		})
	})
	mux.HandleFunc("/doc/", func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.WriteMessage(websocket.TextMessage, []byte(`0{"sid":"test","pingInterval":25000,"pingTimeout":20000}`))
		if _, _, err = c.ReadMessage(); err != nil {
			return
		}
		c.WriteMessage(websocket.TextMessage, []byte(`40{"sid":"test"}`))
		if _, _, err = c.ReadMessage(); err != nil {
			return
		}
		c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"auth","result":1}]`))
		joined <- r.URL.Path
		for {
			if _, _, err = c.ReadMessage(); err != nil {
				return
			}
		}
	})
	oldAPI := editAPI
	editAPI = srv.URL + "/edit"
	defer func() { editAPI = oldAPI }()
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	tr.Start()
	defer tr.Stop()
	waitRoom := func(want string) {
		t.Helper()
		select {
		case path := <-joined:
			if path != "/doc/"+want+"/c/" {
				t.Fatalf("joined %s", path)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("did not join expected room")
		}
		deadline := time.Now().Add(time.Second)
		for !tr.IsConnected() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if !tr.IsConnected() {
			t.Fatal("room not ready")
		}
	}
	waitRoom("old")
	tr.refreshDocumentKey()
	if !tr.IsConnected() || tr.Stats().Reconnects != 0 {
		t.Fatal("unchanged key disturbed connection")
	}
	key.Store("new")
	tr.refreshDocumentKey()
	waitRoom("new")
	if tr.Stats().Reconnects != 1 {
		t.Fatal("key rollover did not reconnect exactly once")
	}
}

func TestDocumentKeyRefreshCannotDisconnectReplacement(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-release
		json.NewEncoder(w).Encode(map[string]interface{}{
			"api": "wss://unused.invalid", "token": "test",
			"document":     map[string]string{"key": "new", "fileType": "docx"},
			"editorConfig": map[string]interface{}{"user": map[string]string{"id": "test"}},
		})
	}))
	defer srv.Close()
	oldAPI := editAPI
	editAPI = srv.URL
	defer func() { editAPI = oldAPI }()
	old, _ := controlPeer(t)
	old.Info.DocKey = "old"
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	tr.BaseTransport.Start()
	tr.session = old
	tr.SetConnected(true)
	defer tr.Stop()
	done := make(chan struct{})
	go func() { tr.refreshDocumentKey(); close(done) }()
	<-started
	tr.Mu.Lock()
	tr.session = &DocSession{Info: MailruDocsInfo{DocKey: "new"}}
	tr.Mu.Unlock()
	close(release)
	<-done
	if !tr.IsConnected() {
		t.Fatal("stale metadata response disconnected replacement")
	}
}

func TestMetadataFailureKeepsHealthySocket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	oldAPI := editAPI
	editAPI = srv.URL
	defer func() { editAPI = oldAPI }()
	session, _ := controlPeer(t)
	session.Info.DocKey = "current"
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	tr.BaseTransport.Start()
	tr.session = session
	tr.SetConnected(true)
	defer tr.Stop()
	tr.refreshDocumentKey()
	if !tr.IsConnected() || tr.Stats().Reconnects != 0 {
		t.Fatal("metadata failure disturbed healthy connection")
	}
}

func TestStopCancelsDocumentKeyRefresh(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	oldAPI := editAPI
	editAPI = srv.URL
	defer func() { editAPI = oldAPI }()
	session, _ := controlPeer(t)
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	tr.BaseTransport.Start()
	tr.session = session
	tr.SetConnected(true)
	defer tr.Stop()
	done := make(chan struct{})
	go func() { tr.documentKeyLoop(time.Millisecond); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("metadata refresh did not start")
	}
	tr.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("metadata refresh survived Stop")
	}
}
