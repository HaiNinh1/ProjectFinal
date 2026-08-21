package sim

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"verity/node"
	"verity/prng"
)

// FaultKind identifies one class of scheduled disturbance. See docs/SPEC.md
// 5.4.
type FaultKind uint8

const (
	// FaultCrash takes A down. Its unsynced disk tail is lost, per the disk
	// model (docs/SPEC.md 5.3). Always paired with a later FaultRestart for
	// the same A.
	FaultCrash FaultKind = iota + 1
	// FaultRestart brings A back up. Always the pair of a FaultCrash.
	FaultRestart
	// FaultPartition blocks the directed link A->B. B is unaffected sending
	// to A. Always paired with a later FaultHeal for the same (A, B).
	FaultPartition
	// FaultHeal unblocks the directed link A->B. Always the pair of a
	// FaultPartition.
	FaultHeal
	// FaultDropBurst makes A's links drop everything for Arg nanoseconds,
	// starting at At. Not paired: the duration is carried in Arg rather than
	// in a second event, since nothing else needs to observe the burst
	// ending.
	FaultDropBurst
	// FaultDiskSlow multiplies A's disk latency by Arg for a while. Not
	// paired: the runtime's disk model owns how long "a while" lasts, in the
	// same way it owns every other disk parameter.
	FaultDiskSlow
	// FaultClockSkew offsets A's clock by Arg nanoseconds, from At onward.
	// Not paired: a skew is a standing offset, not a bounded interval.
	FaultClockSkew
	// FaultClockJump moves A's clock by Arg nanoseconds, once, at At.
	FaultClockJump
)

// String reports the schedule text form of k, as ParseSchedule expects it.
func (k FaultKind) String() string {
	switch k {
	case FaultCrash:
		return "crash"
	case FaultRestart:
		return "restart"
	case FaultPartition:
		return "partition"
	case FaultHeal:
		return "heal"
	case FaultDropBurst:
		return "dropburst"
	case FaultDiskSlow:
		return "diskslow"
	case FaultClockSkew:
		return "clockskew"
	case FaultClockJump:
		return "clockjump"
	default:
		return fmt.Sprintf("faultkind(%d)", uint8(k))
	}
}

// faultParseKind is the inverse of FaultKind.String, used by ParseSchedule.
// Kind names, not the numeric values, are what a saved schedule stores, so
// that renumbering the constants above can never silently change the
// meaning of a file a person has been hand-editing during minimisation.
func faultParseKind(name string) (FaultKind, bool) {
	switch name {
	case "crash":
		return FaultCrash, true
	case "restart":
		return FaultRestart, true
	case "partition":
		return FaultPartition, true
	case "heal":
		return FaultHeal, true
	case "dropburst":
		return FaultDropBurst, true
	case "diskslow":
		return FaultDiskSlow, true
	case "clockskew":
		return FaultClockSkew, true
	case "clockjump":
		return FaultClockJump, true
	default:
		return 0, false
	}
}

// Fault is one scheduled disturbance.
type Fault struct {
	At   node.Time
	Kind FaultKind
	A    node.NodeID // the subject
	B    node.NodeID // the peer, for the directed link kinds; zero otherwise
	Arg  int64       // kind-specific magnitude; see the FaultKind constants
}

