// Package echo implements the simplest possible node.Node: a
// majority-acknowledged echo.
//
// A client call is durably logged, broadcast to every peer, and answered once
// a strict majority of the cluster (including the coordinator itself) has
// echoed it back. There is no leader election, no log replay, and no
// conflict resolution — the point of this node is not to be useful, it is to
// be a real node.Node that the simulator can drive before any consensus code
// exists, so that the determinism harness (docs/ROADMAP.md T1.11) is tested
// against real event and action traffic rather than against nothing.
//
// Every event kind and every action kind is exercised: ClientCall produces
// Persist; PersistDone produces Send, SetTimer, or an error Reply; Deliver of
// either message kind produces a Send or a Reply; TimerFired produces
// retransmission; Restarted clears in-flight state. That breadth is
// deliberate — a harness that has only ever carried a no-op node has not
// been tested.
package echo

import "verity/node"

// New returns an echo node. peers must include id itself. rng is not taken:
// the echo node has no randomised behaviour, which keeps it a clean control
// for the determinism test.
//
// New panics if id does not appear in peers, or if peers contains a
// duplicate — both are caller bugs that would otherwise surface much later
// as a quorum that can never be reached.
func New(id node.NodeID, peers []node.NodeID, retry node.Duration) *Node {
	sorted := make([]node.NodeID, len(peers))
	copy(sorted, peers)
	insertionSortIDs(sorted)

	found := false
	for i, p := range sorted {
		if p == id {
			found = true
		}
		if i > 0 && sorted[i-1] == sorted[i] {
			panic("echo.New: peers contains a duplicate id")
		}
	}
	if !found {
		panic("echo.New: id is not present in peers")
	}

	self := 0
	for i, p := range sorted {
		if p == id {
			self = i
			break
		}
	}

	return &Node{
		id:           id,
		peers:        sorted,
		self:         self,
		retry:        retry,
		reqs:         make(map[uint64]*request),
		persistToReq: make(map[uint64]uint64),
	}
}

// Node is a majority-acknowledged echo node. It implements node.Node.
type Node struct {
	id    node.NodeID
	peers []node.NodeID // sorted ascending at construction; includes id
	self  int           // index of id within peers
	retry node.Duration

	nextPersistID uint64
	nextIndex     uint64

	// reqs tracks requests this node is coordinating, keyed by ReqID. A
	// request that has been replied to (or that failed to persist) is
	// removed; a lingering entry means the client is still waiting.
	reqs map[uint64]*request

	// persistToReq maps an outstanding Persist.ID back to the ReqID that
	// issued it, since PersistDone carries only the former.
	persistToReq map[uint64]uint64
}

// request is the state this node keeps while coordinating one ClientCall:
// the command being echoed, and which peers (by index into Node.peers) have
// acknowledged it. broadcasting is false while the entry is still waiting on
// PersistDone — until then, nothing has been sent and there is nothing for
// a retry timer to resend.
type request struct {
	cmd          []byte
	acked        []bool // parallel to Node.peers
	acks         int
	broadcasting bool
}

// majorityOf returns the number of acknowledgements, out of n peers, needed
// to commit: strictly more than half.
func majorityOf(n int) int { return n/2 + 1 }

// ID reports this node's identity.
func (n *Node) ID() node.NodeID { return n.id }

// Restore reinitialises the node from durable records recovered after a
// restart. The echo node writes nothing but RecordEntry, so any other kind
// is a record that belongs to some other node and Restore rejects it. The
// next log index is set one past the highest index seen, so freshly issued
// entries continue the sequence rather than colliding with recovered ones.
func (n *Node) Restore(recs []node.Record) error {
	var maxIndex uint64
	var sawEntry bool
	for _, r := range recs {
		if r.Kind != node.RecordEntry {
			return errNotEntry
		}
		if !sawEntry || r.Index > maxIndex {
			maxIndex = r.Index
			sawEntry = true
		}
	}
	if sawEntry {
		n.nextIndex = maxIndex + 1
	}
	return nil
}

// errNotEntry is returned by Restore when handed a record kind the echo
// node never writes.
var errNotEntry = restoreError("echo: Restore: record kind is not RecordEntry")

// restoreError is a static error value. The package may import nothing but
// verity/node, so it cannot use the standard errors package to construct
// one.
type restoreError string

func (e restoreError) Error() string { return string(e) }

// Step consumes one event and returns the actions it produces. See the
// package doc for the state machine this implements.
func (n *Node) Step(now node.Time, ev node.Event) []node.Action {
	switch e := ev.(type) {
	case node.ClientCall:
		return n.onClientCall(e)
	case node.PersistDone:
		return n.onPersistDone(e)
	case node.Deliver:
		return n.onDeliver(e)
	case node.TimerFired:
		return n.onTimerFired(e)
	case node.Restarted:
		return n.onRestarted()
	}
	return nil
}

