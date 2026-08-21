package raft

import (
	"errors"
	"fmt"
)

// EntryType distinguishes what a log entry means to the layers above Raft.
//
// The type is part of the durable record format, so the constants' numeric
// values are frozen: changing one silently reinterprets every log already on
// disk. New kinds get new values; existing values are never reused.
type EntryType uint8

const (
	// EntryNormal carries a client command for the state machine.
	EntryNormal EntryType = 0
	// EntryNoop carries nothing. A new leader appends one so that it has an
	// entry of its own term to commit, which is what lets earlier terms'
	// entries commit implicitly rather than by counting replicas (R2).
	EntryNoop EntryType = 1
	// EntryConfig carries a cluster configuration change (R10).
	EntryConfig EntryType = 2
)

// String names the entry type for traces and test failures.
func (t EntryType) String() string {
	switch t {
	case EntryNormal:
		return "Normal"
	case EntryNoop:
		return "Noop"
	case EntryConfig:
		return "Config"
	default:
		return "EntryType(?)"
	}
}

// Valid reports whether t is a type this build knows how to interpret. The
// record decoder uses it to reject a log written by a future version rather
// than guess at it.
func (t EntryType) Valid() bool {
	return t == EntryNormal || t == EntryNoop || t == EntryConfig
}

// Entry is one command in the replicated log.
//
// Index and Term together identify an entry across the whole cluster: Raft's
// central claim is that if two logs hold an entry with the same index and
// term, the logs are identical up to that point. Cmd is owned by the log once
// appended and must not be mutated by the caller afterwards.
type Entry struct {
	Term  Term
	Index Index
	Type  EntryType
	Cmd   []byte
}

// Errors reported by the log. They are sentinels so that callers and tests can
// distinguish a programming mistake (a gap, a truncation into history) from a
// merely absent entry, which is reported by an ok result instead.
var (
	// ErrNonContiguous means an append would leave a hole in the log or
	// overwrite an existing entry. Raft overwrites only by truncating first.
	ErrNonContiguous = errors.New("raft: non-contiguous append")
	// ErrCompacted means the operation addressed an index already discarded
	// by a snapshot, which the log can no longer reason about.
	ErrCompacted = errors.New("raft: index already compacted")
	// ErrUnavailable means the operation addressed an index past the end of
	// the log.
	ErrUnavailable = errors.New("raft: index not in log")
	// ErrTermMismatch means two accounts of the same log position disagree
	// about its term, which means they describe different histories and one
	// of them is wrong. It is never a recoverable condition.
	ErrTermMismatch = errors.New("raft: term mismatch")
)

// Log is a replica's replicated log.
//
// The representation keeps one sentinel entry ahead of the real ones, at
// entries[0]. The sentinel carries the index and term of the last entry that a
// snapshot has absorbed — 0 and 0 for a log that has never been compacted —
// and never carries a command. It exists so that "the entry immediately before
// index i" is always addressable, which is precisely what the AppendEntries
// consistency check asks for at the very start of the log and again at every
// snapshot boundary. Without it, both are special cases, and R4 is a rule that
// dies by special case.
type Log struct {
	entries []Entry
}

// NewLog returns an empty log: no entries, and a sentinel at index 0, term 0.
func NewLog() *Log {
	return &Log{entries: []Entry{{Index: 0, Term: 0}}}
}

// Base reports the index of the sentinel — the last index absorbed by a
// snapshot, or 0 if the log has never been compacted. The log knows Base's
// term but holds no command for it.
func (l *Log) Base() Index { return l.entries[0].Index }

// BaseTerm reports the term of the entry at Base.
func (l *Log) BaseTerm() Term { return l.entries[0].Term }

// FirstIndex reports the lowest index whose command the log still holds. It is
// one past Base, and it may exceed LastIndex when the log is empty.
func (l *Log) FirstIndex() Index { return l.entries[0].Index + 1 }

// LastIndex reports the highest index in the log, which is Base when the log
// holds no entries of its own.
func (l *Log) LastIndex() Index { return l.entries[len(l.entries)-1].Index }

// LastTerm reports the term of the entry at LastIndex.
func (l *Log) LastTerm() Term { return l.entries[len(l.entries)-1].Term }

// Len reports how many real entries the log holds, excluding the sentinel.
func (l *Log) Len() int { return len(l.entries) - 1 }

// offset converts a log index into a position in the entries slice. The result
// is only meaningful when i is in [Base, LastIndex].
func (l *Log) offset(i Index) int { return int(i - l.entries[0].Index) }

// TermAt reports the term of the entry at index i.
//
// It succeeds at Base as well as at every real index, because the consistency
// check needs the term of the entry before the one being appended, and that
// entry may well be the one a snapshot absorbed. ok is false when i has been
// compacted away or lies past the end of the log.
func (l *Log) TermAt(i Index) (Term, bool) {
	if i < l.Base() || i > l.LastIndex() {
		return 0, false
	}
	return l.entries[l.offset(i)].Term, true
}

// At returns the entry at index i. Unlike TermAt it fails at Base, because the
// sentinel is not an entry and carries no command.
func (l *Log) At(i Index) (Entry, bool) {
	if i <= l.Base() || i > l.LastIndex() {
		return Entry{}, false
	}
	return l.entries[l.offset(i)], true
}

