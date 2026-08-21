// Package raft implements the Raft consensus algorithm as a pure state
// machine, in the sense of node.Node: it reads no clock, performs no I/O,
// starts no goroutines, and draws randomness only from an injected *prng.Rand.
// Its entire effect on the world is the node.Actions it returns.
//
// The rules that are easy to get wrong are numbered R1 through R10 in
// docs/SPEC.md section 6.3, and each is cited at the code implementing it. The
// numbering is load-bearing rather than decorative: the RQ2 bug-injection
// study reintroduces each rule's violation one at a time as a mutant, so a
// rule, the code that enforces it, and the test that catches its absence must
// all stay findable from the identifier alone.
//
// One idea shapes the whole package. Raft may not act on state it has not yet
// written down (INV-8), so a replica keeps two copies of its hard state — what
// it believes, and what it can prove — and every response whose safety rests
// on the second is issued from the PersistDone handler rather than from the
// handler that decided it. See persist.go and vote.go.
//
// The pieces, in dependency order:
//
//   - state.go — terms, indices, roles, and the hard state that must survive a
//     crash.
//   - log.go — the replicated log, with a sentinel entry ahead of the real
//     ones so that the consistency check (R4) has no special case at the start
//     of the log or at a snapshot boundary.
//   - record.go — the codec for the three durable record kinds fixed by
//     SPEC section 6.2.
//   - persist.go — outstanding writes and the deferred work each authorises;
//     the mechanism behind R1 and INV-8.
//   - msg.go — the Raft RPCs as node.Message values.
//   - raft.go — the replica: construction, Restore, and the Step dispatch.
//   - vote.go — RequestVote and its response, where R1 and R3 are enforced.
package raft
