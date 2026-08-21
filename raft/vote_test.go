package raft

import (
	"errors"
	"sort"
	"testing"

	"verity/node"
	"verity/prng"
)

// ---------------------------------------------------------------- helpers ---

// voteHarness drives one replica and keeps the account of its disk that the
// node itself is not allowed to keep: which records have actually landed.
//
// The distinction is the whole subject of this file. The node's own hard state
// says what it believes; durable says what would survive a crash. A test that
// only inspected the node could not tell the difference, and R1 is precisely a
// claim about the difference.
type voteHarness struct {
	t       *testing.T
	n       *Node
	id      node.NodeID
	peers   []node.NodeID
	now     node.Time
	pending []node.Persist // issued, not yet acknowledged
	landed  []node.Persist // acknowledged successfully, kept in issue order
	sent    []node.Send
}

// durableRecords returns what a crash would leave on disk, in the order it was
// written.
//
// Ordering by persist ID rather than by completion is the faithful model. The
// simulator's disk takes the bytes at the moment the action is performed, so
// on-disk order is issue order however the acknowledgements are scheduled
// (SPEC section 5.3). Recording them in completion order instead would quietly
// reorder the log whenever two writes overlapped, and a later hard state would
// stop superseding an earlier one — which is precisely the case this file
// needs to be able to see.
func (h *voteHarness) durableRecords() []node.Record {
	landed := make([]node.Persist, len(h.landed))
	copy(landed, h.landed)
	sort.Slice(landed, func(i, j int) bool { return landed[i].ID < landed[j].ID })
	var out []node.Record
	for _, p := range landed {
		out = append(out, p.Records...)
	}
	return out
}

// newVoteHarness builds a replica of the given membership with a fixed seed.
func newVoteHarness(t *testing.T, id node.NodeID, peers ...node.NodeID) *voteHarness {
	t.Helper()
	return &voteHarness{
		t:     t,
		n:     New(id, peers, prng.New(0x5EED)),
		id:    id,
		peers: peers,
	}
}

// step advances the clock and feeds the node one event, recording every
// Persist it issues and every Send it emits.
func (h *voteHarness) step(ev node.Event) []node.Action {
	h.t.Helper()
	h.now = h.now.Add(node.Millisecond)
	acts := h.n.Step(h.now, ev)
	for _, a := range acts {
		switch act := a.(type) {
		case node.Persist:
			h.pending = append(h.pending, act)
		case node.Send:
			h.sent = append(h.sent, act)
		}
	}
	h.checkNoUnprovenGrant(acts)
	return acts
}

// requestVote delivers a RequestVote from the given candidate.
func (h *voteHarness) requestVote(from node.NodeID, term Term, lastIdx Index, lastTerm Term) []node.Action {
	h.t.Helper()
	return h.step(node.Deliver{From: from, Msg: RequestVote{
		Term:         term,
		CandidateID:  from,
		LastLogIndex: lastIdx,
		LastLogTerm:  lastTerm,
	}})
}

// complete delivers a successful PersistDone for the outstanding write with
// the given ID, wherever it sits in the queue, so that a test can acknowledge
// writes out of the order they were issued.
func (h *voteHarness) complete(id uint64) []node.Action {
	h.t.Helper()
	for i, p := range h.pending {
		if p.ID != id {
			continue
		}
		h.pending = append(h.pending[:i], h.pending[i+1:]...)
		h.landed = append(h.landed, p)
		return h.step(node.PersistDone{ID: id})
	}
	h.t.Fatalf("no outstanding persist with ID %d", id)
	return nil
}

// completeLastPersist delivers a successful PersistDone for the oldest
// outstanding write, which is the ordinary case.
func (h *voteHarness) completeLastPersist() []node.Action {
	h.t.Helper()
	if len(h.pending) == 0 {
		h.t.Fatal("no outstanding persist to complete")
	}
	return h.complete(h.pending[0].ID)
}

// failLastPersist delivers a failed PersistDone for the oldest outstanding
// write. Nothing is added to the durable set, which is exactly what a write
// that did not reach stable storage means (INV-8).
func (h *voteHarness) failLastPersist(err error) []node.Action {
	h.t.Helper()
	if len(h.pending) == 0 {
		h.t.Fatal("no outstanding persist to fail")
	}
	p := h.pending[0]
	h.pending = h.pending[1:]
	return h.step(node.PersistDone{ID: p.ID, Err: err})
}

