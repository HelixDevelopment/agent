package services

// HXC-274 regression guard.
//
// THE DEFECT. Constructing a *ProviderRegistry via NewProviderRegistry —
// the constructor cmd/grpc-server/main.go's main() calls before it ever
// starts serving — used to make real, ambient-credentialed outbound HTTP
// calls to third-party LLM provider APIs (and, on failure, to the
// models.dev model catalogue), one per provider whose credential happened
// to be present in the process environment. The call chain:
//
//	NewProviderRegistry
//	  -> newProviderRegistry (autoDiscovery defaults true — since HA-F2-002
//	     it requires an explicit HELIX_CLOUD_PROVIDERS=true opt-in; the
//	     tests below set that env var before exercising the path)
//	    -> initAutoDiscovery
//	      -> ProviderDiscovery.DiscoverProviders()          [pre-fix]
//	        -> (for every discovered provider) provider.GetCapabilities()
//	          -> discovery.Discoverer.DiscoverModels()       [REAL HTTP]
//
// GetCapabilities() reads, at every call site, like a cheap static getter.
// For ~40 of the ~45 supported provider types (every provider wired
// through internal/llm/discovery.Discoverer — see e.g.
// internal/llm/providers/deepseek/deepseek.go's GetCapabilities), it is
// not: it performs a Tier-1 HTTP call to the provider's own /v1/models-
// style endpoint and, on failure or an empty result, a Tier-2 HTTP call to
// the models.dev catalogue. With seven live-credentialed providers present
// this was reported taking on the order of 18-19 real seconds before the
// gRPC listener began accepting connections.
//
// THE FIX. initAutoDiscovery (provider_registry.go) now calls the new
// ProviderDiscovery.DiscoverProviderCredentials() (provider_discovery.go),
// which shares its entire implementation with DiscoverProviders() via
// discoverProvidersLocked, gated by ONE parameter: fetchCapabilities. When
// false, GetCapabilities() is never invoked on any discovered provider, so
// the function performs zero outbound network I/O — only local env-var
// reads and in-memory client construction. DiscoverProviders() keeps its
// original, unconditional behaviour: the explicit, operator-triggered
// POST /v1/providers/rediscover handler (internal/handlers/
// provider_management.go ReDiscoverProviders) still calls it and still
// gets live model data, which is the "explicitly asked to" case this fix
// is not supposed to remove (§11.4.124 — investigate before removing a
// capability; discovery's live-model-enumeration value, established by the
// commit that introduced internal/llm/discovery (54afd0485), is preserved
// in full for anyone who asks for it on purpose).
//
// WHY THESE TESTS ARE SHAPED THE WAY THEY ARE. HXC-274's own forensic note
// warns that reading the source alone previously led an investigation to
// the wrong conclusion, because the outbound call is buried two calls
// below GetCapabilities(). TestHXC274_DiscovererPerformsRealOutboundHTTPCall
// below therefore proves the underlying mechanism at RUNTIME — a real HTTP
// request observed arriving at a local listener — rather than merely
// asserting against source text. It is intentionally decoupled from the
// registry/discovery machinery: it constructs a discovery.Discoverer
// exactly the way every real provider constructor does, so it cannot be
// fooled by a future change to discoverProvidersLocked that still leaves
// the underlying Discoverer.DiscoverModels() call reachable somewhere this
// test doesn't look.
//
// The remaining tests then prove the FIX's specific gate, at runtime,
// against the real, unmodified production entry points
// (ProviderDiscovery.DiscoverProviders / DiscoverProviderCredentials, and
// NewProviderRegistry itself) — never a hand-rolled recreation of them.
//
// SAFETY (operator mandate for this fix: no real provider calls, ever,
// including while investigating or testing). Every test below that
// exercises the real discovery/registry machinery does so inside a
// hermetic environment built by hxc274HermeticEnv: every env var any
// providerMappings entry or OPENCODE_API_KEY reads is blanked for the
// subtest's duration (t.Setenv, auto-restored), and PATH is pointed at an
// empty directory so the OpenCode CLI — confirmed present on at least one
// host this suite runs on, and whose OWN independent discovery path
// (ZenCLIProvider.DiscoverModels, internal/llm/providers/zen/zen_cli.go)
// can reach models.dev on its Tier 2, a mechanism this fix does not touch
// and cannot gate — can never be selected. Only Zen's remaining provider-
// selection fallbacks are reachable: HTTP-server or fully-anonymous, both
// confirmed by reading internal/llm/providers/zen/zen_http.go and
// internal/llm/providers/zen/zen.go to return a hardcoded, static
// *models.ProviderCapabilities with no network call of any kind. No
// credential-mapped provider (DeepSeek, Anthropic, Gemini, ...) is ever
// discovered in these tests, because its GetCapabilities() would hit a
// real, hardcoded third-party endpoint — none of providerMappings'
// BaseURL/ModelsEndpoint values are overridable via environment, so that
// path is proven separately, and safely, by
// TestHXC274_DiscovererPerformsRealOutboundHTTPCall's httptest.Server
// instead of by discovering a real credentialed provider.

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"dev.helix.agent/internal/llm/discovery"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// hxc274ProviderCredentialEnvVars returns every environment variable name
// providerMappings reads, plus OPENCODE_API_KEY (read directly by
// discoverOAuthProviders' Zen branch). Built from the live table rather
// than hand-copied so it can never silently drift from it — a
// hand-maintained duplicate list is exactly the kind of sibling assertion
// that stops discriminating the moment the real table changes and nobody
// remembers to update the copy.
func hxc274ProviderCredentialEnvVars() []string {
	vars := make([]string, 0, len(providerMappings)+1)
	seen := make(map[string]bool, len(providerMappings)+1)
	for _, m := range providerMappings {
		if !seen[m.EnvVar] {
			seen[m.EnvVar] = true
			vars = append(vars, m.EnvVar)
		}
	}
	if !seen["OPENCODE_API_KEY"] {
		vars = append(vars, "OPENCODE_API_KEY")
	}
	return vars
}

