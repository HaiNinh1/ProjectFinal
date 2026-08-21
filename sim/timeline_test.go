package sim

import (
	"math/rand"
	"sort"
	"testing"

	"verity/node"
)

// timelineTestPayload is a minimal Payload for these tests only.
type timelineTestPayload int

func (timelineTestPayload) payload() {}

// TestTimelineOrdering pushes items with scrambled times and checks that
// Pop yields them in nondecreasing At order.
func TestTimelineOrdering(t *testing.T) {
	times := []int64{50, 10, 30, 5, 40, 20, 1, 100, 0, 25}
	tl := newTimeline()
	for _, at := range times {
		tl.Push(node.Time(at), timelineTestPayload(at))
	}

	var last node.Time = -1
	count := 0
	for {
		it, ok := tl.Pop()
		if !ok {
			break
		}
		if it.At < last {
			t.Fatalf("Pop returned At %d after %d: order violated", it.At, last)
		}
		last = it.At
		count++
	}
	if count != len(times) {
		t.Fatalf("popped %d items, want %d", count, len(times))
	}
}

// TestTimelineExactTies pushes many items at the same At and checks that
// Pop returns them in exactly insertion (seq) order. The body runs several
// times so a wrong less() that only shows up for certain heap array shapes
// does not slip through by accident.
func TestTimelineExactTies(t *testing.T) {
	const n = 1000
	for iter := 0; iter < 5; iter++ {
		tl := newTimeline()
		wantSeqs := make([]uint64, n)
		for i := 0; i < n; i++ {
			seq := tl.Push(node.Time(7), timelineTestPayload(i))
			wantSeqs[i] = seq
		}
		for i := 0; i < n; i++ {
			it, ok := tl.Pop()
			if !ok {
				t.Fatalf("iter %d: Pop returned false at i=%d, want an item", iter, i)
			}
			if it.Seq != wantSeqs[i] {
				t.Fatalf("iter %d: Pop #%d returned Seq %d, want %d", iter, i, it.Seq, wantSeqs[i])
			}
			if it.At != 7 {
				t.Fatalf("iter %d: Pop #%d returned At %d, want 7", iter, i, it.At)
			}
		}
		if _, ok := tl.Pop(); ok {
			t.Fatalf("iter %d: timeline not empty after popping all items", iter)
		}
	}
}

// TestTimelineMixedTies interleaves pushes across three distinct timestamps
// and checks that Pop order matches a reference sort by (At, Seq).
func TestTimelineMixedTies(t *testing.T) {
	stamps := []node.Time{100, 50, 200}
	const perStamp = 50

	tl := newTimeline()
	var want []Item

	// Interleave: for each of perStamp rounds, push one item at each stamp,
	// in the same stamps order each round.
	for round := 0; round < perStamp; round++ {
		for _, at := range stamps {
			seq := tl.Push(at, timelineTestPayload(round))
			want = append(want, Item{At: at, Seq: seq})
		}
	}

	sort.Slice(want, func(i, j int) bool { return less(want[i], want[j]) })

	for i, wantItem := range want {
		it, ok := tl.Pop()
		if !ok {
			t.Fatalf("Pop #%d returned false, want At=%d Seq=%d", i, wantItem.At, wantItem.Seq)
		}
		if it.At != wantItem.At || it.Seq != wantItem.Seq {
			t.Fatalf("Pop #%d = {At:%d Seq:%d}, want {At:%d Seq:%d}", i, it.At, it.Seq, wantItem.At, wantItem.Seq)
		}
	}
	if _, ok := tl.Pop(); ok {
		t.Fatal("timeline not empty after popping all items")
	}
}

// TestTimelinePushDuringDrain pops some items, pushes new ones at or after
// Now, and keeps popping — checking that ordering still holds and Now never
// decreases throughout.
func TestTimelinePushDuringDrain(t *testing.T) {
	tl := newTimeline()
	for _, at := range []int64{10, 20, 30, 40, 50} {
		tl.Push(node.Time(at), timelineTestPayload(at))
	}

	var lastNow node.Time = -1
	checkNow := func() {
		now := tl.Now()
		if now < lastNow {
			t.Fatalf("Now went backward: %d -> %d", lastNow, now)
		}
		lastNow = now
	}

	// Pop two.
	if _, ok := tl.Pop(); !ok {
		t.Fatal("Pop 1 failed")
	}
	checkNow()
	if _, ok := tl.Pop(); !ok {
		t.Fatal("Pop 2 failed")
	}
	checkNow()

	// Push more, at and after the current Now.
	tl.Push(tl.Now(), timelineTestPayload(1000))
	tl.Push(tl.Now().Add(5), timelineTestPayload(1001))
	tl.Push(node.Time(1000), timelineTestPayload(1002))

	var lastAt node.Time = -1
	for {
		it, ok := tl.Pop()
		if !ok {
			break
		}
		if it.At < lastAt {
			t.Fatalf("Pop returned At %d after %d during drain", it.At, lastAt)
		}
		lastAt = it.At
		checkNow()
	}
}

