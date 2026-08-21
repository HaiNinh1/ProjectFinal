# Verity — implementation roadmap

Ordered, checkable tasks. Every task has a **Done when** that can be verified
by running something, not by reading the diff and feeling satisfied.

Work tasks in order within a milestone. Where order does not matter it is
noted. Update `STATE.md` as each task completes — that file is how the next
session knows where it is.

Milestone exit criteria come from `docs/proposal.html` §8 and are repeated here
so this file is self-sufficient.

---

## M1 — The harness, before the system

**Exit criterion:** the same seed produces a byte-identical trace across 100
runs and across two different machines.

Nothing in M2 onward is trustworthy if this milestone is approximated. Build it
first and build it properly.

| ID | Task | Done when |
|---|---|---|
| T1.1 | Go module, package skeleton, import guard test | `go test ./internal/policy` passes; guard skips packages not yet created ✅ |
| T1.2 | `node` API: `Time`, `Duration`, IDs, events, actions, `Record`, `Node` | `go build ./node` clean; package imports nothing ✅ |
| T1.3 | `prng`: splitmix64, no global state, explicit `Split(id)` for per-node streams | Unit test: same seed → same sequence; `Split` streams are independent and reproducible |
| T1.4 | `sim` scheduler: min-heap keyed by `(Time, seq)`, virtual clock, run horizon | Unit test asserts strict `(Time, seq)` ordering including exact-tie cases (INV-7) |
| T1.5 | `sim` network model: delay, jitter, bandwidth, drop, duplicate, directed partitions | Unit tests for each parameter; partition test proves an asymmetric partition blocks one direction only |
| T1.6 | `sim` disk model: write/sync latency, unsynced tail lost on crash, torn last record | Unit test: crash without sync loses the tail; torn write is rejected by the CRC frame loader, not returned as data |
| T1.7 | Record framing `length \| crc32 \| payload` shared by `sim` disk and `store` | Round-trip test; truncated and bit-flipped frames both rejected at the right offset |
| T1.8 | Fault schedule generated from seed, with swarm profile selection (S2) | Same seed → same schedule; schedule is dumpable as text and re-loadable |
| T1.9 | Trace recorder: one line per `Step`, rolling FNV-1a hash | Trace of a fixed scenario matches a golden file |
| T1.10 | `echo` node implementing `node.Node`, running under `sim` | 3-node echo cluster, 100 client calls, all replied under drops and delays |
| T1.11 | **Determinism test**: 100 seeds × 2 runs, compare hashes; on mismatch dump and diff both traces | `go test ./sim -run TestDeterminism` passes; deliberately introducing a `range` over a map makes it fail with a localised diff |
| T1.12 | `cmd/veritysim`: `-seed` to replay one, `-seeds N` to sweep across cores | `go run ./cmd/veritysim -seed 0x1234` twice produces identical output |
| T1.13 | CI: `gofmt -l`, `go vet`, `go test ./...` | Pipeline green on a clean checkout |

---

## M2 — Consensus, part one

**Exit criterion:** independent suite passing through the log-persistence
tests; 10³ simulated cluster-hours clean under crash and partition seeds.

| ID | Task | Done when |
|---|---|---|
| T2.1 | Raft state, log storage, term/vote persistence (**R1**) | Unit tests; vote is durable before the response action is returned |
| T2.2 | Leader election with randomised timeouts from injected `prng` | Single leader elected under partition; no two leaders in one term, asserted every step |
| T2.3 | Election restriction (**R3**) | Test: a candidate with a shorter/staler log cannot win |
| T2.4 | Log replication, `AppendEntries` consistency check, conflict truncation (**R4**) | Divergent follower logs converge; committed entries are never truncated |
| T2.5 | Commit rule including the Figure 8 restriction (**R2**) | The Figure 8 scenario is reproduced as a test and does not lose a committed entry |
| T2.6 | Apply pipeline in index order (**R5**), election-timer reset discipline (**R6**) | Dedicated tests per rule |
| T2.7 | Wire into `sim`; run the fault catalogue | 10³ simulated cluster-hours, zero violations |
| T2.8 | Hook up the MIT 6.5840 suite as an external oracle | Suite passes through the persistence tests |

---

## M3 — Consensus, part two

**Exit criterion:** full independent suite passing; 10⁴ simulated
cluster-hours clean; first entries in `docs/BUGS.md`.

| ID | Task | Done when |
|---|---|---|
| T3.1 | Snapshotting and log compaction (**R8**) | Log truncates; a restarted node recovers from snapshot + tail |
| T3.2 | `InstallSnapshot` for lagging followers | A follower whose needed entry was discarded catches up |
| T3.3 | PreVote (**R9**) | Test: a partitioned node rejoining does not depose a healthy leader |
| T3.4 | Term-based conflict backtracking | Divergence of N entries converges in O(terms) round trips, not O(N) — asserted by counting messages |
| T3.5 | Batching and pipelining | Throughput measurably improves; correctness tests still pass with depth > 1 |
| T3.6 | Single-server membership change (**R10**) | Add and remove a server under load without losing availability or committed data |
| T3.7 | Long soak | 10⁴ simulated cluster-hours, zero unresolved violations |
| T3.8 | Start `docs/BUGS.md` | Every bug found so far recorded with seed, class, root cause, fix commit |

