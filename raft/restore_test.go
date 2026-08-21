package raft

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"verity/node"
	"verity/prng"
)

// ---------------------------------------------------------------- helpers ---

// restoreSeed seeds every *prng.Rand built by newTestNode. Its value is
// arbitrary: Restore never draws from the stream, so nothing in this file
// depends on which seed is used, only that New always gets a valid one.
const restoreSeed = 0x5EED_1234

// newTestNode builds a fresh replica with the given id and full membership
// (which must include id, per New's precondition).
func newTestNode(t *testing.T, id node.NodeID, peers ...node.NodeID) *Node {
	t.Helper()
	return New(id, peers, prng.New(restoreSeed))
}

// restoreFrom calls n.Restore(recs) and fails the test immediately if it
// returns an error, so that tests exercising the happy path do not each
// repeat the same nil check.
func restoreFrom(t *testing.T, n *Node, recs ...node.Record) {
	t.Helper()
	if err := n.Restore(recs); err != nil {
		t.Fatalf("Restore: unexpected error: %v", err)
	}
}

// mkRestoreEntry returns one EntryNormal at index i, term term, carrying a
// small payload derived from both so that mismatched entries in a failure
// message are easy to tell apart.
func mkRestoreEntry(i Index, term Term) Entry {
	return Entry{Index: i, Term: term, Type: EntryNormal, Cmd: []byte{byte(i), byte(term)}}
}

// mkRestoreEntries returns entries at every index in [first, last], all of
// term term.
func mkRestoreEntries(first, last Index, term Term) []Entry {
	es := make([]Entry, 0, int(last-first+1))
	for i := first; i <= last; i++ {
		es = append(es, mkRestoreEntry(i, term))
	}
	return es
}

// toEntryRecords encodes each entry as a RecordEntry, in order, ready to hand
// to Restore.
func toEntryRecords(es []Entry) []node.Record {
	recs := make([]node.Record, len(es))
	for i, e := range es {
		recs[i] = encodeEntry(e)
	}
	return recs
}

// assertLogState checks every externally visible property of a log in one
// call: Base, BaseTerm, LastIndex, LastTerm, the entry count, and the
// content (index, term, type and command) of every entry want claims the log
// holds. want must be given in index order; wantBase/wantBaseTerm describe
// the sentinel. This is used after nearly every restore in this file, since
// a wrong Base or a leftover entry the assertion did not think to check for
// is exactly the kind of bug a partial check would miss.
func assertLogState(t *testing.T, l *Log, wantBase Index, wantBaseTerm Term, want []Entry) {
	t.Helper()

	if got := l.Base(); got != wantBase {
		t.Fatalf("log Base() = %d, want %d", got, wantBase)
	}
	if got := l.BaseTerm(); got != wantBaseTerm {
		t.Fatalf("log BaseTerm() = %d, want %d", got, wantBaseTerm)
	}

	wantLastIndex, wantLastTerm := wantBase, wantBaseTerm
	if len(want) > 0 {
		last := want[len(want)-1]
		wantLastIndex, wantLastTerm = last.Index, last.Term
	}
	if got := l.LastIndex(); got != wantLastIndex {
		t.Fatalf("log LastIndex() = %d, want %d", got, wantLastIndex)
	}
	if got := l.LastTerm(); got != wantLastTerm {
		t.Fatalf("log LastTerm() = %d, want %d", got, wantLastTerm)
	}
	if got := l.Len(); got != len(want) {
		t.Fatalf("log Len() = %d, want %d", got, len(want))
	}

	for _, e := range want {
		got, ok := l.At(e.Index)
		if !ok {
			t.Fatalf("log At(%d): ok = false, want true", e.Index)
		}
		if got.Term != e.Term {
			t.Fatalf("log entry %d: term = %d, want %d", e.Index, got.Term, e.Term)
		}
		if got.Type != e.Type {
			t.Fatalf("log entry %d: type = %s, want %s", e.Index, got.Type, e.Type)
		}
		if !bytes.Equal(got.Cmd, e.Cmd) {
			t.Fatalf("log entry %d: cmd = %v, want %v", e.Index, got.Cmd, e.Cmd)
		}
	}
}

// ------------------------------------------------------------- New / zero ---