// Matches reports whether the log holds an entry at index i whose term is
// term. This is the AppendEntries consistency check (R4): a follower accepts
// an append only where its own log agrees with the leader at the preceding
// position.
//
// It deliberately answers for Base too. A follower whose log begins after a
// snapshot has no entry at Base, but it does know that index's term, and that
// is enough to confirm agreement there.
func (l *Log) Matches(i Index, term Term) bool {
	t, ok := l.TermAt(i)
	return ok && t == term
}

// Slice returns the entries in [lo, hi), copied.
//
// The copy is not defensive pedantry. A leader slices its log to fill an
// AppendEntries, and may truncate or append before that message is delivered;
// handing out a subslice of the backing array would let a later append
// overwrite the contents of a message already in flight, which under the
// simulator is a divergence with no visible cause at the point it happens. The
// commands themselves are shared, and are immutable once appended.
//
// lo is clamped to FirstIndex and hi to LastIndex+1, so a caller asking for
// more than the log holds gets what there is rather than an error.
func (l *Log) Slice(lo, hi Index) []Entry {
	if lo < l.FirstIndex() {
		lo = l.FirstIndex()
	}
	if hi > l.LastIndex()+1 {
		hi = l.LastIndex() + 1
	}
	if lo >= hi {
		return nil
	}
	out := make([]Entry, hi-lo)
	copy(out, l.entries[l.offset(lo):l.offset(hi)])
	return out
}

// From returns every entry from lo to the end of the log, copied.
func (l *Log) From(lo Index) []Entry { return l.Slice(lo, l.LastIndex()+1) }

// Append adds entries to the end of the log.
//
// The entries must be contiguous, must carry the indices they claim, and must
// begin exactly one past LastIndex. Raft never overwrites in place: a leader
// whose entries conflict with a follower's makes the follower truncate first,
// so an append that is not a clean extension is a bug in the caller and is
// reported rather than absorbed.
func (l *Log) Append(es ...Entry) error {
	want := l.LastIndex() + 1
	for k, e := range es {
		if e.Index != want {
			return fmt.Errorf("%w: entry %d has index %d, want %d", ErrNonContiguous, k, e.Index, want)
		}
		want++
	}
	l.entries = append(l.entries, es...)
	return nil
}

// TruncateFrom discards the entry at index i and everything after it.
//
// This is the only way the log loses entries at its tail, and R4 restricts
// when a caller may invoke it: only on a genuine term conflict at an
// overlapping index, never merely because a stale or duplicated AppendEntries
// carried fewer entries than the follower already has. Enforcing that is the
// caller's job; the log enforces only that the truncation point is addressable.
func (l *Log) TruncateFrom(i Index) error {
	switch {
	case i <= l.Base():
		return fmt.Errorf("%w: cannot truncate from %d, log begins after %d", ErrCompacted, i, l.Base())
	case i > l.LastIndex():
		return fmt.Errorf("%w: cannot truncate from %d, log ends at %d", ErrUnavailable, i, l.LastIndex())
	}
	// Clear the dropped entries' command references so a truncated log does
	// not pin their memory through the retained backing array.
	for k := l.offset(i); k < len(l.entries); k++ {
		l.entries[k].Cmd = nil
	}
	l.entries = l.entries[:l.offset(i)]
	return nil
}

// Compact discards every entry up to and including lastIndex, moving the
// sentinel there and recording lastTerm as its term. It is how a snapshot
// releases the log it has absorbed.
//
// Compacting past LastIndex is allowed and empties the log: a follower that
// installs a snapshot from ahead of its own log discards all of it. Compacting
// to an index the log still holds requires the terms to agree, since
// disagreeing means the snapshot and the log describe different histories and
// one of them is wrong.
func (l *Log) Compact(lastIndex Index, lastTerm Term) error {
	if lastIndex < l.Base() {
		return fmt.Errorf("%w: cannot compact to %d, log already begins after %d", ErrCompacted, lastIndex, l.Base())
	}
	if t, ok := l.TermAt(lastIndex); ok && t != lastTerm {
		return fmt.Errorf("raft: %w: compact to %d term %d conflicts with log term %d", ErrTermMismatch, lastIndex, lastTerm, t)
	}
	if lastIndex >= l.LastIndex() {
		l.entries = []Entry{{Index: lastIndex, Term: lastTerm}}
		return nil
	}
	kept := l.entries[l.offset(lastIndex):]
	entries := make([]Entry, len(kept))
	copy(entries, kept)
	entries[0] = Entry{Index: lastIndex, Term: lastTerm}
	l.entries = entries
	return nil
}

// UpToDate reports whether a candidate whose last entry is (lastIndex,
// lastTerm) has a log at least as current as this one.
//
// This is the election restriction, R3, and the order of the comparison is the
// whole of it: the later term wins outright, and only when the terms are equal
// does length decide. Comparing indices first would let a longer log full of
// entries from a deposed leader outvote a shorter log holding the committed
// truth, and a committed entry would be lost.
func (l *Log) UpToDate(lastTerm Term, lastIndex Index) bool {
	if lastTerm != l.LastTerm() {
		return lastTerm > l.LastTerm()
	}
	return lastIndex >= l.LastIndex()
}
