package services

import (
	"os"
	"strings"
)

// Local-first serving defaults (spec 002, operator mandate 2026-09-05,
// gap-ledger HA-F2-002). Everything served through HelixLLM runs locally via
// llama.cpp + Colibri, chosen per host hardware; cloud providers remain in
// the code but are DISABLED BY DEFAULT and require an explicit operator
// opt-in. The two switches:
//
//	USE_HELIX_LLM        — the LOCAL HelixLLM/llama.cpp chain.
//	                       Default ON (empty/unset or any value other than an
//	                       explicit false). Explicit "false"/"0"/"no"/"off"
//	                       (case-insensitive) opts OUT.
//	HELIX_CLOUD_PROVIDERS — cloud provider auto-discovery (every env-
//	                       credentialed cloud provider + the credential-less
//	                       anonymous zen endpoint). Default OFF. Explicit
//	                       "true"/"1"/"yes"/"on" opts IN. Config-level
//	                       RegistryConfig.DisableAutoDiscovery remains an
//	                       unconditional kill switch regardless of the env.
func HelixLLMEnabledDefault() bool {
	return !envExplicitFalse("USE_HELIX_LLM")
}

// CloudProvidersOptedIn reports whether the operator explicitly enabled
// cloud provider auto-discovery via HELIX_CLOUD_PROVIDERS.
func CloudProvidersOptedIn() bool {
	return envExplicitTrue("HELIX_CLOUD_PROVIDERS")
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
