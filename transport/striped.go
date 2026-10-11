package transport

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"universal-bypass-tool/utils"
)

const stripeHeader = 20
const stripeWindow = 4096

type stripeReplay struct {
	highest uint64
	seen    [stripeWindow]uint64
	last    time.Time
}

// StripedTransport distributes datagrams over a bounded set of participants.
// A document broadcasts to every participant, so receivers must discard echoes
// and duplicates. TCP above this layer supplies ordering and retransmission.
// Both endpoints must use the same wrapper; this is deliberately opt-in.
type StripedTransport struct {
	lanes    []Transport
	epoch    [8]byte
	sequence atomic.Uint64
	next     atomic.Uint64
	mu       sync.Mutex
	replay   map[[8]byte]*stripeReplay
	done     chan struct{}
	stopOnce sync.Once
}

func NewStripedTransport(lanes []Transport) (*StripedTransport, error) {
	if len(lanes) < 2 || len(lanes) > 32 {
		return nil, errors.New("striping requires two to 32 lanes")
	}
	s := &StripedTransport{lanes: append([]Transport(nil), lanes...), replay: make(map[[8]byte]*stripeReplay), done: make(chan struct{})}
	if _, err := rand.Read(s.epoch[:]); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *StripedTransport) Start() error {
	select {
	case <-s.done:
		return errors.New("stripe transport stopped")
	default:
	}
	for i, lane := range s.lanes {
		if err := lane.Start(); err != nil {
			for _, started := range s.lanes[:i] {
				_ = started.Stop()
			}
			return err
		}
	}
	go s.statusLoop()
	return nil
}

func (s *StripedTransport) Stop() error {
	s.stopOnce.Do(func() { close(s.done) })
	var result error
	for _, lane := range s.lanes {
		result = errors.Join(result, lane.Stop())
	}
	return result
}

func (s *StripedTransport) statusLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			ready := 0
			for _, lane := range s.lanes {
				if lane.IsConnected() {
					ready++
				}
			}
			utils.Statusf("[YDOCS-POOL] lanes_ready=%d/%d", ready, len(s.lanes))
		}
	}
}

func (s *StripedTransport) IsConnected() bool {
	for _, lane := range s.lanes {
		if lane.IsConnected() {
			return true
		}
	}
	return false
}

// Stats counts physical traffic, including broadcasts; application benchmarks
// must use unique delivered bytes, not the sum of carrier receive counters.
func (s *StripedTransport) Stats() TransportStats {
	var result TransportStats
	for _, lane := range s.lanes {
		x := lane.Stats()
		result.BytesSent += x.BytesSent
		result.BytesReceived += x.BytesReceived
		result.PacketsSent += x.PacketsSent
		result.PacketsRecv += x.PacketsRecv
		result.Reconnects += x.Reconnects
		result.Connected = result.Connected || x.Connected
		result.Uptime = max(result.Uptime, x.Uptime)
	}
	return result
}

func (s *StripedTransport) Send(data []byte) error {
	frame := make([]byte, stripeHeader+len(data))
	copy(frame, "OFP1")
	copy(frame[4:12], s.epoch[:])
	binary.BigEndian.PutUint64(frame[12:20], s.sequence.Add(1))
	copy(frame[stripeHeader:], data)
	start := s.next.Add(1) - 1
	var result error
	for i := range s.lanes {
		lane := s.lanes[(int(start%uint64(len(s.lanes)))+i)%len(s.lanes)]
		if !lane.IsConnected() {
			continue
		}
		if err := lane.Send(frame); err == nil {
			return nil
		} else {
			result = errors.Join(result, err)
		}
	}
	if result == nil {
		result = errors.New("no connected stripe lane")
	}
	return result
}

func (s *StripedTransport) Receive(callback func([]byte)) {
	for _, lane := range s.lanes {
		lane.Receive(func(data []byte) {
			if s.accept(data) {
				callback(data[stripeHeader:])
			}
		})
	}
}

func (s *StripedTransport) accept(data []byte) bool {
	if len(data) <= stripeHeader || !bytes.Equal(data[:4], []byte("OFP1")) {
		return false
	}
	var epoch [8]byte
	copy(epoch[:], data[4:12])
	if epoch == s.epoch {
		return false
	}
	seq := binary.BigEndian.Uint64(data[12:20])
	if seq == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	p := s.replay[epoch]
	if p == nil {
		for k, v := range s.replay {
			if now.Sub(v.last) > time.Minute {
				delete(s.replay, k)
			}
		}
		if len(s.replay) >= 8 {
			return false
		}
		p = &stripeReplay{}
		s.replay[epoch] = p
	}
	p.last = now
	if seq <= p.highest && p.highest-seq >= stripeWindow {
		return false
	}
	if p.seen[seq%stripeWindow] == seq {
		return false
	}
	p.seen[seq%stripeWindow] = seq
	p.highest = max(p.highest, seq)
	return true
}

var _ Transport = (*StripedTransport)(nil)
