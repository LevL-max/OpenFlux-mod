#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


# Applied AFTER volga_batch_trace_v4d.py.
#
# V4e is a narrow behavioural experiment based on the V4d forensic result:
# EXIT received HTTP 204 for logical batches that the Mini receiver did not see
# for many seconds. V4c's 10s gap wait therefore amplified a real carrier hole
# into severe head-of-line blocking before inner TCP could recover.
#
# V4e keeps sender behaviour unchanged and changes only receive reordering:
#   * normal short reordering is buffered for 250ms;
#   * a real gap is skipped automatically after 250ms even if no new WS event
#     arrives to trigger the old synchronous timeout check;
#   * existing V4d trace remains active so true holes and late arrivals stay
#     observable.
#
# Deliberately unchanged: sender retry/requeue, workers, batching, H1/H2,
# HTTP shards, compression, fragmentation, TCP buffers, auth/reconnect,
# normalisation and all production/router logic.

p = Path("transport/yandex/vyandex.go")
s = p.read_text()

# 250ms is above the healthy reorder waits seen in the V4d failure capture
# (mostly single/tens of ms, ~88ms at the upper end before the real hole), while
# avoiding the 10-20s application stall created by V4c.
s = replace_once(
    s,
    "\tvolgaOrderGapWait    = 10 * time.Second\n\tvolgaOrderMaxPending = 32768\n",
    "\tvolgaOrderGapWait    = 250 * time.Millisecond\n\tvolgaOrderMaxPending = 32768\n",
    "V4e bounded reorder wait",
)

# Timer generation state. We do not retain/cancel time.Timer pointers; an epoch
# makes old callbacks harmless after a natural fill, reset, or replacement gap.
s = replace_once(
    s,
    "\torderGapSince time.Time\n",
    "\torderGapSince      time.Time\n"
    "\torderGapTimerSince time.Time\n"
    "\torderGapEpoch      uint64\n",
    "V4e gap timer state",
)

# Insert asynchronous expiry helpers immediately before the reset helpers.
marker = "func (w *wsListener) resetOrderLocked(userID int) {\n"
if s.count(marker) != 1:
    raise SystemExit(f"V4e helper marker: expected exactly one match, got {s.count(marker)}")
helpers = r'''func (w *wsListener) ensureOrderGapTimerLocked() {
	if len(w.orderPending) == 0 || w.orderGapSince.IsZero() {
		if !w.orderGapTimerSince.IsZero() {
			w.orderGapEpoch++
			w.orderGapTimerSince = time.Time{}
		}
		return
	}

	if !w.orderGapTimerSince.IsZero() && w.orderGapTimerSince.Equal(w.orderGapSince) {
		return
	}

	w.orderGapEpoch++
	epoch := w.orderGapEpoch
	since := w.orderGapSince
	w.orderGapTimerSince = since

	delay := time.Until(since.Add(volgaOrderGapWait))
	if delay < time.Millisecond {
		delay = time.Millisecond
	}
	time.AfterFunc(delay, func() {
		w.expireOrderGap(epoch, since)
	})
}

func (w *wsListener) expireOrderGap(epoch uint64, since time.Time) {
	now := time.Now()
	w.orderMu.Lock()
	if epoch != w.orderGapEpoch || w.orderGapSince.IsZero() || !w.orderGapSince.Equal(since) {
		w.orderMu.Unlock()
		return
	}

	// This timer has fired. Mark it consumed before evaluating the gap so that
	// a newly-created gap can arm its own independent deadline.
	w.orderGapTimerSince = time.Time{}
	ready := w.skipExpiredGapLocked(now)
	w.ensureOrderGapTimerLocked()
	w.orderMu.Unlock()

	for _, bundle := range ready {
		w.deliverOrderedBundle(bundle)
	}
}

'''.replace('\\t', '\t')
s = s.replace(marker, helpers + marker, 1)

