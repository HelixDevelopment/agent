package localfirst

import "testing"

// The two predicates are deliberately asymmetric: the cloud switch is
// default-DENY (only an explicit affirmation opens it) and the local switch is
// default-ALLOW (only an explicit negation closes it). Asserting both closed
// vocabularies here is what stops a future "helpful" normalisation — treating
// "1"/"yes" as the only truthy forms, or letting a stray "TRUE " with
// whitespace fall through to the default — from silently changing which side
// of the gate an operator lands on.
func TestCloudProvidersOptedIn(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		// Default-deny: unset and every non-affirmation stay closed.
		{"", false},
		{"false", false},
		{"0", false},
		{"no", false},
		{"off", false},
		{"maybe", false},
		{"TRUEISH", false},
		// Explicit affirmations, case- and whitespace-insensitive.
		{"true", true},
		{"TRUE", true},
		{"  True  ", true},
		{"1", true},
		{"yes", true},
		{"YES", true},
		{"on", true},
	} {
		t.Setenv(EnvCloudProviders, tc.value)
		if got := CloudProvidersOptedIn(); got != tc.want {
			t.Errorf("CloudProvidersOptedIn() with %s=%q = %v, want %v",
				EnvCloudProviders, tc.value, got, tc.want)
		}
	}
}

func TestHelixLLMEnabled(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		// Default-allow: unset and anything that is not an explicit negation
		// keep the local chain on.
		{"", true},
		{"true", true},
		{"1", true},
		{"yes", true},
		{"anything", true},
		// Explicit negations, case- and whitespace-insensitive.
		{"false", false},
		{"FALSE", false},
		{"  false  ", false},
		{"0", false},
		{"no", false},
		{"off", false},
	} {
		t.Setenv(EnvUseHelixLLM, tc.value)
		if got := HelixLLMEnabled(); got != tc.want {
			t.Errorf("HelixLLMEnabled() with %s=%q = %v, want %v",
				EnvUseHelixLLM, tc.value, got, tc.want)
		}
	}
}

// TestUnsetEnvIsLocalFirst pins the property that matters when neither switch
// is present at all — the out-of-the-box posture of a fresh deployment.
func TestUnsetEnvIsLocalFirst(t *testing.T) {
	t.Setenv(EnvCloudProviders, "")
	t.Setenv(EnvUseHelixLLM, "")
	if CloudProvidersOptedIn() {
		t.Error("with no env set, cloud must be OFF")
	}
	if !HelixLLMEnabled() {
		t.Error("with no env set, the local chain must be ON")
	}
}
