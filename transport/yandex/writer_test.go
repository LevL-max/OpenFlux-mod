package yandex

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"sort"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"universal-bypass-tool/transport"
)

// writerPeer returns a connected transport whose writer sends to a test
// WebSocket server, and the messages that server receives.
func writerPeer(t testing.TB) (*YandexDocsTransport, <-chan string) {
	t.Helper()
	received := make(chan string, 1024)
	session := handshakePeer(t, func(c *websocket.Conn) {
		c.SetReadDeadline(time.Time{})
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			received <- string(msg)
		}
	})
	session.WriteQueue = make(chan []byte, 1024)
	tr := NewYandexDocsTransport("https://disk.yandex.ru/i/test", transport.DefaultConfig())
	tr.BaseTransport.Start()
	tr.session = session
	tr.SetConnected(true)
	t.Cleanup(func() { tr.Stop() })
	go tr.writerLoop()
	return tr, received
}

func cursorMessage(packet []byte) string {
	return fmt.Sprintf(`42["message",{"type":"cursor","cursor":"18;%s"}]`, base64.StdEncoding.EncodeToString(packet))
}

func expectMessage(t *testing.T, received <-chan string, want string) {
	t.Helper()
	select {
	case got := <-received:
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
	}
}

func TestWriterSendsQueuedPacketsInOrder(t *testing.T) {
	tr, received := writerPeer(t)
	for i := 0; i < 50; i++ {
		if err := tr.Send([]byte{byte(i), 1, 2, 3}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 50; i++ {
		expectMessage(t, received, cursorMessage([]byte{byte(i), 1, 2, 3}))
	}
}

func TestWriterHoldsPacketUntilReconnected(t *testing.T) {
	tr, received := writerPeer(t)
	tr.SetConnected(false)
	tr.session.WriteQueue <- []byte("held")
	select {
	case got := <-received:
		t.Fatalf("sent %q while disconnected", got)
	case <-time.After(100 * time.Millisecond):
	}
	tr.SetConnected(true)
	expectMessage(t, received, cursorMessage([]byte("held")))
}

func TestWriterExitsAfterStop(t *testing.T) {
	tr := NewYandexDocsTransport("https://disk.yandex.ru/i/test", transport.DefaultConfig())
	tr.BaseTransport.Start()
	tr.session = &DocSession{WriteQueue: make(chan []byte, 1)}
	done := make(chan struct{})
	go func() {
		tr.writerLoop()
		close(done)
	}()
	tr.Stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not exit after Stop")
	}
}

func TestWriterDropsAConnectionThatStopsTakingData(t *testing.T) {
	old := docWriteTimeout
	docWriteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { docWriteTimeout = old })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	// The peer never reads: the socket buffers fill and the next write blocks,
	// as on a half-open connection.
	session := handshakePeer(t, func(*websocket.Conn) { <-release })
	session.WriteQueue = make(chan []byte, 1024)
	tr := NewYandexDocsTransport("https://disk.yandex.ru/i/test", transport.DefaultConfig())
	tr.BaseTransport.Start()
	tr.session = session
	tr.SetConnected(true)
	t.Cleanup(func() { tr.Stop() })
	go tr.writerLoop()
	packet := bytes.Repeat([]byte{1}, 32<<10)
	for deadline := time.Now().Add(10 * time.Second); tr.IsConnected(); {
		if time.Now().After(deadline) {
			t.Fatal("the writer kept a connection whose peer stopped taking data")
		}
		select {
		case session.WriteQueue <- packet:
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// The connection is closed, so a reader blocked on it fails at once.
	if _, _, err := session.Conn.ReadMessage(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read after the drop: %v, want a closed connection", err)
	}
}

func TestKeepAliveFailureDropsTheConnection(t *testing.T) {
	session := handshakePeer(t, func(c *websocket.Conn) { c.ReadMessage() })
	cfg := transport.DefaultConfig()
	cfg.KeepAliveInterval = 20 * time.Millisecond
	tr := NewYandexDocsTransport("https://disk.yandex.ru/i/test", cfg)
	tr.BaseTransport.Start()
	tr.session = session
	tr.SetConnected(true)
	t.Cleanup(func() { tr.Stop() })
	session.Conn.UnderlyingConn().Close() // every write now fails
	go tr.keepAliveLoop()
	for deadline := time.Now().Add(5 * time.Second); tr.IsConnected(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a failed keep-alive left the connection up")
		}
	}
}

// BenchmarkDocWrite measures one cursor message with and without the write
// deadline: the per-packet cost of bounding writes.
func BenchmarkDocWrite(b *testing.B) {
	for _, timeout := range []time.Duration{0, 20 * time.Second} {
		b.Run(fmt.Sprintf("deadline=%v", timeout), func(b *testing.B) {
			old := docWriteTimeout
			docWriteTimeout = timeout
			defer func() { docWriteTimeout = old }()
			session := handshakePeer(b, func(c *websocket.Conn) {
				c.SetReadDeadline(time.Time{})
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						return
					}
				}
			})
			msg := []byte(cursorMessage(bytes.Repeat([]byte{7}, 1200)))
			b.SetBytes(int64(len(msg)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := session.safeWrite(websocket.TextMessage, msg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestExtractCursorPayload(t *testing.T) {
	tr := &YandexDocsTransport{}
	for msg, want := range map[string]string{
		`42["message",{"type":"cursor","cursor":"18;QUJD"}]`:            "QUJD",
		`42["message",{"type":"cursor","cursor":"7;AAAA","other":"x"}]`: "AAAA",
		`42["message",{"type":"cursor"}]`:                               "",
	} {
		if got := tr.extractBase64String(msg); got != want {
			t.Errorf("%s: got %q, want %q", msg, got, want)
		}
	}
}

// BenchmarkWriterIdleLatency measures send-to-receive time for a packet
// that arrives after the queue has been idle, the common case for
// interactive traffic and the start of every TCP burst.
func BenchmarkWriterIdleLatency(b *testing.B) {
	tr, received := writerPeer(b)
	samples := make([]time.Duration, 0, b.N)
	for i := 0; i < b.N; i++ {
		time.Sleep(15 * time.Millisecond)
		start := time.Now()
		if err := tr.Send([]byte{byte(i)}); err != nil {
			b.Fatal(err)
		}
		<-received
		samples = append(samples, time.Since(start))
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	b.ReportMetric(float64(samples[len(samples)/2].Microseconds()), "p50-us")
	b.ReportMetric(float64(samples[len(samples)*9/10].Microseconds()), "p90-us")
	b.ReportMetric(float64(samples[len(samples)-1].Microseconds()), "max-us")
}
