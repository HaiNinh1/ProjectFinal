// Package node defines the contract every Verity replica implements.
//
// A node is a pure state machine. It has no clock, no network, no disk, no
// goroutines, and no global randomness. It learns the current time only from
// the now argument of Step, and it performs I/O only by returning Actions that
// the runtime carries out on its behalf. The runtime is either the
// deterministic simulator (package sim) or the real server (package runtime);
// neither is importable from here, and that is the point.
//
// Everything in Verity's determinism story reduces to this one property: given
// the same starting state and the same sequence of (now, Event) pairs, a node
// returns the same sequence of Actions, forever, on any machine. See
// docs/SPEC.md, invariants INV-1 through INV-4.
package node

// Time is monotonic nanoseconds since the start of the run.
//
// It is deliberately not time.Time. Nodes must never mix in a wall-clock
// reading, and a distinct type makes that a compile error rather than a
// review comment.
type Time int64

// Duration is a span of Time in nanoseconds.
type Duration int64

// Duration units. Nodes must not import the standard time package, so the
// familiar constants are redefined here.
const (
	Nanosecond  Duration = 1
	Microsecond          = 1000 * Nanosecond
	Millisecond          = 1000 * Microsecond
	Second               = 1000 * Millisecond
)

// Add returns t advanced by d.
func (t Time) Add(d Duration) Time { return t + Time(d) }

// Sub returns the span from u to t.
func (t Time) Sub(u Time) Duration { return Duration(t - u) }

// Before reports whether t precedes u.
func (t Time) Before(u Time) bool { return t < u }

// After reports whether t follows u.
func (t Time) After(u Time) bool { return t > u }

// NodeID identifies a single replica process. IDs are assigned by the runtime
// and are stable across restarts of the same replica.
type NodeID uint64

// GroupID identifies one Raft group. The shard controller is group 0; replica
// groups are numbered from 1.
type GroupID uint64

// ShardID identifies one shard of the key space. Verity uses a fixed shard
// count; see docs/SPEC.md section 8.
type ShardID uint32

// Message is a payload sent from one node to another.
//
// Kind names the message type for traces and must be stable, since determinism
// traces are compared textually across runs. Size reports the wire size in
// bytes, which the simulator's network model uses for transmission delay; it
// need not be exact, only deterministic.
type Message interface {
	Kind() string
	Size() int
}

// Event is something that happens to a node. The runtime delivers exactly one
// Event per Step call.
//
// EventKind names the event for traces. The method also seals the interface:
// only types in this package satisfy it.
type Event interface {
	EventKind() string
}

// Action is something a node asks the runtime to do. A node's entire effect on
// the world is the Actions it returns, in the order it returns them.
//
// ActionKind names the action for traces and seals the interface.
type Action interface {
	ActionKind() string
}

// ---------------------------------------------------------------- events ---

// Deliver reports the arrival of a message from another node. The runtime
// guarantees only that From sent Msg at some earlier point; it may be
// duplicated, delayed arbitrarily, or reordered relative to other messages.
type Deliver struct {
	From NodeID
	Msg  Message
}

// TimerFired reports that a timer previously requested by SetTimer has
// elapsed. Name matches the SetTimer that armed it.
type TimerFired struct {
	Name string
}

// PersistDone reports the completion of a Persist action. ID matches the
// Persist that requested it. A non-nil Err means the write did not reach
// durable storage and must not be treated as committed.
type PersistDone struct {
	ID  uint64
	Err error
}

// ClientCall delivers a client request. ReqID is unique per (client, attempt)
// and is echoed back in the corresponding Reply.
type ClientCall struct {
	ReqID uint64
	Cmd   []byte
}

// Restarted reports that the node has just come back up. It is delivered once,
// after Restore, before any other event.
type Restarted struct{}

func (Deliver) EventKind() string     { return "Deliver" }
func (TimerFired) EventKind() string  { return "TimerFired" }
func (PersistDone) EventKind() string { return "PersistDone" }
func (ClientCall) EventKind() string  { return "ClientCall" }
func (Restarted) EventKind() string   { return "Restarted" }

// --------------------------------------------------------------- actions ---

// Send asks the runtime to transmit Msg to To. Delivery is best-effort: the
// runtime may drop, duplicate, delay, or reorder it.
type Send struct {
	To  NodeID
	Msg Message
}

// Persist asks the runtime to write Records to durable storage. If Sync is
// true the write must reach stable storage before PersistDone reports success;
// if false the records may be lost on crash. A node must not act on a record
// as durable until it has seen the matching PersistDone with a nil Err.
type Persist struct {
	ID      uint64
	Records []Record
	Sync    bool
}

// SetTimer arms a timer that fires After from now, delivering TimerFired with
// the same Name. Arming a timer whose Name is already armed replaces it.
type SetTimer struct {
	Name  string
	After Duration
}

// ClearTimer disarms the timer with the given Name. Clearing an unarmed timer
// is not an error.
type ClearTimer struct {
	Name string
}

// Reply answers a ClientCall. ReqID matches the call being answered.
type Reply struct {
	ReqID uint64
	Resp  []byte
	Err   error
}

// Apply reports that the command at Index is committed and must be applied to
// the state machine. The runtime hands it to the state machine in index order;
// a node emits each index at most once per term of leadership, and the runtime
// tolerates duplicates by ignoring any index it has already applied.
type Apply struct {
	Index uint64
	Cmd   []byte
}

func (Send) ActionKind() string       { return "Send" }
func (Persist) ActionKind() string    { return "Persist" }
func (SetTimer) ActionKind() string   { return "SetTimer" }
func (ClearTimer) ActionKind() string { return "ClearTimer" }
func (Reply) ActionKind() string      { return "Reply" }
func (Apply) ActionKind() string      { return "Apply" }

// --------------------------------------------------------------- storage ---

// RecordKind distinguishes the durable record types a node writes.
type RecordKind uint8

const (
	// RecordHardState holds the Raft term, vote, and commit index. Exactly one
	// is live at a time; a later record supersedes an earlier one.
	RecordHardState RecordKind = iota + 1
	// RecordEntry holds one Raft log entry, identified by Index.
	RecordEntry
	// RecordSnapshot holds a state machine snapshot covering all indices up to
	// and including Index.
	RecordSnapshot
)

// Record is one durable unit of node state. Index is meaningful for
// RecordEntry and RecordSnapshot and is zero for RecordHardState.
type Record struct {
	Kind  RecordKind
	Index uint64
	Data  []byte
}

// ------------------------------------------------------------------ node ---

// Node is the state machine every Verity replica implements.
//
// Implementations must be deterministic: the same sequence of calls must
// produce the same sequence of Actions. In particular, an implementation must
// not read a clock, iterate a map without sorting, start a goroutine, or draw
// from a randomness source other than the seeded generator handed to it at
// construction.
type Node interface {
	// ID reports this node's identity.
	ID() NodeID

	// Restore reinitialises the node from durable records recovered after a
	// restart. Records arrive in the order they were written. Restore is
	// called at most once, before the first Step.
	Restore(recs []Record) error

	// Step consumes one event at time now and returns the actions the runtime
	// should perform, in order. Step must not block and must not retain the
	// event.
	Step(now Time, ev Event) []Action
}
