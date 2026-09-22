package config

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HXC-159 T-P6.02 — MCP client configuration path.
//
// T-P5.04 (the "generate .mcp.json entries from the T-P1.04.2 manifest"
// generator, in helix_code/) was not yet landed in this checkout when this
// task was worked. Per the fallback the task text authorizes, this reads
// the HelixSkills MCP server's own documented invocation contract directly
// — `go run ./cmd/server --mcp stdio` (cmd/server/main.go's package doc
// comment, github.com/HelixDevelopment/skills) — rather than blocking on
// that sibling stream. Honest dependency, recorded rather than hidden.

// TestGenerateAllMCPs_HelixSkillsEntry_DisabledWithoutModulePath proves the
// entry is present but disabled (never silently active) when
// HELIXSKILLS_MODULE_PATH is unset — every existing deployment that has
// not opted in keeps today's behaviour, exactly like every other
// credential/path-gated entry in this generator.
func TestGenerateAllMCPs_HelixSkillsEntry_DisabledWithoutModulePath(t *testing.T) {
	t.Setenv("HELIXSKILLS_MODULE_PATH", "")
	g := NewFullMCPConfigGenerator("http://localhost:8100")
	mcps := g.GenerateAllMCPs()

	entry, exists := mcps["helixskills"]
	require.True(t, exists, "the helixskills entry must exist in the map (present-but-disabled, not absent)")
	assert.False(t, entry.Enabled, "without HELIXSKILLS_MODULE_PATH the entry must be disabled")
}

// TestGenerateAllMCPs_HelixSkillsEntry_NoGoDependencyIntroduced is T-P6.02.3:
// the generated entry is a plain string-slice Command (a subprocess
// invocation description) — asserting this at the type level pins that
// wiring an MCP client entry never requires an `import` of
// github.com/HelixDevelopment/skills, mirroring D-4 / T-P6.05's module-graph
// gate at the config-generation layer.
func TestGenerateAllMCPs_HelixSkillsEntry_NoGoDependencyIntroduced(t *testing.T) {
	t.Setenv("HELIXSKILLS_MODULE_PATH", "/tmp/does-not-need-to-exist-for-this-assertion")
	g := NewFullMCPConfigGenerator("http://localhost:8100")
	mcps := g.GenerateAllMCPs()
	entry, exists := mcps["helixskills"]
	require.True(t, exists)
	require.Equal(t, "local", entry.Type)
	require.NotEmpty(t, entry.Command)
	assert.Equal(t, "sh", entry.Command[0], "the entry MUST invoke the skills server as a subprocess, never a Go import")
	joined := strings.Join(entry.Command, " ")
	assert.Contains(t, joined, "go run ./cmd/server --mcp stdio",
		"the underlying invocation must be the skills server's own documented `go run ./cmd/server --mcp stdio` contract")
	assert.NotContains(t, joined, "import", "sanity: no accidental Go import syntax leaked into the command string")
}

// TestGenerateAllMCPs_HelixSkillsEntry_StartsAndAnswersProbe is RS-17: the
// GENERATED entry — not a hand-typed equivalent — is executed for real
// (exec.Command with the entry's own Command + Environment) and answers a
// live MCP tools/list JSON-RPC probe over stdio. Path correctness is
// proven by execution.
//
// Requires a reachable Postgres with the pgvector extension at
// HELIXSKILLS_TEST_DB_* (the skills MCP server is fail-closed on DB connect
// — see cmd/server/main.go). Honest SKIP when not configured, never a
// faked PASS (§11.4.3).
func TestGenerateAllMCPs_HelixSkillsEntry_StartsAndAnswersProbe(t *testing.T) {
	modulePath := os.Getenv("HELIXSKILLS_MODULE_PATH")
	if modulePath == "" {
		t.Skip("SKIP-OK: #hxc159-t-p6.02-live-probe — HELIXSKILLS_MODULE_PATH not set; " +
			"set it to the HelixSkills module checkout to run the live probe")
	}
	dbHost := os.Getenv("HELIXSKILLS_TEST_DB_HOST")
	if dbHost == "" {
		t.Skip("SKIP-OK: #hxc159-t-p6.02-live-probe — HELIXSKILLS_TEST_DB_HOST not set; " +
			"a live Postgres+pgvector instance is required (the skills MCP server is fail-closed on DB connect)")
	}

	g := NewFullMCPConfigGenerator("http://localhost:8100")
	mcps := g.GenerateAllMCPs()
	entry, exists := mcps["helixskills"]
	require.True(t, exists)
	require.True(t, entry.Enabled, "sanity: HELIXSKILLS_MODULE_PATH is set, so the entry must be enabled")
	require.NotEmpty(t, entry.Command)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Execute the EXACT generated command — this is the "path correctness
	// proven by execution" RS-17 requires, not a hand-typed stand-in.
	cmd := exec.CommandContext(ctx, entry.Command[0], entry.Command[1:]...) //nolint:gosec // generated, fixed argv — test-only subprocess
	cmd.Env = os.Environ()
	for k, v := range entry.Environment {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// DB connection env vars for the skills server (cmd/server/main.go
	// reads HELIX_DB_*), sourced from this test's own HELIXSKILLS_TEST_DB_*
	// so the generated entry's own Environment map stays free of
	// deployment-specific DB coordinates (T-P6.02 scope: the MCP client
	// entry, not the skills server's own DB provisioning).
	cmd.Env = append(cmd.Env,
		"HELIX_DB_HOST="+dbHost,
		"HELIX_DB_PORT="+os.Getenv("HELIXSKILLS_TEST_DB_PORT"),
		"HELIX_DB_NAME="+os.Getenv("HELIXSKILLS_TEST_DB_NAME"),
		"HELIX_DB_USER="+os.Getenv("HELIXSKILLS_TEST_DB_USER"),
		"HELIX_DB_PASSWORD="+os.Getenv("HELIXSKILLS_TEST_DB_PASSWORD"),
		"HELIX_DB_SSLMODE=disable",
	)

	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr

	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	reader := bufio.NewReader(stdout)

	_, err = stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"hxc159-t-p6.02-probe","version":"0"}}}` + "\n"))
	require.NoError(t, err)
	initLine, err := reader.ReadString('\n')
	require.NoError(t, err, "must receive an initialize response on stdout")
	assert.True(t, strings.Contains(initLine, `"result"`), "initialize response must carry a result: %s", initLine)

	_, err = stdin.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n"))
	require.NoError(t, err)
	toolsLine, err := reader.ReadString('\n')
	require.NoError(t, err, "must receive a tools/list response on stdout")

	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolsLine), &resp))
	assert.NotEmpty(t, resp.Result.Tools, "RS-17: the generated entry's server must answer tools/list with at least one tool")
}
