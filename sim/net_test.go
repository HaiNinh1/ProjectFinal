package sim

import (
	"testing"

	"verity/node"
	"verity/prng"
)

// netNodes are the stand-in node IDs used throughout this file.
const (
	netA node.NodeID = 1
	netB node.NodeID = 2
	netC node.NodeID = 3
	netD node.NodeID = 4
)

// netCleanCfg returns a NetConfig with every parameter at its zero value, for
// tests that want to override exactly one parameter.
func netCleanCfg() NetConfig {
	return NetConfig{}
}

func TestNetBaselineDelay(t *testing.T) {
	cfg := netCleanCfg()
	cfg.BaseDelay = 10 * node.Millisecond
	n := NewNet(cfg, prng.New(1))

	const now node.Time = 1000
	want := now.Add(cfg.BaseDelay)
	for i := 0; i < 1000; i++ {
		got := n.Route(now, netA, netB, 0)
		if len(got) != 1 {
			t.Fatalf("route %d: got %d deliveries, want 1", i, len(got))
		}
		if got[0] != want {
			t.Fatalf("route %d: got delivery %v, want %v", i, got[0], want)
		}
	}
}

func TestNetJitter(t *testing.T) {
	cfg := netCleanCfg()
	cfg.BaseDelay = 5 * node.Millisecond
	cfg.DelayJitter = 100 * node.Millisecond
	n := NewNet(cfg, prng.New(2))

	const now node.Time = 0
	const iterations = 10000
	var min, max node.Duration = cfg.DelayJitter, 0
	for i := 0; i < iterations; i++ {
		got := n.Route(now, netA, netB, 0)
		if len(got) != 1 {
			t.Fatalf("route %d: got %d deliveries, want 1", i, len(got))
		}
		offset := got[0].Sub(now) - cfg.BaseDelay
		if offset < 0 || offset >= cfg.DelayJitter {
			t.Fatalf("route %d: offset %v out of [0, %v)", i, offset, cfg.DelayJitter)
		}
		if offset < min {
			min = offset
		}
		if offset > max {
			max = offset
		}
	}

	tenth := cfg.DelayJitter / 10
	if min > tenth {
		t.Errorf("min offset %v not in lowest tenth (<= %v)", min, tenth)
	}
	if max < cfg.DelayJitter-tenth {
		t.Errorf("max offset %v not in highest tenth (>= %v)", max, cfg.DelayJitter-tenth)
	}
}

func TestNetBandwidth(t *testing.T) {
	cfg := netCleanCfg()
	cfg.Bandwidth = 1000 // bytes/sec

	cases := []struct {
		size int
		want node.Duration
	}{
		{size: 0, want: 0},
		{size: -5, want: 0}, // guarded: non-positive size adds no transmission delay
		{size: 100, want: 100 * node.Millisecond},
		{size: 200, want: 200 * node.Millisecond}, // double the size, double the delay
		{size: 1000, want: 1 * node.Second},
	}
	for _, tc := range cases {
		n := NewNet(cfg, prng.New(3))
		const now node.Time = 0
		got := n.Route(now, netA, netB, tc.size)
		if len(got) != 1 {
			t.Fatalf("size %d: got %d deliveries, want 1", tc.size, len(got))
		}
		if got[0] != now.Add(tc.want) {
			t.Errorf("size %d: got delay %v, want %v", tc.size, got[0].Sub(now), tc.want)
		}
	}

	// Bandwidth 0 means unlimited: no transmission delay regardless of size.
	unlimited := netCleanCfg()
	n := NewNet(unlimited, prng.New(4))
	const now node.Time = 0
	got := n.Route(now, netA, netB, 1_000_000)
	if len(got) != 1 || got[0] != now {
		t.Errorf("unlimited bandwidth: got %v, want delivery at %v", got, now)
	}
}

