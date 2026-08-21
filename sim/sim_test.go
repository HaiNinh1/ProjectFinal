package sim

import (
	"testing"

	"verity/node"
	"verity/prng"
)

// simScript is a node whose behaviour is supplied by the test: on each Step it
// calls react and returns whatever that says. It exists so the driver can be
// tested directly, rather than only through the echo node — a driver bug and a
// node bug produce the same symptom at the cluster level, and this separates
// them.
type simScript struct {
	id    node.NodeID
	react func(now node.Time, ev node.Event) []node.Action

	// seenNow records the time the node was told it was, per event kind. Clock
	// skew is only observable from inside a node, so this is the only place a
	// test can check it.
	seen     []simSeen
	restored [][]node.Record
	restarts int
}

type simSeen struct {
	now  node.Time
	kind string
}

func (n *simScript) ID() node.NodeID { return n.id }

func (n *simScript) Restore(recs []node.Record) error {
	cp := make([]node.Record, len(recs))
	copy(cp, recs)
	n.restored = append(n.restored, cp)
	return nil
}

func (n *simScript) Step(now node.Time, ev node.Event) []node.Action {
	n.seen = append(n.seen, simSeen{now: now, kind: ev.EventKind()})
	if _, ok := ev.(node.Restarted); ok {
		n.restarts++
	}
	if n.react == nil {
		return nil
	}
	return n.react(now, ev)
}

// simCount reports how many recorded events had the given kind.
func (n *simScript) count(kind string) int {
	c := 0
	for _, s := range n.seen {
		if s.kind == kind {
			c++
		}
	}
	return c
}

// simHarness wires one scripted node into a Sim and keeps a handle on the live
// instance, which the factory replaces on every restart.
type simHarness struct {
	s    *Sim
	live *simScript
}

func simBuild(t *testing.T, cfg Config, react func(now node.Time, ev node.Event) []node.Action) *simHarness {
	t.Helper()
	h := &simHarness{}
	if len(cfg.Nodes) == 0 {
		cfg.Nodes = []node.NodeID{1}
	}
	if cfg.Horizon == 0 {
		cfg.Horizon = node.Second
	}
	if cfg.Profile == nil && cfg.Schedule == nil {
		cfg.Profile = &Profile{Name: "quiet"}
	}
	cfg.New = func(id node.NodeID, _ []node.NodeID, _ *prng.Rand) node.Node {
		n := &simScript{id: id, react: react}
		h.live = n
		return n
	}
	h.s = New(cfg)
	return h
}

// TestSimTimerReplacementFiresOnce checks that arming a timer name that is
// already armed replaces it, rather than leaving two pending fires (SPEC §3).
// Getting this wrong would give Raft two election timeouts for every one it
// asked for, which looks like a flapping cluster rather than like a bug in the
// runtime.
func TestSimTimerReplacementFiresOnce(t *testing.T) {
	h := simBuild(t, Config{Seed: 1}, func(now node.Time, ev node.Event) []node.Action {
		if _, ok := ev.(node.ClientCall); ok {
			return []node.Action{
				node.SetTimer{Name: "t", After: 10 * node.Millisecond},
				node.SetTimer{Name: "t", After: 20 * node.Millisecond},
			}
		}
		return nil
	})
	h.s.Call(0, 1, 1, nil)
	if err := h.s.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := h.live.count("TimerFired"); got != 1 {
		t.Fatalf("TimerFired count = %d, want 1", got)
	}
	for _, s := range h.live.seen {
		if s.kind == "TimerFired" && s.now != node.Time(20*node.Millisecond) {
			t.Fatalf("timer fired at %d, want %d (the replacing timer, not the replaced one)",
				s.now, node.Time(20*node.Millisecond))
		}
	}
}

// TestSimClearTimerPreventsFire checks that a cleared timer never arrives.
func TestSimClearTimerPreventsFire(t *testing.T) {
	h := simBuild(t, Config{Seed: 1}, func(now node.Time, ev node.Event) []node.Action {
		c, ok := ev.(node.ClientCall)
		if !ok {
			return nil
		}
		if c.ReqID == 1 {
			return []node.Action{node.SetTimer{Name: "t", After: 10 * node.Millisecond}}
		}
		return []node.Action{node.ClearTimer{Name: "t"}}
	})
	h.s.Call(0, 1, 1, nil)
	h.s.Call(node.Time(5*node.Millisecond), 1, 2, nil)
	if err := h.s.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := h.live.count("TimerFired"); got != 0 {
		t.Fatalf("TimerFired count = %d, want 0 — a cleared timer fired", got)
	}
}

