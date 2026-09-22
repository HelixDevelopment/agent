package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MockTool implements the Tool interface for testing
type MockTool struct {
	name        string
	description string
	parameters  map[string]interface{}
	source      string
	executeFunc func(ctx context.Context, params map[string]interface{}) (interface{}, error)
}

func (m *MockTool) Name() string {
	return m.name
}

func (m *MockTool) Description() string {
	return m.description
}

func (m *MockTool) Parameters() map[string]interface{} {
	return m.parameters
}

func (m *MockTool) Execute(ctx context.Context, params map[string]interface{}) (interface{}, error) {
	if m.executeFunc != nil {
		return m.executeFunc(ctx, params)
	}
	return map[string]interface{}{"result": "success"}, nil
}

func (m *MockTool) Source() string {
	return m.source
}

func newValidMockTool(name string) *MockTool {
	return &MockTool{
		name:        name,
		description: "A test tool for " + name,
		parameters: map[string]interface{}{
			"input": map[string]interface{}{
				"type":        "string",
				"description": "Input parameter",
			},
		},
		source: "custom",
	}
}

func TestNewToolRegistry(t *testing.T) {
	registry := NewToolRegistry(nil, nil)

	require.NotNil(t, registry)
	assert.NotNil(t, registry.tools)
	assert.NotNil(t, registry.customTools)
	assert.Nil(t, registry.mcpManager)
	assert.Nil(t, registry.lspClient)
}

func TestToolRegistry_RegisterCustomTool(t *testing.T) {
	registry := NewToolRegistry(nil, nil)

	t.Run("register valid tool", func(t *testing.T) {
		tool := newValidMockTool("test-tool")
		err := registry.RegisterCustomTool(tool)

		require.NoError(t, err)

		// Verify tool is registered
		registeredTool, exists := registry.GetTool("test-tool")
		assert.True(t, exists)
		assert.Equal(t, "test-tool", registeredTool.Name())
	})

	t.Run("register duplicate tool fails", func(t *testing.T) {
		tool := newValidMockTool("duplicate-tool")
		err := registry.RegisterCustomTool(tool)
		require.NoError(t, err)

		// Try to register again
		duplicateTool := newValidMockTool("duplicate-tool")
		err = registry.RegisterCustomTool(duplicateTool)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "already registered")
	})

	t.Run("register tool with empty name fails", func(t *testing.T) {
		tool := &MockTool{
			name:        "",
			description: "No name tool",
			parameters:  map[string]interface{}{},
			source:      "custom",
		}
		err := registry.RegisterCustomTool(tool)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name cannot be empty")
	})

	t.Run("register tool with empty description fails", func(t *testing.T) {
		tool := &MockTool{
			name:        "no-desc-tool",
			description: "",
			parameters:  map[string]interface{}{},
			source:      "custom",
		}
		err := registry.RegisterCustomTool(tool)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "description cannot be empty")
	})

	t.Run("register tool with nil parameters fails", func(t *testing.T) {
		tool := &MockTool{
			name:        "nil-params-tool",
			description: "Tool with nil params",
			parameters:  nil,
			source:      "custom",
		}
		err := registry.RegisterCustomTool(tool)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "parameters cannot be nil")
	})
}

func TestToolRegistry_ValidateParameterSchema(t *testing.T) {
	registry := NewToolRegistry(nil, nil)

	t.Run("valid parameter schema", func(t *testing.T) {
		tool := &MockTool{
			name:        "valid-schema-tool",
			description: "Tool with valid schema",
			parameters: map[string]interface{}{
				"input": map[string]interface{}{
					"type":        "string",
					"description": "Input parameter",
				},
			},
			source: "custom",
		}
		err := registry.RegisterCustomTool(tool)
		assert.NoError(t, err)
	})

	t.Run("parameter schema without type fails", func(t *testing.T) {
		tool := &MockTool{
			name:        "no-type-tool",
			description: "Tool with no type in schema",
			parameters: map[string]interface{}{
				"input": map[string]interface{}{
					"description": "Missing type field",
				},
			},
			source: "custom",
		}
		err := registry.RegisterCustomTool(tool)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "must have a type")
	})

	t.Run("parameter schema not a map fails", func(t *testing.T) {
		tool := &MockTool{
			name:        "invalid-schema-tool",
			description: "Tool with invalid schema",
			parameters: map[string]interface{}{
				"input": "not a map",
			},
			source: "custom",
		}
		err := registry.RegisterCustomTool(tool)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "must be a map")
	})
}

