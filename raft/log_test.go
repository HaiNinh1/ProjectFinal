package raft

import (
	"errors"
	"reflect"
	"testing"

	"verity/prng"
)

// mkEntries returns n entries with consecutive indices starting at first, all
// carrying term term and type EntryNormal. Cmd is a small deterministic
// payload so appended entries can be told apart in failure output.
func mkEntries(first Index, term Term, n int) []Entry {
	es := make([]Entry, n)
	for i := 0; i < n; i++ {
		es[i] = Entry{Index: first + Index(i), Term: term, Type: EntryNormal, Cmd: []byte{byte(i)}}
	}
	return es
}

// mustAppend appends es to l and fails the test if Append returns an error.
func mustAppend(t *testing.T, l *Log, es ...Entry) {
	t.Helper()
	if err := l.Append(es...); err != nil {
		t.Fatalf("Append: unexpected error: %v", err)
	}
}

// snapshot returns a copy of l's internal entries, sentinel included, for
// comparison against a later snapshot to confirm an operation left the log
// completely unmodified.
func snapshot(l *Log) []Entry {
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

func TestNewLogIsEmpty(t *testing.T) {
	l := NewLog()
	if got := l.Base(); got != 0 {
		t.Errorf("Base() = %d, want 0", got)
	}
	if got := l.BaseTerm(); got != 0 {
		t.Errorf("BaseTerm() = %d, want 0", got)
	}
	if got := l.FirstIndex(); got != 1 {
		t.Errorf("FirstIndex() = %d, want 1", got)
	}
	if got := l.LastIndex(); got != 0 {
		t.Errorf("LastIndex() = %d, want 0", got)
	}
	if got := l.LastTerm(); got != 0 {
		t.Errorf("LastTerm() = %d, want 0", got)
	}
	if got := l.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}

// TestAtFailsAtSentinelButTermAtSucceeds checks the deliberate asymmetry
// between At and TermAt at index 0: the sentinel carries a term but no
// entry, so At must fail there while TermAt succeeds. The AppendEntries
// consistency check (R4, docs/SPEC.md §6.3) needs the term of the entry
// immediately before the one it is appending, and that entry is often the
// sentinel itself — treating it as absent would turn the very first append,
// and every append just after a snapshot, into a special case.
func TestAtFailsAtSentinelButTermAtSucceeds(t *testing.T) {
	l := NewLog()

	if _, ok := l.At(0); ok {
		t.Fatal("At(0) succeeded, want ok=false at the sentinel")
	}
	term, ok := l.TermAt(0)
	if !ok {
		t.Fatal("TermAt(0) failed, want ok=true at the sentinel")
	}
	if term != 0 {
		t.Fatalf("TermAt(0) = %d, want 0", term)
	}
}

func TestAppendExtendsLog(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 3)...)

	if got := l.LastIndex(); got != 3 {
		t.Fatalf("LastIndex() = %d, want 3", got)
	}
	if got := l.LastTerm(); got != 1 {
		t.Fatalf("LastTerm() = %d, want 1", got)
	}
	if got := l.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}

	mustAppend(t, l, mkEntries(4, 2, 2)...)
	if got := l.LastIndex(); got != 5 {
		t.Fatalf("LastIndex() after second append = %d, want 5", got)
	}
	if got := l.LastTerm(); got != 2 {
		t.Fatalf("LastTerm() after second append = %d, want 2", got)
	}
}

func TestAppendZeroEntriesIsNoop(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 2)...)
	before := snapshot(l)

	if err := l.Append(); err != nil {
		t.Fatalf("Append() with no entries: unexpected error: %v", err)
	}
	after := snapshot(l)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("Append() with no entries changed the log: got %+v, want %+v", after, before)
	}
}

