package helixllm

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HA-F2-003 — the adapter's own defaulting must not silently enable it under
// a zero Config. §11.4.115 RED-baseline + polarity switch.
//
//	RED_MODE=1   — assert the DEFECT IS PRESENT: NewAdapter(Config{}) with
//	               USE_HELIX_LLM unset yields an ENABLED adapter.
//	default (=0) — standing GREEN guard: zero Config means disabled; callers
//	               state intent explicitly via Config.Enabled.
//
// Gap ledger: docs/qa/2026-09-05-gap-ledger.md HA-F2-003.
func TestAdapter_ZeroConfigIsDisabled(t *testing.T) {
	prev, had := os.LookupEnv("USE_HELIX_LLM")
	require.NoError(t, os.Unsetenv("USE_HELIX_LLM"))
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("USE_HELIX_LLM", prev)
		} else {
			_ = os.Unsetenv("USE_HELIX_LLM")
		}
	})

	a, err := NewAdapter(Config{})
	require.NoError(t, err)

	if os.Getenv("RED_MODE") == "1" {
		assert.True(t, a.IsEnabled(),
			"RED_MODE=1: defect — zero-Config construction silently enables the adapter (getEnvBool USE_HELIX_LLM default true)")
		return
	}
	assert.False(t, a.IsEnabled(),
		"zero-Config adapter must be disabled; callers state intent via Config.Enabled explicitly")
}