// TestSimTimerArmedBeforeCrashNeverFiresAfterRestart is the test for decision
// D13, and it is the reason the timer generation counter is not reset by a
// crash.
//
// The node arms a timer, crashes before it fires, and restarts. It then arms a
// timer again. If the generation counter had been reset on the crash, the
// pre-crash fire — still sitting in the heap — would match the post-restart
// generation and be delivered, giving the restarted node an election timeout
// it never asked for. That bug would only ever appear on seeds where a crash
// lands inside a timer window, which is the worst possible place to have to
// find one.
func TestSimTimerArmedBeforeCrashNeverFiresAfterRestart(t *testing.T) {
	sched := &Schedule{
		Profile: "handwritten",
		Horizon: node.Second,
		Nodes:   []node.NodeID{1},
		Faults: []Fault{
			{At: node.Time(5 * node.Millisecond), Kind: FaultCrash, A: 1},
			{At: node.Time(6 * node.Millisecond), Kind: FaultRestart, A: 1},
		},
	}

	h := simBuild(t, Config{Seed: 1, Schedule: sched}, func(now node.Time, ev node.Event) []node.Action {
		switch ev.(type) {
		case node.ClientCall:
			// Armed at t=0, due at t=100ms, but the crash lands at t=5ms.
			return []node.Action{node.SetTimer{Name: "t", After: 100 * node.Millisecond}}
		case node.Restarted:
			// Armed again after the restart, due at t=206ms. If the pre-crash
			// fire is wrongly delivered it arrives at t=100ms, well before.
			return []node.Action{node.SetTimer{Name: "t", After: 200 * node.Millisecond}}
		}
		return nil
	})
	h.s.Call(0, 1, 1, nil)
	if err := h.s.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	if h.live.restarts != 1 {
		t.Fatalf("restarts = %d, want 1", h.live.restarts)
	}
	// The post-restart node is a fresh instance, so its own seen list holds
	// only what happened after the restart.
	if got := h.live.count("TimerFired"); got != 1 {
		t.Fatalf("TimerFired count after restart = %d, want exactly 1", got)
	}
	for _, s := range h.live.seen {
		if s.kind != "TimerFired" {
			continue
		}
		if want := node.Time(206 * node.Millisecond); s.now != want {
			t.Fatalf("timer fired at %d, want %d — a timer armed before the crash was delivered after it",
				s.now, want)
		}
	}
}

// TestSimMessagesToDownNodeAreLost checks that a node that is down neither
// receives nor is stepped. A crashed process does not read its socket.
func TestSimMessagesToDownNodeAreLost(t *testing.T) {
	sched := &Schedule{
		Profile: "handwritten",
		Horizon: node.Second,
		Nodes:   []node.NodeID{1, 2},
		Faults: []Fault{
			{At: node.Time(time1ms), Kind: FaultCrash, A: 2},
		},
	}

	var sawDeliverOn2 bool
	h := &simHarness{}
	cfg := Config{
		Seed:     1,
		Nodes:    []node.NodeID{1, 2},
		Horizon:  node.Second,
		Schedule: sched,
		Net:      NetConfig{BaseDelay: 5 * node.Millisecond},
	}
	cfg.New = func(id node.NodeID, _ []node.NodeID, _ *prng.Rand) node.Node {
		n := &simScript{id: id}
		n.react = func(now node.Time, ev node.Event) []node.Action {
			if _, ok := ev.(node.ClientCall); ok && id == 1 {
				return []node.Action{node.Send{To: 2, Msg: simMsg{}}}
			}
			if _, ok := ev.(node.Deliver); ok && id == 2 {
				sawDeliverOn2 = true
			}
			return nil
		}
		if id == 2 {
			h.live = n
		}
		return n
	}
	h.s = New(cfg)
	// Sent at t=0, would arrive at t=5ms; node 2 crashes at t=1ms.
	h.s.Call(0, 1, 1, nil)
	if err := h.s.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sawDeliverOn2 {
		t.Fatal("a message was delivered to a node that was down")
	}
	if h.s.Up(2) {
		t.Fatal("node 2 should still be down")
	}
}