// faultLess reports whether a sorts strictly before b under the schedule's
// total order: (At, Kind, A, B, Arg). Every field is compared, so two
// faults are never left in an order that depends on how the generator
// happened to append them (see the sort in GenerateSchedule).
func faultLess(a, b Fault) bool {
	if a.At != b.At {
		return a.At < b.At
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.A != b.A {
		return a.A < b.A
	}
	if a.B != b.B {
		return a.B < b.B
	}
	return a.Arg < b.Arg
}

// Profile is one point in the space of fault mixes. Rates are events per
// simulated second across the whole cluster; zero disables the class.
type Profile struct {
	Name          string
	CrashRate     float64
	PartitionRate float64
	DropBurstRate float64
	DiskSlowRate  float64
	ClockSkewRate float64
	ClockJumpRate float64
	MaxDowntime   node.Duration // longest a crashed node stays down
	MaxPartition  node.Duration // longest a partition lasts before healing
	MaxSkew       node.Duration // largest per-node clock offset
}

// faultProfiles is the fixed, ordered catalogue Profiles returns.
//
// The catalogue is deliberately a set of EXTREMES, not a set of averages. A
// single "average" fault mix hides whole bug classes: a run with zero
// partitions but extreme clock skew finds things a balanced run never will,
// and a run that mixes everything at half intensity tends to find neither
// class's bugs reliably. Each profile below turns one or two classes up and
// leaves the rest off or low, so that a sweep across the catalogue (T6.2,
// objective S2) covers the space's corners rather than its centre.
//
// New profiles are appended at the END. DrawProfile and any seed recorded
// against an index into this slice depend on position, so inserting a
// profile in the middle would silently change what every previously
// recorded seed reproduces.
//
// Rates below are chosen against the default 60-simulated-second run
// horizon (docs/SPEC.md 11): each is picked to produce a handful to a few
// dozen events over that horizon, enough to matter without drowning a run
// in so many overlapping faults that a failure can no longer be attributed
// to one of them.
var faultProfiles = []Profile{
	{
		// The control. Bugs that reproduce here are not fault-induced, and
		// a failing "clean" seed is a bug in the system under test, not in
		// the fault injector.
		Name: "clean",
	},
	{
		// High crash rate, nothing else. 0.1/s over 60s expects ~6 crashes
		// across the cluster: enough for repeated leader loss without every
		// node being down at once.
		Name:        "crashy",
		CrashRate:   0.1,
		MaxDowntime: 8 * node.Second,
	},
	{
		// High partition rate, few crashes, long partitions: this is where
		// asymmetric-partition and split-brain-shaped bugs live. A rare
		// crash (0.01/s, ~0.6 expected) is kept so partition-during-crash
		// interactions are still occasionally exercised.
		Name:          "partitioned",
		CrashRate:     0.01,
		PartitionRate: 0.1,
		MaxDowntime:   5 * node.Second,
		MaxPartition:  20 * node.Second,
	},
	{
		// Frequent short drop bursts (0.5/s, ~30 expected) is the dominant
		// class; everything else is present but turned down, "moderate" as
		// specified, so the drop bursts are not drowned out by other noise.
		Name:          "flaky",
		CrashRate:     0.02,
		PartitionRate: 0.02,
		DropBurstRate: 0.5,
		DiskSlowRate:  0.05,
		ClockSkewRate: 0.02,
		MaxDowntime:   3 * node.Second,
		MaxPartition:  5 * node.Second,
		MaxSkew:       100 * node.Millisecond,
	},
	{
		// High disk slowdown, otherwise quiet: isolates persistence-latency
		// bugs (slow Persist interacting with election timeouts, batching,
		// pipelining depth) from every other fault class.
		Name:         "slow-disk",
		DiskSlowRate: 0.2,
	},
	{
		// Large clock skew and jumps, otherwise quiet. This is RQ5's
		// profile: it isolates the skew threshold at which LeaseRead starts
		// returning stale values while ReadIndex does not, from every other
		// source of noise.
		Name:          "skewed",
		ClockSkewRate: 0.1,
		ClockJumpRate: 0.05,
		MaxSkew:       3 * node.Second,
	},
	{
		// Everything on at once, at roughly each class's individual "high"
		// rate above. This is the profile least likely to isolate a single
		// bug class, and most likely to turn up an interaction between two
		// of them that no single-class profile ever could.
		Name:          "chaos",
		CrashRate:     0.1,
		PartitionRate: 0.1,
		DropBurstRate: 0.3,
		DiskSlowRate:  0.1,
		ClockSkewRate: 0.1,
		ClockJumpRate: 0.05,
		MaxDowntime:   5 * node.Second,
		MaxPartition:  10 * node.Second,
		MaxSkew:       2 * node.Second,
	},
}

// Profiles returns the fixed, ordered catalogue of swarm profiles. The
// result is a fresh copy on every call, so a caller cannot mutate the
// catalogue itself by mutating what it got back.
func Profiles() []Profile {
	out := make([]Profile, len(faultProfiles))
	copy(out, faultProfiles)
	return out
}

// DrawProfile picks one profile uniformly from the catalogue.
func DrawProfile(rng *prng.Rand) Profile {
	cat := Profiles()
	return cat[rng.Intn(len(cat))]
}

// Schedule is the complete fault plan for one run.
type Schedule struct {
	Profile string
	Horizon node.Duration
	Nodes   []node.NodeID
	Faults  []Fault
}

// Magnitudes for the two fault classes whose Arg has no corresponding field
// on Profile (DropBurst's duration and DiskSlow's multiplier). Every other
// class's magnitude is bounded by a Profile field, because the task
// description that defined Profile gives it exactly MaxDowntime,
// MaxPartition, and MaxSkew; these two ranges are fixed constants instead of
// per-profile knobs, so they do not vary across the catalogue the way rates
// and the other three maxima do.
const (
	faultDropBurstMinNS = int64(10 * node.Millisecond)
	faultDropBurstMaxNS = int64(500 * node.Millisecond)

	faultDiskSlowMinMult = int64(2)
	faultDiskSlowMaxMult = int64(20)
)

// faultClassCount converts a rate (events per simulated second, across the
// whole cluster) and a horizon into a deterministic event count: the
// integer part of the expectation, plus one more with probability equal to
// the fractional part. A true Poisson draw was considered and rejected — it
// would need more than one random draw and a nontrivial sampling loop, for
// no reproducibility benefit over this simpler construction, which is exact
// in expectation and precise enough for fault injection.
//
// This always draws exactly one value from rng (via Chance, which always
// draws exactly once regardless of its probability), whether or not the
// class is enabled. That means turning a profile's rate up from zero, or
// tuning it from one nonzero value to another, changes only that class's own
// draws and never reorders any other class's — the same reason Chance
// itself always draws once.
func faultClassCount(rng *prng.Rand, rate float64, horizon node.Duration) int {
	expected := rate * (float64(horizon) / float64(node.Second))
	if expected < 0 {
		expected = 0
	}
	whole := int(expected)
	frac := expected - float64(whole)
	if rng.Chance(frac) {
		whole++
	}
	return whole
}

// faultDrawTime draws an instant uniformly in [0, horizon).
func faultDrawTime(rng *prng.Rand, horizon node.Duration) node.Time {
	return node.Time(rng.Between(0, int64(horizon)))
}

// faultClampToHorizon returns t, or horizonAt if t falls after it. Used to
// keep a paired restart/heal inside the schedule when its drawn offset would
// otherwise run past the end of the run, so the schedule text stays complete
// and a minimiser can still see the intended pairing.
func faultClampToHorizon(t, horizonAt node.Time) node.Time {
	if t.After(horizonAt) {
		return horizonAt
	}
	return t
}

// GenerateSchedule draws the complete fault plan for one run from rng, given
// a fault profile, the run's node set, and its horizon.
//
// nodes is sorted into a private copy before anything else happens (INV-6):
// the caller may have built it from map iteration, and if the draws below
// depended on that order the schedule would not be reproducible. The sorted
// copy is also what ends up in the returned Schedule's Nodes field, and what
// every node index below is drawn against.
//
// The classes are drawn in a FIXED order — crash (paired with restart),
// partition (paired with heal), drop burst, disk slowdown, clock skew, clock
// jump — and within each class, every event draws in a fixed field order.
// This order is part of what a recorded seed means: changing it changes the
// schedule every previously recorded seed produces, silently.
//
// Within a class, if the node set is too small for that class to make sense
// (no nodes at all, or fewer than two for a partition), the class's count is
// still drawn from rng so later classes are unaffected, but the individual
// per-event draws are skipped — a degenerate node set produces a shorter
// schedule rather than a panic.
func GenerateSchedule(rng *prng.Rand, p Profile, nodes []node.NodeID, horizon node.Duration) Schedule {
	sorted := append([]node.NodeID(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] }) // INV-6
	n := len(sorted)
	horizonAt := node.Time(horizon)

	var faults []Fault

	// --- crash / restart ---------------------------------------------
	if count := faultClassCount(rng, p.CrashRate, horizon); count > 0 && n > 0 {
		for i := 0; i < count; i++ {
			at := faultDrawTime(rng, horizon)
			a := sorted[rng.Intn(n)]
			downtime := node.Duration(rng.Between(1, int64(p.MaxDowntime)+1))
			restartAt := faultClampToHorizon(at.Add(downtime), horizonAt)
			faults = append(faults, Fault{At: at, Kind: FaultCrash, A: a})
			faults = append(faults, Fault{At: restartAt, Kind: FaultRestart, A: a})
		}
	}

	// --- partition / heal ----------------------------------------------
	if count := faultClassCount(rng, p.PartitionRate, horizon); count > 0 && n > 1 {
		for i := 0; i < count; i++ {
			at := faultDrawTime(rng, horizon)
			idxA := rng.Intn(n)
			a := sorted[idxA]
			// offset in [1, n-1] against idxA, taken mod n, always lands on
			// an index distinct from idxA: this is how a directed pair with
			// A != B is drawn in one draw, without a reject-and-retry loop
			// that would otherwise make the draw count itself vary with
			// how often A happened to equal B.
			offset := rng.Intn(n-1) + 1
			b := sorted[(idxA+offset)%n]
			duration := node.Duration(rng.Between(1, int64(p.MaxPartition)+1))
			healAt := faultClampToHorizon(at.Add(duration), horizonAt)
			faults = append(faults, Fault{At: at, Kind: FaultPartition, A: a, B: b})
			faults = append(faults, Fault{At: healAt, Kind: FaultHeal, A: a, B: b})
		}
	}

	// --- drop burst ------------------------------------------------------
	if count := faultClassCount(rng, p.DropBurstRate, horizon); count > 0 && n > 0 {
		for i := 0; i < count; i++ {
			at := faultDrawTime(rng, horizon)
			a := sorted[rng.Intn(n)]
			dur := rng.Between(faultDropBurstMinNS, faultDropBurstMaxNS+1)
			faults = append(faults, Fault{At: at, Kind: FaultDropBurst, A: a, Arg: dur})
		}
	}

	// --- disk slowdown -----------------------------------------------
	if count := faultClassCount(rng, p.DiskSlowRate, horizon); count > 0 && n > 0 {
		for i := 0; i < count; i++ {
			at := faultDrawTime(rng, horizon)
			a := sorted[rng.Intn(n)]
			mult := rng.Between(faultDiskSlowMinMult, faultDiskSlowMaxMult+1)
			faults = append(faults, Fault{At: at, Kind: FaultDiskSlow, A: a, Arg: mult})
		}
	}

	// --- clock skew --------------------------------------------------
	if count := faultClassCount(rng, p.ClockSkewRate, horizon); count > 0 && n > 0 {
		for i := 0; i < count; i++ {
			at := faultDrawTime(rng, horizon)
			a := sorted[rng.Intn(n)]
			off := rng.Between(-int64(p.MaxSkew), int64(p.MaxSkew)+1)
			faults = append(faults, Fault{At: at, Kind: FaultClockSkew, A: a, Arg: off})
		}
	}

	// --- clock jump ----------------------------------------------------
	// Reuses MaxSkew as its magnitude bound rather than having a field of
	// its own: both a standing skew and a one-off jump are the same kind of
	// quantity, a clock offset in nanoseconds, and Profile has only one
	// field for that.
	if count := faultClassCount(rng, p.ClockJumpRate, horizon); count > 0 && n > 0 {
		for i := 0; i < count; i++ {
			at := faultDrawTime(rng, horizon)
			a := sorted[rng.Intn(n)]
			jump := rng.Between(-int64(p.MaxSkew), int64(p.MaxSkew)+1)
			faults = append(faults, Fault{At: at, Kind: FaultClockJump, A: a, Arg: jump})
		}
	}

	// A TOTAL order over all fields, not just At: two faults drawn at the
	// same instant would otherwise stay in whichever order the classes
	// above happened to append them, which is reproducible today but
	// fragile forever — any reordering of the class loop, or a future class
	// inserted between two existing ones, would silently change the tie
	// order of every seed that has one.
	sort.Slice(faults, func(i, j int) bool { return faultLess(faults[i], faults[j]) })

	return Schedule{Profile: p.Name, Horizon: horizon, Nodes: sorted, Faults: faults}
}