// TestAppendRejectsNonContiguousBatches checks that Append refuses every way
// a batch can fail to be a clean extension of the log — a gap above
// LastIndex, a re-append of an index already held, a batch that is
// internally contiguous but starts at the wrong place, and a batch that
// starts correctly but opens a gap partway through — and that a rejected
// batch never partially applies: the log is byte-for-byte unchanged
// afterwards.
func TestAppendRejectsNonContiguousBatches(t *testing.T) {
	cases := []struct {
		name string
		bad  []Entry
	}{
		{"gap above LastIndex", mkEntries(4, 1, 1)},
		{"reappend of existing index", mkEntries(2, 1, 1)},
		{"batch contiguous internally but starts wrong", mkEntries(5, 1, 3)},
		{"batch starts correctly but has internal gap", append(mkEntries(3, 1, 2), Entry{Index: 6, Term: 1, Type: EntryNormal})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLog()
			mustAppend(t, l, mkEntries(1, 1, 2)...)
			before := snapshot(l)

			err := l.Append(tc.bad...)
			if err == nil {
				t.Fatalf("Append(%+v): got nil error, want ErrNonContiguous", tc.bad)
			}
			if !errors.Is(err, ErrNonContiguous) {
				t.Fatalf("Append(%+v): err = %v, want wraps ErrNonContiguous", tc.bad, err)
			}

			after := snapshot(l)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("Append(%+v): log modified by rejected append: got %+v, want %+v", tc.bad, after, before)
			}
		})
	}
}

// TestAtTermAtAcrossValidRange checks At and TermAt agree with what was
// appended for every real index, and both report ok=false past LastIndex.
func TestAtTermAtAcrossValidRange(t *testing.T) {
	l := NewLog()
	want := mkEntries(1, 1, 3)
	want = append(want, mkEntries(4, 2, 2)...)
	mustAppend(t, l, want...)

	for _, e := range want {
		got, ok := l.At(e.Index)
		if !ok {
			t.Fatalf("At(%d): ok = false, want true", e.Index)
		}
		if !reflect.DeepEqual(got, e) {
			t.Fatalf("At(%d) = %+v, want %+v", e.Index, got, e)
		}
		term, ok := l.TermAt(e.Index)
		if !ok {
			t.Fatalf("TermAt(%d): ok = false, want true", e.Index)
		}
		if term != e.Term {
			t.Fatalf("TermAt(%d) = %d, want %d", e.Index, term, e.Term)
		}
	}

	if _, ok := l.At(l.LastIndex() + 1); ok {
		t.Fatalf("At(%d) past LastIndex: ok = true, want false", l.LastIndex()+1)
	}
	if _, ok := l.TermAt(l.LastIndex() + 1); ok {
		t.Fatalf("TermAt(%d) past LastIndex: ok = true, want false", l.LastIndex()+1)
	}
}

// TestAtTermAtBelowBaseAfterCompaction checks both accessors report
// ok=false for an index a compaction has discarded.
func TestAtTermAtBelowBaseAfterCompaction(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 5)...)
	if err := l.Compact(3, 1); err != nil {
		t.Fatalf("Compact: unexpected error: %v", err)
	}

	if _, ok := l.At(2); ok {
		t.Fatal("At(2) below Base: ok = true, want false")
	}
	if _, ok := l.TermAt(2); ok {
		t.Fatal("TermAt(2) below Base: ok = true, want false")
	}
}

// TestMatches exercises R4's consistency check: it must agree at Base (the
// sentinel, which has a term but no entry), agree at a real entry, disagree
// on a term mismatch, and return false for an index the log does not hold.
func TestMatches(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 2)...)
	mustAppend(t, l, mkEntries(3, 2, 1)...)

	cases := []struct {
		name string
		i    Index
		term Term
		want bool
	}{
		{"agrees at Base", 0, 0, true},
		{"agrees at a real entry", 2, 1, true},
		{"disagrees on term mismatch", 2, 2, false},
		{"absent index past LastIndex", 10, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := l.Matches(tc.i, tc.term); got != tc.want {
				t.Errorf("Matches(%d, %d) = %v, want %v", tc.i, tc.term, got, tc.want)
			}
		})
	}
}

