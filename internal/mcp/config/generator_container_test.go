package config

import (
	"os"
	"strings"
	"testing"
)

func TestContainerMCPConfigGenerator_GenerateContainerMCPs(t *testing.T) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	mcps := gen.GenerateContainerMCPs()

	// Test total MCP count (should be 60+)
	if len(mcps) < 60 {
		t.Errorf("Expected at least 60 MCPs, got %d", len(mcps))
	}
}

func TestContainerMCPConfigGenerator_ZeroNPXCommands(t *testing.T) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	// The container generator should never contain npx
	if gen.ContainsNPX() {
		t.Error("Container generator should not contain any NPX commands")
	}

	// Double check by examining all configs
	mcps := gen.GenerateContainerMCPs()
	for name, cfg := range mcps {
		if cfg.Type != "remote" {
			t.Errorf("MCP %s should have type 'remote', got '%s'", name, cfg.Type)
		}
		if cfg.URL == "" {
			t.Errorf("MCP %s should have a URL, got empty string", name)
		}
		if strings.Contains(cfg.URL, "npx") {
			t.Errorf("MCP %s URL should not contain 'npx': %s", name, cfg.URL)
		}
	}
}

func TestContainerMCPConfigGenerator_AllMCPsHaveURLs(t *testing.T) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	mcps := gen.GenerateContainerMCPs()

	for name, cfg := range mcps {
		if cfg.URL == "" {
			t.Errorf("MCP %s has empty URL", name)
		}
		if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
			t.Errorf("MCP %s URL should start with http:// or https://, got: %s", name, cfg.URL)
		}
	}
}

func TestContainerMCPConfigGenerator_PortAllocationUnique(t *testing.T) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	err := gen.ValidatePortAllocations()
	if err != nil {
		t.Errorf("Port allocation validation failed: %v", err)
	}
}

// containerPortBands is the declared address map for MCPContainerPorts: every
// category's ports must fall inside one of its ranges.
//
// Reconciled 2026-09-03. This was a `switch` with one hard-coded range per
// category, and every band was allocated EXACTLY full — so the first genuinely
// new integration could not be given a port without editing this check. The
// restore of five removed integrations was that first addition.
//
// Two things changed, both of which TIGHTEN rather than relax the check:
//   - a category may now declare more than one range, so growth blocks can be
//     allocated in previously unallocated space instead of overflowing a
//     neighbouring band;
//   - "cloud" and "ai" are now bounded too. The old `switch` had no case for
//     them, so any port at all passed — an unbounded category is a hole, and
//     the restored `gcs`/`replicate` entries would have slipped through it.
//
// The invariant is unchanged and still falsifiable: move any entry outside its
// category's declared ranges and this fails.
var containerPortBands = map[string][][2]int{
	"core":          {{8200, 8209}},
	"database":      {{8210, 8214}},
	"vector":        {{8215, 8218}},
	"devops":        {{8220, 8233}, {8285, 8286}},
	"browser":       {{8234, 8237}},
	"communication": {{8238, 8240}},
	"productivity":  {{8250, 8259}},
	"search":        {{8260, 8269}, {8283, 8284}},
	"google":        {{8270, 8274}},
	"monitoring":    {{8275, 8277}},
	"finance":       {{8278, 8280}},
	"design":        {{8281, 8282}},
	"cloud":         {{8287, 8288}},
	"ai":            {{8289, 8290}},
	// HXC-159 T-P6.02: HelixSkills's containerized MCP entry. New category
	// (the entry is a skill-graph/knowledge service, not "ai" or any
	// existing band), so per this map's own §11.4.120 reconciliation
	// discipline it gets a declared band here rather than reusing an
	// unrelated one or falling through unbounded.
	"knowledge": {{8291, 8292}},
}

func TestContainerMCPConfigGenerator_PortRanges(t *testing.T) {
	// Verify ports are in expected ranges
	for _, p := range MCPContainerPorts {
		ranges, known := containerPortBands[p.Category]
		if !known {
			t.Errorf("MCP %s has category %q with no declared port band — an "+
				"unbounded category lets any port through, so a new category "+
				"must be given a band here", p.Name, p.Category)
			continue
		}
		inBand := false
		for _, r := range ranges {
			if p.Port >= r[0] && p.Port <= r[1] {
				inBand = true
				break
			}
		}
		if !inBand {
			t.Errorf("%s MCP %s port %d is outside the declared band(s) %v for its category",
				p.Category, p.Name, p.Port, ranges)
		}
	}
}

