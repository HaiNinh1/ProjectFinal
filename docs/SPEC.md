# Verity — engineering specification

This is the implementation contract. `docs/proposal.html` explains *why* the
project exists and how it will be evaluated; this document says *what to
build*, precisely enough that two people (or two sessions) building from it
independently would produce compatible code.

Read order for anyone picking this up cold: this file, then `ROADMAP.md` for
what to do next, then `STATE.md` for where the work currently stands.

Anything in `node/api.go` overrides prose here — the Go is the source of truth
for interfaces.

---

## 1. Invariants

These are not style preferences. Each one is load-bearing for determinism or
correctness, and most are machine-enforced (`internal/policy`). Violating one
does not produce a test failure at the point of violation — it produces a
silent loss of reproducibility discovered weeks later. Treat a change that
breaks one as a bug regardless of whether tests pass.

| ID | Invariant | Enforced by |
|---|---|---|
| **INV-1** | Node packages perform no I/O. All effects are returned as `node.Action`. | `internal/policy` import guard |
| **INV-2** | Node packages never read a clock. Time arrives only as the `now` argument to `Step`. `time` is a banned import; use `node.Time`. | import guard + distinct type |
| **INV-3** | Node packages are single-threaded. No goroutines, no `sync`, no `context`. The runtime owns all concurrency. | import guard |
| **INV-4** | All randomness inside a node comes from a `*prng.Rand` injected at construction and seeded from the run seed. `math/rand` and `crypto/rand` are banned. | import guard |
| **INV-5** | Node packages never import `verity/sim`. A node cannot know which runtime it is under, or the simulation is not validating the deployed code. | import guard |
| **INV-6** | Iteration order is always explicit. Never `range` a map where the order can affect behaviour or output; build a sorted key slice first. | review + trace-diff test |
| **INV-7** | The simulator's event queue orders strictly by `(Time, seq)`, where `seq` is a global insertion counter. Ties are never broken by node ID, map order, or heap accident. | `sim` unit test |
| **INV-8** | Nothing is durable until the matching `PersistDone` returns with a nil error. A node must not acknowledge, commit, or vote on the strength of a write it has only *issued*. | Raft invariant tests |
| **INV-9** | Every merged change keeps `go test ./...` green, including the determinism trace-diff test. | CI |

**INV-6 in practice.** Go randomises map iteration deliberately. A single
`for id := range r.peers` that decides message send order will produce a
different trace on every run, and the determinism test will fail with no
indication of where. Standard fix:

```go
ids := slices.Sorted(maps.Keys(r.peers))
for _, id := range ids { /* ... */ }
```

---

## 2. Repository layout

Dependency direction is strictly downward. Nothing above imports anything
below it in this list except where stated.

```
verity/
  node/          Contract every replica implements. Imports NOTHING.
  prng/          Deterministic PRNG (splitmix64). Imports nothing.
  raft/          Consensus core. Pure node.Node. Imports node, prng.
  kvsm/          Key-value state machine + session/dedup table. Imports node.
  shard/         Shard controller and migration logic. Imports node, raft, kvsm.
  client/        Client library: routing, retry, session management.
  sim/           Deterministic runtime: scheduler, network, disk, faults, trace.
                 Imports node. Imported ONLY by cmd/veritysim and tests.
  runtime/       Real runtime: goroutines, gRPC, real disk. Imports node, transport, store.
  transport/     gRPC transport implementing the real network side.
  store/         Real WAL + snapshot files.
  check/         Porcupine model, history recording, linearizability checking.
  bench/         Workload generator (YCSB-style) and measurement harness.
  internal/policy/  Machine-enforced invariants (import guard, and later trace diff).
  cmd/
    veritysim/   Run seeds under simulation. The primary development tool.
    verityd/     Real server binary.
    verityctl/   CLI client.
  docs/          proposal.html, SPEC.md, ROADMAP.md, STATE.md, BUGS.md
```

