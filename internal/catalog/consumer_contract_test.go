package catalog

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// HA-F2-005 — contract test for the exported consumer projections in
// consumer.go. The /v1/models facade (and any future consumer) relies on
// these filters to never widen a serving state; the contract is enforced
// here against hand-built entries covering every availability state.

func TestServingModels_FiltersToConfirmedServingOnly(t *testing.T) {
	entries := []Entry{
		{Name: "helixllm/served-a", Kind: KindModel, Provider: NameHelixLLM, Model: "served-a", Availability: AvailabilityServing, Enabled: true},
		{Name: "helixllm/withheld-b", Kind: KindModel, Provider: NameHelixLLM, Model: "withheld-b", Availability: AvailabilityWithheld, WithheldReason: ReasonProviderUnavailable},
		{Name: "helixllm/unreported-c", Kind: KindModel, Provider: NameHelixLLM, Model: "unreported-c", Availability: AvailabilityUnreported},
		{Name: "ensemble", Kind: KindEnsemble, Enabled: true},
		{Name: "anthropic", Kind: KindProvider, Provider: "anthropic", Enabled: true},
	}

	serving := ServingModels(entries)

	assert.Equal(t, []Entry{entries[0]}, serving,
		"only the entry the serving layer confirmed serving may pass; "+
			"withheld, unreported and non-availability-carrying entries must not")
}

func TestServingModels_EmptyInputIsHonestlyEmpty(t *testing.T) {
	assert.Empty(t, ServingModels(nil))
	assert.Empty(t, ServingModels([]Entry{}))
}

func TestHelixLLMOptions_SelectsHelixLLMModelOptionsAcrossStates(t *testing.T) {
	entries := []Entry{
		{Name: NameHelixLLM, Kind: KindProvider, Provider: NameHelixLLM, Enabled: true},
		{Name: "helixllm/served-a", Kind: KindModel, Provider: NameHelixLLM, Model: "served-a", Availability: AvailabilityServing, Enabled: true},
		{Name: "helixllm/withheld-b", Kind: KindModel, Provider: NameHelixLLM, Model: "withheld-b", Availability: AvailabilityWithheld, WithheldReason: ReasonInsufficientResources},
		{Name: "helixllm/unreported-c", Kind: KindModel, Provider: NameHelixLLM, Model: "unreported-c", Availability: AvailabilityUnreported},
		{Name: "anthropic/claude", Kind: KindModel, Provider: "anthropic", Model: "claude", Verified: true},
		{Name: "ensemble/majority_vote", Kind: KindEnsemble},
	}

	opts := HelixLLMOptions(entries)

	assert.Equal(t, []Entry{entries[1], entries[2], entries[3]}, opts,
		"all three availability states of the helixllm option set must pass — "+
			"each carries its own annotation; the provider root, other providers' "+
			"models and ensemble presets must not")
	for _, e := range opts {
		assert.Equal(t, NameHelixLLM, e.Provider)
		assert.Equal(t, KindModel, e.Kind)
	}
}