func TestSliceHalfOpenAndEmpty(t *testing.T) {
	l := NewLog()
	es := mkEntries(1, 1, 5)
	mustAppend(t, l, es...)

	got := l.Slice(2, 4)
	want := es[1:3]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Slice(2, 4) = %+v, want %+v", got, want)
	}

	if got := l.Slice(3, 3); got != nil {
		t.Fatalf("Slice(3, 3) = %+v, want nil (lo == hi)", got)
	}
	if got := l.Slice(4, 2); got != nil {
		t.Fatalf("Slice(4, 2) = %+v, want nil (lo > hi)", got)
	}
}

// TestSliceClampsToLogBounds checks that an over-wide request returns what
// the log holds rather than erroring: lo is clamped up to FirstIndex and hi
// down to LastIndex+1.
func TestSliceClampsToLogBounds(t *testing.T) {
	l := NewLog()
	es := mkEntries(1, 1, 3)
	mustAppend(t, l, es...)

	got := l.Slice(0, 100)
	if !reflect.DeepEqual(got, es) {
		t.Fatalf("Slice(0, 100) = %+v, want %+v", got, es)
	}

	if err := l.Compact(1, 1); err != nil {
		t.Fatalf("Compact(1, 1): unexpected error: %v", err)
	}
	// FirstIndex is now 2; a request from 0 must clamp up to it.
	got2 := l.Slice(0, 100)
	want2 := es[1:]
	if !reflect.DeepEqual(got2, want2) {
		t.Fatalf("Slice(0, 100) after compact = %+v, want %+v", got2, want2)
	}
}

// TestSliceReturnsCopy checks the property Slice's doc comment calls out
// explicitly: the returned entries are a copy, not a view into the log's
// backing array. A leader slices its log to build an AppendEntries message
// and may append or truncate before that message is delivered; if Slice
// aliased the backing array, a later mutation would silently corrupt a
// message already in flight.
func TestSliceReturnsCopy(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 3)...)

	got := l.Slice(1, 4)
	want := make([]Entry, len(got))
	copy(want, got)

	mustAppend(t, l, mkEntries(4, 1, 3)...)
	if err := l.TruncateFrom(2); err != nil {
		t.Fatalf("TruncateFrom: unexpected error: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("slice mutated by later append/truncate: got %+v, want %+v", got, want)
	}
}

func TestFromReturnsTail(t *testing.T) {
	l := NewLog()
	es := mkEntries(1, 1, 5)
	mustAppend(t, l, es...)

	got := l.From(3)
	want := es[2:]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("From(3) = %+v, want %+v", got, want)
	}
}

func TestTruncateFromDropsTailAndAllowsContinuedAppend(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 5)...)

	if err := l.TruncateFrom(3); err != nil {
		t.Fatalf("TruncateFrom(3): unexpected error: %v", err)
	}
	if got := l.LastIndex(); got != 2 {
		t.Fatalf("LastIndex() after TruncateFrom(3) = %d, want 2", got)
	}
	if got := l.LastTerm(); got != 1 {
		t.Fatalf("LastTerm() after TruncateFrom(3) = %d, want 1", got)
	}

	mustAppend(t, l, mkEntries(3, 2, 2)...)
	if got := l.LastIndex(); got != 4 {
		t.Fatalf("LastIndex() after continued append = %d, want 4", got)
	}
	if got := l.LastTerm(); got != 2 {
		t.Fatalf("LastTerm() after continued append = %d, want 2", got)
	}
}

