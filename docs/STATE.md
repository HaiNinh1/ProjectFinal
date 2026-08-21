# Verity — state ledger

Where the work actually stands. **Update this file as part of every task**, not
afterwards. A session that picks this project up cold reads `SPEC.md` for the
design, `ROADMAP.md` for the plan, and this file for the position.

Last updated: 2026-08-21

---

## Position

**Milestone:** M2 — consensus, part one. T2.1 is done; the `raft` package
exists and holds terms, the log, the durable record codec, and vote
persistence.

**Next task:** T2.2 — leader election with randomised timeouts drawn from the
injected `prng`.

**Still outstanding from M1**, and not something this machine can settle:
`.github/workflows/ci.yml` has never run. M1's exit criterion is a
byte-identical trace *across two different machines*, and the cross-machine
half is exactly what CI is there to prove. Push the branch, watch the
`cross-machine` job go green, then move M1 to closed. Until then it is "done
pending CI", which is not the same thing.

The single-machine half is settled: 100 runs of one seed and 1000 seeds twice
each all reproduce byte for byte.

---

## Done

| Task | What landed | Verified by |
|---|---|---|
| T1.1 | Go module `verity`; `node/`, `internal/policy/`, `docs/`; import guard covering INV-1…INV-5, skipping packages not yet created | `go test ./internal/policy` passes |
| T1.2 | `node/api.go` — `Time`, `Duration`, IDs, five events, six actions, `Record`, `Node` | `go build ./node` clean; package imports nothing |
| T1.3 | `prng`: splitmix64, zero imports, no global state, `Split(id)` per-node streams | `go test ./prng`. Golden vectors pin the algorithm. `Split` derives from the **seed**, not the running state, so a stream is the same however much the parent has drawn and whatever order siblings were split in — tested directly |
| T1.4 | `sim/timeline.go`: hand-written min-heap keyed by `(Time, seq)`, virtual clock, horizon | `go test ./sim -run Timeline`. 1000 items at one identical timestamp come out in insertion order (INV-7); a 10k-item stress case is checked against a reference sort |
| T1.5 | `sim/net.go`: delay, jitter, bandwidth, drop, duplicate, directed partitions | `go test ./sim -run Net`. Asymmetric partition proven: `Partition(1,2)` blocks 1→2 and leaves 2→1 alone. Draw order is documented and fixed |
| T1.6 | `sim/disk.go`: write/sync latency, unsynced tail lost on crash, torn last record | `go test ./sim -run Disk`. Crash without sync loses the tail; a torn write is rejected by the frame loader over 100 seeds and never returned as data |
| T1.7 | `internal/frame`: `length \| crc32 \| payload`, shared by the sim disk and (later) `store` | `go test ./internal/frame`. Exhaustive truncation and bit-flip sweeps; every rejection reports the offset of the frame that failed |
| T1.8 | `sim/fault.go`: seven swarm profiles, schedule drawn from the seed, dumpable and re-loadable text; `sim.Config.Schedule` and `veritysim -schedule-file` run one back | `go test ./sim -run 'Fault\|Profile\|Schedule'`. Round-trips byte-identically for every profile. Verified by hand: dumping seed `0x1234`'s schedule and replaying it from the file reproduces hash `0x705d02f1ae17adf2` exactly; commenting one fault out changes it |
| T1.9 | `sim/trace.go`: one line per `Step`, rolling FNV-1a; golden file `sim/testdata/echo3.trace` | `go test ./sim -run TestGoldenTrace`. Regenerate with `-update` |
| T1.10 | `internal/echo`: majority-acknowledged echo node, a real `node.Node` under `sim` | `go test ./sim -run EchoCluster`. 3 nodes, 100 calls, `DropRate` 0.1 — all 100 replied, each exactly once |
| — | `sim/sim_test.go`: the driver tested directly rather than only through the echo node — timer replacement, clearing, the crash boundary, clock skew, action ordering, config rejection, and node-order independence | `go test ./sim -run TestSim`. The D13 crash-timer test was checked by mutation: resetting the generation counter on crash makes a pre-crash timer arrive at the restarted node, and the test catches it |
| T1.11 | **Determinism test** — 100 seeds × 2 runs, plus one seed × 100 runs; dumps and diffs both traces on mismatch | `go test ./sim -run TestDeterminism`. **The mutation check was performed**: commenting out the INV-6 sort in `echo.onTimerFired` makes it fail on the first seed with a three-line localised diff. Reverted afterwards |
| T1.12 | `cmd/veritysim`: `-seed` to replay, `-seeds N` to sweep across cores, `-hashes`, `-trace`, `-schedule`, `-profiles` | `go run ./cmd/veritysim -seed 0x1234` twice → byte-identical output. `-seeds 1000` → every seed reproduced itself, all seven profiles exercised |
| T1.13 | `.github/workflows/ci.yml`: gofmt, vet, test, `-race`, a 500-seed sweep, on Linux/macOS/Windows, plus a `cross-machine` job that diffs per-seed trace hashes between the three | **Written, never run.** See Position above |
| T2.1 | `raft`: terms and roles, the replicated log with a sentinel ahead of it, the codec for all three durable record kinds, `Restore`, and vote persistence with the response gated behind `PersistDone` (**R1**). 984 lines of source, 2419 of tests | `go test ./raft` — 84 test functions, 130 cases with subtests. **Mutation-checked twice.** Returning the vote response in the same batch as its `Persist` (SPEC §3: ordering within a batch is not a durability guarantee) fails nine tests, the first on the very first granted vote. Removing the B2 staleness guard fails three more, including the randomised sweep |
| — | The import guard rewritten to parse source rather than shell out to `go list`, after it was caught reporting a cached pass for a package that already existed | `go test ./internal/policy`. Verified by regression: with a throwaway `kvsm` importing `time`, the guard now fails *without* `-count=1`, where before it returned `(cached) PASS`. See D18 |

