package prng

import "testing"

// TestGoldenVectors locks the splitmix64 sequence to specific literals so a
// seed recorded today can never silently come to mean a different sequence
// after some future refactor of this package (project decision D4). These
// values were produced by this exact algorithm, not invented.
func TestGoldenVectors(t *testing.T) {
	wantSeed0 := []uint64{
		0xE220A8397B1DCDAF,
		0x6E789E6AA1B965F4,
		0x06C45D188009454F,
		0xF88BB8A8724C81EC,
		0x1B39896A51A8749B,
		0x53CB9F0C747EA2EA,
		0x2C829ABE1F4532E1,
		0xC584133AC916AB3C,
	}
	wantSeed1234 := []uint64{
		0x5F642F87D5E23888,
		0x5A4D78533D034CB5,
		0x8A85FFDAEA35A5A6,
		0xAD002EDB4259D53A,
		0x807B1F5869C624BC,
		0x1E2AC6B725FC033E,
		0x3071C63893F2DCBD,
		0x82642FEA4A219753,
	}
	checkGolden(t, New(0), wantSeed0)
	checkGolden(t, New(0x1234), wantSeed1234)
}

func checkGolden(t *testing.T, r *Rand, want []uint64) {
	t.Helper()
	for i, w := range want {
		got := r.Uint64()
		if got != w {
			t.Fatalf("draw %d: got 0x%016X, want 0x%016X", i, got, w)
		}
	}
}

func TestSeedReported(t *testing.T) {
	r := New(0xC0FFEE)
	if r.Seed() != 0xC0FFEE {
		t.Fatalf("Seed() = 0x%X, want 0xC0FFEE", r.Seed())
	}
	r.Uint64()
	r.Uint64()
	if r.Seed() != 0xC0FFEE {
		t.Fatalf("Seed() changed after drawing: got 0x%X", r.Seed())
	}
}

func TestSameSeedSameSequenceDifferentSeedDifferentSequence(t *testing.T) {
	const n = 1000
	a := drawN(New(42), n)
	b := drawN(New(42), n)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same seed diverged at draw %d: 0x%X vs 0x%X", i, a[i], b[i])
		}
	}

	c := drawN(New(43), n)
	identical := true
	for i := range a {
		if a[i] != c[i] {
			identical = false
			break
		}
	}
	if identical {
		t.Fatal("different seeds produced identical sequences")
	}
}

func drawN(r *Rand, n int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		out[i] = r.Uint64()
	}
	return out
}

// TestSplitDerivesFromSeedNotState is the critical semantic: Split(id) must
// return the same stream regardless of how many values the parent already
// produced. It compares a child split off immediately against a child split
// off after the parent has been advanced 1000 times.
func TestSplitDerivesFromSeedNotState(t *testing.T) {
	const seed = uint64(0xABCD1234)

	fresh := New(seed).Split(7)

	advanced := New(seed)
	for i := 0; i < 1000; i++ {
		advanced.Uint64()
	}
	advancedChild := advanced.Split(7)

	a := drawN(fresh, 100)
	b := drawN(advancedChild, 100)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("Split(7) depends on parent state at draw %d: 0x%X vs 0x%X", i, a[i], b[i])
		}
	}
}

// TestSplitOrderIndependent confirms that splitting a set of ids off a
// parent gives the same streams regardless of the order the ids were split
// in — a node created third must get the same stream whether it was the
// first, second, or third node actually constructed.
func TestSplitOrderIndependent(t *testing.T) {
	const seed = uint64(0x99887766)

	r1 := New(seed)
	c1a := r1.Split(1)
	c1b := r1.Split(2)
	c1c := r1.Split(3)

	r2 := New(seed)
	c2c := r2.Split(3)
	c2a := r2.Split(1)
	c2b := r2.Split(2)

	pairs := [][2]*Rand{{c1a, c2a}, {c1b, c2b}, {c1c, c2c}}
	for idx, pair := range pairs {
		x := drawN(pair[0], 20)
		y := drawN(pair[1], 20)
		for j := range x {
			if x[j] != y[j] {
				t.Fatalf("split id %d is order-dependent at draw %d", idx+1, j)
			}
		}
	}
}

// TestSplitStreamsAreIndependent draws from 16 sibling streams and checks
// none of their values collide, which is what makes it safe to hand each
// node its own stream without one node's randomness leaking into another's.
func TestSplitStreamsAreIndependent(t *testing.T) {
	const seed = uint64(0x1357)
	seen := make(map[uint64]uint64) // value -> id that produced it first
	for id := uint64(0); id < 16; id++ {
		child := New(seed).Split(id)
		for i := 0; i < 100; i++ {
			v := child.Uint64()
			if owner, ok := seen[v]; ok {
				t.Fatalf("collision: value 0x%X produced by both id %d and id %d", v, owner, id)
			}
			seen[v] = id
		}
	}
}

