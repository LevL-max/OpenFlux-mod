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
	errVolgaV6TransportNotStarted = errors.New("volga v6 transport not started")
	errVolgaV6TransportQueueFull  = errors.New("volga v6 transport queue full")
)

type VolgaV6TransportConfig struct {
	Documents []string

	BatchPackets      int
	BatchBytes        int
	BatchTimeout      time.Duration
	QueueSize         int
	SendQueueSize     int
	SendWorkers       int
	TickInterval      time.Duration
	Telemetry         bool
	TelemetryInterval time.Duration

	Runtime volgaV6RuntimeConfig
	Yandex  volgaV6YandexConfig
}

func DefaultVolgaV6TransportConfig(documents []string) VolgaV6TransportConfig {
	return VolgaV6TransportConfig{
		Documents:         append([]string(nil), documents...),
		BatchPackets:      20,
		BatchBytes:        5000,
		BatchTimeout:      2 * time.Millisecond,
		QueueSize:         1_000_000,
		SendQueueSize:     8192,
		SendWorkers:       32,
		TickInterval:      20 * time.Millisecond,
		Telemetry:         true,
		TelemetryInterval: time.Second,
		Runtime:           defaultVolgaV6RuntimeConfig(),
		Yandex:            defaultVolgaV6YandexConfig(),
	}
}

type YandexVolgaV6Transport struct {
	*transport.BaseTransport

	config  VolgaV6TransportConfig
	factory volgaV6CarrierFactory
	runtime *volgaV6Runtime

	ctx    context.Context
	cancel context.CancelFunc

	queue     chan []byte
	sendQueue chan [][]byte
	wg        sync.WaitGroup

	started atomic.Bool
	stopped atomic.Bool
}

func NewYandexVolgaV6Transport(documents []string, cfg transport.TransportConfig) *YandexVolgaV6Transport {
	v6cfg := DefaultVolgaV6TransportConfig(documents)
	factory := newVolgaV6YandexCarrierFactory(v6cfg.Documents, v6cfg.Yandex)
	return newYandexVolgaV6TransportWithFactory(cfg, v6cfg, factory)
}

