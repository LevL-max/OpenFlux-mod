package transport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"testing"
	"time"
)

type fragmentCapture struct {
	*BaseTransport
	frames  [][]byte
	receive func([]byte)
	fail    bool
}

func (f *fragmentCapture) Send(data []byte) error {
	if f.fail {
		return errors.New("full")
	}
	f.frames = append(f.frames, append([]byte(nil), data...))
	return nil
}
func (f *fragmentCapture) Receive(cb func([]byte)) { f.receive = cb }
func fragmentFixture(t *testing.T) (*FragmentingTransport, *fragmentCapture) {
	t.Helper()
	c := &fragmentCapture{BaseTransport: NewBaseTransport(DefaultConfig())}
	f, err := NewFragmentingTransport(c, 1800)
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func TestFragmentsRoundtripShuffledAndDuplicated(t *testing.T) {
	for _, size := range []int{1, 1501, 1800, 1801, 6023, 65535, 131072} {
		f, c := fragmentFixture(t)
		data := make([]byte, size)
		rand.New(rand.NewSource(int64(size))).Read(data)
		var got [][]byte
		f.Receive(func(b []byte) { got = append(got, b) })
		if err := f.Send(data); err != nil {
			t.Fatal(err)
		}
		for _, frame := range c.frames {
			if len(frame) > 1800 {
				t.Fatal("oversize frame")
			}
		}
		for i := len(c.frames) - 1; i >= 0; i-- {
			c.receive(c.frames[i])
			if i > 0 && len(c.frames) > 1 {
				c.receive(c.frames[i])
			}
		}
		if len(got) != 1 || !bytes.Equal(got[0], data) {
			t.Fatalf("size %d: incomplete or corrupt", size)
		}
	}
}
func TestFragmentsBoundAssemblyMemoryAndExpire(t *testing.T) {
	f, c := fragmentFixture(t)
	now := time.Now()
	f.now = func() time.Time { return now }
	f.Receive(func([]byte) { t.Fatal("delivered incomplete") })
	for i := 0; i < maxAssemblies+5; i++ {
		c.frames = nil
		f.Send(make([]byte, 4000))
		c.receive(c.frames[0])
	}
	if len(f.pending) != maxAssemblies {
		t.Fatalf("assemblies=%d", len(f.pending))
	}
	now = now.Add(assemblyTTL)
	c.frames = nil
	f.Send(make([]byte, 4000))
	c.receive(c.frames[0])
	if len(f.pending) != 1 {
		t.Fatal("expired assemblies retained")
	}
}
func TestFragmentsRejectInvalidBounds(t *testing.T) {
	f, c := fragmentFixture(t)
	f.Receive(func([]byte) { t.Fatal("delivered malformed") })
	f.Send(make([]byte, 4000))
	for _, mutate := range []func([]byte){func(b []byte) { binary.BigEndian.PutUint32(b[20:24], maxReassembled+1) }, func(b []byte) { binary.BigEndian.PutUint16(b[24:26], 100) }, func(b []byte) { binary.BigEndian.PutUint16(b[26:28], 65535) }} {
		b := append([]byte(nil), c.frames[0]...)
		mutate(b)
		c.receive(b)
	}
	if len(f.pending) != 0 {
		t.Fatal("allocated invalid assembly")
	}
	if err := f.Send(make([]byte, maxReassembled+1)); err == nil {
		t.Fatal("accepted oversized input")
	}
}
func TestFragmentsPropagateSendFailure(t *testing.T) {
	f, c := fragmentFixture(t)
	c.fail = true
	if f.Send(make([]byte, 6000)) == nil {
		t.Fatal("hid send failure")
	}
}
func TestFragmentsEscapeMagicPrefix(t *testing.T) {
	f, c := fragmentFixture(t)
	data := []byte("OFL1 packet")
	var got []byte
	f.Receive(func(b []byte) { got = b })
	f.Send(data)
	for _, b := range c.frames {
		c.receive(b)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("magic prefix corrupted")
	}
}
