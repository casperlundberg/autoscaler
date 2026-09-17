package arch

import (
	"go/build"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const module = "github.com/casperlundberg/autoscaler/"

// mayImport is the layering, written out. A package may import the packages
// listed for it and no others.
//
// `api → controller → {policy, platform} → domain`, with `config` beneath
// `policy`, and dependencies pointing downward only. app is the composition
// root, so it is the one package allowed to see everything — that is what a
// composition root is for, and keeping it the only one is what makes the set
// of platforms a visible fact about the binary.
var mayImport = map[string][]string{
	"cmd/autoscaler": {"internal/app"},

	"internal/domain": {},
	"internal/secret": {},

	// Which code this process is. Imports nothing, so anything may report it.
	"internal/buildinfo": {},
	"internal/config":    {"internal/domain"},

	// This package. It reads the graph and imports none of it, which is the
	// only way a layering test cannot quietly exempt itself.
	"internal/arch": {},

	// Pure functions over the vocabulary and the settings. No platform, no
	// registry, no I/O.
	"internal/policy": {"internal/config", "internal/domain"},

	"internal/platform": {"internal/domain", "internal/secret"},

	// Adapters may build on one another — a ColonyOS executor running as a pod
	// is a Kubernetes Deployment of a ColonyOS executor — but none of them may
	// see the policy that decides how many to run.
	"internal/platform/kubernetes":          {"internal/domain", "internal/platform"},
	"internal/platform/kubernetes/kubetest": {},
	"internal/platform/colonyos":            {"internal/domain", "internal/platform"},
	"internal/platform/colonyos/colonytest": {"internal/platform/colonyos"},
	"internal/platform/colonypods":          {"internal/domain", "internal/platform", "internal/platform/colonyos", "internal/platform/kubernetes"},
	"internal/platform/colonycontainers":    {"internal/domain", "internal/platform", "internal/platform/colonyos"},
	"internal/platform/simulation":          {"internal/domain", "internal/platform"},

	"internal/registry": {"internal/config", "internal/domain", "internal/platform", "internal/secret"},

	// The one place policy and platform meet.
	"internal/controller": {"internal/config", "internal/domain", "internal/platform", "internal/policy", "internal/registry"},

	"internal/api": {"internal/buildinfo", "internal/config", "internal/controller", "internal/domain", "internal/platform", "internal/registry"},

	"internal/app": nil, // the composition root sees everything
}

// ours maps every package directory to the packages of ours that it imports.
func ours(t *testing.T) map[string][]string {
	t.Helper()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("locating the module root: %v", err)
	}

	graph := map[string][]string{}
	for _, tree := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry os.DirEntry, err error) error {
			if err != nil || !entry.IsDir() {
				return err
			}
			pkg, err := build.ImportDir(path, 0)
			if err != nil {
				// A directory with no buildable Go files is not a package.
				// internal/arch itself is one of those.
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			var imports []string
			for _, imported := range pkg.Imports {
				if after, found := strings.CutPrefix(imported, module); found {
					imports = append(imports, after)
				}
			}
			sort.Strings(imports)
			graph[filepath.ToSlash(rel)] = imports
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", tree, err)
		}
	}
	if len(graph) == 0 {
		t.Fatal("found no packages — this test is not looking where it thinks it is")
	}
	return graph
}

// Every package's imports are checked against the list written above, so a new
// dependency that crosses a layer fails here with the rule it broke, rather
// than being discovered later as a package that cannot be tested without
// standing up a platform.
func TestEveryPackageImportsOnlyWhatItsLayerAllows(t *testing.T) {
	graph := ours(t)

	for pkg, imports := range graph {
		allowed, known := mayImport[pkg]
		if !known {
			t.Errorf("package %s is not in the layering table: add it with the "+
				"packages it may import, or the rules do not cover it", pkg)
			continue
		}
		if allowed == nil {
			continue // the composition root
		}
		for _, imported := range imports {
			if !contains(allowed, imported) {
				t.Errorf("%s imports %s, which its layer does not allow.\n"+
					"  allowed: %v\n"+
					"  dependencies point downward only; if this import is right, "+
					"the layering has changed and the table and the docs have to change with it",
					pkg, imported, allowed)
			}
		}
	}

	// A package listed in the table that no longer exists means the table is
	// describing an architecture the code has moved on from.
	for pkg := range mayImport {
		if _, found := graph[pkg]; !found {
			t.Errorf("the layering table lists %s, which is not a package any more", pkg)
		}
	}
}

// The vocabulary sits at the bottom. If domain imports anything of ours, there
// is a cycle waiting to happen and the bottom of the stack has become a middle
// of it.
func TestTheDomainImportsNothingOfOurs(t *testing.T) {
	if imports := ours(t)["internal/domain"]; len(imports) > 0 {
		t.Errorf("internal/domain imports %v, want nothing of ours", imports)
	}
}

// The invariant that pays for itself: policy knows nothing about pods,
// containers or ColonyOS executors, and no adapter knows how a decision is
// reached. It is what makes a simulation run evidence about the real engine
// rather than about a model of it — the substitution happens at one point, and
// it can only stay at one point while this holds.
func TestPolicyAndThePlatformsNeverSeeEachOther(t *testing.T) {
	graph := ours(t)

	for _, imported := range graph["internal/policy"] {
		if strings.HasPrefix(imported, "internal/platform") {
			t.Errorf("internal/policy imports %s: the decision engine has learned "+
				"what a platform is, and a simulated run no longer proves anything "+
				"about a real one", imported)
		}
	}

	for pkg, imports := range graph {
		if !strings.HasPrefix(pkg, "internal/platform") {
			continue
		}
		if contains(imports, "internal/policy") {
			t.Errorf("%s imports internal/policy: an adapter has learned how "+
				"decisions are made, and can now disagree with the engine", pkg)
		}
	}
}

// And the other half of that rule: somewhere has to put the two together, and
// exactly one package may. Anything that seems to need both belongs in the
// controller, which is the statement this test turns into a check.
func TestTheControllerIsTheOnlyPlaceTheTwoMeet(t *testing.T) {
	var both []string
	for pkg, imports := range ours(t) {
		if pkg == "internal/app" {
			continue // the composition root wires them together by definition
		}
		platform := false
		for _, imported := range imports {
			if strings.HasPrefix(imported, "internal/platform") {
				platform = true
			}
		}
		if platform && contains(imports, "internal/policy") {
			both = append(both, pkg)
		}
	}
	sort.Strings(both)

	if len(both) != 1 || both[0] != "internal/controller" {
		t.Errorf("packages importing both policy and a platform: %v, want only "+
			"internal/controller", both)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
