package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HA-F2-001 (FR-019) — the /v1/models facade must not present pseudo-models as
// usable when nothing is serving them. §11.4.115 RED-baseline + polarity switch.
//
//	RED_MODE=1   — assert the DEFECT IS PRESENT: all five hardcoded
//	               pseudo-models carry full allow_* permissions and NO
//	               availability annotation of any kind.
//	default (=0) — standing GREEN guard: with no catalog wired, availability
//	               is "unreported" and dispatch permission is withheld
//	               (never advertised usable on no evidence).
//
// Gap ledger: docs/qa/2026-09-05-gap-ledger.md HA-F2-001.
//
// This test intentionally uses ONLY the pre-existing public surface so it
// compiles and reproduces the defect on the pre-fix tree; the catalog-wired
// GREEN assertions live in openai_models_catalog_test.go (which requires the
// SetCatalogService seam introduced by the fix).

// facadePseudoModelIDs are the five hardcoded facade ids under test.
var facadePseudoModelIDs = []string{
	"helixagent-debate",
	"helixagent-llm",
	"helixagent-ensemble",
	"helix-debate",
	"helix-llm",
}

// callUnifiedModels invokes UnifiedHandler.Models and decodes the response.
func callUnifiedModels(t *testing.T, h *UnifiedHandler) []map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	h.Models(c)
	require.Equal(t, http.StatusOK, w.Code)

	var response struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, "list", response.Object)
	return response.Data
}

// findModelByID locates one model entry by its id.
func findModelByID(t *testing.T, data []map[string]any, id string) map[string]any {
	t.Helper()
	for _, m := range data {
		if m["id"] == id {
			return m
		}
	}
	require.Failf(t, "model not listed", "id %q missing from /v1/models data", id)
	return nil
}

// permissionOf extracts the first (only) permission block of a model entry.
func permissionOf(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	perms, ok := m["permission"].([]any)
	require.True(t, ok, "permission must be a list")
	require.Len(t, perms, 1)
	p, ok := perms[0].(map[string]any)
	require.True(t, ok)
	return p
}

func TestModelsFacade_PseudoModelsRequireEvidence(t *testing.T) {
	h := NewUnifiedHandler(nil, nil)
	data := callUnifiedModels(t, h)

	if os.Getenv("RED_MODE") == "1" {
		// Defect reproduction (pre-fix tree): every pseudo-model is advertised
		// as unconditionally dispatchable, with no availability/withheld state.
		for _, id := range facadePseudoModelIDs {
			m := findModelByID(t, data, id)
			perm := permissionOf(t, m)
			assert.Equal(t, true, perm["allow_sampling"],
				"RED_MODE=1: defect — %q advertises sampling with nothing serving it", id)
			assert.Equal(t, true, perm["allow_create_engine"],
				"RED_MODE=1: defect — %q advertises dispatch with nothing serving it", id)
			_, hasAvailability := m["availability"]
			assert.False(t, hasAvailability,
				"RED_MODE=1: defect — %q carries no availability state at all", id)
		}
		return
	}

	// GREEN guard (catalog unwired): nothing has confirmed any backend is
	// serving, so no pseudo-model may advertise dispatch capability.
	for _, id := range facadePseudoModelIDs {
		m := findModelByID(t, data, id)
		perm := permissionOf(t, m)
		assert.Equal(t, false, perm["allow_sampling"],
			"%q must not advertise sampling when no serving backend is confirmed", id)
		assert.Equal(t, false, perm["allow_create_engine"],
			"%q must not advertise dispatch when no serving backend is confirmed", id)
		assert.Equal(t, "unreported", m["availability"],
			"%q availability must be 'unreported' (never a serving claim) when the catalog is unwired", id)
	}
}
