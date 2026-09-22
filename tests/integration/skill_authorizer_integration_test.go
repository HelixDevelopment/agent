package integration

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"dev.helix.agent/internal/services"
	"dev.helix.agent/internal/skills"
)

// HXC-159 T-P6.03 — Make AllowedTools enforcing (RS-14), exercised with the
// REAL production skills.SkillAuthorizer (not the services-package fake
// used for the narrower unit tests in tool_registry_test.go).
func TestSkillAuthorizer_EnforcesAllowedToolsAtRealCallBoundary(t *testing.T) {
	config := skills.DefaultSkillConfig()
	service := skills.NewService(config)
	service.Start()

	// A real skill declaring it may only Read.
	readerSkill := &skills.Skill{
		Name:         "hxc159-reader-skill",
		Description:  "Reads things",
		AllowedTools: "Read",
		Instructions: "Only ever read.",
	}
	service.RegisterSkill(readerSkill)

	// A real skill declaring NO restriction (empty AllowedTools) — proves
	// the documented "no declaration = unrestricted" choice (T-P6.03.3).
	unrestrictedSkill := &skills.Skill{
		Name:         "hxc159-unrestricted-skill",
		Description:  "Declares nothing",
		Instructions: "Anything goes.",
	}
	service.RegisterSkill(unrestrictedSkill)

	authorizer := skills.NewSkillAuthorizer(service)

	registry := services.NewToolRegistry(nil, nil)
	readTool := &fakeExecTool{name: "Read"}
	writeTool := &fakeExecTool{name: "Write"}
	require.NoError(t, registry.RegisterCustomTool(readTool))
	require.NoError(t, registry.RegisterCustomTool(writeTool))
	registry.SetAuthorizer(authorizer)

	t.Run("declared tool is allowed", func(t *testing.T) {
		result, err := registry.ExecuteToolAs(context.Background(), "hxc159-reader-skill", "Read", map[string]interface{}{})
		require.NoError(t, err)
		assert.Equal(t, "executed:Read", result)
	})

	t.Run("RS-14: undeclared tool is refused, never executed", func(t *testing.T) {
		writeTool.executed = false
		result, err := registry.ExecuteToolAs(context.Background(), "hxc159-reader-skill", "Write", map[string]interface{}{})
		require.Error(t, err)
		assert.Nil(t, result)
		assert.False(t, writeTool.executed, "the tool's Execute must never run when the authorizer refuses")
	})

	t.Run("empty AllowedTools means unrestricted", func(t *testing.T) {
		result, err := registry.ExecuteToolAs(context.Background(), "hxc159-unrestricted-skill", "Write", map[string]interface{}{})
		require.NoError(t, err)
		assert.Equal(t, "executed:Write", result)
	})

	t.Run("unknown actor is refused (fail-closed)", func(t *testing.T) {
		result, err := registry.ExecuteToolAs(context.Background(), "no-such-skill", "Read", map[string]interface{}{})
		require.Error(t, err)
		assert.Nil(t, result)
	})

	t.Run("RS-14: the refusal is audited", func(t *testing.T) {
		log := authorizer.AuditLog()
		require.NotEmpty(t, log)

		var foundRefusal bool
		for _, entry := range log {
			if entry.SkillName == "hxc159-reader-skill" && entry.ToolName == "Write" && !entry.Allowed {
				foundRefusal = true
				assert.NotEmpty(t, entry.Reason, "an audited refusal must carry an operator-visible reason")
			}
		}
		assert.True(t, foundRefusal, "the reader-skill's refused Write attempt must appear in the audit log")
	})
}

// fakeExecTool is a minimal real services.Tool (not a scripted/mocked
// authorizer — this is the actual tool the registry executes) used to
// prove Execute is genuinely never invoked when the authorizer refuses.
type fakeExecTool struct {
	name     string
	executed bool
}

func (f *fakeExecTool) Name() string                       { return f.name }
func (f *fakeExecTool) Description() string                { return "fake exec tool " + f.name }
func (f *fakeExecTool) Parameters() map[string]interface{} { return map[string]interface{}{} }
func (f *fakeExecTool) Source() string                     { return "custom" }
func (f *fakeExecTool) Execute(_ context.Context, _ map[string]interface{}) (interface{}, error) {
	f.executed = true
	return "executed:" + f.name, nil
}
