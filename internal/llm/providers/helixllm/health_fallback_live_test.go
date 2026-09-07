package helixllm

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// HA-CB-001 (provider half) — HealthCheck against a REAL HTTP server.
//
// §11.4.115 polarity switch: RED_MODE=1 reproduces the defect on the pre-fix
// artifact; unset/0 is the standing GREEN regression guard.
//
// This exercises the REAL provider over REAL HTTP (httptest), not a mock of it.
// The server mimics the measured behaviour of the coder backend at :18434:
//
//	GET /internal/health -> 404 {"error":{"message":"File Not Found",...}}
//	GET /v1/models       -> 200 {"data":[...]}
//
// captured 2026-09-07:
//
//	$ curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:18434/internal/health
//	404
//	$ curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:18434/v1/models
//	200
//
// /internal/health is a HelixLLM-GATEWAY path. Pointed at a plain
// OpenAI-compatible server it 404s, so the provider reported a fully working
// backend as unhealthy — the false signal that (through
// circuitBreakerProvider.HealthCheck) permanently opened the traffic breaker.
// ---------------------------------------------------------------------------

func redModeHelixLLM() bool { return os.Getenv("RED_MODE") == "1" }

// newCoderLikeServer returns a server shaped like the OpenAI-compatible coder:
// no /internal/health, a working /v1/models. modelsStatus lets a test drive the
// fallback surface to a genuine failure.
func newCoderLikeServer(t *testing.T, modelsStatus *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"File Not Found","type":"not_found_error","code":404}}`))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		code := int(atomic.LoadInt32(modelsStatus))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if code == http.StatusOK {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen2.5-coder-3b-instruct-q4_k_m","object":"model"}]}`))
		}
	})
	return httptest.NewServer(mux)
}

// TestHACB001Provider_HealthCheckFallsBackWhenGatewayPathAbsent is the core
// guard: an OpenAI-compatible backend that serves /v1/models but has no
// /internal/health MUST be reported HEALTHY.
func TestHACB001Provider_HealthCheckFallsBackWhenGatewayPathAbsent(t *testing.T) {
	status := int32(http.StatusOK)
	srv := newCoderLikeServer(t, &status)
	defer srv.Close()

	p := NewProvider(Config{Endpoint: srv.URL})
	err := p.HealthCheck()

	if redModeHelixLLM() {
		require.Error(t, err,
			"RED: pre-fix, the provider probes only /internal/health and reports a 404")
		assert.Contains(t, err.Error(), "404",
			"RED: the exact false signal observed in production")
	} else {
		require.NoError(t, err,
			"GREEN: /internal/health absent but /v1/models serving 200 -> the backend is HEALTHY")
	}
}

// TestHACB001Provider_GatewayHealthPathStillPreferred proves the fix did not
// break the HelixLLM-gateway deployment: when /internal/health answers 200 it
// is authoritative and no fallback request is needed.
//
// Holds in BOTH polarities — behaviour the fix preserves.
func TestHACB001Provider_GatewayHealthPathStillPreferred(t *testing.T) {
	var healthHits, modelsHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/health", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&healthHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&modelsHits, 1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := NewProvider(Config{Endpoint: srv.URL})
	require.NoError(t, p.HealthCheck(), "a gateway answering /internal/health 200 is healthy")
	assert.Equal(t, int32(1), atomic.LoadInt32(&healthHits), "the gateway health path is probed")
	assert.Equal(t, int32(0), atomic.LoadInt32(&modelsHits),
		"no fallback probe when the gateway path answers 200")
}

// TestHACB001Provider_RealFailuresStillReportUnhealthy is the NEGATIVE control:
// the fallback must not fail OPEN. A backend that is genuinely broken MUST
// still be reported unhealthy, or the health surface becomes a bluff.
//
// Holds in BOTH polarities.
func TestHACB001Provider_RealFailuresStillReportUnhealthy(t *testing.T) {
	t.Run("both probe paths failing", func(t *testing.T) {
		status := int32(http.StatusServiceUnavailable)
		srv := newCoderLikeServer(t, &status) // /internal/health 404, /v1/models 503
		defer srv.Close()

		p := NewProvider(Config{Endpoint: srv.URL})
		err := p.HealthCheck()
		require.Error(t, err,
			"a backend whose fallback surface is also failing MUST be unhealthy — never fail open")
		assert.Contains(t, err.Error(), "503")
	})

	t.Run("non-404 status is reported directly, no fallback", func(t *testing.T) {
		var modelsHits int32
		mux := http.NewServeMux()
		mux.HandleFunc("/internal/health", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&modelsHits, 1)
			w.WriteHeader(http.StatusOK)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		p := NewProvider(Config{Endpoint: srv.URL})
		err := p.HealthCheck()
		require.Error(t, err,
			"a gateway returning 500 is genuinely unhealthy — a healthy /v1/models must NOT mask it")
		assert.Contains(t, err.Error(), "500")
		assert.Equal(t, int32(0), atomic.LoadInt32(&modelsHits),
			"fallback fires only when the gateway path is ABSENT (404/405), never to paper over a real error")
	})

	t.Run("unreachable endpoint", func(t *testing.T) {
		srv := newCoderLikeServer(t, new(int32))
		url := srv.URL
		srv.Close() // now refusing connections

		p := NewProvider(Config{Endpoint: url})
		require.Error(t, p.HealthCheck(), "an unreachable backend MUST be unhealthy")
	})
}
