#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


def replace_region(text: str, start_marker: str, end_marker: str, new_block: str, label: str) -> str:
    start = text.find(start_marker)
    if start < 0:
        raise SystemExit(f"{label}: start marker not found")
    end = text.find(end_marker, start)
    if end < 0:
        raise SystemExit(f"{label}: end marker not found")
    return text[:start] + new_block + text[end:]


# Applied AFTER V5.1.
#
# V5.2 fixes receiver restart/resume with a still-running sender session.
# V5/V5.1 receiver state always restarted cumulative ACK at base=0. If the
# peer sender had already advanced and ACKed/forgotten early sequence numbers
# against the previous receiver instance, a restarted receiver could never
# observe seq=1 again and cumulative ACK stayed pinned at zero forever.
#
# V5.2 adds sender replay-floor to each DATA metadata record. On first DATA for
# a sender session, receiver initializes base=floor-1, where floor is the oldest
# sequence still present in sender replay. This is safe under ordinary reorder:
# floor points to the oldest still-recoverable sequence, not merely first seen.
# Old V5 metadata remains parseable with implicit floor=1.

p = Path("transport/yandex/volga_reliable_v5.go")
s = p.read_text()

s = replace_once(
    s,
    '\tvolgaReliableDataMetaBytes    = len(volgaReliableMagic) + 1 + 8 + 8\n',
    '\tvolgaReliableDataMetaV5Bytes  = len(volgaReliableMagic) + 1 + 8 + 8\n'
    '\tvolgaReliableDataMetaBytes    = len(volgaReliableMagic) + 1 + 8 + 8 + 8\n',
    'V5.2 metadata size',
)

meta_block = r'''func makeVolgaReliableDataMeta(session, seq uint64) []byte {
	record := make([]byte, volgaReliableDataMetaV5Bytes)
	copy(record[:len(volgaReliableMagic)], volgaReliableMagic)
	record[len(volgaReliableMagic)] = volgaReliableFrameData
	off := len(volgaReliableMagic) + 1
	binary.BigEndian.PutUint64(record[off:off+8], session)
	binary.BigEndian.PutUint64(record[off+8:off+16], seq)
	return record
}

func makeVolgaReliableDataMetaV52(session, seq, floor uint64) []byte {
	if floor == 0 || floor > seq {
		floor = seq
	}
	record := make([]byte, volgaReliableDataMetaBytes)
	copy(record[:len(volgaReliableMagic)], volgaReliableMagic)
	record[len(volgaReliableMagic)] = volgaReliableFrameData
	off := len(volgaReliableMagic) + 1
	binary.BigEndian.PutUint64(record[off:off+8], session)
	binary.BigEndian.PutUint64(record[off+8:off+16], seq)
	binary.BigEndian.PutUint64(record[off+16:off+24], floor)
	return record
}

func parseVolgaReliableDataMeta(record []byte) (session, seq uint64, ok bool) {
	session, seq, _, ok = parseVolgaReliableDataMetaV52(record)
	return session, seq, ok
}

func parseVolgaReliableDataMetaV52(record []byte) (session, seq, floor uint64, ok bool) {
	if len(record) != volgaReliableDataMetaV5Bytes && len(record) != volgaReliableDataMetaBytes {
		return 0, 0, 0, false
	}
	if string(record[:len(volgaReliableMagic)]) != volgaReliableMagic || record[len(volgaReliableMagic)] != volgaReliableFrameData {
		return 0, 0, 0, false
	}
	off := len(volgaReliableMagic) + 1
	session = binary.BigEndian.Uint64(record[off : off+8])
	seq = binary.BigEndian.Uint64(record[off+8 : off+16])
	if len(record) == volgaReliableDataMetaBytes {
		floor = binary.BigEndian.Uint64(record[off+16 : off+24])
	} else {
		floor = 1
	}
	if session == 0 || seq == 0 || floor == 0 || floor > seq {
		return 0, 0, 0, false
	}
	return session, seq, floor, true
}

'''.replace('\\t', '\t')
s = replace_region(
    s,
    'func makeVolgaReliableDataMeta(session, seq uint64) []byte {\n',
    'func makeVolgaReliableAckRecord(ack volgaReliableAck) []byte {\n',
    meta_block,
    'V5.2 DATA metadata',
)

insert_before_ack = r'''func (r *relayClient) volgaReliableReplayFloor(fallback uint64) uint64 {
	st := r.volgaReliableState()
	floor := uint64(0)
	st.mu.Lock()
	for seq := range st.replay {
		if seq == 0 {
			continue
		}
		if floor == 0 || seq < floor {
			floor = seq
		}
	}
	st.mu.Unlock()
	if floor == 0 {
		floor = fallback
	}
	return floor
}

'''.replace('\\t', '\t')
s = replace_once(
    s,
    'func volgaReliableRetryDelay(base time.Duration, retries int) time.Duration {\n',
    insert_before_ack + 'func volgaReliableRetryDelay(base time.Duration, retries int) time.Duration {\n',
    'V5.2 replay floor helper',
)

