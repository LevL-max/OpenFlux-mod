#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


def replace_region(text: str, start_marker: str, end_marker: str, new_block: str, label: str) -> str:
    start = text.find(start_marker)
    if start < 0:
        raise SystemExit(f"{label}: start marker not found")
    end = text.find(end_marker, start)
    if end < 0:
        raise SystemExit(f"{label}: end marker not found")
    return text[:start] + new_block + text[end:]


# Applied AFTER volga_reliable_v5.py.
#
# V5.1 keeps the V5 reliability protocol/wire format unchanged and only adds
# congestion control to selective retransmission. V5 field telemetry showed a
# retry storm under load: ~2000 retransmits in ~75s, 88% fast retries and more
# than 8200 receiver duplicates. The original fast path could retransmit the
# same unresolved hole again on every fresh ACK once 80ms elapsed.
#
# V5.1 policy:
#   * only one fast retransmit is allowed per logical batch;
#   * fast retransmit requires at least 3 later sequences as loss evidence;
#   * fast retry age raised to 180ms to tolerate ordinary Volga reordering;
#   * retransmits are oldest-hole-first and burst-limited to 8;
#   * retry concurrency is reduced 32 -> 8;
#   * subsequent timeout retries use per-batch exponential backoff capped 4s;
#   * ACK/SACK wire format, replay cap, session epochs and immediate delivery
#     are unchanged.

p = Path("transport/yandex/volga_reliable_v5.go")
s = p.read_text()

s = replace_once(s, "\tvolgaReliableRetryConcurrency = 32\n", "\tvolgaReliableRetryConcurrency = 8\n", "retry concurrency")
s = replace_once(s, "\tvolgaReliableFastRetryMin     = 80 * time.Millisecond\n", "\tvolgaReliableFastRetryMin     = 180 * time.Millisecond\n", "fast retry minimum")
s = replace_once(
    s,
    "\tvolgaReliableMaxRTO           = 2 * time.Second\n",
    "\tvolgaReliableMaxRTO           = 2 * time.Second\n"
    "\tvolgaReliableRetryBackoffMax  = 4 * time.Second\n"
    "\tvolgaReliableFastRetryDepth   = 3\n"
    "\tvolgaReliableFastRetryBurst   = 8\n"
    "\tvolgaReliableTimeoutRetryBurst = 8\n",
    "V5.1 retry bounds",
)

handle_ack = r'''func volgaReliableRetryDelay(base time.Duration, retries int) time.Duration {
	if base <= 0 {
		base = volgaReliableInitialRTO
	}
	if retries < 0 {
		retries = 0
	}
	shift := retries
	if shift > 4 {
		shift = 4
	}
	delay := base * time.Duration(1<<shift)
	if delay > volgaReliableRetryBackoffMax {
		delay = volgaReliableRetryBackoffMax
	}
	return delay
}

func volgaReliableFastRetryEligible(seq, highest uint64, e *volgaReliableReplayEntry, now time.Time) bool {
	if e == nil || e.queued || e.retries != 0 || e.lastSent.IsZero() || highest <= seq {
		return false
	}
	if highest-seq < volgaReliableFastRetryDepth {
		return false
	}
	return now.Sub(e.lastSent) >= volgaReliableFastRetryMin
}

func (r *relayClient) volgaReliableHandleAck(ack volgaReliableAck) {
	st := r.volgaReliableState()
	if ack.Session != st.session {
		return
	}
	st.ackRx.Add(1)
	now := time.Now()
	highest := ack.Base
	for _, rg := range ack.Ranges {
		if rg.End > highest {
			highest = rg.End
		}
	}

	var released int
	var samples []time.Duration
	var fast []uint64

	st.mu.Lock()
	for seq, e := range st.replay {
		if volgaReliableAckContains(ack, seq) {
			if e.retries == 0 && !e.firstSent.IsZero() {
				samples = append(samples, now.Sub(e.firstSent))
			}
			delete(st.replay, seq)
			released++
			continue
		}
		if volgaReliableFastRetryEligible(seq, highest, e, now) {
			fast = append(fast, seq)
		}
	}
	st.mu.Unlock()

	for i := 0; i < released; i++ {
		<-st.slots
	}
	if released > 0 {
		st.acked.Add(uint64(released))
	}
	for _, sample := range samples {
		r.volgaReliableObserveRTT(sample)
	}

	// Repair the oldest visible holes first. This advances cumulative ACK base
	// quickly and prevents one SACK from launching a large parallel retry wave.
	sort.Slice(fast, func(i, j int) bool { return fast[i] < fast[j] })
	if len(fast) > volgaReliableFastRetryBurst {
		fast = fast[:volgaReliableFastRetryBurst]
	}
	for _, seq := range fast {
		r.volgaReliableScheduleRetry(seq, true)
	}
}

'''.replace('\\t', '\t')
s = replace_region(
    s,
    "func (r *relayClient) volgaReliableHandleAck(ack volgaReliableAck) {\n",
    "func (r *relayClient) volgaReliableScheduleRetry(seq uint64, fast bool) {\n",
    handle_ack,
    "V5.1 ACK/retry selection",
)

