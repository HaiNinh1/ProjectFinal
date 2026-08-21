// Package policy holds machine-enforced project invariants that no amount of
// code review reliably catches by hand.
package policy_test

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// pureNodePackages must stay free of I/O, wall clocks, concurrency, and
// ambient randomness. They implement node.Node and must be deterministic
// functions of their inputs. See docs/SPEC.md, INV-1 through INV-4.
//
// Packages listed here that do not exist yet are skipped, so this test is
// written once and bites as each package lands.
var pureNodePackages = []string{
	"verity/node",
	"verity/prng",
	"verity/raft",
	"verity/kvsm",
	"verity/shard",
}

// bannedImports maps a forbidden direct import to the reason, which is printed
// on failure so the fix is obvious without opening the spec.
//
// Only DIRECT imports are checked. Transitive dependencies are irrelevant here
// and checking them would be meaningless: fmt itself imports os and sync.
var bannedImports = map[string]string{
	"time":         "use node.Time and node.Duration; the runtime supplies now via Step (INV-2)",
	"os":           "nodes perform no I/O; return a node.Persist action instead (INV-1)",
	"os/exec":      "nodes perform no I/O (INV-1)",
	"net":          "nodes perform no I/O; return a node.Send action instead (INV-1)",
	"net/http":     "nodes perform no I/O; return a node.Send action instead (INV-1)",
	"bufio":        "nodes perform no I/O (INV-1)",
	"io/ioutil":    "nodes perform no I/O (INV-1)",
	"log":          "nodes emit no side effects; the runtime traces on their behalf (INV-1)",
	"math/rand":    "use verity/prng seeded from the run seed and injected at construction (INV-4)",
	"math/rand/v2": "use verity/prng seeded from the run seed and injected at construction (INV-4)",
	"crypto/rand":  "nondeterministic; use verity/prng (INV-4)",
	"sync":         "nodes are single-threaded state machines; the runtime owns concurrency (INV-3)",
	"sync/atomic":  "nodes are single-threaded state machines (INV-3)",
	"context":      "nodes never block, so there is nothing to cancel (INV-3)",
	"runtime":      "leaks scheduling and GC nondeterminism into node behaviour (INV-3)",
}

// TestPureNodePackagesHaveNoForbiddenImports is the primary determinism guard.
// It is cheap, it runs on every commit, and it is the difference between
// determinism holding and determinism silently rotting.
func TestPureNodePackagesHaveNoForbiddenImports(t *testing.T) {
	for _, pkg := range pureNodePackages {
		imports, ok := directImports(t, pkg)
		if !ok {
			t.Logf("%s: not created yet, skipping", pkg)
			continue
		}
		for _, imp := range imports {
			if reason, banned := bannedImports[imp]; banned {
				t.Errorf("%s imports %q\n  reason: %s", pkg, imp, reason)
			}
		}
	}
}

// TestSimulatorIsNotImportedByProductionCode keeps the simulator a test-time
// dependency. If a node package ever imports sim, the node has learned
// something about its runtime and the real deployment no longer runs the same
// code the simulation validated.
func TestSimulatorIsNotImportedByProductionCode(t *testing.T) {
	for _, pkg := range pureNodePackages {
		imports, ok := directImports(t, pkg)
		if !ok {
			continue
		}
		for _, imp := range imports {
			if imp == "verity/sim" || strings.HasPrefix(imp, "verity/sim/") {
				t.Errorf("%s imports %q: nodes must not know which runtime they are under (INV-5)", pkg, imp)
			}
		}
	}
}

// directImports returns the non-test imports declared by pkg. The second
// result is false when the package does not exist yet.
func directImports(t *testing.T, pkg string) ([]string, bool) {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", "{{range .Imports}}{{.}}\n{{end}}", pkg).Output()
	if err != nil {
		return nil, false
	}
	var imports []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			imports = append(imports, line)
		}
	}
	sort.Strings(imports)
	return imports, true
}
