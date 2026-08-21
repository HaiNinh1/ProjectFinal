package sim

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"verity/node"
	"verity/prng"
)

// faultTestNodes returns a fixed 5-node set, in the given order. Tests that
// care about order independence pass the same IDs in scrambled orders.
func faultTestNodes(order ...int) []node.NodeID {
	ids := make([]node.NodeID, len(order))
	for i, v := range order {
		ids[i] = node.NodeID(v)
	}
	return ids
}

const faultTestHorizon = 120 * node.Second

// faultBusyProfile is a profile with every class enabled, used by tests that
// want a schedule dense enough to exercise every code path in one call.
func faultBusyProfile() Profile {
	for _, p := range faultProfiles {
		if p.Name == "chaos" {
			return p
		}
	}
	panic("sim: fault_test: no chaos profile in catalogue")
}

// --- 1. determinism --------------------------------------------------------

func TestFault_Deterministic(t *testing.T) {
	nodes := faultTestNodes(1, 2, 3, 4, 5)
	p := faultBusyProfile()

	s1 := GenerateSchedule(prng.New(0xC0FFEE), p, nodes, faultTestHorizon)
	s2 := GenerateSchedule(prng.New(0xC0FFEE), p, nodes, faultTestHorizon)

	if s1.String() != s2.String() {
		t.Fatalf("same seed produced different text:\n--- s1 ---\n%s\n--- s2 ---\n%s", s1.String(), s2.String())
	}
	if !reflect.DeepEqual(s1.Faults, s2.Faults) {
		t.Fatalf("same seed produced different Faults slices:\n%+v\n%+v", s1.Faults, s2.Faults)
	}
}

func TestFault_DifferentSeedsDiffer(t *testing.T) {
	nodes := faultTestNodes(1, 2, 3, 4, 5)
	p := faultBusyProfile()

	s1 := GenerateSchedule(prng.New(1), p, nodes, faultTestHorizon)
	s2 := GenerateSchedule(prng.New(2), p, nodes, faultTestHorizon)

	if reflect.DeepEqual(s1.Faults, s2.Faults) {
		t.Fatalf("different seeds produced identical schedules")
	}
}

// --- 2. node-order independence (INV-6) ------------------------------------

func TestFault_NodeOrderIndependent(t *testing.T) {
	p := faultBusyProfile()
	orders := [][]node.NodeID{
		faultTestNodes(1, 2, 3, 4, 5),
		faultTestNodes(5, 4, 3, 2, 1),
		faultTestNodes(3, 1, 5, 2, 4),
	}

	var want string
	for i, nodes := range orders {
		s := GenerateSchedule(prng.New(0xABCD), p, nodes, faultTestHorizon)
		if i == 0 {
			want = s.String()
			continue
		}
		if s.String() != want {
			t.Fatalf("node order %v produced a different schedule than the first order:\n--- want ---\n%s\n--- got ---\n%s", nodes, want, s.String())
		}
	}
}

// --- 3. round trip -----------------------------------------------------

func TestFault_RoundTrip(t *testing.T) {
	nodes := faultTestNodes(1, 2, 3, 4, 5)
	for _, p := range Profiles() {
		t.Run(p.Name, func(t *testing.T) {
			s1 := GenerateSchedule(prng.New(0x5EED), p, nodes, faultTestHorizon)
			text1 := s1.String()

			s2, err := ParseSchedule(text1)
			if err != nil {
				t.Fatalf("ParseSchedule: %v", err)
			}
			text2 := s2.String()

			if text1 != text2 {
				t.Fatalf("String output changed across a round trip:\n--- first ---\n%s\n--- second ---\n%s", text1, text2)
			}
			if !reflect.DeepEqual(s1, s2) {
				t.Fatalf("Schedule changed across a round trip:\n%+v\n%+v", s1, s2)
			}
		})
	}
}

// --- 4. comments and blank lines --------------------------------------

func TestFault_CommentsAndBlankLinesIgnored(t *testing.T) {
	nodes := faultTestNodes(1, 2, 3)
	s := GenerateSchedule(prng.New(0x42), faultBusyProfile(), nodes, faultTestHorizon)
	plain := s.String()

	var decorated strings.Builder
	decorated.WriteString("# schedule under test\n")
	decorated.WriteString("\n")
	for _, line := range strings.Split(strings.TrimRight(plain, "\n"), "\n") {
		decorated.WriteString(line)
		decorated.WriteString("\n")
		decorated.WriteString("  # note about the line above\n")
		decorated.WriteString("\n")
	}

	got, err := ParseSchedule(decorated.String())
	if err != nil {
		t.Fatalf("ParseSchedule with comments/blanks: %v", err)
	}
	if got.String() != plain {
		t.Fatalf("comments/blank lines changed the parsed schedule:\n--- want ---\n%s\n--- got ---\n%s", plain, got.String())
	}
}