func TestToolRegistry_GetTool(t *testing.T) {
	registry := NewToolRegistry(nil, nil)

	t.Run("get existing tool", func(t *testing.T) {
		tool := newValidMockTool("get-test-tool")
		_ = registry.RegisterCustomTool(tool)

		foundTool, exists := registry.GetTool("get-test-tool")
		assert.True(t, exists)
		assert.Equal(t, "get-test-tool", foundTool.Name())
	})

	t.Run("get non-existent tool", func(t *testing.T) {
		tool, exists := registry.GetTool("non-existent-tool")
		assert.False(t, exists)
		assert.Nil(t, tool)
	})
}

func TestToolRegistry_ListTools(t *testing.T) {
	registry := NewToolRegistry(nil, nil)

	t.Run("empty registry", func(t *testing.T) {
		tools := registry.ListTools()
		assert.Empty(t, tools)
	})

	t.Run("with registered tools", func(t *testing.T) {
		_ = registry.RegisterCustomTool(newValidMockTool("tool-1"))
		_ = registry.RegisterCustomTool(newValidMockTool("tool-2"))
		_ = registry.RegisterCustomTool(newValidMockTool("tool-3"))

		tools := registry.ListTools()
		assert.Len(t, tools, 3)
	})
}

func TestToolRegistry_ExecuteTool(t *testing.T) {
	registry := NewToolRegistry(nil, nil)

	t.Run("execute existing tool", func(t *testing.T) {
		tool := &MockTool{
			name:        "exec-tool",
			description: "Executable tool",
			parameters: map[string]interface{}{
				"input": map[string]interface{}{
					"type": "string",
				},
			},
			source: "custom",
			executeFunc: func(ctx context.Context, params map[string]interface{}) (interface{}, error) {
				return map[string]interface{}{"executed": true, "input": params["input"]}, nil
			},
		}
		_ = registry.RegisterCustomTool(tool)

		result, err := registry.ExecuteTool(context.Background(), "exec-tool", map[string]interface{}{"input": "test"})
		require.NoError(t, err)

		resultMap, ok := result.(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, true, resultMap["executed"])
		assert.Equal(t, "test", resultMap["input"])
	})

	t.Run("execute non-existent tool", func(t *testing.T) {
		result, err := registry.ExecuteTool(context.Background(), "non-existent", nil)
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("execute with missing required parameter", func(t *testing.T) {
		tool := &MockTool{
			name:        "required-param-tool",
			description: "Tool with required param",
			parameters: map[string]interface{}{
				"required_input": map[string]interface{}{
					"type": "string",
				},
			},
			source: "custom",
		}
		_ = registry.RegisterCustomTool(tool)

		result, err := registry.ExecuteTool(context.Background(), "required-param-tool", map[string]interface{}{})
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "missing required parameter")
	})

	t.Run("execute tool that returns error", func(t *testing.T) {
		tool := &MockTool{
			name:        "error-tool",
			description: "Tool that errors",
			parameters: map[string]interface{}{
				"input": map[string]interface{}{
					"type": "string",
				},
			},
			source: "custom",
			executeFunc: func(ctx context.Context, params map[string]interface{}) (interface{}, error) {
				return nil, errors.New("execution failed")
			},
		}
		_ = registry.RegisterCustomTool(tool)

		result, err := registry.ExecuteTool(context.Background(), "error-tool", map[string]interface{}{"input": "test"})
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "execution failed")
	})

	t.Run("execute with context timeout", func(t *testing.T) {
		tool := &MockTool{
			name:        "slow-tool",
			description: "Slow executing tool",
			parameters: map[string]interface{}{
				"input": map[string]interface{}{
					"type": "string",
				},
			},
			source: "custom",
			executeFunc: func(ctx context.Context, params map[string]interface{}) (interface{}, error) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(100 * time.Millisecond):
					return "completed", nil
				}
			},
		}
		_ = registry.RegisterCustomTool(tool)

		// Create a context with short timeout
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		result, err := registry.ExecuteTool(ctx, "slow-tool", map[string]interface{}{"input": "test"})
		assert.Error(t, err)
		assert.Nil(t, result)
	})
}

