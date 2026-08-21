// Package sim is Verity's deterministic runtime.
//
// It drives node.Node implementations on a single thread against a virtual
// clock, a modelled network, and a modelled disk, so that a run is a pure
// function of its seed. Two runs of the same seed produce a byte-identical
// trace, on any machine, forever. That property is the project's research
// artifact; the datastore is what it is used to test.
//
// The simulator imports node. Nothing in node imports the simulator, and
// nothing outside cmd/veritysim and tests imports the simulator (INV-5). A
// node must not be able to tell which runtime it is under, or the simulation
// is no longer validating the code that is actually deployed.
//
// The pieces, in dependency order:
//
//   - timeline.go — the scheduler. A min-heap ordered strictly by (Time, seq)
//     (INV-7), and the virtual clock that follows it.
//   - net.go — the network model: delay, jitter, bandwidth, drop, duplicate,
//     and directed partitions.
//   - disk.go — the disk model: write and sync latency, an unsynced tail lost
//     on crash, and torn trailing writes.
//   - fault.go — the fault schedule, drawn from the seed before the run so it
//     can be inspected, dumped, and minimised.
//   - trace.go — one line per Step, folded into a rolling FNV-1a hash.
//   - sim.go — the loop that ties them together.
//
// Every source of variation in a run is a *prng.Rand derived from the run
// seed by an explicit Split. There is no ambient randomness anywhere, which
// is why "replay seed 0x1234" is a complete instruction.
package sim
