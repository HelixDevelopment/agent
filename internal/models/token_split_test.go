package models

import (
	"encoding/json"
	"testing"
)

// TestLLMResponse_TokenSplit covers the honest-usage accessor added
// 2026-09-03 to replace the fabricated `TokensUsed / 2` split that every
// OpenAI-compatible response shaper used to emit.
//
// The contract under test: report the provider's REAL per-direction
// counts when it supplied them (they live in Metadata, written by 30+
// providers under internal/llm/providers), and report honest zeros when
// it did not — never an invented half.
func TestLLMResponse_TokenSplit(t *testing.T) {
	tests := []struct {
		name           string
		resp           *LLMResponse
		wantPrompt     int
		wantCompletion int
		wantTotal      int
	}{
		{
			// The shape the live defect produced: a real asymmetric
			// split available in Metadata, discarded in favour of
			// 20/20/41. 7+34=41 so the correct answer is unambiguous.
			name: "real_split_from_metadata_survives",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"prompt_tokens":     7,
					"completion_tokens": 34,
					"total_tokens":      41,
				},
			},
			wantPrompt: 7, wantCompletion: 34, wantTotal: 41,
		},
		{
			// float64 is what int values become after a JSON round-trip
			// through map[string]interface{} — a real path whenever a
			// response is serialised and re-read.
			name: "float64_after_json_roundtrip",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"prompt_tokens":     float64(7),
					"completion_tokens": float64(34),
				},
			},
			wantPrompt: 7, wantCompletion: 34, wantTotal: 41,
		},
		{
			name: "json_number_from_decoder_usenumber",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"prompt_tokens":     json.Number("7"),
					"completion_tokens": json.Number("34"),
				},
			},
			wantPrompt: 7, wantCompletion: 34, wantTotal: 41,
		},
		{
			name: "int64_values",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"prompt_tokens":     int64(7),
					"completion_tokens": int64(34),
				},
			},
			wantPrompt: 7, wantCompletion: 34, wantTotal: 41,
		},
		{
			// No split reported: zeros, NOT 20/20. Zero is
			// OpenAI-legal and reads as "not reported"; a fabricated
			// half claims a measurement that never happened.
			name:           "no_metadata_reports_honest_zeros",
			resp:           &LLMResponse{TokensUsed: 41},
			wantPrompt:     0,
			wantCompletion: 0,
			wantTotal:      41,
		},
		{
			name: "empty_metadata_reports_honest_zeros",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata:   map[string]interface{}{},
			},
			wantPrompt: 0, wantCompletion: 0, wantTotal: 41,
		},
		{
			// A garbage value is treated as absent, never guessed at
			// (§11.4.6). Both directions unparseable ⇒ honest zeros.
			name: "unparseable_values_treated_as_absent",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"prompt_tokens":     "not-a-number",
					"completion_tokens": []int{1, 2},
				},
			},
			wantPrompt: 0, wantCompletion: 0, wantTotal: 41,
		},
		{
			// Negative counts are impossible; treat as unreported.
			name: "negative_values_treated_as_absent",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"prompt_tokens":     -5,
					"completion_tokens": -9,
				},
			},
			wantPrompt: 0, wantCompletion: 0, wantTotal: 41,
		},
		{
			// RECONCILED 2026-09-08 (§11.4.120). Now asserts: the
			// reported direction survives verbatim (7), the missing one
			// stays unknown (0), and the bare aggregate becomes the total.
			//
			// This case previously demanded completion=34 be DERIVED as
			// 41 − 7 from the bare TokensUsed aggregate. That derivation
			// is unsafe and was correctly refused by the accessor:
			// TokensUsed has no total-semantics guarantee (the
			// pre-2026-09-03 claude provider stored the OUTPUT count
			// there, and those rows are persisted + reloaded), so on a
			// legacy row the "arithmetic" publishes a fabricated number.
			// Only an EXPLICIT total_tokens key licenses the subtraction
			// — see partial_report_with_explicit_total_derives_the_rest.
			name: "only_prompt_reported_survives_without_derivation",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"prompt_tokens": 7,
				},
			},
			wantPrompt: 7, wantCompletion: 0, wantTotal: 41,
		},
		{
			// Same rule in the other direction: the measured completion
			// side is kept, the unmeasured prompt side stays 0.
			name: "only_completion_reported_survives_without_derivation",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"completion_tokens": 34,
				},
			},
			wantPrompt: 0, wantCompletion: 34, wantTotal: 41,
		},
		{
			// RECONCILED 2026-09-08 (§11.4.120). Now asserts: a malformed
			// aggregate is discarded, the measurement is kept.
			//
			// This case previously expected (0, 0, 3) — it threw away the
			// good measured datum (prompt=7) and published the bad one
			// (total=3) instead. That is backwards: a total smaller than
			// a part it contains is impossible, which makes the AGGREGATE
			// the untrustworthy value, not the measurement. The total
			// falls back to the known part so the envelope cannot
			// self-contradict.
			name: "malformed_smaller_aggregate_is_discarded_not_the_measurement",
			resp: &LLMResponse{
				TokensUsed: 3,
				Metadata: map[string]interface{}{
					"prompt_tokens": 7,
				},
			},
			wantPrompt: 7, wantCompletion: 0, wantTotal: 7,
		},
		{
			// Partial report with no aggregate at all: the one known
			// part is all we have, so it becomes the total.
			name: "only_prompt_reported_no_aggregate",
			resp: &LLMResponse{
				Metadata: map[string]interface{}{
					"prompt_tokens": 7,
				},
			},
			wantPrompt: 7, wantCompletion: 0, wantTotal: 7,
		},
		{
			// Anthropic-shaped naming. internal/llm/providers/cohere
			// writes input_tokens + output_tokens; reading only the
			// OpenAI-shaped keys would return honest-looking zeros
			// while a REAL split sat available in the map — a subtler
			// form of the fabrication defect this accessor exists to
			// end.
			name: "anthropic_shaped_input_output_keys",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"input_tokens":  7,
					"output_tokens": 34,
				},
			},
			wantPrompt: 7, wantCompletion: 34, wantTotal: 41,
		},
		{
			// The claude provider now writes BOTH input_tokens and
			// output_tokens with TokensUsed as the true total (it used
			// to write input only, with TokensUsed holding the OUTPUT
			// count — so a Claude response silently under-reported
			// usage by the entire input side).
			name: "claude_shape_both_directions",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"model":         "claude-x",
					"input_tokens":  7,
					"output_tokens": 34,
				},
			},
			wantPrompt: 7, wantCompletion: 34, wantTotal: 41,
		},
		{
			// RECONCILED 2026-09-08 (§11.4.120). Now asserts: the measured
			// input side survives, the output side stays unknown.
			//
			// A legacy/stored response predating that provider fix carries
			// input_tokens only. This case previously expected the output
			// side to be recovered as 41 − 7 = 34 from the aggregate —
			// which happens to look right ONLY because this fixture's
			// TokensUsed is a true total. On a genuine legacy row it is
			// not: `git show eaa73056^:internal/llm/providers/claude/
			// claude.go` sets TokensUsed to Usage.OutputTokens, so the
			// real shape is input=7 / TokensUsed=34, and the same
			// subtraction publishes 7/27/34 against a truth of 7/34/41.
			// The accessor cannot tell the two shapes apart, so it must
			// not derive from either.
			name: "legacy_input_only_survives_without_derivation",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"model":        "claude-x",
					"input_tokens": 7,
				},
			},
			wantPrompt: 7, wantCompletion: 0, wantTotal: 41,
		},
		{
			// The genuine legacy-claude row the comment above describes,
			// asserted directly: TokensUsed holds the OUTPUT count, so it
			// is not a total at all. The prompt side (measured) survives;
			// nothing is invented for the output side.
			name: "legacy_claude_row_where_tokensused_is_the_output_count",
			resp: &LLMResponse{
				TokensUsed: 34,
				Metadata: map[string]interface{}{
					"model":        "claude-x",
					"input_tokens": 7,
				},
			},
			wantPrompt: 7, wantCompletion: 0, wantTotal: 34,
		},
		{
			// The ONLY licensed derivation: an explicit total_tokens key
			// is written solely by a provider that parsed a real usage
			// object, so total = prompt + completion holds by
			// construction and the third quantity is determined. 41−7=34.
			name: "partial_report_with_explicit_total_derives_the_rest",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"prompt_tokens": 7,
					"total_tokens":  41,
				},
			},
			wantPrompt: 7, wantCompletion: 34, wantTotal: 41,
		},
		{
			// Same, other direction, and with no TokensUsed at all — the
			// explicit total stands alone.
			name: "partial_completion_with_explicit_total_derives_prompt",
			resp: &LLMResponse{
				Metadata: map[string]interface{}{
					"completion_tokens": 34,
					"total_tokens":      41,
				},
			},
			wantPrompt: 7, wantCompletion: 34, wantTotal: 41,
		},
		{
			// A malformed EXPLICIT total (smaller than the measured part)
			// licenses no derivation either. The measurement survives and
			// the total falls back to the usable aggregate.
			name: "explicit_total_smaller_than_known_part_derives_nothing",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"prompt_tokens": 7,
					"total_tokens":  3,
				},
			},
			wantPrompt: 7, wantCompletion: 0, wantTotal: 41,
		},
		{
			// Boundary: an explicit total EQUAL to the known part is
			// well-formed, and determines the missing direction as
			// exactly zero — a measured zero, not an unknown.
			name: "explicit_total_equal_to_known_part_yields_zero_completion",
			resp: &LLMResponse{
				TokensUsed: 7,
				Metadata: map[string]interface{}{
					"prompt_tokens": 7,
					"total_tokens":  7,
				},
			},
			wantPrompt: 7, wantCompletion: 0, wantTotal: 7,
		},
		{
			// Anthropic-shaped partial report resolves identically — the
			// alias set is read for the total key's siblings too.
			name: "anthropic_partial_output_only_survives",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"output_tokens": 34,
				},
			},
			wantPrompt: 0, wantCompletion: 34, wantTotal: 41,
		},
		{
			// A partial report whose sibling direction is present but
			// UNPARSEABLE is still a partial report: the good value
			// survives, the garbage one is treated as absent, never
			// guessed at.
			name: "partial_with_unparseable_sibling_keeps_the_good_value",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"prompt_tokens":     7,
					"completion_tokens": "not-a-number",
				},
			},
			wantPrompt: 7, wantCompletion: 0, wantTotal: 41,
		},
		{
			// Zero is a legal measured value. A reported prompt_tokens=0
			// alongside a real completion count must not be mistaken for
			// "unreported" — the completion side still survives.
			name: "zero_valued_reported_direction_is_not_a_missing_one",
			resp: &LLMResponse{
				TokensUsed: 34,
				Metadata: map[string]interface{}{
					"prompt_tokens":     0,
					"completion_tokens": 34,
				},
			},
			wantPrompt: 0, wantCompletion: 34, wantTotal: 34,
		},
		{
			// OpenAI-shaped keys win over Anthropic aliases when both
			// somehow appear, so precedence is deterministic and never
			// depends on map iteration order (§11.4.6).
			name: "openai_keys_take_precedence_over_aliases",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"prompt_tokens":     7,
					"completion_tokens": 34,
					"input_tokens":      1000,
					"output_tokens":     2000,
				},
			},
			wantPrompt: 7, wantCompletion: 34, wantTotal: 41,
		},
		{
			// Provider's own total disagrees with its own parts — trust
			// the parts so prompt+completion==total always holds.
			name: "inconsistent_provider_total_is_derived_from_parts",
			resp: &LLMResponse{
				TokensUsed: 999,
				Metadata: map[string]interface{}{
					"prompt_tokens":     7,
					"completion_tokens": 34,
					"total_tokens":      12345,
				},
			},
			wantPrompt: 7, wantCompletion: 34, wantTotal: 41,
		},
		{
			name:       "nil_receiver_is_all_zeros",
			resp:       nil,
			wantPrompt: 0, wantCompletion: 0, wantTotal: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt, completion, total := tt.resp.TokenSplit()

			if prompt != tt.wantPrompt {
				t.Errorf("prompt = %d, want %d", prompt, tt.wantPrompt)
			}
			if completion != tt.wantCompletion {
				t.Errorf("completion = %d, want %d", completion, tt.wantCompletion)
			}
			if total != tt.wantTotal {
				t.Errorf("total = %d, want %d", total, tt.wantTotal)
			}

			// The fabrication signature: both directions equal AND
			// non-zero. Never legal output for a real split unless the
			// provider genuinely reported equal counts (no fixture here
			// does), so this catches a reinstated halving directly.
			if prompt != 0 && prompt == completion && tt.wantPrompt != tt.wantCompletion {
				t.Errorf(
					"prompt == completion == %d looks like a fabricated 50/50 split", prompt)
			}
			// When BOTH directions are known the envelope must add up —
			// the exact invariant the fabricated 50/50 split violated
			// on odd totals (20 + 20 != 41). A partial report is
			// exempt: one direction is genuinely unknown there, and the
			// provider's aggregate is kept rather than understated.
			if prompt != 0 && completion != 0 && prompt+completion != total {
				t.Errorf("inconsistent envelope: %d + %d != %d", prompt, completion, total)
			}
		})
	}
}
