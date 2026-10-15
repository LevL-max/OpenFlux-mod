package mailru

import (
	"bytes"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

// Opt-in only. Writes tagged synthetic data to the supplied document's chat.
// Never put the document URL in source, logs or CI configuration.
func TestLiveChatExchangeAndReconnect(t *testing.T) {
	doc := os.Getenv("OPENFLUX_MAILRU_TEST_DOCUMENT")
	if doc == "" {
		t.Skip("set OPENFLUX_MAILRU_TEST_DOCUMENT to a disposable public document")
	}
	peers := []*MailruDocsTransport{
		NewMailruDocsTransport(doc, transport.DefaultConfig()),
		NewMailruDocsTransport(doc, transport.DefaultConfig()),
	}
	channels := []chan []byte{make(chan []byte, 128), make(chan []byte, 128)}
	for i, peer := range peers {
		ch := channels[i]
		peer.Receive(func(b []byte) {
			select {
			case ch <- append([]byte(nil), b...):
			default:
			}
		})
		if err := peer.Start(); err != nil {
			t.Fatal(err)
		}
		defer peer.Stop()
	}
	waitConnected := func() {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if peers[0].IsConnected() && peers[1].IsConnected() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("peers not ready: %t/%t", peers[0].IsConnected(), peers[1].IsConnected())
	}
	waitConnected()
	exchange := func(from, size int) {
		t.Helper()
		packet := make([]byte, size)
		if _, err := rand.Read(packet); err != nil {
			t.Fatal(err)
		}
		deadline := time.NewTimer(60 * time.Second)
		defer deadline.Stop()
		retry := time.NewTicker(time.Second)
		defer retry.Stop()
		start := time.Now()
		_ = peers[from].Send(packet)
		for {
			select {
			case got := <-channels[1-from]:
				if bytes.Equal(got, packet) {
					t.Logf("direction=%d bytes=%d elapsed=%s", from, size, time.Since(start).Round(time.Millisecond))
					return
				}
			case got := <-channels[from]:
				if bytes.Equal(got, packet) {
					t.Fatal("own chat echo delivered")
				}
			case <-retry.C:
				_ = peers[from].Send(packet)
			case <-deadline.C:
				t.Fatalf("direction %d: no intact %d-byte packet", from, size)
			}
		}
	}
	for _, size := range []int{1500, 32768, 500000} {
		for from := 0; from < 2; from++ {
			exchange(from, size)
		}
	}
	peers[0].Mu.RLock()
	old := peers[0].session
	peers[0].Mu.RUnlock()
	peers[0].dropConnection(old)
	waitConnected()
	for i := 0; i < 16; i++ {
		exchange(i%2, 128000)
	}
	if peers[0].Stats().Reconnects == 0 {
		t.Fatal("forced reconnect not observed")
	}
	t.Logf("reconnects=%d/%d", peers[0].Stats().Reconnects, peers[1].Stats().Reconnects)
}
