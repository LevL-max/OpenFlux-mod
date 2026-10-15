package mailru

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

func chatBroadcast(sender string, packets ...[]byte) []byte {
	entries := []map[string]string{}
	for _, packet := range packets {
		entries = append(entries, map[string]string{"message": chatDataTag + base64.StdEncoding.EncodeToString(packet), "useridoriginal": sender})
	}
	b, _ := json.Marshal([]any{"message", map[string]any{"type": "message", "messages": entries}})
	return append([]byte("42"), b...)
}

func TestChatDecodeFiltersEchoHistoryAndMalformedFrames(t *testing.T) {
	packet := []byte{0, 1, 255, 10, 13}
	for _, tc := range []struct {
		name  string
		frame []byte
		want  [][]byte
	}{
		{"binary", chatBroadcast("peer", packet), [][]byte{packet}},
		{"multiple", chatBroadcast("peer", []byte("a"), packet), [][]byte{[]byte("a"), packet}},
		{"own editor id", chatBroadcast("self", packet), nil},
		{"own fallback id", chatBroadcast("fallback", packet), nil},
		{"history", []byte(`42["message",{"type":"auth","messages":[{"message":"OFX1YWJj"}]}]`), nil},
		{"wrong envelope", []byte(`42["other",{"type":"message","messages":[{"message":"OFX1YWJj"}]}]`), nil},
		{"human and bad data", []byte(`42["message",{"type":"message","messages":[{"message":"hello"},{"message":"OFX1!"},{"message":"OFX1"}]}]`), nil},
		{"cursor", []byte(`42["message",{"type":"cursor","messages":[{"cursor":"3;YWJj"}]}]`), nil},
		{"malformed", []byte(`42["message",`), nil},
		{"wrong packet", []byte(`43["message",{"type":"message","messages":[{"message":"OFX1YWJj"}]}]`), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeChatData(tc.frame, "self", "fallback"); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestChatHandlerPreservesOrderAndFiltersSelf(t *testing.T) {
	tr := NewMailruDocsTransport("unused/doc", transport.DefaultConfig())
	defer tr.Stop()
	s := &DocSession{Info: MailruDocsInfo{EditorUserID: "self"}, UserID: "fallback"}
	var got [][]byte
	tr.Receive(func(packet []byte) { got = append(got, packet) })
	tr.handleMessage(s, chatBroadcast("self", []byte("echo")))
	tr.handleMessage(s, chatBroadcast("peer", []byte("first"), []byte("second")))
	if !reflect.DeepEqual(got, [][]byte{[]byte("first"), []byte("second")}) {
		t.Fatalf("got %q", got)
	}
	if tr.Stats().PacketsRecv != 2 {
		t.Fatal("receive accounting")
	}
}

func TestChatWriterWaitsForAckOrGracePeriod(t *testing.T) {
	for _, ack := range []bool{true, false} {
		t.Run(map[bool]string{true: "ack", false: "fallback"}[ack], func(t *testing.T) {
			s, received := controlPeer(t)
			s.chatReadyAfter = time.Now().Add(150 * time.Millisecond)
			tr := NewMailruDocsTransport("unused/doc", transport.DefaultConfig())
			tr.BaseTransport.Start()
			tr.session = s
			tr.SetConnected(true)
			done := make(chan struct{})
			go func() { tr.writerLoop(); close(done) }()
			defer func() { tr.Stop(); <-done }()
			tr.Send([]byte("queued"))
			select {
			case <-received:
				t.Fatal("write before readiness")
			case <-time.After(40 * time.Millisecond):
			}
			if ack {
				if _, err := handleMailruControl(s, []byte(`42["message",{"type":"authChanges"}]`)); err != nil {
					t.Fatal(err)
				}
				if got := <-received; got != `42["message",{"type":"authChangesAck"}]` {
					t.Fatalf("ack order: %s", got)
				}
			}
			select {
			case got := <-received:
				if !bytes.Equal([]byte(got), encodeChatData([]byte("queued"))) {
					t.Fatal("wrong data")
				}
			case <-time.After(time.Second):
				t.Fatal("writer stayed blocked")
			}
		})
	}
}

func TestFailedAckDoesNotReleaseChatWriter(t *testing.T) {
	s, _ := controlPeer(t)
	s.Conn.Close()
	if _, err := handleMailruControl(s, []byte(`42["message",{"type":"authChanges"}]`)); err == nil {
		t.Fatal("expected closed socket")
	}
	if s.chatReady.Load() {
		t.Fatal("failed ack made session ready")
	}
}