// TestNewNodeStartsAtZeroValue checks the state of a freshly constructed
// replica, before Restore or Step has touched it: Follower, term 0, no vote
// cast, nothing committed, an empty log, and no recovered snapshot.
func TestNewNodeStartsAtZeroValue(t *testing.T) {
	n := newTestNode(t, 2, 1, 2, 3)

	if got := n.ID(); got != 2 {
		t.Fatalf("ID() = %d, want 2", got)
	}
	if n.role != Follower {
		t.Fatalf("role = %s, want %s", n.role, Follower)
	}
	if got := n.hard.Term; got != 0 {
		t.Fatalf("hard.Term = %d, want 0", got)
	}
	if got := n.hard.VotedFor; got != None {
		t.Fatalf("hard.VotedFor = %d, want None", got)
	}
	if got := n.hard.CommitIndex; got != 0 {
		t.Fatalf("hard.CommitIndex = %d, want 0", got)
	}
	if got := n.log.Base(); got != 0 {
		t.Fatalf("log.Base() = %d, want 0", got)
	}
	if got := n.log.LastIndex(); got != 0 {
		t.Fatalf("log.LastIndex() = %d, want 0", got)
	}
	if _, ok := n.Snapshot(); ok {
		t.Fatal("Snapshot() ok = true, want false on a fresh node")
	}
}

func TestNewPanicsOnZeroID(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with id = None: want panic, got none")
		}
	}()
	New(None, []node.NodeID{1, 2, 3}, prng.New(restoreSeed))
}

func TestNewPanicsOnNilRand(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with a nil rng: want panic, got none")
		}
	}()
	New(1, []node.NodeID{1, 2, 3}, nil)
}

func TestNewPanicsOnDuplicatePeers(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with duplicate peers: want panic, got none")
		}
	}()
	New(1, []node.NodeID{1, 2, 2, 3}, prng.New(restoreSeed))
}

func TestNewPanicsOnPeersMissingID(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with id absent from peers: want panic, got none")
		}
	}()
	New(9, []node.NodeID{1, 2, 3}, prng.New(restoreSeed))
}

// TestNewCopiesAndSortsPeers checks New's second contract on peers, beyond
// the panics: the stored slice is sorted ascending regardless of the order
// it was handed in, and it is a copy, not an alias — mutating the caller's
// slice afterwards must not be visible to the node. Without the copy, a
// caller reusing a peers slice across several New calls (as the simulator
// does when building a cluster) could silently corrupt every node's
// membership at once.
func TestNewCopiesAndSortsPeers(t *testing.T) {
	orig := []node.NodeID{9, 5, 1, 7}
	n := newTestNode(t, 5, orig...)

	want := []node.NodeID{1, 5, 7, 9}
	if !reflect.DeepEqual(n.peers, want) {
		t.Fatalf("peers = %v, want %v (sorted)", n.peers, want)
	}

	orig[0] = 999
	if !reflect.DeepEqual(n.peers, want) {
		t.Fatalf("peers after mutating caller's slice = %v, want %v (unaffected)", n.peers, want)
	}
}

// -------------------------------------------------------- hard state ---

func TestRestoreHardStateSingleRecord(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)
	h := HardState{Term: 5, VotedFor: 2, CommitIndex: 7}

	restoreFrom(t, n, encodeHardState(h))

	if !n.hard.Equal(h) {
		t.Fatalf("hard = %+v, want %+v", n.hard, h)
	}
}

// TestRestoreHardStateLastWins checks that when several RecordHardState
// records appear in one restore stream, replaying them in write order lets
// the last one supersede the earlier ones — exactly as SPEC section 6.2 says
// a later hard state does at runtime.
func TestRestoreHardStateLastWins(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)
	h1 := HardState{Term: 1, VotedFor: 1, CommitIndex: 0}
	h2 := HardState{Term: 3, VotedFor: 2, CommitIndex: 1}
	h3 := HardState{Term: 5, VotedFor: 3, CommitIndex: 2}

	restoreFrom(t, n, encodeHardState(h1), encodeHardState(h2), encodeHardState(h3))

	if !n.hard.Equal(h3) {
		t.Fatalf("hard = %+v, want last record %+v", n.hard, h3)
	}
}