// checkNoUnprovenGrant enforces R1 on every batch of actions any test in this
// file produces: a granted vote must never leave the node unless the term and
// vote justifying it are already durable.
//
// It is applied automatically from step rather than asserted case by case, so
// that a test written later cannot forget it. Rejections are deliberately not
// checked: refusing a candidate commits this node to nothing, so a rejection
// needs no proof behind it.
func (h *voteHarness) checkNoUnprovenGrant(acts []node.Action) {
	h.t.Helper()
	for _, a := range acts {
		s, ok := a.(node.Send)
		if !ok {
			continue
		}
		resp, ok := s.Msg.(RequestVoteResp)
		if !ok || !resp.VoteGranted {
			continue
		}
		hard, found := h.durableHardState()
		if !found {
			h.t.Fatalf("granted a vote in term %d with no durable hard state at all", resp.Term)
		}
		if hard.Term != resp.Term {
			h.t.Fatalf("granted a vote in term %d but the durable term is %d", resp.Term, hard.Term)
		}
		if hard.VotedFor == None {
			h.t.Fatalf("granted a vote in term %d but the durable state records no vote", resp.Term)
		}
	}
}

// durableHardState decodes the last hard state actually written to disk, which
// is what a restart would recover.
func (h *voteHarness) durableHardState() (HardState, bool) {
	h.t.Helper()
	var out HardState
	found := false
	for _, rec := range h.durableRecords() {
		if rec.Kind != node.RecordHardState {
			continue
		}
		hs, err := decodeHardState(rec)
		if err != nil {
			h.t.Fatalf("durable hard state does not decode: %v", err)
		}
		out, found = hs, true
	}
	return out, found
}

// restart rebuilds the replica from the durable records alone, which is what
// the runtime does after a crash: the node value is dropped, not paused, so
// anything not on disk is genuinely gone.
func (h *voteHarness) restart() {
	h.t.Helper()
	h.n = New(h.id, h.peers, prng.New(0x5EED))
	if err := h.n.Restore(h.durableRecords()); err != nil {
		h.t.Fatalf("Restore after restart: %v", err)
	}
	h.pending = nil
}

// onlyPersist asserts that acts is exactly one Persist and returns it. This is
// the shape R1 demands of the interesting path: the node's entire visible
// output on deciding a vote is a write.
func onlyPersist(t *testing.T, acts []node.Action) node.Persist {
	t.Helper()
	if len(acts) != 1 {
		t.Fatalf("got %d actions %s, want exactly one Persist", len(acts), kindsOf(acts))
	}
	p, ok := acts[0].(node.Persist)
	if !ok {
		t.Fatalf("got %s, want Persist", acts[0].ActionKind())
	}
	return p
}

// onlyVoteResp asserts that acts is exactly one Send of a RequestVoteResp and
// returns the message together with its destination.
func onlyVoteResp(t *testing.T, acts []node.Action) (RequestVoteResp, node.NodeID) {
	t.Helper()
	if len(acts) != 1 {
		t.Fatalf("got %d actions %s, want exactly one Send", len(acts), kindsOf(acts))
	}
	s, ok := acts[0].(node.Send)
	if !ok {
		t.Fatalf("got %s, want Send", acts[0].ActionKind())
	}
	resp, ok := s.Msg.(RequestVoteResp)
	if !ok {
		t.Fatalf("got message %s, want RequestVoteResp", s.Msg.Kind())
	}
	return resp, s.To
}

// kindsOf renders a batch of actions for a failure message.
func kindsOf(acts []node.Action) string {
	if len(acts) == 0 {
		return "[]"
	}
	out := "["
	for i, a := range acts {
		if i > 0 {
			out += ","
		}
		out += a.ActionKind()
	}
	return out + "]"
}