func TestFault_CommentingOutFaultRemovesExactlyThatFault(t *testing.T) {
	nodes := faultTestNodes(1, 2, 3)
	s := GenerateSchedule(prng.New(0x99), faultBusyProfile(), nodes, faultTestHorizon)
	if len(s.Faults) < 2 {
		t.Fatalf("need at least 2 faults for this test, got %d", len(s.Faults))
	}

	lines := strings.Split(strings.TrimRight(s.String(), "\n"), "\n")
	var removed string
	commentedOut := false
	for i, line := range lines {
		if !commentedOut && strings.HasPrefix(line, "fault ") {
			removed = line
			lines[i] = "# " + line
			commentedOut = true
			break
		}
	}
	if !commentedOut {
		t.Fatalf("no fault line found to comment out")
	}

	got, err := ParseSchedule(strings.Join(lines, "\n"))
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	if len(got.Faults) != len(s.Faults)-1 {
		t.Fatalf("want %d faults after commenting one out, got %d", len(s.Faults)-1, len(got.Faults))
	}
	for _, f := range got.Faults {
		if faultLine(f) == removed {
			t.Fatalf("commented-out fault %q is still present", removed)
		}
	}
}

// faultLine renders f the same way Schedule.String renders a fault line, for
// comparison against a line pulled out of dumped text.
func faultLine(f Fault) string {
	var b strings.Builder
	b.WriteString("fault ")
	b.WriteString(strconv.FormatInt(int64(f.At), 10))
	b.WriteByte(' ')
	b.WriteString(f.Kind.String())
	b.WriteByte(' ')
	b.WriteString(strconv.FormatUint(uint64(f.A), 10))
	b.WriteByte(' ')
	b.WriteString(strconv.FormatUint(uint64(f.B), 10))
	b.WriteByte(' ')
	b.WriteString(strconv.FormatInt(f.Arg, 10))
	return b.String()
}

// --- 5. parse errors -----------------------------------------------------

func TestFault_ParseErrorsNameTheLine(t *testing.T) {
	cases := []struct {
		name string
		text string
		line int
	}{
		{
			name: "unknown kind",
			text: "profile clean\nhorizon 1000\nnodes 1 2\nfault 10 explode 1 0 0\n",
			line: 4,
		},
		{
			name: "non-numeric at",
			text: "profile clean\nhorizon 1000\nnodes 1 2\nfault ten crash 1 0 0\n",
			line: 4,
		},
		{
			// Absence is only detectable once the whole input has been
			// scanned, so the reported line is the last line of the
			// input (including the trailing empty line strings.Split
			// produces for text ending in "\n") rather than a specific
			// offending line.
			name: "missing profile header",
			text: "horizon 1000\nnodes 1 2\n",
			line: 3,
		},
		{
			name: "missing horizon header",
			text: "profile clean\nnodes 1 2\n",
			line: 3,
		},
		{
			name: "truncated fault line",
			text: "profile clean\nhorizon 1000\nnodes 1 2\nfault 10 crash 1\n",
			line: 4,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseSchedule(c.text)
			if err == nil {
				t.Fatalf("want an error, got nil")
			}
			want := "line " + strconv.Itoa(c.line)
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not name %q", err.Error(), want)
			}
		})
	}
}

// --- 6. pairing ----------------------------------------------------------

func TestFault_Pairing(t *testing.T) {
	nodes := faultTestNodes(1, 2, 3, 4, 5)

	for _, p := range Profiles() {
		t.Run(p.Name, func(t *testing.T) {
			for seed := uint64(0); seed < 50; seed++ {
				s := GenerateSchedule(prng.New(seed), p, nodes, faultTestHorizon)
				faultCheckPaired(t, s.Faults, FaultCrash, FaultRestart, func(f Fault) any { return f.A })
				faultCheckPaired(t, s.Faults, FaultPartition, FaultHeal, func(f Fault) any { return Link{From: f.A, To: f.B} })
			}
		})
	}
}

// faultCheckPaired verifies that the faults of kind "open" (e.g. crash) and
// "close" (e.g. restart) grouped by key(f) have equal counts, and that
// matching them by rank within each group — the i-th earliest open with the
// i-th earliest close — always puts the close strictly after the open. This
// does not recover the exact pairing GenerateSchedule produced (the final
// sort discards it), but a valid rank-matching exists if and only if some
// valid matching exists, so this is exactly the property "every open has a
// matching close" reduces to.
func faultCheckPaired(t *testing.T, faults []Fault, open, close_ FaultKind, key func(Fault) any) {
	t.Helper()
	opens := map[any][]node.Time{}
	closes := map[any][]node.Time{}
	for _, f := range faults {
		switch f.Kind {
		case open:
			k := key(f)
			opens[k] = append(opens[k], f.At)
		case close_:
			k := key(f)
			closes[k] = append(closes[k], f.At)
		}
	}
	for k, o := range opens {
		c := closes[k]
		if len(o) != len(c) {
			t.Fatalf("key %v: %d %v faults but %d %v faults", k, len(o), open, len(c), close_)
		}
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
		sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
		for i := range o {
			if !(c[i] > o[i]) {
				t.Fatalf("key %v: rank %d: %v at %d has no strictly later %v (rank-matched close at %d)", k, i, open, o[i], close_, c[i])
			}
		}
	}
	for k := range closes {
		if _, ok := opens[k]; !ok {
			t.Fatalf("key %v: %v faults with no %v faults at all", k, close_, open)
		}
	}
}