// TestRestoreDurableEqualsHardState is the INV-8 test: whatever Restore
// leaves in n.hard must equal what it leaves in n.persist.durable, because
// nothing else in this incarnation has been written yet, so the recovered
// hard state is the whole of what this node can prove. It also checks that
// a fresh incarnation's persister starts with nextID and durableID at zero,
// which is what makes the first real write's ID (1) unambiguous.
func TestRestoreDurableEqualsHardState(t *testing.T) {
	cases := []struct {
		name string
		recs []node.Record
	}{
		{"no records", nil},
		{"hard-state-only", []node.Record{
			encodeHardState(HardState{Term: 9, VotedFor: 3, CommitIndex: 4}),
		}},
		{"mixed stream", []node.Record{
			encodeHardState(HardState{Term: 2, VotedFor: 1, CommitIndex: 1}),
			encodeEntry(mkRestoreEntry(1, 1)),
			encodeEntry(mkRestoreEntry(2, 1)),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newTestNode(t, 1, 1, 2, 3)
			restoreFrom(t, n, tc.recs...)

			if !n.persist.durable.Equal(n.hard) {
				t.Fatalf("persist.durable = %+v, want equal to n.hard %+v (INV-8)", n.persist.durable, n.hard)
			}
			if got := n.persist.nextID; got != 0 {
				t.Fatalf("persist.nextID = %d, want 0", got)
			}
			if got := n.persist.durableID; got != 0 {
				t.Fatalf("persist.durableID = %d, want 0", got)
			}
		})
	}
}

// ------------------------------------------------------------ entries ---

func TestRestoreEntriesContiguousRebuildsLog(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)
	want := mkRestoreEntries(1, 10, 1)

	restoreFrom(t, n, toEntryRecords(want)...)

	assertLogState(t, n.log, 0, 0, want)
}

// TestRestoreEntryRewriteTruncatesDiscardedTail is the most important
// restore case. Entries 6..8 with a different term are what a leader's
// correction looked like when it was originally appended: the follower's
// old entries at those indices were truncated and replaced, and entries 9
// and 10 above them were discarded at the same time. Replaying the stream
// must reproduce exactly that, truncating before applying the correction —
// appending the correction on top of the original entries instead would
// leave the discarded history in place and silently resurrect it.
func TestRestoreEntryRewriteTruncatesDiscardedTail(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)

	var recs []node.Record
	recs = append(recs, toEntryRecords(mkRestoreEntries(1, 10, 1))...)
	recs = append(recs, toEntryRecords(mkRestoreEntries(6, 8, 2))...)

	restoreFrom(t, n, recs...)

	want := append(mkRestoreEntries(1, 5, 1), mkRestoreEntries(6, 8, 2)...)
	assertLogState(t, n.log, 0, 0, want)
}

// TestRestoreEntryRewriteOfLastEntryOnly checks the smallest possible
// rewrite: a correction that touches only the entry that was, at the time,
// the tail of the log. Nothing above it needs discarding, but the
// truncate-then-append path must still run rather than a plain append,
// which would leave two entries at index 5.
func TestRestoreEntryRewriteOfLastEntryOnly(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)

	var recs []node.Record
	recs = append(recs, toEntryRecords(mkRestoreEntries(1, 5, 1))...)
	recs = append(recs, encodeEntry(mkRestoreEntry(5, 2)))

	restoreFrom(t, n, recs...)

	want := append(mkRestoreEntries(1, 4, 1), mkRestoreEntry(5, 2))
	assertLogState(t, n.log, 0, 0, want)
}

// TestRestoreEntryRewriteFromIndexOneTruncatesWholeLog checks the other
// extreme: a correction starting at the very first index discards
// everything the log held and rebuilds from scratch.
func TestRestoreEntryRewriteFromIndexOneTruncatesWholeLog(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)

	var recs []node.Record
	recs = append(recs, toEntryRecords(mkRestoreEntries(1, 5, 1))...)
	recs = append(recs, toEntryRecords(mkRestoreEntries(1, 3, 2))...)

	restoreFrom(t, n, recs...)

	want := mkRestoreEntries(1, 3, 2)
	assertLogState(t, n.log, 0, 0, want)
}

// TestRestoreEntryGapReturnsErrNonContiguous checks that a stream jumping
// over an index is reported as corruption rather than silently accepted or
// skipped. This cannot be a torn write: the framing layer (internal/frame)
// already stops at the first record whose checksum fails and hands Restore
// only the clean prefix before it, so every record reaching here passed its
// checksum. A gap in that clean prefix means the bytes on disk are wrong,
// not merely incomplete, and restoreEntry's third case (raft.go) reports it
// rather than guessing at what belongs in the hole.
func TestRestoreEntryGapReturnsErrNonContiguous(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)

	var recs []node.Record
	recs = append(recs, toEntryRecords(mkRestoreEntries(1, 3, 1))...)
	recs = append(recs, encodeEntry(mkRestoreEntry(7, 1)))

	err := n.Restore(recs)
	if err == nil {
		t.Fatal("Restore with a gap: want error, got nil")
	}
	if !errors.Is(err, ErrNonContiguous) {
		t.Fatalf("Restore with a gap: err = %v, want wraps ErrNonContiguous", err)
	}
}