// String renders s in the schedule text format:
//
//	profile <name>
//	horizon <nanoseconds>
//	nodes <id> <id> ...
//	fault <at> <kindname> <a> <b> <arg>
//	...
//
// one fault per line, in s.Faults order. ParseSchedule is String's exact
// inverse: parsing the output of String and rendering it again produces the
// same text.
func (s Schedule) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "profile %s\n", s.Profile)
	fmt.Fprintf(&b, "horizon %d\n", int64(s.Horizon))
	b.WriteString("nodes")
	for _, id := range s.Nodes {
		fmt.Fprintf(&b, " %d", uint64(id))
	}
	b.WriteByte('\n')
	for _, f := range s.Faults {
		fmt.Fprintf(&b, "fault %d %s %d %d %d\n", int64(f.At), f.Kind, uint64(f.A), uint64(f.B), f.Arg)
	}
	return b.String()
}

// faultParseError formats a ParseSchedule failure so the offending line
// number and content are always visible in the message: a schedule file is
// meant to be hand-edited during minimisation, and an error that only says
// "malformed" sends the editor searching the whole file for the mistake.
func faultParseError(lineNo int, raw, format string, args ...any) error {
	return fmt.Errorf("sim: ParseSchedule: line %d: %s: %q", lineNo, fmt.Sprintf(format, args...), raw)
}

