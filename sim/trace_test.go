package sim

import (
	"bytes"
	"errors"
	"testing"

	"verity/node"
)

// traceTestMsg is a minimal node.Message for building Deliver/Send events and
// actions in these tests without pulling in a real Raft or kvsm message type.
type traceTestMsg struct {
	kind string
	size int
}

func (m traceTestMsg) Kind() string { return m.kind }
func (m traceTestMsg) Size() int    { return m.size }

// traceStep is one hand-built (seq, at, id, ev, acts) tuple, paired with the
// exact line Step must produce for it.
type traceStep struct {
	seq  uint64
	at   node.Time
	id   node.NodeID
	ev   node.Event
	acts []node.Action
	want string
}

// goldenSteps is the fixed 5-line sequence used to pin the trace line format
// and, in TestTrace_GoldenHash, the hash that format folds to. It exercises
// every Event and Action kind at least once, including the no-actions and
// multi-action cases.
func goldenSteps() []traceStep {
	return []traceStep{
		{
			seq: 1, at: node.Time(1000), id: node.NodeID(1),
			ev:   node.TimerFired{Name: "election"},
			acts: nil,
			want: "1 1000 1 TimerFired -> -\n",
		},
		{
			seq: 2, at: node.Time(2500), id: node.NodeID(3),
			ev: node.Deliver{From: 2, Msg: traceTestMsg{kind: "Ping", size: 10}},
			acts: []node.Action{
				node.Send{To: 4, Msg: traceTestMsg{kind: "Pong", size: 10}},
			},
			want: "2 2500 3 Deliver -> Send\n",
		},
		{
			seq: 3, at: node.Time(3000), id: node.NodeID(5),
			ev: node.PersistDone{ID: 9},
			acts: []node.Action{
				node.Persist{ID: 1, Sync: true},
				node.SetTimer{Name: "heartbeat", After: 50},
			},
			want: "3 3000 5 PersistDone -> Persist,SetTimer\n",
		},
		{
			seq: 4, at: node.Time(4000), id: node.NodeID(7),
			ev: node.ClientCall{ReqID: 11, Cmd: []byte("cmd")},
			acts: []node.Action{
				node.Reply{ReqID: 11, Resp: []byte("ok")},
				node.Apply{Index: 2, Cmd: []byte("x")},
				node.ClearTimer{Name: "lease"},
			},
			want: "4 4000 7 ClientCall -> Reply,Apply,ClearTimer\n",
		},
		{
			seq: 5, at: node.Time(5000), id: node.NodeID(9),
			ev:   node.Restarted{},
			acts: nil,
			want: "5 5000 9 Restarted -> -\n",
		},
	}
}

// TestTrace_Format checks each hand-built tuple against its literal expected
// line text.
func TestTrace_Format(t *testing.T) {
	for _, s := range goldenSteps() {
		var buf bytes.Buffer
		tr := NewTrace(&buf)
		tr.Step(s.seq, s.at, s.id, s.ev, s.acts)
		if got := buf.String(); got != s.want {
			t.Errorf("seq %d: got %q, want %q", s.seq, got, s.want)
		}
	}
}