func TestUint64nInRange(t *testing.T) {
	r := New(123)
	ns := []uint64{1, 2, 3, 5, 7, 16, 1000, 1<<32 - 1}
	for _, n := range ns {
		for i := 0; i < 500; i++ {
			v := r.Uint64n(n)
			if v >= n {
				t.Fatalf("Uint64n(%d) returned out-of-range value %d", n, v)
			}
		}
	}
}

func TestUint64nPanicsOnZero(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Uint64n(0) did not panic")
		}
	}()
	New(1).Uint64n(0)
}

func TestBetweenInRange(t *testing.T) {
	r := New(55)
	for i := 0; i < 1000; i++ {
		v := r.Between(-10, 10)
		if v < -10 || v >= 10 {
			t.Fatalf("Between(-10, 10) returned out-of-range value %d", v)
		}
	}
}

func TestBetweenPanicsOnEmptyRange(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Between did not panic when hi <= lo")
		}
	}()
	New(1).Between(5, 5)
}

func TestFloat64Range(t *testing.T) {
	r := New(7)
	for i := 0; i < 10000; i++ {
		v := r.Float64()
		if v < 0 || v >= 1 {
			t.Fatalf("Float64 out of [0, 1) range: %v", v)
		}
	}
}

func TestChanceBoundaries(t *testing.T) {
	r := New(42)
	for i := 0; i < 1000; i++ {
		if r.Chance(0) {
			t.Fatalf("Chance(0) returned true at draw %d", i)
		}
	}
	r2 := New(42)
	for i := 0; i < 1000; i++ {
		if !r2.Chance(1) {
			t.Fatalf("Chance(1) returned false at draw %d", i)
		}
	}
}

// TestChanceConsumesExactlyOneDraw guards the property that tuning a
// probability parameter must not shift any later draw in the run: whatever p
// is, Chance must advance the stream's state exactly as far as one bare
// Uint64 call would.
func TestChanceConsumesExactlyOneDraw(t *testing.T) {
	const seed = uint64(999)
	ps := []float64{-1, 0, 0.001, 0.5, 0.999, 1, 2}
	for _, p := range ps {
		byChance := New(seed)
		byUint64 := New(seed)
		byChance.Chance(p)
		byUint64.Uint64()
		if byChance.state != byUint64.state {
			t.Fatalf("Chance(%v) did not consume exactly one draw: state 0x%X vs 0x%X", p, byChance.state, byUint64.state)
		}
	}
}

func TestPermIsValidPermutationAndReproducible(t *testing.T) {
	const n = 50
	p1 := New(9).Perm(n)
	p2 := New(9).Perm(n)

	if len(p1) != n {
		t.Fatalf("Perm(%d) returned length %d", n, len(p1))
	}
	seen := make([]bool, n)
	for _, v := range p1 {
		if v < 0 || v >= n || seen[v] {
			t.Fatalf("Perm(%d) is not a valid permutation: value %d", n, v)
		}
		seen[v] = true
	}
	for i := range p1 {
		if p1[i] != p2[i] {
			t.Fatalf("Perm not reproducible at index %d: %d vs %d", i, p1[i], p2[i])
		}
	}
}

func TestShuffleReproducible(t *testing.T) {
	const n = 30
	a := make([]int, n)
	b := make([]int, n)
	for i := range a {
		a[i] = i
		b[i] = i
	}
	New(17).Shuffle(n, func(i, j int) { a[i], a[j] = a[j], a[i] })
	New(17).Shuffle(n, func(i, j int) { b[i], b[j] = b[j], b[i] })
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("Shuffle not reproducible at index %d: %d vs %d", i, a[i], b[i])
		}
	}
}

// TestCloneIndependentContinuation checks both halves of Clone's contract:
// the clone continues exactly where the original would have, and drawing
// from either copy afterward does not affect the other.
func TestCloneIndependentContinuation(t *testing.T) {
	const seed = uint64(2024)

	base := New(seed)
	for i := 0; i < 10; i++ {
		base.Uint64()
	}
	clone := base.Clone()

	// Advance only the clone.
	for i := 0; i < 5; i++ {
		clone.Uint64()
	}

	// The original must be unaffected: it should still match a fresh
	// generator advanced to the same point (10 draws).
	reference := New(seed)
	for i := 0; i < 10; i++ {
		reference.Uint64()
	}
	if got, want := base.Uint64(), reference.Uint64(); got != want {
		t.Fatalf("clone's draws affected the original: got 0x%X, want 0x%X", got, want)
	}

	// The clone must have genuinely continued from its fork point: it
	// should match a generator advanced 10+5 times from the same seed.
	reference2 := New(seed)
	for i := 0; i < 15; i++ {
		reference2.Uint64()
	}
	if got, want := clone.Uint64(), reference2.Uint64(); got != want {
		t.Fatalf("clone did not continue correctly from its fork point: got 0x%X, want 0x%X", got, want)
	}
}