func newYandexVolgaV6TransportWithFactory(baseCfg transport.TransportConfig, v6cfg VolgaV6TransportConfig, factory volgaV6CarrierFactory) *YandexVolgaV6Transport {
	defaults := DefaultVolgaV6TransportConfig(v6cfg.Documents)
	if v6cfg.BatchPackets <= 0 {
		v6cfg.BatchPackets = defaults.BatchPackets
	}
	if v6cfg.BatchBytes <= 0 {
		v6cfg.BatchBytes = defaults.BatchBytes
	}
	if v6cfg.BatchTimeout <= 0 {
		v6cfg.BatchTimeout = defaults.BatchTimeout
	}
	if v6cfg.QueueSize <= 0 {
		v6cfg.QueueSize = defaults.QueueSize
	}
	if v6cfg.SendQueueSize <= 0 {
		v6cfg.SendQueueSize = defaults.SendQueueSize
	}
	if v6cfg.SendWorkers <= 0 {
		v6cfg.SendWorkers = defaults.SendWorkers
	}
	if v6cfg.TickInterval <= 0 {
		v6cfg.TickInterval = defaults.TickInterval
	}
	if v6cfg.TelemetryInterval <= 0 {
		v6cfg.TelemetryInterval = defaults.TelemetryInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &YandexVolgaV6Transport{
		BaseTransport: transport.NewBaseTransport(baseCfg),
		config:        v6cfg,
		factory:       factory,
		ctx:           ctx,
		cancel:        cancel,
		queue:         make(chan []byte, v6cfg.QueueSize),
		sendQueue:     make(chan [][]byte, v6cfg.SendQueueSize),
	}
	t.runtime = newVolgaV6Runtime(0, factory, v6cfg.Runtime, func(payload [][]byte) {
		for _, packet := range payload {
			cp := append([]byte(nil), packet...)
			t.RecordReceive(len(cp))
			t.CallReceive(cp)
		}
	})
	return t
}

func (t *YandexVolgaV6Transport) Start() error {
	if t.stopped.Load() {
		return fmt.Errorf("volga v6 transport cannot restart after Stop")
	}
	if !t.started.CompareAndSwap(false, true) {
		return nil
	}
	if err := t.BaseTransport.Start(); err != nil {
		t.started.Store(false)
		return err
	}
	if err := t.runtime.Start(t.ctx); err != nil {
		t.started.Store(false)
		_ = t.BaseTransport.Stop()
		return err
	}
	t.SetConnected(true)

	goroutines := 2 + t.config.SendWorkers
	if t.config.Telemetry {
		goroutines++
	}
	t.wg.Add(goroutines)
	go t.batchLoop()
	go t.tickLoop()
	for i := 0; i < t.config.SendWorkers; i++ {
		go t.sendWorker()
	}
	if t.config.Telemetry {
		go t.telemetryLoop()
	}
	return nil
}

func (t *YandexVolgaV6Transport) Stop() error {
	if !t.stopped.CompareAndSwap(false, true) {
		return nil
	}

	// Stop accepting new work and cancel physical carriers before waiting for
	// workers. A worker may be blocked inside an HTTP relay POST; stopping the
	// runtime first cancels that carrier context instead of making Stop wait for
	// the full relay timeout.
	t.cancel()
	runtimeErr := t.runtime.Stop()
	t.wg.Wait()

	t.SetConnected(false)
	baseErr := t.BaseTransport.Stop()
	if runtimeErr != nil {
		return runtimeErr
	}
	return baseErr
}

func (t *YandexVolgaV6Transport) Send(data []byte) error {
	if !t.started.Load() || t.stopped.Load() {
		return errVolgaV6TransportNotStarted
	}
	if len(data) == 0 {
		return nil
	}
	cp := append([]byte(nil), data...)
	select {
	case t.queue <- cp:
		return nil
	default:
		return errVolgaV6TransportQueueFull
	}
}

func (t *YandexVolgaV6Transport) acceptBatch(batch [][]byte) bool {
	if len(batch) == 0 {
		return true
	}
	for {
		seq, err := t.runtime.Send(batch)
		if err == nil || seq != 0 {
			// Once seq is allocated the logical replay buffer owns the batch even
			// if the current physical POST returned an error. Recovery must reuse
			// that seq instead of the outer transport allocating a duplicate.
			for _, packet := range batch {
				t.RecordSend(len(packet))
			}
			return true
		}
		if !errors.Is(err, errVolgaV6RecoveryBackpressure) && !errors.Is(err, errVolgaV6NoActiveCarrier) {
			return false
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-t.ctx.Done():
			return false
		}
	}
}

func (t *YandexVolgaV6Transport) enqueueBatch(batch [][]byte) bool {
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

func (t *YandexVolgaV6Transport) batchLoop() {
	defer t.wg.Done()
	ticker := time.NewTicker(t.config.BatchTimeout)
	defer ticker.Stop()

	batch := make([][]byte, 0, t.config.BatchPackets)
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
			if len(batch) >= t.config.BatchPackets || bytesInBatch >= t.config.BatchBytes {
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

func (t *YandexVolgaV6Transport) sendWorker() {
	defer t.wg.Done()
	for {
		select {
		case <-t.ctx.Done():
			return
		case batch := <-t.sendQueue:
			if !t.acceptBatch(batch) {
				return
			}
		}
}

func (t *YandexVolgaV6Transport) tickLoop() {
	defer t.wg.Done()
	ticker := time.NewTicker(t.config.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case now := <-ticker.C:
			result := t.runtime.Tick(t.ctx, now)
			if result.Handoff {
				t.RecordReconnect()
				utils.Debugf("[VOLGA-V6-HANDOFF] old=%d new=%d reason=%s replay=%d oldest_ms=%d",
					result.OldGeneration, result.NewGeneration, result.Recovery.Reason,
					t.runtime.Snapshot(now).Reliable.ReplayDepth,
					t.runtime.Snapshot(now).Reliable.OldestUnackedAge.Milliseconds())
			}
		}
	}
}

func (t *YandexVolgaV6Transport) telemetryLoop() {
	defer t.wg.Done()
	ticker := time.NewTicker(t.config.TelemetryInterval)
	defer ticker.Stop()

	lastAt := time.Now()
	last := t.runtime.Snapshot(lastAt)
	for {
		select {
		case <-t.ctx.Done():
			return
		case now := <-ticker.C:
			current := t.runtime.Snapshot(now)
			seconds := now.Sub(lastAt).Seconds()
			if seconds <= 0 {
				seconds = 1
			}
			dataRate := float64(current.Reliable.NextSeq-last.Reliable.NextSeq) / seconds
			retryRate := float64(current.RepairsSent-last.RepairsSent) / seconds
			ackRate := float64(current.AckSent-last.AckSent) / seconds

			health := current.Carrier.ActiveHealth
			postDelta := health.Posts
			microsDelta := health.PostMicros
			failDelta := health.PostFailures
			if health.Known && last.Carrier.ActiveHealth.Known &&
				health.Generation == last.Carrier.ActiveHealth.Generation {
				postDelta -= minU64(health.Posts, last.Carrier.ActiveHealth.Posts)
				microsDelta -= minU64(health.PostMicros, last.Carrier.ActiveHealth.PostMicros)
				failDelta -= minU64(health.PostFailures, last.Carrier.ActiveHealth.PostFailures)
			}
			postRate := float64(postDelta) / seconds
			avgPostMs := float64(0)
			if postDelta > 0 {
				avgPostMs = float64(microsDelta) / float64(postDelta) / 1000.0
			}
			reason := current.LastHandoffReason
			if reason == "" {
				reason = "-"
			}
			doc := health.Document
			if doc == "" {
				doc = "-"
			}

			utils.Debugf("[VOLGA-V6] session=%d seq=%d ack=%d replay=%d sacked=%d oldest_ms=%d data=%.1f/s retry=%.1f/s acktx=%.1f/s gen=%d doc=%s age_ms=%d ws=%t post=%.1f/s post_fail=%d http_avg_ms=%.2f http_max_ms=%.2f handoffs=%d reason=%s draining=%v q=%d sendq=%d",
				current.Reliable.Session,
				current.Reliable.NextSeq,
				current.Reliable.AckBase,
				current.Reliable.ReplayDepth,
				current.Reliable.SackedDepth,
				current.Reliable.OldestUnackedAge.Milliseconds(),
				dataRate,
				retryRate,
				ackRate,
				current.Carrier.ActiveGeneration,
				doc,
				health.Age.Milliseconds(),
				health.Connected,
				postRate,
				failDelta,
				avgPostMs,
				float64(health.MaxPostMicros)/1000.0,
				current.Carrier.Handoffs,
				reason,
				current.Carrier.Draining,
				len(t.queue),
				len(t.sendQueue))

			headSeq, headRetries := t.runtime.session.diagnosticHead()
			utils.Debugf("[VOLGA-V6-DIAG] session=%d head_seq=%d head_retries=%d rx_session=%d rx_base=%d rx_pending=%d rx_duplicates=%d rx_resets=%d rx_stale=%d",
				current.Reliable.Session,
				headSeq,
				headRetries,
				current.Receiver.PeerSession,
				current.Receiver.Base,
				current.Receiver.Pending,
				current.Receiver.Duplicates,
				current.Receiver.Resets,
				current.Receiver.Stale)

			lastAt = now
			last = current
		}
	}
}

func minU64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func (t *YandexVolgaV6Transport) IsConnected() bool {
	if !t.started.Load() || t.stopped.Load() || !t.BaseTransport.IsConnected() {
		return false
	}
	snap := t.runtime.Snapshot(time.Now())
	if snap.Carrier.ActiveHealth.Known {
		return snap.Carrier.ActiveHealth.Connected
	}
	return true
}

func (t *YandexVolgaV6Transport) Snapshot(now time.Time) volgaV6RuntimeSnapshot {
	return t.runtime.Snapshot(now)
}
