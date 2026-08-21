package raft

import "verity/node"

// The Raft RPCs, as node.Message values.
//
// SPEC section 6.1 names four request/response pairs: PreVote, RequestVote,
// AppendEntries and InstallSnapshot. The vote pair is defined here because it
// is what term and vote persistence exists to protect; the rest arrive with
// the tasks that implement them, so that no message type exists before
// anything sends or handles it.
//
// Kind strings are frozen. They name the message in determinism traces, and a
// trace is compared textually across runs and across machines, so renaming one
// silently invalidates every recorded trace that mentions it.
//
// Size is the wire size the simulator's network model charges for
// transmission. It need not be exact — nothing serialises these structs under
// simulation — only deterministic and roughly proportional, so that a message
// carrying more data takes longer to arrive.

// RequestVote asks a peer to vote for the sender in Term.
//
// LastLogIndex and LastLogTerm describe the candidate's log and exist for the
// election restriction: a voter compares them against its own log and refuses
// a candidate that is behind (R3).
type RequestVote struct {
	Term         Term
	CandidateID  node.NodeID
	LastLogIndex Index
	LastLogTerm  Term
}

// Kind names the message for traces.
func (RequestVote) Kind() string { return "RequestVote" }

// Size reports the wire size: four eight-byte fields.
func (RequestVote) Size() int { return 32 }

// RequestVoteResp answers a RequestVote.
//
// Term is the voter's current term, which may exceed the candidate's. A
// candidate that sees a higher term here has been superseded and steps down;
// that is true whether or not the vote was granted, so the field is meaningful
// in both cases.
type RequestVoteResp struct {
	Term        Term
	VoteGranted bool
}

// Kind names the message for traces.
func (RequestVoteResp) Kind() string { return "RequestVoteResp" }

// Size reports the wire size: a term and a flag.
func (RequestVoteResp) Size() int { return 16 }
