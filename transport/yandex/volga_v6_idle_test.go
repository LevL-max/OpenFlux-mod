//go:build volga

package yandex

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestVolgaV6RecoveryBacksOffRecyclesWithoutProgress(t *testing.T) {
	c := newVolgaV6RecoveryController(volgaV6RecoveryConfig{ProgressStall: time.Second, RecycleCooldown: 5 * time.Second, MaxRecycleCooldown: 30 * time.Second})
	start := time.Unix(2000, 0)
	c.Observe(start, volgaV6ReliableSnapshot{})
	stalled := func(at time.Time, base uint64) volgaV6ReliableSnapshot {
		return volgaV6ReliableSnapshot{AckBase: base, ReplayDepth: 1, OldestUnackedAge: at.Sub(start)}
	}
	var got []int
	for s := 0; s <= 120; s++ {
		at := start.Add(time.Duration(s) * time.Second)
		if c.Observe(at, stalled(at, 0)).Recycle {
			got = append(got, s)
		}
	}
	// A peer that never ACKs: base cooldown twice, then doubling up to the cap.
	if want := []int{1, 6, 16, 36, 66, 96}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recycles at %v s, want %v", got, want)
	}

	// ACK progress restores the base cooldown.
	c.Observe(start.Add(121*time.Second), stalled(start.Add(121*time.Second), 1))
	got = got[:0]
	for s := 122; s <= 140; s++ {
		at := start.Add(time.Duration(s) * time.Second)
		if c.Observe(at, stalled(at, 1)).Recycle {
			got = append(got, s)
		}
	}
	if want := []int{122, 127, 137}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after progress recycles at %v s, want %v", got, want)
	}
}

func TestVolgaV6RecoveryPeerActivityRestoresRepairRate(t *testing.T) {
	c := newVolgaV6RecoveryController(volgaV6RecoveryConfig{RetryRatePerSecond: 16, RetryBurst: 1})
	start := time.Unix(3000, 0)
	c.unproductive = 5 // four doublings: 1 repair/s
	if got := c.TakeRetryBudget(start, 1); got != 1 {
		t.Fatalf("initial burst=%d", got)
	}
	if got := c.TakeRetryBudget(start.Add(500*time.Millisecond), 1); got != 0 {
		t.Fatalf("backed-off budget after 500 ms=%d, want 0", got)
	}
	c.PeerActive()
	if got := c.TakeRetryBudget(start.Add(time.Second), 1); got != 1 {
		t.Fatalf("budget after peer activity=%d, want 1", got)
	}
}

func TestVolgaV6RuntimePeerFrameEndsRecoveryBackoff(t *testing.T) {
	a, b, _, _, _, _ := newLinkedVolgaV6Pair(t, defaultVolgaV6RuntimeConfig())
	a.recovery.unproductive = 4
	if _, err := b.sendAt([][]byte{[]byte("peer")}, time.Unix(4000, 0)); err != nil {
		t.Fatal(err)
	}
	a.Tick(context.Background(), time.Unix(4000, 0))
	if a.recovery.unproductive != 0 {
		t.Fatalf("peer DATA left recovery backoff at %d", a.recovery.unproductive)
	}
}