// --- 7. bounds -------------------------------------------------------------

func TestFault_Bounds(t *testing.T) {
	nodes := faultTestNodes(1, 2, 3, 4, 5)
	nodeSet := map[node.NodeID]bool{}
	for _, id := range nodes {
		nodeSet[id] = true
	}

	for _, p := range Profiles() {
		t.Run(p.Name, func(t *testing.T) {
			for seed := uint64(0); seed < 50; seed++ {
				s := GenerateSchedule(prng.New(seed), p, nodes, faultTestHorizon)
				for _, f := range s.Faults {
					if f.At < 0 || f.At > node.Time(s.Horizon) {
						t.Fatalf("fault %+v: At out of [0, horizon]", f)
					}
					if f.Kind == FaultPartition || f.Kind == FaultHeal {
						if f.A == f.B {
							t.Fatalf("fault %+v: partition/heal from a node to itself", f)
						}
					}
					if !nodeSet[f.A] {
						t.Fatalf("fault %+v: A not in node set", f)
					}
					if (f.Kind == FaultPartition || f.Kind == FaultHeal) && !nodeSet[f.B] {
						t.Fatalf("fault %+v: B not in node set", f)
					}
					if f.Kind == FaultClockSkew || f.Kind == FaultClockJump {
						if f.Arg < -int64(p.MaxSkew) || f.Arg > int64(p.MaxSkew) {
							t.Fatalf("fault %+v: Arg out of [-MaxSkew, MaxSkew] (MaxSkew=%d)", f, p.MaxSkew)
						}
					}
				}
			}
		})
	}
}

// --- 8. total ordering ---------------------------------------------------

func TestFault_TotalOrdering(t *testing.T) {
	nodes := faultTestNodes(1, 2, 3, 4, 5)
	for _, p := range Profiles() {
		for seed := uint64(0); seed < 50; seed++ {
			s := GenerateSchedule(prng.New(seed), p, nodes, faultTestHorizon)
			if !sort.SliceIsSorted(s.Faults, func(i, j int) bool { return faultLess(s.Faults[i], s.Faults[j]) }) {
				t.Fatalf("profile %s seed %d: Faults is not totally ordered by (At, Kind, A, B, Arg)", p.Name, seed)
			}
		}
	}
}

// --- 9. clean profile ---------------------------------------------------

func TestFault_CleanProfileIsAlwaysEmpty(t *testing.T) {
	nodes := faultTestNodes(1, 2, 3, 4, 5)
	var clean Profile
	for _, p := range Profiles() {
		if p.Name == "clean" {
			clean = p
		}
	}
	if clean.Name != "clean" {
		t.Fatalf("no clean profile in catalogue")
	}

	horizons := []node.Duration{0, node.Second, faultTestHorizon, 3600 * node.Second}
	for _, h := range horizons {
		for seed := uint64(0); seed < 10; seed++ {
			s := GenerateSchedule(prng.New(seed), clean, nodes, h)
			if len(s.Faults) != 0 {
				t.Fatalf("horizon %d seed %d: clean profile produced %d faults", h, seed, len(s.Faults))
			}
		}
	}
}

// --- 10. Profiles returns a copy -----------------------------------------

func TestFault_ProfilesReturnsACopy(t *testing.T) {
	first := Profiles()
	first[0].Name = "tampered"
	first[0].CrashRate = 999

	second := Profiles()
	if second[0].Name == "tampered" || second[0].CrashRate == 999 {
		t.Fatalf("mutating a Profiles() result affected a later call")
	}
}

// --- 11. rate scaling ------------------------------------------------------

func TestFault_RateScalingWithHorizon(t *testing.T) {
	nodes := faultTestNodes(1, 2, 3, 4, 5)
	p := faultBusyProfile()
	const seeds = 40

	var short, long int
	for seed := uint64(0); seed < seeds; seed++ {
		s1 := GenerateSchedule(prng.New(seed), p, nodes, faultTestHorizon)
		s2 := GenerateSchedule(prng.New(seed), p, nodes, 2*faultTestHorizon)
		short += faultCount(s1.Faults, FaultCrash)
		long += faultCount(s2.Faults, FaultCrash)
	}

	if short == 0 {
		t.Fatalf("no crash faults drawn at all; cannot assess scaling")
	}
	ratio := float64(long) / float64(short)
	if ratio < 1.5 || ratio > 2.5 {
		t.Fatalf("doubling the horizon should roughly double the fault count; got ratio %.2f (short=%d, long=%d)", ratio, short, long)
	}
}

func faultCount(faults []Fault, kind FaultKind) int {
	n := 0
	for _, f := range faults {
		if f.Kind == kind {
			n++
		}
	}
	return n
}
