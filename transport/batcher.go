package transport

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

var batchMagic = []byte{'O', 'F', 'B', '1'}

type BatchingTransport struct {
	inner      Transport
	maxPackets int
	maxBytes   int
	flushDelay time.Duration

	mu           sync.Mutex
	pending      [][]byte
	pendingBytes int
	timer        *time.Timer

	running atomic.Int32

	txPackets    atomic.Uint64
	txBatches    atomic.Uint64
	txBytes      atomic.Uint64
	timerFlushes atomic.Uint64
	fullFlushes  atomic.Uint64
	sendErrors   atomic.Uint64
	rxBatches    atomic.Uint64
	rxPackets    atomic.Uint64
	decodeErrors atomic.Uint64
}

func NewBatchingTransport(inner Transport, maxPackets, maxBytes int, flushDelay time.Duration) Transport {
	return &BatchingTransport{
		inner:      inner,
		maxPackets: maxPackets,
		maxBytes:   maxBytes,
		flushDelay: flushDelay,
	}
}

func (b *BatchingTransport) Start() error {
	if err := b.inner.Start(); err != nil {
		return err
	}
	b.running.Store(1)

	if b.maxPackets > 1 {
		go b.statsLoop()
	}

	return nil
}

func (b *BatchingTransport) Stop() error {
	b.running.Store(0)

	var frame []byte

	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	frame = b.makeFrameLocked()
	b.mu.Unlock()

	if frame != nil {
		if err := b.inner.Send(frame); err != nil {
			b.sendErrors.Add(1)
		}
	}

	return b.inner.Stop()
}

func (b *BatchingTransport) IsConnected() bool {
	return b.inner.IsConnected()
}

func (b *BatchingTransport) Stats() TransportStats {
	return b.inner.Stats()
}

func (b *BatchingTransport) Send(data []byte) error {
	if b.maxPackets <= 1 {
		return b.inner.Send(data)
	}

	packet := append([]byte(nil), data...)
	b.txPackets.Add(1)
	b.txBytes.Add(uint64(len(packet)))

	var frame []byte

	b.mu.Lock()

	b.pending = append(b.pending, packet)
	b.pendingBytes += len(packet)

	if len(b.pending) == 1 {
		b.timer = time.AfterFunc(b.flushDelay, b.flushTimer)
	}

	if len(b.pending) >= b.maxPackets || b.pendingBytes >= b.maxBytes {
		b.fullFlushes.Add(1)

		if b.timer != nil {
			b.timer.Stop()
			b.timer = nil
		}

		frame = b.makeFrameLocked()
	}

	b.mu.Unlock()

	if frame != nil {
		if err := b.inner.Send(frame); err != nil {
			b.sendErrors.Add(1)
			return err
		}
		b.txBatches.Add(1)
	}

	return nil
}

func (b *BatchingTransport) flushTimer() {
	if b.running.Load() == 0 {
		return
	}

	b.mu.Lock()
	b.timer = nil
	frame := b.makeFrameLocked()
	b.mu.Unlock()

	if frame == nil {
		return
	}

	b.timerFlushes.Add(1)

	if err := b.inner.Send(frame); err != nil {
		b.sendErrors.Add(1)
		return
	}

	b.txBatches.Add(1)
}

func (b *BatchingTransport) makeFrameLocked() []byte {
	if len(b.pending) == 0 {
		return nil
	}

	total := 6
	for _, p := range b.pending {
		total += 4 + len(p)
	}

	out := make([]byte, total)
	copy(out[:4], batchMagic)
	binary.BigEndian.PutUint16(out[4:6], uint16(len(b.pending)))

	pos := 6
	for _, p := range b.pending {
		binary.BigEndian.PutUint32(out[pos:pos+4], uint32(len(p)))
		pos += 4
		copy(out[pos:pos+len(p)], p)
		pos += len(p)
	}

	b.pending = b.pending[:0]
	b.pendingBytes = 0

	return out
}

func (b *BatchingTransport) Receive(callback func([]byte)) {
	if b.maxPackets <= 1 {
		b.inner.Receive(callback)
		return
	}

	b.inner.Receive(func(data []byte) {
		if len(data) < 6 || !bytes.Equal(data[:4], batchMagic) {
			callback(data)
			return
		}

		count := int(binary.BigEndian.Uint16(data[4:6]))
		if count < 1 {
			b.decodeErrors.Add(1)
			return
		}

		pos := 6

		for i := 0; i < count; i++ {
			if pos+4 > len(data) {
				b.decodeErrors.Add(1)
				return
			}

			n := int(binary.BigEndian.Uint32(data[pos : pos+4]))
			pos += 4

			if n < 1 || pos+n > len(data) {
				b.decodeErrors.Add(1)
				return
			}

			callback(data[pos : pos+n])
			pos += n
			b.rxPackets.Add(1)
		}

		if pos != len(data) {
			b.decodeErrors.Add(1)
			return
		}

		b.rxBatches.Add(1)
	})
}

func (b *BatchingTransport) statsLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var prevTxPackets uint64
	var prevTxBatches uint64
	var prevRxPackets uint64
	var prevRxBatches uint64
	var prevTimer uint64
	var prevFull uint64
	var prevErrors uint64

	for b.running.Load() == 1 {
		<-ticker.C

		txPackets := b.txPackets.Load()
		txBatches := b.txBatches.Load()
		rxPackets := b.rxPackets.Load()
		rxBatches := b.rxBatches.Load()
		timerFlushes := b.timerFlushes.Load()
		fullFlushes := b.fullFlushes.Load()
		sendErrors := b.sendErrors.Load()

		b.mu.Lock()
		pending := len(b.pending)
		pendingBytes := b.pendingBytes
		b.mu.Unlock()

		dTxPackets := txPackets - prevTxPackets
		dTxBatches := txBatches - prevTxBatches
		dRxPackets := rxPackets - prevRxPackets
		dRxBatches := rxBatches - prevRxBatches

		txAvg := 0.0
		if dTxBatches > 0 {
			txAvg = float64(dTxPackets) / float64(dTxBatches)
		}

		rxAvg := 0.0
		if dRxBatches > 0 {
			rxAvg = float64(dRxPackets) / float64(dRxBatches)
		}

		log.Printf(
			"[BATCH] tx_pkt=%d tx_batch=%d avg_tx=%.2f rx_pkt=%d rx_batch=%d avg_rx=%.2f pending=%d/%dB timer_flush=%d(+%d) full_flush=%d(+%d) send_err=%d(+%d) decode_err=%d",
			dTxPackets,
			dTxBatches,
			txAvg,
			dRxPackets,
			dRxBatches,
			rxAvg,
			pending,
			pendingBytes,
			timerFlushes,
			timerFlushes-prevTimer,
			fullFlushes,
			fullFlushes-prevFull,
			sendErrors,
			sendErrors-prevErrors,
			b.decodeErrors.Load(),
		)

		prevTxPackets = txPackets
		prevTxBatches = txBatches
		prevRxPackets = rxPackets
		prevRxBatches = rxBatches
		prevTimer = timerFlushes
		prevFull = fullFlushes
		prevErrors = sendErrors
	}
}

var _ Transport = (*BatchingTransport)(nil)

func validateBatchConfig(maxPackets int, maxBytes int) error {
	if maxPackets < 1 || maxPackets > 64 {
		return fmt.Errorf("invalid batch packet count: %d", maxPackets)
	}
	if maxBytes < 4096 {
		return fmt.Errorf("invalid batch max bytes: %d", maxBytes)
	}
	return nil
}
