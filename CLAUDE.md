# Verity

A sharded, linearizable, replicated key-value store in Go, validated by
deterministic simulation testing. Final-year project, solo, Sep 2026 – Mar 2027.

The datastore is the system under test; the deterministic simulator is the
research artifact.

## Read before writing any code

1. `docs/SPEC.md` — the implementation contract. The invariants in §1 are
   load-bearing, not style preferences.
2. `docs/ROADMAP.md` — ordered tasks, each with a checkable "Done when".
3. `docs/STATE.md` — where the work stands and what the next task is.
4. `docs/proposal.html` — why the project exists and how it will be evaluated.
   Background; not needed to write code.

`node/api.go` is the source of truth for interfaces. Prose that contradicts it
is a documentation bug.

## The invariants, in short

Full statements and enforcement in `docs/SPEC.md` §1.

Node packages — `node`, `prng`, `raft`, `kvsm`, `shard` — are **pure state
machines**. They may not:

- perform I/O (return a `node.Action` instead) — **INV-1**
- read a clock (`now` arrives as the first argument to `Step`; `time` is a
  banned import; use `node.Time`) — **INV-2**
- start goroutines or import `sync`/`context` — **INV-3**
- use `math/rand` or `crypto/rand` (use the injected `*prng.Rand`) — **INV-4**
- import `verity/sim` — a node must not know which runtime it is under — **INV-5**

Everywhere: never `range` a map where order can affect behaviour or output;
build a sorted key slice first (**INV-6**). Go randomises map iteration
deliberately, and a single unsorted range destroys reproducibility with no
error at the point of failure.

Nothing is durable until `PersistDone` returns with a nil error (**INV-8**).

`internal/policy` enforces INV-1…INV-5 mechanically and skips packages that do
not exist yet, so it bites as each one lands.

## Layout

```
node/     contract every replica implements; imports nothing
prng/     deterministic PRNG
raft/     consensus core
kvsm/     key-value state machine + session dedup
shard/    shard controller and migration
client/   routing, retry, sessions
sim/      deterministic runtime — imports node, imported only by cmd + tests
runtime/  real runtime: goroutines, gRPC, real disk
check/    Porcupine model and history recording
bench/    workload generator
cmd/      veritysim, verityd, verityctl
```

Dependency direction is strictly downward. `sim` and `runtime` both drive
`node`; neither is visible to it.

## Commands

```bash
go test ./...                        # everything, including the guards
go test ./internal/policy -v         # invariant guards alone
go run ./cmd/veritysim -seed 0x1234  # replay one seed
go run ./cmd/veritysim -seeds 1000   # sweep across cores
gofmt -l .                           # must print nothing
go vet ./...
```

## Working protocol

- Pick up at **Next task** in `docs/STATE.md`. The task ordering is a
  dependency ordering; do not skip ahead.
- Before claiming a task done: `gofmt -l .` empty, `go vet ./...` clean,
  `go test ./...` green. Paste the real output rather than asserting it.
- No stubs, no `TODO` placeholders, no skipped tests left behind. An unfinished
  task is reported unfinished, not merged as done.
- Update `docs/STATE.md` as part of the task: move it to Done with its
  verification, set the new Next task, record any decision or open question.
- Record every bug found in `docs/BUGS.md` **with its seed, the day it is
  found**. That file is a graded deliverable, not a scratch pad.

## Non-goals

Permanently out of scope, per proposal §4: Byzantine fault tolerance,
cross-shard transactions, SQL or secondary indexes, leaderless or WAN consensus
(EPaxos), custom transports, auth/TLS/multi-tenancy, production ops tooling.

Cross-shard transactions are the tempting one — the migration machinery makes
them look close. That is exactly why the boundary is written down in advance.
