// Command veritysim runs Verity's deterministic simulator.
//
// It is the primary development tool. Two modes:
//
//	veritysim -seed 0x1234     replay exactly one seed and print what happened
//	veritysim -seeds 1000      sweep a thousand seeds in parallel across cores
//
// The sweep runs every seed twice and compares the trace hashes, so it is
// simultaneously a soak and the determinism check: a seed that disagrees with
// itself is reported by number and the command exits non-zero.
//
// Output is deliberately free of timings, paths, and anything else that varies
// between invocations. `veritysim -seed 0x1234` run twice must print
// byte-identical text, or the tool cannot be trusted to say whether the
// simulator is reproducible.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"

	"verity/internal/echo"
	"verity/node"
	"verity/prng"
	"verity/sim"
)

func main() {
	var (
		seedFlag  = flag.String("seed", "", "replay a single seed (decimal, or 0x-prefixed hex)")
		seedsFlag = flag.Int("seeds", 0, "sweep this many seeds, in parallel across cores")
		firstFlag = flag.String("first", "1", "first seed of a sweep (decimal, or 0x-prefixed hex)")
		nodesFlag = flag.Int("nodes", 3, "cluster size")
		callsFlag = flag.Int("calls", 30, "client calls issued during the run")
		horizFlag = flag.Int64("horizon-ms", 10000, "run horizon in simulated milliseconds")
		profFlag  = flag.String("profile", "", "force a named fault profile instead of drawing one")
		traceFlag = flag.Bool("trace", false, "with -seed, write the full trace to stdout")
		schedFlag = flag.Bool("schedule", false, "with -seed, print the fault schedule")
		workFlag  = flag.Int("workers", runtime.NumCPU(), "sweep workers")
		listFlag  = flag.Bool("profiles", false, "list the fault profile catalogue and exit")
		hashFlag  = flag.Bool("hashes", false, "with -seeds, print one 'seed hash' line per seed instead of a summary")
		fileFlag  = flag.String("schedule-file", "", "run this dumped fault schedule instead of drawing one")
	)
	flag.Parse()

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	if *listFlag {
		for _, p := range sim.Profiles() {
			fmt.Fprintf(out, "%s\n", p.Name)
		}
		return
	}

	scn := scenario{
		nodes:   *nodesFlag,
		calls:   *callsFlag,
		horizon: node.Duration(*horizFlag) * node.Millisecond,
		profile: *profFlag,
	}
	// A schedule read from a file is the input end of seed minimisation: dump
	// a failing seed's schedule, comment faults out of it, and run it again to
	// see whether the failure survives. The parser ignores blank lines and
	// lines beginning with '#', so a fault can be disabled without losing the
	// record of what it was.
	if *fileFlag != "" {
		text, err := os.ReadFile(*fileFlag)
		if err != nil {
			fatal(err)
		}
		parsed, err := sim.ParseSchedule(string(text))
		if err != nil {
			fatal(fmt.Errorf("%s: %w", *fileFlag, err))
		}
		scn.schedule = &parsed
		// The schedule carries its own membership and horizon; taking them
		// from the file rather than the flags is what makes a dumped schedule
		// a complete reproducer on its own.
		scn.nodes = len(parsed.Nodes)
		scn.horizon = parsed.Horizon
	}
	if err := scn.validate(); err != nil {
		fatal(err)
	}

	switch {
	case *seedFlag != "" && *seedsFlag > 0:
		fatal(fmt.Errorf("-seed and -seeds are mutually exclusive"))

	case *seedFlag != "":
		seed, err := parseSeed(*seedFlag)
		if err != nil {
			fatal(err)
		}
		if err := replay(out, scn, seed, *traceFlag, *schedFlag); err != nil {
			fatal(err)
		}

	case *seedsFlag > 0:
		first, err := parseSeed(*firstFlag)
		if err != nil {
			fatal(err)
		}
		if !sweep(out, scn, first, *seedsFlag, *workFlag, *hashFlag) {
			out.Flush()
			os.Exit(1)
		}

	default:
		flag.Usage()
		os.Exit(2)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "veritysim:", err)
	os.Exit(2)
}

// parseSeed accepts decimal or 0x-prefixed hex, because seeds are quoted both
// ways: hex in a bug report, decimal out of a sweep counter.
func parseSeed(s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("bad seed %q: %w", s, err)
	}
	return v, nil
}

