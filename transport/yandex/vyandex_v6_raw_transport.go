package yandex

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"universal-bypass-tool/transport"
	"universal-bypass-tool/utils"
)

var (
	errVolgaV6RawNotStarted = errors.New("volga v6 raw transport not started")
	errVolgaV6RawQueueFull  = errors.New("volga v6 raw transport queue full")
)

// YandexVolgaV6RawTransport is a diagnostic control transport. It deliberately
// reuses the V6 physical Yandex carrier and V6 wire codec while bypassing the
// entire logical reliability stack: no ACK/SACK, replay, retry scheduler,
// recovery controller, backpressure window or carrier handoff.
//
// Its only purpose is to answer one question: can the V6 physical/wire data
// path reproduce the known-good Volga throughput before reliability is added?
type YandexVolgaV6RawTransport struct {
	*transport.BaseTransport

	document string
	config   volgaV6YandexConfig
	carrier  *volgaV6YandexCarrier

	ctx    context.Context
	cancel context.CancelFunc

	queue     chan []byte
	sendQueue chan [][]byte
	wg        sync.WaitGroup

	session uint64
	seq     atomic.Uint64

	started atomic.Bool
	stopped atomic.Bool
}

const (
	volgaV6RawBatchPackets  = 20
	volgaV6RawBatchBytes    = 5000
	volgaV6RawSendWorkers   = 32
	volgaV6RawQueueSize     = 1_000_000
	volgaV6RawSendQueueSize = 8192
)

var volgaV6RawBatchTimeout = 2 * time.Millisecond

func NewYandexVolgaV6RawTransport(document string, cfg transport.TransportConfig) *YandexVolgaV6RawTransport {
	ctx, cancel := context.WithCancel(context.Background())
	session := uint64(time.Now().UnixNano())
	if session == 0 {
		session = 1
	}
	t := &YandexVolgaV6RawTransport{
		BaseTransport: transport.NewBaseTransport(cfg),
		document:      document,
		config:        defaultVolgaV6YandexConfig(),
		ctx:           ctx,
		cancel:        cancel,
		queue:         make(chan []byte, volgaV6RawQueueSize),
		sendQueue:     make(chan [][]byte, volgaV6RawSendQueueSize),
		session:       session,
	}
	t.carrier = newVolgaV6YandexCarrier(1, document, t.config, t.handleFrame)
	return t
}

func (t *YandexVolgaV6RawTransport) Start() error {
	if t.stopped.Load() {
		return fmt.Errorf("volga v6 raw transport cannot restart after Stop")
	}
	if !t.started.CompareAndSwap(false, true) {
		return nil
	}
	if err := t.BaseTransport.Start(); err != nil {
		t.started.Store(false)
		return err
	}
	if err := t.carrier.Start(t.ctx); err != nil {
		t.started.Store(false)
		_ = t.BaseTransport.Stop()
		return err
	}
	t.SetConnected(true)

	t.wg.Add(2 + volgaV6RawSendWorkers)
	go t.batchLoop()
	for i := 0; i < volgaV6RawSendWorkers; i++ {
		go t.sendWorker()
	}
	go t.telemetryLoop()
	return nil
}

func (t *YandexVolgaV6RawTransport) Stop() error {
	if !t.stopped.CompareAndSwap(false, true) {
		return nil
	}
	t.cancel()
	carrierErr := t.carrier.Stop()
	t.wg.Wait()
	t.SetConnected(false)
	baseErr := t.BaseTransport.Stop()
	if carrierErr != nil {
		return carrierErr
	}
	return baseErr
}

func (t *YandexVolgaV6RawTransport) Send(data []byte) error {
	if !t.started.Load() || t.stopped.Load() {
		return errVolgaV6RawNotStarted
	}
	if len(data) == 0 {
		return nil
	}
	cp := append([]byte(nil), data...)
	select {
	case t.queue <- cp:
		return nil
	default:
		return errVolgaV6RawQueueFull
	}
}

func (t *YandexVolgaV6RawTransport) handleFrame(frame volgaV6WireFrame) {
	if frame.Kind != volgaV6FrameData || len(frame.Payload) == 0 {
		return
	}
	for _, packet := range frame.Payload {
		cp := append([]byte(nil), packet...)
		t.RecordReceive(len(cp))
		t.CallReceive(cp)
	}
}

func (t *YandexVolgaV6RawTransport) enqueueBatch(batch [][]byte) bool {
	if len(batch) == 0 {
		return true
	}
	copyBatch := cloneVolgaV6Payload(batch)
	select {
	case t.sendQueue <- copyBatch:
		return true
	case <-t.ctx.Done():
		return false
	}
}

func (t *YandexVolgaV6RawTransport) batchLoop() {
	defer t.wg.Done()
	ticker := time.NewTicker(volgaV6RawBatchTimeout)
	defer ticker.Stop()

	batch := make([][]byte, 0, volgaV6RawBatchPackets)
	bytesInBatch := 0
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		toSend := cloneVolgaV6Payload(batch)
		batch = batch[:0]
		bytesInBatch = 0
		return t.enqueueBatch(toSend)
	}

	for {
		select {
		case <-t.ctx.Done():
			return
		case packet := <-t.queue:
			batch = append(batch, packet)
			bytesInBatch += len(packet)
			if len(batch) >= volgaV6RawBatchPackets || bytesInBatch >= volgaV6RawBatchBytes {
				if !flush() {
					return
				}
			}
		case <-ticker.C:
			if !flush() {
				return
			}
		}
	}
}

func (t *YandexVolgaV6RawTransport) sendWorker() {
	defer t.wg.Done()
	for {
		select {
		case <-t.ctx.Done():
			return
		case batch := <-t.sendQueue:
			seq := t.seq.Add(1)
			frame := volgaV6WireFrame{
				Kind:    volgaV6FrameData,
				Session: t.session,
				Seq:     seq,
				Floor:   1,
				Payload: batch,
			}
			if err := t.carrier.SendVolgaV6(frame); err != nil {
				// Raw control intentionally has no retry/replay. A failed POST is a
				// failed raw delivery and is visible in carrier telemetry.
				continue
			}
			for _, packet := range batch {
				t.RecordSend(len(packet))
			}
		}
	}
}

func (t *YandexVolgaV6RawTransport) telemetryLoop() {
	defer t.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastPosts uint64
	var lastFailures uint64
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
			s := t.carrier.Snapshot()
			posts := s.Posts - minU64(s.Posts, lastPosts)
			failures := s.PostFailures - minU64(s.PostFailures, lastFailures)
			utils.Debugf("[VOLGA-V6-RAW] session=%d seq=%d ws=%t post=%d/s post_fail=%d/s q=%d sendq=%d",
				t.session, t.seq.Load(), s.Connected, posts, failures, len(t.queue), len(t.sendQueue))
			lastPosts = s.Posts
			lastFailures = s.PostFailures
		}
	}
}

func (t *YandexVolgaV6RawTransport) IsConnected() bool {
	if !t.started.Load() || t.stopped.Load() || !t.BaseTransport.IsConnected() {
		return false
	}
	return t.carrier.Snapshot().Connected
}
