package helixllm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HA-F2-004 — GetCapabilities must report what the serving layer actually
// serves, not a fabricated model id ("helixllm-default") and not broad
// capability flags no serving layer confirmed (CONST-036/CONST-040 class).
// §11.4.115 RED-baseline + polarity switch.
//
//	RED_MODE=1   — assert the DEFECT IS PRESENT: SupportedModels is the
//	               fabricated literal [helixllm-default] and ZERO live
//	               /v1/models requests were made to source capabilities.
//	default (=0) — standing GREEN guard: SupportedModels comes from a live,
//	               bounded GET /v1/models against the provider's endpoint.
//
// Gap ledger: docs/qa/2026-09-05-gap-ledger.md HA-F2-004.

// newModelsTestServer serves a fixed /v1/models payload and counts requests.
func newModelsTestServer(t *testing.T, ids []string) (*httptest.Server, *int64) {
	t.Helper()
	var requests int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt64(&requests, 1)
		type modelInfo struct {
			ID string `json:"id"`
		}
		data := make([]modelInfo, 0, len(ids))
		for _, id := range ids {
			data = append(data, modelInfo{ID: id})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func TestProvider_GetCapabilities_SupportedModelsAreLive(t *testing.T) {
	srv, requests := newModelsTestServer(t, []string{"coder-7b", "coder-32b"})

	p := NewProvider(Config{Endpoint: srv.URL, Timeout: 2 * time.Second})
	require.NotNil(t, p)

	caps := p.GetCapabilities()
	require.NotNil(t, caps)

	if os.Getenv("RED_MODE") == "1" {
		assert.Equal(t, []string{"helixllm-default"}, caps.SupportedModels,
			"RED_MODE=1: defect — SupportedModels is the fabricated literal, not the live listing")
		assert.Zero(t, atomic.LoadInt64(requests),
			"RED_MODE=1: defect — capabilities were never sourced from the serving layer")
		return
	}

	assert.Equal(t, []string{"coder-7b", "coder-32b"}, caps.SupportedModels,
		"SupportedModels must be exactly the ids the serving layer reports")
	assert.GreaterOrEqual(t, atomic.LoadInt64(requests), int64(1),
		"capabilities must be sourced from a live GET /v1/models")
}

func TestProvider_GetCapabilities_NoServedModelsReportedHonestly(t *testing.T) {
	srv, _ := newModelsTestServer(t, nil)

	p := NewProvider(Config{Endpoint: srv.URL, Timeout: 2 * time.Second})
	caps := p.GetCapabilities()
	require.NotNil(t, caps)

	if os.Getenv("RED_MODE") == "1" {
		assert.Equal(t, []string{"helixllm-default"}, caps.SupportedModels,
			"RED_MODE=1: defect — the fabricated id is reported even when nothing is served")
		return
	}
	assert.Empty(t, caps.SupportedModels,
		"when the serving layer lists zero models, SupportedModels must be honestly empty (CONST-036)")
	assert.NotContains(t, caps.SupportedModels, "helixllm-default",
		"the non-evidenced placeholder id must never surface as a supported model")
}

func TestProvider_GetCapabilities_UnreachableServingLayerFailsClosed(t *testing.T) {
	// A listener that is immediately closed: every connection refused, so the
	// listing fails fast. Capabilities must then carry NO fabricated models.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	p := NewProvider(Config{Endpoint: url, Timeout: 2 * time.Second})
	caps := p.GetCapabilities()
	require.NotNil(t, caps)

	if os.Getenv("RED_MODE") == "1" {
		assert.Equal(t, []string{"helixllm-default"}, caps.SupportedModels,
			"RED_MODE=1: defect — fabricated id even with the serving layer down")
		return
	}
	assert.Empty(t, caps.SupportedModels,
		"an unreachable serving layer must yield honest-empty capabilities, never a fabricated model")
}
