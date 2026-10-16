package mailru

import (
	"encoding/json"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

func TestRenewalPreservesPendingPacketAndWaitsForEcho(t *testing.T) {
	s, received := controlPeer(t)
	s.Info.Authenticated = true
	tr := NewMailruDocsTransport("test/document", transport.DefaultConfig())
	if err := tr.SetChatSessionBudget(65536); err != nil {
		t.Fatal(err)
	}
	tr.BaseTransport.Start()
	tr.session = s
	tr.SetConnected(true)
	done := make(chan struct{})
	go func() { tr.writerLoop(); close(done) }()
	defer func() { tr.Stop(); <-done }()
	payload := make([]byte, 32768)
	tr.Send(payload)
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("first packet missing")
	}
	tr.Send([]byte(string(payload) + "second"))
	select {
	case <-received:
		t.Fatal("session budget exceeded")
	case <-time.After(30 * time.Millisecond):
	}
	if !tr.IsConnected() {
		t.Fatal("renewed before echo")
	}
	// Only a matching live self echo acknowledges a write, not arbitrary chat.
	frame, _ := json.Marshal([]any{"message", map[string]any{"type": "message", "messages": []any{map[string]string{"message": chatDataText(payload, tr.senderID)}}}})
	tr.handleMessage(s, append([]byte("42"), frame...))
	deadline := time.Now().Add(time.Second)
	for tr.IsConnected() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if tr.IsConnected() {
		t.Fatal("did not renew after echo")
	}
	if s.trace.snapshot(0).Cause != "session_budget" {
		t.Fatal("renewal cause missing")
	}
	next, nextReceived := controlPeer(t)
	next.Info.Authenticated = true
	next.WriteQueue = s.WriteQueue
	tr.Mu.Lock()
	tr.session = next
	tr.SetConnected(true)
	tr.Mu.Unlock()
	select {
	case msg := <-nextReceived:
		if msg != string(encodeChatData([]byte(string(payload)+"second"), tr.senderID)) {
			t.Fatal("pending packet changed")
		}
	case <-time.After(time.Second):
		t.Fatal("renewal lost pending packet")
	}
}

func TestChatEchoWaitCancellationAndDuplicates(t *testing.T) {
	e := &chatEchoes{}
	e.expect("one")
	e.expect("one")
	e.observe("unrelated")
	e.observe("one")
	done := make(chan struct{})
	result := make(chan bool, 1)
	go func() { result <- e.wait(done, time.Second) }()
	select {
	case <-result:
		t.Fatal("one echo acknowledged duplicate writes")
	case <-time.After(20 * time.Millisecond):
	}
	close(done)
	if <-result {
		t.Fatal("stop did not cancel drain")
	}
	e.observe("one")
	if !e.wait(make(chan struct{}), time.Millisecond) {
		t.Fatal("all echoes not acknowledged")
	}
}