---

## Decisions taken

Recorded so they are not re-litigated. Reopen one only with a reason.

| # | Decision | Why |
|---|---|---|
| D1 | Go, not Rust | Strong `Step`-based precedent (etcd `raft`); Porcupine and etcd are Go, making the RQ3 comparison like-for-like; the borrow checker fights Raft's shared-mutable shape hard enough to cost weeks this schedule lacks. |
| D2 | Nodes have **zero** dependencies rather than injected clock/net/disk interfaces | Stronger and simpler. Time arrives as an argument, I/O leaves as data. Enables the import guard to be exhaustive rather than partial. |
| D3 | Own `node.Time`/`node.Duration` | Makes mixing in a wall clock a compile error, not a review catch. |
| D4 | Own `prng`; `math/rand` banned outright | Removes the exception the guard would otherwise need, and guarantees the generator never changes underneath a recorded seed. Golden vectors in `prng_test.go` enforce it. |
| D5 | Single-server membership change (Raft §4), not joint consensus | Materially simpler to get right, sufficient for Verity. |
| D6 | Fixed 1024 shards | Avoids dynamic shard splitting, a project of its own. |
| D7 | Module path is the bare name `verity` | Chosen before the repo existed. Still open — see Q1. |
| D8 | Determinism test built in month 1, before any consensus code | Retrofitting determinism is a rewrite, not a refactor. |
| D9 | Framing lives in `internal/frame`, not in `node` or duplicated | SPEC §5.3 requires the sim disk and the real WAL to share it, and `node` imports nothing. `internal/` because it is not API. SPEC §2's layout has been updated to list it. |
| D10 | The echo fixture lives in `internal/echo` and is **added to the import guard** | It is the node the determinism test actually drives. A fixture that could read a clock would prove nothing about the harness, so it is held to INV-1…INV-5 like a real node. |
| D11 | `.gitattributes` forces LF everywhere | `core.autocrlf=true` is set on this machine, so a fresh Windows clone made `gofmt -l .` list every file — the project's own pre-commit gate, reduced to noise. Worse, it would have rewritten `sim/testdata/echo3.trace` on checkout and failed the golden-trace test on Windows while passing on Linux: a difference between machines, in the one test that exists to prove there is none. |
| D12 | The simulator hands each node a **factory**, not an instance | `node.Node` says `Restore` is called at most once, before the first `Step`, so a node cannot be rewound. Rebuilding it on restart is the only honest model of a crash: whatever was in memory is genuinely gone. |
| D13 | Timers are cancelled by a monotonic generation counter, never reset by a crash | A `TimerFired` is delivered only if its generation still matches. Resetting the counter on crash would let a fire scheduled *before* the crash match a timer armed *after* it — a bug that would appear only under crash-during-election seeds, which is the worst possible place to find one. |
| D14 | The determinism check compares the reply sequence as well as the trace hash | A trace line carries action *kinds*, not destinations or payloads, so it cannot see every reordering. Comparing replies too costs nothing and closes the gap. See Q6. |
| D15 | `sim.Config` accepts a `*Schedule` to run verbatim, and `veritysim` grows `-schedule-file` | T1.8 requires a schedule to be "dumpable as text and re-loadable", but until now nothing could *consume* a parsed schedule, so the round-trip test round-tripped into nothing. Seed minimisation (T6.1) is built entirely on this loop: dump a failing seed's schedule, comment faults out, run it again, see whether the failure survives. Verified end to end by hand. Supplying a schedule skips the fault stream's draws, which perturbs nothing — the fault stream is its own `Split`. |
| D16 | `raft` uses pure-computation stdlib (`errors`, `fmt`, `sort`, `encoding/binary`) rather than importing only `verity/node` the way `internal/echo` does | SPEC §2's "Imports node, prng" lists *verity* packages, not stdlib — `internal/frame`'s entry reads "Imports node" while the package imports four stdlib packages. `internal/policy`'s denylist is the machine-enforced boundary and permits all four; none of them can read a clock, perform I/O, start a goroutine or draw randomness. Echo's zero-import rule was a fixture's self-imposed strictness, and hand-rolling insertion sorts through a consensus implementation would trade real correctness for an invariant that is not actually at stake. |
| D17 | A replica keeps two hard states: what it believes (`n.hard`) and what it can prove (`persister.durable`) | R1 and INV-8 are both statements about the gap between the two, and a single field cannot express a gap. Making the distinction structural forces every response that depends on durability to be written as a deferred action, so forgetting to defer one becomes a visible shape in the code rather than an invisible ordering assumption. It is also what made B2 findable at all. |
| D18 | The import guard parses package source with `go/parser` instead of running `go list` | Found the hard way. `go test`'s result cache invalidates on files the test process itself opens, tracked through the testlog hooks on `os.Open`/`os.Stat`/`os.ReadDir`; it cannot see what a subprocess reads. So once `raft/` existed, `go test ./internal/policy -v` printed `verity/raft: not created yet, skipping` followed by `PASS`, reported `(cached)` — for a package that by then existed and compiled. A guard that can silently not run is worse than no guard, because it still reports green, and this one is the mechanical half of INV-1…INV-5. Parsing the files directly puts every file the verdict depends on into the cache's dependency set. The comment on `directImports` records all of this, because the obvious "simplification" is to put `go list` straight back. |

