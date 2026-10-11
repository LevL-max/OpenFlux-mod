package transport

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
)

func stripeFixture(t *testing.T) (*StripedTransport, []*fragmentCapture) {
	t.Helper()
	a := &fragmentCapture{BaseTransport: NewBaseTransport(DefaultConfig())}
	b := &fragmentCapture{BaseTransport: NewBaseTransport(DefaultConfig())}
	a.SetConnected(true)
	b.SetConnected(true)
	s, err := NewStripedTransport([]Transport{a, b})
	if err != nil {
		t.Fatal(err)
	}
	return s, []*fragmentCapture{a, b}
}

func TestStripesRoundRobinDeduplicateAndIgnoreEcho(t *testing.T) {
	sender, lanes := stripeFixture(t)
	receiver, receiveLanes := stripeFixture(t)
	var got [][]byte
	receiver.Receive(func(data []byte) { got = append(got, append([]byte(nil), data...)) })
	sender.Receive(func([]byte) { t.Error("own broadcast delivered") })
	for i := 0; i < 8; i++ {
		if err := sender.Send([]byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, lane := range lanes {
		if len(lane.frames) != 4 {
			t.Fatal("unbalanced stripe")
		}
		for _, frame := range lane.frames {
			lane.receive(frame)
			for _, receiveLane := range receiveLanes {
				receiveLane.receive(frame)
				receiveLane.receive(frame)
			}
		}
	}
	if len(got) != 8 {
		t.Fatalf("delivered %d packets", len(got))
	}
	seen := make(map[byte]bool)
	for _, data := range got {
		seen[data[0]] = true
	}
	if len(seen) != 8 {
		t.Fatal("payload missing")
	}
}

func TestStripesFallbackAndRetainPayload(t *testing.T) {
	s, lanes := stripeFixture(t)
	lanes[0].fail = true
	data := []byte("payload")
	if err := s.Send(data); err != nil {
		t.Fatal(err)
	}
	data[0] = 'X'
	if !bytes.Equal(lanes[1].frames[0][stripeHeader:], []byte("payload")) {
		t.Fatal("aliased payload")
	}
	lanes[1].SetConnected(false)
	if err := s.Send(data); err == nil {
		t.Fatal("hid unavailable lanes")
	}
}

func TestStripesReplayWindowAndConcurrentBroadcast(t *testing.T) {
	s, _ := stripeFixture(t)
	sender, lanes := stripeFixture(t)
	sender.Send([]byte("data"))
	frame := lanes[0].frames[0]
	var wg sync.WaitGroup
	count := 0
	var mu sync.Mutex
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.accept(frame) {
				mu.Lock()
				count++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if count != 1 {
		t.Fatal("concurrent duplicate delivered")
	}
	newer := append([]byte(nil), frame...)
	binary.BigEndian.PutUint64(newer[12:20], stripeWindow+2)
	if !s.accept(newer) || s.accept(frame) {
		t.Fatal("replay window did not advance")
	}
	if s.accept([]byte("invalid")) {
		t.Fatal("malformed accepted")
	}
}

func TestStripesParticipantCounts(t *testing.T) {
	for _, count := range []int{4, 8, 16, 32} {
		lanes := make([]Transport, count)
		for i := range lanes {
			lane := &fragmentCapture{BaseTransport: NewBaseTransport(DefaultConfig())}
			lane.SetConnected(true)
			lanes[i] = lane
		}
		s, err := NewStripedTransport(lanes)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < count*3; i++ {
			if err := s.Send([]byte{byte(i)}); err != nil {
				t.Fatal(err)
			}
		}
		for _, lane := range lanes {
			if len(lane.(*fragmentCapture).frames) != 3 {
				t.Fatal("unbalanced lane distribution")
			}
		}
	}
	if _, err := NewStripedTransport(make([]Transport, 33)); err == nil {
		t.Fatal("unbounded lane count")
	}
	if _, err := NewStripedTransport(nil); err == nil {
		t.Fatal("empty pool")
	}
}
