package yandex

import "encoding/binary"

const (
	volgaV6Magic = "OFV6REL1"

	volgaV6DataMetaBytes  = len(volgaV6Magic) + 1 + 8 + 8 + 8
	volgaV6AckHeaderBytes = len(volgaV6Magic) + 1 + 8 + 8 + 2
)

func encodeVolgaV6DataMeta(frame volgaV6WireFrame) ([]byte, bool) {
	if frame.Kind != volgaV6FrameData || frame.Session == 0 || frame.Seq == 0 || frame.Floor == 0 || frame.Floor > frame.Seq {
		return nil, false
	}
	record := make([]byte, volgaV6DataMetaBytes)
	copy(record[:len(volgaV6Magic)], volgaV6Magic)
	record[len(volgaV6Magic)] = byte(volgaV6FrameData)
	off := len(volgaV6Magic) + 1
	binary.BigEndian.PutUint64(record[off:off+8], frame.Session)
	binary.BigEndian.PutUint64(record[off+8:off+16], frame.Seq)
	binary.BigEndian.PutUint64(record[off+16:off+24], frame.Floor)
	return record, true
}

func decodeVolgaV6DataMeta(record []byte) (volgaV6WireFrame, bool) {
	if len(record) != volgaV6DataMetaBytes || string(record[:len(volgaV6Magic)]) != volgaV6Magic || record[len(volgaV6Magic)] != byte(volgaV6FrameData) {
		return volgaV6WireFrame{}, false
	}
	off := len(volgaV6Magic) + 1
	frame := volgaV6WireFrame{
		Kind:    volgaV6FrameData,
		Session: binary.BigEndian.Uint64(record[off : off+8]),
		Seq:     binary.BigEndian.Uint64(record[off+8 : off+16]),
		Floor:   binary.BigEndian.Uint64(record[off+16 : off+24]),
	}
	if frame.Session == 0 || frame.Seq == 0 || frame.Floor == 0 || frame.Floor > frame.Seq {
		return volgaV6WireFrame{}, false
	}
	return frame, true
}

func validVolgaV6Ack(ack volgaV6Ack) bool {
	if ack.Session == 0 || len(ack.Ranges) > volgaV6AckMaxRanges {
		return false
	}
	previousEnd := ack.Base
	for _, rg := range ack.Ranges {
		if rg.Start == 0 || rg.End < rg.Start || rg.Start <= ack.Base || rg.Start <= previousEnd {
			return false
		}
		previousEnd = rg.End
	}
	return true
}

func encodeVolgaV6Ack(ack volgaV6Ack) ([]byte, bool) {
	if !validVolgaV6Ack(ack) {
		return nil, false
	}
	record := make([]byte, volgaV6AckHeaderBytes+len(ack.Ranges)*16)
	copy(record[:len(volgaV6Magic)], volgaV6Magic)
	record[len(volgaV6Magic)] = byte(volgaV6FrameAck)
	off := len(volgaV6Magic) + 1
	binary.BigEndian.PutUint64(record[off:off+8], ack.Session)
	binary.BigEndian.PutUint64(record[off+8:off+16], ack.Base)
	binary.BigEndian.PutUint16(record[off+16:off+18], uint16(len(ack.Ranges)))
	p := off + 18
	for _, rg := range ack.Ranges {
		binary.BigEndian.PutUint64(record[p:p+8], rg.Start)
		binary.BigEndian.PutUint64(record[p+8:p+16], rg.End)
		p += 16
	}
	return record, true
}

func decodeVolgaV6Ack(record []byte) (volgaV6Ack, bool) {
	if len(record) < volgaV6AckHeaderBytes || string(record[:len(volgaV6Magic)]) != volgaV6Magic || record[len(volgaV6Magic)] != byte(volgaV6FrameAck) {
		return volgaV6Ack{}, false
	}
	off := len(volgaV6Magic) + 1
	count := int(binary.BigEndian.Uint16(record[off+16 : off+18]))
	if count > volgaV6AckMaxRanges || len(record) != volgaV6AckHeaderBytes+count*16 {
		return volgaV6Ack{}, false
	}
	ack := volgaV6Ack{
		Session: binary.BigEndian.Uint64(record[off : off+8]),
		Base:    binary.BigEndian.Uint64(record[off+8 : off+16]),
		Ranges:  make([]volgaV6AckRange, 0, count),
	}
	p := off + 18
	for i := 0; i < count; i++ {
		ack.Ranges = append(ack.Ranges, volgaV6AckRange{
			Start: binary.BigEndian.Uint64(record[p : p+8]),
			End:   binary.BigEndian.Uint64(record[p+8 : p+16]),
		})
		p += 16
	}
	if !validVolgaV6Ack(ack) {
		return volgaV6Ack{}, false
	}
	return ack, true
}

// encodeVolgaV6Records converts a logical frame into the record list carried
// inside one Volga relay body. Yandex-specific JSON/op IDs are deliberately a
// lower layer and are not part of this codec.
func encodeVolgaV6Records(frame volgaV6WireFrame) ([][]byte, bool) {
	switch frame.Kind {
	case volgaV6FrameData:
		meta, ok := encodeVolgaV6DataMeta(frame)
		if !ok {
			return nil, false
		}
		records := make([][]byte, 0, len(frame.Payload)+1)
		records = append(records, meta)
		records = append(records, cloneVolgaV6Payload(frame.Payload)...)
		return records, true
	case volgaV6FrameAck:
		ackRecord, ok := encodeVolgaV6Ack(frame.Ack)
		if !ok {
			return nil, false
		}
		return [][]byte{ackRecord}, true
	default:
		return nil, false
	}
}

func decodeVolgaV6Records(records [][]byte) (volgaV6WireFrame, bool) {
	if len(records) == 0 {
		return volgaV6WireFrame{}, false
	}
	if data, ok := decodeVolgaV6DataMeta(records[0]); ok {
		if len(records) < 2 {
			return volgaV6WireFrame{}, false
		}
		data.Payload = cloneVolgaV6Payload(records[1:])
		return data, true
	}
	if len(records) == 1 {
		if ack, ok := decodeVolgaV6Ack(records[0]); ok {
			return volgaV6WireFrame{Kind: volgaV6FrameAck, Session: ack.Session, Ack: ack}, true
		}
	}
	return volgaV6WireFrame{}, false
}
