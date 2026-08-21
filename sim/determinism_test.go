package sim

import (
	"bytes"
	"os"
	"testing"

	"verity/node"
)

// detPayload returns a command whose LENGTH varies with i, which matters more
// than it looks.
//
// A trace line names the kinds of the actions a Step returned — "Send,Send" —
// and not their destinations or contents (docs/SPEC.md §5.5). So if every
// in-flight request carried an identical payload and went to an identical set
// of peers, then re-ordering the requests would produce an identical sequence
// of (destination, size) routing calls, consume the network's random draws in
// an identical order, and yield a byte-identical trace. The run really would
// have been nondeterministic — replies come back in a different order — and
// the trace would not show it.
//
// Varying the payload length breaks that symmetry: message size feeds the
// bandwidth term of the network delay, so a re-ordering changes delivery times
// and the divergence surfaces in the trace where it can be diffed. See
// docs/STATE.md Q6, which asks whether the trace format should carry the
// destination so this does not depend on the workload being asymmetric.
func detPayload(i int) []byte {
	b := make([]byte, 1+i%17)
	for j := range b {
		b[j] = byte(i + j)
	}
	return b
}

// detRun builds and runs one scenario at the given seed, optionally writing
// the trace to w. Everything that varies between runs must come from seed and
// nothing else; that is the property under test, so this function is
// deliberately the only place the scenario is described.
//
// The parameters are chosen to be ADVERSARIAL, not typical, and the reason is
// worth stating because a gentler scenario looks equally green while testing
// far less. Determinism can only break where the code makes a choice, so the
// scenario has to force those choices to happen:
//
//   - calls are issued 200µs apart, far faster than a request completes, so
//     that many are in flight at once. The echo node keeps its in-flight
//     requests in a map, and a map with one entry in it cannot expose an
//     iteration-order bug.
//   - DropRate is 0.35, so the shared retry timer fires often and with several
//     requests outstanding, which is precisely when iteration order becomes
//     observable.
//   - the retry interval is short relative to the call rate, for the same
//     reason.
//
// This was not the original scenario. The first version used a 0.05 drop rate
// and 1ms spacing, and a deliberately introduced `range` over a map — the
// mutation T1.11 requires the test to catch — passed it cleanly, because
// retries almost never fired with more than one request outstanding. Verified
// by re-running the mutation against these parameters; see docs/STATE.md.
func detRun(seed uint64, calls int, w *bytes.Buffer) *Sim {
	cfg := Config{
		Seed:    seed,
		Nodes:   []node.NodeID{1, 2, 3},
		New:     clusterEcho(5 * node.Millisecond),
		Horizon: 10 * node.Second,
		Net: NetConfig{
			BaseDelay:   node.Millisecond,
			DelayJitter: 500 * node.Microsecond,
			Bandwidth:   1 << 20,
			DropRate:    0.35,
			DupRate:     0.05,
		},
		Disk: DiskConfig{
			WriteLatency: 100 * node.Microsecond,
			SyncLatency:  node.Millisecond,
			TornRate:     0.25,
		},
		// Profile is left nil so each seed draws its own from the catalogue.
		// That is the swarm behaviour objective S2 asks for, and it means the
		// sweep below exercises crashes, partitions, clock skew and the rest
		// across the hundred seeds rather than only one fault mix.
	}
	if w != nil {
		cfg.TraceTo = w
	}
	s := New(cfg)
	for i := 0; i < calls; i++ {
		at := node.Time(10*node.Millisecond) + node.Time(i)*node.Time(200*node.Microsecond)
		s.Call(at, node.NodeID(1+i%3), uint64(i+1), detPayload(i))
	}
	s.Run()
	return s
}

// TestDeterminism is the project's foundation, task T1.11 and milestone M1's
// exit criterion: the same seed produces the same trace, every time.
//
// It runs each of a hundred seeds twice and compares the rolling trace hashes.
// Neither run writes a trace, because writing two traces per seed would be
// gigabytes of I/O for runs that almost always match. On a mismatch the seed
// is replayed with writers attached and the two traces are diffed, which
// localises the nondeterminism to a single Step — the difference between "some
// run somewhere disagrees" and "line 4,182, node 2, TimerFired".
//
// A change that breaks this test is a bug regardless of what else passes.
func TestDeterminism(t *testing.T) {
	const (
		seeds = 100
		calls = 30
	)

	for i := 0; i < seeds; i++ {
		seed := uint64(0x9E3779B97F4A7C15 * uint64(i+1))

		a := detRun(seed, calls, nil)
		b := detRun(seed, calls, nil)

		// The trace hash is the primary comparison, but it is not the whole
		// observable run: a trace line carries the kinds of the actions a Step
		// returned, not which peer a Send went to or which request a Reply
		// answered. Comparing the reply sequence as well closes that gap, and
		// costs nothing.
		if a.Hash() == b.Hash() && a.Steps() == b.Steps() && detRepliesEqual(a, b) {
			continue
		}

		// Replay with traces attached. This is the whole reason the trace is
		// hashed rather than only counted: the hash says a seed diverged, and
		// the replay says where.
		var bufA, bufB bytes.Buffer
		detRun(seed, calls, &bufA)
		detRun(seed, calls, &bufB)

		dir, err := os.MkdirTemp("", "verity-trace-")
		if err == nil {
			os.WriteFile(dir+"/a.trace", bufA.Bytes(), 0o644)
			os.WriteFile(dir+"/b.trace", bufB.Bytes(), 0o644)
		}

		t.Fatalf("seed %#016x is not deterministic\n"+
			"  run A: %d steps, hash %#016x, %d replies\n"+
			"  run B: %d steps, hash %#016x, %d replies\n"+
			"  replies identical: %v\n"+
			"  traces written to %s\n%s",
			seed,
			a.Steps(), a.Hash(), len(a.Replies()),
			b.Steps(), b.Hash(), len(b.Replies()),
			detRepliesEqual(a, b), dir,
			clusterDiff(bufA.String(), bufB.String()))
	}
}

