//go:build volga

package yandex

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePoolLane records what a pool does with one lane.
type fakePoolLane struct {
	fail  bool          // Start fails
	block chan struct{} // when set, Start waits for it or for its context

	mu      sync.Mutex
	starts  int
	started bool
	stopped bool
	sent    atomic.Int32
}

func (f *fakePoolLane) Generation() uint64 { return 1 }

func (f *fakePoolLane) Start(ctx context.Context) error {
	f.mu.Lock()
	f.starts++
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.fail {
		return errors.New("lane start failed")
	}
	f.mu.Lock()
	f.started = true
	f.mu.Unlock()
	return nil
}

func (f *fakePoolLane) Stop() error {
	f.mu.Lock()
	f.started, f.stopped = false, true
	f.mu.Unlock()
	return nil
}

func (f *fakePoolLane) SendVolgaV6(volgaV6WireFrame) error {
	f.sent.Add(1)
	return nil
}

func (f *fakePoolLane) VolgaV6PhysicalHealth(time.Time) volgaV6PhysicalHealth {
	f.mu.Lock()
	defer f.mu.Unlock()
	return volgaV6PhysicalHealth{Known: true, Connected: f.started, SessionPings: 1}
}

func (f *fakePoolLane) state() (starts int, started, stopped bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.started, f.stopped
}

// fakeLanes builds lanes for a pool: queued instances first, then working ones.
type fakeLanes struct {
	mu     sync.Mutex
	queued map[int][]*fakePoolLane
	made   map[int][]*fakePoolLane
}

func newFakeLanes() *fakeLanes {
	return &fakeLanes{queued: map[int][]*fakePoolLane{}, made: map[int][]*fakePoolLane{}}
}

func (l *fakeLanes) queue(i int, lanes ...*fakePoolLane) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queued[i] = append(l.queued[i], lanes...)
}

func (l *fakeLanes) make(i int) volgaV6PoolLane {
	l.mu.Lock()
	defer l.mu.Unlock()
	lane := &fakePoolLane{}
	if q := l.queued[i]; len(q) > 0 {
		lane, l.queued[i] = q[0], q[1:]
	}
	l.made[i] = append(l.made[i], lane)
	return lane
}

func (l *fakeLanes) instances(i int) []*fakePoolLane {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*fakePoolLane(nil), l.made[i]...)
}

func (l *fakeLanes) sentPerLane(n int) []int32 {
	out := make([]int32, n)
	for i := range out {
		for _, lane := range l.instances(i) {
			out[i] += lane.sent.Load()
		}
	}
	return out
}

func startFakePool(t *testing.T, lanes *fakeLanes, idle []int, startTimeout time.Duration) *experimentalV6Pool {
	t.Helper()
	p := newExperimentalV6Pool(1, 4, idle, startTimeout, lanes.make)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return p
}