// onClientCall starts coordinating a new request. Nothing is sent to any
// peer yet — INV-8 requires the entry be durable first — so the only action
// is the Persist itself. A ReqID already being coordinated is a retried
// call arriving before the first attempt finished; it is not re-persisted.
func (n *Node) onClientCall(e node.ClientCall) []node.Action {
	if _, inFlight := n.reqs[e.ReqID]; inFlight {
		return nil
	}

	id := n.nextPersistID
	n.nextPersistID++
	index := n.nextIndex
	n.nextIndex++

	n.reqs[e.ReqID] = &request{
		cmd:   e.Cmd,
		acked: make([]bool, len(n.peers)),
	}
	n.persistToReq[id] = e.ReqID

	return []node.Action{node.Persist{
		ID:      id,
		Records: []node.Record{{Kind: node.RecordEntry, Index: index, Data: e.Cmd}},
		Sync:    true,
	}}
}

// onPersistDone reacts to the entry backing a coordinated request reaching
// (or failing to reach) durable storage. A failure answers the client
// immediately with that error; INV-8 means a write that is not durable must
// not be treated as though it were, so nothing is sent to any peer. Success
// counts the coordinator's own durability as its first ack — the entry is
// durable locally, which is what a self-ack means — and either replies at
// once (a single-node cluster is already a majority of one) or broadcasts
// to every other peer and arms the retry timer.
func (n *Node) onPersistDone(e node.PersistDone) []node.Action {
	reqID, ok := n.persistToReq[e.ID]
	if !ok {
		return nil
	}
	delete(n.persistToReq, e.ID)

	req, ok := n.reqs[reqID]
	if !ok {
		return nil
	}

	if e.Err != nil {
		delete(n.reqs, reqID)
		return []node.Action{node.Reply{ReqID: reqID, Err: e.Err}}
	}

	req.acked[n.self] = true
	req.acks = 1

	if req.acks >= majorityOf(len(n.peers)) {
		delete(n.reqs, reqID)
		return []node.Action{node.Reply{ReqID: reqID, Resp: req.cmd, Err: nil}}
	}

	req.broadcasting = true
	actions := make([]node.Action, 0, len(n.peers))
	for _, p := range n.peers {
		if p == n.id {
			continue
		}
		actions = append(actions, node.Send{To: p, Msg: echoReq{ReqID: reqID, Payload: req.cmd}})
	}
	actions = append(actions, node.SetTimer{Name: "retry", After: n.retry})
	return actions
}

// onDeliver dispatches on the delivered message's concrete type.
func (n *Node) onDeliver(e node.Deliver) []node.Action {
	switch m := e.Msg.(type) {
	case echoReq:
		// A follower does not persist; it is an echo, not a log entry.
		return []node.Action{node.Send{To: e.From, Msg: echoResp{ReqID: m.ReqID, Payload: m.Payload}}}
	case echoResp:
		return n.onEchoResp(e.From, m)
	}
	return nil
}

// onEchoResp records one peer's acknowledgement of a request this node is
// coordinating. A response for a request this node is not coordinating (it
// already finished, or was never started here) is ignored, as is a second
// response from a peer that has already acked — the runtime duplicates and
// delays messages, and counting a peer twice would let a minority of
// distinct peers look like a majority. Reaching a majority answers the
// client and forgets the request, so any further response for it is
// silently dropped.
//
// The retry timer is cleared only when no other coordinated request still
// needs it (see anyOutstanding): SPEC section 3 makes timer names per-node,
// not per-request, so with several requests in flight there is exactly one
// shared "retry" timer. Clearing it unconditionally on the first request to
// finish disarms it for every other request too — the bug recorded as B1 in
// docs/BUGS.md, found by the simulator (100 overlapping client calls under
// packet loss, 6 of them stalled forever because an earlier call's Reply
// cleared the timer out from under them).
func (n *Node) onEchoResp(from node.NodeID, m echoResp) []node.Action {
	req, ok := n.reqs[m.ReqID]
	if !ok {
		return nil
	}

	idx := n.peerIndex(from)
	if idx < 0 || req.acked[idx] {
		return nil
	}
	req.acked[idx] = true
	req.acks++

	if req.acks >= majorityOf(len(n.peers)) {
		delete(n.reqs, m.ReqID)
		actions := []node.Action{node.Reply{ReqID: m.ReqID, Resp: req.cmd, Err: nil}}
		if !n.anyOutstanding() {
			actions = append(actions, node.ClearTimer{Name: "retry"})
		}
		return actions
	}
	return nil
}

// requestOutstanding reports whether req still needs the retry timer: it
// has broadcast (so it actually sent messages that could need resending)
// and at least one non-self peer has not yet acked. A request still
// awaiting PersistDone has not broadcast yet and is never outstanding by
// this definition, since nothing has been sent for it for a retry to
// repeat.
//
// This is the single definition of "outstanding" shared by onTimerFired
// (what to resend) and anyOutstanding (whether to disarm the timer). They
// must agree: if onTimerFired resent a request that anyOutstanding called
// finished, the timer would already be cleared and that request would wait
// forever (B1 again, in a different shape); if anyOutstanding called a
// request outstanding that onTimerFired never resends, the timer would keep
// re-arming itself forever for nothing.
func (n *Node) requestOutstanding(req *request) bool {
	if !req.broadcasting {
		return false
	}
	for _, p := range n.peers {
		if p == n.id {
			continue
		}
		if !req.acked[n.peerIndex(p)] {
			return true
		}
	}
	return false
}