recv_block = r'''func (w *wsListener) volgaReliableHandleData(session, seq uint64, records [][]byte) {
	w.volgaReliableHandleDataV52(session, seq, 1, records)
}

func (w *wsListener) volgaReliableHandleDataV52(session, seq, floor uint64, records [][]byte) {
	st := w.volgaReliableRecvState()
	if session == 0 || seq == 0 || floor == 0 || floor > seq {
		return
	}

	deliver := false
	st.mu.Lock()
	if st.peerSession == 0 || session > st.peerSession {
		st.peerSession = session
		st.base = floor - 1
		st.seen = make(map[uint64]struct{})
		st.resets.Add(1)
		utils.Debugf("[VOLGA-RELIABLE] receiver baseline session=%d floor=%d first-seq=%d base=%d", session, floor, seq, st.base)
	} else if session < st.peerSession {
		st.stale.Add(1)
		st.mu.Unlock()
		return
	}

	if seq <= st.base {
		st.duplicates.Add(1)
	} else if _, exists := st.seen[seq]; exists {
		st.duplicates.Add(1)
	} else {
		st.seen[seq] = struct{}{}
		deliver = true
		for {
			next := st.base + 1
			if _, ok := st.seen[next]; !ok {
				break
			}
			delete(st.seen, next)
			st.base = next
		}
	}
	st.mu.Unlock()

	w.volgaReliableWakeAck(st)
	if !deliver {
		return
	}

	for _, record := range records {
		packets := w.reassembler.accept(record)
		w.stats.PacketsRecv.Add(uint64(len(packets)))
		for _, pkt := range packets {
			if w.onData != nil {
				w.onData(pkt)
			}
		}
	}
}

'''.replace('\\t', '\t')
s = replace_region(
    s,
    'func (w *wsListener) volgaReliableHandleData(session, seq uint64, records [][]byte) {\n',
    'func (w *wsListener) volgaReliableAckSnapshot(st *volgaReliableRecvState) (volgaReliableAck, bool) {\n',
    recv_block,
    'V5.2 receiver baseline',
)

s = replace_once(
    s,
    'if session, seq, ok := parseVolgaReliableDataMeta(records[0]); ok {\n\t\tw.volgaReliableHandleData(session, seq, records[1:])\n\t\treturn\n\t}\n',
    'if session, seq, floor, ok := parseVolgaReliableDataMetaV52(records[0]); ok {\n\t\tw.volgaReliableHandleDataV52(session, seq, floor, records[1:])\n\t\treturn\n\t}\n',
    'V5.2 receive metadata parser',
)

p.write_text(s)

vy = Path('transport/yandex/vyandex.go')
v = vy.read_text()
v = replace_once(
    v,
    'meta := makeVolgaReliableDataMeta(r.volgaReliableSession(), batchSeq)',
    'meta := makeVolgaReliableDataMetaV52(r.volgaReliableSession(), batchSeq, r.volgaReliableReplayFloor(batchSeq))',
    'V5.2 live sender metadata',
)
vy.write_text(v)

Path('transport/yandex/volga_reliable_v5_2_test.go').write_text(r'''package yandex

import (
	"reflect"
	"testing"
)

func TestVolgaReliableV52DataMetaRoundTripWithFloor(t *testing.T) {
	record := makeVolgaReliableDataMetaV52(1234, 120, 101)
	session, seq, floor, ok := parseVolgaReliableDataMetaV52(record)
	if !ok || session != 1234 || seq != 120 || floor != 101 {
		t.Fatalf("parsed session=%d seq=%d floor=%d ok=%v", session, seq, floor, ok)
	}
}

func TestVolgaReliableV52ParsesLegacyV5Metadata(t *testing.T) {
	record := makeVolgaReliableDataMeta(777, 9)
	session, seq, floor, ok := parseVolgaReliableDataMetaV52(record)
	if !ok || session != 777 || seq != 9 || floor != 1 {
		t.Fatalf("legacy parsed session=%d seq=%d floor=%d ok=%v", session, seq, floor, ok)
	}
}

func TestVolgaReliableV52ReceiverResumesAtReplayFloor(t *testing.T) {
	stats := &VolgaStats{}
	var got []string
	w := &wsListener{
		stats:       stats,
		reassembler: newVolgaFragmentReassembler(volgaReassemblyTimeout, DefaultVolgaConfig().MaxPayloadBytes, stats),
		onData:      func(pkt []byte) { got = append(got, string(pkt)) },
	}

	w.volgaReliableHandleDataV52(5000, 101, 101, [][]byte{[]byte("current")})
	peer, base, pending, _, _, resets, stale, _ := w.volgaReliableRecvSnapshot()
	if peer != 5000 || base != 101 || pending != 0 || resets != 1 || stale != 0 {
		t.Fatalf("state peer=%d base=%d pending=%d resets=%d stale=%d", peer, base, pending, resets, stale)
	}
	if !reflect.DeepEqual(got, []string{"current"}) {
		t.Fatalf("delivery=%v", got)
	}

	w.volgaReliableHandleDataV52(5000, 103, 101, [][]byte{[]byte("three")})
	w.volgaReliableHandleDataV52(5000, 102, 101, [][]byte{[]byte("two")})
	_, base, pending, _, _, _, _, _ = w.volgaReliableRecvSnapshot()
	if base != 103 || pending != 0 {
		t.Fatalf("after reorder base=%d pending=%d", base, pending)
	}
}
''')

print('Volga V5.2 receiver-resume replay-floor transform applied')