// ----------------------------------------------------------- snapshots ---

func TestRestoreSnapshotAloneSetsBaseAndSnapshot(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)
	snap := Snapshot{
		LastIndex: 50,
		LastTerm:  4,
		Config:    []node.NodeID{1, 2, 3}, // pre-sorted: encodeSnapshot sorts a copy
		Data:      []byte("state machine bytes"),
	}

	restoreFrom(t, n, encodeSnapshot(snap))

	if got := n.log.Base(); got != 50 {
		t.Fatalf("log.Base() = %d, want 50", got)
	}
	if got := n.log.BaseTerm(); got != 4 {
		t.Fatalf("log.BaseTerm() = %d, want 4", got)
	}
	if term, ok := n.log.TermAt(50); !ok || term != 4 {
		t.Fatalf("log.TermAt(50) = (%d, %v), want (4, true)", term, ok)
	}

	got, ok := n.Snapshot()
	if !ok {
		t.Fatal("Snapshot() ok = false, want true")
	}
	if got.LastIndex != snap.LastIndex || got.LastTerm != snap.LastTerm {
		t.Fatalf("Snapshot() = %+v, want LastIndex/LastTerm %d/%d", got, snap.LastIndex, snap.LastTerm)
	}
	if !reflect.DeepEqual(got.Config, snap.Config) {
		t.Fatalf("Snapshot().Config = %v, want %v", got.Config, snap.Config)
	}
	if !bytes.Equal(got.Data, snap.Data) {
		t.Fatalf("Snapshot().Data = %q, want %q", got.Data, snap.Data)
	}
}

// TestRestoreSnapshotAfterEntriesCompactsToTail models a snapshot taken
// during normal operation, whose record was written to the log after the
// entries it summarises: entries 1..100 arrive first, then a snapshot at
// index 50 compacts everything up to and including it away, leaving only
// the tail.
func TestRestoreSnapshotAfterEntriesCompactsToTail(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)

	var recs []node.Record
	recs = append(recs, toEntryRecords(mkRestoreEntries(1, 100, 1))...)
	recs = append(recs, encodeSnapshot(Snapshot{LastIndex: 50, LastTerm: 1}))

	restoreFrom(t, n, recs...)

	want := mkRestoreEntries(51, 100, 1)
	assertLogState(t, n.log, 50, 1, want)

	snap, ok := n.Snapshot()
	if !ok || snap.LastIndex != 50 || snap.LastTerm != 1 {
		t.Fatalf("Snapshot() = (%+v, %v), want ({LastIndex:50 LastTerm:1 ...}, true)", snap, ok)
	}
}

// TestRestoreSnapshotBeforeEntriesThenAppends models the other order: the
// snapshot's record was written before the entries following it, which is
// what a follower that installed a snapshot ahead of its log and then kept
// replicating looks like on disk.
func TestRestoreSnapshotBeforeEntriesThenAppends(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)

	recs := []node.Record{encodeSnapshot(Snapshot{LastIndex: 50, LastTerm: 1})}
	recs = append(recs, toEntryRecords(mkRestoreEntries(51, 60, 1))...)

	restoreFrom(t, n, recs...)

	want := mkRestoreEntries(51, 60, 1)
	assertLogState(t, n.log, 50, 1, want)
}

// TestRestoreEntryAtOrBelowBaseAfterSnapshotIsIgnored checks restoreEntry's
// first case: an entry record whose index the snapshot has already absorbed
// is not an error, it is simply redundant, and replaying it must leave the
// log exactly as the snapshot left it — neither erroring nor, worse,
// resurrecting an entry the snapshot deliberately discarded. Both the
// boundary (index == Base) and strictly below it are covered.
func TestRestoreEntryAtOrBelowBaseAfterSnapshotIsIgnored(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)

	recs := []node.Record{
		encodeSnapshot(Snapshot{LastIndex: 50, LastTerm: 1}),
		encodeEntry(mkRestoreEntry(50, 1)), // == Base
		encodeEntry(mkRestoreEntry(30, 1)), // < Base
	}

	restoreFrom(t, n, recs...)

	assertLogState(t, n.log, 50, 1, nil)
}