func TestTruncateFromAtFirstIndexEmptiesLog(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 3)...)

	if err := l.TruncateFrom(l.FirstIndex()); err != nil {
		t.Fatalf("TruncateFrom(FirstIndex): unexpected error: %v", err)
	}
	if got := l.Len(); got != 0 {
		t.Fatalf("Len() after truncating to FirstIndex = %d, want 0", got)
	}
	if got := l.LastIndex(); got != l.Base() {
		t.Fatalf("LastIndex() = %d, want Base() (%d)", got, l.Base())
	}
}

// TestTruncateFromRejectsAtOrBelowBase checks TruncateFrom refuses to
// truncate at Base itself, and at an index a compaction has since
// discarded, wrapping ErrCompacted in both cases and leaving the log
// unmodified.
func TestTruncateFromRejectsAtOrBelowBase(t *testing.T) {
	t.Run("at Base", func(t *testing.T) {
		l := NewLog()
		mustAppend(t, l, mkEntries(1, 1, 3)...)
		before := snapshot(l)

		if err := l.TruncateFrom(0); !errors.Is(err, ErrCompacted) {
			t.Fatalf("TruncateFrom(0): err = %v, want wraps ErrCompacted", err)
		}
		if after := snapshot(l); !reflect.DeepEqual(before, after) {
			t.Fatalf("TruncateFrom(0): log modified by rejected truncate: got %+v, want %+v", after, before)
		}
	})

	t.Run("below Base after compaction", func(t *testing.T) {
		l := NewLog()
		mustAppend(t, l, mkEntries(1, 1, 5)...)
		if err := l.Compact(3, 1); err != nil {
			t.Fatalf("Compact: unexpected error: %v", err)
		}
		before := snapshot(l)

		if err := l.TruncateFrom(2); !errors.Is(err, ErrCompacted) {
			t.Fatalf("TruncateFrom(2): err = %v, want wraps ErrCompacted", err)
		}
		if after := snapshot(l); !reflect.DeepEqual(before, after) {
			t.Fatalf("TruncateFrom(2): log modified by rejected truncate: got %+v, want %+v", after, before)
		}
	})
}

func TestTruncateFromRejectsPastLastIndex(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 3)...)
	before := snapshot(l)

	if err := l.TruncateFrom(l.LastIndex() + 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("TruncateFrom(LastIndex+1): err = %v, want wraps ErrUnavailable", err)
	}
	if after := snapshot(l); !reflect.DeepEqual(before, after) {
		t.Fatalf("TruncateFrom(LastIndex+1): log modified by rejected truncate: got %+v, want %+v", after, before)
	}
}

func TestCompactMovesSentinelPreservingSurvivingEntries(t *testing.T) {
	l := NewLog()
	es := mkEntries(1, 1, 5)
	mustAppend(t, l, es...)

	if err := l.Compact(2, 1); err != nil {
		t.Fatalf("Compact(2, 1): unexpected error: %v", err)
	}
	if got := l.Base(); got != 2 {
		t.Fatalf("Base() = %d, want 2", got)
	}
	if got := l.BaseTerm(); got != 1 {
		t.Fatalf("BaseTerm() = %d, want 1", got)
	}
	if got := l.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}

	for _, e := range es[2:] { // indices 3, 4, 5 survive
		got, ok := l.At(e.Index)
		if !ok {
			t.Fatalf("At(%d): ok = false, want true", e.Index)
		}
		if !reflect.DeepEqual(got, e) {
			t.Fatalf("At(%d) = %+v, want %+v", e.Index, got, e)
		}
	}

	term, ok := l.TermAt(l.Base())
	if !ok || term != 1 {
		t.Fatalf("TermAt(Base()) = (%d, %v), want (1, true)", term, ok)
	}
}

func TestCompactToLastIndexEmptiesLog(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 3)...)

	if err := l.Compact(3, 1); err != nil {
		t.Fatalf("Compact(3, 1): unexpected error: %v", err)
	}
	if got := l.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
	if got := l.Base(); got != 3 {
		t.Fatalf("Base() = %d, want 3", got)
	}
	if got := l.BaseTerm(); got != 1 {
		t.Fatalf("BaseTerm() = %d, want 1", got)
	}
	if got := l.LastIndex(); got != 3 {
		t.Fatalf("LastIndex() = %d, want 3", got)
	}
}

