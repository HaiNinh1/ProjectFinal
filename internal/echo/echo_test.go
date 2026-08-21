package echo

import (
	"testing"

	"verity/node"
)

// ---------------------------------------------------------------- helpers ---
//
// This file may import only "testing" and "verity/node", so it has no
// access to fmt, strconv, or errors. The helpers below hand-roll just
// enough string conversion to give assertion failures a readable message,
// and to give the determinism test (below) a way to compare two action
// sequences for exact equality.

func uitoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func errStr(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

func formatMessage(m node.Message) string {
	switch v := m.(type) {
	case echoReq:
		return "echoReq{ReqID:" + uitoa(v.ReqID) + ",Payload:" + string(v.Payload) + "}"
	case echoResp:
		return "echoResp{ReqID:" + uitoa(v.ReqID) + ",Payload:" + string(v.Payload) + "}"
	}
	return "unknownMessage"
}

func formatRecords(recs []node.Record) string {
	s := "["
	for i, r := range recs {
		if i > 0 {
			s += ","
		}
		s += "{Kind:" + uitoa(uint64(r.Kind)) + ",Index:" + uitoa(r.Index) + ",Data:" + string(r.Data) + "}"
	}
	return s + "]"
}

func formatAction(a node.Action) string {
	switch v := a.(type) {
	case node.Send:
		return "Send{To:" + uitoa(uint64(v.To)) + ",Msg:" + formatMessage(v.Msg) + "}"
	case node.Persist:
		return "Persist{ID:" + uitoa(v.ID) + ",Records:" + formatRecords(v.Records) + ",Sync:" + boolStr(v.Sync) + "}"
	case node.SetTimer:
		return "SetTimer{Name:" + v.Name + ",After:" + uitoa(uint64(v.After)) + "}"
	case node.ClearTimer:
		return "ClearTimer{Name:" + v.Name + "}"
	case node.Reply:
		return "Reply{ReqID:" + uitoa(v.ReqID) + ",Resp:" + string(v.Resp) + ",Err:" + errStr(v.Err) + "}"
	case node.Apply:
		return "Apply{Index:" + uitoa(v.Index) + ",Cmd:" + string(v.Cmd) + "}"
	}
	return "unknownAction"
}

func formatActions(actions []node.Action) string {
	s := "["
	for i, a := range actions {
		if i > 0 {
			s += "|"
		}
		s += formatAction(a)
	}
	return s + "]"
}

func assertActions(t *testing.T, got, want []node.Action) {
	t.Helper()
	gs, ws := formatActions(got), formatActions(want)
	if gs != ws {
		t.Fatalf("actions mismatch:\n got: %s\nwant: %s", gs, ws)
	}
}

// testErr is a minimal error implementation for tests that need to hand a
// non-nil error to PersistDone; the package has no access to the standard
// errors package.
type testErr string

func (e testErr) Error() string { return string(e) }

// makePayload deterministically derives a small payload from i, for tests
// that need varied but reproducible message bodies.
func makePayload(i int) []byte {
	return []byte{byte(i), byte(i >> 8), byte('a' + (i % 26))}
}

// ------------------------------------------------------------------ tests ---

func TestSingleNodeClientCall(t *testing.T) {
	n := New(1, []node.NodeID{1}, 50)

	got := n.Step(0, node.ClientCall{ReqID: 5, Cmd: []byte("x")})
	want := []node.Action{node.Persist{
		ID:      0,
		Records: []node.Record{{Kind: node.RecordEntry, Index: 0, Data: []byte("x")}},
		Sync:    true,
	}}
	assertActions(t, got, want)

	got = n.Step(0, node.PersistDone{ID: 0, Err: nil})
	want = []node.Action{node.Reply{ReqID: 5, Resp: []byte("x"), Err: nil}}
	assertActions(t, got, want)
}

func TestThreeNodeHappyPath(t *testing.T) {
	peers := []node.NodeID{1, 2, 3}
	n := New(1, peers, 50)

	got := n.Step(0, node.ClientCall{ReqID: 10, Cmd: []byte("a")})
	want := []node.Action{node.Persist{
		ID:      0,
		Records: []node.Record{{Kind: node.RecordEntry, Index: 0, Data: []byte("a")}},
		Sync:    true,
	}}
	assertActions(t, got, want)

	got = n.Step(0, node.PersistDone{ID: 0, Err: nil})
	want = []node.Action{
		node.Send{To: 2, Msg: echoReq{ReqID: 10, Payload: []byte("a")}},
		node.Send{To: 3, Msg: echoReq{ReqID: 10, Payload: []byte("a")}},
		node.SetTimer{Name: "retry", After: 50},
	}
	assertActions(t, got, want)

	got = n.Step(0, node.Deliver{From: 2, Msg: echoResp{ReqID: 10, Payload: []byte("a")}})
	want = []node.Action{
		node.Reply{ReqID: 10, Resp: []byte("a"), Err: nil},
		node.ClearTimer{Name: "retry"},
	}
	assertActions(t, got, want)
}

func TestFiveNodeMajority(t *testing.T) {
	peers := []node.NodeID{1, 2, 3, 4, 5}
	n := New(1, peers, 50)

	n.Step(0, node.ClientCall{ReqID: 1, Cmd: []byte("v")})
	got := n.Step(0, node.PersistDone{ID: 0, Err: nil})
	want := []node.Action{
		node.Send{To: 2, Msg: echoReq{ReqID: 1, Payload: []byte("v")}},
		node.Send{To: 3, Msg: echoReq{ReqID: 1, Payload: []byte("v")}},
		node.Send{To: 4, Msg: echoReq{ReqID: 1, Payload: []byte("v")}},
		node.Send{To: 5, Msg: echoReq{ReqID: 1, Payload: []byte("v")}},
		node.SetTimer{Name: "retry", After: 50},
	}
	assertActions(t, got, want)

	// One ack (2 of 5 including self): not yet a majority of 3.
	got = n.Step(0, node.Deliver{From: 2, Msg: echoResp{ReqID: 1, Payload: []byte("v")}})
	assertActions(t, got, nil)

	// Second ack (3 of 5): majority reached.
	got = n.Step(0, node.Deliver{From: 3, Msg: echoResp{ReqID: 1, Payload: []byte("v")}})
	want = []node.Action{
		node.Reply{ReqID: 1, Resp: []byte("v"), Err: nil},
		node.ClearTimer{Name: "retry"},
	}
	assertActions(t, got, want)
}

func TestDuplicateAckSamePeerDoesNotDoubleCount(t *testing.T) {
	peers := []node.NodeID{1, 2, 3, 4, 5}
	n := New(1, peers, 50)

	n.Step(0, node.ClientCall{ReqID: 2, Cmd: []byte("d")})
	n.Step(0, node.PersistDone{ID: 0, Err: nil})

	for i := 0; i < 3; i++ {
		got := n.Step(0, node.Deliver{From: 2, Msg: echoResp{ReqID: 2, Payload: []byte("d")}})
		assertActions(t, got, nil)
	}
}

func TestLateAckAfterCompletionProducesNothing(t *testing.T) {
	peers := []node.NodeID{1, 2, 3}
	n := New(1, peers, 50)

	n.Step(0, node.ClientCall{ReqID: 3, Cmd: []byte("e")})
	n.Step(0, node.PersistDone{ID: 0, Err: nil})

	got := n.Step(0, node.Deliver{From: 2, Msg: echoResp{ReqID: 3, Payload: []byte("e")}})
	want := []node.Action{
		node.Reply{ReqID: 3, Resp: []byte("e"), Err: nil},
		node.ClearTimer{Name: "retry"},
	}
	assertActions(t, got, want)

	// A peer that never acked, and the peer that already did, both arrive
	// late. Neither produces a second Reply.
	got = n.Step(0, node.Deliver{From: 3, Msg: echoResp{ReqID: 3, Payload: []byte("e")}})
	assertActions(t, got, nil)
	got = n.Step(0, node.Deliver{From: 2, Msg: echoResp{ReqID: 3, Payload: []byte("e")}})
	assertActions(t, got, nil)
}

// TestSharedRetryTimerAcrossOverlappingRequests is the regression test for
// B1 (docs/BUGS.md): timer names are per-node (SPEC section 3), so with two
// requests broadcasting at once there is exactly one "retry" timer serving
// both. The bug was that the first request to reach a majority cleared it
// unconditionally, stranding the other forever if its echoReq had been
// dropped. This reproduces the shape directly: finishing A while B is still
// outstanding must not clear the timer, a subsequent TimerFired must still
// resend B, and only once B itself reaches a majority — with nothing else
// outstanding — does ClearTimer appear.
func TestSharedRetryTimerAcrossOverlappingRequests(t *testing.T) {
	peers := []node.NodeID{1, 2, 3}
	n := New(1, peers, 50)

	n.Step(0, node.ClientCall{ReqID: 100, Cmd: []byte("A")})
	n.Step(0, node.PersistDone{ID: 0, Err: nil})

	n.Step(0, node.ClientCall{ReqID: 200, Cmd: []byte("B")})
	n.Step(0, node.PersistDone{ID: 1, Err: nil})

	// A reaches a majority (self + peer 2). B has not been acked by anyone
	// yet, so it is still outstanding: no ClearTimer.
	got := n.Step(0, node.Deliver{From: 2, Msg: echoResp{ReqID: 100, Payload: []byte("A")}})
	want := []node.Action{node.Reply{ReqID: 100, Resp: []byte("A"), Err: nil}}
	assertActions(t, got, want)

	// The shared timer is still armed and still serving B: firing it must
	// resend B's echoReq to both peers (neither has acked) and re-arm.
	got = n.Step(100, node.TimerFired{Name: "retry"})
	want = []node.Action{
		node.Send{To: 2, Msg: echoReq{ReqID: 200, Payload: []byte("B")}},
		node.Send{To: 3, Msg: echoReq{ReqID: 200, Payload: []byte("B")}},
		node.SetTimer{Name: "retry", After: 50},
	}
	assertActions(t, got, want)

	// B now reaches a majority too. Nothing is outstanding any more, so
	// this Reply does come with ClearTimer.
	got = n.Step(0, node.Deliver{From: 3, Msg: echoResp{ReqID: 200, Payload: []byte("B")}})
	want = []node.Action{
		node.Reply{ReqID: 200, Resp: []byte("B"), Err: nil},
		node.ClearTimer{Name: "retry"},
	}
	assertActions(t, got, want)
}

// TestPendingPersistRequestDoesNotKeepTimerArmed covers the other half of
// requestOutstanding's definition: a request that has only been called, and
// has not yet broadcast (still awaiting PersistDone), must not count as a
// reason to keep the shared retry timer armed. onTimerFired already skips
// such a request when deciding what to resend; anyOutstanding must agree,
// or completing the one broadcasting request would wrongly leave the timer
// armed forever.
func TestPendingPersistRequestDoesNotKeepTimerArmed(t *testing.T) {
	peers := []node.NodeID{1, 2, 3}
	n := New(1, peers, 50)

	n.Step(0, node.ClientCall{ReqID: 1, Cmd: []byte("A")})
	n.Step(0, node.PersistDone{ID: 0, Err: nil})

	// B is called but never persisted in this test: it sits in reqs with
	// broadcasting still false.
	got := n.Step(0, node.ClientCall{ReqID: 2, Cmd: []byte("B")})
	want := []node.Action{node.Persist{
		ID:      1,
		Records: []node.Record{{Kind: node.RecordEntry, Index: 1, Data: []byte("B")}},
		Sync:    true,
	}}
	assertActions(t, got, want)

	// A reaches a majority. B is not outstanding (it never broadcast), so
	// this must clear the timer.
	got = n.Step(0, node.Deliver{From: 2, Msg: echoResp{ReqID: 1, Payload: []byte("A")}})
	want = []node.Action{
		node.Reply{ReqID: 1, Resp: []byte("A"), Err: nil},
		node.ClearTimer{Name: "retry"},
	}
	assertActions(t, got, want)
}

func TestRetryResendsToAllUnacked(t *testing.T) {
	peers := []node.NodeID{1, 2, 3}
	n := New(1, peers, 75)

	n.Step(0, node.ClientCall{ReqID: 4, Cmd: []byte("r")})
	n.Step(0, node.PersistDone{ID: 0, Err: nil})

	got := n.Step(100, node.TimerFired{Name: "retry"})
	want := []node.Action{
		node.Send{To: 2, Msg: echoReq{ReqID: 4, Payload: []byte("r")}},
		node.Send{To: 3, Msg: echoReq{ReqID: 4, Payload: []byte("r")}},
		node.SetTimer{Name: "retry", After: 75},
	}
	assertActions(t, got, want)
}

func TestRetryResendsOnlyToUnackedPeers(t *testing.T) {
	peers := []node.NodeID{1, 2, 3, 4, 5}
	n := New(1, peers, 75)

	n.Step(0, node.ClientCall{ReqID: 5, Cmd: []byte("p")})
	n.Step(0, node.PersistDone{ID: 0, Err: nil})
	n.Step(0, node.Deliver{From: 3, Msg: echoResp{ReqID: 5, Payload: []byte("p")}})

	got := n.Step(100, node.TimerFired{Name: "retry"})
	want := []node.Action{
		node.Send{To: 2, Msg: echoReq{ReqID: 5, Payload: []byte("p")}},
		node.Send{To: 4, Msg: echoReq{ReqID: 5, Payload: []byte("p")}},
		node.Send{To: 5, Msg: echoReq{ReqID: 5, Payload: []byte("p")}},
		node.SetTimer{Name: "retry", After: 75},
	}
	assertActions(t, got, want)
}

func TestRetryWithNothingOutstandingReturnsNil(t *testing.T) {
	n := New(1, []node.NodeID{1, 2, 3}, 50)

	got := n.Step(0, node.TimerFired{Name: "retry"})
	if got != nil {
		t.Fatalf("want nil, got %s", formatActions(got))
	}
}

func TestFollowerRepliesToEchoReq(t *testing.T) {
	n := New(2, []node.NodeID{1, 2, 3}, 50)

	got := n.Step(0, node.Deliver{From: 1, Msg: echoReq{ReqID: 7, Payload: []byte("z")}})
	want := []node.Action{node.Send{To: 1, Msg: echoResp{ReqID: 7, Payload: []byte("z")}}}
	assertActions(t, got, want)
}

func TestPersistDoneErrorRepliesWithErrorAndNoSends(t *testing.T) {
	n := New(1, []node.NodeID{1, 2, 3}, 50)

	n.Step(0, node.ClientCall{ReqID: 8, Cmd: []byte("q")})
	got := n.Step(0, node.PersistDone{ID: 0, Err: testErr("disk failure")})
	want := []node.Action{node.Reply{ReqID: 8, Err: testErr("disk failure")}}
	assertActions(t, got, want)
}

func TestRestartedClearsInFlightRequests(t *testing.T) {
	n := New(1, []node.NodeID{1, 2, 3}, 50)

	n.Step(0, node.ClientCall{ReqID: 9, Cmd: []byte("m")})
	n.Step(0, node.PersistDone{ID: 0, Err: nil})

	got := n.Step(0, node.Restarted{})
	if got != nil {
		t.Fatalf("want nil, got %s", formatActions(got))
	}

	got = n.Step(0, node.Deliver{From: 2, Msg: echoResp{ReqID: 9, Payload: []byte("m")}})
	if got != nil {
		t.Fatalf("want nil after restart, got %s", formatActions(got))
	}
}

func TestRestoreSetsNextIndex(t *testing.T) {
	n := New(1, []node.NodeID{1, 2, 3}, 50)

	err := n.Restore([]node.Record{
		{Kind: node.RecordEntry, Index: 2, Data: []byte("a")},
		{Kind: node.RecordEntry, Index: 5, Data: []byte("b")},
		{Kind: node.RecordEntry, Index: 3, Data: []byte("c")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := n.Step(0, node.ClientCall{ReqID: 1, Cmd: []byte("x")})
	want := []node.Action{node.Persist{
		ID:      0,
		Records: []node.Record{{Kind: node.RecordEntry, Index: 6, Data: []byte("x")}},
		Sync:    true,
	}}
	assertActions(t, got, want)
}

func TestRestoreRejectsNonEntryRecord(t *testing.T) {
	n := New(1, []node.NodeID{1, 2, 3}, 50)

	err := n.Restore([]node.Record{{Kind: node.RecordHardState, Index: 0, Data: nil}})
	if err == nil {
		t.Fatalf("want error, got nil")
	}
}

func TestConstructorSortsScrambledPeers(t *testing.T) {
	n := New(2, []node.NodeID{5, 2, 9, 1}, 50)

	n.Step(0, node.ClientCall{ReqID: 1, Cmd: []byte("s")})
	got := n.Step(0, node.PersistDone{ID: 0, Err: nil})
	want := []node.Action{
		node.Send{To: 1, Msg: echoReq{ReqID: 1, Payload: []byte("s")}},
		node.Send{To: 5, Msg: echoReq{ReqID: 1, Payload: []byte("s")}},
		node.Send{To: 9, Msg: echoReq{ReqID: 1, Payload: []byte("s")}},
		node.SetTimer{Name: "retry", After: 50},
	}
	assertActions(t, got, want)
}

func TestConstructorPanicsOnIDNotInPeers(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("want panic, got none")
		}
	}()
	New(9, []node.NodeID{1, 2, 3}, 50)
}

func TestConstructorPanicsOnDuplicatePeers(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("want panic, got none")
		}
	}()
	New(1, []node.NodeID{1, 2, 2, 3}, 50)
}