// TestRestoreSnapshotTermConflictWithHeldEntryFails checks that a snapshot
// whose term at an index the log still holds disagrees with what the log
// already recorded there is rejected outright. The two records describe
// different histories, and only one of them can be right; Compact reports
// ErrTermMismatch rather than silently trusting the newer-looking record.
func TestRestoreSnapshotTermConflictWithHeldEntryFails(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)

	var recs []node.Record
	recs = append(recs, toEntryRecords(mkRestoreEntries(1, 10, 1))...)
	recs = append(recs, encodeSnapshot(Snapshot{LastIndex: 5, LastTerm: 99}))

	err := n.Restore(recs)
	if err == nil {
		t.Fatal("Restore with conflicting snapshot term: want error, got nil")
	}
	if !errors.Is(err, ErrTermMismatch) {
		t.Fatalf("Restore with conflicting snapshot term: err = %v, want wraps ErrTermMismatch", err)
	}
}

// ------------------------------------------------------------- malformed ---

func TestRestoreUnknownRecordKindFails(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)
	rec := node.Record{Kind: node.RecordKind(9), Index: 0, Data: nil}

	err := n.Restore([]node.Record{rec})
	if err == nil {
		t.Fatal("Restore with an unknown record kind: want error, got nil")
	}
	if !errors.Is(err, ErrBadRecordKind) {
		t.Fatalf("Restore with an unknown record kind: err = %v, want wraps ErrBadRecordKind", err)
	}
}

// TestRestoreMalformedHardStatePayloadNamesRecordIndex checks that a
// truncated payload is reported as ErrShortRecord, and — separately — that
// the error names which record in the stream failed. A corrupt log with no
// indication of where the corruption is would be undiagnosable in practice;
// the record's position in recs is the only coordinate Restore has, so it
// must appear in the message (see raft.go's "record %d" wrapping).
func TestRestoreMalformedHardStatePayloadNamesRecordIndex(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)
	bad := node.Record{Kind: node.RecordHardState, Index: 0, Data: []byte{1, 2, 3}} // needs 24 bytes

	recs := []node.Record{
		encodeHardState(HardState{Term: 1}), // record 0: fine
		bad,                                 // record 1: short
	}

	err := n.Restore(recs)
	if err == nil {
		t.Fatal("Restore with a short hard state payload: want error, got nil")
	}
	if !errors.Is(err, ErrShortRecord) {
		t.Fatalf("Restore with a short hard state payload: err = %v, want wraps ErrShortRecord", err)
	}
	if !strings.Contains(err.Error(), "record 1") {
		t.Fatalf("Restore error %q does not name the failing record's position (record 1)", err.Error())
	}
}

// ---------------------------------------------------- restore then step ---

// TestRestoredHardStateRejectsStaleVoteRequest is a light integration check
// that a restored hard state actually governs the very next decision, not
// merely a static field: after restoring term 5, a RequestVote at term 3 is
// stale and is rejected immediately with the higher term, exactly as it
// would be had the node reached term 5 through ordinary operation rather
// than a restart. Detailed R1/R3 behaviour belongs to vote_test.go; this
// only proves Restore's output is what Step consults.
func TestRestoredHardStateRejectsStaleVoteRequest(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)
	restoreFrom(t, n, encodeHardState(HardState{Term: 5, VotedFor: None, CommitIndex: 0}))

	got := n.Step(0, node.Deliver{From: 2, Msg: RequestVote{Term: 3, CandidateID: 2}})
	want := []node.Action{node.Send{To: 2, Msg: RequestVoteResp{Term: 5, VoteGranted: false}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("actions = %#v, want %#v", got, want)
	}
}

// TestRestoredLogDrivesElectionRestrictionOnNextVote is the log-side
// counterpart: a log rebuilt by Restore, ending at term 1, is what the very
// next RequestVote is checked against. A candidate offering an empty log is
// behind it and is refused under R3, with the response answered immediately
// since nothing about the hard state changed as a result.
func TestRestoredLogDrivesElectionRestrictionOnNextVote(t *testing.T) {
	n := newTestNode(t, 1, 1, 2, 3)
	restoreFrom(t, n, toEntryRecords(mkRestoreEntries(1, 5, 1))...)

	got := n.Step(0, node.Deliver{From: 2, Msg: RequestVote{Term: 0, CandidateID: 2, LastLogIndex: 0, LastLogTerm: 0}})
	want := []node.Action{node.Send{To: 2, Msg: RequestVoteResp{Term: 0, VoteGranted: false}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("actions = %#v, want %#v", got, want)
	}
}
