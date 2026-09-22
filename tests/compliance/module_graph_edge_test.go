package compliance

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// forbiddenSkillsModule is the Go module path HelixAgent MUST NEVER depend
// on (HXC-159 T-P6.05, decision D-4). Declared as data and matched against
// REAL `go mod graph` output, never grepped from go.mod text — a `replace`
// directive, an indirect pseudo-version, or a nested module's own
// requirement could all put an edge in the resolved build graph that a
// plain-text grep of this module's own go.mod would never see.
//
// D-4 (spec.md, plan.md §6 "the dependency-edge inversion"): upstream's own
// helix-deps.yaml declares HelixAgent as a dependency OF HelixSkills
// ("multi-provider LLM + embeddings client used by the skill-graph
// service"). HelixAgent's own governance MUST enforce the mirror-image
// rule so that edge never inverts back: HelixAgent is a CLIENT of a skills
// source — via services.ToolRegistry.RegisterExternalToolSource (T-P6.01)
// or the MCP config path (T-P6.02) — NEVER a Go importer of
// github.com/HelixDevelopment/skills. Inverting the edge would create the
// two-way dependency cycle CONST-051(C) forbids.
const forbiddenSkillsModule = "github.com/HelixDevelopment/skills"

// TestModuleGraphHasNoEdgeToSkillsModule is the D-4 enforcement gate
// (T-P6.05 / RS-11). It runs the real `go mod graph` for dev.helix.agent —
// the actual resolved dependency graph the Go toolchain builds from, direct
// AND transitive — and asserts it contains ZERO edges naming the skills
// module. A future accidental `import "github.com/HelixDevelopment/skills"`
// (or a transitive pull-in through some other dependency) fails this test
// immediately rather than silently reintroducing the forbidden cycle.
func TestModuleGraphHasNoEdgeToSkillsModule(t *testing.T) {
	moduleDir := helixAgentModuleRoot(t)

	cmd := exec.Command("go", "mod", "graph")
	cmd.Dir = moduleDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("`go mod graph` failed in %s: %v\n%s", moduleDir, err, out)
	}

	offending := scanGraphForModule(string(out), forbiddenSkillsModule)

	t.Logf("COMPLIANCE: scanned real `go mod graph` output (%d bytes, module root %s) for edges to %s",
		len(out), moduleDir, forbiddenSkillsModule)

	if len(offending) > 0 {
		t.Errorf("D-4 VIOLATION: dev.helix.agent's module graph contains %d edge(s) to %s "+
			"(HelixAgent must be a CLIENT of the skills library — RegisterExternalToolSource "+
			"or MCP config — NEVER a Go importer; see D-4 in spec.md):\n%s",
			len(offending), forbiddenSkillsModule, strings.Join(offending, "\n"))
	} else {
		t.Logf("COMPLIANCE: D-4 edge direction preserved — no dependency edge on %s", forbiddenSkillsModule)
	}
}

// TestModuleGraphEdgeDetector_CatchesPlantedEdge is the T-P6.05.2 paired
// mutation: it proves scanGraphForModule (the SAME detection logic
// TestModuleGraphHasNoEdgeToSkillsModule uses against the real graph) is
// not a no-op / always-pass. Rather than mutate the real go.mod of a live
// module (risky, slow, and would leave a genuinely-broken build behind if
// anything went wrong mid-test), this plants the forbidden edge into a
// synthetic `go mod graph`-shaped fixture — the "add a require on the
// skills module to a test fixture" the task describes — and asserts the
// SAME code path that gates real changes reports it. If this test ever
// starts passing with an empty offending-list, the detector itself is
// broken and the primary gate above is worthless.
func TestModuleGraphEdgeDetector_CatchesPlantedEdge(t *testing.T) {
	// A minimal but realistic `go mod graph` fixture: dev.helix.agent
	// depending on a handful of real-shaped modules, PLUS one planted line
	// requiring the forbidden skills module — exactly the shape a
	// mistaken `import "github.com/HelixDevelopment/skills/pkg/skills"`
	// would produce in the real graph.
	plantedFixture := strings.Join([]string{
		"dev.helix.agent github.com/gin-gonic/gin@v1.12.0",
		"dev.helix.agent github.com/sirupsen/logrus@v1.9.3",
		"dev.helix.agent github.com/HelixDevelopment/skills@v0.0.0-20260101000000-abcdef123456",
		"github.com/HelixDevelopment/skills@v0.0.0-20260101000000-abcdef123456 github.com/BurntSushi/toml@v1.3.2",
	}, "\n")

	offending := scanGraphForModule(plantedFixture, forbiddenSkillsModule)
	if len(offending) == 0 {
		t.Fatal("PAIRED-MUTATION FAILURE: scanGraphForModule did not catch a planted edge to " +
			forbiddenSkillsModule + " — the D-4 gate would be a bluff gate that never fails")
	}
	t.Logf("COMPLIANCE: detector correctly caught %d planted edge(s): %v", len(offending), offending)

	// Golden-good half of the same self-validation (§11.4.107(10)): a
	// fixture with NO forbidden edge must report clean, so the detector
	// is proven to discriminate rather than always-flag.
	cleanFixture := strings.Join([]string{
		"dev.helix.agent github.com/gin-gonic/gin@v1.12.0",
		"dev.helix.agent github.com/sirupsen/logrus@v1.9.3",
	}, "\n")
	clean := scanGraphForModule(cleanFixture, forbiddenSkillsModule)
	if len(clean) != 0 {
		t.Fatalf("PAIRED-MUTATION FAILURE: scanGraphForModule flagged a clean fixture as offending: %v — "+
			"the detector is over-broad, not just checking for %s", clean, forbiddenSkillsModule)
	}
}

// scanGraphForModule returns every line of `go mod graph`-shaped output
// (space-separated "<module>@<version> <dependency>@<version>" edges) that
// names the given forbidden module on either side of the edge. Shared by
// the real-graph gate and its paired-mutation self-validation so both
// exercise identical detection logic.
func scanGraphForModule(graphOutput, forbidden string) []string {
	var offending []string
	scanner := bufio.NewScanner(strings.NewReader(graphOutput))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, forbidden) {
			offending = append(offending, line)
		}
	}
	return offending
}

// helixAgentModuleRoot locates THIS module's own root (the directory
// holding go.mod) by walking up from this test file's location — never a
// hardcoded path (CONST-051(B): HelixAgent stays project-not-aware, and
// this specifically must resolve to HelixAgent's OWN module root, not the
// consuming project's — unlike resolveModulePath/getRoot in
// module_compliance_test.go, which resolve the CONSUMING project's root).
func helixAgentModuleRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file location via runtime.Caller")
	}
	dir := filepath.Dir(filename)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate dev.helix.agent's go.mod walking up from " + filename)
	return ""
}