---

## Open questions

Answer before the task that depends on each.

| # | Question | Needed by |
|---|---|---|
| Q1 | Module path. Repo is `github.com/HaiNinh1/ProjectFinal` (public) but the module is bare `verity`. Either rename the module to the repo path, or rename the repo to `verity` and use `github.com/HaiNinh1/verity` — the second is cleaner. Mechanical either way, but it touches every import in the tree, so it gets cheaper the sooner it is done. | T7.3 |
| Q2 | Which MIT 6.5840 suite revision to vendor as the external oracle, and how to record that it was not authored here | T2.8 |
| Q3 | Benchmark hardware for RQ3 — one machine with containers, or cloud VMs | T6.6 |
| Q4 | Whether S3 (TLA⁺ model of migration) is attempted; depends on M5 finishing on schedule | T6.10 |
| ~~Q5~~ | ~~Trace format stability: golden-file the whole trace, or only the rolling hash?~~ **Answered: both.** The golden file catches format and sequence changes and shows *where*; the hash is what the thousand-seed sweep compares, because a trace file per seed would be gigabytes for runs that almost always match. They cost almost nothing together. | — |
| **Q6** | **Should a trace line carry a `Send`'s destination?** Found while verifying T1.11. SPEC §5.5 records action *kinds* only, so when several in-flight requests are symmetric — same payload size, same peer set — reordering them produces an identical sequence of routing calls, consumes the network's draws in the same order, and yields a **byte-identical trace even though the run genuinely diverged**. The determinism test only caught the deliberate INV-6 mutation once the workload was made asymmetric (varying payload lengths). Raft messages differ in size and content, so this may never bite in practice — but relying on the workload to be asymmetric is a weaker guarantee than the trace being sensitive by construction. Adding the destination to `Send` lines is cheap; it widens the golden file and slows the hash slightly. Decide when Raft messages land and the trace has real content to carry. | T2.7 |

