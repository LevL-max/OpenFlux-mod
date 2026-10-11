package yandex

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"universal-bypass-tool/transport"
)

func TestCursorWireSizeMatchesSerialization(t *testing.T) {
	for _, size := range []int{0, 1, 9, 10, 99, 100, 1501, 1914, 1915, 6023, 32768} {
		if got, want := cursorWireSize(size), len(cursorMessage(make([]byte, size))); got != want {
			t.Fatalf("size %d: %d != %d", size, got, want)
		}
	}
}

func TestCursorLimitRejectsOversizeBeforeQueue(t *testing.T) {
	tr, _ := writerPeer(t)
	tr.SetCursorSafety(2600, 0)
	if err := tr.Send(make([]byte, 1914)); err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(make([]byte, 1915)); err == nil {
		t.Fatal("oversize accepted")
	}
}

func TestAllCursorEntriesSurviveKeepalive(t *testing.T) {
	tr := NewYandexDocsTransport("test", transport.DefaultConfig())
	var got []string
	tr.Receive(func(b []byte) { got = append(got, string(b)) })
	tr.handleMessage(&DocSession{}, []byte(`42["message",{"type":"cursor","messages":[{"cursor":"1;YQ=="},{"cursor":"18;---KA---"},{"cursor":"1;Yg=="}]}]`))
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("delivered %v", got)
	}
}

func TestSessionPacesDataAndHeartbeatTogether(t *testing.T) {
	times := make(chan time.Time, 10)
	session := handshakePeer(t, func(c *websocket.Conn) {
		for i := 0; i < 5; i++ {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
			times <- time.Now()
		}
	})
	session.writeEvery = 30 * time.Millisecond
	for _, msg := range []string{cursorMessage(bytes.Repeat([]byte{1}, 1501)), "3", cursorMessage([]byte{1}), `42["message",{"type":"cursor","cursor":"18;---KA---"}]`, "3"} {
		if err := session.safeWrite(websocket.TextMessage, []byte(msg)); err != nil {
			t.Fatal(err)
		}
	}
	var first, last time.Time
	for i := 0; i < 5; i++ {
		select {
		case at := <-times:
			if i == 0 {
				first = at
			}
			last = at
		case <-time.After(time.Second):
			t.Fatal(fmt.Sprintf("only %d writes", i))
		}
	}
	if last.Sub(first) < 110*time.Millisecond {
		t.Fatalf("burst across data/control: %v", last.Sub(first))
	}
}
