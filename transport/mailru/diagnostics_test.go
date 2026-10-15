package mailru

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestSessionDiagnosticsEmptyPeerClose(t *testing.T) {
	serverDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		c.WriteMessage(websocket.TextMessage, []byte(`0{"sid":"private-session","pingInterval":25000,"pingTimeout":20000}`))
		if _, _, err = c.ReadMessage(); err != nil {
			return
		}
		c.WriteMessage(websocket.TextMessage, []byte(`40{"sid":"private-session"}`))
		c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"auth","result":1}]`))
		c.WriteMessage(websocket.TextMessage, []byte(`2`))
		_, pong, err := c.ReadMessage()
		if err != nil || string(pong) != "3" {
			t.Error("heartbeat reply missing")
			return
		}
		c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"warning","code":17,"description":"private-cookie"}]`))
		c.WriteMessage(websocket.TextMessage, []byte(`41`))
		// A real empty WebSocket close frame is the observed 1005 failure.
		c.WriteControl(websocket.CloseMessage, nil, time.Now().Add(time.Second))
	}))
	defer srv.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := &DocSession{Conn: c}
	s.trace.start(1)
	if err = waitMailruSocketIO(s, "private-token"); err != nil {
		t.Fatal(err)
	}
	s.trace.phase("editor_auth")
	if err = waitMailruEditorAuth(s); err != nil {
		t.Fatal(err)
	}
	s.trace.phase("ready")
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, data, err := s.readMessage()
		if err != nil {
			break
		}
		if _, err := handleMailruControl(s, data); err != nil {
			t.Fatal(err)
		}
	}
	<-serverDone
	r := s.trace.snapshot(s.heartbeatTimeout)
	if r.Cause != "read" || r.Error != "websocket_close" || r.CloseCode != 1005 || r.Phase != "ready" {
		t.Fatalf("incorrect close attribution: %+v", r)
	}
	if r.Pings != 1 || r.Pongs != 1 || r.TerminalControl != "socket_disconnect" || r.HeartbeatMS != 50000 || r.LastPingAgoMS < 0 {
		t.Fatalf("missing control context: %+v", r)
	}
	if r.EditorEvent != "warning" || r.EditorCode == nil || *r.EditorCode != 17 || r.RXMessages != 6 || r.TXMessages != 2 {
		t.Fatalf("missing session metadata: %+v", r)
	}
	// One report per socket, including two failures less than a minute apart.
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	s.reportClosed()
	s.reportClosed()
	s2 := &DocSession{}
	s2.trace.start(2)
	s2.trace.fail("read", &websocket.CloseError{Code: 1008, Text: "private-cookie"})
	s2.reportClosed()
	if strings.Count(output.String(), "[M-SESSION]") != 2 || strings.Contains(output.String(), "private-") {
		t.Fatalf("duplicate, suppressed or unsafe report: %s", output.String())
	}
}

func TestSessionDiagnosticsTimeoutAndLocalClose(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		s, _ := controlPeer(t)
		s.trace.start(1)
		s.Conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		_, _, err := s.readMessage()
		if err == nil {
			t.Fatal("expected idle socket deadline")
		}
		r := s.trace.snapshot(0)
		if r.Error != "timeout" || r.Cause != "read" || r.CloseCode != 0 || r.LastPingAgoMS != -1 {
			t.Fatalf("timeout mistaken for provider close: %+v", r)
		}
	})
	for _, reason := range []string{"stop", "profile_changed", "document_key_changed"} {
		t.Run(reason, func(t *testing.T) {
			s, _ := controlPeer(t)
			s.trace.start(1)
			s.closeWithReason(reason)
			s.readMessage()
			r := s.trace.snapshot(0)
			if r.Cause != reason || r.Error != "" {
				t.Fatalf("local close mistaken for transport failure: %+v", r)
			}
		})
	}
}

func TestSessionDiagnosticsDoesNotRetainServerText(t *testing.T) {
	var d sessionTrace
	d.start(1)
	for _, frame := range []string{
		`44{"message":"private-token"}`,
		`42["message",{"type":"private-name","code":42}]`,
		`42["message",{"type":"message","messages":[{"message":"private-chat"}]}]`,
		`42["message",{"type":"warning","code":"private-code"}]`,
	} {
		d.received([]byte(frame))
	}
	d.fail("read", errors.New("private-cookie https://private-doc"))
	r := d.snapshot(0)
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), "private-") || r.EditorCode != nil || r.TerminalControl != "socket_connect_error" {
		t.Fatalf("unsafe metadata: %s", encoded)
	}
}

func TestSessionDiagnosticsConcurrentWriterAndReader(t *testing.T) {
	var d sessionTrace
	d.start(1)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 1000 {
			d.wrote([]byte("3"), 2*time.Millisecond, time.Millisecond, nil)
		}
		d.wrote([]byte("not retained"), 0, 0, errors.New("private-error"))
	})
	wg.Go(func() {
		for range 1000 {
			d.received([]byte("2"))
			d.snapshot(0)
		}
	})
	wg.Wait()
	d.fail("read", &websocket.CloseError{Code: 1006})
	r := d.snapshot(0)
	if r.Pings != 1000 || r.Pongs != 1000 || r.TXBytes != 1000 || r.MaxPongMS != 3 || r.Cause != "write" {
		t.Fatalf("lost counters or initiating cause: %+v", r)
	}
}