---

## M4 — The key-value service

**Exit criterion:** Porcupine-clean under simulation; a real 5-node cluster
survives `tc netem` partitions; RQ5 produces its first data point.

| ID | Task | Done when |
|---|---|---|
| T4.1 | `kvsm`: Get/Put/Append/Delete, snapshot and restore | Unit tests; snapshot round-trips exactly |
| T4.2 | Client sessions, dedup table, session expiry as a Raft command | Duplicate delivery of an Append does not double-apply |
| T4.3 | `check`: history recording + Porcupine KV model | A deliberately stale read is caught and reported as a violating history |
| T4.4 | `LogRead`, `ReadIndex`, `LeaseRead`, selectable at runtime | All three serve correct values in the clean profile |
| T4.5 | `client` library: routing, retry with stable `Seq`, redirection | Client survives leader failover without user-visible error |
| T4.6 | `runtime` + `transport` + `store`: real goroutines, gRPC, real WAL | 5-node cluster runs outside the simulator |
| T4.7 | Real-network fault testing with `tc netem` in containers | Cluster survives partition and heal; no violation in recorded history |
| T4.8 | **RQ5 first result**: linearizability vs injected clock skew, per read strategy | A plot exists showing the skew threshold where `LeaseRead` breaks and `ReadIndex` does not |

---

## M5 — Sharding

**Exit criterion:** migration under continuous write load with zero
violations; a migration interrupted by leader failure on either side completes
correctly.

| ID | Task | Done when |
|---|---|---|
| T5.1 | Shard controller as Raft group 0; numbered configurations | Join/leave produces a new config; config history is durable |
| T5.2 | Rebalancing that minimises shard movement | Deterministic given the same group set — no map iteration (INV-6) |
| T5.3 | Migration protocol steps 1–5 (SPEC §8.1) | Shard moves under load; no shard served by two groups at any instant |
| T5.4 | Dedup table migrates with the shard | Test: apply on source, lose response, retry against destination — applied exactly once |
| T5.5 | Migration interrupted by leader change on source and on destination | Both resume and complete; no data loss, no double-serve |
| T5.6 | Config `N+2` blocked until `N+1` settles | Test asserts the block |
| T5.7 | Soak under continuous rebalancing | 10⁴ simulated cluster-hours with migrations running throughout, zero violations |

---

## M6 — Measurement

**Exit criterion:** complete result set collected; every thesis figure
regenerated end to end from raw data by script.

| ID | Task | Done when |
|---|---|---|
| T6.1 | Seed minimisation (S1): replay with faults progressively removed | A known failing seed reduces to a minimal reproducer automatically |
| T6.2 | Swarm profile sweep across seeds (S2) | Sweep reports coverage per profile; results are reproducible |
| T6.3 | **RQ2 mutant harness**: inject R1–R11 one at a time, run both regimes at equal CPU budget | Detection rate and time-to-first-failure table produced by script |
| T6.4 | **RQ2 observational half**: for each real bug in `BUGS.md`, give the conventional regime equal budget | Table of "would conventional testing have found it" |
| T6.5 | `bench`: YCSB A/B/C workload generator | Reproducible load; records history for the checker |
| T6.6 | **RQ3**: throughput and latency vs etcd, cluster sizes 3 and 5, value sizes 64 B–64 KiB, all three read strategies, batching on/off | Full result set with ≥3 repetitions and reported variance |
| T6.7 | **RQ4**: ≥500 injected leader kills; failover distribution; rolling restart; migration window | Distribution plots, outliers explained individually |
| T6.8 | **RQ1** final soak | ≥10⁵ simulated cluster-hours, zero unresolved violations |
| T6.9 | Figure generation scripts | `make figures` reproduces every plot from raw data |
| T6.10 | *(S3, optional)* TLA⁺ model of the migration protocol, TLC-checked on small configs | Model checks clean, or a discrepancy with the implementation is documented |

---

## M7 — Write-up and defence

**No engineering.** This month is the schedule's shock absorber; if earlier
months slipped, they eat into it and the contingencies in proposal §9 apply.

| ID | Task | Done when |
|---|---|---|
| T7.1 | Thesis: background, design, implementation, evaluation, threats to validity | Draft complete |
| T7.2 | `BUGS.md` finalised as a standalone deliverable | Every entry has seed, class, root cause, fix, and conventional-testing assessment |
| T7.3 | Publish `sim` as a standalone package with its own README | Usable by someone who has never seen Verity |
| T7.4 | Defence demo rehearsed | Live seed replay → violating history → fix → same seed clean, in under two minutes |
| T7.5 | Buffer | — |

---

## Contingencies

Pulled forward from proposal §9 so they are visible while working, not only
while writing.

- **Membership change unresolved by end of M3** → freeze cluster membership,
  ship without reconfiguration, report as a stated limitation. Nothing
  downstream depends on it.
- **Sharding slips in M5** → fall back to static shard assignment at startup.
  All five research questions remain answerable; only the online-migration
  result is lost.
- **Determinism proves unmaintainable** → this is the one failure with no
  fallback, which is why T1.11 exists in month one rather than month five.
