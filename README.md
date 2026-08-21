# ProjectFinal — Verity

A sharded, linearizable, replicated key-value store written from first
principles in Go, validated by **deterministic simulation testing**.

Final-year Computer Science project · Sep 2026 – Mar 2027 · solo.

---

## What this is

Three layers:

1. **Raft consensus**, implemented to the specification *and* its dissertation
   extensions — durable state, snapshots, single-server membership change,
   PreVote, leader leases.
2. **Multi-Raft sharding** across replica groups, with a replicated shard
   controller and online shard migration under live write load.
3. **A deterministic simulator** in which the entire cluster runs on one thread
   under a virtual clock, with every message delay, drop, duplication,
   reordering, partition, node crash, disk fault and clock skew drawn from a
   single 64-bit seed.

The datastore is the system under test. The simulator is the research artifact.

## Why it is built this way

Distributed systems fail in rare interleavings of message ordering, partial
failure, and timing. Conventional testing runs the real system on a real
network in real time, which samples a vanishingly small fraction of the
schedule space — and samples it *non-reproducibly*. A bug that appears once in
five hundred runs is, in practice, undebuggable.

Under simulation, time is virtual (one simulated hour costs milliseconds of
CPU) and everything is seeded, so **any failure reproduces exactly, from a
single integer, forever, on any machine**.

The consequence shows up in the node design: a Verity node has *no*
dependencies. It never reads a clock, opens a socket, touches a file, or starts
a goroutine. Time arrives as an argument to `Step`; I/O leaves as returned
data. The simulator and the real server are two runtimes driving identical node
code — which is what makes "the simulation tests the deployed system" literally
true rather than approximately true.

```go
type Node interface {
    ID() NodeID
    Restore(recs []Record) error
    Step(now Time, ev Event) []Action
}
```

## Evaluation

Five research questions, each with a stated metric and success criterion:

| | Question |
|---|---|
| RQ1 | Is it linearizable under fault? — violations per 10³ simulated cluster-hours, checked by Porcupine |
| RQ2 | Does deterministic simulation find bugs conventional testing misses? — known consensus defects injected one at a time, both regimes given equal CPU budget |
| RQ3 | What does strong consistency cost? — throughput and tail latency against etcd v3 |
| RQ4 | How available is it? — failover distribution over ≥500 injected leader kills |
| RQ5 | Are leader-lease reads actually safe? — the measured clock-skew threshold at which they stop being linearizable |

RQ2 is the central study.

## Layout

```
node/     contract every replica implements; imports nothing
prng/     deterministic PRNG (splitmix64)
raft/     consensus core
kvsm/     key-value state machine + session dedup
shard/    shard controller and migration
client/   routing, retry, sessions
sim/      deterministic runtime — imported only by cmd and tests
runtime/  real runtime: goroutines, gRPC, real disk
check/    Porcupine model and history recording
bench/    workload generator
internal/
  frame/  record framing, shared by the sim disk and the real WAL
  echo/   minimal node.Node used as the harness's fixture before Raft exists
  policy/ machine-enforced invariants (the import guard)
cmd/      veritysim, verityd, verityctl
```

Packages not yet written are listed because the dependency direction is part of
the design, not an outcome of it. The import guard already names `raft`, `kvsm`
and `shard`, and skips them until they exist.

## Documentation

| File | Contents |
|---|---|
| [`docs/proposal.html`](docs/proposal.html) | Project proposal — motivation, objectives, architecture, evaluation methodology, timeline, risk register |
| [`docs/SPEC.md`](docs/SPEC.md) | Implementation contract — invariants, interfaces, protocols, the eleven consensus rules that are easy to get wrong |
| [`docs/ROADMAP.md`](docs/ROADMAP.md) | 53 tasks across seven milestones, each with a checkable completion criterion |
| [`docs/STATE.md`](docs/STATE.md) | Current position, decisions taken, open questions |
| [`docs/BUGS.md`](docs/BUGS.md) | Every bug found, with the seed that reproduces it |

## Build

```bash
go test ./...                          # everything, including the invariant guards
go test ./internal/policy -v           # determinism guards alone
go test ./sim -run TestDeterminism     # the test the project rests on

go run ./cmd/veritysim -seed 0x1234            # replay one seed
go run ./cmd/veritysim -seed 0x1234 -trace     # ...and print every Step
go run ./cmd/veritysim -seed 0x1234 -schedule  # ...and print its fault schedule
go run ./cmd/veritysim -seeds 1000             # sweep across cores
go run ./cmd/veritysim -profiles               # list the fault profiles

# minimise a failing seed: dump its schedule, comment faults out, run it back
go run ./cmd/veritysim -seed 0x1234 -schedule > sched.txt
go run ./cmd/veritysim -schedule-file sched.txt
```

`-seed` twice produces byte-identical output. That is the whole idea, and it is
checkable in two seconds.

## Status

**M1 — the harness, before the system — is implemented.** The simulator is
built before any consensus code exists, because retrofitting determinism onto a
codebase already structured around goroutines and wall clocks is a rewrite, not
a refactor.

What runs today: a seeded scheduler, network, and disk model; a fault schedule
drawn from the seed and dumpable as editable text; a trace hashed per `Step`;
and a three-node cluster of a real `node.Node` answering client calls under
drops, delays, duplication, partitions, crashes and torn writes. One seed
replayed a hundred times, and a thousand seeds replayed twice, all reproduce
byte for byte.

The remaining M1 criterion is the one a single machine cannot check: that a
seed produces the same trace on a *different* machine. CI is written to prove
it and has not yet run.

See [`docs/STATE.md`](docs/STATE.md) for the current task and the open
questions.
