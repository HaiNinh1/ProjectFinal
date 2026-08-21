# Verity — state ledger

Where the work actually stands. **Update this file as part of every task**, not
afterwards. A session that picks this project up cold reads `SPEC.md` for the
design, `ROADMAP.md` for the plan, and this file for the position.

Last updated: 2026-08-21

---

## Position

**Milestone:** M1 — the harness, before the system
**Next task:** T1.3 — `prng` package (splitmix64, `Split(id)` per-node streams)

M1's exit criterion is the determinism test at T1.11. Everything before it is
scaffolding for that one check.

---

## Done

| Task | What landed | Verified by |
|---|---|---|
| T1.1 | Go module `verity`; `node/`, `internal/policy/`, `docs/`; import guard test covering INV-1…INV-5, skipping packages not yet created | `go test ./internal/policy` passes; skip lines confirm prng/raft/kvsm/shard are pending |
| T1.2 | `node/api.go` — `Time`, `Duration`, `NodeID`, `GroupID`, `ShardID`, `Message`, the five events, the six actions, `Record`/`RecordKind`, `Node` | `go build ./node` clean; `go vet ./...` clean; `gofmt -l .` empty; package imports nothing |

---

## Decisions taken

Recorded so they are not re-litigated. Reopen one only with a reason.

| # | Decision | Why |
|---|---|---|
| D1 | Go, not Rust | The `Step`-based architecture has a strong Go precedent (etcd `raft`); Porcupine and etcd are Go, making the RQ3 comparison like-for-like; the borrow checker fights Raft's shared-mutable shape hard enough to cost weeks this schedule lacks. |
| D2 | Nodes have **zero** dependencies rather than injected clock/net/disk interfaces | Stronger and simpler. Time arrives as an argument, I/O leaves as data. Nothing to stub, nothing to accidentally call. Enables the import guard to be an exhaustive check rather than a partial one. |
| D3 | Own `node.Time`/`node.Duration` instead of `time.Time`/`time.Duration` | Makes mixing in a wall clock a compile error, not a review catch. |
| D4 | Own `prng` package; `math/rand` banned outright | Removes the exception that would otherwise have to be carved into the import guard, and guarantees the generator's algorithm never changes underneath a recorded seed. |
| D5 | Single-server membership change (Raft §4), not joint consensus | Materially simpler to get right, sufficient for what Verity needs. |
| D6 | Fixed 1024 shards | Avoids dynamic shard splitting, which is a project of its own. |
| D7 | Module path is the bare name `verity` | No repo URL committed to yet. Change to the real path at first publish; it is a mechanical rename. |
| D8 | Determinism test built in month 1, before any consensus code | Retrofitting determinism is a rewrite, not a refactor. This is the single highest-leverage sequencing decision in the plan. |

---

## Open questions

Answer before the task that depends on each; none block current work.

| # | Question | Needed by |
|---|---|---|
| Q1 | Repository host and final module path | T7.3 (publishing `sim`) |
| Q2 | Which MIT 6.5840 suite revision to vendor as the external oracle, and how to record that it was not authored here | T2.8 |
| Q3 | Benchmark hardware for RQ3 — one machine with containers, or cloud VMs | T6.6 |
| Q4 | Whether S3 (TLA⁺ model of migration) is attempted; depends on M5 finishing on schedule | T6.10 |
| Q5 | Trace format stability: golden-file the whole trace, or only the rolling hash | T1.9 |

---

## Known issues

None yet.

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
7. Record every bug found in `docs/BUGS.md` with its seed. That file is a
   graded deliverable (D2), not a scratch pad — an entry written weeks later
   from memory is worth much less than one written the day it was found.
