package transport

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const fragmentHeader = 28
const maxReassembled = 128 << 10
const maxAssemblies = 64
const assemblyTTL = 30 * time.Second

var fragmentMagic = []byte("OFL1")

type fragmentAssembly struct {
	parts                 [][]byte
	size, received, bytes int
	created               time.Time
}

// FragmentingTransport bounds the binary cursor payload, including packets
// larger than the nominal MTU received through Linux raw sockets/GRO.
// Place it below compression; the complete compressed packet is reassembled
// before decompression. Both peers must enable this wrapper.
type FragmentingTransport struct {
	Transport
	limit    int
	epoch    [8]byte
	sequence atomic.Uint64
	mu       sync.Mutex
	pending  map[[16]byte]*fragmentAssembly
	now      func() time.Time
}

func NewFragmentingTransport(inner Transport, limit int) (*FragmentingTransport, error) {
	if limit <= fragmentHeader || limit > maxReassembled {
		return nil, fmt.Errorf("invalid fragment limit")
	}
	f := &FragmentingTransport{Transport: inner, limit: limit, pending: make(map[[16]byte]*fragmentAssembly), now: time.Now}
	if _, err := rand.Read(f.epoch[:]); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *FragmentingTransport) Send(data []byte) error {
	if len(data) == 0 || len(data) > maxReassembled {
		return fmt.Errorf("invalid packet size: %d", len(data))
	}
	if len(data) <= f.limit && !bytes.HasPrefix(data, fragmentMagic) {
		return f.Transport.Send(data)
	}
	partSize := f.limit - fragmentHeader
	count := (len(data) + partSize - 1) / partSize
	id := f.sequence.Add(1)
	for index := 0; index < count; index++ {
		start := index * partSize
		end := min(start+partSize, len(data))
		frame := make([]byte, fragmentHeader+end-start)
		copy(frame, fragmentMagic)
		copy(frame[4:12], f.epoch[:])
		binary.BigEndian.PutUint64(frame[12:20], id)
		binary.BigEndian.PutUint32(frame[20:24], uint32(len(data)))
		binary.BigEndian.PutUint16(frame[24:26], uint16(index))
		binary.BigEndian.PutUint16(frame[26:28], uint16(count))
		copy(frame[28:], data[start:end])
		if err := f.Transport.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

func (f *FragmentingTransport) Receive(callback func([]byte)) {
	f.Transport.Receive(func(data []byte) {
		if !bytes.HasPrefix(data, fragmentMagic) {
			callback(data)
			return
		}
		complete := f.accept(data)
		if complete != nil {
			callback(complete)
		}
	})
}

func (f *FragmentingTransport) accept(data []byte) []byte {
	if len(data) <= fragmentHeader || len(data) > f.limit {
		return nil
	}
	size := int(binary.BigEndian.Uint32(data[20:24]))
	index, count := int(binary.BigEndian.Uint16(data[24:26])), int(binary.BigEndian.Uint16(data[26:28]))
	partSize := f.limit - fragmentHeader
	if size < 1 || size > maxReassembled || count != (size+partSize-1)/partSize || index >= count {
		return nil
	}
	want := min(partSize, size-index*partSize)
	if len(data)-fragmentHeader != want {
		return nil
	}
	var key [16]byte
	copy(key[:], data[4:20])
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	for k, p := range f.pending {
		if now.Sub(p.created) >= assemblyTTL {
			delete(f.pending, k)
		}
	}
	p := f.pending[key]
	if p == nil {
		if len(f.pending) >= maxAssemblies {
			return nil
		}
		p = &fragmentAssembly{parts: make([][]byte, count), size: size, created: now}
		f.pending[key] = p
	}
	if p.size != size || len(p.parts) != count {
		return nil
	}
	if p.parts[index] != nil {
		return nil
	}
	p.parts[index] = append([]byte(nil), data[fragmentHeader:]...)
	p.received++
	p.bytes += want
	if p.received != count {
		return nil
	}
	delete(f.pending, key)
	if p.bytes != size {
		return nil
	}
	out := make([]byte, 0, size)
	for _, part := range p.parts {
		out = append(out, part...)
	}
	return out
}
