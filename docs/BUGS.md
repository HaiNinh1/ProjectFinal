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

---

## B2 — a vote answered after the node had already moved to a later term

| | |
|---|---|
| **Found** | 2026-08-21 |
| **Seed** | None. Found by inspection while writing the R1 tests for T2.1, before the code was committed. The randomised sweep in `TestNoGrantEverPrecedesItsWrite` reproduces it on seed `0xB0A710A5` once the guard is removed |
| **Class** | `safety` |
| **Found by** | Design review — reasoning about two writes being in flight at once |
| **Component** | `raft` (`onPersistDone`) |
| **Status** | Fixed |

**Symptom.** A replica announced a granted vote for a term it had already left.
The response carried `Term: 1, VoteGranted: true` at a moment when the node's
durable hard state said term 4, so a restart taken immediately afterwards would
have recovered no trace of the vote that had just been promised.

**Root cause.** `onRequestVote` decides the answer, parks it, and writes; the
answer is sent when the write lands. That is R1 working as intended. What the
first version missed is that the node does not stand still in between. A second
vote request — or any RPC response carrying a later term — can arrive while the
first write is still in flight, step the node down into the new term, and issue
a second write. `node.Persist` promises nothing about the order the two
completions arrive in, and the simulator's disk flushes the entire unsynced
tail on any sync, so both records land with the later one last. Replaying them
gives the later term, as it should: a hard state supersedes every earlier one.

The parked answer, however, was still the one decided in the older term, and
`onPersistDone` sent it verbatim:

```go
case pendVoteResp:
    return []node.Action{node.Send{To: pend.to, Msg: pend.resp}}
```

So the node vouched for a vote that no longer existed anywhere on disk.

It is worth being precise about how bad this is, because the honest answer is
"not very, and that is the problem". No double vote can actually follow from
it. Having reached term 4, the node rejects every later request in term 1, and
it granted only one term-1 vote before moving on. The bug is safe — but it is
safe by a coincidence two rules away from the one being relied on, and R1
exists exactly so that vote safety does not rest on that kind of reasoning. A
later change that made a lower term reachable again, or that let `VotedFor`
move within a term, would convert it into a real split-brain with nothing in
the diff to suggest it.

**Fix.** Compare the parked answer's term against the current one when the
write lands, and if the node has moved on, replace it with a rejection carrying
the current term. A rejection needs no durability behind it, and it is strictly
more useful than the stale answer: it tells a superseded candidate to give up
rather than leaving it to time out.

```go
resp := pend.resp
if resp.Term != n.hard.Term {
    resp = RequestVoteResp{Term: n.hard.Term, VoteGranted: false}
}
```

Regression test: `TestLateWriteAnswersWithTheCurrentTermNotTheDecidedOne`.
Reverting the guard fails it, `TestOlderCompletionDoesNotRegressTheDurableState`
and the randomised sweep together.

**Conventional-testing assessment.** Very unlikely to be found by conventional
testing, and unlikely to be found by a simulator either without the specific
assertion that catches it. The bug needs two writes in flight at once, which
needs a vote request to arrive during the disk latency of a previous one, and
it produces no crash, no error and no lost data — only a message that is
misleading rather than wrong. Every liveness and safety property an ordinary
integration test would check still holds. What caught it here was not a
failing run but an invariant stated sharply enough to be checkable: *a granted
vote must never leave the node unless the durable state already justifies it*,
asserted after every step by the test harness rather than at the end of a
scenario. Verdict: **found only by making the rule mechanical**, which is the
argument for R1..R10 being numbered and individually asserted rather than
reviewed. It is also a good candidate mutant for the RQ2 study, since it is a
one-line deletion that no ordinary test suite notices.
