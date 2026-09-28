//go:build volga

package yandex

import "testing"

func experimentalPoolGen(t *testing.T, tr *YandexVolgaV6Transport, generation uint64) *experimentalV6Pool {
	t.Helper()
	carrier, err := tr.factory(generation, nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	pool, ok := carrier.(*experimentalV6Pool)
	if !ok {
		t.Fatalf("carrier type %T, want *experimentalV6Pool", carrier)
	}
	return pool
}

func experimentalPool(t *testing.T, o VolgaV6ExperimentalOptions, generation uint64) *experimentalV6Pool {
	t.Helper()
	tr, err := NewVolgaV6Experimental(o)
	if err != nil {
		t.Fatalf("NewVolgaV6Experimental: %v", err)
	}
	return experimentalPoolGen(t, tr, generation)
}

func TestVolgaV6ExperimentalSharedGateByDefault(t *testing.T) {
	docs := []string{"https://disk.yandex.ru/i/a", "https://disk.yandex.ru/i/b"}
	pool := experimentalPool(t, VolgaV6ExperimentalOptions{Documents: docs}, 1)
	if len(pool.lanes) != 2 {
		t.Fatalf("lanes=%d, want 2", len(pool.lanes))
	}
	g0, g1 := pool.lanes[0].config.relayGate, pool.lanes[1].config.relayGate
	if g0 == nil || g0 != g1 {
		t.Fatalf("default must share one gate across lanes: g0=%p g1=%p", g0, g1)
	}
	if g0.rate != 360 {
		t.Fatalf("default shared rate=%v, want 360", g0.rate)
	}
}

func TestVolgaV6ExperimentalPerLaneGates(t *testing.T) {
	docs := []string{"https://disk.yandex.ru/i/a", "https://disk.yandex.ru/i/b", "https://disk.yandex.ru/i/c"}
	o := VolgaV6ExperimentalOptions{Documents: docs, PostsPerSecond: 150, PerLaneBudget: true}
	tr, err := NewVolgaV6Experimental(o)
	if err != nil {
		t.Fatalf("NewVolgaV6Experimental: %v", err)
	}
	gen1 := experimentalPoolGen(t, tr, 1)
	seen := map[*volgaV6RelayGate]bool{}
	for i, lane := range gen1.lanes {
		g := lane.config.relayGate
		if g == nil || g.rate != 150 {
			t.Fatalf("lane %d gate=%p rate=%v, want distinct gate at 150", i, g, gateRate(g))
		}
		if seen[g] {
			t.Fatalf("lane %d shares a gate in per-lane mode", i)
		}
		seen[g] = true
	}

	// A second generation (handoff) must reuse the same per-lane gate instances
	// so a lane keeps its own Retry-After cooldown across re-authorization.
	gen2 := experimentalPoolGen(t, tr, 2)
	for i := range gen1.lanes {
		if gen1.lanes[i].config.relayGate != gen2.lanes[i].config.relayGate {
			t.Fatalf("lane %d gate not reused across generations", i)
		}
	}
}

func TestVolgaV6ExperimentalRejectsBadRate(t *testing.T) {
	docs := []string{"https://disk.yandex.ru/i/a"}
	if _, err := NewVolgaV6Experimental(VolgaV6ExperimentalOptions{Documents: docs, PostsPerSecond: 5000}); err == nil {
		t.Fatal("expected rejection of out-of-range posts_per_second")
	}
}

func gateRate(g *volgaV6RelayGate) float64 {
	if g == nil {
		return 0
	}
	return g.rate
}