// anyOutstanding reports whether any coordinated request still needs the
// shared retry timer. It ranges n.reqs for existence only — the result is a
// plain yes/no, not an ordered sequence of actions, so unlike onTimerFired
// there is nothing here for map iteration order to make nondeterministic
// (INV-6 constrains order that affects behaviour or output; a symmetric
// boolean check has neither).
func (n *Node) anyOutstanding() bool {
	for _, req := range n.reqs {
		if n.requestOutstanding(req) {
			return true
		}
	}
	return false
}

// onTimerFired resends echoReq to every peer that has not yet acked, for
// every request still outstanding (see requestOutstanding), and re-arms the
// timer. If nothing is outstanding, the timer is left disarmed — nothing
// will fire it again until the next PersistDone starts a new round.
func (n *Node) onTimerFired(e node.TimerFired) []node.Action {
	if e.Name != "retry" {
		return nil
	}

	// Requests live in a map keyed by ReqID, so visiting all of them in a
	// stable order requires collecting and sorting the keys first (INV-6):
	// Go's map iteration order is randomised, and re-sending retries in a
	// different order on every run would make this node's trace, and so
	// the determinism test, nondeterministic.
	ids := make([]uint64, 0, len(n.reqs))
	for id := range n.reqs {
		ids = append(ids, id)
	}
	insertionSortU64(ids)

	var actions []node.Action
	for _, id := range ids {
		req := n.reqs[id]
		if !n.requestOutstanding(req) {
			continue
		}
		for _, p := range n.peers {
			if p == n.id || req.acked[n.peerIndex(p)] {
				continue
			}
			actions = append(actions, node.Send{To: p, Msg: echoReq{ReqID: id, Payload: req.cmd}})
		}
	}
	if len(actions) == 0 {
		return nil
	}
	actions = append(actions, node.SetTimer{Name: "retry", After: n.retry})
	return actions
}

// onRestarted clears every request this node was coordinating. An echo that
// was in flight when the process crashed did not survive the crash — it was
// never applied anywhere as a durable, replied-to result — and the client
// is expected to retry it.
//
// This does not emit ClearTimer for the shared "retry" timer, and does not
// need to: with reqs empty, a "retry" TimerFired that still arrives after
// this returns finds requestOutstanding false for everything (there is
// nothing left to range), so onTimerFired returns nil and does not re-arm.
// The timer, if it was armed, fires at most once more and then falls
// silent on its own.
func (n *Node) onRestarted() []node.Action {
	n.reqs = make(map[uint64]*request)
	n.persistToReq = make(map[uint64]uint64)
	return nil
}

// peerIndex returns id's position in n.peers, or -1 if id is not a peer.
func (n *Node) peerIndex(id node.NodeID) int {
	for i, p := range n.peers {
		if p == id {
			return i
		}
	}
	return -1
}

// echoReq is broadcast by the coordinator of a request to every other peer.
type echoReq struct {
	ReqID   uint64
	Payload []byte
}

func (echoReq) Kind() string { return "echoReq" }

// Size reports a deterministic stand-in for wire size: a fixed header plus
// the payload. It need not be exact, only stable across runs.
func (m echoReq) Size() int { return 16 + len(m.Payload) }

// echoResp answers an echoReq, carrying the same ReqID and payload back to
// the sender.
type echoResp struct {
	ReqID   uint64
	Payload []byte
}

func (echoResp) Kind() string { return "echoResp" }

// Size reports a deterministic stand-in for wire size; see echoReq.Size.
func (m echoResp) Size() int { return 16 + len(m.Payload) }

// insertionSortIDs sorts ids ascending in place. The package may import
// nothing but verity/node, so it cannot reach for sort or slices; the peer
// list is tiny and sorted once at construction, so a hand-written
// insertion sort is simpler than justifying an exception to that rule.
func insertionSortIDs(ids []node.NodeID) {
	for i := 1; i < len(ids); i++ {
		v := ids[i]
		j := i - 1
		for j >= 0 && ids[j] > v {
			ids[j+1] = ids[j]
			j--
		}
		ids[j+1] = v
	}
}

// insertionSortU64 sorts ids ascending in place. See insertionSortIDs: same
// constraint, same reasoning, different element type (request keys are
// ReqIDs, not NodeIDs).
func insertionSortU64(ids []uint64) {
	for i := 1; i < len(ids); i++ {
		v := ids[i]
		j := i - 1
		for j >= 0 && ids[j] > v {
			ids[j+1] = ids[j]
			j--
		}
		ids[j+1] = v
	}
}
