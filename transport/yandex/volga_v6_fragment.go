//go:build volga

package yandex

import (
	"encoding/binary"
	"sync"
	"time"
)

const (
	volgaV6MaxRecordBytes    = 0xffff
	volgaV6MaxFrameBytes     = 1 << 20
	volgaV6FragmentBytes     = 4096
	volgaV6FragmentMetaBytes = len(volgaV6Magic) + 1 + 8 + 8 + 4 + 2
	volgaV6MaxAssemblies     = 256
	volgaV6MaxAssemblyBytes  = 16 << 20
	volgaV6AssemblyTTL       = 30 * time.Second
)

// Fragmentation is below logical reliability: all pieces retain Session/Seq.
// Receivers ACK only the reassembled DATA frame, never individual pieces.
// Both peers must support this frame kind to exchange oversized records.
type volgaV6Fragment struct {
	Total uint32
	Index uint16
	Data  []byte
}

func validVolgaV6Payload(payload [][]byte) bool {
	if len(payload) == 0 {
		return false
	}
	size := 2 + volgaV6DataMetaBytes
	for _, record := range payload {
		if len(record) == 0 || len(record) > volgaV6MaxRecordBytes {
			return false
		}
		size += 2 + len(record)
		if size > volgaV6MaxFrameBytes {
			return false
		}
	}
	return true
}

func validVolgaV6Fragment(frame volgaV6WireFrame) bool {
	f := frame.Fragment
	if frame.Kind != volgaV6FrameFragment || frame.Session == 0 || frame.Seq == 0 ||
		f.Total <= uint32(volgaV6DataMetaBytes+2) || f.Total > volgaV6MaxFrameBytes {
		return false
	}
	offset := int(f.Index) * volgaV6FragmentBytes
	return offset < int(f.Total) && len(f.Data) == min(volgaV6FragmentBytes, int(f.Total)-offset)
}

func encodeVolgaV6Fragment(frame volgaV6WireFrame) ([][]byte, bool) {
	if !validVolgaV6Fragment(frame) {
		return nil, false
	}
	meta := make([]byte, volgaV6FragmentMetaBytes)
	copy(meta, volgaV6Magic)
	meta[len(volgaV6Magic)] = byte(volgaV6FrameFragment)
	off := len(volgaV6Magic) + 1
	binary.BigEndian.PutUint64(meta[off:], frame.Session)
	binary.BigEndian.PutUint64(meta[off+8:], frame.Seq)
	binary.BigEndian.PutUint32(meta[off+16:], frame.Fragment.Total)
	binary.BigEndian.PutUint16(meta[off+20:], frame.Fragment.Index)
	return [][]byte{meta, frame.Fragment.Data}, true
}

func decodeVolgaV6Fragment(records [][]byte) (volgaV6WireFrame, bool) {
	if len(records) != 2 || len(records[0]) != volgaV6FragmentMetaBytes {
		return volgaV6WireFrame{}, false
	}
	meta := records[0]
	if string(meta[:len(volgaV6Magic)]) != volgaV6Magic || meta[len(volgaV6Magic)] != byte(volgaV6FrameFragment) {
		return volgaV6WireFrame{}, false
	}
	off := len(volgaV6Magic) + 1
	frame := volgaV6WireFrame{
		Kind:    volgaV6FrameFragment,
		Session: binary.BigEndian.Uint64(meta[off:]),
		Seq:     binary.BigEndian.Uint64(meta[off+8:]),
		Fragment: volgaV6Fragment{
			Total: binary.BigEndian.Uint32(meta[off+16:]),
			Index: binary.BigEndian.Uint16(meta[off+20:]),
			Data:  records[1],
		},
	}
	return frame, validVolgaV6Fragment(frame)
}

func volgaV6RecordBlob(records [][]byte) ([]byte, error) {
	size := 0
	for _, record := range records {
		if len(record) == 0 || len(record) > volgaV6MaxRecordBytes {
			return nil, errVolgaV6InvalidPayload
		}
		size += 2 + len(record)
		if size > volgaV6MaxFrameBytes {
			return nil, errVolgaV6InvalidPayload
		}
	}
	if size == 0 {
		return nil, errVolgaV6InvalidPayload
	}
	blob := make([]byte, 0, size)
	for _, record := range records {
		blob = binary.BigEndian.AppendUint16(blob, uint16(len(record)))
		blob = append(blob, record...)
	}
	return blob, nil
}

func decodeVolgaV6RecordBlob(blob []byte) ([][]byte, bool) {
	var records [][]byte
	for len(blob) > 0 {
		if len(blob) < 2 {
			return nil, false
		}
		n := int(binary.BigEndian.Uint16(blob))
		blob = blob[2:]
		if n == 0 || n > len(blob) {
			return nil, false
		}
		records = append(records, blob[:n])
		blob = blob[n:]
	}
	return records, len(records) > 0
}

type volgaV6AssemblyKey struct{ session, seq uint64 }
type volgaV6Assembly struct {
	created   time.Time
	data      []byte
	seen      []bool
	remaining int
}

// Shared by all physical generations. Limits and expiry bound incomplete
// reassembly memory; eviction is safe because the sender still owns replay.
type volgaV6Reassembler struct {
	mu      sync.Mutex
	entries map[volgaV6AssemblyKey]*volgaV6Assembly
	bytes   int
}

func (r *volgaV6Reassembler) remove(key volgaV6AssemblyKey) {
	r.bytes -= len(r.entries[key].data)
	delete(r.entries, key)
}

func (r *volgaV6Reassembler) Accept(frame volgaV6WireFrame, now time.Time) (volgaV6WireFrame, bool) {
	if !validVolgaV6Fragment(frame) {
		return volgaV6WireFrame{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[volgaV6AssemblyKey]*volgaV6Assembly)
	}
	for key, entry := range r.entries {
		if now.Sub(entry.created) >= volgaV6AssemblyTTL {
			r.remove(key)
		}
	}
	f := frame.Fragment
	key := volgaV6AssemblyKey{frame.Session, frame.Seq}
	entry := r.entries[key]
	if entry != nil && len(entry.data) != int(f.Total) {
		return volgaV6WireFrame{}, false
	}
	if entry == nil {
		for len(r.entries) >= volgaV6MaxAssemblies || r.bytes+int(f.Total) > volgaV6MaxAssemblyBytes {
			var oldest volgaV6AssemblyKey
			var at time.Time
			for k, e := range r.entries {
				if at.IsZero() || e.created.Before(at) {
					oldest, at = k, e.created
				}
			}
			r.remove(oldest)
		}
		count := (int(f.Total) + volgaV6FragmentBytes - 1) / volgaV6FragmentBytes
		entry = &volgaV6Assembly{created: now, data: make([]byte, f.Total), seen: make([]bool, count), remaining: count}
		r.entries[key] = entry
		r.bytes += len(entry.data)
	}
	if !entry.seen[f.Index] {
		copy(entry.data[int(f.Index)*volgaV6FragmentBytes:], f.Data)
		entry.seen[f.Index] = true
		entry.remaining--
	}
	if entry.remaining != 0 {
		return volgaV6WireFrame{}, false
	}
	r.remove(key)
	records, ok := decodeVolgaV6RecordBlob(entry.data)
	if !ok {
		return volgaV6WireFrame{}, false
	}
	data, ok := decodeVolgaV6Records(records)
	if !ok || data.Kind != volgaV6FrameData || data.Session != frame.Session || data.Seq != frame.Seq {
		return volgaV6WireFrame{}, false
	}
	return data, true
}
