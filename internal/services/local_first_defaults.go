package services

import (
	"dev.helix.agent/internal/localfirst"
)

// Local-first serving defaults (spec 002, operator mandate 2026-09-05,
// gap-ledger HA-F2-002). Everything served through HelixLLM runs locally via
// llama.cpp + Colibri, chosen per host hardware; cloud providers remain in
// the code but are DISABLED BY DEFAULT and require an explicit operator
// opt-in.
//
// The predicates themselves live in the leaf package internal/localfirst so
// that internal/verifier can consult the SAME predicate: services imports
// verifier, so verifier cannot import services, and an earlier revision of
// this change worked around that with a duplicated env-parsing mirror in the
// verifier. One predicate, one meaning, every call site.
//
//	USE_HELIX_LLM         — the LOCAL HelixLLM/llama.cpp chain.
//	                        Default ON (empty/unset or any value other than an
//	                        explicit false). Explicit "false"/"0"/"no"/"off"
//	                        (case-insensitive) opts OUT.
//	HELIX_CLOUD_PROVIDERS — every path by which a cloud provider becomes
//	                        reachable without the operator naming it. Default
//	                        OFF; explicit "true"/"1"/"yes"/"on" opts IN.
//	                        Config-level RegistryConfig.DisableAutoDiscovery
//	                        remains an unconditional kill switch for
//	                        AUTO-DISCOVERY SPECIFICALLY, regardless of the env.
//	                        It is not a cloud kill switch: with the opt-in set,
//	                        DisableAutoDiscovery = true still leaves the
//	                        env-credentialed provider route open — which is
//	                        exactly what TestCloudGate_OptInStillOpensTheCloudRoute
//	                        asserts.
func HelixLLMEnabledDefault() bool {
	return localfirst.HelixLLMEnabled()
}

// CloudProvidersOptedIn reports whether the operator explicitly enabled
// cloud providers via HELIX_CLOUD_PROVIDERS.
//
// Call sites (each gates an IMPLICIT acquisition — the process deciding on its
// own, from an ambient credential or from no credential at all, to build a
// cloud client or send a cloud request):
//
//  1. NewProviderRegistry               — registry auto-discovery + the
//     credential-less anonymous zen default.
//  2. LoadRegistryConfigFromAppConfig   — env-credentialed provider
//     enablement (deepseek/claude/gemini/qwen/openrouter).
//  3. verifier.discoverProviders        — boot-time discovery + verification
//     (via localfirst.CloudProvidersOptedIn).
//  4. NewEmbeddingManager               — the ambient OPENAI_API_KEY embedding
//     path behind POST /v1/protocols/execute.
//  5. router.newOAuthCredentialManager  — the OAuth refresh ticker started
//     from the mere PRESENCE of ~/.claude/.credentials.json or
//     ~/.qwen/oauth_creds.json (via localfirst.CloudProvidersOptedIn).
//
// NOT gated, deliberately: a provider the operator names explicitly in the
// registry config. That operator has already made the decision this switch
// exists to ask about.
func CloudProvidersOptedIn() bool {
	return localfirst.CloudProvidersOptedIn()
}
