package yandex

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"universal-bypass-tool/transport"
)

var (
	errVolgaV6TransportNotStarted = errors.New("volga v6 transport not started")
	errVolgaV6TransportQueueFull  = errors.New("volga v6 transport queue full")
)

type VolgaV6TransportConfig struct {
	Documents []string

	BatchPackets int
	BatchBytes   int
	BatchTimeout time.Duration
	QueueSize    int
	TickInterval time.Duration

	Runtime volgaV6RuntimeConfig
	Yandex  volgaV6YandexConfig
}

func DefaultVolgaV6TransportConfig(documents []string) VolgaV6TransportConfig {
	return VolgaV6TransportConfig{
		Documents:    append([]string(nil), documents...),
		BatchPackets: 20,
		BatchBytes:   5000,
		BatchTimeout: 2 * time.Millisecond,
		QueueSize:    1_000_000,
		TickInterval: 50 * time.Millisecond,
		Runtime:      defaultVolgaV6RuntimeConfig(),
		Yandex:       defaultVolgaV6YandexConfig(),
	}
}

type YandexVolgaV6Transport struct {
	*transport.BaseTransport

	config  VolgaV6TransportConfig
	factory volgaV6CarrierFactory
	runtime *volgaV6Runtime

	ctx    context.Context
	cancel context.CancelFunc

	queue chan []byte
	wg    sync.WaitGroup

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
	if v6cfg.TickInterval <= 0 {
		v6cfg.TickInterval = defaults.TickInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &YandexVolgaV6Transport{
		BaseTransport: transport.NewBaseTransport(baseCfg),
		config:        v6cfg,
		factory:       factory,
		ctx:           ctx,
		cancel:        cancel,
		queue:         make(chan []byte, v6cfg.QueueSize),
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
	t.wg.Add(2)
	go t.batchLoop()
	go t.tickLoop()
	return nil
}

func (t *YandexVolgaV6Transport) Stop() error {
	if !t.stopped.CompareAndSwap(false, true) {
		return nil
	}
	t.cancel()
	t.wg.Wait()
	runtimeErr := t.runtime.Stop()
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
		return t.acceptBatch(toSend)
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
			}
		}
	}
}

func (t *YandexVolgaV6Transport) IsConnected() bool {
	return t.started.Load() && !t.stopped.Load() && t.BaseTransport.IsConnected()
}

func (t *YandexVolgaV6Transport) Snapshot(now time.Time) volgaV6RuntimeSnapshot {
	return t.runtime.Snapshot(now)
}