const time1ms = node.Time(1000000)

type simMsg struct{}

func (simMsg) Kind() string { return "simMsg" }
func (simMsg) Size() int    { return 8 }

// TestSimClockSkewShiftsTheNodeNotTheTrace checks that a skewed node sees a
// shifted clock while the trace keeps recording true virtual time. If the skew
// leaked into the trace, two runs with different skew would be trivially
// different and the trace would stop being a stable basis for comparison; if
// it did not reach the node, the clock-skew fault class would do nothing at
// all and RQ5 would have nothing to measure.
func TestSimClockSkewShiftsTheNodeNotTheTrace(t *testing.T) {
	const skew = node.Duration(50 * node.Millisecond)

	sched := &Schedule{
		Profile: "handwritten",
		Horizon: node.Second,
		Nodes:   []node.NodeID{1},
		Faults: []Fault{
			{At: 0, Kind: FaultClockSkew, A: 1, Arg: int64(skew)},
		},
	}
	h := simBuild(t, Config{Seed: 1, Schedule: sched}, nil)

	callAt := node.Time(10 * node.Millisecond)
	h.s.Call(callAt, 1, 1, nil)
	if err := h.s.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(h.live.seen) != 1 {
		t.Fatalf("steps seen = %d, want 1", len(h.live.seen))
	}
	if got, want := h.live.seen[0].now, callAt.Add(skew); got != want {
		t.Fatalf("node saw now = %d, want %d (true time plus skew)", got, want)
	}
	if got := h.s.Now(); got != callAt {
		t.Fatalf("simulator virtual time = %d, want %d (unskewed)", got, callAt)
	}
}

