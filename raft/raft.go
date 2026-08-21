package raft

import (
	"fmt"
	"sort"

	"verity/node"
	"verity/prng"
)

// Node is one Raft replica: a pure state machine implementing node.Node.
//
// The field that does the most work here is the pair of hard states. n.hard is
// what this replica believes; persister.durable is what it has proof of. Raft
// is only allowed to act on the second, and the gap between them is where R1
// and INV-8 live. Every response whose safety rests on the term or the vote is
// issued from the PersistDone handler, never from the handler that decided it.
type Node struct {
	id    node.NodeID
	peers []node.NodeID // sorted ascending at construction; includes id
	rng   *prng.Rand    // per-node stream; election jitter draws from it (T2.2)

	role Role
	hard HardState
	log  *Log
	snap Snapshot // the most recent snapshot recovered from disk

	persist persister
}

// Node implements node.Node. Stated here so that a signature drift is a
// compile error in this package rather than at every call site.
var _ node.Node = (*Node)(nil)

// New builds a replica.
//
// peers is the full membership including id, matching the simulator's
// NewNodeFunc. It is copied and sorted, so callers keep their slice and the
// order this node walks the membership in never depends on the order it was
// handed.
//
// A replica always starts as a Follower, on a fresh boot and on a restart
// alike. Nothing durable records that a node was ever a leader, and there is
// no safe way to assume it: a node that was leader before it crashed may have
// been replaced while it was down, and resuming as leader would put two
// leaders in one term.
func New(id node.NodeID, peers []node.NodeID, rng *prng.Rand) *Node {
	if id == None {
		panic("raft: New: id must not be zero")
	}
	if rng == nil {
		panic("raft: New: rng must not be nil")
	}
	ps := make([]node.NodeID, len(peers))
	copy(ps, peers)
	sort.Slice(ps, func(i, j int) bool { return ps[i] < ps[j] })

	self := false
	for i, p := range ps {
		if i > 0 && p == ps[i-1] {
			panic("raft: New: peers contains a duplicate id")
		}
		if p == id {
			self = true
		}
	}
	if !self {
		panic("raft: New: peers does not contain id")
	}

	return &Node{
		id:    id,
		peers: ps,
		rng:   rng,
		role:  Follower,
		hard:  HardState{Term: 0, VotedFor: None, CommitIndex: 0},
		log:   NewLog(),
	}
}

// ID reports this node's identity.
func (n *Node) ID() node.NodeID { return n.id }

// Snapshot reports the most recent snapshot recovered from durable storage,
// and whether there was one. Raft does not interpret its Data; the field
// exists so that a restart does not silently discard the state machine bytes
// that the log entries it compacted away were the only other record of.
func (n *Node) Snapshot() (Snapshot, bool) {
	return n.snap, n.snap.LastIndex > 0 || n.snap.LastTerm > 0
}

// Restore rebuilds the replica from the records recovered after a restart.
//
// Records arrive in the order they were written, which is what makes replaying
// them faithful rather than merely approximate: a later hard state supersedes
// an earlier one, and an entry record at an index the log already holds is a
// leader's correction, so it truncates the log there before appending, exactly
// as the original append did.
//
// The recovered hard state is also the durable one. Nothing else in this
// incarnation has been written yet, so what came off the disk is the whole of
// what this node can prove.
//
// Torn writes do not reach here: the framing layer stops at the first record
// whose checksum fails and passes on the clean prefix. So a record that fails
// to decode here is genuine corruption, and it is reported rather than
// skipped. The simulator turns that error into a panic, which is the correct
// loudness for a log that says something impossible.
func (n *Node) Restore(recs []node.Record) error {
	for i, rec := range recs {
		switch rec.Kind {
		case node.RecordHardState:
			h, err := decodeHardState(rec)
			if err != nil {
				return fmt.Errorf("raft: Restore: record %d: %w", i, err)
			}
			n.hard = h

		case node.RecordEntry:
			e, err := decodeEntry(rec)
			if err != nil {
				return fmt.Errorf("raft: Restore: record %d: %w", i, err)
			}
			if err := n.restoreEntry(e); err != nil {
				return fmt.Errorf("raft: Restore: record %d: %w", i, err)
			}

		case node.RecordSnapshot:
			s, err := decodeSnapshot(rec)
			if err != nil {
				return fmt.Errorf("raft: Restore: record %d: %w", i, err)
			}
			if err := n.log.Compact(s.LastIndex, s.LastTerm); err != nil {
				return fmt.Errorf("raft: Restore: record %d: %w", i, err)
			}
			n.snap = s

		default:
			return fmt.Errorf("raft: Restore: record %d: %w: %d", i, ErrBadRecordKind, rec.Kind)
		}
	}
	n.persist.durable = n.hard
	return nil
}