// appendEntries puts entries into a node's log directly, standing in for the
// replication that has not been written yet, so that the election restriction
// has logs of different lengths to compare.
func appendEntries(t *testing.T, n *Node, terms ...Term) {
	t.Helper()
	for i, term := range terms {
		e := Entry{Term: term, Index: n.log.LastIndex() + 1, Type: EntryNormal}
		if err := n.log.Append(e); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
}

// ------------------------------------------------------------------ tests ---

// TestVoteIsDurableBeforeTheResponseIsSent is T2.1's acceptance criterion.
//
// Deciding to grant a vote produces a write and nothing else. The response
// appears only once that write has been acknowledged. Between the two, the
// node has committed to nothing observable, which is what makes the commitment
// survivable: a crash in that window loses the vote and the candidate simply
// asks again.
func TestVoteIsDurableBeforeTheResponseIsSent(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)

	acts := h.requestVote(2, 1, 0, 0)
	p := onlyPersist(t, acts)

	if !p.Sync {
		t.Fatal("vote persisted without Sync, so PersistDone would promise nothing")
	}
	if len(p.Records) != 1 || p.Records[0].Kind != node.RecordHardState {
		t.Fatalf("got records %+v, want one RecordHardState", p.Records)
	}
	hs, err := decodeHardState(p.Records[0])
	if err != nil {
		t.Fatalf("persisted record does not decode: %v", err)
	}
	if hs.Term != 1 || hs.VotedFor != 2 {
		t.Fatalf("persisted hard state is term %d voted %d, want term 1 voted 2", hs.Term, hs.VotedFor)
	}

	resp, to := onlyVoteResp(t, h.completeLastPersist())
	if !resp.VoteGranted {
		t.Fatal("vote was not granted after the write landed")
	}
	if resp.Term != 1 {
		t.Fatalf("response term = %d, want 1", resp.Term)
	}
	if to != 2 {
		t.Fatalf("response sent to %d, want 2", to)
	}
}

// TestGrantSurvivesACrashTakenTheInstantItWasSent is the argument for R1 stated
// as a test rather than as a comment.
//
// It crashes the replica at the one moment that matters — after the vote has
// been answered — and rebuilds it from what was genuinely on disk. The
// restored node must still remember the vote, because a node that forgot it
// would happily grant a second one in the same term, and two candidates would
// each hold a majority.
func TestGrantSurvivesACrashTakenTheInstantItWasSent(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)

	h.requestVote(2, 1, 0, 0)
	if resp, _ := onlyVoteResp(t, h.completeLastPersist()); !resp.VoteGranted {
		t.Fatal("vote was not granted")
	}

	h.restart()

	if got := h.n.hard.Term; got != 1 {
		t.Fatalf("term after restart = %d, want 1", got)
	}
	if got := h.n.hard.VotedFor; got != 2 {
		t.Fatalf("vote after restart = %d, want 2", got)
	}

	// The restored node must now refuse a different candidate in that term.
	resp, _ := onlyVoteResp(t, h.requestVote(3, 1, 0, 0))
	if resp.VoteGranted {
		t.Fatal("restored node granted a second vote in a term it had already voted in")
	}
}

// TestCrashBeforeThePersistLandsLosesNothingObservable is the other half of
// the argument. A crash in the window between deciding and answering must
// leave no trace: no response went out, so nobody counted a vote that the
// restarted node cannot account for.
func TestCrashBeforeThePersistLandsLosesNothingObservable(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)

	onlyPersist(t, h.requestVote(2, 1, 0, 0))
	if len(h.sent) != 0 {
		t.Fatalf("got %d sends before the write landed, want none", len(h.sent))
	}

	h.restart()

	if got := h.n.hard.VotedFor; got != None {
		t.Fatalf("vote after restart = %d, want None: the write never landed", got)
	}
	// Having promised nothing, the restarted node is free to vote for anyone.
	h.requestVote(3, 1, 0, 0)
	if resp, _ := onlyVoteResp(t, h.completeLastPersist()); !resp.VoteGranted {
		t.Fatal("restarted node refused a vote it had never actually cast")
	}
}

// TestFailedPersistWithholdsTheResponse covers INV-8's unhappy path: a write
// that did not reach stable storage authorises nothing, so the response that
// would have rested on it is not sent. The candidate times out and asks again,
// which costs a round trip and never correctness.
func TestFailedPersistWithholdsTheResponse(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)

	onlyPersist(t, h.requestVote(2, 1, 0, 0))

	if acts := h.failLastPersist(errors.New("disk full")); len(acts) != 0 {
		t.Fatalf("got %s after a failed write, want no actions", kindsOf(acts))
	}
	if _, found := h.durableHardState(); found {
		t.Fatal("a failed write left durable state behind")
	}
	if h.n.persist.durable.VotedFor != None {
		t.Fatalf("node treats a failed write as durable: durable vote = %d", h.n.persist.durable.VotedFor)
	}
}