The rule that matters: **`sim` and `runtime` both drive `node`, and neither is
visible to it.** That is what makes "the simulation tests the deployed code"
literally true rather than approximately true.

---

## 3. The node contract

Defined in `node/api.go`. Summary:

```go
type Node interface {
    ID() NodeID
    Restore(recs []Record) error
    Step(now Time, ev Event) []Action
}
```

Events the runtime delivers: `Deliver`, `TimerFired`, `PersistDone`,
`ClientCall`, `Restarted`.

Actions the node returns: `Send`, `Persist`, `SetTimer`, `ClearTimer`,
`Reply`, `Apply`.

Rules for implementers:

- `Step` handles exactly one event and must not block.
- Actions are performed by the runtime **in the order returned**. A node that
  needs a persist to happen before a send must return `Persist` first and wait
  for `PersistDone` before returning the `Send` — ordering within one batch is
  not a durability guarantee (INV-8).
- `Step` must not retain a reference to the event or its buffers.
- A node with nothing to do returns `nil`.

### Timers

Timer names are per-node strings. Verity uses exactly three in Raft:
`"election"`, `"heartbeat"`, `"lease"`. Re-arming an already-armed name
replaces it. The runtime does not deliver a `TimerFired` for a name that was
cleared or replaced before it elapsed.

---

## 4. The runtime contract

Both runtimes implement the same loop:

1. Take the next event.
2. Call `n.Step(now, ev)`.
3. Perform each returned action in order.
4. Record a trace line.

`sim` does this on one thread with a virtual clock. `runtime` does it with one
goroutine per node, a real clock, real sockets, and a real disk — and is
deliberately thin, because every line of logic in the runtime is a line the
simulator does not test.

---

## 5. The simulator

### 5.1 Scheduler

A min-heap of pending events keyed by `(Time, seq)` (INV-7). `seq` is a
`uint64` incremented on every insertion. The loop pops the earliest event,
advances the virtual clock to its timestamp, and steps the target node. When
the heap empties or the configured horizon is reached, the run ends.

Because time is virtual, an idle cluster costs nothing: the clock jumps
straight to the next timer. This is why 10⁴ simulated hours fits in a night.

### 5.2 Network model

Per-run parameters drawn from the seed:

| Parameter | Meaning |
|---|---|
| `BaseDelay` | Minimum one-way delay |
| `DelayJitter` | Additional delay, uniform in `[0, DelayJitter)` |
| `Bandwidth` | Bytes/sec; adds `Msg.Size() / Bandwidth` to delay |
| `DropRate` | Probability a message is silently discarded |
| `DupRate` | Probability a message is delivered twice, at independent times |
| `Partitions` | Set of blocked **directed** pairs. Asymmetric partitions are legal and are where the good bugs live. |

Reordering is not a separate parameter — it falls out of per-message jitter,
which is more realistic than an explicit shuffle.

### 5.3 Disk model

Each node has an in-memory log of records plus an unsynced tail.

- `Persist{Sync: false}` appends to the unsynced tail; `PersistDone` fires
  after `WriteLatency`.
- `Persist{Sync: true}` appends, then moves the whole tail to durable after
  `WriteLatency + SyncLatency`, then fires `PersistDone`.
- **Crash** discards the unsynced tail. With probability `TornRate`, the last
  durable record is additionally truncated to a random prefix.
- `Restore` receives the durable records in write order.

Records are framed on disk as `length | crc32 | payload`. The loader stops at
the first frame whose CRC fails or whose length overruns the file — which is
how a torn write is survived rather than mistaken for valid data. Implementing
this framing in `store` **and** in the sim's disk model is required; a sim disk
that cannot produce a torn write cannot test the recovery path.

### 5.4 Fault schedule

Generated from the seed before the run starts, so it is inspectable and
minimisable.

