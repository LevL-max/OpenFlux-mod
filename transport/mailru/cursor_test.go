package mailru

import (
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

// A scheduled reconnect that is already waiting makes a second one a no-op.
func TestSecondReconnectWhileOneIsWaitingReturnsAtOnce(t *testing.T) {
	tr := NewMailruDocsTransport("AbCdEfGh1/IjKlMnOp2", transport.DefaultConfig())
	if err := tr.BaseTransport.Start(); err != nil {
		t.Fatal(err)
	}
	defer tr.BaseTransport.Stop()
	tr.reconnecting.Store(true)
	done := make(chan struct{})
	go func() { tr.scheduleReconnect(0); close(done) }()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("a second scheduled reconnect waited out its own backoff instead of yielding")
	}
}