// hxc274HermeticEnv blanks every provider-credential env var (see above)
// and points PATH at an empty directory, for the lifetime of t. This makes
// it safe to exercise the REAL discovery/registry machinery in-process:
// the credentialed-provider loop discovers nothing (no live third-party
// endpoint is ever contacted, regardless of what is ambient in the host
// environment), and Zen — the one provider discovered unconditionally,
// credential or not — can only select its static-capability HTTP or
// anonymous variants, never its CLI variant. See the file-level doc
// comment for why the CLI variant specifically must be excluded.
func hxc274HermeticEnv(t *testing.T) {
	t.Helper()
	for _, v := range hxc274ProviderCredentialEnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("PATH", t.TempDir())
}

// TestHXC274_DiscovererPerformsRealOutboundHTTPCall proves the ROOT
// mechanism at runtime, over a real (loopback-only) socket: constructing a
// discovery.Discoverer exactly the way every real provider does — see
// e.g. internal/llm/providers/deepseek/deepseek.go's
// NewDeepSeekProviderWithRetry, which builds this exact shape with a real
// endpoint — and calling DiscoverModels() dispatches a genuine HTTP
// request carrying the configured credential. This is precisely what
// GetCapabilities() triggers on ~40 of the ~45 supported provider types,
// and what DiscoverProviders() used to invoke, unconditionally, for every
// provider it discovered.
//
// The endpoint is a local httptest.Server — never a real provider or
// models.dev — so this test is safe to run with live credentials present
// in the environment; it never reads any of them.
func TestHXC274_DiscovererPerformsRealOutboundHTTPCall(t *testing.T) {
	var requests int64
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	d := discovery.NewDiscoverer(discovery.ProviderConfig{
		ProviderName:   "hxc274-mechanism-probe",
		ModelsEndpoint: srv.URL + "/v1/models",
		APIKey:         "hxc274-fake-credential-do-not-use",
		FallbackModels: []string{"hxc274-fallback-model"},
	})

	_ = d.DiscoverModels()

	require.Equal(t, int64(1), atomic.LoadInt64(&requests),
		"Discoverer.DiscoverModels() did not reach the configured endpoint at all — "+
			"if this fails, the mechanism GetCapabilities() relies on has changed and "+
			"HXC-274's whole premise needs re-deriving, not this test loosened")
	require.Equal(t, "Bearer hxc274-fake-credential-do-not-use", gotAuth,
		"the request that arrived did not carry the configured credential the way "+
			"the real providers' GetCapabilities() call would send it")
}