Swarm-style (objective S2): first draw a **profile** — which fault classes are
enabled and at what intensity — then draw the individual faults. A fixed
"average" fault mix hides whole bug classes; a run with zero partitions but
extreme clock skew finds things a balanced run never will.

Fault classes: node crash, node restart, network partition (form and heal),
message drop burst, disk slowdown, clock skew (per-node offset), clock jump.

### 5.5 Trace and the determinism test

One line per `Step`:

```
<seq> <vtime_ns> <node_id> <EventKind> -> <ActionKind>[,<ActionKind>...]
```

A rolling FNV-1a hash accumulates over these lines. The determinism test runs
each seed twice and compares hashes; on mismatch it writes both traces and
diffs them, which localises the nondeterminism to a single `Step`.

This test is the project's foundation. It goes in during month 1 and runs on
every commit thereafter.

---

## 6. Raft

### 6.1 Messages

`PreVote` / `PreVoteResp`, `RequestVote` / `RequestVoteResp`,
`AppendEntries` / `AppendEntriesResp`, `InstallSnapshot` / `InstallSnapshotResp`.

### 6.2 Durable state

`RecordHardState` carries `{Term, VotedFor, CommitIndex}`. `RecordEntry`
carries one log entry. `RecordSnapshot` carries `{LastIndex, LastTerm, Config,
StateMachineBytes}`.

### 6.3 Rules that are easy to get wrong

These are numbered because the RQ2 bug-injection study injects them one at a
time as mutants. Each must have a dedicated test.

| ID | Rule |
|---|---|
| **R1** | Persist `Term` and `VotedFor` durably *before* responding to a vote request. |
| **R2** | **Figure 8.** A leader marks an entry committed by counting replicas only for entries of its **own** term. Earlier-term entries commit implicitly when a current-term entry above them commits. Counting replicas on an earlier-term entry can be undone. |
| **R3** | **Election restriction.** A voter rejects a candidate whose log is less up to date: compare last log *term* first, then last log *index*. |
| **R4** | `AppendEntries` truncates the follower's log **only** on an actual term conflict at an overlapping index. A stale or duplicated append must never truncate committed entries. |
| **R5** | Apply strictly in index order and never beyond `commitIndex`. |
| **R6** | Reset the election timer on exactly three events: a valid `AppendEntries` from the current leader, granting a vote, and starting an election. Not on a rejected vote request, not on a stale append. |
| **R7** | Do not acknowledge replication before the entry is durable (INV-8). |
| **R8** | Never snapshot past `lastApplied`, and never discard log entries a follower still needs without being able to serve `InstallSnapshot`. |
| **R9** | `PreVote` does **not** increment the term and does **not** set `VotedFor`. |
| **R10** | Single-server membership change: a configuration entry takes effect when **appended**, not when committed, and only one uncommitted configuration change may be in flight. |

### 6.4 Reads

Three strategies, selectable at runtime because RQ5 compares them:

- `LogRead` — append the read to the log. Always safe, slowest. The baseline.
- `ReadIndex` — record `commitIndex`, confirm leadership with a heartbeat
  quorum, wait for `lastApplied >= commitIndex`, serve. One round trip. Safe
  under arbitrary clock behaviour.
- `LeaseRead` — serve locally while within `LeaseDuration` of the last
  successful heartbeat quorum. Zero round trips. **Safe only if inter-node
  clock drift stays within the assumed bound**, which is exactly what RQ5
  attacks.

---

## 7. Key-value state machine

`kvsm` applies `Get`, `Put`, `Append`, `Delete`.

**Sessions and exactly-once.** A client registers via a `RegisterClient`
command through Raft, receiving a `ClientID`. Every subsequent command carries
`{ClientID, Seq}` with `Seq` strictly increasing per client. The state machine
keeps `dedup[ClientID] = {LastSeq, LastResp}`. On apply:

- `Seq < LastSeq` — stale retry, ignore, no response needed.
- `Seq == LastSeq` — duplicate, return `LastResp` without re-applying.
- `Seq > LastSeq` — apply, record.

