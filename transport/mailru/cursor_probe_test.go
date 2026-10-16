package mailru

import (
	"testing"
	"time"
	"universal-bypass-tool/transport"
)

func TestCursorControlWireAndMultipleEntries(t *testing.T) {
	s, received := controlPeer(t)
	tr := NewMailruDocsTransport("test/document", transport.DefaultConfig())
	tr.SetCursorProbe(true)
	tr.BaseTransport.Start()
	tr.session = s
	tr.SetConnected(true)
	done := make(chan struct{})
	go func() { tr.writerLoop(); close(done) }()
	defer func() { tr.Stop(); <-done }()
	tr.Send([]byte{1, 2, 3})
	select {
	case msg := <-received:
		if msg != `42["message",{"type":"cursor","cursor":"3;AQID"}]` {
			t.Fatal("incorrect cursor wire")
		}
	case <-time.After(time.Second):
		t.Fatal("missing cursor write")
	}
	var got [][]byte
	tr.Receive(func(p []byte) { got = append(got, p) })
	tr.handleMessage(s, []byte(`42["message",{"type":"cursor","messages":[{"cursor":"18;---KA---"},{"cursor":"3;AQID"},{"cursor":"99;AQID"},{"cursor":"1;BA=="}]}]`))
	if len(got) != 2 || len(got[0]) != 3 || got[1][0] != 4 {
		t.Fatal("batched cursor decode failed")
	}
	if len(decodeProbeCursors([]byte(`42["message",{"type":"auth","messages":[{"cursor":"3;AQID"}]}]`))) != 0 {
		t.Fatal("accepted history as live traffic")
	}
}

func TestCursorControlRejectsChatRenewal(t *testing.T) {
	tr := NewMailruDocsTransport("test/document", transport.DefaultConfig())
	defer tr.Stop()
	tr.SetCursorProbe(true)
	tr.SetChatSessionBudget(65536)
	if tr.Start() == nil || tr.IsRunning() {
		t.Fatal("mixed experiment accepted")
	}
}
