package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"dev.helix.agent/internal/handlers"
	"dev.helix.agent/internal/services"
	"dev.helix.agent/internal/skills"
)

// HXC-159 T-P6.01 — Wire RegisterExternalToolSource so the HelixAgent skills
// HTTP surface (GET /v1/skills, /categories, /:category, POST /match —
// RS-10) reflects an externally-sourced skill.
//
// mountSkillsRoutes mirrors internal/router/router.go's skills wiring
// EXACTLY (router.go:1353-1363 as of this session) — same group path, same
// four routes, same handler constructor — so this test exercises the real
// production route shape, not an approximation of it.
func mountSkillsRoutes(t *testing.T, integration *skills.Integration) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	skillsHandler := handlers.NewSkillsHandler(integration)
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	skillsHandler.SetLogger(logger)

	engine := gin.New()
	skillsGroup := engine.Group("/v1/skills")
	{
		skillsGroup.GET("", skillsHandler.ListSkills)
		skillsGroup.GET("/categories", skillsHandler.ListCategories)
		skillsGroup.GET("/:category", skillsHandler.GetSkillsByCategory)
		skillsGroup.POST("/match", skillsHandler.MatchSkills)
	}
	return engine
}

const externalFixtureSkillMD = `---
name: "hxc159-external-skill"
description: |
  A skill sourced from an external directory outside HelixAgent's own
  bundled skills/ tree. Triggers on: "hxc159 external trigger phrase"
category: "hxc159-external-category"
allowed-tools: "Read"
version: 1.0.0
license: MIT
author: "HXC-159 T-P6.01 fixture"
---

# HXC-159 External Fixture Skill

## Overview

Fixture used by tests/integration/skills_external_source_integration_test.go
to prove RS-10: GET /v1/skills reflects a skill registered through
services.ToolRegistry.RegisterExternalToolSource via skills.ExternalSkillSource.
`

func writeExternalFixtureSkill(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "hxc159-external-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(externalFixtureSkillMD), 0644))
	return dir
}

// TestRegisterExternalToolSourceAlone_DoesNotReachSkillsHTTPRoutes is the
// T-P6.01.1 RED baseline, kept as a standing regression guard: registering
// a tool via the RAW services.ToolRegistry.RegisterExternalToolSource API
// — WITHOUT the skills.ExternalSkillSource bridge this task adds — proves
// GET /v1/skills does NOT contain it, because ToolRegistry and
// skills.Service are genuinely disjoint registries. This is the exact gap
// RS-10 requires closing; skills.ExternalSkillSource (this task) is the
// closer, exercised by the test below.
func TestRegisterExternalToolSourceAlone_DoesNotReachSkillsHTTPRoutes(t *testing.T) {
	config := skills.DefaultSkillConfig()
	config.MinConfidence = 0.5
	service := skills.NewService(config)
	service.Start()
	integration := skills.NewIntegration(service)

	toolRegistry := services.NewToolRegistry(nil, nil)
	fetcher := func() ([]services.Tool, error) {
		return []services.Tool{&bareMockTool{name: "hxc159-bare-external-tool"}}, nil
	}
	require.NoError(t, toolRegistry.RegisterExternalToolSource("bare-source", fetcher))
	_, exists := toolRegistry.GetTool("hxc159-bare-external-tool")
	require.True(t, exists, "sanity: the tool IS registered in ToolRegistry")

	engine := mountSkillsRoutes(t, integration)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/skills", nil)
	engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp handlers.ListSkillsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	for _, s := range resp.Skills {
		assert.NotEqual(t, "hxc159-bare-external-tool", s.Name,
			"a bare ToolRegistry registration must NOT be visible on GET /v1/skills without the bridge")
	}
}

// TestExternalSkillSource_AllFourSkillsRoutesReflectIt is the T-P6.01.4
// assertion: with skills.ExternalSkillSource wired through
// RegisterExternalToolSource, ALL FOUR skills routes reflect the
// externally-sourced skill.
func TestExternalSkillSource_AllFourSkillsRoutesReflectIt(t *testing.T) {
	config := skills.DefaultSkillConfig()
	config.MinConfidence = 0.5
	service := skills.NewService(config)
	service.Start()
	integration := skills.NewIntegration(service)

	toolRegistry := services.NewToolRegistry(nil, nil)
	fixtureDir := writeExternalFixtureSkill(t)
	source := skills.NewExternalSkillSource("hxc159-external", fixtureDir, service)
	require.NoError(t, source.RegisterWith(toolRegistry))

	// Sanity: the ToolRegistry half of the bridge also holds it (the
	// "unified search + stats for free" T-P6.01 scope line promises).
	_, existsInToolRegistry := toolRegistry.GetTool("hxc159-external-skill")
	require.True(t, existsInToolRegistry, "sanity: ExternalSkillSource must ALSO register into ToolRegistry")

	engine := mountSkillsRoutes(t, integration)

	t.Run("GET /v1/skills", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/v1/skills", nil)
		engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var resp handlers.ListSkillsResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

		found := false
		for _, s := range resp.Skills {
			if s.Name == "hxc159-external-skill" {
				found = true
				assert.Equal(t, "hxc159-external-category", s.Category)
			}
		}
		assert.True(t, found, "GET /v1/skills must contain the externally-sourced skill (RS-10)")
	})

	t.Run("GET /v1/skills/categories", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/v1/skills/categories", nil)
		engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var resp handlers.CategoriesResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Contains(t, resp.Categories, "hxc159-external-category",
			"GET /v1/skills/categories must list the externally-sourced skill's category")
	})

	t.Run("GET /v1/skills/:category", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/v1/skills/hxc159-external-category", nil)
		engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var resp handlers.ListSkillsResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		found := false
		for _, s := range resp.Skills {
			if s.Name == "hxc159-external-skill" {
				found = true
			}
		}
		assert.True(t, found, "GET /v1/skills/hxc159-external-category must return the externally-sourced skill")
	})

	t.Run("POST /v1/skills/match", func(t *testing.T) {
		body := []byte(`{"input":"hxc159 external trigger phrase"}`)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/v1/skills/match", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var resp handlers.MatchResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		found := false
		for _, m := range resp.Matches {
			if m.Skill.Name == "hxc159-external-skill" {
				found = true
			}
		}
		assert.True(t, found, "POST /v1/skills/match must match the externally-sourced skill's trigger phrase")
	})
}

// bareMockTool is the minimal services.Tool used to prove the RED baseline
// (a tool that exists in ToolRegistry but was never bridged into
// skills.Service).
type bareMockTool struct{ name string }

func (b *bareMockTool) Name() string        { return b.name }
func (b *bareMockTool) Description() string { return "bare test tool" }
func (b *bareMockTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"input": map[string]interface{}{"type": "string"}}
}
func (b *bareMockTool) Execute(_ context.Context, _ map[string]interface{}) (interface{}, error) {
	return "ok", nil
}
func (b *bareMockTool) Source() string { return "bare-source" }