// TestTimelineNowSemantics checks Now is 0 before any Pop, tracks the last
// popped At after each Pop, and is unaffected by Push.
func TestTimelineNowSemantics(t *testing.T) {
	tl := newTimeline()
	if got := tl.Now(); got != 0 {
		t.Fatalf("Now() before any Pop = %d, want 0", got)
	}

	tl.Push(node.Time(100), timelineTestPayload(1))
	if got := tl.Now(); got != 0 {
		t.Fatalf("Now() after Push (no Pop yet) = %d, want 0", got)
	}

	tl.Push(node.Time(50), timelineTestPayload(2))
	if got := tl.Now(); got != 0 {
		t.Fatalf("Now() after second Push (no Pop yet) = %d, want 0", got)
	}

	it, ok := tl.Pop()
	if !ok {
		t.Fatal("Pop failed")
	}
	if got := tl.Now(); got != it.At {
		t.Fatalf("Now() after Pop = %d, want %d", got, it.At)
	}
	if it.At != 50 {
		t.Fatalf("first Pop At = %d, want 50", it.At)
	}

	it2, ok := tl.Pop()
	if !ok {
		t.Fatal("second Pop failed")
	}
	if got := tl.Now(); got != it2.At {
		t.Fatalf("Now() after second Pop = %d, want %d", got, it2.At)
	}
	if it2.At != 100 {
		t.Fatalf("second Pop At = %d, want 100", it2.At)
	}
}

// TestTimelinePushPastPanics checks that scheduling before Now panics.
func TestTimelinePushPastPanics(t *testing.T) {
	tl := newTimeline()
	tl.Push(node.Time(100), timelineTestPayload(1))
	tl.Pop() // Now == 100

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("Push at a past time did not panic")
			}
		}()
		tl.Push(node.Time(99), timelineTestPayload(2))
	}()
}

// TestTimelinePushNilPanics checks that scheduling a nil payload panics.
func TestTimelinePushNilPanics(t *testing.T) {
	tl := newTimeline()
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("Push with a nil payload did not panic")
			}
		}()
		tl.Push(node.Time(1), nil)
	}()
}

// TestTimelinePeekTime checks PeekTime agrees with the next Pop, and
// returns false on an empty timeline.
func TestTimelinePeekTime(t *testing.T) {
	tl := newTimeline()
	if _, ok := tl.PeekTime(); ok {
		t.Fatal("PeekTime on empty timeline returned true")
	}

	for _, at := range []int64{30, 10, 20} {
		tl.Push(node.Time(at), timelineTestPayload(at))
	}

	for tl.Len() > 0 {
		peeked, ok := tl.PeekTime()
		if !ok {
			t.Fatal("PeekTime returned false while items remain")
		}
		it, ok := tl.Pop()
		if !ok {
			t.Fatal("Pop returned false while items remain")
		}
		if peeked != it.At {
			t.Fatalf("PeekTime = %d, but Pop returned At %d", peeked, it.At)
		}
	}

	if _, ok := tl.PeekTime(); ok {
		t.Fatal("PeekTime after draining returned true")
	}
}

// TestTimelineSeqUnique checks Seq is globally unique and strictly
// increasing across every Push in a timeline's life, including after the
// heap has drained to empty and been refilled.
func TestTimelineSeqUnique(t *testing.T) {
	tl := newTimeline()
	var lastSeq uint64
	seen := make(map[uint64]bool)

	push := func(at int64) {
		seq := tl.Push(node.Time(at), timelineTestPayload(at))
		if seen[seq] {
			t.Fatalf("Seq %d reused", seq)
		}
		seen[seq] = true
		if len(seen) > 1 && seq <= lastSeq {
			t.Fatalf("Seq %d did not increase past previous %d", seq, lastSeq)
		}
		lastSeq = seq
	}

	for i := 0; i < 100; i++ {
		push(int64(i))
	}
	// Drain fully.
	for tl.Len() > 0 {
		tl.Pop()
	}
	// Refill from the (now advanced) Now and keep checking Seq.
	base := int64(tl.Now())
	for i := 0; i < 100; i++ {
		push(base + int64(i))
	}
	for tl.Len() > 0 {
		tl.Pop()
	}

	if len(seen) != 200 {
		t.Fatalf("saw %d distinct seqs, want 200", len(seen))
	}
}

// TestTimelineStress pushes 10000 items at times drawn from a small set (so
// there are many ties), interleaved with pops, and checks the result
// against a reference implementation built by sorting a slice.
func TestTimelineStress(t *testing.T) {
	rng := rand.New(rand.NewSource(12345))
	stampPool := []int64{0, 1, 2, 5, 10}

	tl := newTimeline()
	var pending []Item // reference: everything pushed but not yet popped
	var got []Item     // everything this test has popped, in pop order

	const totalPushes = 10000
	pushed := 0
	for pushed < totalPushes || len(pending) > 0 {
		doPush := pushed < totalPushes && (len(pending) == 0 || rng.Intn(2) == 0)
		if doPush {
			at := node.Time(stampPool[rng.Intn(len(stampPool))])
			if at < tl.Now() {
				at = tl.Now()
			}
			seq := tl.Push(at, timelineTestPayload(pushed))
			pending = append(pending, Item{At: at, Seq: seq})
			pushed++
			continue
		}
		// Pop: find and remove the (At, Seq)-least pending item from the
		// reference, and compare against the real Pop.
		sort.Slice(pending, func(i, j int) bool { return less(pending[i], pending[j]) })
		want := pending[0]
		pending = pending[1:]

		it, ok := tl.Pop()
		if !ok {
			t.Fatalf("Pop returned false, want {At:%d Seq:%d}", want.At, want.Seq)
		}
		if it.At != want.At || it.Seq != want.Seq {
			t.Fatalf("Pop = {At:%d Seq:%d}, want {At:%d Seq:%d}", it.At, it.Seq, want.At, want.Seq)
		}
		got = append(got, it)
	}

	if _, ok := tl.Pop(); ok {
		t.Fatal("timeline not empty after draining all pushes")
	}
	if len(got) != totalPushes {
		t.Fatalf("popped %d items total, want %d", len(got), totalPushes)
	}
}
