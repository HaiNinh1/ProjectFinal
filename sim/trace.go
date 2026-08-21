package sim

import (
	"hash/fnv"
	"io"
	"strconv"

	"verity/node"
)

// Trace records one line per Step call and folds each line into a rolling
// FNV-1a 64 hash. The determinism test (see docs/SPEC.md 5.5) runs a seed
// twice and compares Hash: on a mismatch it re-runs with a writer attached to
// both sides and diffs the two texts, which localises the nondeterminism to a
// single Step. The line format is fixed and documented at Step; it is
// compared textually across runs and across machines, so it must never
// depend on anything but the values passed in.
type Trace struct {
	w   io.Writer
	sum interface {
		io.Writer
		Sum64() uint64
	}
	lines uint64
	err   error
	buf   []byte
}

// NewTrace returns a recorder that hashes every line it is given. If w is
// nil, lines are hashed and counted but not written anywhere: that is what a
// sweep across many seeds wants, since the hash alone is the comparison and
// writing a trace per seed would be gigabytes of I/O for runs that almost
// always match. On a mismatch, the seed is replayed with a writer attached.
func NewTrace(w io.Writer) *Trace {
	return &Trace{w: w, sum: fnv.New64a()}
}

// Step records one Step call as a line of the form
//
//	<seq> <vtime_ns> <node_id> <EventKind> -> <ActionKind>[,<ActionKind>...]
//
// seq, the nanosecond value of at, and id are printed as plain base-10
// unsigned integers with no padding: padding would make the format depend on
// the magnitude of the numbers, which is a needless way to break a golden
// file. When acts is empty the text after the arrow is a single "-" rather
// than nothing, so the line never ends in trailing whitespace, which does not
// survive editors and diff tools reliably. Action kinds are joined by ","
// with no space, in the exact order acts was returned in: that order is a
// node's entire observable effect on the world, and reordering or sorting it
// would hide the exact class of bug this trace exists to catch. A nil ev is
// rendered as the literal event kind "nil" rather than causing a panic, so
// that a runtime bug delivering a nil event shows up as a visible anomaly in
// the trace instead of a crash that loses the trace. Every line, including
// the last, ends in "\n", and the hash covers that byte.
func (t *Trace) Step(seq uint64, at node.Time, id node.NodeID, ev node.Event, acts []node.Action) {
	buf := t.buf[:0]
	buf = strconv.AppendUint(buf, seq, 10)
	buf = append(buf, ' ')
	buf = strconv.AppendUint(buf, uint64(at), 10)
	buf = append(buf, ' ')
	buf = strconv.AppendUint(buf, uint64(id), 10)
	buf = append(buf, ' ')
	if ev == nil {
		buf = append(buf, "nil"...)
	} else {
		buf = append(buf, ev.EventKind()...)
	}
	buf = append(buf, " -> "...)
	if len(acts) == 0 {
		buf = append(buf, '-')
	} else {
		for i, a := range acts {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, a.ActionKind()...)
		}
	}
	buf = append(buf, '\n')
	t.buf = buf

	// fnv's Write never returns an error; only the caller-supplied writer can
	// fail, and that failure must not stop the hash from advancing (see Err).
	t.sum.Write(buf)
	t.lines++

	if t.w != nil && t.err == nil {
		if _, err := t.w.Write(buf); err != nil {
			t.err = err
		}
	}
}

// Hash reports the FNV-1a 64 hash of every line recorded so far.
func (t *Trace) Hash() uint64 { return t.sum.Sum64() }

// Lines reports how many lines have been recorded.
func (t *Trace) Lines() uint64 { return t.lines }

// Err reports the first error seen writing to w, if any. A trace that failed
// to write is not a trace, and the caller must not report the run as clean
// even though Hash keeps advancing regardless of write errors.
func (t *Trace) Err() error { return t.err }