// TestDeterminism feeds two freshly constructed, identically configured
// nodes the same 200-event sequence — a mix of every event kind, several
// concurrent ReqIDs, and out-of-order and duplicated responses — and
// asserts the two actually produce the identical sequence of actions,
// step by step. This is the property the whole package exists to model
// correctly, since T1.11's determinism test depends on it holding for
// every node, not just this one.
func TestDeterminism(t *testing.T) {
	peers := []node.NodeID{1, 2, 3}
	a := New(1, peers, 40)
	b := New(1, peers, 40)

	// Tracks, per concurrent ReqID slot (100..104), the persist ID from the
	// most recent unconsumed Persist this node issued for it. A fixed-size
	// array indexed by slot is used instead of a map so that picking "the
	// next pending slot" below is a plain, already-ordered scan — the same
	// reasoning as INV-6, applied to the test driver.
	var pendingActive [5]bool
	var pendingPersistID [5]uint64

	firstActiveSlot := func() int {
		for s := 0; s < 5; s++ {
			if pendingActive[s] {
				return s
			}
		}
		return -1
	}

	peerCycle := []node.NodeID{2, 3}

	for i := 0; i < 200; i++ {
		now := node.Time(i)
		var ev node.Event

		switch i % 8 {
		case 0:
			slot := i % 5
			ev = node.ClientCall{ReqID: uint64(100 + slot), Cmd: makePayload(i)}
		case 1:
			if slot := firstActiveSlot(); slot >= 0 {
				ev = node.PersistDone{ID: pendingPersistID[slot], Err: nil}
				pendingActive[slot] = false
			} else {
				ev = node.TimerFired{Name: "retry"}
			}
		case 2:
			from := peerCycle[i%2]
			ev = node.Deliver{From: from, Msg: echoReq{ReqID: uint64(900 + i%7), Payload: makePayload(i)}}
		case 3:
			from := peerCycle[(i/3)%2]
			ev = node.Deliver{From: from, Msg: echoResp{ReqID: uint64(100 + i%5), Payload: makePayload(i)}}
		case 4:
			ev = node.TimerFired{Name: "retry"}
		case 5:
			// Same construction as case 3: with i%5 and i%2 both cycling,
			// this reliably repeats an earlier (peer, ReqID) pair, giving
			// duplicated and out-of-order acknowledgements.
			from := peerCycle[(i/5)%2]
			ev = node.Deliver{From: from, Msg: echoResp{ReqID: uint64(100 + i%5), Payload: makePayload(i)}}
		case 6:
			if slot := firstActiveSlot(); slot >= 0 && i%16 == 6 {
				ev = node.PersistDone{ID: pendingPersistID[slot], Err: testErr("simulated failure")}
				pendingActive[slot] = false
			} else {
				ev = node.ClientCall{ReqID: uint64(100 + i%5), Cmd: makePayload(i)}
			}
		default: // 7
			if i%50 == 7 {
				ev = node.Restarted{}
			} else {
				ev = node.TimerFired{Name: "retry"}
			}
		}

		gotA := a.Step(now, ev)
		gotB := b.Step(now, ev)

		if formatActions(gotA) != formatActions(gotB) {
			t.Fatalf("step %d diverged for %#v:\n a: %s\n b: %s", i, ev, formatActions(gotA), formatActions(gotB))
		}

		if _, restarted := ev.(node.Restarted); restarted {
			pendingActive = [5]bool{}
			continue
		}
		if cc, ok := ev.(node.ClientCall); ok {
			for _, act := range gotA {
				if p, ok := act.(node.Persist); ok {
					slot := int(cc.ReqID) - 100
					pendingActive[slot] = true
					pendingPersistID[slot] = p.ID
				}
			}
		}
	}
}
