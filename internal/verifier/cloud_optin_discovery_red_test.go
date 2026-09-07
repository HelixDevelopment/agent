package verifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HA-F2-002 (review remediation) — the boot-time verifier half of the cloud
// opt-in gate.
//
// The gate shipped in 7ad2b508 (services.CloudProvidersOptedIn) was wired at
// exactly one site, in the provider registry. The startup verifier was left
// ungated: with a cloud API key in the environment and HELIX_CLOUD_PROVIDERS
// unset, discoverProviders() still walked every env-credentialed provider and
// called DiscoverModels(), i.e. it made a REAL OUTBOUND REQUEST to a
// third-party cloud endpoint at boot while both HelixAgent and its consuming
// project reported "cloud disabled".
//
// This test observes the ROUTE, not a flag: it points the provider's models
// endpoint at a local httptest server and asserts on whether a request is
// actually issued.
//
//	RED_MODE=1   — assert the DEFECT IS PRESENT (run against the pre-fix tree):
//	               the provider is discovered AND the endpoint is hit.
//	default (=0) — the standing GREEN guard: nothing discovered, zero requests.
//
// §11.4.115 RED-baseline-on-the-broken-artifact + polarity switch.
// §11.4.135 standing regression guard.
func TestCloudGate_VerifierDoesNotDiscoverCloudWithoutOptIn(t *testing.T) {
	var hits int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-chat"}]}`))
	}))
	defer server.Close()

	scrubProviderEnvForTest(t)
	redirectModelsURLForTest(t, "deepseek", server.URL+"/v1/models")

	// The one credential under test. HELIX_CLOUD_PROVIDERS stays unset.
	t.Setenv("DEEPSEEK_API_KEY", "sk-fake-deepseek-key-for-test-only")

	sv := newDiscoveryOnlyVerifierForTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	discovered, err := sv.discoverProviders(ctx)
	require.NoError(t, err)

	found := false
	for _, d := range discovered {
		if d.Type == "deepseek" {
			found = true
		}
	}
	observedHits := atomic.LoadInt64(&hits)

	if os.Getenv("RED_MODE") == "1" {
		assert.True(t, found,
			"RED_MODE=1: defect — the verifier must discover a cloud provider from an env key alone, with HELIX_CLOUD_PROVIDERS unset")
		assert.Positive(t, observedHits,
			"RED_MODE=1: defect — the verifier must issue a real outbound request to the cloud provider's endpoint at boot with zero operator opt-in")
		return
	}

	assert.False(t, found,
		"the verifier must not discover a cloud provider while HELIX_CLOUD_PROVIDERS is unset")
	assert.Zero(t, observedHits,
		"the verifier must issue ZERO outbound requests to a cloud provider endpoint while HELIX_CLOUD_PROVIDERS is unset")
}

// TestCloudGate_VerifierDiscoversCloudWithOptIn is the other polarity: opting
// in must still give the operator boot-time verification of their cloud keys.
// A gate that refuses everything is not a fix.
func TestCloudGate_VerifierDiscoversCloudWithOptIn(t *testing.T) {
	if os.Getenv("RED_MODE") == "1" {
		t.Skip("SKIP-OK: HA-F2-002 opt-in seam is asserted by the GREEN guard; the RED evidence is the defect reproduction above")
	}

	var hits int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-chat"}]}`))
	}))
	defer server.Close()

	scrubProviderEnvForTest(t)
	redirectModelsURLForTest(t, "deepseek", server.URL+"/v1/models")

	t.Setenv("DEEPSEEK_API_KEY", "sk-fake-deepseek-key-for-test-only")
	t.Setenv("HELIX_CLOUD_PROVIDERS", "true")

	sv := newDiscoveryOnlyVerifierForTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	discovered, err := sv.discoverProviders(ctx)
	require.NoError(t, err)

	found := false
	for _, d := range discovered {
		if d.Type == "deepseek" {
			found = true
		}
	}
	assert.True(t, found,
		"with HELIX_CLOUD_PROVIDERS=true the operator asked for cloud: boot-time verification of their keys must still happen")
	assert.Positive(t, atomic.LoadInt64(&hits),
		"with the opt-in set, model discovery must still reach the provider endpoint")
}