// TestCompactPastLastIndexEmptiesLog checks that compacting beyond
// LastIndex is accepted, not rejected: a follower installing a snapshot
// from ahead of its own log must discard everything it has.
func TestCompactPastLastIndexEmptiesLog(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 3)...)

	if err := l.Compact(10, 5); err != nil {
		t.Fatalf("Compact(10, 5): unexpected error: %v", err)
	}
	if got := l.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
	if got := l.Base(); got != 10 {
		t.Fatalf("Base() = %d, want 10", got)
	}
	if got := l.BaseTerm(); got != 5 {
		t.Fatalf("BaseTerm() = %d, want 5", got)
	}
}

func TestCompactRejectsMismatchedTermAtHeldIndex(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 5)...)
	before := snapshot(l)

	err := l.Compact(3, 99)
	if err == nil {
		t.Fatal("Compact(3, 99) with mismatched term: got nil error, want an error")
	}
	after := snapshot(l)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("Compact(3, 99): log modified by rejected compact: got %+v, want %+v", after, before)
	}
}

func TestCompactRejectsBackwardsCompaction(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 5)...)
	if err := l.Compact(3, 1); err != nil {
		t.Fatalf("Compact(3, 1): unexpected error: %v", err)
	}
	before := snapshot(l)

	if err := l.Compact(1, 1); !errors.Is(err, ErrCompacted) {
		t.Fatalf("Compact(1, 1) below Base: err = %v, want wraps ErrCompacted", err)
	}
	if after := snapshot(l); !reflect.DeepEqual(before, after) {
		t.Fatalf("Compact(1, 1): log modified by rejected compact: got %+v, want %+v", after, before)
	}
}

func TestAppendAfterCompactContinuesFromNewLastIndex(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 3)...)
	if err := l.Compact(3, 1); err != nil {
		t.Fatalf("Compact(3, 1): unexpected error: %v", err)
	}

	mustAppend(t, l, mkEntries(4, 2, 2)...)
	if got := l.LastIndex(); got != 5 {
		t.Fatalf("LastIndex() = %d, want 5", got)
	}
	if got := l.LastTerm(); got != 2 {
		t.Fatalf("LastTerm() = %d, want 2", got)
	}
}

// TestUpToDateComparesTermBeforeIndex is R3, the election restriction, and
// the single most important test in this file: the comparison must weigh
// term before index. Reversing that order is exactly mutant R3 in the RQ2
// bug-injection study — comparing index first would let a longer log full of
// entries from a deposed leader outvote a shorter log holding the committed
// truth, which loses a committed entry.
func TestUpToDateComparesTermBeforeIndex(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 3)...) // LastIndex=3, LastTerm=1

	cases := []struct {
		name      string
		candTerm  Term
		candIndex Index
		want      bool
	}{
		{"equal term, longer candidate log", 1, 5, true},
		{"equal term, equal length", 1, 3, true},
		{"equal term, shorter candidate", 1, 2, false},
		{"higher candidate term but strictly shorter log", 2, 1, true},
		{"lower candidate term but much longer log", 0, 100, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := l.UpToDate(tc.candTerm, tc.candIndex); got != tc.want {
				t.Errorf("UpToDate(term=%d, index=%d) = %v, want %v", tc.candTerm, tc.candIndex, got, tc.want)
			}
		})
	}
}

