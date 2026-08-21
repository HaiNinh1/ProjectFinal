package raft

import "verity/node"

// Term is a Raft term number. Terms are a logical clock: they increase
// monotonically, never reset, and a node that learns of a higher term always
// steps down to a follower in it.
type Term uint64

// Index is a position in the replicated log. The first real entry is index 1.
// Index 0 is the position before the log begins; it always exists, always has
// term 0, and never holds a command, which lets the AppendEntries consistency
// check (R4) treat "the entry before the first one" without a special case.
type Index uint64

// None is the NodeID meaning "no node": no vote cast, or no leader known.
// Replicas are numbered from 1, so zero is free to mean absence.
const None node.NodeID = 0

// Role is a replica's current position in the protocol.
type Role uint8

// The three Raft roles. A replica starts as a Follower on both boot and
// restart: nothing durable records that this node was ever a leader, and
// assuming otherwise would let a restarted node act on authority it may have
// lost while it was down.
const (
	Follower Role = iota
	Candidate
	Leader
)

// String names the role for traces and test failures.
func (r Role) String() string {
	switch r {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	default:
		return "Role(?)"
	}
}

// HardState is the part of a replica's state that Raft requires to survive a
// crash. Its contents are dictated by SPEC section 6.2.
//
// Term and VotedFor are the safety-critical pair: a node that forgets either
// one can vote twice in the same term and elect two leaders. That is why R1
// exists, and why this type is written durably before any response that
// depends on it leaves the node.
//
// CommitIndex is included because SPEC section 6.2 says RecordHardState
// carries it. Raft itself does not require it to be durable — a restarted node
// can relearn the commit index from its leader — but persisting it lets a node
// resume applying without waiting for contact, and it costs nothing to write
// alongside the other two.
type HardState struct {
	Term        Term
	VotedFor    node.NodeID
	CommitIndex Index
}

// Equal reports whether two hard states are identical. It is the test for
// "does this need to be written down", so it must compare every field: a
// missed field is a durable write silently skipped.
func (h HardState) Equal(o HardState) bool {
	return h.Term == o.Term && h.VotedFor == o.VotedFor && h.CommitIndex == o.CommitIndex
}