# A peer/order reset invalidates any callback belonging to the prior receive
# generation.
s = replace_once(
    s,
    "\tw.orderPending = make(map[uint64]volgaPendingBundle)\n"
    "\tw.orderGapSince = time.Time{}\n"
    "\tw.stats.OrderResets.Add(1)\n",
    "\tw.orderPending = make(map[uint64]volgaPendingBundle)\n"
    "\tw.orderGapSince = time.Time{}\n"
    "\tw.orderGapTimerSince = time.Time{}\n"
    "\tw.orderGapEpoch++\n"
    "\tw.stats.OrderResets.Add(1)\n",
    "V4e reset invalidates gap timer",
)

# Keep exactly one timer attached to the current gap. This is called after both
# natural collection and the existing synchronous skip path, so pending-cap
# skips and newly-exposed later gaps also get a deadline.
s = replace_once(
    s,
    "\tready := w.collectReadyLocked(now)\n"
    "\tready = append(ready, w.skipExpiredGapLocked(now)...)\n"
    "\tw.orderMu.Unlock()\n",
    "\tready := w.collectReadyLocked(now)\n"
    "\tready = append(ready, w.skipExpiredGapLocked(now)...)\n"
    "\tw.ensureOrderGapTimerLocked()\n"
    "\tw.orderMu.Unlock()\n",
    "V4e arm bounded reorder timer",
)

p.write_text(s)

Path("transport/yandex/volga_bounded_reorder_v4e_test.go").write_text(r'''package yandex

import (
	"reflect"
	"testing"
	"time"
)

func TestVolgaV4eShortReorderFillsBeforeDeadline(t *testing.T) {
	stats := &VolgaStats{}
	var got []string
	w := newWSListener(
		&volgaAuth{UserID: 42},
		DefaultVolgaConfig(),
		stats,
		&relayClient{stats: stats},
		func(pkt []byte) { got = append(got, string(pkt)) },
	)

	w.handleOrderedBundleWithSeq(99, 10, true, makeOrderedTestBundle(t, 99, 10, []byte("ten")))
	w.handleOrderedBundleWithSeq(99, 12, true, makeOrderedTestBundle(t, 99, 12, []byte("twelve")))
	time.Sleep(40 * time.Millisecond)
	w.handleOrderedBundleWithSeq(99, 11, true, makeOrderedTestBundle(t, 99, 11, []byte("eleven")))

	if !reflect.DeepEqual(got, []string{"ten", "eleven", "twelve"}) {
		t.Fatalf("delivery=%v want [ten eleven twelve]", got)
	}
	if stats.OrderGapSkips.Load() != 0 {
		t.Fatalf("gap skips=%d want 0", stats.OrderGapSkips.Load())
	}
}

func TestVolgaV4eGapExpiresWithoutNewArrival(t *testing.T) {
	stats := &VolgaStats{}
	delivered := make(chan string, 4)
	w := newWSListener(
		&volgaAuth{UserID: 42},
		DefaultVolgaConfig(),
		stats,
		&relayClient{stats: stats},
		func(pkt []byte) { delivered <- string(pkt) },
	)

	w.handleOrderedBundleWithSeq(99, 20, true, makeOrderedTestBundle(t, 99, 20, []byte("twenty")))
	if got := <-delivered; got != "twenty" {
		t.Fatalf("first delivery=%q want twenty", got)
	}

	// Sequence 21 never arrives. Sequence 22 must be released by the timer,
	// without requiring any later WS message to wake skipExpiredGapLocked.
	w.handleOrderedBundleWithSeq(99, 22, true, makeOrderedTestBundle(t, 99, 22, []byte("twenty-two")))

	select {
	case got := <-delivered:
		if got != "twenty-two" {
			t.Fatalf("timer delivery=%q want twenty-two", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for asynchronous gap expiry")
	}

	if stats.OrderGapSkips.Load() != 1 {
		t.Fatalf("gap skips=%d want 1", stats.OrderGapSkips.Load())
	}
	pending, next := w.orderState()
	if pending != 0 || next != 23 {
		t.Fatalf("state pending=%d next=%d want 0,23", pending, next)
	}
}
''')

print("Volga V4e bounded reorder timer transform applied")