// detRepliesEqual reports whether two runs answered the same requests, in the
// same order, at the same virtual times, with the same bytes.
func detRepliesEqual(a, b *Sim) bool {
	ra, rb := a.Replies(), b.Replies()
	if len(ra) != len(rb) {
		return false
	}
	for i := range ra {
		x, y := ra[i], rb[i]
		if x.At != y.At || x.From != y.From || x.ReqID != y.ReqID {
			return false
		}
		if !bytes.Equal(x.Resp, y.Resp) {
			return false
		}
		if (x.Err == nil) != (y.Err == nil) {
			return false
		}
	}
	return true
}

// TestDeterminismOneSeedHundredRuns is milestone M1's exit criterion taken
// literally: one seed, a hundred runs, a byte-identical trace every time.
//
// TestDeterminism sweeps a hundred seeds twice each, which is the better test
// of coverage. This is the better test of repetition, and they fail in
// different ways: a run that depends on how many runs preceded it passes the
// sweep and fails here.
func TestDeterminismOneSeedHundredRuns(t *testing.T) {
	const (
		seed  = 0x1234
		runs  = 100
		calls = 30
	)

	first := detRun(seed, calls, nil)
	var want bytes.Buffer
	detRun(seed, calls, &want)

	for i := 1; i < runs; i++ {
		got := detRun(seed, calls, nil)
		if got.Hash() == first.Hash() && got.Steps() == first.Steps() && detRepliesEqual(got, first) {
			continue
		}

		var buf bytes.Buffer
		detRun(seed, calls, &buf)
		t.Fatalf("run %d of %d diverged from run 1\n"+
			"  run 1: %d steps, hash %#016x\n"+
			"  run %d: %d steps, hash %#016x\n%s",
			i+1, runs, first.Steps(), first.Hash(),
			i+1, got.Steps(), got.Hash(),
			clusterDiff(want.String(), buf.String()))
	}
}

// TestDeterminismAcrossFreshProcessState guards the thing a same-process
// double run cannot: state that persists between the two runs and happens to
// be identical because it was set up identically. Running the seeds in reverse
// order and comparing against the forward pass's hashes catches a simulator
// that has come to depend on how many runs preceded it — a package-level
// counter, a sync.Pool, a cached slice.
func TestDeterminismAcrossFreshProcessState(t *testing.T) {
	const (
		seeds = 20
		calls = 20
	)

	seedAt := func(i int) uint64 { return uint64(0x9E3779B97F4A7C15 * uint64(i+1)) }

	forward := make([]uint64, seeds)
	for i := 0; i < seeds; i++ {
		forward[i] = detRun(seedAt(i), calls, nil).Hash()
	}
	for i := seeds - 1; i >= 0; i-- {
		if got := detRun(seedAt(i), calls, nil).Hash(); got != forward[i] {
			t.Fatalf("seed %#016x: hash %#016x in reverse order, %#016x in forward order",
				seedAt(i), got, forward[i])
		}
	}
}

// TestDeterminismDistinguishesSeeds is the control for TestDeterminism. A
// simulator whose trace hash did not depend on the seed at all would pass the
// determinism test perfectly while testing nothing, so assert that different
// seeds do in fact produce different traces.
func TestDeterminismDistinguishesSeeds(t *testing.T) {
	const seeds = 50

	hashes := make(map[uint64]uint64, seeds)
	for i := 0; i < seeds; i++ {
		seed := uint64(0x9E3779B97F4A7C15 * uint64(i+1))
		h := detRun(seed, 20, nil).Hash()
		if prev, ok := hashes[h]; ok {
			t.Fatalf("seeds %#016x and %#016x produce the same trace hash %#016x", prev, seed, h)
		}
		hashes[h] = seed
	}
}

// TestScheduleIsReproducibleFromSeed checks the other half of replayability:
// not only does a seed produce the same trace, it produces the same fault
// schedule, and that schedule can be dumped and read back. Without this, a
// failing seed could be replayed but not minimised.
func TestScheduleIsReproducibleFromSeed(t *testing.T) {
	for i := 0; i < 20; i++ {
		seed := uint64(0x9E3779B97F4A7C15 * uint64(i+1))

		a := detRun(seed, 5, nil).Schedule()
		b := detRun(seed, 5, nil).Schedule()

		if a.String() != b.String() {
			t.Fatalf("seed %#016x: schedule differs between runs\n--- a ---\n%s\n--- b ---\n%s",
				seed, a.String(), b.String())
		}

		parsed, err := ParseSchedule(a.String())
		if err != nil {
			t.Fatalf("seed %#016x: schedule does not parse back: %v\n%s", seed, err, a.String())
		}
		if parsed.String() != a.String() {
			t.Fatalf("seed %#016x: schedule does not round-trip\n--- before ---\n%s\n--- after ---\n%s",
				seed, a.String(), parsed.String())
		}
	}
}
