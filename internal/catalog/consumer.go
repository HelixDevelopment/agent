package catalog

// Consumer-side projections (HA-F2-005). These are the exported, reusable
// filters a consuming surface — today the /v1/models facade (HA-F2-001,
// FR-019), tomorrow any other consumer — applies to a built catalog to
// decide what the serving layer actually confirmed. Consumed by reference
// per CONST-051: the catalog stays decoupled from every consumer.
//
// The filters never widen a state: an entry carrying no serving claim
// (AvailabilityUnreported) or a withholding is never returned as serving,
// and usability flows ONLY through Availability.Usable() (§11.4.6 — the
// absence of a serving claim is not a serving claim).

// ServingModels returns only the entries the serving layer confirmed
// serving (AvailabilityServing). Everything else — unreported, withheld —
// is excluded: a consumer must never present a non-served entry as
// selectable on the strength of this filter.
func ServingModels(entries []Entry) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Availability.Usable() {
			out = append(out, e)
		}
	}
	return out
}

// HelixLLMOptions returns the HelixLLM-served model option entries from a
// built catalog — serving, withheld and unreported alike, each carrying its
// own availability annotation. The /v1/models facade appends these to its
// listing so the wire-visible surface matches what the serving layer is
// actually offering; each entry's usability is decided by its annotation,
// never by its presence in the list.
//
// UNCONFIRMED: how a [:<variant>] suffix on ModelIdentity is handled
// downstream of the facade is not yet exercised by any consumer; entries
// are keyed by Name (helixllm/<id>) here and ModelIdentity is a label only.
func HelixLLMOptions(entries []Entry) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Provider == NameHelixLLM && e.Kind == KindModel {
			out = append(out, e)
		}
	}
	return out
}