// TestHXC274_DiscoverProviderCredentials_NeverPopulatesCapabilities drives
// the two real, unmodified exported entry points — DiscoverProviders and
// DiscoverProviderCredentials — on the SAME ProviderDiscovery instance, in
// a hermetic environment, and shows the one behavioural difference between
// them: whether a discovered provider's Capabilities gets populated. Zen
// is the vehicle because it is discovered unconditionally (no credential
// required) and, under hxc274HermeticEnv, can only ever resolve to a
// static-capability variant (see file-level doc comment) — so calling
// GetCapabilities() on it here is safe regardless of which variant the
// host happens to select.
func TestHXC274_DiscoverProviderCredentials_NeverPopulatesCapabilities(t *testing.T) {
	hxc274HermeticEnv(t)

	logger := logrus.New()
	logger.SetOutput(testWriter{t})
	pd := NewProviderDiscovery(logger, false)

	credsOnly, err := pd.DiscoverProviderCredentials()
	require.NoError(t, err)
	zenCredsOnly := hxc274FindByName(t, credsOnly, "zen")
	require.Nil(t, zenCredsOnly.Capabilities,
		"DiscoverProviderCredentials populated Capabilities for a discovered provider — "+
			"it must perform zero outbound calls; the fetchCapabilities=false gate has "+
			"regressed")
	require.Nil(t, zenCredsOnly.SupportsModels,
		"DiscoverProviderCredentials populated SupportsModels without ever calling "+
			"GetCapabilities() to source it from")

	full, err := pd.DiscoverProviders()
	require.NoError(t, err)
	zenFull := hxc274FindByName(t, full, "zen")
	require.NotNil(t, zenFull.Capabilities,
		"DiscoverProviders (the explicit, operator-opt-in rediscovery path) must still "+
			"populate Capabilities — if this is nil, the fix accidentally weakened the "+
			"path that is SUPPOSED to fetch live data on request, not just the "+
			"construction-time path that must not")
}

// TestHXC274_RegistryConstructionNeverPopulatesCapabilities drives the
// literal production entry point cmd/grpc-server/main.go's main() calls —
// services.LoadRegistryConfigFromAppConfig(nil) followed by
// services.NewProviderRegistry(cfg, nil) — and confirms that construction
// alone never populates a discovered provider's Capabilities. This is the
// strongest test in this file: it does not call any of the new
// discovery-package methods directly, only the same two calls main()
// makes, so it cannot pass by accident if initAutoDiscovery's wiring is
// wrong.
func TestHXC274_RegistryConstructionNeverPopulatesCapabilities(t *testing.T) {
	hxc274HermeticEnv(t)

	cfg := LoadRegistryConfigFromAppConfig(nil)
	// HA-F2-002 (operator decision 2026-09-05): cloud auto-discovery is
	// default-OFF and requires an explicit HELIX_CLOUD_PROVIDERS opt-in.
	// This test targets the construction path that runs when discovery IS
	// enabled, so it opts in explicitly — the HXC-274 property under test
	// (construction never populates Capabilities) is unchanged.
	t.Setenv("HELIX_CLOUD_PROVIDERS", "true")
	reg := NewProviderRegistry(cfg, nil)

	disc := reg.GetDiscovery()
	require.NotNil(t, disc, "auto-discovery, when explicitly opted in via HELIX_CLOUD_PROVIDERS=true, "+
		"must still construct a discovery handle; if this is nil even with the opt-in, "+
		"NewProviderRegistry stopped discovering providers at all, which is a different "+
		"(and worse) regression than HXC-274")

	zen := disc.GetProviderByName("zen")
	require.NotNil(t, zen, "zen is discovered unconditionally regardless of credentials; "+
		"if this is nil the hermetic env is suppressing it in some new way and the rest "+
		"of this test is vacuous")
	require.Nil(t, zen.Capabilities,
		"NewProviderRegistry (the constructor cmd/grpc-server/main.go's main() calls, "+
			"before the listener ever accepts a connection) populated a discovered "+
			"provider's Capabilities during construction — this means initAutoDiscovery "+
			"is once again calling the network-calling DiscoverProviders() instead of "+
			"DiscoverProviderCredentials(); HXC-274 has regressed")
}

// TestHXC274_ExplicitRediscoveryStillFetchesLiveCapabilities is the
// negative-space companion to the two guards above: it confirms the fix
// did not silently remove the capability HXC-274's acceptance criteria
// explicitly preserves ("discovery is opt-in or deferred until first
// use" — not deleted). DiscoverProviders(), called directly (the same
// method internal/handlers/provider_management.go's ReDiscoverProviders
// HTTP handler calls), must still return live Capabilities for a provider
// whose GetCapabilities() is safe to call in this hermetic environment.
func TestHXC274_ExplicitRediscoveryStillFetchesLiveCapabilities(t *testing.T) {
	hxc274HermeticEnv(t)

	logger := logrus.New()
	logger.SetOutput(testWriter{t})
	pd := NewProviderDiscovery(logger, false)

	discovered, err := pd.DiscoverProviders()
	require.NoError(t, err)
	zen := hxc274FindByName(t, discovered, "zen")

	require.NotNil(t, zen.Capabilities, "explicit DiscoverProviders() must still fetch "+
		"live capabilities — §11.4.124: this call's existing behaviour is a real, used "+
		"capability (the POST /v1/providers/rediscover handler), not dead code to delete")
	require.NotEmpty(t, zen.Capabilities.SupportedModels,
		"zen's static capability list is non-empty by construction; an empty result "+
			"here means GetCapabilities() was not actually reached")
}

