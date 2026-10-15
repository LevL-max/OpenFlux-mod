package mailru

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

// End-to-end regression without an external service: real WebSockets, editor
// auth, echoing relay, OFB1/LZ4 and gVisor TCP, including socket replacement.
func TestChatTCPThroughRelayAndReconnect(t *testing.T) {
	type client struct {
		conn *websocket.Conn
		mu   sync.Mutex
	}
	var mu sync.Mutex
	clients := map[*client]bool{}
	write := func(c *client, data []byte) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.conn.WriteMessage(websocket.TextMessage, data)
	}
	var sequence atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/edit", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"api": "ws" + strings.TrimPrefix(srv.URL, "http"), "token": "test", "document": map[string]string{"key": "test", "fileType": "docx"}, "editorConfig": map[string]any{"user": map[string]string{"id": fmt.Sprintf("user%d", sequence.Add(1))}}})
	})
	mux.HandleFunc("/doc/test/c/", func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := &client{conn: ws}
		defer ws.Close()
		defer func() { mu.Lock(); delete(clients, c); mu.Unlock() }()
		write(c, []byte(`0{"sid":"test","pingInterval":25000,"pingTimeout":20000}`))
		if _, _, err = ws.ReadMessage(); err != nil {
			return
		}
		write(c, []byte(`40{"sid":"test"}`))
		_, auth, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var parts []json.RawMessage
		var body struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		}
		if json.Unmarshal(auth[2:], &parts) != nil || len(parts) != 2 || json.Unmarshal(parts[1], &body) != nil {
			t.Error("invalid editor auth")
			return
		}
		write(c, []byte(`42["message",{"type":"authChanges"}]`))
		_, ack, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if string(ack) != `42["message",{"type":"authChangesAck"}]` {
			t.Error("data before editor readiness")
			return
		}
		mu.Lock()
		clients[c] = true
		mu.Unlock()
		write(c, []byte(`42["message",{"type":"auth","result":1}]`))
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if string(data) == "3" {
				continue
			}
			var event struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			if len(data) < 2 || json.Unmarshal(data[2:], &parts) != nil || len(parts) != 2 || json.Unmarshal(parts[1], &event) != nil {
				t.Error("bad chat frame")
				return
			}
			if event.Type != "message" {
				t.Errorf("unexpected synthetic editor write %s", event.Type)
				return
			}
			broadcast, _ := json.Marshal([]any{"message", map[string]any{"type": "message", "messages": []map[string]string{{"message": event.Message, "useridoriginal": body.User.ID}}}})
			mu.Lock()
			targets := make([]*client, 0, len(clients))
			for target := range clients {
				targets = append(targets, target)
			}
			mu.Unlock()
			for _, target := range targets {
				write(target, append([]byte("42"), broadcast...))
			}
		}
	})
	old := editAPI
	editAPI = srv.URL + "/edit"
	defer func() { editAPI = old }()
	// A subtest finishes its cleanup before the fake API is restored.
	t.Run("TCP", func(t *testing.T) { runChatTCPProbe(t, "test/doc") })
}
