package yandex

import (
	"reflect"
	"testing"
)

func TestVolgaV6WireDataRoundTrip(t *testing.T) {
	frame := volgaV6WireFrame{
		Kind:    volgaV6FrameData,
		Session: 1234,
		Seq:     77,
		Floor:   71,
		Payload: [][]byte{[]byte("one"), []byte("two")},
	}
	records, ok := encodeVolgaV6Records(frame)
	if !ok {
		t.Fatal("DATA encode failed")
	}
	decoded, ok := decodeVolgaV6Records(records)
	if !ok {
		t.Fatal("DATA decode failed")
	}
	if decoded.Kind != frame.Kind || decoded.Session != frame.Session || decoded.Seq != frame.Seq || decoded.Floor != frame.Floor || !reflect.DeepEqual(decoded.Payload, frame.Payload) {
		t.Fatalf("decoded=%+v want=%+v", decoded, frame)
	}
}

func TestVolgaV6WireAckRoundTrip(t *testing.T) {
	ack := volgaV6Ack{
		Session: 5678,
		Base:    100,
		Ranges: []volgaV6AckRange{
			{Start: 102, End: 104},
			{Start: 108, End: 110},
		},
	}
	records, ok := encodeVolgaV6Records(volgaV6WireFrame{Kind: volgaV6FrameAck, Session: ack.Session, Ack: ack})
	if !ok || len(records) != 1 {
		t.Fatalf("ACK encode ok=%t records=%d", ok, len(records))
	}
	decoded, ok := decodeVolgaV6Records(records)
	if !ok {
		t.Fatal("ACK decode failed")
	}
	if decoded.Kind != volgaV6FrameAck || decoded.Session != ack.Session || !reflect.DeepEqual(decoded.Ack, ack) {
		t.Fatalf("decoded=%+v want ack=%+v", decoded, ack)
	}
}

func TestVolgaV6WireRejectsInvalidDataFloor(t *testing.T) {
	_, ok := encodeVolgaV6Records(volgaV6WireFrame{
		Kind:    volgaV6FrameData,
		Session: 1,
		Seq:     5,
		Floor:   6,
		Payload: [][]byte{[]byte("x")},
	})
	if ok {
		t.Fatal("invalid replay floor encoded")
	}
}

func TestVolgaV6WireRejectsDataWithoutPayload(t *testing.T) {
	meta, ok := encodeVolgaV6DataMeta(volgaV6WireFrame{Kind: volgaV6FrameData, Session: 1, Seq: 1, Floor: 1})
	if !ok {
		t.Fatal("metadata encode failed")
	}
	if _, ok := decodeVolgaV6Records([][]byte{meta}); ok {
		t.Fatal("DATA metadata without payload accepted")
	}
}

func TestVolgaV6WireRejectsOverlappingAckRanges(t *testing.T) {
	ack := volgaV6Ack{
		Session: 9,
		Base:    10,
		Ranges: []volgaV6AckRange{
			{Start: 12, End: 15},
			{Start: 15, End: 17},
		},
	}
	if _, ok := encodeVolgaV6Ack(ack); ok {
		t.Fatal("overlapping ACK ranges encoded")
	}
}

func TestVolgaV6WireRejectsAckRangeAtOrBelowBase(t *testing.T) {
	ack := volgaV6Ack{
		Session: 9,
		Base:    10,
		Ranges:  []volgaV6AckRange{{Start: 10, End: 12}},
	}
	if _, ok := encodeVolgaV6Ack(ack); ok {
		t.Fatal("ACK range at cumulative base encoded")
	}
}

func TestVolgaV6WireRejectsWrongMagic(t *testing.T) {
	meta, ok := encodeVolgaV6DataMeta(volgaV6WireFrame{Kind: volgaV6FrameData, Session: 1, Seq: 1, Floor: 1})
	if !ok {
		t.Fatal("metadata encode failed")
	}
	meta[0] ^= 0xff
	if _, ok := decodeVolgaV6DataMeta(meta); ok {
		t.Fatal("wrong magic accepted")
	}
}

func TestVolgaV6WireCopiesPayload(t *testing.T) {
	payload := [][]byte{[]byte("original")}
	records, ok := encodeVolgaV6Records(volgaV6WireFrame{
		Kind:    volgaV6FrameData,
		Session: 1,
		Seq:     1,
		Floor:   1,
		Payload: payload,
	})
	if !ok {
		t.Fatal("encode failed")
	}
	payload[0][0] = 'X'
	if string(records[1]) != "original" {
		t.Fatalf("wire payload aliased caller buffer: %q", records[1])
	}
}