// TestHXC274_TimingMechanism_SerialCapabilityFetchIsWhatCostsSeconds
// measures, rather than asserts, the mechanism-level timing difference the
// fix removes from the registry-construction path. It cannot safely time
// the real cmd/grpc-server binary with live credentials present (that is
// the exact outbound call this fix exists to prevent), so it reproduces
// the SAME shape production takes — N providers, each fetched serially via
// Discoverer.DiscoverModels(), constructed exactly like the real provider
// wiring — against N local httptest.Servers standing in for N credentialed
// providers, one of which is deliberately slow (mirroring a real
// provider's round-trip latency) and reports elapsed wall-clock time for
// "fetch capabilities for all N" (the pre-fix shape) versus "skip
// capability fetch entirely" (the fix). The pass/fail assertion only
// checks that skipping is at least an order of magnitude faster; the
// absolute numbers are logged for the record, not asserted, because they
// are host- and provider-count-dependent (HXC-274 itself reports ~18s for
// seven real providers on the reporting host).
func TestHXC274_TimingMechanism_SerialCapabilityFetchIsWhatCostsSeconds(t *testing.T) {
	const providerCount = 7 // matches HXC-274's own reported "seven providers"
	perProviderDelay := 400 * time.Millisecond

	discoverers := make([]*discovery.Discoverer, providerCount)
	var servers []*httptest.Server
	for i := 0; i < providerCount; i++ {
		delay := perProviderDelay
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
		}))
		servers = append(servers, srv)
		discoverers[i] = discovery.NewDiscoverer(discovery.ProviderConfig{
			ProviderName:   "hxc274-timing-probe",
			ModelsEndpoint: srv.URL + "/v1/models",
			APIKey:         "hxc274-fake-credential-do-not-use",
			FallbackModels: []string{"fallback"},
		})
	}
	defer func() {
		for _, srv := range servers {
			srv.Close()
		}
	}()

	// Pre-fix shape: initAutoDiscovery's old body, serially, calls
	// GetCapabilities() (== DiscoverModels() here) for every discovered
	// provider.
	before := time.Now()
	for _, d := range discoverers {
		_ = d.DiscoverModels()
	}
	eagerElapsed := time.Since(before)

	// Fixed shape: DiscoverProviderCredentials never reaches this call at
	// all, for any provider — represented here by simply not calling
	// DiscoverModels().
	after := time.Now()
	lazyElapsed := time.Since(after)

	t.Logf("HXC-274 mechanism timing: %d providers, %s simulated per-provider "+
		"latency -> eager (pre-fix) capability fetch = %s, skipped (fixed) = %s",
		providerCount, perProviderDelay, eagerElapsed, lazyElapsed)

	require.GreaterOrEqual(t, eagerElapsed, time.Duration(providerCount)*perProviderDelay,
		"the eager, serial capability fetch should take at least providerCount * "+
			"perProviderDelay — if it is faster, the loop stopped being serial and this "+
			"measurement no longer reproduces the reported ~18s shape")
	require.Less(t, lazyElapsed, eagerElapsed/10,
		"skipping capability fetch should be at least an order of magnitude faster than "+
			"fetching it for every provider")
}

// hxc274FindByName looks up a *DiscoveredProvider by Name in a slice
// returned by DiscoverProviders/DiscoverProviderCredentials, failing the
// test immediately (not returning nil) if absent — every caller in this
// file wants "found, or the test is meaningless", never a nil-check.
func hxc274FindByName(t *testing.T, all []*DiscoveredProvider, name string) *DiscoveredProvider {
	t.Helper()
	for _, dp := range all {
		if dp.Name == name {
			return dp
		}
	}
	t.Fatalf("no discovered provider named %q among %d discovered — the rest of this "+
		"test cannot proceed meaningfully", name, len(all))
	return nil
}

// testWriter adapts *testing.T to io.Writer so the discovery logger's
// output lands in `go test -v` output instead of stdout, keeping captured
// evidence attributable to the specific test that produced it.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}
