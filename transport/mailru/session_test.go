package mailru

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"universal-bypass-tool/transport"
)

func controlPeer(t *testing.T) (*DocSession, <-chan string) {
	t.Helper()
	messages := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			messages <- string(b)
		}
	}))
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); srv.Close() })
	return &DocSession{Conn: c, WriteQueue: make(chan queuedPacket, 16)}, messages
}

func TestControlReplies(t *testing.T) {
	s, received := controlPeer(t)
	for _, tc := range []struct{ in, want string }{
		{`2`, `3`},
		{`42["message",{"type":"authChanges"}]`, `42["message",{"type":"authChangesAck"}]`},
		{`42["message",{"type":"connectState","waitAuth":true}]`, `42["message",{"type":"unLockDocument","unlock":true,"isSave":false,"releaseLocks":false}]`},
	} {
		ok, err := handleMailruControl(s, []byte(tc.in))
		if !ok || err != nil {
			t.Fatalf("handled=%t error=%v", ok, err)
		}
		select {
		case got := <-received:
			if got != tc.want {
				t.Fatalf("got %s", got)
			}
		case <-time.After(time.Second):
			t.Fatal("missing control reply")
		}
	}
}

func TestSendDuringReconnectAndBoundedQueue(t *testing.T) {
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	tr.BaseTransport.Start()
	defer tr.Stop()
	tr.session = &DocSession{WriteQueue: make(chan queuedPacket, 2)}
	data := []byte("retain during reconnect")
	if err := tr.Send(data); err != nil {
		t.Fatal(err)
	}
	data[0] = 'X'
	if got := <-tr.session.WriteQueue; string(got.data) != "retain during reconnect" {
		t.Fatal("queued caller's mutable buffer")
	}
	tr.queuedBytes.Store(0)
	if err := tr.Send(make([]byte, maxQueuedBytes)); err != nil {
		t.Fatal(err)
	}
	if err := tr.Send([]byte{1}); err == nil {
		t.Fatal("byte limit not enforced")
	}
	if got := tr.queuedBytes.Load(); got != maxQueuedBytes {
		t.Fatalf("accounting %d", got)
	}
}

func TestOldSessionCannotDisconnectNewSession(t *testing.T) {
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	tr.BaseTransport.Start()
	defer tr.Stop()
	tr.session = &DocSession{}
	tr.SetConnected(true)
	tr.dropConnection(&DocSession{})
	if !tr.IsConnected() {
		t.Fatal("old generation disconnected current session")
	}
}

func TestWriterRecoversAndExpiresOldPackets(t *testing.T) {
	s, received := controlPeer(t)
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	tr.BaseTransport.Start()
	tr.session = s
	done := make(chan struct{})
	go func() { tr.writerLoop(); close(done) }()
	t.Cleanup(func() {
		tr.Stop()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("writer did not stop")
		}
	})
	tr.queuedBytes.Add(3)
	s.WriteQueue <- queuedPacket{data: []byte("old"), at: time.Now().Add(-maxQueuedAge - time.Second)}
	if err := tr.Send([]byte("new")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
		t.Fatal("sent while disconnected")
	case <-time.After(30 * time.Millisecond):
	}
	tr.SetConnected(true)
	select {
	case got := <-received:
		if !strings.Contains(got, "bmV3") {
			t.Fatalf("wrong packet: %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no packet after recovery")
	}
	if tr.queueDrops.Load() != 1 {
		t.Fatal("expired packet not counted")
	}
}

func TestFailedWriteDisconnectsSession(t *testing.T) {
	s, _ := controlPeer(t)
	s.Conn.Close()
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	tr.BaseTransport.Start()
	tr.session = s
	tr.SetConnected(true)
	done := make(chan struct{})
	go func() { tr.writerLoop(); close(done) }()
	defer func() { tr.Stop(); <-done }()
	if err := tr.Send([]byte("pending")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for tr.IsConnected() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if tr.IsConnected() {
		t.Fatal("failed socket still reported ready")
	}
	if tr.writeErrors.Load() != 1 {
		t.Fatal("writer repeatedly used failed socket")
	}
}

func TestTransportWaitsForEditorAuth(t *testing.T) {
	authSeen := make(chan struct{})
	releaseAuth := make(chan struct{})
	serverDone := make(chan struct{})
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/edit", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"api": "ws" + strings.TrimPrefix(srv.URL, "http"), "token": "test", "document": map[string]interface{}{"key": "test", "fileType": "docx"}, "editorConfig": map[string]interface{}{"user": map[string]string{"id": "test"}}})
	})
	mux.HandleFunc("/doc/test/c/", func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		defer close(serverDone)
		c.WriteMessage(websocket.TextMessage, []byte(`0{"sid":"test","pingInterval":25000,"pingTimeout":20000}`))
		if _, _, err = c.ReadMessage(); err != nil {
			return
		}
		c.WriteMessage(websocket.TextMessage, []byte(`40{"sid":"test"}`))
		if _, _, err = c.ReadMessage(); err != nil {
			return
		}
		close(authSeen)
		<-releaseAuth
		c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"auth","result":1}]`))
		for {
			if _, _, err = c.ReadMessage(); err != nil {
				return
			}
		}
	})
	old := editAPI
	editAPI = srv.URL + "/edit"
	defer func() { editAPI = old }()
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	tr.Start()
	defer func() {
		tr.Stop()
		select {
		case <-serverDone:
		case <-time.After(time.Second):
			t.Error("reader did not stop")
		}
	}()
	select {
	case <-authSeen:
	case <-time.After(2 * time.Second):
		close(releaseAuth)
		t.Fatal("no auth request")
	}
	if tr.IsConnected() {
		close(releaseAuth)
		t.Fatal("reported ready before editor auth")
	}
	close(releaseAuth)
	deadline := time.Now().Add(time.Second)
	for !tr.IsConnected() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !tr.IsConnected() {
		t.Fatal("not ready after editor auth")
	}
	if err := tr.Send(bytes.Repeat([]byte{7}, 1500)); err != nil {
		t.Fatal(err)
	}
}

func TestStopCancelsHTTPBootstrap(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer srv.Close()
	old := editAPI
	editAPI = srv.URL
	defer func() { editAPI = old }()
	tr := NewMailruDocsTransport("test/doc", transport.DefaultConfig())
	defer tr.Stop()
	if err := tr.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("bootstrap did not start")
	}
	tr.Stop()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP bootstrap survived Stop")
	}
	if err := tr.Start(); err == nil {
		t.Fatal("restarted a stopped transport")
	}
}