// TestStaleCandidateIsRejectedWithoutAWrite records the deliberate asymmetry
// between granting and refusing. A rejection commits this node to nothing, so
// it needs no proof behind it and is answered immediately. Making every
// response wait for a write would be safe but would put a disk round trip in
// front of the most common message in a busy election.
func TestStaleCandidateIsRejectedWithoutAWrite(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)

	// Reach term 5 durably.
	h.requestVote(2, 5, 0, 0)
	h.completeLastPersist()

	acts := h.requestVote(3, 3, 0, 0)
	resp, to := onlyVoteResp(t, acts)
	if resp.VoteGranted {
		t.Fatal("granted a vote to a candidate from an older term")
	}
	if resp.Term != 5 {
		t.Fatalf("response term = %d, want 5 so the stale candidate learns it is behind", resp.Term)
	}
	if to != 3 {
		t.Fatalf("response sent to %d, want 3", to)
	}
}

// TestRepeatedRequestFromTheSameCandidateIsNotRewritten checks that a
// duplicated or retried RequestVote is answered from the durable state rather
// than rewriting it. The network may duplicate freely, and a node that wrote
// to disk on every copy would turn a lost response into an unbounded amount of
// I/O.
func TestRepeatedRequestFromTheSameCandidateIsNotRewritten(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)

	h.requestVote(2, 1, 0, 0)
	h.completeLastPersist()

	for i := 0; i < 3; i++ {
		resp, _ := onlyVoteResp(t, h.requestVote(2, 1, 0, 0))
		if !resp.VoteGranted {
			t.Fatalf("retry %d: the same candidate was refused the vote it already holds", i)
		}
	}
	if len(h.pending) != 0 {
		t.Fatalf("got %d further writes for a repeated request, want none", len(h.pending))
	}
}

// TestOneVotePerTerm is the property the durable vote exists to enforce.
func TestOneVotePerTerm(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)

	h.requestVote(2, 1, 0, 0)
	if resp, _ := onlyVoteResp(t, h.completeLastPersist()); !resp.VoteGranted {
		t.Fatal("first candidate was refused")
	}

	resp, _ := onlyVoteResp(t, h.requestVote(3, 1, 0, 0))
	if resp.VoteGranted {
		t.Fatal("granted a second vote in the same term")
	}
}

// TestHigherTermForgetsTheOldVote checks that a vote is scoped to its term. A
// node carrying a vote forward into a later term would refuse to vote in a
// term it had never actually voted in, and an election that should have
// succeeded would stall.
func TestHigherTermForgetsTheOldVote(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)

	h.requestVote(2, 1, 0, 0)
	h.completeLastPersist()

	// A different candidate, one term later.
	h.requestVote(3, 2, 0, 0)
	resp, _ := onlyVoteResp(t, h.completeLastPersist())
	if !resp.VoteGranted {
		t.Fatal("refused a vote in a new term because of a vote cast in the old one")
	}
	if resp.Term != 2 {
		t.Fatalf("response term = %d, want 2", resp.Term)
	}
	if h.n.role != Follower {
		t.Fatalf("role = %s, want Follower after seeing a higher term", h.n.role)
	}
}

// TestVoteRefusedForAStaleLog is the election restriction (R3) seen through
// the vote handler rather than through the log comparison it delegates to.
func TestVoteRefusedForAStaleLog(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)
	appendEntries(t, h.n, 1, 1, 2) // this node holds three entries, last term 2

	cases := []struct {
		name     string
		lastIdx  Index
		lastTerm Term
		want     bool
	}{
		{name: "shorter log, same term", lastIdx: 2, lastTerm: 2, want: false},
		{name: "identical log", lastIdx: 3, lastTerm: 2, want: true},
		{name: "longer log, same term", lastIdx: 9, lastTerm: 2, want: true},
		{name: "older term, much longer log", lastIdx: 99, lastTerm: 1, want: false},
		{name: "newer term, shorter log", lastIdx: 1, lastTerm: 3, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A fresh replica per case, so an earlier grant cannot decide a
			// later one.
			h := newVoteHarness(t, 1, 1, 2, 3)
			appendEntries(t, h.n, 1, 1, 2)

			acts := h.requestVote(2, 5, c.lastIdx, c.lastTerm)
			var resp RequestVoteResp
			if _, isPersist := acts[0].(node.Persist); isPersist {
				resp, _ = onlyVoteResp(t, h.completeLastPersist())
			} else {
				resp, _ = onlyVoteResp(t, acts)
			}
			if resp.VoteGranted != c.want {
				t.Fatalf("granted = %v, want %v", resp.VoteGranted, c.want)
			}
		})
	}
}