// scenario is the workload the simulator drives. It is described here rather
// than inside package sim because it is a choice about what to test, not part
// of the runtime — and it will be replaced wholesale once Raft and the
// key-value state machine exist.
type scenario struct {
	nodes    int
	calls    int
	horizon  node.Duration
	profile  string
	schedule *sim.Schedule
}

func (s scenario) validate() error {
	if s.nodes < 1 {
		return fmt.Errorf("-nodes must be at least 1")
	}
	if s.calls < 0 {
		return fmt.Errorf("-calls must not be negative")
	}
	if s.horizon <= 0 {
		return fmt.Errorf("-horizon-ms must be positive")
	}
	if s.schedule != nil && s.profile != "" {
		return fmt.Errorf("-schedule-file and -profile are mutually exclusive; the file already names its profile")
	}
	if s.profile == "" {
		return nil
	}
	for _, p := range sim.Profiles() {
		if p.Name == s.profile {
			return nil
		}
	}
	return fmt.Errorf("unknown profile %q (see -profiles)", s.profile)
}

// build constructs one run. Everything that varies between runs comes from
// seed; the scenario itself is fixed.
func (s scenario) build(seed uint64, trace *os.File) *sim.Sim {
	ids := make([]node.NodeID, s.nodes)
	for i := range ids {
		ids[i] = node.NodeID(i + 1)
	}

	cfg := sim.Config{
		Seed:    seed,
		Nodes:   ids,
		Horizon: s.horizon,
		New: func(id node.NodeID, peers []node.NodeID, _ *prng.Rand) node.Node {
			return echo.New(id, peers, 20*node.Millisecond)
		},
		Net: sim.NetConfig{
			BaseDelay:   node.Millisecond,
			DelayJitter: 500 * node.Microsecond,
			Bandwidth:   1 << 20,
			DropRate:    0.05,
			DupRate:     0.02,
		},
		Disk: sim.DiskConfig{
			WriteLatency: 100 * node.Microsecond,
			SyncLatency:  node.Millisecond,
			TornRate:     0.25,
		},
	}
	switch {
	case s.schedule != nil:
		cfg.Schedule = s.schedule
	case s.profile != "":
		for _, p := range sim.Profiles() {
			if p.Name == s.profile {
				forced := p
				cfg.Profile = &forced
				break
			}
		}
	}
	if trace != nil {
		cfg.TraceTo = trace
	}

	run := sim.New(cfg)
	for i := 0; i < s.calls; i++ {
		at := node.Time(10*node.Millisecond) + node.Time(i)*node.Time(node.Millisecond)
		run.Call(at, ids[i%len(ids)], uint64(i+1), []byte{byte(i), byte(i >> 8)})
	}
	return run
}

// replay runs one seed and reports it. The report is the reproducer: profile,
// step count, trace hash, and how many client calls came back.
func replay(out *bufio.Writer, scn scenario, seed uint64, withTrace, withSchedule bool) error {
	var traceTo *os.File
	if withTrace {
		// The trace goes to stdout ahead of the summary, so that piping the
		// whole output to a file gives a self-describing artefact.
		out.Flush()
		traceTo = os.Stdout
	}

	run := scn.build(seed, traceTo)
	if withSchedule {
		out.Flush()
		fmt.Fprint(os.Stdout, run.Schedule().String())
	}
	if err := run.Run(); err != nil {
		return fmt.Errorf("seed %#016x: %w", seed, err)
	}

	replied := 0
	failed := 0
	for _, r := range run.Replies() {
		if r.Err != nil {
			failed++
			continue
		}
		replied++
	}

	fmt.Fprintf(out, "seed      %#016x\n", seed)
	fmt.Fprintf(out, "profile   %s\n", run.Schedule().Profile)
	fmt.Fprintf(out, "faults    %d\n", len(run.Schedule().Faults))
	fmt.Fprintf(out, "steps     %d\n", run.Steps())
	fmt.Fprintf(out, "hash      %#016x\n", run.Hash())
	fmt.Fprintf(out, "calls     %d\n", scn.calls)
	fmt.Fprintf(out, "replied   %d\n", replied)
	fmt.Fprintf(out, "errored   %d\n", failed)
	fmt.Fprintf(out, "unanswered %d\n", scn.calls-replied-failed)
	return nil
}

