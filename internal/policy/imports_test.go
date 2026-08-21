// Package policy holds machine-enforced project invariants that no amount of
// code review reliably catches by hand.
package policy_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	// The echo node is a test fixture, but it is the node the determinism test
	// actually drives, so it must obey the same rules as the real ones. A
	// fixture that could read a clock would prove nothing about the harness.
	"verity/internal/echo",
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

// moduleRoot locates the directory containing go.mod by walking up from the
// working directory using os.Stat, so the walk itself is a filesystem access
// the test process performs directly (see directImports for why that
// matters) rather than something buried in a subprocess.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod above %s", dir)
		}
		dir = parent
	}
}

// directImports returns the sorted, deduplicated non-test imports declared by
// pkg's .go files. The second result is false when the package does not
// exist yet, or exists only as an empty directory with no non-test .go files
// in it — an empty leftover directory is not a package.
//
// This parses source directly with go/parser instead of shelling out to
// `go list`, and that is not a style choice, it is a fix for a real false
// pass. go test's result cache invalidates based on which files the test
// process itself opens, tracked via the testlog hooks on os.Open/os.Stat/
// os.ReadDir. It has no visibility into what a subprocess reads. directImports
// used to run `go list -f ... <pkg>` via os/exec, so when a brand-new package
// appeared in the tree the cache had no idea the subprocess's answer had
// changed, and kept reusing the old result. That happened for real: after
// creating raft/, `go test ./internal/policy -v` printed
// "verity/raft: not created yet, skipping" and PASS, reported (cached), even
// though verity/raft existed and compiled by then. A guard that can silently
// not run is worse than no guard, because it still reports green. Parsing the
// files ourselves means every file this decision depends on is opened by the
// test binary directly, so the cache invalidates whenever a package's
// contents change. Do not "simplify" this back to go list.
func directImports(t *testing.T, pkg string) ([]string, bool) {
	t.Helper()
	rel := strings.TrimPrefix(strings.TrimPrefix(pkg, "verity"), "/")
	dir := filepath.Join(moduleRoot(t), filepath.FromSlash(rel))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}

	fset := token.NewFileSet()
	seen := map[string]bool{}
	var imports []string
	found := false
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		found = true
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, spec := range file.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("parsing %s: unquoting import %s: %v", path, spec.Path.Value, err)
			}
			if !seen[imp] {
				seen[imp] = true
				imports = append(imports, imp)
			}
		}
	}
	if !found {
		return nil, false
	}
	sort.Strings(imports)
	return imports, true
}
