package services

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HA-F2-002 — spec 002 "local adaptive serving": cloud endpoints must NOT be
// reachable by default, and the local HelixLLM/llama.cpp chain MUST be the
// default. §11.4.115 RED-baseline-on-the-broken-artifact + polarity switch.
//
//	RED_MODE=1   — assert the DEFECT IS PRESENT (run against the pre-fix tree):
//	               auto-discovery ON, anonymous zen enabled, local helixllm
//	               disabled, all with zero operator action.
//	default (=0) — the standing GREEN guard: auto-discovery OFF unless the
//	               operator opted in (HELIX_CLOUD_PROVIDERS), anonymous zen
//	               disabled, local helixllm enabled by default.
//
// Gap ledger: docs/qa/2026-09-05-gap-ledger.md HA-F2-002 (operator decision
// 2026-09-05: flip the defaults; do not re-litigate).
func TestProviderRegistry_LocalFirstDefaults(t *testing.T) {
	// Scrub both the new-switch env vars and every provider credential so the
	// "default" being observed is really the code default, not the dev shell's.
	clearProviderEnvVarsForTest(t)
	// providerDiscoveryEnvVars is missing two mappings observed on a live dev
	// host (INFERENCE_API_KEY, ApiKey_Hyper) — scrub them here so the observed
	// default is the code default, never the dev shell's credentials.
	t.Setenv("INFERENCE_API_KEY", "")
	t.Setenv("ApiKey_Hyper", "")
	t.Setenv("HELIX_CLOUD_PROVIDERS", "")
	t.Setenv("USE_HELIX_LLM", "")
	t.Setenv("PATH", t.TempDir()) // keep OpenCode CLI (zen) out of the picture

	redMode := os.Getenv("RED_MODE") == "1"

	registry := NewProviderRegistry(nil, nil)
	require.NotNil(t, registry)

	zenCfg, zenFound := registry.providerConfigs.Get("zen")
	require.True(t, zenFound, "synthesized zen default config must exist")
	llmCfg, llmFound := registry.providerConfigs.Get("helixllm")
	require.True(t, llmFound, "synthesized helixllm default config must exist")

	if redMode {
		// Defect reproduction (pre-fix tree): cloud is reachable by default
		// and the local chain is off. Every assertion here describes the
		// defect; on the pre-fix tree this branch PASSES, which is the RED
		// evidence that the defect genuinely existed.
		assert.True(t, registry.autoDiscovery,
			"RED_MODE=1: defect — provider auto-discovery must be ON by default (cloud reachable with zero operator action)")
		assert.True(t, zenCfg.Enabled,
			"RED_MODE=1: defect — credential-less anonymous zen must be enabled by default (talks to a public endpoint)")
		assert.False(t, llmCfg.Enabled,
			"RED_MODE=1: defect — the local HelixLLM chain must be OFF by default (USE_HELIX_LLM required)")
		return
	}

	assert.False(t, registry.autoDiscovery,
		"cloud auto-discovery must be OFF by default (opt in via HELIX_CLOUD_PROVIDERS=true)")
	assert.False(t, zenCfg.Enabled,
		"anonymous zen must never be enabled by default: it talks to a public endpoint with no credential")
	assert.True(t, llmCfg.Enabled,
		"the local HelixLLM/llama.cpp chain must be ON by default (spec 002 local-first)")
}

// TestProviderRegistry_LocalFirstOptIn covers the operator opt-in/opt-out
// seams (GREEN-mode only: these env vars do not exist on the pre-fix tree,
// so RED_MODE=1 reproduces the defect in the test above instead).
func TestProviderRegistry_LocalFirstOptIn(t *testing.T) {
	if os.Getenv("RED_MODE") == "1" {
		t.Skip("SKIP-OK: HA-F2-002 opt-in seams are asserted by the GREEN guard; RED evidence is the defect reproduction above")
	}
	clearProviderEnvVarsForTest(t)
	t.Setenv("INFERENCE_API_KEY", "")
	t.Setenv("ApiKey_Hyper", "")
	t.Setenv("PATH", t.TempDir())

	t.Run("HELIX_CLOUD_PROVIDERS=true enables auto-discovery", func(t *testing.T) {
		t.Setenv("HELIX_CLOUD_PROVIDERS", "true")
		registry := NewProviderRegistry(nil, nil)
		require.NotNil(t, registry)
		assert.True(t, registry.autoDiscovery,
			"explicit operator opt-in must enable cloud provider auto-discovery")

		zenCfg, found := registry.providerConfigs.Get("zen")
		require.True(t, found)
		assert.True(t, zenCfg.Enabled,
			"with auto-discovery opted in, the synthesized zen default follows the switch (documented behaviour)")
	})

	t.Run("explicit config DisableAutoDiscovery still wins over env opt-in", func(t *testing.T) {
		t.Setenv("HELIX_CLOUD_PROVIDERS", "true")
		registry := NewProviderRegistry(&RegistryConfig{DisableAutoDiscovery: true}, nil)
		require.NotNil(t, registry)
		assert.False(t, registry.autoDiscovery,
			"config-level DisableAutoDiscovery must remain an unconditional kill switch")
	})

	t.Run("USE_HELIX_LLM=false opts out of the local chain", func(t *testing.T) {
		t.Setenv("USE_HELIX_LLM", "false")
		registry := NewProviderRegistry(nil, nil)
		require.NotNil(t, registry)
		llmCfg, found := registry.providerConfigs.Get("helixllm")
		require.True(t, found)
		assert.False(t, llmCfg.Enabled,
			"an explicit USE_HELIX_LLM=false must disable the local chain")
	})
}
