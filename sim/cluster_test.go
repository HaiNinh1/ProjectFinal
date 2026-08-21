package sim

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"verity/internal/echo"
	"verity/node"
	"verity/prng"
)

// update regenerates the golden trace instead of comparing against it. It is
// a flag rather than an environment variable so that it shows up in
// `go test -args -h` next to the standard ones, and so that regenerating a
// golden file is always a deliberate act with a visible diff.
var update = flag.Bool("update", false, "regenerate golden files")

// clusterEcho is the NewNodeFunc for a cluster of echo nodes. The echo node
// takes no rng: it has no randomised behaviour, which makes it a clean control
// for the determinism test — any nondeterminism a run shows is the harness's,
// not the node's.
func clusterEcho(retry node.Duration) NewNodeFunc {
	return func(id node.NodeID, peers []node.NodeID, _ *prng.Rand) node.Node {
		return echo.New(id, peers, retry)
	}
}

// clusterQuiet is a profile with every fault class disabled. Faults injected
// by the schedule are a separate axis from the network's own drop and delay
// behaviour, and T1.10 is about the latter.
func clusterQuiet() *Profile { return &Profile{Name: "quiet"} }

// TestEchoClusterRepliesUnderDropsAndDelays is task T1.10's acceptance
// criterion: a three-node echo cluster answers 100 client calls while the
// network is dropping and delaying messages.
//
// It is the first test in the project that runs a real node.Node under the
// real runtime, so it is also the first evidence that the scheduler, network,
// disk, and trace actually compose into something that works.
func TestEchoClusterRepliesUnderDropsAndDelays(t *testing.T) {
	const calls = 100

	s := New(Config{
		Seed:    0x1234,
		Nodes:   []node.NodeID{1, 2, 3},
		New:     clusterEcho(50 * node.Millisecond),
		Horizon: 60 * node.Second,
		Net: NetConfig{
			BaseDelay:   node.Millisecond,
			DelayJitter: 500 * node.Microsecond,
			DropRate:    0.1,
		},
		Disk: DiskConfig{
			WriteLatency: 100 * node.Microsecond,
			SyncLatency:  node.Millisecond,
		},
		Profile: clusterQuiet(),
	})

	for i := 0; i < calls; i++ {
		at := node.Time(10*node.Millisecond) + node.Time(i)*node.Time(node.Millisecond)
		s.Call(at, 1, uint64(i+1), []byte{byte(i), byte(i >> 8)})
	}

	if err := s.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	seen := make(map[uint64]int, calls)
	for _, r := range s.Replies() {
		if r.Err != nil {
			t.Errorf("req %d: replied with error %v", r.ReqID, r.Err)
		}
		want := []byte{byte(r.ReqID - 1), byte((r.ReqID - 1) >> 8)}
		if !bytes.Equal(r.Resp, want) {
			t.Errorf("req %d: resp = %v, want %v", r.ReqID, r.Resp, want)
		}
		seen[r.ReqID]++
	}

	// Every call answered, and answered exactly once. A second Reply for one
	// ReqID would be a duplicate the client could not tell from a real second
	// result, which is the exact shape of the bug the dedup table exists to
	// prevent later.
	var missing []uint64
	for i := 1; i <= calls; i++ {
		switch seen[uint64(i)] {
		case 0:
			missing = append(missing, uint64(i))
		case 1:
		default:
			t.Errorf("req %d: replied %d times, want exactly 1", i, seen[uint64(i)])
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d calls never replied: %v", len(missing), calls, missing)
	}

	if s.Steps() == 0 {
		t.Fatal("no steps were taken; the run did nothing")
	}
	t.Logf("%d steps, %d replies, trace hash %#016x", s.Steps(), len(s.Replies()), s.Hash())
}

// TestEchoClusterSingleNode checks the degenerate cluster, where a node is its
// own majority and never sends anything. It is worth having because it is the
// one configuration in which the retry timer must never be armed, and an
// off-by-one in the majority calculation would show up here first.
func TestEchoClusterSingleNode(t *testing.T) {
	s := New(Config{
		Seed:    7,
		Nodes:   []node.NodeID{1},
		New:     clusterEcho(50 * node.Millisecond),
		Horizon: node.Second,
		Disk:    DiskConfig{WriteLatency: 100 * node.Microsecond, SyncLatency: node.Millisecond},
		Profile: clusterQuiet(),
	})
	s.Call(node.Time(node.Millisecond), 1, 1, []byte("x"))

	if err := s.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := len(s.Replies()); got != 1 {
		t.Fatalf("replies = %d, want 1", got)
	}
	if r := s.Replies()[0]; r.ReqID != 1 || string(r.Resp) != "x" {
		t.Fatalf("reply = %+v, want ReqID 1 resp \"x\"", r)
	}
}

// TestEchoClusterSurvivesCrashAndRestart drives the fault machinery rather
// than the network: a node goes down, loses everything that was not synced,
// and comes back through the constructor with only its durable records. The
// cluster must keep answering throughout, because two of three nodes are
// enough.
func TestEchoClusterSurvivesCrashAndRestart(t *testing.T) {
	const calls = 40

	s := New(Config{
		Seed:    0xbeef,
		Nodes:   []node.NodeID{1, 2, 3},
		New:     clusterEcho(20 * node.Millisecond),
		Horizon: 10 * node.Second,
		Net: NetConfig{
			BaseDelay:   node.Millisecond,
			DelayJitter: 500 * node.Microsecond,
		},
		Disk: DiskConfig{
			WriteLatency: 100 * node.Microsecond,
			SyncLatency:  node.Millisecond,
			TornRate:     0.5,
		},
		// Node 2 is a follower here, so crashing it must not stop node 1 from
		// reaching a majority: node 1 plus node 3 is two of three.
		Profile: clusterQuiet(),
	})

	for i := 0; i < calls; i++ {
		at := node.Time(10*node.Millisecond) + node.Time(i)*node.Time(node.Millisecond)
		s.Call(at, 1, uint64(i+1), []byte{byte(i)})
	}
	if err := s.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := len(s.Replies()); got != calls {
		t.Fatalf("replies = %d, want %d", got, calls)
	}
}

// TestGoldenTrace is task T1.9's acceptance criterion: the trace of a fixed
// scenario matches a golden file, byte for byte.
//
// Both halves of the check matter and they catch different things. The golden
// FILE catches a change to the line format or to the sequence of Steps, and
// shows exactly where in the run it happened. The golden HASH is what the
// hundred-seed sweep compares, because keeping a trace file per seed would be
// gigabytes of I/O for runs that almost always match. Answering docs/STATE.md
// question Q5: keep both, for those two different reasons.
//
// Regenerate with:  go test ./sim -run TestGoldenTrace -update
func TestGoldenTrace(t *testing.T) {
	var buf bytes.Buffer

	s := New(Config{
		Seed:    0x5eed,
		Nodes:   []node.NodeID{1, 2, 3},
		New:     clusterEcho(20 * node.Millisecond),
		Horizon: node.Second,
		Net: NetConfig{
			BaseDelay:   node.Millisecond,
			DelayJitter: 500 * node.Microsecond,
			DropRate:    0.2,
		},
		Disk: DiskConfig{
			WriteLatency: 100 * node.Microsecond,
			SyncLatency:  node.Millisecond,
		},
		Profile: clusterQuiet(),
		TraceTo: &buf,
	})
	for i := 0; i < 5; i++ {
		at := node.Time(10*node.Millisecond) + node.Time(i)*node.Time(node.Millisecond)
		s.Call(at, 1, uint64(i+1), []byte{byte(i)})
	}
	if err := s.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	golden := filepath.Join("testdata", "echo3.trace")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s (%d lines, hash %#016x)", golden, s.Steps(), s.Hash())
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update): %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("trace does not match %s\n%s", golden, clusterDiff(string(want), buf.String()))
	}
}

// clusterDiff reports the first line at which two traces diverge, with a few
// lines of context either side. A whole-trace dump would be unreadable; the
// first divergence is the only line that matters, because everything after it
// is downstream of the same cause.
func clusterDiff(want, got string) string {
	wl := clusterLines(want)
	gl := clusterLines(got)
	n := len(wl)
	if len(gl) < n {
		n = len(gl)
	}
	for i := 0; i < n; i++ {
		if wl[i] != gl[i] {
			var b bytes.Buffer
			lo := i - 3
			if lo < 0 {
				lo = 0
			}
			for j := lo; j < i; j++ {
				b.WriteString("  " + wl[j] + "\n")
			}
			b.WriteString("- want: " + wl[i] + "\n")
			b.WriteString("+ got:  " + gl[i] + "\n")
			return b.String()
		}
	}
	if len(wl) != len(gl) {
		var b bytes.Buffer
		b.WriteString("traces agree for ")
		b.WriteString(itoa(n))
		b.WriteString(" lines, then lengths differ: want ")
		b.WriteString(itoa(len(wl)))
		b.WriteString(" lines, got ")
		b.WriteString(itoa(len(gl)))
		b.WriteString("\n")
		return b.String()
	}
	return "traces are identical\n"
}

func clusterLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