func TestToolRegistry_RegisterExternalToolSource(t *testing.T) {
	registry := NewToolRegistry(nil, nil)

	t.Run("register tools from external source", func(t *testing.T) {
		fetcher := func() ([]Tool, error) {
			return []Tool{
				newValidMockTool("external-tool-1"),
				newValidMockTool("external-tool-2"),
			}, nil
		}

		err := registry.RegisterExternalToolSource("test-source", fetcher)
		require.NoError(t, err)

		tools := registry.ListTools()
		assert.Len(t, tools, 2)
	})

	t.Run("fetcher returns error", func(t *testing.T) {
		fetcher := func() ([]Tool, error) {
			return nil, errors.New("fetch failed")
		}

		err := registry.RegisterExternalToolSource("failing-source", fetcher)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "fetch failed")
	})

	t.Run("skip duplicate tools from external source", func(t *testing.T) {
		// Register a tool first
		_ = registry.RegisterCustomTool(newValidMockTool("existing-tool"))

		fetcher := func() ([]Tool, error) {
			return []Tool{
				newValidMockTool("existing-tool"), // Duplicate
				newValidMockTool("new-tool"),
			}, nil
		}

		err := registry.RegisterExternalToolSource("test-source", fetcher)
		require.NoError(t, err)

		// Should have the original plus new-tool (not duplicate)
		_, exists := registry.GetTool("existing-tool")
		assert.True(t, exists)
		_, exists = registry.GetTool("new-tool")
		assert.True(t, exists)
	})
}

func TestToolRegistry_RefreshTools(t *testing.T) {
	registry := NewToolRegistry(nil, nil)

	t.Run("refresh keeps custom tools", func(t *testing.T) {
		// Register custom tool
		_ = registry.RegisterCustomTool(newValidMockTool("custom-tool"))

		err := registry.RefreshTools(context.Background())
		require.NoError(t, err)

		// Custom tool should still exist
		_, exists := registry.GetTool("custom-tool")
		assert.True(t, exists)
	})

	t.Run("refresh updates lastRefresh time", func(t *testing.T) {
		beforeRefresh := time.Now()
		time.Sleep(10 * time.Millisecond)

		err := registry.RefreshTools(context.Background())
		require.NoError(t, err)

		assert.True(t, registry.lastRefresh.After(beforeRefresh))
	})

	t.Run("refresh with MCP manager adds MCP tools", func(t *testing.T) {
		log := logrus.New()
		log.SetLevel(logrus.PanicLevel)
		mcpManager := NewMCPManager(nil, nil, log)

		registryWithMCP := NewToolRegistry(mcpManager, nil)

		err := registryWithMCP.RefreshTools(context.Background())
		require.NoError(t, err)
		// MCP manager starts with no tools, so count should be 0
		tools := registryWithMCP.ListTools()
		// Just verify it ran without error
		assert.NotNil(t, tools)
	})

	t.Run("refresh clears non-custom tools", func(t *testing.T) {
		freshRegistry := NewToolRegistry(nil, nil)

		// Add a custom tool
		_ = freshRegistry.RegisterCustomTool(newValidMockTool("custom-keep"))

		// Add a non-custom tool by directly modifying (simulating MCP tool)
		mcpTool := &MCPTool{
			Name:        "mcp-tool-to-remove",
			Description: "Should be removed",
		}
		wrapper := &MCPToolWrapper{mcpTool: mcpTool, mcpManager: nil}
		freshRegistry.tools.Put("mcp-tool-to-remove", wrapper)

		// Verify both tools exist
		_, customExists := freshRegistry.GetTool("custom-keep")
		_, mcpExists := freshRegistry.GetTool("mcp-tool-to-remove")
		assert.True(t, customExists)
		assert.True(t, mcpExists)

		// Refresh should remove non-custom tools
		err := freshRegistry.RefreshTools(context.Background())
		require.NoError(t, err)

		// Custom tool should still exist
		_, customExistsAfter := freshRegistry.GetTool("custom-keep")
		assert.True(t, customExistsAfter)

		// MCP tool should be removed (and not re-added since no MCP manager)
		_, mcpExistsAfter := freshRegistry.GetTool("mcp-tool-to-remove")
		assert.False(t, mcpExistsAfter)
	})

	// HXC-159 T-P6.01.3: RefreshTools must NOT wipe a source registered via
	// RegisterExternalToolSource. The existing tests above use
	// newValidMockTool, whose Source() is always the literal "custom" —
	// so they never actually exercised the wipe path for an
	// externally-registered tool (a "custom" tool always survives
	// RefreshTools's `tool.Source() != "custom"` clear). This test uses a
	// tool whose Source() reports the REAL external-source name, exactly
	// as attachment-point-rank-2 wiring (T-P6.01.2) does.
	t.Run("refresh preserves tools registered via RegisterExternalToolSource", func(t *testing.T) {
		freshRegistry := NewToolRegistry(nil, nil)

		externalTool := &MockTool{
			name:        "helixskills-example-skill",
			description: "A skill sourced from an external tool source",
			parameters: map[string]interface{}{
				"query": map[string]interface{}{"type": "string"},
			},
			source: "helixskills-external", // NOT "custom" — the real shape
		}
		fetchCount := 0
		fetcher := func() ([]Tool, error) {
			fetchCount++
			return []Tool{externalTool}, nil
		}

		require.NoError(t, freshRegistry.RegisterExternalToolSource("helixskills-external", fetcher))
		_, existsBeforeRefresh := freshRegistry.GetTool("helixskills-example-skill")
		require.True(t, existsBeforeRefresh, "sanity: the tool must be registered before refresh")
		require.Equal(t, 1, fetchCount, "sanity: registration itself calls the fetcher exactly once")

		require.NoError(t, freshRegistry.RefreshTools(context.Background()))

		_, existsAfterRefresh := freshRegistry.GetTool("helixskills-example-skill")
		assert.True(t, existsAfterRefresh,
			"RegisterExternalToolSource's tool MUST survive RefreshTools — "+
				"a refresh must not silently un-register an external source")
		assert.GreaterOrEqual(t, fetchCount, 2,
			"RefreshTools must re-invoke the registered fetcher, not merely leave stale entries behind")

		// A second refresh must keep working (registration is durable, not
		// a one-shot survivor of exactly one refresh).
		require.NoError(t, freshRegistry.RefreshTools(context.Background()))
		_, existsAfterSecondRefresh := freshRegistry.GetTool("helixskills-example-skill")
		assert.True(t, existsAfterSecondRefresh, "external source must survive repeated refreshes")
	})
}

