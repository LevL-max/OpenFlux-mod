package yandex

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestExistingParticipantUnlocksFirstJoin(t *testing.T) {
	ready := make(chan struct{})
	existing := handshakePeer(t, func(c *websocket.Conn) {
		c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"connectState","waitAuth":true}]`))
		_, ack, err := c.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		var parts []json.RawMessage
		var command map[string]interface{}
		if !strings.HasPrefix(string(ack), "42") || json.Unmarshal(ack[2:], &parts) != nil || len(parts) != 2 || json.Unmarshal(parts[1], &command) != nil {
			t.Errorf("Invalid co-editing acknowledgment: %s", ack)
			return
		}
		if command["type"] != "unLockDocument" || command["unlock"] != true || command["isSave"] != false || command["releaseLocks"] != false || len(command) != 4 {
			t.Errorf("Acknowledgment must release only the authentication lock: %v", command)
			return
		}
		close(ready)
		c.ReadMessage() // Keep the existing participant connected until cleanup.
	})
	joining := handshakePeer(t, func(c *websocket.Conn) {
		c.WriteMessage(websocket.TextMessage, []byte(`0{"sid":"joining"}`))
		c.ReadMessage()
		c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"license"}]`))
		c.ReadMessage()
		c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"waitAuth"}]`))
		select {
		case <-ready:
			c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"auth","result":1}]`))
		case <-time.After(time.Second):
			t.Error("Existing participant did not release the authentication lock")
		}
	})
	_, event, err := existing.Conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	(&YandexDocsTransport{}).handleMessage(existing, event)
	if err := authenticateDoc(joining, time.Second); err != nil {
		t.Fatalf("First join failed: %v", err)
	}
}

func TestCoeditingAcknowledgmentRequiresWaitAuth(t *testing.T) {
	for _, event := range []string{
		`42["message",{"type":"connectState","waitAuth":false}]`,
		`42["message",{"type":"connectState"}]`,
		`42["other",{"type":"connectState","waitAuth":true}]`,
		`42["message",{"type":"cursor","waitAuth":true}]`,
		`42invalid`,
	} {
		// A nil session would panic if any of these frames attempted a write.
		if _, err := acknowledgeCoediting(nil, []byte(event)); err != nil {
			t.Fatal(err)
		}
	}
}

func handshakePeer(t *testing.T, serve func(*websocket.Conn)) *DocSession {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		serve(c)
	}))
	t.Cleanup(server.Close)
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &DocSession{Conn: c, Info: YandexDocsInfo{Token: "test-token", DocID: "test-doc"}, UserID: "test-user"}
}

func TestHandshakeIgnoresBusyDocumentAndKeepsAuthenticatedSocket(t *testing.T) {
	session := handshakePeer(t, func(c *websocket.Conn) {
		c.WriteMessage(websocket.TextMessage, []byte(`0{"sid":"test"}`))
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
		for i := 0; i < 15; i++ {
			c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"cursor"}]`))
		}
		c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"license"}]`))
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
		for i := 0; i < 30; i++ {
			c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"cursor"}]`))
			c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"authChanges"}]`))
			_, ack, err := c.ReadMessage()
			if err != nil {
				return
			}
			if !strings.Contains(string(ack), "authChangesAck") {
				t.Errorf("Missing changes acknowledgment")
				return
			}
		}
		c.WriteMessage(websocket.TextMessage, []byte("2"))
		_, pong, err := c.ReadMessage()
		if err != nil {
			return
		}
		if string(pong) != "3" {
			t.Errorf("Missing heartbeat response")
			return
		}
		c.WriteMessage(websocket.TextMessage, []byte(`42["message", {"type": "auth", "result": 1}]`))
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
		c.WriteMessage(websocket.TextMessage, []byte("still-open"))
	})
	if err := authenticateDoc(session, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := session.safeWrite(websocket.TextMessage, []byte("after-auth")); err != nil {
		t.Fatal(err)
	}
	_, reply, err := session.Conn.ReadMessage()
	if err != nil || string(reply) != "still-open" {
		t.Fatalf("Authenticated socket was not retained: %q %v", reply, err)
	}
}

func TestFailedHandshakeClosesSocketAndDistinguishesRejection(t *testing.T) {
	for _, kind := range []string{"timeout", "rejected"} {
		t.Run(kind, func(t *testing.T) {
			closed := make(chan bool, 1)
			session := handshakePeer(t, func(c *websocket.Conn) {
				c.WriteMessage(websocket.TextMessage, []byte(`0{"sid":"test"}`))
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
				c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"license"}]`))
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
				if kind == "rejected" {
					c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"auth","result":10}]`))
				}
				_, _, err := c.ReadMessage()
				closed <- err != nil
			})
			err := authenticateDoc(session, 100*time.Millisecond)
			if err == nil {
				t.Fatal("Expected failed handshake")
			}
			if errors.Is(err, errDocumentAuthRejected) != (kind == "rejected") {
				t.Fatalf("Wrong failure classification: %v", err)
			}
			select {
			case ok := <-closed:
				if !ok {
					t.Fatal("Failed socket stayed open")
				}
			case <-time.After(time.Second):
				t.Fatal("Failed attempt leaked its socket")
			}
		})
	}
}
