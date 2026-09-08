package models

import (
	"os"
	"testing"
)

// TestTokenSplit_PartialReportIsNotErased is the §11.4.115 polarity test
// for the partial-report ERASURE defect in LLMResponse.TokenSplit.
//
// THE DEFECT. When a provider reported exactly ONE direction and no
// explicit "total_tokens" key, the accessor returned:
//
//	return 0, 0, total
//
// discarding the direction the provider actually measured. That is the
// same class of under-reporting this accessor was written to end — the
// pre-2026-09-03 claude provider recorded only "input_tokens" in
// Metadata (verified: `git show eaa73056^:internal/llm/providers/claude/
// claude.go` sets `TokensUsed: claudeResp.Usage.OutputTokens` while
// writing input_tokens alone), and such rows are persisted in the
// `tokens_used` column and re-SELECTed by
// internal/database/response_repository.go. For one of those rows the
// erasing branch published prompt=0 for a response whose prompt side was
// measured at 7 — a real number replaced by a false "not reported".
//
// It also contradicted the function's own documented purpose: "report
// the provider's REAL per-direction counts when it supplied them".
//
// WHY NOT DERIVE INSTEAD. Deriving the missing direction from the bare
// TokensUsed aggregate is the OPPOSITE error and is equally forbidden:
// TokensUsed carries no total-semantics guarantee, so for that same
// legacy claude row (input=7, TokensUsed=34-the-OUTPUT) deriving would
// publish 7/27/34 against a truth of 7/34/41 — a fabricated 27. The
// honest rule keeps the measured datum and leaves the unmeasured one at
// 0 (= "not reported", OpenAI-legal), which is strictly more information
// than the erasure and strictly less invention than the derivation.
//
// POLARITY. RED_MODE=1 reproduces the defect on a pre-fix build: it
// asserts the reported direction IS erased to 0. RED_MODE unset/"0" (the
// default, and the standing GREEN regression guard per §11.4.135)
// asserts the reported direction survives verbatim.
//
// Fixtures use an ASYMMETRIC non-zero reported direction so that erased
// and preserved outputs can never coincide.
func TestTokenSplit_PartialReportIsNotErased(t *testing.T) {
	redMode := os.Getenv("RED_MODE") == "1"

	cases := []struct {
		name string
		resp *LLMResponse
		// The direction the provider genuinely reported, and which the
		// pre-fix build erased to 0.
		wantReported int
		// Reads the direction under test out of the accessor's results.
		pick func(prompt, completion int) int
	}{
		{
			// OpenAI-shaped partial report, aggregate present but not an
			// explicit total_tokens key.
			name: "openai_prompt_only_with_bare_aggregate",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"prompt_tokens": 7,
				},
			},
			wantReported: 7,
			pick:         func(prompt, _ int) int { return prompt },
		},
		{
			name: "openai_completion_only_with_bare_aggregate",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"completion_tokens": 34,
				},
			},
			wantReported: 34,
			pick:         func(_, completion int) int { return completion },
		},
		{
			// The exact legacy-claude persisted shape described above.
			name: "anthropic_legacy_input_only",
			resp: &LLMResponse{
				TokensUsed: 41,
				Metadata: map[string]interface{}{
					"model":        "claude-x",
					"input_tokens": 7,
				},
			},
			wantReported: 7,
			pick:         func(prompt, _ int) int { return prompt },
		},
		{
			// Malformed aggregate (smaller than the measured part). The
			// aggregate is the untrustworthy value here, so it is the
			// aggregate that must be discarded — never the measurement.
			name: "prompt_only_with_malformed_smaller_aggregate",
			resp: &LLMResponse{
				TokensUsed: 3,
				Metadata: map[string]interface{}{
					"prompt_tokens": 7,
				},
			},
			wantReported: 7,
			pick:         func(prompt, _ int) int { return prompt },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompt, completion, total := tc.resp.TokenSplit()
			got := tc.pick(prompt, completion)

			if redMode {
				// Reproduce-and-assert-defect-present.
				if got != 0 {
					t.Fatalf(
						"RED_MODE=1: expected the reported direction to be ERASED to 0 by the "+
							"pre-fix partial branch, but got %d (prompt=%d completion=%d total=%d). "+
							"The defect did NOT reproduce — this is a FINDING, not evidence of a "+
							"fix: either the erasure is already repaired in this build (run without "+
							"RED_MODE for the GREEN guard) or the fixture no longer reaches the "+
							"partial-report branch.",
						got, prompt, completion, total,
					)
				}
				t.Logf(
					"RED_MODE=1: reproduced the partial-report erasure — provider reported %d "+
						"for this direction, accessor published prompt=%d completion=%d total=%d.",
					tc.wantReported, prompt, completion, total,
				)
				return
			}

			// GREEN guard: the measured direction survives verbatim.
			if got != tc.wantReported {
				t.Errorf(
					"reported direction = %d, want %d (provider-measured, must never be erased); "+
						"full split prompt=%d completion=%d total=%d",
					got, tc.wantReported, prompt, completion, total,
				)
			}

			// The unmeasured direction stays 0 — deriving it from the bare
			// TokensUsed aggregate would be the fabrication described above.
			other := prompt + completion - got
			if other != 0 {
				t.Errorf(
					"unreported direction = %d, want 0; with no explicit total_tokens the missing "+
						"direction is NOT derivable from the bare TokensUsed aggregate, which "+
						"carries no total-semantics guarantee (legacy claude rows hold the OUTPUT "+
						"count there)",
					other,
				)
			}

			// The emitted envelope must never contradict itself: a total
			// smaller than a part it contains is impossible.
			if total < got {
				t.Errorf("total = %d is smaller than its own measured part %d", total, got)
			}
		})
	}
}