func TestNetDropRate(t *testing.T) {
	const now node.Time = 0

	t.Run("never", func(t *testing.T) {
		cfg := netCleanCfg()
		cfg.DropRate = 0.0
		n := NewNet(cfg, prng.New(5))
		for i := 0; i < 10000; i++ {
			if got := n.Route(now, netA, netB, 0); got == nil {
				t.Fatalf("route %d: dropped with DropRate 0.0", i)
			}
		}
	})

	t.Run("always", func(t *testing.T) {
		cfg := netCleanCfg()
		cfg.DropRate = 1.0
		n := NewNet(cfg, prng.New(6))
		for i := 0; i < 10000; i++ {
			if got := n.Route(now, netA, netB, 0); got != nil {
				t.Fatalf("route %d: delivered with DropRate 1.0: %v", i, got)
			}
		}
	})

	t.Run("roughly half and deterministic", func(t *testing.T) {
		cfg := netCleanCfg()
		cfg.DropRate = 0.5
		const iterations = 10000

		na := NewNet(cfg, prng.New(7))
		nb := NewNet(cfg, prng.New(7))
		drops := make([]bool, iterations)
		dropped := 0
		for i := 0; i < iterations; i++ {
			gotA := na.Route(now, netA, netB, 0)
			gotB := nb.Route(now, netA, netB, 0)
			if (gotA == nil) != (gotB == nil) {
				t.Fatalf("route %d: same seed disagreed on drop", i)
			}
			drops[i] = gotA == nil
			if drops[i] {
				dropped++
			}
		}
		frac := float64(dropped) / float64(iterations)
		if frac < 0.45 || frac > 0.55 {
			t.Errorf("dropped %.4f of routes, want ~0.5 (0.45-0.55)", frac)
		}
	})
}

func TestNetDupRate(t *testing.T) {
	const now node.Time = 0

	t.Run("never", func(t *testing.T) {
		cfg := netCleanCfg()
		cfg.DupRate = 0.0
		n := NewNet(cfg, prng.New(8))
		for i := 0; i < 10000; i++ {
			if got := n.Route(now, netA, netB, 0); len(got) != 1 {
				t.Fatalf("route %d: got %d deliveries, want 1", i, len(got))
			}
		}
	})

	t.Run("always", func(t *testing.T) {
		cfg := netCleanCfg()
		cfg.DupRate = 1.0
		cfg.DelayJitter = 50 * node.Millisecond
		n := NewNet(cfg, prng.New(9))
		differed := false
		for i := 0; i < 10000; i++ {
			got := n.Route(now, netA, netB, 0)
			if len(got) != 2 {
				t.Fatalf("route %d: got %d deliveries, want 2", i, len(got))
			}
			if got[0] != got[1] {
				differed = true
			}
		}
		if !differed {
			t.Error("with jitter > 0, the two draws never differed across 10000 routes")
		}
	})

	t.Run("roughly half", func(t *testing.T) {
		cfg := netCleanCfg()
		cfg.DupRate = 0.5
		const iterations = 10000
		n := NewNet(cfg, prng.New(10))
		doubled := 0
		for i := 0; i < iterations; i++ {
			got := n.Route(now, netA, netB, 0)
			if len(got) == 2 {
				doubled++
			} else if len(got) != 1 {
				t.Fatalf("route %d: got %d deliveries, want 1 or 2", i, len(got))
			}
		}
		frac := float64(doubled) / float64(iterations)
		if frac < 0.45 || frac > 0.55 {
			t.Errorf("duplicated %.4f of routes, want ~0.5 (0.45-0.55)", frac)
		}
	})
}

// TestNetAsymmetricPartition is T1.5's headline test: proves a directed
// partition blocks exactly one direction, that Isolate blocks both, and that
// Heal/HealAll restore delivery.
func TestNetAsymmetricPartition(t *testing.T) {
	cfg := netCleanCfg()
	cfg.BaseDelay = 1 * node.Millisecond
	n := NewNet(cfg, prng.New(11))
	const now node.Time = 0

	n.Partition(netA, netB)
	if got := n.Route(now, netA, netB, 0); got != nil {
		t.Errorf("Partition(A,B): Route(A,B) = %v, want nil", got)
	}
	if got := n.Route(now, netB, netA, 0); got == nil {
		t.Error("Partition(A,B): Route(B,A) = nil, want a delivery")
	}
	if !n.Blocked(netA, netB) {
		t.Error("Blocked(A,B) = false, want true")
	}
	if n.Blocked(netB, netA) {
		t.Error("Blocked(B,A) = true, want false")
	}

	n.Isolate(netC, netD)
	if !n.Blocked(netC, netD) || !n.Blocked(netD, netC) {
		t.Error("Isolate(C,D) did not block both directions")
	}

	n.Heal(netA, netB)
	if n.Blocked(netA, netB) {
		t.Error("Heal(A,B): Blocked(A,B) = true, want false")
	}
	if got := n.Route(now, netA, netB, 0); got == nil {
		t.Error("Heal(A,B): Route(A,B) = nil, want a delivery")
	}
	if !n.Blocked(netC, netD) {
		t.Error("Heal(A,B) affected an unrelated link")
	}

	n.HealAll()
	if n.Blocked(netC, netD) || n.Blocked(netD, netC) {
		t.Error("HealAll did not clear Isolate(C,D)")
	}
	if len(n.BlockedLinks()) != 0 {
		t.Errorf("BlockedLinks after HealAll = %v, want empty", n.BlockedLinks())
	}
}

