package recordconn

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func TestCoalescedWriteRejectsClosedAndExpired(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired", true: "closed"}[closed], func(t *testing.T) {
			a, _ := optPair(t, Options{FlushDelay: time.Hour})
			want := error(os.ErrDeadlineExceeded)
			if closed {
				a.Close()
				want = net.ErrClosed
			} else {
				a.SetWriteDeadline(time.Now().Add(-time.Second))
			}
			if n, err := a.Write([]byte("header")); n != 0 || !errors.Is(err, want) {
				t.Fatalf("short write accepted after failure: n=%d err=%v want=%v", n, err, want)
			}
		})
	}
}

func TestCoalescedRetryDoesNotSendUnacceptedBytes(t *testing.T) {
	a, b := optPair(t, Options{Chunk: 256, Window: 1024, FlushDelay: time.Hour})
	initial := bytes.Repeat([]byte{1}, 1024)
	if _, err := a.Write(initial); err != nil {
		t.Fatal(err)
	}
	a.SetWriteDeadline(time.Now().Add(25 * time.Millisecond))
	if n, err := a.Write(bytes.Repeat([]byte{2}, 256)); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("full-window write: n=%d err=%v", n, err)
	}
	a.SetWriteDeadline(time.Now().Add(time.Second))
	got := make([]byte, len(initial))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, initial) {
		t.Fatal("initial bytes changed")
	}
	retry := bytes.Repeat([]byte{3}, 256)
	if n, err := a.Write(retry); n != len(retry) || err != nil {
		t.Fatalf("retry: n=%d err=%v", n, err)
	}
	if actual := a.Stats().WrittenBytes; actual != 1280 {
		t.Fatalf("unaccepted bytes escaped on retry: written=%d want=1280", actual)
	}
	got = make([]byte, len(retry))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, retry) {
		t.Fatal("received failed write instead of retry")
	}
}

func TestCoalescedFlushFailureWakesSession(t *testing.T) {
	a, _ := optPair(t, Options{FlushDelay: time.Hour})
	if _, err := a.Write([]byte("accepted tail")); err != nil {
		t.Fatal(err)
	}
	a.SetWriteDeadline(time.Now().Add(-time.Second))
	// Deterministically run the same operation the timer performs.
	a.flushTail()
	select {
	case <-a.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("accepted tail stranded after async flush failure; session still looks live")
	}
}
