package sim

import (
	"io"

	"verity/node"
	"verity/prng"
)

// NewNodeFunc constructs the node under test.
//
// It is a factory rather than a prebuilt instance because a restart must
// produce a genuinely fresh state machine: node.Node says Restore is called at
// most once, before the first Step, so a node cannot be rewound in place.
// Handing the simulator a constructor is what lets it model a crash as the
// loss of everything that was not on disk, which is the only honest model of
// one.
//
// peers is the full sorted membership, including id. rng is this node's own
// stream, derived from the run seed by an explicit Split, so a node
// reconstructed later in the run still draws the values it drew last time.
type NewNodeFunc func(id node.NodeID, peers []node.NodeID, rng *prng.Rand) node.Node

// Config is everything one run needs. Two Sims built from equal Configs
// produce byte-identical traces; nothing outside it is consulted.
type Config struct {
	Seed    uint64
	Nodes   []node.NodeID
	New     NewNodeFunc
	Horizon node.Duration
	Net     NetConfig
	Disk    DiskConfig

	// Profile selects the fault mix. Nil means the run draws one from the
	// catalogue, which is the swarm behaviour objective S2 wants. Naming a
	// profile explicitly is for reproducing and minimising a known failure.
	// Ignored when Schedule is set.
	Schedule *Schedule
	Profile  *Profile

	// TraceTo receives the trace text. Nil hashes without writing, which is
	// what a sweep across thousands of seeds wants.
	TraceTo io.Writer
}

// Reply is one answer delivered back to a client, recorded in the order the
// simulator observed it.
type Reply struct {
	At    node.Time
	From  node.NodeID
	ReqID uint64
	Resp  []byte
	Err   error
}

// replica is the simulator's bookkeeping for one node: the state machine
// itself, plus everything about it that lives outside it — its disk, its armed
// timers, whether it is up, and how far its clock is offset.
type replica struct {
	id   node.NodeID
	n    node.Node
	rng  *prng.Rand
	disk *Disk
	up   bool
	skew node.Duration

	// timers maps an armed timer name to the generation that is live. A
	// TimerFired is delivered only if its generation still matches, which is
	// how SetTimer replacing an armed name, ClearTimer, and a crash all cancel
	// a pending fire without having to hunt for it in the heap. Generations
	// come from timerSeq, which only ever increases and is deliberately NOT
	// reset by a crash: a reset would let a fire scheduled before the crash
	// match a timer armed after it.
	timers   map[string]uint64
	timerSeq uint64

	applied []uint64 // indices handed to the state machine, in order
}

// The payload types. Each is one thing the simulator scheduled for itself to
// do later; together they are the complete set of ways this world changes.
type pDeliver struct {
	to, from node.NodeID
	msg      node.Message
}

type pTimer struct {
	to   node.NodeID
	name string
	gen  uint64
}

type pPersist struct {
	to   node.NodeID
	id   uint64
	sync bool
}

type pClient struct {
	to    node.NodeID
	reqID uint64
	cmd   []byte
}

type pRestarted struct{ to node.NodeID }

type pFault struct{ f Fault }

func (pDeliver) payload()   {}
func (pTimer) payload()     {}
func (pPersist) payload()   {}
func (pClient) payload()    {}
func (pRestarted) payload() {}
func (pFault) payload()     {}

// Sim is one deterministic run: a virtual clock, a set of replicas, and the
// models that stand between them.
type Sim struct {
	cfg   Config
	tl    *timeline
	net   *Net
	trace *Trace
	sched Schedule

	ids  []node.NodeID // sorted; the iteration order for everything (INV-6)
	reps map[node.NodeID]*replica

	replies []Reply
}

// Stream identifiers for the top-level Splits off the run seed. They are named
// constants because the numbers are load-bearing: changing one changes what
// every recorded seed means. A new subsystem takes the next number rather than
// renumbering what is already here.
const (
	streamNet   = 1
	streamFault = 2
	streamDisk  = 3
	streamNode  = 4
)