func TestUpToDateAgainstCompactedLog(t *testing.T) {
	l := NewLog()
	mustAppend(t, l, mkEntries(1, 1, 5)...)
	if err := l.Compact(3, 1); err != nil {
		t.Fatalf("Compact(3, 1): unexpected error: %v", err)
	}
	// LastIndex=5, LastTerm=1 still, after compacting the first three
	// entries away.

	if !l.UpToDate(1, 5) {
		t.Error("UpToDate(1, 5) against compacted log: got false, want true (equal length)")
	}
	if l.UpToDate(1, 4) {
		t.Error("UpToDate(1, 4) against compacted log: got true, want false (shorter)")
	}
	if !l.UpToDate(2, 0) {
		t.Error("UpToDate(2, 0) against compacted log: got false, want true (higher term dominates)")
	}
}

func TestUpToDateAgainstEmptyLog(t *testing.T) {
	l := NewLog()
	if !l.UpToDate(0, 0) {
		t.Error("UpToDate(0, 0) against empty log: got false, want true (equal term, equal length)")
	}
	if !l.UpToDate(1, 0) {
		t.Error("UpToDate(1, 0) against empty log: got false, want true (higher term)")
	}
}

// TestLogInvariantsHoldAcrossRandomOperations drives a log through a long,
// reproducible sequence of appends, truncations and compactions from a fixed
// seed, checking the representation's basic invariants after every single
// operation. It does not assert any particular resulting content — that is
// covered by the tests above — only that the log never becomes internally
// inconsistent no matter what sequence of legal operations it is put
// through.
func TestLogInvariantsHoldAcrossRandomOperations(t *testing.T) {
	const seed = uint64(0xF00DCAFE)
	r := prng.New(seed)
	l := NewLog()
	term := Term(1)

	const steps = 2000
	for step := 0; step < steps; step++ {
		switch r.Intn(3) {
		case 0: // append
			if r.Chance(0.3) {
				term++
			}
			n := r.Intn(5) + 1
			es := mkEntries(l.LastIndex()+1, term, n)
			if err := l.Append(es...); err != nil {
				t.Fatalf("step %d: Append: unexpected error: %v", step, err)
			}
		case 1: // truncate
			if l.Len() > 0 {
				i := l.FirstIndex() + Index(r.Intn(l.Len()))
				if err := l.TruncateFrom(i); err != nil {
					t.Fatalf("step %d: TruncateFrom(%d): unexpected error: %v", step, i, err)
				}
			}
		case 2: // compact
			if l.LastIndex() > l.Base() {
				size := int(l.LastIndex() - l.Base())
				i := l.Base() + Index(r.Intn(size)) + 1
				ct, ok := l.TermAt(i)
				if !ok {
					t.Fatalf("step %d: TermAt(%d): ok = false, want true", step, i)
				}
				if err := l.Compact(i, ct); err != nil {
					t.Fatalf("step %d: Compact(%d, %d): unexpected error: %v", step, i, ct, err)
				}
			}
		}
		checkLogInvariants(t, l, step)
	}
}

// checkLogInvariants asserts the internal-consistency properties that must
// hold for any Log, regardless of what sequence of legal operations produced
// it.
func checkLogInvariants(t *testing.T, l *Log, step int) {
	t.Helper()

	if got, want := l.LastIndex()-l.Base(), Index(l.Len()); got != want {
		t.Fatalf("step %d: LastIndex()-Base() = %d, want Len() (%d)", step, got, want)
	}

	prevTerm := l.BaseTerm()
	for i := l.FirstIndex(); i <= l.LastIndex(); i++ {
		e, ok := l.At(i)
		if !ok {
			t.Fatalf("step %d: At(%d): ok = false, want true", step, i)
		}
		if e.Index != i {
			t.Fatalf("step %d: At(%d).Index = %d, want %d", step, i, e.Index, i)
		}
		if e.Term < prevTerm {
			t.Fatalf("step %d: term went backwards at index %d: %d < %d", step, i, e.Term, prevTerm)
		}
		prevTerm = e.Term
	}

	if _, ok := l.TermAt(l.Base()); !ok {
		t.Fatalf("step %d: TermAt(Base()) (%d): ok = false, want true", step, l.Base())
	}
}
