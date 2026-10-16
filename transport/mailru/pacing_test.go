package mailru

import (
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

func TestChatPacingKeepsControlResponsive(t *testing.T) {
	s, received := controlPeer(t)
	tr := NewMailruDocsTransport("test/document", transport.DefaultConfig())
	// The byte budget, rather than the minimum interval, must govern this packet.
	if err := tr.SetChatPacing(20*time.Millisecond, 4096); err != nil {
		t.Fatal(err)
	}
	tr.BaseTransport.Start()
	tr.session = s
	tr.SetConnected(true)
	done := make(chan struct{})
	go func() { tr.writerLoop(); close(done) }()
	defer func() { tr.Stop(); <-done }()
	payload := make([]byte, 1500)
	tr.Send(payload)
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("first packet missing")
	}
	first := time.Now()
	tr.Send(payload)
	// A heartbeat must bypass the waiting chat writer, including its socket lock.
	if _, err := handleMailruControl(s, []byte("2")); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-received:
		if msg != "3" {
			t.Fatal("chat overtook heartbeat")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("pacing blocked heartbeat")
	}
	select {
	case <-received:
		if time.Since(first) < 400*time.Millisecond {
			t.Fatal("byte budget not enforced")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("paced packet missing")
	}
	tr.Send(payload)
	tr.Stop()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("stop did not cancel pacing")
	}
}

func TestChatPacingBounds(t *testing.T) {
	tr := NewMailruDocsTransport("test/document", transport.DefaultConfig())
	for _, budget := range []int{-1, 1, 4095, (10 << 20) + 1} {
		if tr.SetChatPacing(0, budget) == nil {
			t.Fatal("accepted invalid budget")
		}
	}
	if tr.SetChatPacing(100*time.Millisecond, 4096) != nil {
		t.Fatal("valid pacing rejected")
	}
	if got := tr.chatSpacing(1); got != 100*time.Millisecond {
		t.Fatalf("minimum interval: %v", got)
	}
	if got := tr.chatSpacing(4096); got != time.Second {
		t.Fatalf("byte interval: %v", got)
	}
	tr.BaseTransport.Start()
	defer tr.Stop()
	if tr.SetChatPacing(0, 0) == nil {
		t.Fatal("accepted configuration while running")
	}
}