// New builds a run from cfg. It panics on a configuration that cannot produce
// a meaningful run — no nodes, no constructor, a non-positive horizon, or a
// duplicate node ID — because each of those would otherwise surface as a
// silently empty trace, which looks exactly like a passing test.
func New(cfg Config) *Sim {
	if len(cfg.Nodes) == 0 {
		panic("sim: New: no nodes")
	}
	if cfg.New == nil {
		panic("sim: New: nil node constructor")
	}
	if cfg.Horizon <= 0 {
		panic("sim: New: horizon must be positive")
	}

	// The caller may well have built this slice by ranging a map. Sorting a
	// copy means the run does not inherit that order (INV-6), and copying
	// means the caller's slice is not reordered underneath them.
	ids := make([]node.NodeID, len(cfg.Nodes))
	copy(ids, cfg.Nodes)
	sortNodeIDs(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			panic("sim: New: duplicate node ID")
		}
	}

	root := prng.New(cfg.Seed)
	s := &Sim{
		cfg:   cfg,
		tl:    newTimeline(),
		net:   NewNet(cfg.Net, root.Split(streamNet)),
		trace: NewTrace(cfg.TraceTo),
		ids:   ids,
		reps:  make(map[node.NodeID]*replica, len(ids)),
	}

	diskRoot := root.Split(streamDisk)
	nodeRoot := root.Split(streamNode)
	for _, id := range ids {
		rng := nodeRoot.Split(uint64(id))
		r := &replica{
			id:     id,
			rng:    rng,
			disk:   NewDisk(cfg.Disk, diskRoot.Split(uint64(id))),
			up:     true,
			timers: make(map[string]uint64),
		}
		r.n = cfg.New(id, ids, rng)
		s.reps[id] = r
	}

	// The fault schedule is drawn before the run starts so that it can be
	// dumped, edited, and minimised (SPEC 5.4). Drawing faults lazily would
	// make the schedule an artefact of how far the run got, which is exactly
	// what a minimiser cannot work with.
	//
	// A caller may supply a schedule instead of drawing one. That is the other
	// end of the same idea: dumping a schedule as text, commenting faults out
	// of it, and reading it back is only useful if something will then run it.
	// Seed minimisation (T6.1) is built on exactly that loop, which is why the
	// round trip through String and ParseSchedule has to be exact.
	//
	// Skipping the draw perturbs nothing else: the fault stream is its own
	// Split off the run seed, so consuming nothing from it leaves the network,
	// disk, and per-node streams exactly where they would otherwise have been.
	if cfg.Schedule != nil {
		s.sched = *cfg.Schedule
	} else {
		faultRng := root.Split(streamFault)
		p := cfg.Profile
		if p == nil {
			drawn := DrawProfile(faultRng)
			p = &drawn
		}
		s.sched = GenerateSchedule(faultRng, *p, ids, cfg.Horizon)
	}
	for _, f := range s.sched.Faults {
		s.tl.Push(f.At, pFault{f: f})
	}

	return s
}

// sortNodeIDs sorts ids ascending with an insertion sort. The slice is a
// cluster membership — three to seven entries — so having the comparison
// visible here is worth more than an asymptotically better sort would be.
func sortNodeIDs(ids []node.NodeID) {
	for i := 1; i < len(ids); i++ {
		v := ids[i]
		j := i - 1
		for j >= 0 && ids[j] > v {
			ids[j+1] = ids[j]
			j--
		}
		ids[j+1] = v
	}
}

// Schedule reports the fault schedule this run follows. It is fixed before the
// first Step, so it is safe to dump before, during, or after a run.
func (s *Sim) Schedule() Schedule { return s.sched }

// Replies reports the client replies observed so far, in observation order.
func (s *Sim) Replies() []Reply { return s.replies }

// Applied reports the log indices the given node handed to its state machine,
// in order. It is how a test asserts apply-order rules without reaching inside
// the node.
func (s *Sim) Applied(id node.NodeID) []uint64 {
	if r := s.reps[id]; r != nil {
		return r.applied
	}
	return nil
}

// Up reports whether the given node is currently running.
func (s *Sim) Up(id node.NodeID) bool {
	r := s.reps[id]
	return r != nil && r.up
}

// Hash reports the rolling trace hash: the value the determinism test compares
// across two runs of one seed.
func (s *Sim) Hash() uint64 { return s.trace.Hash() }

// Steps reports how many Step calls the run has made, which is the number of
// lines in its trace.
func (s *Sim) Steps() uint64 { return s.trace.Lines() }

// Now reports the current virtual time.
func (s *Sim) Now() node.Time { return s.tl.Now() }

// Call schedules a client request against node to. It is the only way work
// enters the cluster from outside.
func (s *Sim) Call(at node.Time, to node.NodeID, reqID uint64, cmd []byte) {
	s.tl.Push(at, pClient{to: to, reqID: reqID, cmd: cmd})
}

// Run advances the virtual clock until the horizon is reached or nothing is
// left to do, and reports the first trace write error if there was one.
//
// Because time is virtual, an idle cluster costs nothing: the clock jumps
// straight to the next scheduled instant. That is why a run measured in
// simulated hours finishes in milliseconds.
func (s *Sim) Run() error {
	horizon := node.Time(s.cfg.Horizon)
	for {
		at, ok := s.tl.PeekTime()
		if !ok || at > horizon {
			break
		}
		it, _ := s.tl.Pop()
		s.dispatch(it)
	}
	return s.trace.Err()
}