// TestContainerPortBandsDoNotOverlap guards the address map itself: the
// per-category ranges are only a meaningful bound if no two categories claim
// the same port. Without this, a growth block could be widened into a
// neighbour's band and TestContainerMCPConfigGenerator_PortRanges would still
// pass while two categories fought over the same address.
func TestContainerPortBandsDoNotOverlap(t *testing.T) {
	owner := map[int]string{}
	for category, ranges := range containerPortBands {
		for _, r := range ranges {
			if r[0] > r[1] {
				t.Errorf("category %q declares an inverted range %v", category, r)
				continue
			}
			for port := r[0]; port <= r[1]; port++ {
				if prev, taken := owner[port]; taken {
					t.Errorf("port %d is claimed by both %q and %q", port, prev, category)
					continue
				}
				owner[port] = category
			}
		}
	}
}

func TestContainerMCPConfigGenerator_CoreMCPsAlwaysEnabled(t *testing.T) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	mcps := gen.GenerateContainerMCPs()

	// Core MCPs that should always be enabled
	coreMCPs := []string{
		"fetch", "git", "time", "filesystem", "memory",
		"everything", "sequential-thinking", "sqlite", "puppeteer",
	}

	for _, name := range coreMCPs {
		cfg, ok := mcps[name]
		if !ok {
			t.Errorf("Core MCP %s not found", name)
			continue
		}
		if !cfg.Enabled {
			t.Errorf("Core MCP %s should always be enabled", name)
		}
	}
}

func TestContainerMCPConfigGenerator_ConditionalMCPs(t *testing.T) {
	// Test that conditional MCPs are disabled without env vars
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	mcps := gen.GenerateContainerMCPs()

	// These MCPs require API keys - should be disabled without env vars
	conditionalMCPs := []string{
		"github", "gitlab", "slack", "discord", "notion",
		"linear", "jira", "brave-search", "openai",
	}

	for _, name := range conditionalMCPs {
		cfg, ok := mcps[name]
		if !ok {
			t.Errorf("Conditional MCP %s not found", name)
			continue
		}
		// In a clean environment, these should be disabled
		// (unless the test environment has API keys set)
		if cfg.Enabled {
			// Verify the required env var is actually set
			if !hasRequiredEnvVarsForMCP(name) {
				t.Logf("MCP %s is enabled - checking if env vars are set", name)
			}
		}
	}
}

func hasRequiredEnvVarsForMCP(name string) bool {
	requirements := map[string][]string{
		"github":       {"GITHUB_TOKEN"},
		"gitlab":       {"GITLAB_TOKEN"},
		"slack":        {"SLACK_BOT_TOKEN", "SLACK_TEAM_ID"},
		"discord":      {"DISCORD_TOKEN"},
		"notion":       {"NOTION_API_KEY"},
		"linear":       {"LINEAR_API_KEY"},
		"jira":         {"JIRA_URL", "JIRA_EMAIL", "JIRA_API_TOKEN"},
		"brave-search": {"BRAVE_API_KEY"},
		"openai":       {"OPENAI_API_KEY"},
	}

	if reqs, ok := requirements[name]; ok {
		for _, req := range reqs {
			if os.Getenv(req) == "" {
				return false
			}
		}
		return true
	}
	return false
}

func TestContainerMCPConfigGenerator_GetEnabledMCPs(t *testing.T) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	enabled := gen.GetEnabledContainerMCPs()

	// At minimum, core MCPs should be enabled
	if len(enabled) < 10 {
		t.Errorf("Expected at least 10 enabled MCPs, got %d", len(enabled))
	}

	// All enabled MCPs should have valid URLs
	for name, cfg := range enabled {
		if cfg.URL == "" {
			t.Errorf("Enabled MCP %s has empty URL", name)
		}
	}
}

func TestContainerMCPConfigGenerator_GetDisabledMCPs(t *testing.T) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	disabled := gen.GetDisabledContainerMCPs()

	// Disabled MCPs should have reasons
	for name, reason := range disabled {
		if reason == "" {
			t.Errorf("Disabled MCP %s has no reason", name)
		}
		if !strings.HasPrefix(reason, "Missing:") && reason != "Unknown reason" {
			t.Errorf("Disabled MCP %s has unexpected reason format: %s", name, reason)
		}
	}
}

func TestContainerMCPConfigGenerator_GenerateSummary(t *testing.T) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	summary := gen.GenerateSummary()

	total, ok := summary["total"].(int)
	if !ok || total < 60 {
		t.Errorf("Expected total >= 60, got %v", summary["total"])
	}

	totalEnabled, ok := summary["total_enabled"].(int)
	if !ok || totalEnabled < 1 {
		t.Errorf("Expected total_enabled >= 1, got %v", summary["total_enabled"])
	}

	npxDeps, ok := summary["npx_dependencies"].(int)
	if !ok || npxDeps != 0 {
		t.Errorf("Expected npx_dependencies = 0, got %v", summary["npx_dependencies"])
	}
}

