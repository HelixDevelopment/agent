package services

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HA-F2-002 (review remediation) — the ROUTE-level guard for the cloud
// opt-in gate.
//
// Why this file exists even though TestProviderRegistry_LocalFirstDefaults
// already covers HA-F2-002: that test calls clearProviderEnvVarsForTest first,
// so it can never observe the case that actually matters — an operator whose
// shell/`.env`/docker-compose DOES carry a cloud API key while
// HELIX_CLOUD_PROVIDERS is unset. It also asserts internal flags
// (registry.autoDiscovery, cfg.Enabled), not the absence of a cloud ROUTE.
// The gate shipped in 7ad2b508 covered exactly one of the three paths a key
// can take, and the flag-level assertions could not see the other two.
//
// This test therefore sets real-shaped (fake) credentials and asserts on the
// observable an end user cares about: can a request be routed to a cloud
// provider? The observable is "GetProvider returns a constructed live client",
// which is the single funnel every registry-mediated cloud route passes
// through (handlers' ListProvidersOrderedByScore fallback, debate_service's
// GetProvider("claude")/GetProvider("deepseek"), and the participant-driven
// GetProvider(participant.LLMProvider) all land here).
//
//	RED_MODE=1   — assert the DEFECT IS PRESENT (run against the pre-fix tree):
//	               a set API key alone constructs a live cloud client with
//	               HELIX_CLOUD_PROVIDERS unset.
//	default (=0) — the standing GREEN guard: no cloud client is constructed
//	               unless the operator opted in.
//
// §11.4.115 RED-baseline-on-the-broken-artifact + polarity switch.
// §11.4.135 standing regression guard.
func TestCloudGate_EnvKeyAloneDoesNotOpenACloudRoute(t *testing.T) {
	setupCloudGateEnv(t)

	// Two independent providers so a single-provider special case cannot
	// make the guard pass by accident.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-fake-key-for-test-only")
	t.Setenv("DEEPSEEK_API_KEY", "sk-fake-deepseek-key-for-test-only")

	// Mirrors the standard deployment: router.go:263 builds the registry from
	// LoadRegistryConfigFromAppConfig, NOT from a hand-written config.
	cfg := LoadRegistryConfigFromAppConfig(nil)
	require.NotNil(t, cfg)
	registry := NewProviderRegistry(cfg, nil)
	require.NotNil(t, registry)

	claudeProvider, claudeErr := registry.GetProvider("claude")
	deepseekProvider, deepseekErr := registry.GetProvider("deepseek")

	if os.Getenv("RED_MODE") == "1" {
		// Defect reproduction on the pre-fix tree: the key alone is enough.
		assert.NoError(t, claudeErr,
			"RED_MODE=1: defect — a set ANTHROPIC_API_KEY must construct a live Claude client with HELIX_CLOUD_PROVIDERS unset")
		assert.NotNil(t, claudeProvider,
			"RED_MODE=1: defect — a live Claude client must exist with zero operator opt-in")
		assert.NoError(t, deepseekErr,
			"RED_MODE=1: defect — a set DEEPSEEK_API_KEY must construct a live DeepSeek client with HELIX_CLOUD_PROVIDERS unset")
		assert.NotNil(t, deepseekProvider,
			"RED_MODE=1: defect — a live DeepSeek client must exist with zero operator opt-in")
		return
	}

	assert.Error(t, claudeErr,
		"a set ANTHROPIC_API_KEY must NOT open a Claude route while HELIX_CLOUD_PROVIDERS is unset")
	assert.Nil(t, claudeProvider,
		"no live Claude client may be constructed without an explicit cloud opt-in")
	assert.Error(t, deepseekErr,
		"a set DEEPSEEK_API_KEY must NOT open a DeepSeek route while HELIX_CLOUD_PROVIDERS is unset")
	assert.Nil(t, deepseekProvider,
		"no live DeepSeek client may be constructed without an explicit cloud opt-in")
}