// dispatch turns one scheduled item into either a Step or a change to the
// world around the nodes.
func (s *Sim) dispatch(it Item) {
	switch p := it.P.(type) {
	case pDeliver:
		r := s.reps[p.to]
		if r == nil || !r.up {
			return // a message to a node that is down is simply lost
		}
		s.step(it.Seq, r, node.Deliver{From: p.from, Msg: p.msg})

	case pTimer:
		r := s.reps[p.to]
		if r == nil || !r.up {
			return
		}
		if r.timers[p.name] != p.gen {
			return // cleared, replaced, or armed before a crash
		}
		delete(r.timers, p.name)
		s.step(it.Seq, r, node.TimerFired{Name: p.name})

	case pPersist:
		r := s.reps[p.to]
		if r == nil || !r.up {
			return
		}
		r.disk.Completed(p.sync)
		s.step(it.Seq, r, node.PersistDone{ID: p.id})

	case pClient:
		r := s.reps[p.to]
		if r == nil || !r.up {
			return
		}
		s.step(it.Seq, r, node.ClientCall{ReqID: p.reqID, Cmd: p.cmd})

	case pRestarted:
		r := s.reps[p.to]
		if r == nil || !r.up {
			return
		}
		s.step(it.Seq, r, node.Restarted{})

	case pFault:
		s.applyFault(p.f)
	}
}

// step runs one event through one node and performs everything it asks for.
//
// The node is handed its own skewed view of the clock, while the trace records
// the simulator's true virtual time. That difference is the whole point of the
// clock skew fault class: the node reasons about a time that is wrong, and the
// trace still lines up across runs, so the resulting misbehaviour is diffable
// rather than merely mysterious.
func (s *Sim) step(seq uint64, r *replica, ev node.Event) {
	now := s.tl.Now()
	acts := r.n.Step(now.Add(r.skew), ev)
	s.trace.Step(seq, now, r.id, ev, acts)
	for _, a := range acts {
		s.perform(r, now, a)
	}
}

// perform carries out one action. Actions happen in the order the node
// returned them (SPEC 3); nothing here reorders or batches them, because that
// order is the node's entire observable effect on the world.
func (s *Sim) perform(r *replica, now node.Time, a node.Action) {
	switch act := a.(type) {
	case node.Send:
		size := 0
		if act.Msg != nil {
			size = act.Msg.Size()
		}
		for _, at := range s.net.Route(now, r.id, act.To, size) {
			s.tl.Push(at, pDeliver{to: act.To, from: r.id, msg: act.Msg})
		}

	case node.Persist:
		at := r.disk.Persist(now, act.Records, act.Sync)
		s.tl.Push(at, pPersist{to: r.id, id: act.ID, sync: act.Sync})

	case node.SetTimer:
		r.timerSeq++
		r.timers[act.Name] = r.timerSeq
		s.tl.Push(now.Add(act.After), pTimer{to: r.id, name: act.Name, gen: r.timerSeq})

	case node.ClearTimer:
		delete(r.timers, act.Name)

	case node.Reply:
		s.replies = append(s.replies, Reply{
			At:    now,
			From:  r.id,
			ReqID: act.ReqID,
			Resp:  act.Resp,
			Err:   act.Err,
		})

	case node.Apply:
		r.applied = append(r.applied, act.Index)
	}
}

// applyFault changes the world around the nodes. Nothing here calls Step
// directly: a fault is something that happens TO a node, and the node learns of
// it only through the events that follow.
func (s *Sim) applyFault(f Fault) {
	r := s.reps[f.A]

	switch f.Kind {
	case FaultCrash:
		if r == nil || !r.up {
			return
		}
		r.up = false
		r.disk.Crash()
		// Everything volatile goes: the armed timers and the state machine
		// itself. Dropping the node rather than reusing it is what makes the
		// crash honest — anything it held in memory is genuinely gone, and
		// only what reached the disk can come back.
		r.timers = make(map[string]uint64)
		r.n = nil

	case FaultRestart:
		if r == nil || r.up {
			return
		}
		recs, _ := r.disk.Restore()
		r.n = s.cfg.New(r.id, s.ids, r.rng)
		if err := r.n.Restore(recs); err != nil {
			// A node that cannot read its own disk is a bug worth stopping
			// for, not one to paper over by pretending the restart worked.
			panic("sim: restart: Restore failed: " + err.Error())
		}
		r.up = true
		// Restarted is delivered as a scheduled event rather than stepped
		// inline, so that every Step in the trace comes from a timeline pop
		// and carries a timeline seq.
		s.tl.Push(s.tl.Now(), pRestarted{to: r.id})

	case FaultPartition:
		s.net.Partition(f.A, f.B)

	case FaultHeal:
		s.net.Heal(f.A, f.B)

	case FaultClockSkew:
		if r != nil {
			r.skew = node.Duration(f.Arg)
		}

	case FaultClockJump:
		if r != nil {
			r.skew += node.Duration(f.Arg)
		}
	}
}
