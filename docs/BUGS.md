# Verity — bug ledger

Every bug the harness finds, recorded **on the day it is found**, with the seed
that reproduces it. This file is a graded deliverable (proposal §8, D2), not a
scratch pad: an entry written weeks later from memory is worth much less than
one written while the failing trace is still on screen.

Each entry carries:

- **Seed** — the exact reproducer. If it is not replayable it is not an entry.
- **Class** — what kind of mistake it was, so the classes can be counted later.
- **Found by** — which layer caught it. This column is the evidence for RQ2:
  it is what makes "would conventional testing have found this?" a question
  with an answer rather than an opinion.
- **Root cause** — the actual defect, not the symptom.
- **Fix** — what changed.
- **Conventional-testing assessment** — filled in for T6.4. Would a unit or
  integration test written without a deterministic simulator have caught it,
  and at what budget? Answer honestly, including when the answer is "yes,
  easily".

Classes used so far: `liveness`, `safety`, `durability`, `determinism`,
`protocol`, `resource`.

---

## B1 — a completed request disarms the retry timer shared by every other request

| | |
|---|---|
| **Found** | 2026-08-21 |
| **Seed** | `0x1234` (`TestEchoClusterRepliesUnderDropsAndDelays`, 3 nodes, `DropRate` 0.1, 100 calls) |
| **Class** | `liveness` |
| **Found by** | Simulation — the first cluster-level test that ever ran |
| **Component** | `internal/echo` (the harness's node fixture, not the datastore) |
| **Status** | Fixed |

**Symptom.** 94 of 100 client calls were answered. Six — request IDs 24, 26,
57, 60, 72 and 76 — were never answered at all, and the run went quiet well
before the horizon. Every failing run under this seed failed identically, which
is what made it worth chasing rather than dismissing as flakiness.

**Root cause.** The echo node arms a single timer named `"retry"` and uses it
to re-send for *all* in-flight requests. On reaching a majority for one
request it returned:

```go
node.Reply{...},
node.ClearTimer{Name: "retry"},
```

unconditionally. With one request in flight that is correct. With a hundred
overlapping requests it is not: the moment any one of them completed, the
shared timer was disarmed, and every other request whose `echoReq` had been
dropped lost the only thing that would ever have re-sent it. Nothing was
corrupted and nothing was lost — the requests simply waited forever, which is
why a test that only checked the *answers it got* would have passed.

The deeper mistake is a category error that will recur in Raft: treating a
timer as though it belonged to one request, when timer names are per-node
(SPEC §3) and therefore shared by everything the node is doing.

**Fix.** Clear the timer only when no request is still awaiting acknowledgement.
Added a regression test that runs two overlapping requests, completes the
first, and asserts the timer survives.

**Conventional-testing assessment.** Catchable without a simulator, but only by
someone who thought to write the specific test. The single-request tests all
passed, and so did a hundred-call run with no drops — the bug needs
concurrency *and* message loss *and* an assertion on the calls that did **not**
come back. An integration test that asserted "all responses correct" rather
than "all requests answered" would have missed it indefinitely. Time-to-find
under simulation: the first run of the first cluster test. Verdict:
**plausibly found by conventional testing, but easily missed**, and the failure
mode — a silent stall under partial message loss — is one that hides
particularly well in a real system, where it looks like an unlucky timeout.
