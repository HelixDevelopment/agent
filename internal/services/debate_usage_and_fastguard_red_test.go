package services

import (
	"testing"
	"time"

	"dev.helix.agent/internal/models"
)

// §11.4.115 RED-baseline-on-the-broken-artifact polarity tests for the two
// debate-path defects measured live on 2026-09-07 against HelixAgent on
// :7061 (qwen2.5-coder-3b-instruct-q4_k_m served by the local `helixllm`
// provider).
//
// ONE source, TWO roles (§11.4.115): RED_MODE=1 reproduces the
// defect on the PRE-FIX artifact and asserts it is PRESENT; the default (RED_MODE
// unset) turns
// the same source into the standing GREEN regression guard asserting the
// defect is ABSENT. There is no separate happy-path test — the bug-catcher IS
// the regression guard.
//
// Captured defect evidence (journalctl --user -u helixagent.service):
//
//	msg="Suspiciously fast response detected - may be cached error, triggering
//	     fallback" content_length=5 response_preview=Paris response_time_ms=82
//	     provider=helixllm model=qwen2.5-coder-3b-instruct-q4_k_m
//	msg="Primary LLM failed, attempting fallback chain"
//	     error="[Helixllm-1] suspiciously fast response (82.172844ms) ..."
//
// and, from a direct probe of POST /v1/chat/completions:
//
//	model=helixagent-debate  usage={prompt_tokens:0 completion_tokens:0
//	                                total_tokens:613}   reply='Paris'
// This file reuses the package-level redMode() helper
// (protocol_security_hxc221_red_test.go): RED_MODE=1 selects the
// defect-reproduction polarity; anything else is the GREEN regression guard.

// --------------------------------------------------------------------------
// Defect 1 — the fast-response guard rejects CORRECT answers from a LOCAL
// model. debate_service.go:163 keys "did real work happen?" off wall-clock
// latency alone, an inference that only holds for a REMOTE provider where
// network RTT already exceeds the threshold. A loopback 3B model answers
// "What is the capital of France?" in 40-90ms with 5 characters, so every
// short correct answer was reported as a participant FAILURE.
// --------------------------------------------------------------------------

func TestFastResponseGuard_DoesNotRejectCorrectLocalAnswer(t *testing.T) {
	// The exact shape measured live: a correct, complete, non-canned answer
	// from a local model, WITH the provider reporting real token usage.
	const (
		measuredLatency = 82 * time.Millisecond // captured: response_time_ms=82
		measuredLength  = 5                     // captured: content_length=5 ("Paris")
		measuredTokens  = 206                   // captured: tokens_used=206
	)

	rejected := IsNonGenuineFastResponse(measuredLatency, measuredLength, measuredTokens)

	if redMode() {
		if !rejected {
			t.Fatalf("RED_MODE=1: expected the PRE-FIX guard to REJECT a correct "+
				"local answer (latency=%v len=%d tokens=%d) — the defect is not "+
				"present on this artifact", measuredLatency, measuredLength, measuredTokens)
		}
		t.Logf("RED confirmed: a correct local answer (%v, %d chars, %d tokens "+
			"reported) is rejected as 'suspiciously fast'",
			measuredLatency, measuredLength, measuredTokens)
		return
	}

	if rejected {
		t.Fatalf("GREEN: a correct local answer (latency=%v len=%d tokens=%d) "+
			"must NOT be rejected — the provider reported %d tokens of real work",
			measuredLatency, measuredLength, measuredTokens, measuredTokens)
	}
}

// The guard must still catch what it was built for (commit 5d5dbb7b, "Canned
// Response Detection"): a short reply returned with NO evidence of work. This
// half of the contract holds in BOTH polarities — a fix that silences the
// guard entirely would be a weakening, not a fix.
func TestFastResponseGuard_StillCatchesWorklessStub(t *testing.T) {
	if IsNonGenuineFastResponse(5*time.Millisecond, 12, 0) != true {
		t.Fatal("a fast, short reply with ZERO reported tokens must still be " +
			"rejected — this is the cached-error / stub case the guard exists for")
	}
	if IsNonGenuineFastResponse(2*time.Second, 12, 0) != false {
		t.Fatal("a slow reply must never be rejected by the fast-response guard")
	}
	if IsNonGenuineFastResponse(5*time.Millisecond, 4096, 0) != false {
		t.Fatal("a long reply must never be rejected by the fast-response guard")
	}
}

// --------------------------------------------------------------------------
// Defect 2 — the debate/ensemble envelope reports prompt_tokens=0 and
// completion_tokens=0 beside a real total. The per-participant split IS
// available on each provider response, but ParticipantResponse.Metadata
// recorded only the aggregate "tokens_used", so nothing downstream could sum
// the directions. This asserts the AGGREGATION, never a fabricated split.
// --------------------------------------------------------------------------