func TestMCPToolWrapper(t *testing.T) {
	mcpTool := &MCPTool{
		Name:        "mcp-test-tool",
		Description: "An MCP test tool",
		InputSchema: map[string]interface{}{
			"param1": map[string]interface{}{
				"type": "string",
			},
		},
	}

	wrapper := &MCPToolWrapper{
		mcpTool:    mcpTool,
		mcpManager: nil,
	}

	t.Run("Name", func(t *testing.T) {
		assert.Equal(t, "mcp-test-tool", wrapper.Name())
	})

	t.Run("Description", func(t *testing.T) {
		assert.Equal(t, "An MCP test tool", wrapper.Description())
	})

	t.Run("Parameters", func(t *testing.T) {
		params := wrapper.Parameters()
		assert.NotNil(t, params)
		assert.Contains(t, params, "param1")
	})

	t.Run("Source", func(t *testing.T) {
		assert.Equal(t, "mcp", wrapper.Source())
	})
}

// Benchmark tests
func BenchmarkToolRegistry_RegisterCustomTool(b *testing.B) {
	registry := NewToolRegistry(nil, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tool := newValidMockTool("bench-tool-" + string(rune(i)))
		_ = registry.RegisterCustomTool(tool)
	}
}

func BenchmarkToolRegistry_GetTool(b *testing.B) {
	registry := NewToolRegistry(nil, nil)
	_ = registry.RegisterCustomTool(newValidMockTool("bench-tool"))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = registry.GetTool("bench-tool")
	}
}

func BenchmarkToolRegistry_ExecuteTool(b *testing.B) {
	registry := NewToolRegistry(nil, nil)
	_ = registry.RegisterCustomTool(newValidMockTool("bench-exec-tool"))

	ctx := context.Background()
	params := map[string]interface{}{"input": "test"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = registry.ExecuteTool(ctx, "bench-exec-tool", params)
	}
}

// HXC-159 T-P6.03 — Make AllowedTools enforcing.