// waitWake waits for a background wake to finish.
func waitWake(t *testing.T, p *experimentalV6Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		waking := p.waking
		p.mu.Unlock()
		if !waking {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("lane wake did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func sendFrames(t *testing.T, p *experimentalV6Pool, n int) {
	t.Helper()
	for range n {
		if err := p.SendVolgaV6(volgaV6WireFrame{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVolgaV6FirstLanePerDocument(t *testing.T) {
	a, b := "https://disk.yandex.ru/i/a", "https://disk.yandex.ru/i/b"
	for _, c := range []struct {
		docs []string
		want []int
	}{
		{[]string{a, b, a, b}, []int{0, 1}},
		{[]string{a, a, b, b}, []int{0, 2}},
		{[]string{a, a}, []int{0}},
		{[]string{a, b}, nil}, // no second lane anywhere: start every lane
		{[]string{a}, nil},
	} {
		if got := volgaV6FirstLanePerDocument(c.docs); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%d documents: first lanes %v, want %v", len(c.docs), got, c.want)
		}
	}
}

func TestVolgaV6QuietFactoryHoldsOtherLanesDown(t *testing.T) {
	a, b := "https://disk.yandex.ru/i/a", "https://disk.yandex.ru/i/b"
	docs := []string{a, b, a, b}
	if p := experimentalPool(t, VolgaV6ExperimentalOptions{Documents: docs, Quiet: true}, 1); !reflect.DeepEqual(p.idle, []int{0, 1}) {
		t.Fatalf("server idle lanes %v, want [0 1]", p.idle)
	}
	if p := experimentalPool(t, VolgaV6ExperimentalOptions{Documents: docs}, 1); p.idle != nil {
		t.Fatalf("client idle lanes %v, want every lane", p.idle)
	}
}

func TestVolgaV6WaitingServerKeepsOneLanePerDocumentUp(t *testing.T) {
	lanes := newFakeLanes()
	p := startFakePool(t, lanes, []int{0, 1}, time.Second)
	for i, want := range []bool{true, true, false, false} {
		if _, started, _ := lanes.instances(i)[0].state(); started != want {
			t.Fatalf("lane %d started=%v, want %v", i, started, want)
		}
	}
	// A lane held down is not a disconnected one.
	if h := p.VolgaV6PhysicalHealth(time.Now()); h.LanesUp != 2 || h.LanesTotal != 4 || !h.Connected || h.SessionPings != 2 {
		t.Fatalf("health up=%d/%d connected=%v pings=%d", h.LanesUp, h.LanesTotal, h.Connected, h.SessionPings)
	}
	sendFrames(t, p, 8)
	if got := lanes.sentPerLane(4); !reflect.DeepEqual(got, []int32{4, 4, 0, 0}) {
		t.Fatalf("frames per lane %v, want only the started lanes", got)
	}
}

func TestVolgaV6ClientPoolStartsEveryLane(t *testing.T) {
	lanes := newFakeLanes()
	p := startFakePool(t, lanes, nil, time.Second)
	if h := p.VolgaV6PhysicalHealth(time.Now()); h.LanesUp != 4 || !h.Connected {
		t.Fatalf("client pool up=%d connected=%v", h.LanesUp, h.Connected)
	}
}

func TestVolgaV6WakeStartsTheOtherLanes(t *testing.T) {
	lanes := newFakeLanes()
	p := startFakePool(t, lanes, []int{0, 1}, time.Second)
	now := time.Unix(6000, 0)
	p.Wake(now)
	waitWake(t, p)
	if h := p.VolgaV6PhysicalHealth(now); h.LanesUp != 4 || !h.Connected {
		t.Fatalf("after wake up=%d connected=%v", h.LanesUp, h.Connected)
	}
	sendFrames(t, p, 8)
	if got := lanes.sentPerLane(4); !reflect.DeepEqual(got, []int32{2, 2, 2, 2}) {
		t.Fatalf("frames per lane %v, want every lane", got)
	}
	// Waking an awake pool starts nothing.
	p.Wake(now.Add(time.Second))
	waitWake(t, p)
	for i := range 4 {
		if starts, _, _ := lanes.instances(i)[0].state(); starts != 1 {
			t.Fatalf("lane %d started %d times", i, starts)
		}
	}
}

func TestVolgaV6FailedLaneWakeRetriesAfterADoublingPause(t *testing.T) {
	lanes := newFakeLanes()
	lanes.queue(3, &fakePoolLane{fail: true}, &fakePoolLane{fail: true})
	p := startFakePool(t, lanes, []int{0, 1}, time.Second)
	t0 := time.Unix(7000, 0)
	wakeAt := func(s int) {
		p.Wake(t0.Add(time.Duration(s) * time.Second))
		waitWake(t, p)
	}

	wakeAt(0)
	// The session goes on with the lanes that did start.
	if h := p.VolgaV6PhysicalHealth(t0); h.LanesUp != 3 || !h.Connected {
		t.Fatalf("after a failed lane up=%d connected=%v", h.LanesUp, h.Connected)
	}
	tries := lanes.instances(3)
	if len(tries) != 2 {
		t.Fatalf("lane 3 instances=%d, want the failed one and a replacement", len(tries))
	}
	if _, _, stopped := tries[0].state(); !stopped {
		t.Fatal("the failed lane was not stopped")
	}

	wakeAt(29) // inside the 30 s pause
	if starts, _, _ := tries[1].state(); starts != 0 {
		t.Fatalf("replacement started %d times inside the pause", starts)
	}
	wakeAt(30) // fails again: the next pause is 60 s from this attempt
	wakeAt(89)
	tries = lanes.instances(3)
	if len(tries) != 3 {
		t.Fatalf("lane 3 instances=%d after the second failure, want 3", len(tries))
	}
	if starts, _, _ := tries[2].state(); starts != 0 {
		t.Fatalf("second replacement started %d times inside the doubled pause", starts)
	}
	wakeAt(90)
	if h := p.VolgaV6PhysicalHealth(t0); h.LanesUp != 4 {
		t.Fatalf("after the retry up=%d, want 4", h.LanesUp)
	}
}

func TestVolgaV6StopDuringWakeLeavesNoLaneRunning(t *testing.T) {
	lanes := newFakeLanes()
	lanes.queue(2, &fakePoolLane{block: make(chan struct{})})
	p := startFakePool(t, lanes, []int{0, 1}, time.Minute)
	p.Wake(time.Unix(8000, 0))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if starts, _, _ := lanes.instances(2)[0].state(); starts == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lane 2 never began to start")
		}
	}
	_ = p.Stop() // lane 2 is still starting
	waitWake(t, p)
	for i := range 4 {
		for _, lane := range lanes.instances(i) {
			if _, started, stopped := lane.state(); started || !stopped {
				t.Fatalf("lane %d started=%v stopped=%v after Stop", i, started, stopped)
			}
		}
	}
	if starts, _, _ := lanes.instances(3)[0].state(); starts != 0 {
		t.Fatalf("lane 3 started %d times after Stop", starts)
	}
	// A stopped pool does not wake again.
	p.Wake(time.Unix(9000, 0))
	waitWake(t, p)
	if starts, _, _ := lanes.instances(3)[0].state(); starts != 0 {
		t.Fatalf("lane 3 started %d times by a wake after Stop", starts)
	}
}

// wakeCountingCarrier is a physical carrier that counts Wake calls.
type wakeCountingCarrier struct {
	generation uint64
	wakes      atomic.Int32
}

func (c *wakeCountingCarrier) Generation() uint64                 { return c.generation }
func (c *wakeCountingCarrier) Start(context.Context) error        { return nil }
func (c *wakeCountingCarrier) SendVolgaV6(volgaV6WireFrame) error { return nil }
func (c *wakeCountingCarrier) Stop() error                        { return nil }
func (c *wakeCountingCarrier) Wake(time.Time)                     { c.wakes.Add(1) }

func TestVolgaV6PeerFrameWakesTheActiveCarrier(t *testing.T) {
	carrier := &wakeCountingCarrier{}
	factory := func(generation uint64, _ func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		carrier.generation = generation
		return carrier, nil
	}
	cfg := defaultVolgaV6RuntimeConfig()
	cfg.Quiet = true
	r := newVolgaV6Runtime(40004, factory, cfg, nil)
	start := time.Unix(9000, 0)
	if err := r.startAt(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	r.Tick(context.Background(), start.Add(time.Second))
	if n := carrier.wakes.Load(); n != 0 {
		t.Fatalf("woken %d times without a peer", n)
	}
	r.lastInbound.Store(start.Add(2 * time.Second).UnixNano())
	r.peerActivity.Store(true)
	r.Tick(context.Background(), start.Add(2*time.Second))
	if n := carrier.wakes.Load(); n != 1 {
		t.Fatalf("woken %d times after a peer frame, want 1", n)
	}
}
