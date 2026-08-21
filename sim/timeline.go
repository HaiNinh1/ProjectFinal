package sim

import "verity/node"

// Payload is what happens at a scheduled instant. It is a sealed marker
// interface; the concrete payload types live alongside the models that
// schedule them.
type Payload interface{ payload() }

// Item is one scheduled occurrence.
type Item struct {
	At  node.Time
	Seq uint64
	P   Payload
}

// less reports whether a is ordered strictly before b: an earlier At wins,
// and on an exact tie the smaller Seq wins (INV-7). Every comparison the
// heap makes goes through this one function, so the tie-break rule that
// makes reproducibility possible lives in exactly one place rather than
// being re-derived at each call site.
func less(a, b Item) bool {
	if a.At != b.At {
		return a.At < b.At
	}
	return a.Seq < b.Seq
}

// timeline is the simulator's event queue: a min-heap of pending Items
// ordered by (At, Seq). It is unexported — nothing outside package sim
// schedules directly against it.
//
// The heap is written by hand rather than built on container/heap.
// container/heap asks for a sort.Interface, which spreads the ordering
// across separate Less, Swap, and Len methods that a later change could
// edit independently and quietly desynchronize. Keeping sift-up and
// sift-down here instead means the single (At, Seq) comparison — less,
// above — is the only place the ordering is decided, which is where INV-7
// needs it to live.
type timeline struct {
	items []Item
	seq   uint64
	now   node.Time
}

// newTimeline returns an empty timeline. Its virtual clock starts at 0.
func newTimeline() *timeline {
	return &timeline{}
}

// Now reports the timeline's virtual clock: the At of the most recently
// popped Item, or 0 if nothing has been popped yet.
func (tl *timeline) Now() node.Time { return tl.now }

// Len reports the number of pending items.
func (tl *timeline) Len() int { return len(tl.items) }

// Push schedules p to occur at at, and returns the Seq assigned to it. Seq
// is drawn from a counter that starts at 0 and increases by one on every
// call, for the life of the timeline; it never resets, even after the
// timeline has drained to empty and been refilled.
//
// Push panics if at is before the current virtual clock, or if p is nil.
// Both are always a bug in the model doing the scheduling — scheduling into
// the past cannot be made sense of by any runtime, and a nil payload would
// only surface later, far from its cause. A silent clamp or a nil slipping
// through would hide the bug instead of failing loudly at the call site.
func (tl *timeline) Push(at node.Time, p Payload) uint64 {
	if at < tl.now {
		panic("sim: timeline.Push: at is before Now")
	}
	if p == nil {
		panic("sim: timeline.Push: nil payload")
	}
	it := Item{At: at, Seq: tl.seq, P: p}
	tl.seq++
	tl.items = append(tl.items, it)
	tl.siftUp(len(tl.items) - 1)
	return it.Seq
}

// Pop removes and returns the (At, Seq)-least item, and advances Now to its
// At. It returns false if the timeline is empty, in which case Now is left
// unchanged.
func (tl *timeline) Pop() (Item, bool) {
	if len(tl.items) == 0 {
		return Item{}, false
	}
	top := tl.items[0]
	last := len(tl.items) - 1
	tl.items[0] = tl.items[last]
	tl.items[last] = Item{}
	tl.items = tl.items[:last]
	if len(tl.items) > 0 {
		tl.siftDown(0)
	}
	tl.now = top.At
	return top, true
}

// PeekTime reports the At of the item Pop would return next, without
// popping it. It returns false if the timeline is empty.
func (tl *timeline) PeekTime() (node.Time, bool) {
	if len(tl.items) == 0 {
		return 0, false
	}
	return tl.items[0].At, true
}

// siftUp restores heap order after an append at index i by walking it
// toward the root while it sorts before its parent.
func (tl *timeline) siftUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !less(tl.items[i], tl.items[parent]) {
			break
		}
		tl.items[i], tl.items[parent] = tl.items[parent], tl.items[i]
		i = parent
	}
}

// siftDown restores heap order after the root is replaced by walking index
// i down toward whichever child sorts first, until both children sort after
// it or there are no children left.
func (tl *timeline) siftDown(i int) {
	n := len(tl.items)
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		smallest := left
		if right := left + 1; right < n && less(tl.items[right], tl.items[left]) {
			smallest = right
		}
		if !less(tl.items[smallest], tl.items[i]) {
			break
		}
		tl.items[i], tl.items[smallest] = tl.items[smallest], tl.items[i]
		i = smallest
	}
}
