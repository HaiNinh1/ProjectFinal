package raft

import "verity/node"

// onRequestVote decides whether to vote for a candidate, and — this is R1 —
// makes the decision durable before letting the answer leave the node.
//
// The ordering is the entire point of the rule. A node that answers first and
// writes afterwards can crash in between, come back with no memory of the
// vote, and grant a second one in the same term. Two candidates then each hold
// a majority, two leaders exist in one term, and the logs diverge with nothing
// anywhere reporting an error. So a granted vote is returned from the
// PersistDone handler, never from here: this function's entire output on the
// interesting path is a single Persist.
//
// A rejection is different, and deliberately so. Refusing a candidate commits
// this node to nothing, so a rejection that changes no durable state is
// answered immediately. Only a response the node would have to honour later
// needs proof behind it, which is why the test below is whether the hard state
// changed, not whether the vote was granted.
func (n *Node) onRequestVote(from node.NodeID, m RequestVote) []node.Action {
	// A candidate from a term this node has already left is stale. Answering
	// with the current term is what tells it to give up and catch up.
	if m.Term < n.hard.Term {
		return []node.Action{node.Send{
			To:  from,
			Msg: RequestVoteResp{Term: n.hard.Term, VoteGranted: false},
		}}
	}

	// A higher term supersedes whatever this node was doing, including any
	// vote it had cast in the older term.
	if m.Term > n.hard.Term {
		n.stepDown(m.Term)
	}

	// One vote per term, and only to a candidate whose log is at least as up
	// to date as this node's (R3). Re-granting to the same candidate is
	// allowed and necessary: a lost response makes a candidate ask again, and
	// refusing the retry would strand an election that had already been won.
	granted := (n.hard.VotedFor == None || n.hard.VotedFor == m.CandidateID) &&
		n.log.UpToDate(m.LastLogTerm, m.LastLogIndex)
	if granted {
		n.hard.VotedFor = m.CandidateID
	}
	resp := RequestVoteResp{Term: n.hard.Term, VoteGranted: granted}

	// If nothing changed there is nothing to prove, and the answer can go
	// straight out. This is not merely an optimisation: it is the path a
	// duplicated RequestVote takes, where the vote it asks for is one this
	// node already granted and already wrote down.
	if acts := n.recordHardState(pending{kind: pendVoteResp, to: from, resp: resp}); acts != nil {
		return acts
	}
	return []node.Action{node.Send{To: from, Msg: resp}}
}

// onRequestVoteResp handles a peer's answer to a vote request.
//
// Counting the votes belongs to a candidate, and candidacy arrives with
// elections (T2.2); until then the only rule that applies is the universal
// one, which holds for every RPC response in Raft and not just this one: a
// reply carrying a higher term means this node has been superseded, and it
// steps down into that term.
//
// The step-down is written down before anything else happens, because the new
// term is exactly the state a later vote request will be answered against.
// Nothing is sent in reply either way — a vote response is not itself answered.
func (n *Node) onRequestVoteResp(m RequestVoteResp) []node.Action {
	if m.Term <= n.hard.Term {
		return nil
	}
	n.stepDown(m.Term)
	return n.recordHardState(pending{kind: pendNothing})
}
