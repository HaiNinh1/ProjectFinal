package sim

import (
	"sort"

	"verity/node"
	"verity/prng"
)

// NetConfig holds the network parameters for one run, drawn from the seed.
// See docs/SPEC.md 5.2.
type NetConfig struct {
	BaseDelay   node.Duration // minimum one-way delay
	DelayJitter node.Duration // extra delay, uniform in [0, DelayJitter)
	Bandwidth   int64         // bytes per second; 0 means unlimited
	DropRate    float64       // probability a message is silently discarded
	DupRate     float64       // probability a message is delivered twice
}

// Link is a directed pair of nodes. Verity models partitions as directed,
// because asymmetric partitions are legal in real networks and are where the
// good bugs live (docs/SPEC.md 5.2).
type Link struct{ From, To node.NodeID }

// Net models one run's network: the delay, jitter, bandwidth, drop, and
// duplicate behaviour of NetConfig, plus a set of partitioned links.
//
// Net is self-contained and does not schedule anything itself. It decides
// the fate of one message and reports when each delivery should occur; the
// caller — the simulator's main loop — schedules the returned times on the
// timeline. Keeping Net free of the timeline is what makes it unit-testable
// without a cluster.
type Net struct {
	cfg     NetConfig
	rng     *prng.Rand
	blocked map[Link]bool
}

// NewNet returns a Net configured with cfg, drawing all randomness from rng.
// No links are blocked initially.
func NewNet(cfg NetConfig, rng *prng.Rand) *Net {
	return &Net{cfg: cfg, rng: rng, blocked: make(map[Link]bool)}
}

// Route decides the fate of one message of size bytes sent from -> to at
// time now, and returns the absolute times at which it should be delivered:
// nil if the link is partitioned or the message was dropped, one entry
// normally, two if it was duplicated.
//
// The draw order is part of the contract and must never change casually,
// because changing it changes what every recorded seed means:
//
//  1. If the link is blocked, return nil immediately and consume no draws. A
//     partitioned message never reaches the wire, so it must not perturb the
//     stream — that keeps removing a partition during seed minimisation a
//     local change rather than one that reshuffles the rest of the run.
//  2. Draw the drop decision. The draw is consumed whether or not it drops.
//  3. Draw the jitter for the first delivery.
//  4. Draw the duplicate decision.
//  5. If duplicated, draw the jitter for the second delivery.
//
// The two delivery times are drawn independently, so a duplicate may
// legitimately arrive before the original; Route returns them in draw order
// and leaves the timeline's (At, Seq) ordering to sort them out. Route does
// not sort them itself — the resulting reorder is deliberate, and more
// realistic than an explicit shuffle (docs/SPEC.md 5.2).
func (n *Net) Route(now node.Time, from, to node.NodeID, size int) []node.Time {
	if n.Blocked(from, to) {
		return nil
	}
	if n.rng.Chance(n.cfg.DropRate) {
		return nil
	}
	first := now.Add(n.oneDelay(size))
	times := []node.Time{first}
	if n.rng.Chance(n.cfg.DupRate) {
		second := now.Add(n.oneDelay(size))
		times = append(times, second)
	}
	return times
}

// oneDelay draws the jitter for a single delivery and combines it with
// BaseDelay and the transmission delay implied by size, per the formula
// documented on Route.
func (n *Net) oneDelay(size int) node.Duration {
	var jitter node.Duration
	if n.cfg.DelayJitter > 0 {
		jitter = node.Duration(n.rng.Uint64n(uint64(n.cfg.DelayJitter)))
	}
	return n.cfg.BaseDelay + jitter + n.transmission(size)
}

// transmission returns the delay a message of size bytes adds under
// Bandwidth: 0 if Bandwidth is unlimited (<= 0) or size is non-positive.
func (n *Net) transmission(size int) node.Duration {
	if n.cfg.Bandwidth <= 0 || size <= 0 {
		return 0
	}
	return node.Duration(int64(size) * int64(node.Second) / n.cfg.Bandwidth)
}

// Partition blocks the directed link from -> to. Messages sent the other
// way, to -> from, are unaffected.
func (n *Net) Partition(from, to node.NodeID) {
	n.blocked[Link{From: from, To: to}] = true
}

// Isolate blocks both directions between a and b.
func (n *Net) Isolate(a, b node.NodeID) {
	n.Partition(a, b)
	n.Partition(b, a)
}

// Heal unblocks the directed link from -> to. Healing a link that is not
// blocked is not an error.
func (n *Net) Heal(from, to node.NodeID) {
	delete(n.blocked, Link{From: from, To: to})
}

// HealAll unblocks every link.
func (n *Net) HealAll() {
	n.blocked = make(map[Link]bool)
}

// Blocked reports whether the directed link from -> to is currently
// partitioned.
func (n *Net) Blocked(from, to node.NodeID) bool {
	return n.blocked[Link{From: from, To: to}]
}

// BlockedLinks returns the currently blocked links, sorted by (From, To).
//
// This is the one place the blocked set is ranged (INV-6): everywhere else
// it is only point-queried by Blocked. The sort is what makes the result
// safe to fold into a trace or a dump — an unsorted range over the map would
// reorder nondeterministically across runs with the same seed.
func (n *Net) BlockedLinks() []Link {
	links := make([]Link, 0, len(n.blocked))
	for l := range n.blocked {
		links = append(links, l)
	}
	sort.Slice(links, func(i, j int) bool {
		if links[i].From != links[j].From {
			return links[i].From < links[j].From
		}
		return links[i].To < links[j].To
	})
	return links
}