// TestToolRegistry_ExecuteTool_HasNoActorScopedEnforcement is the T-P6.03.1
// RED baseline, kept as a standing regression guard: ExecuteTool (the
// pre-existing, non-actor-scoped call boundary) has NO concept of "which
// actor is calling" and therefore cannot enforce any per-actor policy —
// an "over-reaching" call always succeeds there, regardless of what any
// skill declares. This is not a bug in ExecuteTool (callers with no actor
// concept are legitimately unrestricted); it is the reason ExecuteToolAs
// exists as a SEPARATE, additive boundary — this test pins that ExecuteTool
// itself never grows implicit enforcement that would silently change its
// contract for those callers.
func TestToolRegistry_ExecuteTool_HasNoActorScopedEnforcement(t *testing.T) {
	registry := NewToolRegistry(nil, nil)
	// Even with an authorizer installed that would refuse EVERYTHING,
	// plain ExecuteTool must not consult it.
	registry.SetAuthorizer(alwaysRefuseAuthorizer{})
	require.NoError(t, registry.RegisterCustomTool(newValidMockTool("unrestricted-tool")))

	result, err := registry.ExecuteTool(context.Background(), "unrestricted-tool", map[string]interface{}{"input": "x"})
	require.NoError(t, err, "ExecuteTool must remain unrestricted even with an authorizer installed")
	assert.NotNil(t, result)
}

// alwaysRefuseAuthorizer implements ToolCallAuthorizer, always refusing —
// used to prove ExecuteTool ignores it and ExecuteToolAs enforces it.
type alwaysRefuseAuthorizer struct{}

func (alwaysRefuseAuthorizer) Authorize(actorID, toolName string) (bool, string) {
	return false, "always-refuse test authorizer"
}

func TestToolRegistry_ExecuteToolAs_EnforcesAuthorizer(t *testing.T) {
	registry := NewToolRegistry(nil, nil)
	require.NoError(t, registry.RegisterCustomTool(newValidMockTool("Read")))
	require.NoError(t, registry.RegisterCustomTool(newValidMockTool("Write")))

	t.Run("allowed tool call succeeds", func(t *testing.T) {
		registry.SetAuthorizer(scriptedAuthorizer{allow: map[string]bool{"reader-skill:Read": true}})
		result, err := registry.ExecuteToolAs(context.Background(), "reader-skill", "Read", map[string]interface{}{"input": "x"})
		require.NoError(t, err)
		assert.NotNil(t, result)
	})

	t.Run("RS-14: an over-reaching skill is refused, not executed", func(t *testing.T) {
		// reader-skill is allowed Read but NOT Write.
		auth := scriptedAuthorizer{allow: map[string]bool{"reader-skill:Read": true}}
		registry.SetAuthorizer(auth)

		result, err := registry.ExecuteToolAs(context.Background(), "reader-skill", "Write", map[string]interface{}{"input": "x"})
		require.Error(t, err, "a skill declaring only Read must be refused when it attempts Write")
		assert.Nil(t, result)
		assert.ErrorIs(t, err, ErrToolCallRefused)
	})

	t.Run("T-P6.03.4 paired mutation: no authorizer installed means the over-reaching call SUCCEEDS", func(t *testing.T) {
		// This is the "strip the enforcement check" mutation, expressed as
		// the observable state a stripped wiring produces: with NO
		// authorizer installed (SetAuthorizer never called, or called with
		// nil — exactly what "stripping" the enforcement wiring in
		// router.go would leave), the same over-reaching call that the
		// subtest above proves is refused now SUCCEEDS. If a future change
		// accidentally removes the SetAuthorizer(...) call in router.go,
		// THIS is the behavioural difference a security test built against
		// the "always refused" assumption would need to catch — and does:
		// asserting refusal here, with no authorizer installed, FAILS.
		unenforced := NewToolRegistry(nil, nil)
		require.NoError(t, unenforced.RegisterCustomTool(newValidMockTool("Write")))

		result, err := unenforced.ExecuteToolAs(context.Background(), "reader-skill", "Write", map[string]interface{}{"input": "x"})
		require.NoError(t, err, "PAIRED-MUTATION PROOF: with the authorizer wiring stripped, an "+
			"over-reaching call is NOT refused — this is exactly what a stripped T-P6.03 enforcement "+
			"check produces, and is why the subtest above (with the authorizer installed) MUST assert refusal")
		assert.NotNil(t, result)
	})
}

// scriptedAuthorizer implements ToolCallAuthorizer from a fixed
// actorID:toolName -> allowed table, refusing everything not listed.
type scriptedAuthorizer struct {
	allow map[string]bool
}

func (s scriptedAuthorizer) Authorize(actorID, toolName string) (bool, string) {
	if s.allow[actorID+":"+toolName] {
		return true, ""
	}
	return false, actorID + " is not authorized to call " + toolName
}