// TestCloudGate_VerifierStillDiscoversLocalProviders proves the gate is
// provider-CLASS aware and not a blunt kill switch: local (self-hosted)
// providers stay discoverable with cloud opted out, which is the whole point
// of "local-first".
func TestCloudGate_VerifierStillDiscoversLocalProviders(t *testing.T) {
	if os.Getenv("RED_MODE") == "1" {
		t.Skip("SKIP-OK: documents post-fix boundary; nothing to reproduce on the pre-fix tree")
	}

	var hits int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3.2"}]}`))
	}))
	defer server.Close()

	scrubProviderEnvForTest(t)
	redirectModelsURLForTest(t, "ollama", server.URL+"/api/tags")

	// OLLAMA_BASE_URL is an entry in SupportedProviders["ollama"].EnvVars, so a
	// non-empty value takes the same env-driven discovery path a cloud key
	// would — and must NOT be gated: it is a local endpoint.
	t.Setenv("OLLAMA_BASE_URL", server.URL)

	sv := newDiscoveryOnlyVerifierForTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	discovered, err := sv.discoverProviders(ctx)
	require.NoError(t, err)

	found := false
	for _, d := range discovered {
		if d.Type == "ollama" {
			found = true
		}
	}
	assert.True(t, found,
		"a LOCAL provider must remain discoverable with cloud opted out — local-first means local works by default")
}

// newDiscoveryOnlyVerifierForTest builds a StartupVerifier configured for
// discovery only: free/anonymous providers off (matching cmd/helixagent's
// main.go, which sets EnableFreeProviders=false) so the assertion is about the
// env-credentialed path and nothing else.
func newDiscoveryOnlyVerifierForTest(t *testing.T) *StartupVerifier {
	t.Helper()
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	cfg := DefaultStartupConfig()
	cfg.EnableFreeProviders = false
	return NewStartupVerifier(cfg, log)
}

// redirectModelsURLForTest points one provider's models endpoint at a local
// test server for the duration of t, restoring the real value afterwards.
// ProviderAccessRegistry holds pointers, so the entry is swapped wholesale
// rather than mutated in place — mutating the shared struct would leak the
// test URL into every other test in the package.
func redirectModelsURLForTest(t *testing.T, providerType, modelsURL string) {
	t.Helper()
	original, ok := ProviderAccessRegistry[providerType]
	require.True(t, ok, "provider %q must exist in ProviderAccessRegistry", providerType)

	replacement := *original
	replacement.ModelsURL = modelsURL
	ProviderAccessRegistry[providerType] = &replacement
	t.Cleanup(func() { ProviderAccessRegistry[providerType] = original })
}

// scrubProviderEnvForTest empties every env var any SupportedProviders entry
// looks at, plus the two local-first switches, so the observed behaviour is
// the code default and never the developer host's real credentials (CONST-035:
// a unit test must not silently become an integration test against the cloud).
// It also chdirs into a temp dir because the discovery path writes
// .faulty_api_keys / .unsupported_api_keys relative to the working directory.
func scrubProviderEnvForTest(t *testing.T) {
	t.Helper()
	seen := make(map[string]bool)
	for _, info := range SupportedProviders {
		for _, envVar := range info.EnvVars {
			if seen[envVar] {
				continue
			}
			seen[envVar] = true
			t.Setenv(envVar, "")
		}
	}
	t.Setenv("HELIX_CLOUD_PROVIDERS", "")
	t.Setenv("USE_HELIX_LLM", "")
	t.Setenv("PATH", t.TempDir())
	t.Chdir(t.TempDir())
}