// TestCloudGate_OptInStillOpensTheCloudRoute is the other polarity: the gate
// must still ALLOW cloud when the operator explicitly asks for it. A gate that
// refuses everything is not a fix — it is a different defect.
//
// GREEN-mode only: HELIX_CLOUD_PROVIDERS does not exist on the pre-fix tree,
// where this behaviour was unconditional.
func TestCloudGate_OptInStillOpensTheCloudRoute(t *testing.T) {
	if os.Getenv("RED_MODE") == "1" {
		t.Skip("SKIP-OK: HA-F2-002 opt-in seam is asserted by the GREEN guard; the RED evidence is the defect reproduction above")
	}
	setupCloudGateEnv(t)

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-fake-key-for-test-only")
	t.Setenv("DEEPSEEK_API_KEY", "sk-fake-deepseek-key-for-test-only")
	t.Setenv("HELIX_CLOUD_PROVIDERS", "true")

	cfg := LoadRegistryConfigFromAppConfig(nil)
	require.NotNil(t, cfg)
	// Isolate the seam under test. The env-key -> Enabled -> live-client path
	// is what this file gates; the auto-discovery seam is already covered by
	// TestProviderRegistry_LocalFirstOptIn. Disabling discovery here also keeps
	// this a unit test rather than a live sweep of the dev host's credentials
	// (CONST-035).
	cfg.DisableAutoDiscovery = true
	registry := NewProviderRegistry(cfg, nil)
	require.NotNil(t, registry)

	claudeProvider, claudeErr := registry.GetProvider("claude")
	require.NoError(t, claudeErr,
		"with HELIX_CLOUD_PROVIDERS=true the operator asked for cloud: the Claude route must open")
	assert.NotNil(t, claudeProvider)

	deepseekProvider, deepseekErr := registry.GetProvider("deepseek")
	require.NoError(t, deepseekErr,
		"with HELIX_CLOUD_PROVIDERS=true the operator asked for cloud: the DeepSeek route must open")
	assert.NotNil(t, deepseekProvider)
}

// TestCloudGate_ExplicitOperatorConfigStillWins documents the deliberate
// boundary of the gate (§11.4.6 honest boundary): HELIX_CLOUD_PROVIDERS gates
// the IMPLICIT, env-credential-driven acquisition path. An operator who writes
// a provider into the registry config by hand has already made the decision
// the switch exists to ask about, so that path is intentionally NOT gated.
func TestCloudGate_ExplicitOperatorConfigStillWins(t *testing.T) {
	if os.Getenv("RED_MODE") == "1" {
		t.Skip("SKIP-OK: documents post-fix boundary; nothing to reproduce on the pre-fix tree")
	}
	setupCloudGateEnv(t)

	cfg := LoadRegistryConfigFromAppConfig(nil)
	require.NotNil(t, cfg)
	cfg.Providers["claude"] = &ProviderConfig{
		Name:    "claude",
		Type:    "claude",
		Enabled: true,
		APIKey:  "sk-ant-explicitly-configured-by-operator",
		Models:  []ModelConfig{{ID: "claude-3-sonnet-20240229", Enabled: true, Weight: 1.0}},
	}

	registry := NewProviderRegistry(cfg, nil)
	require.NotNil(t, registry)

	provider, err := registry.GetProvider("claude")
	require.NoError(t, err,
		"an explicitly configured provider is an operator decision already made; the env switch must not veto it")
	assert.NotNil(t, provider)
}

// setupCloudGateEnv scrubs every credential the discovery mapping table knows
// about plus the two local-first switches, so what the test observes is the
// CODE default and not the developer host's shell. PATH is emptied to keep the
// OpenCode (zen) CLI out of the picture.
func setupCloudGateEnv(t *testing.T) {
	t.Helper()
	clearProviderEnvVarsForTest(t)
	// Not in providerDiscoveryEnvVars but observed on live dev hosts.
	t.Setenv("INFERENCE_API_KEY", "")
	t.Setenv("ApiKey_Hyper", "")
	t.Setenv("HELIX_CLOUD_PROVIDERS", "")
	t.Setenv("USE_HELIX_LLM", "")
	t.Setenv("PATH", t.TempDir())
}
