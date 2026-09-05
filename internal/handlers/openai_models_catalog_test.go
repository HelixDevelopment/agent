package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"dev.helix.agent/internal/catalog"
)

// HA-F2-001 GREEN guard (catalog-wired) — lands WITH the fix; exercises the
// SetCatalogService seam. Uses a fake HelixLLMSource so no serving layer is
// required (§11.4.27 unit-tier isolation; the contract these entries carry
// is produced by the catalog's own translation, covered by its tests).

type fakeHelixLLMSource struct{ opts []catalog.HelixLLMOption }

func (f fakeHelixLLMSource) HelixLLMOptions() []catalog.HelixLLMOption { return f.opts }

func newCatalogForFacadeTest(opts ...catalog.HelixLLMOption) *catalog.CatalogService {
	return catalog.New(catalog.Options{
		HelixLLMEnabled: true,
		HelixLLM:        fakeHelixLLMSource{opts: opts},
	})
}

func TestModelsFacade_ServingBackendMakesSelectorsUsable(t *testing.T) {
	h := NewUnifiedHandler(nil, nil)
	h.SetCatalogService(newCatalogForFacadeTest(catalog.HelixLLMOption{
		ID:           "coder-7b",
		ModelIdentity: "helixllm/localhost/coder-7b",
		Host:         "localhost",
		Availability: catalog.AvailabilityServing,
	}))

	data := callUnifiedModels(t, h)

	// The serving layer's own option is appended, annotated, usable.
	served := findModelByID(t, data, "helixllm/coder-7b")
	assert.Equal(t, "serving", served["availability"])
	perm := permissionOf(t, served)
	assert.Equal(t, true, perm["allow_sampling"], "a serving option advertises sampling")
	assert.Equal(t, true, perm["allow_create_engine"])

	// Any serving model makes the debate selectors usable, with NO
	// availability field (the permission block is the claim).
	for _, id := range []string{"helixagent-debate", "helixagent-ensemble", "helix-debate"} {
		m := findModelByID(t, data, id)
		_, hasAvailability := m["availability"]
		assert.False(t, hasAvailability, "%q usable → no availability field", id)
		p := permissionOf(t, m)
		assert.Equal(t, true, p["allow_sampling"], "%q has a serving backend", id)
		assert.Equal(t, true, p["allow_create_engine"])
	}

	// A serving helixllm option makes the llm-chain selectors usable.
	for _, id := range []string{"helixagent-llm", "helix-llm"} {
		m := findModelByID(t, data, id)
		p := permissionOf(t, m)
		assert.Equal(t, true, p["allow_sampling"], "%q has a serving helixllm backend", id)
	}
}

func TestModelsFacade_WithheldOptionStaysListedButNotUsable(t *testing.T) {
	h := NewUnifiedHandler(nil, nil)
	h.SetCatalogService(newCatalogForFacadeTest(catalog.HelixLLMOption{
		ID:             "big-32b",
		ModelIdentity:  "helixllm/localhost/big-32b",
		Host:           "localhost",
		Availability:   catalog.AvailabilityWithheld,
		WithheldReason: catalog.ReasonProviderUnavailable,
	}))

	data := callUnifiedModels(t, h)

	withheld := findModelByID(t, data, "helixllm/big-32b")
	assert.Equal(t, "withheld", withheld["availability"])
	assert.Equal(t, "provider_unavailable", withheld["withheld_reason"],
		"the catalog's closed-set reason is carried through unaltered")
	perm := permissionOf(t, withheld)
	assert.Equal(t, false, perm["allow_sampling"], "a withheld option never advertises sampling")

	// Nothing is serving, so every selector is withheld with the reason.
	for _, id := range facadePseudoModelIDs {
		m := findModelByID(t, data, id)
		assert.Equal(t, "withheld", m["availability"], "%q has no serving backend", id)
		assert.Equal(t, "no_serving_backend", m["withheld_reason"])
		p := permissionOf(t, m)
		assert.Equal(t, false, p["allow_sampling"])
		assert.Equal(t, false, p["allow_create_engine"])
	}
}

func TestModelsFacade_UnreportedOptionCarriesNoServingClaim(t *testing.T) {
	h := NewUnifiedHandler(nil, nil)
	h.SetCatalogService(newCatalogForFacadeTest(catalog.HelixLLMOption{
		ID: "legacy-x",
		// Availability zero value — nothing reported about serving state.
	}))

	data := callUnifiedModels(t, h)

	m := findModelByID(t, data, "helixllm/legacy-x")
	assert.Equal(t, "unreported", m["availability"])
	_, hasReason := m["withheld_reason"]
	assert.False(t, hasReason, "unreported is not a withholding — no reason may attach")
	perm := permissionOf(t, m)
	assert.Equal(t, false, perm["allow_sampling"])

	// A model that reports nothing is not a serving backend either.
	for _, id := range facadePseudoModelIDs {
		sel := findModelByID(t, data, id)
		assert.Equal(t, "withheld", sel["availability"],
			"%q: an unreported option confirms no serving backend", id)
	}
}

func TestModelsFacade_NoOptionsHonestEmptyFacade(t *testing.T) {
	h := NewUnifiedHandler(nil, nil)
	h.SetCatalogService(newCatalogForFacadeTest())

	data := callUnifiedModels(t, h)
	require.NotEmpty(t, data, "the five selectors are always listed")

	for _, m := range data {
		assert.Equal(t, "withheld", m["availability"],
			"a wired catalog with zero options confirms nothing is serving")
	}
}
