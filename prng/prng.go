// Package prng is Verity's deterministic random source.
//
// Every draw of randomness inside a node — election jitter, fault injection
// choices in the simulator, shard rebalancing tie-breaks — must trace back to
// the run's seed and nothing else (INV-4, docs/SPEC.md). A stream seeded
// twice with the same value produces the same sequence forever, on any
// machine, which is what makes a recorded seed a permanent, replayable
// artifact rather than a snapshot that can silently rot.
//
// The package imports nothing, deliberately. A generator this close to the
// bottom of the dependency graph — node, raft, kvsm, shard, sim, and runtime
// all sit above it — must never be the reason an invariant-guarded package
// gains a transitive import it isn't allowed to have.
//
// The algorithm is splitmix64. It is not cryptographically secure and does
// not need to be: the goal here is reproducibility, not unpredictability.
package prng

// Rand is one deterministic stream of pseudo-random values, built with
// splitmix64.
//
// There is no package-level generator anywhere in this file: every draw is a
// method call on a *Rand the caller constructed and holds. That is what makes
// it possible to hand each node, and each subsystem within a node, its own
// independent stream (see Split) without any of them stepping on a shared,
// hidden generator.
type Rand struct {
	seed  uint64 // the value New was called with; Split derives from this, never from state
	state uint64 // splitmix64's running state; advances on every draw
}

// New constructs a stream seeded with seed. Two calls to New with the same
// seed produce byte-identical output for the entire lifetime of both
// streams; that equivalence is the whole point of the package.
func New(seed uint64) *Rand {
	return &Rand{seed: seed, state: seed}
}

// Seed reports the value this stream was constructed with. It does not
// change as the stream is drawn from — that's state, which Seed deliberately
// does not expose, since exposing running state would invite deriving new
// streams from it and reintroducing the ordering dependency Split exists to
// avoid.
func (r *Rand) Seed() uint64 { return r.seed }

// Uint64 returns the next 64 bits of the stream. This is the splitmix64
// step; every other method on Rand is built out of calls to this one.
func (r *Rand) Uint64() uint64 {
	r.state += 0x9E3779B97F4A7C15
	z := r.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// Uint64n returns a value uniformly distributed over [0, n). It panics if n
// is zero, since there is no value to return.
//
// A plain Uint64() % n would be biased whenever n does not evenly divide
// 2^64: the low remainder gets one extra draw's worth of probability mass.
// Uint64n avoids that with rejection sampling — draws that fall in the
// partial final bucket are discarded and redrawn — except when n is a power
// of two, where truncating bits is exact and rejection is unnecessary.
func (r *Rand) Uint64n(n uint64) uint64 {
	if n == 0 {
		panic("prng: Uint64n: n must be greater than zero")
	}
	if n&(n-1) == 0 {
		return r.Uint64() & (n - 1)
	}
	// 2^64's only divisors are powers of two, so for any other n the space
	// of uint64 values splits into a whole number of n-sized buckets plus a
	// short leftover bucket at the top; rem is that leftover's size, and
	// keep is the last value that belongs to a full bucket.
	rem := -n % n
	keep := ^uint64(0) - rem
	for {
		v := r.Uint64()
		if v <= keep {
			return v % n
		}
	}
}

// Intn returns a value uniformly distributed over [0, n). It panics if n is
// not positive.
func (r *Rand) Intn(n int) int {
	if n <= 0 {
		panic("prng: Intn: n must be greater than zero")
	}
	return int(r.Uint64n(uint64(n)))
}

// Between returns a value uniformly distributed over [lo, hi). It panics if
// hi is not strictly greater than lo.
func (r *Rand) Between(lo, hi int64) int64 {
	if hi <= lo {
		panic("prng: Between: hi must be greater than lo")
	}
	return lo + int64(r.Uint64n(uint64(hi-lo)))
}

// Float64 returns a value uniformly distributed over [0, 1), built from the
// top 53 bits of a draw — float64 has a 53-bit mantissa, so those are the
// only bits that can affect the result.
func (r *Rand) Float64() float64 {
	return float64(r.Uint64()>>11) / float64(uint64(1)<<53)
}

// Chance returns true with probability p. p <= 0 always returns false and
// p >= 1 always returns true.
//
// Chance always draws exactly one value from the stream, regardless of p. If
// it only drew when 0 < p < 1, then tuning a fault-injection probability
// from, say, 0.1 to 0.2 would shift every draw after that call point for the
// rest of the run, changing unrelated later decisions and breaking
// reproducibility across what should be an isolated parameter change.
func (r *Rand) Chance(p float64) bool {
	v := r.Float64()
	if p <= 0 {
		return false
	}
	if p >= 1 {
		return true
	}
	return v < p
}

// Perm returns a deterministic random permutation of [0, n).
func (r *Rand) Perm(n int) []int {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	r.Shuffle(n, func(i, j int) { p[i], p[j] = p[j], p[i] })
	return p
}

// Shuffle randomises the order of a length-n sequence by calling swap for
// each transposition, following the Fisher-Yates algorithm. It is the
// building block Perm is defined in terms of, exposed directly for callers
// shuffling something that isn't a []int — a batch of pending messages
// before simulated delivery, for instance.
func (r *Rand) Shuffle(n int, swap func(i, j int)) {
	for i := n - 1; i > 0; i-- {
		j := r.Intn(i + 1)
		swap(i, j)
	}
}

// Split derives an independent child stream identified by id.
//
// The derivation runs the splitmix64 finaliser over a mix of the PARENT's
// seed and id — never over the parent's current state. That is deliberate:
// it means Split(id) returns the same stream no matter how many values the
// parent has already produced, and no matter what order sibling ids were
// split off in. If it depended on state instead, giving a node its stream
// would depend on exactly when, relative to every other draw the parent
// made, that node happened to be created — so a node created earlier or
// later, or crash-restarted in a different order than a previous run, would
// get a different stream for reasons that have nothing to do with whatever
// bug is being reproduced.
func (r *Rand) Split(id uint64) *Rand {
	mix := r.seed + 0x9E3779B97F4A7C15*(id+1)
	z := mix
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	z = z ^ (z >> 31)
	return New(z)
}

// Clone returns an independent copy of r at its current position: the
// returned stream continues exactly where r would, but drawing from either
// one afterward does not affect the other.
func (r *Rand) Clone() *Rand {
	return &Rand{seed: r.seed, state: r.state}
}
