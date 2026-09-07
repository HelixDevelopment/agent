// Package localfirst holds the canonical local-first serving predicates
// (spec 002, operator mandate 2026-09-05, gap-ledger HA-F2-002).
//
// Everything served through HelixLLM runs locally via llama.cpp + Colibri,
// chosen per host hardware; cloud providers remain in the code but are
// DISABLED BY DEFAULT and require an explicit operator opt-in.
//
// This package is a LEAF: it imports nothing but the standard library, so it
// can be imported from both `internal/services` and `internal/verifier`
// (services imports verifier, so the predicate cannot live in services and be
// reachable from verifier — that cycle is why an earlier revision duplicated
// the switch as a local `getEnvBoolVerifier("USE_HELIX_LLM", true)` mirror in
// the verifier). ONE predicate, one meaning, every call site.
//
// The two switches:
//
//	USE_HELIX_LLM         — the LOCAL HelixLLM/llama.cpp chain.
//	                        Default ON (empty/unset or any value other than an
//	                        explicit false). Explicit "false"/"0"/"no"/"off"
//	                        (case-insensitive) opts OUT.
//	HELIX_CLOUD_PROVIDERS — every path by which a cloud provider becomes
//	                        reachable without the operator naming it:
//	                        registry auto-discovery, the credential-less
//	                        anonymous zen endpoint, env-credentialed provider
//	                        enablement, and boot-time verifier discovery.
//	                        Default OFF. Explicit "true"/"1"/"yes"/"on" opts IN.
package localfirst

import (
	"os"
	"strings"
)

// EnvCloudProviders is the env var that opts IN to cloud providers.
const EnvCloudProviders = "HELIX_CLOUD_PROVIDERS"

// EnvUseHelixLLM is the env var that opts OUT of the local HelixLLM chain.
const EnvUseHelixLLM = "USE_HELIX_LLM"

// CloudProvidersOptedIn reports whether the operator explicitly enabled cloud
// providers via HELIX_CLOUD_PROVIDERS.
//
// This is the SINGLE predicate every implicit cloud-acquisition path must
// consult. "Implicit" means: the operator supplied a credential (or nothing at
// all, in the anonymous-endpoint case) and the process decided on its own to
// build a client or send a request. An operator who names a provider
// explicitly in configuration has already made the decision this switch exists
// to ask about, and is not gated by it.
func CloudProvidersOptedIn() bool {
	return envExplicitTrue(EnvCloudProviders)
}

// HelixLLMEnabled reports whether the local HelixLLM/llama.cpp chain is
// enabled. Local-first: ON unless USE_HELIX_LLM is an explicit false.
func HelixLLMEnabled() bool {
	return !envExplicitFalse(EnvUseHelixLLM)
}

// envExplicitFalse reports whether key is set to an explicit negation.
// An empty/unset value is NOT a negation — the local-first default stands.
func envExplicitFalse(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "false", "0", "no", "off":
		return true
	default:
		return false
	}
}

// envExplicitTrue reports whether key is set to an explicit affirmation.
// An empty/unset value is NOT an affirmation — the default-deny stands.
func envExplicitTrue(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}
