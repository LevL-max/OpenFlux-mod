package transport

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"universal-bypass-tool/network"
	"universal-bypass-tool/utils"
)

// TCPPacketNormalizer splits oversized, non-fragmented IPv4/TCP packets into
// MTU-sized TCP segments before they reach compression/transport framing.
//
// This is intentionally conservative: packets with SYN, RST or URG set, IP
// fragments, malformed headers, non-TCP traffic, or packets whose IP total
// length does not exactly match the supplied buffer are passed through
// unchanged. No host NIC/offload settings are modified.
type TCPPacketNormalizer struct {
	Transport
	mtu       int
	telemetry bool

	normalized atomic.Uint64
	segments   atomic.Uint64
	bytesIn    atomic.Uint64
	maxInput   atomic.Uint64

	stopOnce sync.Once
	stopCh   chan struct{}
}

func NewTCPPacketNormalizer(inner Transport, mtu int, telemetry bool) Transport {
	if inner == nil || mtu <= 0 {
		return inner
	}
	return &TCPPacketNormalizer{
		Transport: inner,
		mtu:       mtu,
		telemetry: telemetry,
		stopCh:    make(chan struct{}),
	}
}

func (n *TCPPacketNormalizer) Start() error {
	if err := n.Transport.Start(); err != nil {
		return err
	}
	if n.telemetry {
		go n.statsLoop()
	}
	return nil
}

func (n *TCPPacketNormalizer) Stop() error {
	n.stopOnce.Do(func() { close(n.stopCh) })
	return n.Transport.Stop()
}

func (n *TCPPacketNormalizer) Send(data []byte) error {
	segments, ok := splitOversizedIPv4TCP(data, n.mtu)
	if !ok {
		return n.Transport.Send(data)
	}

	n.normalized.Add(1)
	n.segments.Add(uint64(len(segments)))
	n.bytesIn.Add(uint64(len(data)))
	atomicMaxNormalizer(&n.maxInput, uint64(len(data)))

	for _, seg := range segments {
		if err := n.Transport.Send(seg); err != nil {
			return err
		}
	}
	return nil
}

func (n *TCPPacketNormalizer) statsLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var lastNormalized, lastSegments, lastBytes uint64
	for {
		select {
		case <-n.stopCh:
			return
		case <-ticker.C:
			normalized := n.normalized.Load()
			segments := n.segments.Load()
			bytesIn := n.bytesIn.Load()
			if normalized != lastNormalized {
				utils.Debugf("[NORMALIZE] split %d pkt/5s -> %d segments | input %d KB/5s | mtu=%d max-input=%d",
					normalized-lastNormalized,
					segments-lastSegments,
					(bytesIn-lastBytes)/1024,
					n.mtu,
					n.maxInput.Load())
			}
			lastNormalized, lastSegments, lastBytes = normalized, segments, bytesIn
		}
	}
}

func splitOversizedIPv4TCP(data []byte, mtu int) ([][]byte, bool) {
	if mtu < 576 || len(data) <= mtu || len(data) < 40 || data[0]>>4 != 4 {
		return nil, false
	}

	ihl := int(data[0]&0x0f) * 4
	if ihl < 20 || ihl > len(data) || len(data) < ihl+20 {
		return nil, false
	}

	totalLen := int(binary.BigEndian.Uint16(data[2:4]))
	if totalLen != len(data) || totalLen <= mtu || data[9] != 6 {
		return nil, false
	}

	flagsFrag := binary.BigEndian.Uint16(data[6:8])
	if flagsFrag&0x2000 != 0 || flagsFrag&0x1fff != 0 { // MF or fragment offset
		return nil, false
	}

	tcpStart := ihl
	tcpHeaderLen := int((data[tcpStart+12] >> 4) & 0x0f) * 4
	if tcpHeaderLen < 20 || tcpStart+tcpHeaderLen > totalLen {
		return nil, false
	}

	flags := data[tcpStart+13]
	if flags&0x02 != 0 || flags&0x04 != 0 || flags&0x20 != 0 { // SYN, RST, URG
		return nil, false
	}

	maxPayload := mtu - ihl - tcpHeaderLen
	payloadLen := totalLen - ihl - tcpHeaderLen
	if maxPayload <= 0 || payloadLen <= maxPayload {
		return nil, false
	}

	seq := binary.BigEndian.Uint32(data[tcpStart+4 : tcpStart+8])
	payloadStart := ihl + tcpHeaderLen
	segmentCount := (payloadLen + maxPayload - 1) / maxPayload
	out := make([][]byte, 0, segmentCount)

	for off := 0; off < payloadLen; off += maxPayload {
		chunkLen := maxPayload
		if remain := payloadLen - off; remain < chunkLen {
			chunkLen = remain
		}
		last := off+chunkLen == payloadLen
		segLen := ihl + tcpHeaderLen + chunkLen
		seg := make([]byte, segLen)

		copy(seg[:ihl+tcpHeaderLen], data[:ihl+tcpHeaderLen])
		copy(seg[payloadStart:], data[payloadStart+off:payloadStart+off+chunkLen])

		binary.BigEndian.PutUint16(seg[2:4], uint16(segLen))
		binary.BigEndian.PutUint32(seg[tcpStart+4:tcpStart+8], seq+uint32(off))

		segFlags := flags
		if !last {
			segFlags &^= 0x01 // FIN belongs only on the final segment
			segFlags &^= 0x08 // PSH belongs only on the final segment
		}
		if off > 0 {
			segFlags &^= 0x80 // CWR, if present, belongs only on the first segment
		}
		seg[tcpStart+13] = segFlags

		seg[10], seg[11] = 0, 0
		ipChecksum := network.IPChecksum(seg[:ihl])
		seg[10] = byte(ipChecksum >> 8)
		seg[11] = byte(ipChecksum)

		tcpBytes := seg[tcpStart:]
		tcpBytes[16], tcpBytes[17] = 0, 0
		src := [4]byte{seg[12], seg[13], seg[14], seg[15]}
		dst := [4]byte{seg[16], seg[17], seg[18], seg[19]}
		tcpChecksum := network.TCPChecksum(tcpBytes, src, dst)
		tcpBytes[16] = byte(tcpChecksum >> 8)
		tcpBytes[17] = byte(tcpChecksum)

		out = append(out, seg)
	}

	return out, true
}

func atomicMaxNormalizer(dst *atomic.Uint64, v uint64) {
	for {
		cur := dst.Load()
		if v <= cur || dst.CompareAndSwap(cur, v) {
			return
		}
	}
}