// TestTrace_NoActionsRendersDash checks the empty-actions case in isolation:
// the text after the arrow is a single "-", not an empty tail.
func TestTrace_NoActionsRendersDash(t *testing.T) {
	var buf bytes.Buffer
	tr := NewTrace(&buf)
	tr.Step(100, node.Time(0), node.NodeID(0), node.Restarted{}, nil)
	const want = "100 0 0 Restarted -> -\n"
	if got := buf.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestTrace_ActionOrderMatters checks that action kinds are joined with no
// space and that the order the node returned them in is part of the line and
// the hash: swapping two actions must change both.
func TestTrace_ActionOrderMatters(t *testing.T) {
	send := node.Send{To: 2, Msg: traceTestMsg{kind: "M"}}
	timer := node.SetTimer{Name: "election", After: 1}

	var bufA, bufB bytes.Buffer
	trA := NewTrace(&bufA)
	trA.Step(1, node.Time(0), node.NodeID(1), node.Restarted{}, []node.Action{send, timer})

	trB := NewTrace(&bufB)
	trB.Step(1, node.Time(0), node.NodeID(1), node.Restarted{}, []node.Action{timer, send})

	const wantA = "1 0 1 Restarted -> Send,SetTimer\n"
	const wantB = "1 0 1 Restarted -> SetTimer,Send\n"
	if bufA.String() != wantA {
		t.Errorf("A: got %q, want %q", bufA.String(), wantA)
	}
	if bufB.String() != wantB {
		t.Errorf("B: got %q, want %q", bufB.String(), wantB)
	}
	if bufA.String() == bufB.String() {
		t.Fatal("swapping action order did not change the line")
	}
	if trA.Hash() == trB.Hash() {
		t.Fatal("swapping action order did not change the hash")
	}
}

// TestTrace_HashStability records the same fixed sequence into two separate
// Traces and checks the hashes, the line counts, and the written bytes all
// agree.
func TestTrace_HashStability(t *testing.T) {
	steps := goldenSteps()

	var bufA, bufB bytes.Buffer
	trA := NewTrace(&bufA)
	trB := NewTrace(&bufB)
	for _, s := range steps {
		trA.Step(s.seq, s.at, s.id, s.ev, s.acts)
		trB.Step(s.seq, s.at, s.id, s.ev, s.acts)
	}

	if trA.Hash() != trB.Hash() {
		t.Fatalf("hashes differ: %d != %d", trA.Hash(), trB.Hash())
	}
	if trA.Lines() != trB.Lines() {
		t.Fatalf("line counts differ: %d != %d", trA.Lines(), trB.Lines())
	}
	if !bytes.Equal(bufA.Bytes(), bufB.Bytes()) {
		t.Fatalf("written bytes differ:\nA: %q\nB: %q", bufA.Bytes(), bufB.Bytes())
	}
}

// TestTrace_HashSensitivity checks that changing any single field of a Step
// call -- seq, at, id, the event kind, one action kind, or the number of
// actions -- changes the resulting hash relative to a fixed baseline.
func TestTrace_HashSensitivity(t *testing.T) {
	baseSeq := uint64(1)
	baseAt := node.Time(1000)
	baseID := node.NodeID(1)
	baseEv := node.TimerFired{Name: "x"}
	baseActs := []node.Action{node.Send{To: 2, Msg: traceTestMsg{kind: "M"}}}

	hashOf := func(seq uint64, at node.Time, id node.NodeID, ev node.Event, acts []node.Action) uint64 {
		tr := NewTrace(nil)
		tr.Step(seq, at, id, ev, acts)
		return tr.Hash()
	}

	base := hashOf(baseSeq, baseAt, baseID, baseEv, baseActs)

	cases := map[string]uint64{
		"seq":         hashOf(baseSeq+1, baseAt, baseID, baseEv, baseActs),
		"at":          hashOf(baseSeq, baseAt+1, baseID, baseEv, baseActs),
		"id":          hashOf(baseSeq, baseAt, baseID+1, baseEv, baseActs),
		"event kind":  hashOf(baseSeq, baseAt, baseID, node.Restarted{}, baseActs),
		"action kind": hashOf(baseSeq, baseAt, baseID, baseEv, []node.Action{node.SetTimer{Name: "y", After: 1}}),
		"action count": hashOf(baseSeq, baseAt, baseID, baseEv, []node.Action{
			node.Send{To: 2, Msg: traceTestMsg{kind: "M"}},
			node.SetTimer{Name: "z", After: 1},
		}),
	}

	for name, h := range cases {
		if h == base {
			t.Errorf("varying %s did not change the hash (both %d)", name, base)
		}
	}
}

// TestTrace_GoldenHash pins the hash of a fixed 5-line sequence to a literal
// value, so a later refactor of the line format or the hash cannot silently
// change what a recorded hash means without this test catching it. The
// literal was produced by running this exact sequence through the recorder
// and printing Hash(), not invented.
func TestTrace_GoldenHash(t *testing.T) {
	const wantHash uint64 = 0xcc3d9fb79cce780e

	tr := NewTrace(nil)
	for _, s := range goldenSteps() {
		tr.Step(s.seq, s.at, s.id, s.ev, s.acts)
	}
	if got := tr.Hash(); got != wantHash {
		t.Fatalf("Hash() = %#x, want %#x", got, wantHash)
	}
	if tr.Lines() != 5 {
		t.Fatalf("Lines() = %d, want 5", tr.Lines())
	}
}

// TestTrace_WriterOutput checks that the bytes written to an attached writer
// are exactly the concatenation of the expected lines, ending in a newline.
func TestTrace_WriterOutput(t *testing.T) {
	steps := goldenSteps()
	var want bytes.Buffer
	for _, s := range steps {
		want.WriteString(s.want)
	}

	var got bytes.Buffer
	tr := NewTrace(&got)
	for _, s := range steps {
		tr.Step(s.seq, s.at, s.id, s.ev, s.acts)
	}

	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatalf("written bytes:\ngot:  %q\nwant: %q", got.Bytes(), want.Bytes())
	}
	b := got.Bytes()
	if len(b) == 0 || b[len(b)-1] != '\n' {
		t.Fatalf("written bytes do not end in a newline: %q", got.Bytes())
	}
}

// errWriter always fails, to exercise Trace.Err.
type errWriter struct{ err error }

func (w errWriter) Write(p []byte) (int, error) { return 0, w.err }

// TestTrace_Err checks that Err reports the first write failure, that it
// stays non-nil across further Step calls, and that Hash keeps advancing
// regardless -- a write error must not be allowed to also corrupt or freeze
// the hash the determinism test relies on.
func TestTrace_Err(t *testing.T) {
	boom := errors.New("boom")
	tr := NewTrace(errWriter{err: boom})

	if tr.Err() != nil {
		t.Fatalf("Err() before any Step = %v, want nil", tr.Err())
	}

	h0 := tr.Hash()
	tr.Step(1, node.Time(0), node.NodeID(1), node.Restarted{}, nil)
	if tr.Err() == nil {
		t.Fatal("Err() after a failed write = nil, want non-nil")
	}
	h1 := tr.Hash()
	if h1 == h0 {
		t.Fatal("Hash() did not advance despite the write failing")
	}

	tr.Step(2, node.Time(1), node.NodeID(1), node.Restarted{}, nil)
	if tr.Err() == nil {
		t.Fatal("Err() after a second Step = nil, want it to stay non-nil")
	}
	if tr.Hash() == h1 {
		t.Fatal("Hash() did not advance on the second Step")
	}
	if tr.Lines() != 2 {
		t.Fatalf("Lines() = %d, want 2", tr.Lines())
	}
}

// TestTrace_Lines checks the line counter independent of hashing or writing.
func TestTrace_Lines(t *testing.T) {
	tr := NewTrace(nil)
	if tr.Lines() != 0 {
		t.Fatalf("Lines() on a fresh Trace = %d, want 0", tr.Lines())
	}
	for i, s := range goldenSteps() {
		tr.Step(s.seq, s.at, s.id, s.ev, s.acts)
		if want := uint64(i + 1); tr.Lines() != want {
			t.Fatalf("after %d Step calls, Lines() = %d, want %d", i+1, tr.Lines(), want)
		}
	}
}
