package transport

import (
	"encoding/binary"
	"testing"

	"universal-bypass-tool/network"
)

func makeOversizedTCPPacket(t *testing.T, payloadLen int) []byte {
	t.Helper()
	const ihl = 20
	const tcpHeaderLen = 32
	total := ihl + tcpHeaderLen + payloadLen
	if total > 65535 {
		t.Fatalf("test packet too large: %d", total)
	}

	p := make([]byte, total)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(total))
	binary.BigEndian.PutUint16(p[4:6], 0x1234)
	binary.BigEndian.PutUint16(p[6:8], 0x4000) // DF
	p[8] = 64
	p[9] = 6
	copy(p[12:16], []byte{192, 0, 2, 1})
	copy(p[16:20], []byte{10, 10, 10, 2})

	tcp := p[ihl:]
	binary.BigEndian.PutUint16(tcp[0:2], 443)
	binary.BigEndian.PutUint16(tcp[2:4], 40000)
	binary.BigEndian.PutUint32(tcp[4:8], 100000)
	binary.BigEndian.PutUint32(tcp[8:12], 200000)
	tcp[12] = byte((tcpHeaderLen / 4) << 4)
	tcp[13] = 0x18 // ACK + PSH
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	// 12 bytes of deterministic TCP options.
	copy(tcp[20:32], []byte{1, 1, 8, 10, 0, 0, 0, 1, 0, 0, 0, 2})
	for i := 0; i < payloadLen; i++ {
		tcp[tcpHeaderLen+i] = byte(i)
	}

	p[10], p[11] = 0, 0
	ipCsum := network.IPChecksum(p[:ihl])
	binary.BigEndian.PutUint16(p[10:12], ipCsum)
	tcp[16], tcp[17] = 0, 0
	src := [4]byte{p[12], p[13], p[14], p[15]}
	dst := [4]byte{p[16], p[17], p[18], p[19]}
	tcpCsum := network.TCPChecksum(tcp, src, dst)
	binary.BigEndian.PutUint16(tcp[16:18], tcpCsum)
	return p
}

func TestSplitOversizedIPv4TCPFiveMSS(t *testing.T) {
	p := makeOversizedTCPPacket(t, 5*1448)
	if len(p) != 7292 {
		t.Fatalf("input len=%d want=7292", len(p))
	}

	segments, ok := splitOversizedIPv4TCP(p, 1500)
	if !ok {
		t.Fatal("expected normalization")
	}
	if len(segments) != 5 {
		t.Fatalf("segments=%d want=5", len(segments))
	}

	for i, seg := range segments {
		if len(seg) != 1500 {
			t.Fatalf("segment %d len=%d want=1500", i, len(seg))
		}
		if got := int(binary.BigEndian.Uint16(seg[2:4])); got != len(seg) {
			t.Fatalf("segment %d ip total=%d len=%d", i, got, len(seg))
		}
		seq := binary.BigEndian.Uint32(seg[24:28])
		wantSeq := uint32(100000 + i*1448)
		if seq != wantSeq {
			t.Fatalf("segment %d seq=%d want=%d", i, seq, wantSeq)
		}
		if binary.BigEndian.Uint16(seg[6:8])&0x4000 == 0 {
			t.Fatalf("segment %d lost DF", i)
		}
		psh := seg[33]&0x08 != 0
		if psh != (i == len(segments)-1) {
			t.Fatalf("segment %d PSH=%v", i, psh)
		}
		if got := network.IPChecksum(seg[:20]); got != 0 {
			t.Fatalf("segment %d bad IP checksum residual=%04x", i, got)
		}
		tcp := seg[20:]
		src := [4]byte{seg[12], seg[13], seg[14], seg[15]}
		dst := [4]byte{seg[16], seg[17], seg[18], seg[19]}
		if got := network.TCPChecksum(tcp, src, dst); got != 0 {
			t.Fatalf("segment %d bad TCP checksum residual=%04x", i, got)
		}
	}
}

func TestSplitOversizedIPv4TCPFourMSS(t *testing.T) {
	p := makeOversizedTCPPacket(t, 4*1448)
	if len(p) != 5844 {
		t.Fatalf("input len=%d want=5844", len(p))
	}
	segments, ok := splitOversizedIPv4TCP(p, 1500)
	if !ok || len(segments) != 4 {
		t.Fatalf("ok=%v segments=%d want=4", ok, len(segments))
	}
}

func TestNormalizerLeavesNormalPacketAlone(t *testing.T) {
	p := makeOversizedTCPPacket(t, 1448)
	if _, ok := splitOversizedIPv4TCP(p, 1500); ok {
		t.Fatal("normal MTU-sized packet must not be split")
	}
}