// TestSimActionsHappenInOrder checks that actions are carried out in the order
// the node returned them, which SPEC §3 requires and on which the
// persist-before-send discipline of INV-8 depends.
func TestSimActionsHappenInOrder(t *testing.T) {
	h := simBuild(t, Config{
		Seed:  1,
		Nodes: []node.NodeID{1},
		Disk:  DiskConfig{WriteLatency: node.Millisecond},
	}, func(now node.Time, ev node.Event) []node.Action {
		if _, ok := ev.(node.ClientCall); !ok {
			return nil
		}
		return []node.Action{
			node.Reply{ReqID: 1, Resp: []byte("a")},
			node.Reply{ReqID: 2, Resp: []byte("b")},
			node.Reply{ReqID: 3, Resp: []byte("c")},
		}
	})
	h.s.Call(0, 1, 1, nil)
	if err := h.s.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := h.s.Replies()
	if len(got) != 3 {
		t.Fatalf("replies = %d, want 3", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if string(got[i].Resp) != want {
			t.Fatalf("reply %d = %q, want %q — actions were reordered", i, got[i].Resp, want)
		}
	}
}

// TestSimRestoreReceivesOnlyDurableRecords checks the crash boundary from the
// node's side: a restarted node is handed exactly what was synced, and nothing
// that was merely written (INV-8).
func TestSimRestoreReceivesOnlyDurableRecords(t *testing.T) {
	sched := &Schedule{
		Profile: "handwritten",
		Horizon: node.Second,
		Nodes:   []node.NodeID{1},
		Faults: []Fault{
			{At: node.Time(50 * node.Millisecond), Kind: FaultCrash, A: 1},
			{At: node.Time(60 * node.Millisecond), Kind: FaultRestart, A: 1},
		},
	}

	h := simBuild(t, Config{
		Seed:     1,
		Schedule: sched,
		Disk:     DiskConfig{WriteLatency: node.Millisecond, SyncLatency: node.Millisecond},
	}, func(now node.Time, ev node.Event) []node.Action {
		c, ok := ev.(node.ClientCall)
		if !ok {
			return nil
		}
		// ReqID 1 is synced and will survive; ReqID 2 is not and will not.
		return []node.Action{node.Persist{
			ID:      c.ReqID,
			Records: []node.Record{{Kind: node.RecordEntry, Index: c.ReqID, Data: []byte{byte(c.ReqID)}}},
			Sync:    c.ReqID == 1,
		}}
	})
	h.s.Call(node.Time(node.Millisecond), 1, 1, nil)
	h.s.Call(node.Time(10*node.Millisecond), 1, 2, nil)
	if err := h.s.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(h.live.restored) != 1 {
		t.Fatalf("Restore called %d times, want 1", len(h.live.restored))
	}
	recs := h.live.restored[0]
	if len(recs) != 1 {
		t.Fatalf("restored %d records, want 1 (only the synced one)", len(recs))
	}
	if recs[0].Index != 1 {
		t.Fatalf("restored record index = %d, want 1 — an unsynced write survived a crash", recs[0].Index)
	}
}

// TestSimSuppliedScheduleIsUsedVerbatim closes the loop T1.8 opened: a schedule
// dumped as text, parsed back, and handed to a run must be the schedule that
// run actually follows. Without this, ParseSchedule would round-trip perfectly
// into nothing, and seed minimisation would have no way to apply its reduced
// schedule.
func TestSimSuppliedScheduleIsUsedVerbatim(t *testing.T) {
	generated := New(Config{
		Seed:    0x1234,
		Nodes:   []node.NodeID{1, 2, 3},
		New:     clusterEcho(20 * node.Millisecond),
		Horizon: node.Second,
	}).Schedule()

	parsed, err := ParseSchedule(generated.String())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	replayed := New(Config{
		Seed:     0x1234,
		Nodes:    []node.NodeID{1, 2, 3},
		New:      clusterEcho(20 * node.Millisecond),
		Horizon:  node.Second,
		Schedule: &parsed,
	}).Schedule()

	if replayed.String() != generated.String() {
		t.Fatalf("supplied schedule was not used verbatim\n--- supplied ---\n%s\n--- used ---\n%s",
			generated.String(), replayed.String())
	}
}

// TestSimNewRejectsBadConfig checks that a configuration which cannot produce a
// meaningful run fails loudly. Each of these would otherwise show up as an
// empty trace, which is indistinguishable from a passing test.
func TestSimNewRejectsBadConfig(t *testing.T) {
	ok := func(id node.NodeID, _ []node.NodeID, _ *prng.Rand) node.Node {
		return &simScript{id: id}
	}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no nodes", Config{New: ok, Horizon: node.Second}},
		{"nil constructor", Config{Nodes: []node.NodeID{1}, Horizon: node.Second}},
		{"zero horizon", Config{Nodes: []node.NodeID{1}, New: ok}},
		{"negative horizon", Config{Nodes: []node.NodeID{1}, New: ok, Horizon: -1}},
		{"duplicate node", Config{Nodes: []node.NodeID{1, 1}, New: ok, Horizon: node.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("New did not panic")
				}
			}()
			New(tc.cfg)
		})
	}
}

// TestSimNodeOrderIsIndependentOfCallerOrder is the INV-6 check for the
// driver: the caller may well have built the node list by ranging a map, and
// the run must not inherit that order.
func TestSimNodeOrderIsIndependentOfCallerOrder(t *testing.T) {
	orders := [][]node.NodeID{
		{1, 2, 3},
		{3, 1, 2},
		{2, 3, 1},
		{3, 2, 1},
	}

	var want uint64
	for i, ids := range orders {
		s := New(Config{
			Seed:    0x99,
			Nodes:   ids,
			New:     clusterEcho(20 * node.Millisecond),
			Horizon: node.Second,
			Net:     NetConfig{BaseDelay: node.Millisecond},
			Disk:    DiskConfig{WriteLatency: node.Millisecond, SyncLatency: node.Millisecond},
		})
		for j := 0; j < 5; j++ {
			s.Call(node.Time(j)*node.Time(node.Millisecond), 1, uint64(j+1), []byte{byte(j)})
		}
		if err := s.Run(); err != nil {
			t.Fatalf("run: %v", err)
		}
		if i == 0 {
			want = s.Hash()
			continue
		}
		if got := s.Hash(); got != want {
			t.Fatalf("node order %v produced hash %#016x, want %#016x from order %v",
				ids, got, want, orders[0])
		}
	}
}