// TestNetBlockedConsumesNoDraws proves step 1 of the draw order: routing a
// blocked pair does not perturb the stream seen by later, unblocked routes.
func TestNetBlockedConsumesNoDraws(t *testing.T) {
	cfg := netCleanCfg()
	cfg.DropRate = 0.3
	cfg.DupRate = 0.3
	cfg.DelayJitter = 20 * node.Millisecond
	const seed = 12
	const now node.Time = 0

	a := NewNet(cfg, prng.New(seed))
	a.Partition(netA, netB)
	for i := 0; i < 5; i++ {
		if got := a.Route(now, netA, netB, 10); got != nil {
			t.Fatalf("blocked route %d unexpectedly delivered: %v", i, got)
		}
	}
	gotA := a.Route(now, netC, netD, 10)

	b := NewNet(cfg, prng.New(seed))
	gotB := b.Route(now, netC, netD, 10)

	if !netTimesEqual(gotA, gotB) {
		t.Errorf("blocked routes perturbed the stream: got %v, want %v", gotA, gotB)
	}
}

// TestNetDeterminism drives two Nets seeded identically through the same
// scripted sequence of routes mixing every parameter, and checks the outputs
// agree element by element.
func TestNetDeterminism(t *testing.T) {
	cfg := NetConfig{
		BaseDelay:   2 * node.Millisecond,
		DelayJitter: 15 * node.Millisecond,
		Bandwidth:   2000,
		DropRate:    0.2,
		DupRate:     0.2,
	}
	const seed = 13
	a := NewNet(cfg, prng.New(seed))
	b := NewNet(cfg, prng.New(seed))

	pairs := []struct{ from, to node.NodeID }{
		{netA, netB}, {netB, netA}, {netA, netC}, {netC, netD}, {netD, netA},
	}
	a.Partition(netD, netA)
	b.Partition(netD, netA)

	var now node.Time
	for i := 0; i < 200; i++ {
		p := pairs[i%len(pairs)]
		size := i % 500
		if i == 50 {
			a.Isolate(netB, netC)
			b.Isolate(netB, netC)
		}
		if i == 120 {
			a.HealAll()
			b.HealAll()
		}
		gotA := a.Route(now, p.from, p.to, size)
		gotB := b.Route(now, p.from, p.to, size)
		if !netTimesEqual(gotA, gotB) {
			t.Fatalf("route %d (%d->%d, size %d): got %v vs %v", i, p.from, p.to, size, gotA, gotB)
		}
		now = now.Add(node.Millisecond)
	}
}

func TestNetBlockedLinksSorted(t *testing.T) {
	n := NewNet(netCleanCfg(), prng.New(14))
	n.Partition(netD, netC)
	n.Partition(netA, netB)
	n.Partition(netA, netA)
	n.Partition(netB, netA)

	links := n.BlockedLinks()
	want := []Link{
		{From: netA, To: netA},
		{From: netA, To: netB},
		{From: netB, To: netA},
		{From: netD, To: netC},
	}
	if len(links) != len(want) {
		t.Fatalf("BlockedLinks = %v, want %v", links, want)
	}
	for i := range want {
		if links[i] != want[i] {
			t.Errorf("BlockedLinks[%d] = %v, want %v (full: %v)", i, links[i], want[i], links)
			break
		}
	}

	n.HealAll()
	if got := n.BlockedLinks(); len(got) != 0 {
		t.Errorf("BlockedLinks after HealAll = %v, want empty", got)
	}
}

func TestNetDropBeatsDuplicate(t *testing.T) {
	cfg := netCleanCfg()
	cfg.DropRate = 1.0
	cfg.DupRate = 1.0
	n := NewNet(cfg, prng.New(15))
	const now node.Time = 0
	for i := 0; i < 100; i++ {
		if got := n.Route(now, netA, netB, 0); got != nil {
			t.Fatalf("route %d: got %v, want nil (drop decided before duplicate)", i, got)
		}
	}
}

// netTimesEqual compares two Route results for exact equality.
func netTimesEqual(a, b []node.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