// TestRejectionStillWritesTheNewTerm guards a case easy to get backwards. A
// candidate with a higher term but a stale log is refused, yet the term it
// carried is real and this node has moved into it. That move must be written
// down: it is the state the next vote request will be judged against.
func TestRejectionStillWritesTheNewTerm(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)
	appendEntries(t, h.n, 1, 1, 5)

	acts := h.requestVote(2, 7, 1, 1) // higher term, hopelessly stale log
	p := onlyPersist(t, acts)
	hs, err := decodeHardState(p.Records[0])
	if err != nil {
		t.Fatalf("persisted record does not decode: %v", err)
	}
	if hs.Term != 7 {
		t.Fatalf("persisted term = %d, want 7", hs.Term)
	}
	if hs.VotedFor != None {
		t.Fatalf("persisted vote = %d, want None for a refused candidate", hs.VotedFor)
	}

	resp, _ := onlyVoteResp(t, h.completeLastPersist())
	if resp.VoteGranted {
		t.Fatal("granted a vote to a candidate with a stale log")
	}
	if resp.Term != 7 {
		t.Fatalf("response term = %d, want 7", resp.Term)
	}
}

// TestVoteRespWithHigherTermStepsDownDurably covers the rule that applies to
// every RPC response in Raft, not only this one: a reply carrying a higher
// term means this node has been superseded. Counting votes belongs to a
// candidate and arrives with elections (T2.2); the step-down is universal and
// belongs here.
func TestVoteRespWithHigherTermStepsDownDurably(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)

	acts := h.step(node.Deliver{From: 2, Msg: RequestVoteResp{Term: 9, VoteGranted: false}})
	p := onlyPersist(t, acts)
	hs, err := decodeHardState(p.Records[0])
	if err != nil {
		t.Fatalf("persisted record does not decode: %v", err)
	}
	if hs.Term != 9 || hs.VotedFor != None {
		t.Fatalf("persisted hard state is term %d voted %d, want term 9 voted None", hs.Term, hs.VotedFor)
	}

	// Nothing is sent in reply: a vote response is not itself answered.
	if acts := h.completeLastPersist(); len(acts) != 0 {
		t.Fatalf("got %s, want no actions", kindsOf(acts))
	}
	if h.n.hard.Term != 9 {
		t.Fatalf("term = %d, want 9", h.n.hard.Term)
	}
}

// TestVoteRespWithoutAHigherTermChangesNothing is the complement: a response
// at or below the current term carries no news, so it must not provoke a write.
func TestVoteRespWithoutAHigherTermChangesNothing(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)
	h.requestVote(2, 4, 0, 0)
	h.completeLastPersist()

	for _, term := range []Term{1, 3, 4} {
		if acts := h.step(node.Deliver{From: 2, Msg: RequestVoteResp{Term: term, VoteGranted: true}}); len(acts) != 0 {
			t.Fatalf("term %d: got %s, want no actions", term, kindsOf(acts))
		}
	}
}

// TestPersistDoneForAnUnknownIDIsIgnored covers a completion for a write this
// incarnation never issued. A restart discards the pending queue while the
// runtime may still hold the event, so this is survivable and must not be
// treated as a bug.
func TestPersistDoneForAnUnknownIDIsIgnored(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)

	if acts := h.step(node.PersistDone{ID: 999}); len(acts) != 0 {
		t.Fatalf("got %s, want no actions", kindsOf(acts))
	}
	if h.n.persist.durable != (HardState{Term: 0, VotedFor: None, CommitIndex: 0}) {
		t.Fatalf("an unknown completion moved the durable state to %+v", h.n.persist.durable)
	}
}

