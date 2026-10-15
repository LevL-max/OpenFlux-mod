package mailru

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"universal-bypass-tool/transport"
)

// Every write is a tagged chat message, including large coalesced frames.
func TestWriterUsesChat(t *testing.T) {
	received := make(chan string, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			received <- string(msg)
		}
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	tr := NewMailruDocsTransport("AbCd/EfGh", transport.DefaultConfig())
	if err := tr.BaseTransport.Start(); err != nil {
		t.Fatal(err)
	}
	defer tr.Stop()
	tr.session = &DocSession{Conn: conn, WriteQueue: make(chan queuedPacket, 16)}
	tr.SetConnected(true)
	go tr.writerLoop()

	for _, size := range []int{1, 18, 1200, 3000, 500000} {
		packet := make([]byte, size)
		for i := range packet {
			packet[i] = byte(i)
		}
		if err := tr.Send(packet); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf(`42["message",{"type":"message","message":"OFX1%s"}]`, base64.StdEncoding.EncodeToString(packet))
		select {
		case got := <-received:
			if got != want {
				t.Fatalf("size %d: got %.80q", size, got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("size %d: timed out", size)
		}
	}
}

func TestNormalizeWeblink(t *testing.T) {
	for in, want := range map[string]string{
		"https://cloud.mail.ru/public/AbCd/EfGh":  "AbCd/EfGh",
		"https://cloud.mail.ru/public/AbCd/EfGh/": "AbCd/EfGh",
		" AbCd/EfGh ": "AbCd/EfGh",
	} {
		if got := normalizeWeblink(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