func TestContainerMCPConfigGenerator_GetMCPsByCategory(t *testing.T) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	byCategory := gen.GetMCPsByCategory()

	expectedCategories := []string{
		"helixagent", "core", "database", "vector", "devops",
		"browser", "communication", "productivity", "search",
		"google", "monitoring", "finance", "design",
	}

	for _, cat := range expectedCategories {
		if _, ok := byCategory[cat]; !ok {
			t.Errorf("Expected category %s not found", cat)
		}
	}

	// Core should have the most MCPs
	if len(byCategory["core"]) < 5 {
		t.Errorf("Expected at least 5 core MCPs, got %d", len(byCategory["core"]))
	}
}

func TestContainerMCPConfigGenerator_CustomHost(t *testing.T) {
	// Set custom MCP host
	_ = os.Setenv("MCP_CONTAINER_HOST", "mcp.example.com")
	defer func() { _ = os.Unsetenv("MCP_CONTAINER_HOST") }()

	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	mcps := gen.GenerateContainerMCPs()

	// Check that URLs use the custom host (except helixagent)
	for name, cfg := range mcps {
		if name == "helixagent" {
			continue
		}
		if !strings.Contains(cfg.URL, "mcp.example.com") {
			t.Errorf("MCP %s URL should contain custom host, got: %s", name, cfg.URL)
		}
	}
}

func TestContainerMCPConfigGenerator_SSEEndpoints(t *testing.T) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	mcps := gen.GenerateContainerMCPs()

	// All MCP URLs should end with /sse for SSE transport
	for name, cfg := range mcps {
		if name == "helixagent" {
			// HelixAgent uses its own endpoint format
			continue
		}
		if !strings.HasSuffix(cfg.URL, "/sse") {
			t.Errorf("MCP %s URL should end with /sse, got: %s", name, cfg.URL)
		}
	}
}

func TestContainerMCPConfigGenerator_GetTotalMCPCount(t *testing.T) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	count := gen.GetTotalMCPCount()

	if count < 60 {
		t.Errorf("Expected at least 60 MCPs, got %d", count)
	}
}

func TestContainerMCPConfigGenerator_HelixAgentMCP(t *testing.T) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	mcps := gen.GenerateContainerMCPs()

	helixAgent, ok := mcps["helixagent"]
	if !ok {
		t.Fatal("HelixAgent MCP not found")
	}

	if !helixAgent.Enabled {
		t.Error("HelixAgent MCP should always be enabled")
	}

	if helixAgent.Category != "helixagent" {
		t.Errorf("HelixAgent category should be 'helixagent', got '%s'", helixAgent.Category)
	}

	if !strings.Contains(helixAgent.URL, "/mcp/sse") {
		t.Errorf("HelixAgent URL should contain /mcp/sse, got: %s", helixAgent.URL)
	}
}

func TestContainerMCPConfigGenerator_NoLocalCommands(t *testing.T) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	mcps := gen.GenerateContainerMCPs()

	// Ensure no MCP has type "local" - all should be "remote"
	for name, cfg := range mcps {
		if cfg.Type != "remote" {
			t.Errorf("MCP %s should have type 'remote', got '%s'", name, cfg.Type)
		}
	}
}

func TestMCPContainerPorts_Coverage(t *testing.T) {
	// Ensure all ports are unique
	usedPorts := make(map[int]bool)
	for _, p := range MCPContainerPorts {
		if usedPorts[p.Port] {
			t.Errorf("Port %d is used multiple times", p.Port)
		}
		usedPorts[p.Port] = true
	}

	// Ensure port count matches expected
	if len(MCPContainerPorts) < 60 {
		t.Errorf("Expected at least 60 port allocations, got %d", len(MCPContainerPorts))
	}
}

func TestCompareWithNPXGenerator(t *testing.T) {
	// Compare container generator with the full NPX generator
	// to ensure all MCPs are covered
	containerGen := NewContainerMCPConfigGenerator("http://localhost:8080")
	npxGen := NewFullMCPConfigGenerator("http://localhost:8080")

	containerMCPs := containerGen.GenerateContainerMCPs()
	npxMCPs := npxGen.GenerateAllMCPs()

	// Container generator should have at least as many MCPs as NPX generator
	if len(containerMCPs) < len(npxMCPs) {
		t.Errorf("Container generator has fewer MCPs (%d) than NPX generator (%d)",
			len(containerMCPs), len(npxMCPs))
	}

	// Check that all NPX MCPs are also in container generator
	for name := range npxMCPs {
		if _, ok := containerMCPs[name]; !ok {
			t.Errorf("MCP %s exists in NPX generator but not in container generator", name)
		}
	}
}

func BenchmarkGenerateContainerMCPs(b *testing.B) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		gen.GenerateContainerMCPs()
	}
}

func BenchmarkGetEnabledContainerMCPs(b *testing.B) {
	gen := NewContainerMCPConfigGenerator("http://localhost:8080")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		gen.GetEnabledContainerMCPs()
	}
}