schedule_retry = r'''func (r *relayClient) volgaReliableScheduleRetry(seq uint64, fast bool) {
	st := r.volgaReliableState()

	st.mu.Lock()
	e := st.replay[seq]
	if e == nil || e.queued {
		st.mu.Unlock()
		return
	}
	select {
	case st.retrySem <- struct{}{}:
	default:
		st.mu.Unlock()
		return
	}
	e.queued = true
	packets := cloneVolgaReliableBatch(e.packets)
	now := time.Now()
	if e.firstSent.IsZero() {
		e.firstSent = now
	}
	e.lastSent = now
	e.retries++
	retryNo := e.retries
	st.mu.Unlock()

	st.retransmits.Add(1)
	if fast {
		st.fastRetransmits.Add(1)
	} else {
		st.timeoutRetransmits.Add(1)
	}

	go func() {
		defer func() { <-st.retrySem }()
		utils.Debugf("[VOLGA-RELIABLE] retransmit seq=%d fast=%t attempt=%d", seq, fast, retryNo)
		err := r.postBatchV5(seq, packets)
		if err == errVolgaAdaptiveLocalSplit || err == errVolgaAdaptive413Retry {
			r.volgaReliableForget(seq)
		}
		if err != nil && err != errVolgaAdaptiveLocalSplit && err != errVolgaAdaptive413Retry {
			utils.Debugf("[VOLGA-RELIABLE] retransmit failed seq=%d: %v", seq, err)
		}
		st.mu.Lock()
		if cur := st.replay[seq]; cur != nil {
			cur.queued = false
		}
		st.mu.Unlock()
	}()
}

'''.replace('\\t', '\t')
s = replace_region(
    s,
    "func (r *relayClient) volgaReliableScheduleRetry(seq uint64, fast bool) {\n",
    "func (r *relayClient) volgaReliableRetryLoop(st *volgaReliableRelayState) {\n",
    schedule_retry,
    "V5.1 retry scheduling",
)

retry_loop = r'''func (r *relayClient) volgaReliableRetryLoop(st *volgaReliableRelayState) {
	ticker := time.NewTicker(volgaReliableRetryScan)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case now := <-ticker.C:
			st.mu.Lock()
			rto := st.rto
			if rto <= 0 {
				rto = volgaReliableInitialRTO
			}
			seqs := make([]uint64, 0, volgaReliableTimeoutRetryBurst)
			for seq, e := range st.replay {
				if e.queued || e.lastSent.IsZero() {
					continue
				}
				if now.Sub(e.lastSent) >= volgaReliableRetryDelay(rto, e.retries) {
					seqs = append(seqs, seq)
				}
			}
			st.mu.Unlock()

			sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
			if len(seqs) > volgaReliableTimeoutRetryBurst {
				seqs = seqs[:volgaReliableTimeoutRetryBurst]
			}
			for _, seq := range seqs {
				r.volgaReliableScheduleRetry(seq, false)
			}
		}
	}
}

'''.replace('\\t', '\t')
s = replace_region(
    s,
    "func (r *relayClient) volgaReliableRetryLoop(st *volgaReliableRelayState) {\n",
    "func (r *relayClient) volgaReliableSendAck(ack volgaReliableAck) error {\n",
    retry_loop,
    "V5.1 retry loop",
)

p.write_text(s)

Path("transport/yandex/volga_reliable_v5_1_test.go").write_text(r'''package yandex

import (
	"testing"
	"time"
)

func TestVolgaReliableV51RetryDelayBackoff(t *testing.T) {
	base := 250 * time.Millisecond
	cases := []struct {
		retries int
		want    time.Duration
	}{
		{0, 250 * time.Millisecond},
		{1, 500 * time.Millisecond},
		{2, 1 * time.Second},
		{3, 2 * time.Second},
		{4, 4 * time.Second},
		{8, 4 * time.Second},
	}
	for _, tc := range cases {
		if got := volgaReliableRetryDelay(base, tc.retries); got != tc.want {
			t.Fatalf("retries=%d delay=%v want=%v", tc.retries, got, tc.want)
		}
	}
}

func TestVolgaReliableV51FastRetryNeedsStrongEvidenceAndRunsOnce(t *testing.T) {
	now := time.Now()
	e := &volgaReliableReplayEntry{lastSent: now.Add(-volgaReliableFastRetryMin)}

	if volgaReliableFastRetryEligible(100, 102, e, now) {
		t.Fatal("two later sequences must not trigger fast retry")
	}
	if !volgaReliableFastRetryEligible(100, 103, e, now) {
		t.Fatal("three later sequences should trigger first fast retry")
	}

	e.retries = 1
	if volgaReliableFastRetryEligible(100, 200, e, now.Add(time.Second)) {
		t.Fatal("a batch must not receive repeated fast retransmits")
	}
}

func TestVolgaReliableV51FastRetryHonorsReorderGrace(t *testing.T) {
	now := time.Now()
	e := &volgaReliableReplayEntry{lastSent: now.Add(-(volgaReliableFastRetryMin - time.Millisecond))}
	if volgaReliableFastRetryEligible(10, 100, e, now) {
		t.Fatal("ordinary reordering inside the grace window must not trigger fast retry")
	}
}
''')

print("Volga V5.1 congestion-controlled retransmit transform applied")