---

## Known issues

| # | Issue |
|---|---|
| K1 | `go test -race` cannot run on this machine: the bundled MinGW is 32-bit and cgo fails with `cc1.exe: sorry, unimplemented: 64-bit mode not compiled in`. Nothing in the node packages is concurrent (INV-3), so the only race-relevant code today is `cmd/veritysim`'s worker pool. CI runs `-race` on all three platforms and is the place this gets covered. Install a 64-bit toolchain if it is wanted locally. |
| K2 | CI has never executed. See Position. |

---

## Bugs found

Full entries in `docs/BUGS.md`. Two so far:

- **B1** (2026-08-21, seed `0x1234`, `liveness`) — the echo node cleared the
  shared `"retry"` timer whenever *any* request completed, stranding every
  other in-flight request whose message had been dropped. Found by the first
  cluster-level test that ever ran, on its first execution. Fixed.
- **B2** (2026-08-21, no seed, `safety`) — with two writes in flight at once, a
  replica answered a vote request with a grant for a term it had already left
  and could no longer prove. Harmless in itself, because the higher term blocks
  any further vote in the older one, but harmless by a coincidence two rules
  away from the one being relied on. Found by inspection while writing the R1
  tests, before the code was committed; the randomised sweep reproduces it once
  the guard is removed. Fixed.

---

## Working protocol

For any session continuing this project:

1. Read `docs/SPEC.md` (invariants especially), then `docs/ROADMAP.md`, then
   this file.
2. Pick up at **Next task** above. Do not skip ahead; the ordering is a
   dependency ordering, not a preference.
3. `node/api.go` is the source of truth for interfaces. Prose in SPEC.md that
   contradicts it is a doc bug — fix the doc.
4. Before claiming a task done: `gofmt -l .` empty, `go vet ./...` clean,
   `go test ./...` green. Paste the actual output; do not assert it.
5. No stub implementations, no `TODO` placeholders, no skipped tests left
   behind. An unfinished task is reported as unfinished, not merged as done.
6. Update this file: move the task into **Done** with its verification, set the
   new **Next task**, and record any decision or open question the work
   surfaced.
7. Record every bug found in `docs/BUGS.md` with its seed, **the day it is
   found**. That file is a graded deliverable (D2), not a scratch pad.
8. When a test is meant to catch a class of bug, **introduce that bug once and
   watch the test fail**, then revert. T1.11 passed against a deliberate INV-6
   violation until the workload was made adversarial; the test had been green
   and worthless, and only the mutation exposed it. A test that has never
   failed has never been tested.