// newLonelyVolgaV6Runtime has outstanding DATA and no peer: every frame is
// lost, like a server announcing itself to nobody.
func newLonelyVolgaV6Runtime(t *testing.T, quiet bool, start time.Time) *volgaV6Runtime {
	t.Helper()
	cfg := defaultVolgaV6RuntimeConfig()
	cfg.Quiet = quiet
	r := newVolgaV6Runtime(30003, newLinkedVolgaV6Factory().create, cfg, nil)
	if err := r.startAt(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if _, err := r.sendAt([][]byte{[]byte("hello")}, start); err != nil {
		t.Fatal(err)
	}
	return r
}

// tickSeconds ticks once per second from..to and returns the seconds with a
// carrier handoff and the repair count.
func tickSeconds(r *volgaV6Runtime, start time.Time, from, to int) (handoffs []int, repairs int) {
	for s := from; s <= to; s++ {
		res := r.Tick(context.Background(), start.Add(time.Duration(s)*time.Second))
		repairs += res.Repairs
		if res.Handoff {
			handoffs = append(handoffs, s)
		}
	}
	return handoffs, repairs
}

func TestVolgaV6QuietServerWaitsForAPeerWithoutRepairsOrRecycles(t *testing.T) {
	start := time.Unix(5000, 0)
	// A client keeps fast recovery even while nobody answers.
	if handoffs, repairs := tickSeconds(newLonelyVolgaV6Runtime(t, false, start), start, 1, 60); len(handoffs) == 0 || repairs == 0 {
		t.Fatalf("client: handoffs at %v s, repairs=%d; want both", handoffs, repairs)
	}

	r := newLonelyVolgaV6Runtime(t, true, start)
	// Nobody to deliver to: no repairs. The session pings keep a client
	// audible, so the carriers are renewed only every 20 min as a safety net.
	if handoffs, repairs := tickSeconds(r, start, 1, 2500); repairs != 0 || !reflect.DeepEqual(handoffs, []int{1200, 2400}) {
		t.Fatalf("quiet server: handoffs at %v s, repairs=%d", handoffs, repairs)
	}
	if !r.Snapshot(start.Add(2500 * time.Second)).WaitingForPeer {
		t.Fatal("snapshot does not report waiting for a peer")
	}

	// The client's first frame restores normal recovery at once.
	r.lastInbound.Store(start.Add(2500 * time.Second).UnixNano())
	r.peerActivity.Store(true)
	res := r.Tick(context.Background(), start.Add(2501*time.Second))
	if res.Repairs == 0 || res.Handoff || res.Recovery.Reason != "" {
		t.Fatalf("after the peer arrived: repairs=%d handoff=%v reason=%q", res.Repairs, res.Handoff, res.Recovery.Reason)
	}
	if r.Snapshot(start.Add(2501 * time.Second)).WaitingForPeer {
		t.Fatal("snapshot still reports waiting with a peer present")
	}
}

func TestVolgaV6QuietServerGoesQuietAfterItsPeerLeaves(t *testing.T) {
	start := time.Unix(6000, 0)
	r := newLonelyVolgaV6Runtime(t, true, start)
	r.lastInbound.Store(start.UnixNano()) // the client's last frame; the hello stays unACKed
	// A session may still be alive at first: normal recovery...
	if handoffs, repairs := tickSeconds(r, start, 1, 29); !reflect.DeepEqual(handoffs, []int{2, 7, 17}) || repairs == 0 {
		t.Fatalf("first 30 s: handoffs at %v s, repairs=%d", handoffs, repairs)
	}
	// ...then QuietAfter of silence: no repairs, a renewal 20 min after the last one.
	if handoffs, repairs := tickSeconds(r, start, 30, 2500); !reflect.DeepEqual(handoffs, []int{1217, 2417}) || repairs != 0 {
		t.Fatalf("after the peer left: handoffs at %v s, repairs=%d", handoffs, repairs)
	}
}

// pingRejectedVolgaV6Carrier reports session pings Yandex refused.
type pingRejectedVolgaV6Carrier struct {
	*linkedVolgaV6Carrier
	rejected uint64
}

func (c *pingRejectedVolgaV6Carrier) VolgaV6PhysicalHealth(time.Time) volgaV6PhysicalHealth {
	return volgaV6PhysicalHealth{Known: true, SessionPingRejected: c.rejected}
}

func TestVolgaV6RefusedSessionPingRenewsTheAuthorization(t *testing.T) {
	for _, quiet := range []bool{true, false} {
		cfg := defaultVolgaV6RuntimeConfig()
		cfg.Quiet = quiet
		var carriers []*pingRejectedVolgaV6Carrier
		r := newVolgaV6Runtime(50007, func(generation uint64, _ func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
			c := &pingRejectedVolgaV6Carrier{linkedVolgaV6Carrier: &linkedVolgaV6Carrier{generation: generation, factory: newLinkedVolgaV6Factory()}}
			carriers = append(carriers, c)
			return c, nil
		}, cfg, nil)
		start := time.Unix(7500, 0)
		if err := r.startAt(context.Background(), start); err != nil {
			t.Fatal(err)
		}
		// The editor session is gone 30 s after authorization...
		carriers[len(carriers)-1].rejected = 1
		if res := r.Tick(context.Background(), start.Add(30*time.Second)); res.Handoff {
			t.Fatalf("quiet=%v: renewed within ResubscribeSpacing", quiet)
		}
		// ...so the carriers are authorized afresh once ResubscribeSpacing has passed.
		res := r.Tick(context.Background(), start.Add(61*time.Second))
		if !res.Handoff || res.Recovery.Reason != "session-ping-rejected" {
			t.Fatalf("quiet=%v: handoff=%v reason=%q", quiet, res.Handoff, res.Recovery.Reason)
		}
		// The fresh carrier's pings are accepted: nothing more for now.
		if res := r.Tick(context.Background(), start.Add(200*time.Second)); res.Handoff {
			t.Fatalf("quiet=%v: extra handoff %q", quiet, res.Recovery.Reason)
		}
	}
}

func TestVolgaV6NewCookiesEndARenewalPause(t *testing.T) {
	cfg := defaultVolgaV6RuntimeConfig()
	cfg.Quiet = true
	changed, checks := false, 0
	cfg.credentialsChanged = func() bool { checks++; return changed }
	attempts, fail := 0, false
	r := newVolgaV6Runtime(50009, func(generation uint64, _ func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		attempts++
		if fail {
			return nil, ErrLoginRequired
		}
		return &linkedVolgaV6Carrier{generation: generation, factory: newLinkedVolgaV6Factory()}, nil
	}, cfg, nil)
	start := time.Unix(8000, 0)
	if err := r.startAt(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	// Two refused safety renewals: the next one would wait an hour (the cap).
	fail = true
	tickSeconds(r, start, 1, 3700)
	if attempts != 3 {
		t.Fatalf("%d authorizations after two refused renewals, want 3", attempts)
	}
	if checks < 3700/int(credentialCheckEvery/time.Second)-1 || checks > 3700/int(credentialCheckEvery/time.Second)+1 {
		t.Fatalf("cookie store checked %d times in 3700 s, want about every %v", checks, credentialCheckEvery)
	}
	// Fresh cookies arrive: the runtime renews within one check, not in an hour.
	changed, fail = true, false
	var at int
	for s := 3701; s <= 3720 && at == 0; s++ {
		if res := r.Tick(context.Background(), start.Add(time.Duration(s)*time.Second)); res.Handoff {
			if res.Recovery.Reason != "credentials-changed" {
				t.Fatalf("renewed for %q", res.Recovery.Reason)
			}
			at = s
		}
	}
	if at == 0 || at > 3701+int(credentialCheckEvery/time.Second) {
		t.Fatalf("renewal after new cookies at %d s", at)
	}
}

// reconnectingVolgaV6Carrier reports push socket reconnects like the Yandex carrier.
type reconnectingVolgaV6Carrier struct {
	*linkedVolgaV6Carrier
	reconnects uint64
}

func (c *reconnectingVolgaV6Carrier) VolgaV6PhysicalHealth(time.Time) volgaV6PhysicalHealth {
	return volgaV6PhysicalHealth{Known: true, WSReconnects: c.reconnects}
}

func TestVolgaV6PushSocketReconnectRenewsTheAuthorization(t *testing.T) {
	for _, quiet := range []bool{true, false} {
		cfg := defaultVolgaV6RuntimeConfig()
		cfg.Quiet = quiet
		var carriers []*reconnectingVolgaV6Carrier
		attempts, fail := 0, false
		r := newVolgaV6Runtime(50005, func(generation uint64, _ func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
			attempts++
			if fail {
				return nil, errors.New("authorization refused")
			}
			c := &reconnectingVolgaV6Carrier{linkedVolgaV6Carrier: &linkedVolgaV6Carrier{generation: generation, factory: newLinkedVolgaV6Factory()}}
			carriers = append(carriers, c)
			return c, nil
		}, cfg, nil)
		start := time.Unix(7000, 0)
		if err := r.startAt(context.Background(), start); err != nil {
			t.Fatal(err)
		}
		// The push socket reconnects 30 s after authorization...
		carriers[len(carriers)-1].reconnects = 1
		if res := r.Tick(context.Background(), start.Add(30*time.Second)); res.Handoff {
			t.Fatalf("quiet=%v: renewed within ResubscribeSpacing", quiet)
		}
		// ...so the carrier is authorized afresh once ResubscribeSpacing has passed.
		res := r.Tick(context.Background(), start.Add(61*time.Second))
		if !res.Handoff || res.Recovery.Reason != "websocket-resubscribe" {
			t.Fatalf("quiet=%v: handoff=%v reason=%q", quiet, res.Handoff, res.Recovery.Reason)
		}
		// The fresh carrier's socket has not reconnected: nothing more for now.
		if res := r.Tick(context.Background(), start.Add(120*time.Second)); res.Handoff {
			t.Fatalf("quiet=%v: extra handoff %q", quiet, res.Recovery.Reason)
		}

		// A refused renewal (a CAPTCHA, for example) is not retried every tick:
		// the next attempt waits twice the spacing, and so does the quiet renewal.
		carriers[len(carriers)-1].reconnects, fail = 1, true
		before := attempts
		for s := 201; s <= 320; s++ {
			r.Tick(context.Background(), start.Add(time.Duration(s)*time.Second))
		}
		if attempts-before != 1 {
			t.Fatalf("quiet=%v: %d renewal attempts in the 120 s after a refusal, want 1", quiet, attempts-before)
		}
		fail = false
		if res := r.Tick(context.Background(), start.Add(321*time.Second)); !res.Handoff {
			t.Fatalf("quiet=%v: no renewal after the doubled spacing", quiet)
		}
	}
}

func TestVolgaV6RuntimeStopsRepeatingAckAfterPeerSilence(t *testing.T) {
	cfg := defaultVolgaV6RuntimeConfig()
	cfg.AckRepeatInterval = time.Millisecond
	cfg.AckRepeatWindow = 50 * time.Millisecond
	a, b, _, factoryB, _, _ := newLinkedVolgaV6Pair(t, cfg)
	acks := func() int {
		n := 0
		for _, s := range factoryB.sentSnapshot() {
			if s.frame.Kind == volgaV6FrameAck {
				n++
			}
		}
		return n
	}
	if _, err := a.Send([][]byte{[]byte("one")}); err != nil {
		t.Fatal(err)
	}
	if err := b.repeatAckIfDue(time.Now()); err != nil || acks() != 1 {
		t.Fatalf("new DATA not ACKed: err=%v acks=%d", err, acks())
	}
	time.Sleep(5 * time.Millisecond)
	if err := b.repeatAckIfDue(time.Now()); err != nil || acks() != 2 {
		t.Fatalf("recent peer not re-ACKed: err=%v acks=%d", err, acks())
	}
	time.Sleep(80 * time.Millisecond)
	if err := b.repeatAckIfDue(time.Now()); err != nil || acks() != 2 {
		t.Fatalf("silent peer still re-ACKed: err=%v acks=%d", err, acks())
	}
	if _, err := a.Send([][]byte{[]byte("two")}); err != nil {
		t.Fatal(err)
	}
	if err := b.repeatAckIfDue(time.Now()); err != nil || acks() != 3 {
		t.Fatalf("returning peer not ACKed: err=%v acks=%d", err, acks())
	}
}