// ParseSchedule parses the text format String produces. Blank lines and
// lines starting with '#' (after leading whitespace is trimmed) are
// ignored — the single most useful affordance the format can offer a
// minimiser, since it lets a fault line be commented out rather than
// deleted, preserving it for the next attempt.
//
// Errors name the line number and content of the line that caused them, for
// an unknown fault kind, a malformed number, a truncated fault line, or a
// missing "profile" or "horizon" header. A missing header is only
// detectable once the whole input has been scanned, so that error names the
// last line of the input rather than a specific offending line.
func ParseSchedule(text string) (Schedule, error) {
	lines := strings.Split(text, "\n")

	var s Schedule
	haveProfile, haveHorizon, haveNodes := false, false, false

	for i, raw := range lines {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)

		switch fields[0] {
		case "profile":
			if len(fields) != 2 {
				return Schedule{}, faultParseError(lineNo, raw, "profile: want 1 field, got %d", len(fields)-1)
			}
			s.Profile = fields[1]
			haveProfile = true

		case "horizon":
			if len(fields) != 2 {
				return Schedule{}, faultParseError(lineNo, raw, "horizon: want 1 field, got %d", len(fields)-1)
			}
			v, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return Schedule{}, faultParseError(lineNo, raw, "horizon: %v", err)
			}
			s.Horizon = node.Duration(v)
			haveHorizon = true

		case "nodes":
			ids := make([]node.NodeID, 0, len(fields)-1)
			for _, f := range fields[1:] {
				v, err := strconv.ParseUint(f, 10, 64)
				if err != nil {
					return Schedule{}, faultParseError(lineNo, raw, "nodes: %v", err)
				}
				ids = append(ids, node.NodeID(v))
			}
			s.Nodes = ids
			haveNodes = true

		case "fault":
			if len(fields) != 6 {
				return Schedule{}, faultParseError(lineNo, raw, "fault: want 5 fields, got %d", len(fields)-1)
			}
			at, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return Schedule{}, faultParseError(lineNo, raw, "fault: at: %v", err)
			}
			kind, ok := faultParseKind(fields[2])
			if !ok {
				return Schedule{}, faultParseError(lineNo, raw, "fault: unknown kind %q", fields[2])
			}
			a, err := strconv.ParseUint(fields[3], 10, 64)
			if err != nil {
				return Schedule{}, faultParseError(lineNo, raw, "fault: a: %v", err)
			}
			bID, err := strconv.ParseUint(fields[4], 10, 64)
			if err != nil {
				return Schedule{}, faultParseError(lineNo, raw, "fault: b: %v", err)
			}
			arg, err := strconv.ParseInt(fields[5], 10, 64)
			if err != nil {
				return Schedule{}, faultParseError(lineNo, raw, "fault: arg: %v", err)
			}
			s.Faults = append(s.Faults, Fault{
				At:   node.Time(at),
				Kind: kind,
				A:    node.NodeID(a),
				B:    node.NodeID(bID),
				Arg:  arg,
			})

		default:
			return Schedule{}, faultParseError(lineNo, raw, "unrecognised line")
		}
	}

	last := len(lines)
	if !haveProfile {
		return Schedule{}, faultParseError(last, "", "missing required %q header", "profile")
	}
	if !haveHorizon {
		return Schedule{}, faultParseError(last, "", "missing required %q header", "horizon")
	}
	if !haveNodes {
		return Schedule{}, faultParseError(last, "", "missing required %q header", "nodes")
	}

	return s, nil
}
