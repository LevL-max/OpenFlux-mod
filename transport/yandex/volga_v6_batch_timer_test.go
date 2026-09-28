//go:build volga

package yandex

import (
	"context"
	"testing"
	"time"
)

func TestVolgaV6BatchTimerAfterBlockedFlush(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tr := &YandexVolgaV6Transport{ctx: ctx, cancel: cancel, config: DefaultVolgaV6TransportConfig(nil), queue: make(chan []byte, 60), sendQueue: make(chan [][]byte)}
	tr.config.BatchTimeout = 10 * time.Millisecond
	for i := 0; i < 60; i++ {
		tr.queue <- make([]byte, 1500)
	}
	tr.wg.Add(1)
	go tr.batchLoop()
	defer func() { cancel(); tr.wg.Wait() }()
	for i := 0; i < 20; i++ {
		// Hold the physical queue longer than the old batch's deadline.
		// A full input queue must still produce full batches after it resumes.
		time.Sleep(25 * time.Millisecond)
		select {
		case batch := <-tr.sendQueue:
			if len(batch) != 3 {
				t.Fatalf("batch %d contains %d packets despite queued bulk input", i, len(batch))
			}
		case <-time.After(time.Second):
			t.Fatal("batcher stalled")
		}
	}
}