// Layer 1 of the aggregation — the point where the provider's real split is
// copied onto the ParticipantResponse. This test exists because the paired
// §1.1 mutation of participantUsageMetadata initially did NOT make any guard
// fail: the other tests construct ParticipantResponse values directly and so
// never exercised this hop, leaving the exact line that dropped the split
// unguarded (a §11.4.125-class finding raised by the mutation itself).
func TestParticipantUsageMetadata_CarriesProviderReportedSplit(t *testing.T) {
	resp := &models.LLMResponse{
		TokensUsed: 206,
		Metadata: map[string]interface{}{
			"prompt_tokens":     200,
			"completion_tokens": 6,
		},
	}
	md := participantUsageMetadata(resp, map[string]any{"finish_reason": "stop"})

	if md["prompt_tokens"] != 200 || md["completion_tokens"] != 6 {
		t.Fatalf("the provider's real split must be carried onto the participant "+
			"metadata: got prompt=%v completion=%v", md["prompt_tokens"], md["completion_tokens"])
	}
	if md["tokens_used"] != 206 {
		t.Fatalf("aggregate must survive: got %v, want 206", md["tokens_used"])
	}
	if md["finish_reason"] != "stop" {
		t.Fatal("caller-supplied metadata must be preserved")
	}
}

// The anti-invention half at layer 1: a provider that reported no split must
// leave the split keys ABSENT, never zero-filled or halved.
func TestParticipantUsageMetadata_OmitsSplitWhenProviderReportedNone(t *testing.T) {
	resp := &models.LLMResponse{TokensUsed: 300} // aggregate only
	md := participantUsageMetadata(resp, nil)

	if _, ok := md["prompt_tokens"]; ok {
		t.Fatal("no split reported by the provider — prompt_tokens must be ABSENT, not invented")
	}
	if _, ok := md["completion_tokens"]; ok {
		t.Fatal("no split reported by the provider — completion_tokens must be ABSENT, not invented")
	}
	if md["tokens_used"] != 300 {
		t.Fatalf("aggregate must survive intact: got %v, want 300", md["tokens_used"])
	}
}

func participantWithUsage(id string, prompt, completion int) ParticipantResponse {
	return ParticipantResponse{
		ParticipantID: id,
		Content:       "Paris",
		Metadata: map[string]any{
			"tokens_used":       prompt + completion,
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
		},
	}
}

func TestDebateEnsembleUsage_AggregatesRealParticipantSplit(t *testing.T) {
	best := participantWithUsage("p1", 200, 6)
	dr := &DebateResult{
		DebateID:     "red-usage",
		BestResponse: &best,
		AllResponses: []ParticipantResponse{
			participantWithUsage("p1", 200, 6),
			participantWithUsage("p2", 199, 6),
			participantWithUsage("p3", 201, 7),
		},
	}
	wantPrompt, wantCompletion := 600, 19
	wantTotal := wantPrompt + wantCompletion

	e := &AgenticEnsemble{}
	result := e.debateResultToEnsemble(dr, AgenticModeReason)
	if result == nil || result.Selected == nil {
		t.Fatal("debateResultToEnsemble returned no selected response")
	}
	prompt, completion, total := result.Selected.TokenSplit()

	if redMode() {
		if prompt != 0 || completion != 0 {
			t.Fatalf("RED_MODE=1: expected the PRE-FIX envelope to report a ZERO "+
				"split beside a real total, got prompt=%d completion=%d total=%d "+
				"— the defect is not present on this artifact",
				prompt, completion, total)
		}
		t.Logf("RED confirmed: usage reports prompt=0 completion=0 total=%d while "+
			"the participants really consumed %d/%d", total, wantPrompt, wantCompletion)
		return
	}

	if prompt != wantPrompt || completion != wantCompletion {
		t.Fatalf("GREEN: aggregated split wrong: got prompt=%d completion=%d, "+
			"want prompt=%d completion=%d", prompt, completion, wantPrompt, wantCompletion)
	}
	if total != wantTotal {
		t.Fatalf("GREEN: envelope must add up: got total=%d, want %d", total, wantTotal)
	}
}

