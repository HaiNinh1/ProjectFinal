package raft

import "verity/node"

// pendingKind names what a node owes the world once a write reaches disk.
//
// A node cannot simply do the work and then write: INV-8 says nothing is
// durable until PersistDone returns with a nil error, and R1 says the vote
// must be durable before the response goes out. So the work is described here,
// parked, and carried out when — and only if — the write lands.
type pendingKind uint8

const (
	// pendNothing is a write with no deferred consequence: the state was
	// worth recording, but nobody is waiting on the acknowledgement.
	pendNothing pendingKind = iota
	// pendVoteResp is a reply to a RequestVote that must not leave this node
	// until the term and vote that justify it are durable (R1).
	pendVoteResp
)

// pending is one outstanding Persist and the deferred action it authorises.
//
// hard is a snapshot of the hard state as it stood when the write was issued,
// not a pointer to the node's live copy. The two diverge whenever a second
// write is issued before the first completes, and mistaking the newer state
// for the durable one is precisely the class of error R1 exists to prevent.
type pending struct {
	id   uint64
	kind pendingKind
	hard HardState
	to   node.NodeID
	resp RequestVoteResp
}

// persister tracks the writes a node has issued but not yet seen land.
//
// The queue is a slice in issue order rather than a map keyed by ID. Both
// would work, but a slice cannot be iterated in a surprising order, and this
// package would otherwise have to remember that every walk over the
// outstanding writes needs its keys sorted first (INV-6). The queue is short —
// it holds only writes genuinely in flight — so the linear scan costs nothing.
type persister struct {
	nextID    uint64
	queue     []pending
	durable   HardState
	durableID uint64
}

// issue allocates the next persist ID, records the deferred action, and
// returns the Persist action the caller should hand back to the runtime.
//
// Sync is always true. Every record this package writes is one Raft may not
// act on until it is genuinely on stable storage, and a Persist with Sync
// false yields a PersistDone that promises nothing (SPEC section 5.3).
func (p *persister) issue(recs []node.Record, pend pending) node.Persist {
	p.nextID++
	pend.id = p.nextID
	p.queue = append(p.queue, pend)
	return node.Persist{ID: pend.id, Records: recs, Sync: true}
}

// take removes and returns the pending write with the given ID.
//
// An unknown ID is not an error to the caller: a PersistDone can arrive for a
// write whose pending entry a restart discarded, and a node that panicked on
// one would be turning a survivable race into a crash.
func (p *persister) take(id uint64) (pending, bool) {
	for i := range p.queue {
		if p.queue[i].id != id {
			continue
		}
		pend := p.queue[i]
		p.queue = append(p.queue[:i], p.queue[i+1:]...)
		return pend, true
	}
	return pending{}, false
}

// complete records that pend's write reached stable storage.
//
// The durable hard state advances only for a write newer than the last one
// acknowledged. Completions arrive in issue order under the simulator today,
// but ordering is not something node.Persist promises, and an older write
// landing late must never be allowed to overwrite the record of a newer one —
// that would report state as durable that is not, which is the one thing this
// type exists to make impossible.
func (p *persister) complete(pend pending) {
	if pend.id > p.durableID {
		p.durableID = pend.id
		p.durable = pend.hard
	}
}

// outstanding reports how many writes are in flight. It exists for tests and
// for assertions; nothing in the protocol branches on it.
func (p *persister) outstanding() int { return len(p.queue) }
