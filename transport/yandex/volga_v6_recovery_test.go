package yandex

import (
	"testing"
	"time"
)

func TestVolgaV6RecoveryDoesNotRecycleWithoutOutstandingData(t *testing.T) {
	cfg := defaultVolgaV6RecoveryConfig()
	cfg.ProgressStall = 500 * time.Millisecond
	controller := newVolgaV6RecoveryController(cfg)
	start := time.Unix(100, 0)

	controller.Observe(start, volgaV6ReliableSnapshot{AckBase: 10})
	decision := controller.Observe(start.Add(5*time.Second), volgaV6ReliableSnapshot{
		AckBase:          10,
		ReplayDepth:      0,
		OldestUnackedAge: 5 * time.Second,
	})
	if decision.Recycle {
		t.Fatalf("idle transport recycled: %+v", decision)
	}
}

func TestVolgaV6RecoveryProgressPreventsRecycle(t *testing.T) {
	cfg := defaultVolgaV6RecoveryConfig()
	cfg.ProgressStall = 500 * time.Millisecond
	controller := newVolgaV6RecoveryController(cfg)
	start := time.Unix(200, 0)

	controller.Observe(start, volgaV6ReliableSnapshot{AckBase: 1, ReplayDepth: 10})
	for i := 1; i <= 5; i++ {
		now := start.Add(time.Duration(i) * 400 * time.Millisecond)
		decision := controller.Observe(now, volgaV6ReliableSnapshot{
			AckBase:          uint64(i + 1),
			ReplayDepth:      10,
			OldestUnackedAge: 2 * time.Second,
		})
		if decision.Recycle {
			t.Fatalf("recycled while ACK base advanced at step %d: %+v", i, decision)
		}
	}
}

func TestVolgaV6RecoveryStallTriggersSingleRecycleWithinCooldown(t *testing.T) {
	cfg := defaultVolgaV6RecoveryConfig()
	cfg.ProgressStall = 500 * time.Millisecond
	cfg.RecycleCooldown = 2 * time.Second
	controller := newVolgaV6RecoveryController(cfg)
	start := time.Unix(300, 0)

	controller.Observe(start, volgaV6ReliableSnapshot{
		AckBase:          50,
		ReplayDepth:      20,
		OldestUnackedAge: 100 * time.Millisecond,
	})
	decision := controller.Observe(start.Add(600*time.Millisecond), volgaV6ReliableSnapshot{
		AckBase:          50,
		ReplayDepth:      20,
		OldestUnackedAge: 700 * time.Millisecond,
	})
	if !decision.Recycle || decision.Reason != "delivery-progress-stall" {
		t.Fatalf("stall decision=%+v", decision)
	}

	again := controller.Observe(start.Add(900*time.Millisecond), volgaV6ReliableSnapshot{
		AckBase:          50,
		ReplayDepth:      20,
		OldestUnackedAge: time.Second,
	})
	if again.Recycle {
		t.Fatalf("duplicate recycle inside cooldown: %+v", again)
	}
}

func TestVolgaV6RecoveryAckProgressResetsStallClock(t *testing.T) {
	cfg := defaultVolgaV6RecoveryConfig()
	cfg.ProgressStall = 500 * time.Millisecond
	cfg.RecycleCooldown = time.Second
	controller := newVolgaV6RecoveryController(cfg)
	start := time.Unix(400, 0)

	controller.Observe(start, volgaV6ReliableSnapshot{AckBase: 10, ReplayDepth: 5})
	controller.Observe(start.Add(400*time.Millisecond), volgaV6ReliableSnapshot{
		AckBase:          11,
		ReplayDepth:      5,
		OldestUnackedAge: time.Second,
	})
	decision := controller.Observe(start.Add(700*time.Millisecond), volgaV6ReliableSnapshot{
		AckBase:          11,
		ReplayDepth:      5,
		OldestUnackedAge: 1300 * time.Millisecond,
	})
	if decision.Recycle {
		t.Fatalf("stale timer ignored recent ACK progress: %+v", decision)
	}

	decision = controller.Observe(start.Add(950*time.Millisecond), volgaV6ReliableSnapshot{
		AckBase:          11,
		ReplayDepth:      5,
		OldestUnackedAge: 1550 * time.Millisecond,
	})
	if !decision.Recycle {
		t.Fatalf("stall after reset was not detected: %+v", decision)
	}
}

func TestVolgaV6RecoveryAdmissionWindowShrinksWithReplayDebt(t *testing.T) {
	cfg := defaultVolgaV6RecoveryConfig()
	cfg.MaxWindow = 400
	cfg.HighWatermark = 200
	cfg.CriticalWatermark = 300
	cfg.MinWindow = 25
	controller := newVolgaV6RecoveryController(cfg)
	start := time.Unix(500, 0)

	cases := []struct {
		depth int
		want  int
	}{
		{0, 400},
		{99, 400},
		{100, 200},
		{199, 200},
		{200, 100},
		{299, 100},
		{300, 25},
		{399, 25},
	}
	for i, tc := range cases {
		decision := controller.Observe(start.Add(time.Duration(i)*time.Millisecond), volgaV6ReliableSnapshot{
			AckBase:     uint64(i),
			ReplayDepth: tc.depth,
		})
		if decision.AdmissionLimit != tc.want {
			t.Fatalf("depth=%d admission=%d want=%d", tc.depth, decision.AdmissionLimit, tc.want)
		}
	}
}

func TestVolgaV6RecoveryRetryBudgetIsBurstAndRateBounded(t *testing.T) {
	cfg := defaultVolgaV6RecoveryConfig()
	cfg.RetryBurst = 3
	cfg.RetryRatePerSecond = 4
	controller := newVolgaV6RecoveryController(cfg)
	start := time.Unix(600, 0)
	controller.ResetRetryBudget(start)

	if got := controller.TakeRetryBudget(start, 10); got != 3 {
		t.Fatalf("initial retry burst=%d want 3", got)
	}
	if got := controller.TakeRetryBudget(start, 1); got != 0 {
		t.Fatalf("budget refilled without time passing: %d", got)
	}
	if got := controller.TakeRetryBudget(start.Add(249*time.Millisecond), 2); got != 0 {
		t.Fatalf("fractional token consumed early: %d", got)
	}
	if got := controller.TakeRetryBudget(start.Add(250*time.Millisecond), 2); got != 1 {
		t.Fatalf("250ms refill=%d want 1", got)
	}
	if got := controller.TakeRetryBudget(start.Add(time.Second), 10); got != 3 {
		t.Fatalf("refill must cap at burst=3, got %d", got)
	}
}