// A participant whose provider reported NO split must never be papered over
// with an invented one. When the aggregate split cannot account for the whole
// total, the honest answer is to publish the total alone (0/0/total) rather
// than a figure that silently loses another participant's tokens. Holds in
// BOTH polarities: this is the anti-invention half of the contract.
func TestDebateEnsembleUsage_NeverInventsAMissingSplit(t *testing.T) {
	best := participantWithUsage("p1", 200, 6)
	dr := &DebateResult{
		DebateID:     "red-usage-partial",
		BestResponse: &best,
		AllResponses: []ParticipantResponse{
			participantWithUsage("p1", 200, 6),
			// p2's provider reported only an aggregate — no split available.
			{ParticipantID: "p2", Content: "Paris",
				Metadata: map[string]any{"tokens_used": 300}},
		},
	}

	e := &AgenticEnsemble{}
	result := e.debateResultToEnsemble(dr, AgenticModeReason)
	if result == nil || result.Selected == nil {
		t.Fatal("debateResultToEnsemble returned no selected response")
	}
	prompt, completion, total := result.Selected.TokenSplit()

	if prompt != 0 || completion != 0 {
		t.Fatalf("a partial split must NOT be published (it would understate both "+
			"directions and drop p2's 300 tokens from the total): got prompt=%d "+
			"completion=%d total=%d", prompt, completion, total)
	}
	if total != 506 {
		t.Fatalf("the real total must survive intact: got %d, want 506", total)
	}
}

// --------------------------------------------------------------------------
// §11.4.146 STEP 3 — fan-out across the case space of the same functionality.
// Enumerated cases with per-case outcome, so "no other issues" is provable
// rather than asserted (§11.4.118).
// --------------------------------------------------------------------------

func TestFastResponseGuard_CaseSpace(t *testing.T) {
	const thresholdLatency = 100 * time.Millisecond
	const thresholdLength = 100

	cases := []struct {
		name      string
		latency   time.Duration
		length    int
		tokens    int
		reject    bool
		rationale string
	}{
		// The measured live shapes.
		{"local-correct-answer-40ms", 40 * time.Millisecond, 5, 206, false,
			"genuine local generation, provider accounted for the work"},
		{"local-correct-answer-82ms", 82 * time.Millisecond, 5, 206, false, "same, at the captured 82ms"},
		{"workless-stub", 1 * time.Millisecond, 12, 0, true, "the cached/canned case the guard exists for"},

		// Latency boundary — the predicate is strictly-less-than.
		{"exactly-at-latency-threshold", thresholdLatency, 5, 0, false, "100ms is NOT < 100ms"},
		{"one-ns-under-latency-threshold", thresholdLatency - 1, 5, 0, true, "just inside the window"},
		{"one-ns-over-latency-threshold", thresholdLatency + 1, 5, 0, false, "outside the window"},

		// Length boundary — likewise strictly-less-than.
		{"exactly-at-length-threshold", 5 * time.Millisecond, thresholdLength, 0, false, "100 chars is NOT < 100"},
		{"one-under-length-threshold", 5 * time.Millisecond, thresholdLength - 1, 0, true, "just inside"},
		{"one-over-length-threshold", 5 * time.Millisecond, thresholdLength + 1, 0, false, "outside"},

		// Token boundary — any positive count is evidence of work.
		{"one-token-reported", 5 * time.Millisecond, 5, 1, false, "a single token is still accounted work"},
		{"zero-tokens-reported", 5 * time.Millisecond, 5, 0, true, "no work accounted for"},
		{"negative-tokens-treated-as-unreported", 5 * time.Millisecond, 5, -1, true,
			"a malformed count is not evidence of work; fall back to the latency+length test"},

		// Degenerate inputs.
		{"empty-content-no-tokens", 0, 0, 0, true, "the empty-response check runs first, but the guard must not crash"},
		{"empty-content-with-tokens", 0, 0, 7, false, "usage reported"},
		{"slow-and-long-with-tokens", 5 * time.Second, 4096, 9000, false, "an ordinary remote generation"},
	}

	for _, tc := range cases {
		got := IsNonGenuineFastResponse(tc.latency, tc.length, tc.tokens)
		if got != tc.reject {
			t.Errorf("case %s (latency=%v len=%d tokens=%d): got reject=%v, want %v — %s",
				tc.name, tc.latency, tc.length, tc.tokens, got, tc.reject, tc.rationale)
			continue
		}
		t.Logf("case %-38s reject=%-5v  %s", tc.name, got, tc.rationale)
	}
}