// TestOlderCompletionDoesNotRegressTheDurableState guards the ordering rule in
// persister.complete. node.Persist promises nothing about the order
// completions arrive in, and an older write landing after a newer one must not
// be allowed to overwrite the record of what is durable — that would report
// state as proven that is not, which is the single thing the type exists to
// prevent.
func TestOlderCompletionDoesNotRegressTheDurableState(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)

	// Two writes in flight: term 1 voted 2, then term 4 voted None.
	first := onlyPersist(t, h.requestVote(2, 1, 0, 0))
	second := onlyPersist(t, h.step(node.Deliver{From: 3, Msg: RequestVoteResp{Term: 4}}))
	if first.ID == second.ID {
		t.Fatalf("both writes used ID %d, want distinct IDs", first.ID)
	}

	// Complete them out of order: the newer one first.
	h.complete(second.ID)
	if got := h.n.persist.durable.Term; got != 4 {
		t.Fatalf("durable term = %d, want 4", got)
	}
	h.complete(first.ID)
	if got := h.n.persist.durable.Term; got != 4 {
		t.Fatalf("durable term regressed to %d after an older write landed, want 4", got)
	}
}

// TestLateWriteAnswersWithTheCurrentTermNotTheDecidedOne covers the window
// between deciding a vote and its write landing, during which a second request
// at a later term can arrive and be written. Nothing in node.Persist promises
// the two writes complete in the order they were issued.
//
// The first answer was decided in term 1 and granted. By the time its write
// lands the node is in term 4, and the term-1 hard state has been superseded
// on disk, so a restart would not recover the vote that answer announces.
// Sending it would be announcing a vote the node cannot prove — the very
// thing R1 forbids — even though the higher term happens to make it harmless.
// The answer is replaced by a rejection carrying the current term.
//
// This is bug B2 in docs/BUGS.md.
func TestLateWriteAnswersWithTheCurrentTermNotTheDecidedOne(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)

	first := onlyPersist(t, h.requestVote(2, 1, 0, 0))
	second := onlyPersist(t, h.step(node.Deliver{From: 3, Msg: RequestVoteResp{Term: 4}}))

	h.complete(second.ID)

	resp, to := onlyVoteResp(t, h.complete(first.ID))
	if resp.VoteGranted {
		t.Fatal("announced a granted vote for a term the node had already left")
	}
	if resp.Term != 4 {
		t.Fatalf("response term = %d, want 4 so the superseded candidate learns it is behind", resp.Term)
	}
	if to != 2 {
		t.Fatalf("response sent to %d, want 2", to)
	}
}

// TestNoGrantEverPrecedesItsWrite sweeps a long pseudo-random sequence of vote
// traffic past the replica, with writes completing, failing, and arriving out
// of order, and lets voteHarness.checkNoUnprovenGrant assert R1 after every
// single step.
//
// The individual tests above each pin one path. This one exists because R1 is
// a property of every path, including the ones nobody thought to enumerate.
func TestNoGrantEverPrecedesItsWrite(t *testing.T) {
	h := newVoteHarness(t, 1, 1, 2, 3)
	appendEntries(t, h.n, 1, 1, 2)
	rng := prng.New(0xB0A710A5)

	grants := 0
	for i := 0; i < 2000; i++ {
		switch {
		case len(h.pending) > 0 && rng.Chance(0.4):
			// Complete or fail an outstanding write, sometimes out of order.
			k := rng.Intn(len(h.pending))
			p := h.pending[k]
			if rng.Chance(0.15) {
				h.pending = append(h.pending[:k], h.pending[k+1:]...)
				h.step(node.PersistDone{ID: p.ID, Err: errors.New("write failed")})
				continue
			}
			for _, a := range h.complete(p.ID) {
				if s, ok := a.(node.Send); ok {
					if resp, ok := s.Msg.(RequestVoteResp); ok && resp.VoteGranted {
						grants++
					}
				}
			}
		default:
			from := node.NodeID(2 + rng.Intn(2))
			term := Term(rng.Uint64n(6))
			h.requestVote(from, term, Index(rng.Uint64n(5)), Term(rng.Uint64n(4)))
		}
	}
	// A sweep that never granted anything would pass the property vacuously.
	if grants == 0 {
		t.Fatal("no vote was granted in 2000 steps, so the property held vacuously")
	}
	t.Logf("granted %d votes across the sweep", grants)
}
