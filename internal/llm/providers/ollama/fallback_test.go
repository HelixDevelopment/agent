package ollama

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestOllamaFallbackOnlyOnDiscoveryFailure pins the ORDERING that makes the
// provider's hardcoded list a permitted CONST-036 exception (operator decision
// 2026-09-13): discovery always wins, and the fallback is used ONLY when
// discovery fails. Without this the list would be a catalogue that can silently
// override the single source of truth, which is what CONST-036 forbids.
func TestOllamaFallbackOnlyOnDiscoveryFailure(t *testing.T) {
	const discovered = "unit-test-model:latest"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"` + discovered + `","model":"` + discovered + `"}]}`))
	}))
	defer srv.Close()

	live := NewOllamaProvider(srv.URL, "")
	caps := live.GetCapabilities()
	assert.Contains(t, caps.SupportedModels, discovered,
		"a successful discovery must be used")
	assert.NotContains(t, caps.SupportedModels, "orca-mini",
		"the hardcoded fallback must NOT appear when discovery succeeded")

	// Nothing listens on port 1: discovery fails, and the documented fallback
	// is the honest answer rather than an empty, unusable list.
	dead := NewOllamaProvider("http://127.0.0.1:1", "")
	deadCaps := dead.GetCapabilities()
	assert.Contains(t, deadCaps.SupportedModels, "orca-mini",
		"the documented fallback is used when discovery fails")
}