func TestDebateTokenTotals_CaseSpace(t *testing.T) {
	anthropicShaped := ParticipantResponse{
		ParticipantID: "anthropic-style",
		Metadata: map[string]any{
			"tokens_used":   41,
			"input_tokens":  7,
			"output_tokens": 34,
		},
	}
	jsonRoundTripped := ParticipantResponse{
		ParticipantID: "after-json",
		Metadata: map[string]any{
			// A metadata map that survived a JSON round-trip carries float64.
			"tokens_used":       float64(206),
			"prompt_tokens":     float64(200),
			"completion_tokens": float64(6),
		},
	}

	cases := []struct {
		name                                  string
		dr                                    *DebateResult
		wantPrompt, wantCompletion, wantTotal int
	}{
		{"nil-result", nil, 0, 0, 0},
		{"no-responses", &DebateResult{DebateID: "empty"}, 0, 0, 0},
		{"response-with-nil-metadata",
			&DebateResult{AllResponses: []ParticipantResponse{{ParticipantID: "p"}}}, 0, 0, 0},
		{"aggregate-only",
			&DebateResult{AllResponses: []ParticipantResponse{
				{ParticipantID: "p", Metadata: map[string]any{"tokens_used": 300}}}}, 0, 0, 300},
		{"anthropic-shaped-keys",
			&DebateResult{AllResponses: []ParticipantResponse{anthropicShaped}}, 7, 34, 41},
		{"float64-after-json-round-trip",
			&DebateResult{AllResponses: []ParticipantResponse{jsonRoundTripped}}, 200, 6, 206},
		{"mixed-shapes-summed",
			&DebateResult{AllResponses: []ParticipantResponse{anthropicShaped, jsonRoundTripped}},
			207, 40, 247},
	}

	for _, tc := range cases {
		p, c, tot := DebateTokenTotals(tc.dr)
		if p != tc.wantPrompt || c != tc.wantCompletion || tot != tc.wantTotal {
			t.Errorf("case %s: got (%d,%d,%d), want (%d,%d,%d)",
				tc.name, p, c, tot, tc.wantPrompt, tc.wantCompletion, tc.wantTotal)
			continue
		}
		t.Logf("case %-32s prompt=%-4d completion=%-4d total=%-4d", tc.name, p, c, tot)
	}
}

func TestDebateUsageMetadata_CaseSpace(t *testing.T) {
	cases := []struct {
		name                      string
		prompt, completion, total int
		wantPublished             bool
		rationale                 string
	}{
		{"complete-and-consistent", 600, 19, 619, true, "every participant reported a split"},
		{"zero-total", 0, 0, 0, false, "nothing to report"},
		{"aggregate-only", 0, 0, 619, false, "no split reported by anyone"},
		{"partial-split-understates-total", 200, 6, 506, false,
			"publishing would shrink the total and lose 300 tokens"},
		{"prompt-only", 600, 0, 619, false, "one direction alone cannot be published"},
		{"completion-only", 0, 19, 619, false, "one direction alone cannot be published"},
		{"split-exceeds-total", 600, 100, 619, false, "internally inconsistent, refuse"},
	}

	for _, tc := range cases {
		md := DebateUsageMetadata(tc.prompt, tc.completion, tc.total)
		published := md != nil
		if published != tc.wantPublished {
			t.Errorf("case %s (%d/%d/%d): published=%v, want %v — %s",
				tc.name, tc.prompt, tc.completion, tc.total, published, tc.wantPublished, tc.rationale)
			continue
		}
		if published {
			if md["prompt_tokens"] != tc.prompt || md["completion_tokens"] != tc.completion ||
				md["total_tokens"] != tc.total {
				t.Errorf("case %s: published values do not match inputs: %v", tc.name, md)
				continue
			}
		}
		t.Logf("case %-32s published=%-5v  %s", tc.name, published, tc.rationale)
	}
}

// The contract the ensemble envelope depends on end-to-end: whatever
// DebateUsageMetadata publishes MUST read back through TokenSplit unchanged.
// Every path that reports debate/ensemble usage (debateResultToEnsemble, the
// handler's debate_service_team result, and the agentic execution loop) relies
// on this round-trip, so breaking it silently returns the zero-usage envelope
// those three fixes exist to eliminate.
func TestDebateUsageMetadata_RoundTripsThroughTokenSplit(t *testing.T) {
	const wantPrompt, wantCompletion, wantTotal = 2493, 208, 2701

	resp := &models.LLMResponse{
		TokensUsed: wantTotal,
		Metadata:   DebateUsageMetadata(wantPrompt, wantCompletion, wantTotal),
	}
	p, c, tot := resp.TokenSplit()
	if p != wantPrompt || c != wantCompletion || tot != wantTotal {
		t.Fatalf("round-trip lost data: got (%d,%d,%d), want (%d,%d,%d)",
			p, c, tot, wantPrompt, wantCompletion, wantTotal)
	}

	// And the withheld case must still surface the real total.
	withheld := &models.LLMResponse{
		TokensUsed: 500,
		Metadata:   DebateUsageMetadata(200, 6, 500), // inconsistent => withheld
	}
	p, c, tot = withheld.TokenSplit()
	if p != 0 || c != 0 || tot != 500 {
		t.Fatalf("withheld split must still report the real total: got (%d,%d,%d), want (0,0,500)",
			p, c, tot)
	}
}