// result is one seed's verdict in a sweep.
type result struct {
	seed     uint64
	profile  string
	steps    uint64
	hashA    uint64
	hashB    uint64
	replied  int
	runErr   error
	mismatch bool
}

// sweep runs count seeds across workers goroutines, twice each, and reports
// any seed whose two runs disagree. It returns false if anything failed.
//
// Concurrency lives here and nowhere below: each run is single-threaded and
// touches nothing shared, and results are written into a preallocated slot per
// seed rather than sent down a channel, so the printed output does not depend
// on which worker finished first. A sweep whose report changed between
// invocations would be a poor tool for proving reproducibility.
//
// With hashes set, the output is one "seed hash" line per seed and nothing
// else. That form exists for the other half of M1's exit criterion: CI runs
// the same sweep on Linux, macOS and Windows and diffs the three files, which
// is what turns "deterministic on my machine" into "deterministic".
func sweep(out *bufio.Writer, scn scenario, first uint64, count, workers int, hashes bool) bool {
	if workers < 1 {
		workers = 1
	}
	results := make([]result, count)

	var next int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				i := int(next)
				next++
				mu.Unlock()
				if i >= count {
					return
				}
				results[i] = runOnce(scn, first+uint64(i))
			}
		}()
	}
	wg.Wait()

	var bad []result
	totalSteps := uint64(0)
	byProfile := map[string]int{}
	for _, r := range results {
		totalSteps += r.steps
		byProfile[r.profile]++
		if r.mismatch || r.runErr != nil {
			bad = append(bad, r)
		}
	}

	if hashes {
		// results is already in seed order, so no sort is needed here — but
		// say so, because a reader checking for INV-6 problems should not have
		// to re-derive it.
		for _, r := range results {
			fmt.Fprintf(out, "%#016x %#016x\n", r.seed, r.hashA)
		}
		return len(bad) == 0
	}

	// Profile names come out of a map, so they are sorted before printing
	// (INV-6). An unsorted range here would make the summary differ between
	// two runs of an otherwise identical sweep.
	names := make([]string, 0, len(byProfile))
	for name := range byProfile {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintf(out, "seeds     %d (%#016x..%#016x)\n", count, first, first+uint64(count)-1)
	fmt.Fprintf(out, "workers   %d\n", workers)
	fmt.Fprintf(out, "steps     %d\n", totalSteps)
	for _, name := range names {
		fmt.Fprintf(out, "profile   %-12s %d\n", name, byProfile[name])
	}

	if len(bad) == 0 {
		fmt.Fprintf(out, "result    OK — every seed reproduced itself\n")
		return true
	}

	sort.Slice(bad, func(i, j int) bool { return bad[i].seed < bad[j].seed })
	fmt.Fprintf(out, "result    FAILED — %d of %d seeds\n", len(bad), count)
	for _, r := range bad {
		switch {
		case r.runErr != nil:
			fmt.Fprintf(out, "  %#016x  error: %v\n", r.seed, r.runErr)
		default:
			fmt.Fprintf(out, "  %#016x  nondeterministic: %#016x vs %#016x  (replay with -seed %#x -trace)\n",
				r.seed, r.hashA, r.hashB, r.seed)
		}
	}
	return false
}

// runOnce runs one seed twice and compares. Neither run writes a trace: the
// hash is the comparison, and writing two traces per seed across a thousand
// seeds would be gigabytes of I/O for runs that almost always match. A seed
// that fails here is replayed with -trace to find out where.
func runOnce(scn scenario, seed uint64) result {
	a := scn.build(seed, nil)
	errA := a.Run()

	b := scn.build(seed, nil)
	errB := b.Run()

	r := result{
		seed:    seed,
		profile: a.Schedule().Profile,
		steps:   a.Steps(),
		hashA:   a.Hash(),
		hashB:   b.Hash(),
		replied: len(a.Replies()),
	}
	switch {
	case errA != nil:
		r.runErr = errA
	case errB != nil:
		r.runErr = errB
	}
	r.mismatch = a.Hash() != b.Hash() || a.Steps() != b.Steps()
	return r
}