// restoreEntry replays one entry record into the log.
//
// Four cases, and the third is the one that matters: an entry at an index the
// log already holds is what a leader's correction looked like when it was
// written, so replaying it must truncate the log there first. Appending
// without truncating would leave the follower's overwritten entries in place
// above the correction and silently resurrect a history that was discarded.
func (n *Node) restoreEntry(e Entry) error {
	switch {
	case e.Index <= n.log.Base():
		// Already absorbed by a snapshot that was written after this entry.
		// The log cannot hold it and does not need to.
		return nil
	case e.Index == n.log.LastIndex()+1:
		return n.log.Append(e)
	case e.Index > n.log.LastIndex()+1:
		return fmt.Errorf("%w: entry index %d leaves a gap after %d", ErrNonContiguous, e.Index, n.log.LastIndex())
	default:
		if err := n.log.TruncateFrom(e.Index); err != nil {
			return err
		}
		return n.log.Append(e)
	}
}

// Step consumes one event and returns the actions the runtime should perform.
//
// The events handled here are the ones term and vote persistence needs:
// PersistDone, which is where every deferred consequence of a write is carried
// out, and Deliver of the vote RPCs. ClientCall and TimerFired fall through to
// nil deliberately rather than being handled emptily — no node can become a
// leader until elections exist (T2.2), so there is as yet no state in which a
// replica could accept a command, and no timer is ever armed for one to fire.
func (n *Node) Step(now node.Time, ev node.Event) []node.Action {
	switch e := ev.(type) {
	case node.PersistDone:
		return n.onPersistDone(e)
	case node.Deliver:
		return n.onDeliver(e)
	}
	return nil
}

// onDeliver dispatches an incoming message by its concrete type. An unknown
// message is ignored: a node must not fail because a peer running a newer
// build sent it something it has not learned about yet.
func (n *Node) onDeliver(e node.Deliver) []node.Action {
	switch m := e.Msg.(type) {
	case RequestVote:
		return n.onRequestVote(e.From, m)
	case RequestVoteResp:
		return n.onRequestVoteResp(m)
	}
	return nil
}

// onPersistDone carries out whatever the completed write authorised.
//
// A failed write authorises nothing. The node keeps the state it believes —
// there is nothing unsafe about believing a higher term than it can prove —
// but the response that would have rested on that proof is not sent, and the
// candidate waiting for it will time out and ask again. That is the whole of
// INV-8 as it applies here: the write not landing costs a round trip, never
// correctness.
func (n *Node) onPersistDone(e node.PersistDone) []node.Action {
	pend, ok := n.persist.take(e.ID)
	if !ok {
		// A completion for a write this incarnation did not issue. A restart
		// discards the pending queue while the runtime may still have the
		// event in flight, so this is survivable and is ignored.
		return nil
	}
	if e.Err != nil {
		return nil
	}
	n.persist.complete(pend)

	switch pend.kind {
	case pendVoteResp:
		// The answer was decided when the write was issued, and the node may
		// have moved on since: a second vote request, at a later term, can
		// arrive and be written while the first write is still in flight, and
		// nothing in node.Persist promises the two complete in order.
		//
		// An answer describing a term this node has left is one it can no
		// longer prove — a later hard state supersedes it on disk, so a
		// restart would not recover the vote it announces. Sending it anyway
		// happens to be safe, because the higher term now blocks any further
		// vote in the older one, but "safe by a coincidence two rules away"
		// is exactly what R1 exists to stop relying on. It is replaced by a
		// rejection carrying the current term, which is provable and more
		// useful: it tells a superseded candidate to give up.
		resp := pend.resp
		if resp.Term != n.hard.Term {
			resp = RequestVoteResp{Term: n.hard.Term, VoteGranted: false}
		}
		return []node.Action{node.Send{To: pend.to, Msg: resp}}
	}
	return nil
}

// recordHardState returns the actions that make the current hard state
// durable, carrying pend as the work to do once it lands. It returns nil when
// the state is already durable and there is nothing to write.
//
// pend.hard is filled in here rather than by the caller, so that what a
// completion later reports as durable is exactly the state that was handed to
// the runtime, and cannot drift if n.hard moves on before the write lands.
func (n *Node) recordHardState(pend pending) []node.Action {
	if n.hard.Equal(n.persist.durable) {
		return nil
	}
	pend.hard = n.hard
	return []node.Action{n.persist.issue([]node.Record{encodeHardState(n.hard)}, pend)}
}

// stepDown moves this node to a follower in a later term, forgetting the vote
// it cast in the old one. Discarding the vote is not optional: a vote is a
// promise scoped to a single term, and carrying one forward would let a node
// refuse to vote in a term it has not yet voted in.
func (n *Node) stepDown(term Term) {
	n.hard.Term = term
	n.hard.VotedFor = None
	n.role = Follower
}