Sessions expire on a lease so `dedup` does not grow without bound; expiry is
itself a Raft command so all replicas expire identically.

---

## 8. Sharding and migration

Fixed shard count: **1024**. `ShardID = hash(key) % 1024`.

The **shard controller** is Raft group 0. It holds an ordered list of
configurations; configuration `N` maps every shard to a group. Groups join and
leave by committing commands to the controller, which rebalances and produces
config `N+1`.

### 8.1 Migration protocol

Moving shard `S` from group `A` to group `B` under config `N+1`. Every step is
committed through the acting group's **own** Raft log, so a leader change on
either side resumes rather than corrupts.

1. **A** commits `FreezeShard(S, N+1)`. A stops serving `S` immediately and
   retains its data and the dedup entries for keys in `S`.
2. **B** commits `AwaitShard(S, N+1)`, then sends `PullShard{S, N+1}` to A's
   leader, repeating until answered.
3. **A** replies `ShardData{S, N+1, kv, dedup}`, read from the frozen state.
   Idempotent; may be re-sent any number of times.
4. **B** commits `InstallShard{S, N+1, kv, dedup}` and begins serving `S`.
5. **B** sends `PullDone{S, N+1}`; **A** commits `DiscardShard(S, N+1)` and
   frees the data.

Invariants:

- **No shard is served by two groups at once.** A stops at step 1; B starts at
  step 4. The gap is unavailability, which is correct; overlap would be a
  linearizability violation.
- **The dedup table migrates with the shard** (steps 3–4). Omitting it silently
  breaks exactly-once: a client whose request was applied by A but whose
  response was lost retries against B, which has no record and applies it
  twice. This is mutant **R11** in the bug-injection study.
- A group must not advance to config `N+2` until every shard movement for
  `N+1` has settled.

---

## 9. Client library

Routes by `hash(key) % 1024` through a cached shard map. On `ErrWrongGroup` it
re-fetches the configuration from the controller and retries. On timeout it
retries **with the same `Seq`**, which is what makes the dedup table load-
bearing. Retries are the normal case, not the exceptional one.

---

## 10. Testing layers

| Layer | What it covers | Runs |
|---|---|---|
| Unit | Single-node state transitions, log manipulation, dedup logic | every commit |
| Import guard | INV-1…INV-5 | every commit |
| Determinism | Same seed → identical trace hash, 100 seeds × 2 runs | every commit |
| Simulation | Full cluster under seeded faults, checked by Porcupine | every commit (short), nightly (long) |
| Independent suite | MIT 6.5840 adversarial tests — an oracle not written here | every commit once Raft lands |
| Real deployment | 5 nodes in containers under `tc netem` | before each milestone |

---

## 11. Configuration defaults

| Setting | Default | Note |
|---|---|---|
| Shard count | 1024 | Fixed for the life of a cluster |
| Election timeout | 150–300 ms, randomised | Jitter drawn from the injected `prng` |
| Heartbeat interval | 50 ms | |
| Lease duration | heartbeat × 9 ÷ 10 − assumed max skew | The assumption RQ5 attacks |
| Snapshot threshold | 10 000 entries or 8 MiB | |
| Max entries per `AppendEntries` | 64, or 1 MiB | |
| Max in-flight `AppendEntries` | 16 | Pipelining depth |
| Sim base network delay | 1 ms ± 0.5 ms | "Clean" profile |
| Sim write / sync latency | 100 µs / 1 ms | |
| Default run horizon | 60 simulated seconds | Per seed, in the short suite |

---

## 12. Commands

```bash
go test ./...                       # everything, including the guards
go test ./internal/policy -v        # invariant guards alone
go run ./cmd/veritysim -seed 0x1234 # replay one seed
go run ./cmd/veritysim -seeds 1000  # sweep, parallel across cores
gofmt -l .                          # must print nothing
go vet ./...
```
