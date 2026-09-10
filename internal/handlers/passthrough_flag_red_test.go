package handlers

// HXC-350 — explicit pass-through flag on the debate/ensemble routes.
//
// §11.4.115 RED-baseline-on-the-broken-artifact + polarity switch.
//
// OPERATOR DECISION (2026-09-08): pass-through ALREADY happens by accident on
// two undocumented paths, so callers will otherwise come to depend on a quirk.
// Make it DELIBERATE and documented — an explicit request flag — while keeping
// DEBATE as the default for the three debate/ensemble model aliases.
//
// MEASURED on the pre-fix artifact (in-process, this harness):
//
//	A) single user message, no flag   -> "model":"helixagent-ensemble" (debate/ensemble route)
//	B) single user message, WITH flag -> "model":"helixagent-ensemble" (FLAG IGNORED = the defect)
//	C) multi-turn (2 user msgs)       -> "model":"helixagent-debate"   (accidental pass-through)
//	D) tools present                  -> "model":"helixagent-ensemble" (direct provider, wrong label)
//	E) stream:true + WITH flag        -> "model":"helixagent-ensemble" (FLAG IGNORED on the
//	                                     streaming path — the mode real CLI agents use)
//
// THE FALSIFYING PROPERTY — THREE independent legs, in falsifying-strength
// order. Leg 1 is load-bearing; legs 2 and 3 corroborate it.
//
//  1. ROUTE — WHICH PROVIDERS WERE INVOKED. Two providers are registered: the
//     primary, and a second one that exists purely as a witness. The ensemble
//     service fans a request out to EVERY registered provider (RunEnsemble ->
//     filterProviders returns all providers when no EnsembleConfig is set),
//     whereas processWithProviderChain calls the primary and returns the moment
//     it succeeds. So "was the witness invoked?" reads the invocation GRAPH and
//     is the one leg a response-relabel cannot forge.
//
//     WHY THIS LEG EXISTS (honest note, §11.4.6): an earlier revision of this
//     guard discriminated the two routes ONLY by the response's model label. A
//     paired §1.1 mutation that routed pass-through through processWithEnsemble
//     and then set resp.Model = req.Model SURVIVED that guard — it protected a
//     label proxy, not the named defect. The content leg could not save it
//     either, because in THIS harness the reachable debate fallback is the
//     legacy services.EnsembleService.RunEnsemble, which forwards req.Messages
//     VERBATIM; the topic-reducing rewrite the content leg keys off lives only
//     in processWithOrchestrator and AgenticEnsemble.toolAugmentedDebate, and
//     neither is wired here. The route leg closes that gap: the same mutation
//     now fails deterministically on "the witness provider was invoked".
//
//  2. CONTENT — the provider must receive the user's instruction VERBATIM. The
//     stub below behaves like an obedient LLM: it answers with the marker ONLY
//     when the literal instruction reaches it unmodified; any injected preamble
//     (the real debate route wraps it: DebateService.buildDebatePrompt emits
//     "USER REQUEST: <topic>")
//     makes it answer differently. The oracle is therefore NOT the code under
//     test agreeing with itself (§11.4.245): the stub's answer is a function of
//     what the handler actually forwarded. Scope, stated honestly: this leg
//     discriminates along the CONTENT-REWRITE axis (it catches a mutation that
//     rewrites or wraps the user's message) — it does NOT by itself
//     discriminate ensemble-vs-chain, which is leg 1's job.
//
//  3. ENVELOPE — the pass-through route reports the REQUESTED model and the
//     provider's own fingerprint; the debate/ensemble route hardcodes
//     "helixagent-ensemble" (UnifiedHandler.convertToOpenAIChatResponse, and
//     the streaming equivalent in the comprehensive stream envelope).
//
// Citations here name SYMBOLS, never line numbers: an earlier revision cited
// openai_compatible.go:3959 / :2679 and both had already drifted by the time
// the guard was reviewed (§11.4.111 — resolve by stable name, not by index).
//
// POLARITY SWITCH (one source, two roles) — uses the package-shared redMode()
// from id_uniqueness_test.go:
//
//	RED_MODE=1     — reproduce-and-assert-the-defect-is-PRESENT (passes pre-fix).
//	RED_MODE unset — the standing GREEN regression guard (passes post-fix).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"dev.helix.agent/internal/models"
	"dev.helix.agent/internal/services"
)

const (
	hxc350Marker      = "ZEBRA_42"
	hxc350Instruction = "Reply with exactly: " + hxc350Marker
	// hxc350NotVerbatim is what the stub answers when the instruction did NOT
	// arrive verbatim — i.e. something rewrote or wrapped the user's message.
	hxc350NotVerbatim = "NOT_VERBATIM"
	// ensembleModelLabel is the hardcoded label the debate/ensemble route emits.
	hxc350EnsembleLabel = "helixagent-ensemble"
	hxc350RequestModel  = "helixagent-debate"
	// hxc350WitnessProviderName is a SECOND registered provider that exists
	// purely as a route witness. The ensemble service fans a request out to
	// EVERY registered provider (services/ensemble.go RunEnsemble ->
	// filterProviders returns all providers when no EnsembleConfig is set),
	// whereas processWithProviderChain calls the PRIMARY provider and returns
	// the moment it succeeds. So "was the witness invoked?" is a direct,
	// structural read of WHICH ROUTE ran — independent of any label the
	// handler stamps on its own response (§11.4.245 oracle independence).
	hxc350WitnessProviderName = "hxc350-witness"
	hxc350WitnessAnswer       = "WITNESS_FANOUT"
	// hxc350ForcedProviderName is the target of a `force_provider` request.
	// It is NOT the primary, so "which provider answered" is a direct read of
	// whether force_provider was honoured on the pass-through route.
	hxc350ForcedProviderName = "hxc350-forced"
	hxc350ForcedAnswer       = "FORCED_PROVIDER_ANSWERED"

	// ---- I1 (operator decision, 2026-09-09): the response `model` field ----
	//
	// hxc350PrimaryReportedModel is what the PRIMARY stub reports as its OWN
	// model id, mirroring how real providers do it: they put the upstream's
	// resolved model name in LLMResponse.Metadata["model"] (helixllm
	// provider.go does it on BOTH the non-streaming and the streaming path,
	// and so do claude/gemini/qwen/deepseek/zai/openrouter/... ).
	hxc350PrimaryReportedModel = "helixllm-stub-v9"
	// hxc350PrimaryLabel is the identity a pass-through answer from the
	// primary provider MUST report. Written out as a LITERAL rather than
	// composed with the production join helper, so the oracle states the
	// contract independently instead of agreeing with the code under test
	// (§11.4.245).
	hxc350PrimaryLabel = "helixllm/helixllm-stub-v9"
	// hxc350ForcedLabel is the identity reported when the answering provider
	// reports NO model id of its own (the forced stub deliberately sets no
	// Metadata). The provider half is still real; the model half is an
	// explicit, visibly-not-a-model placeholder. Silently substituting the
	// requested alias here would reinstate the very defect this decision
	// fixes, so that substitution is what the test forbids.
	hxc350ForcedLabel = "hxc350-forced/<unreported>"
)

// hxc350Capture records what the provider was actually handed.
type hxc350Capture struct {
	mu       sync.Mutex
	calls    int
	lastMsgs []models.Message
	lastTool int
	// byProvider counts, per registered provider name, how many times that
	// provider was actually invoked. This is the ROUTE oracle (leg 1): the
	// ensemble fans out to EVERY registered provider, the pass-through
	// chain calls the primary ONLY. It is a property of the invocation
	// graph, never of a label the handler stamps on its own output.
	byProvider map[string]int
}

func (c *hxc350Capture) record(req *models.LLMRequest, provider string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.byProvider == nil {
		c.byProvider = map[string]int{}
	}
	c.byProvider[provider]++
	// Only the primary provider's view of the messages is the verbatim
	// oracle; a witness provider must not overwrite it.
	if provider == PrimaryProviderName {
		c.lastMsgs = append([]models.Message(nil), req.Messages...)
		c.lastTool = len(req.Tools)
	}
}

func (c *hxc350Capture) snapshot() (calls int, msgs []models.Message, tools int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, append([]models.Message(nil), c.lastMsgs...), c.lastTool
}

// providerCalls returns how many times the named provider was invoked.
func (c *hxc350Capture) providerCalls(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byProvider[name]
}

func hxc350LastUserContent(msgs []models.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

// newHXC350Handler builds a UnifiedHandler backed by a single registered
// provider under the primary chain name, so BOTH candidate routes have a real
// provider to reach. The stub obeys only a verbatim instruction.
func newHXC350Handler(t *testing.T) (*UnifiedHandler, *hxc350Capture) {
	t.Helper()
	cap := &hxc350Capture{}
	reg := services.NewProviderRegistryWithoutAutoDiscovery(nil, nil)
	answer := func(req *models.LLMRequest) string {
		if hxc350LastUserContent(req.Messages) == hxc350Instruction {
			return hxc350Marker
		}
		return hxc350NotVerbatim
	}
	stub := &MockLLMProvider{
		name: PrimaryProviderName,
		completeFunc: func(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error) {
			cap.record(req, PrimaryProviderName)
			return &models.LLMResponse{
				ID:           "hxc350-resp",
				Content:      answer(req),
				ProviderName: PrimaryProviderName,
				TokensUsed:   5,
				FinishReason: "stop",
				CreatedAt:    time.Now(),
				// The provider's OWN model id, exactly where real providers
				// put it. This is the value the response must report.
				Metadata: map[string]interface{}{"model": hxc350PrimaryReportedModel},
			}, nil
		},
		streamFunc: func(ctx context.Context, req *models.LLMRequest) (<-chan *models.LLMResponse, error) {
			cap.record(req, PrimaryProviderName)
			content := answer(req)
			ch := make(chan *models.LLMResponse, 1)
			ch <- &models.LLMResponse{
				ID:           "hxc350-stream",
				Content:      content,
				ProviderName: PrimaryProviderName,
				FinishReason: "stop",
				CreatedAt:    time.Now(),
				// Streaming chunks carry the identity too — helixllm's
				// streaming path sets Metadata["model"] per chunk.
				Metadata: map[string]interface{}{"model": hxc350PrimaryReportedModel},
			}
			close(ch)
			return ch, nil
		},
	}
	require.NoError(t, reg.RegisterProvider(PrimaryProviderName, stub))

	// Route witness (see hxc350WitnessProviderName). It never answers the
	// marker, so if it ever wins the ensemble vote the content leg fails too.
	witness := &MockLLMProvider{
		name: hxc350WitnessProviderName,
		completeFunc: func(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error) {
			cap.record(req, hxc350WitnessProviderName)
			return &models.LLMResponse{
				ID:           "hxc350-witness-resp",
				Content:      hxc350WitnessAnswer,
				ProviderName: hxc350WitnessProviderName,
				TokensUsed:   5,
				FinishReason: "stop",
				CreatedAt:    time.Now(),
			}, nil
		},
		streamFunc: func(ctx context.Context, req *models.LLMRequest) (<-chan *models.LLMResponse, error) {
			cap.record(req, hxc350WitnessProviderName)
			ch := make(chan *models.LLMResponse, 1)
			ch <- &models.LLMResponse{
				ID:           "hxc350-witness-stream",
				Content:      hxc350WitnessAnswer,
				ProviderName: hxc350WitnessProviderName,
				FinishReason: "stop",
				CreatedAt:    time.Now(),
			}
			close(ch)
			return ch, nil
		},
	}
	require.NoError(t, reg.RegisterProvider(hxc350WitnessProviderName, witness))

	// force_provider target (see hxc350ForcedProviderName).
	forced := &MockLLMProvider{
		name: hxc350ForcedProviderName,
		completeFunc: func(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error) {
			cap.record(req, hxc350ForcedProviderName)
			return &models.LLMResponse{
				ID:           "hxc350-forced-resp",
				Content:      hxc350ForcedAnswer,
				ProviderName: hxc350ForcedProviderName,
				TokensUsed:   5,
				FinishReason: "stop",
				CreatedAt:    time.Now(),
			}, nil
		},
		streamFunc: func(ctx context.Context, req *models.LLMRequest) (<-chan *models.LLMResponse, error) {
			cap.record(req, hxc350ForcedProviderName)
			ch := make(chan *models.LLMResponse, 1)
			ch <- &models.LLMResponse{
				ID:           "hxc350-forced-stream",
				Content:      hxc350ForcedAnswer,
				ProviderName: hxc350ForcedProviderName,
				FinishReason: "stop",
				CreatedAt:    time.Now(),
			}
			close(ch)
			return ch, nil
		},
	}
	require.NoError(t, reg.RegisterProvider(hxc350ForcedProviderName, forced))

	return NewUnifiedHandler(reg, nil), cap
}

// newHXC350HandlerWithoutPrimary builds the same harness with NO helixllm
// provider registered — the exact registry state an operator produces with
// USE_HELIX_LLM=false (see TestPassthrough_HelixLLMDisabled_ChainFallsThrough).
func newHXC350HandlerWithoutPrimary(t *testing.T) (*UnifiedHandler, *hxc350Capture) {
	t.Helper()
	cap := &hxc350Capture{}
	reg := services.NewProviderRegistryWithoutAutoDiscovery(nil, nil)
	fallback := &MockLLMProvider{
		name: hxc350ForcedProviderName,
		completeFunc: func(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error) {
			cap.record(req, hxc350ForcedProviderName)
			content := hxc350NotVerbatim
			if hxc350LastUserContent(req.Messages) == hxc350Instruction {
				content = hxc350Marker
			}
			return &models.LLMResponse{
				ID:           "hxc350-fallback-resp",
				Content:      content,
				ProviderName: hxc350ForcedProviderName,
				TokensUsed:   5,
				FinishReason: "stop",
				CreatedAt:    time.Now(),
			}, nil
		},
	}
	require.NoError(t, reg.RegisterProvider(hxc350ForcedProviderName, fallback))
	return NewUnifiedHandler(reg, nil), cap
}

func hxc350Post(t *testing.T, h *UnifiedHandler, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ChatCompletions(c)
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), "response body: %s", w.Body.String())
	return w.Code, out
}

func hxc350Content(t *testing.T, resp map[string]any) string {
	t.Helper()
	choices, ok := resp["choices"].([]any)
	require.True(t, ok && len(choices) > 0, "no choices in response: %v", resp)
	msg, ok := choices[0].(map[string]any)["message"].(map[string]any)
	require.True(t, ok, "no message in first choice: %v", choices[0])
	content, _ := msg["content"].(string)
	return content
}

func hxc350SingleUserBody(extra map[string]any) map[string]any {
	body := map[string]any{
		"model":    hxc350RequestModel,
		"messages": []map[string]string{{"role": "user", "content": hxc350Instruction}},
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// TestPassthroughFlag_SingleUserMessage is the §11.4.115 polarity test.
func TestPassthroughFlag_SingleUserMessage(t *testing.T) {
	h, cap := newHXC350Handler(t)

	code, resp := hxc350Post(t, h, hxc350SingleUserBody(map[string]any{"passthrough": true}))
	require.Equal(t, http.StatusOK, code)

	calls, msgs, _ := cap.snapshot()
	content := hxc350Content(t, resp)
	model, _ := resp["model"].(string)

	if redMode() {
		// RED: the flag is IGNORED — the request still routes to the
		// debate/ensemble path, which stamps the hardcoded ensemble label.
		assert.Equal(t, hxc350EnsembleLabel, model,
			"RED expects the pre-fix defect: passthrough flag ignored, ensemble label emitted")
		return
	}

	// GREEN regression guard — the defect is ABSENT. Three INDEPENDENT legs,
	// in falsifying-strength order.
	//
	// Leg 1 (ROUTE — the strongest, and the only one a relabel cannot forge):
	// the pass-through chain calls the PRIMARY provider and returns the moment
	// it succeeds, so the witness provider is NEVER invoked. The ensemble
	// route fans out to EVERY registered provider, so it always is. This reads
	// the invocation graph, not anything the handler says about itself.
	assert.Equal(t, 0, cap.providerCalls(hxc350WitnessProviderName),
		"passthrough must NOT fan out to the ensemble: the witness provider was invoked, "+
			"so this request went through the debate/ensemble route regardless of its label")
	assert.GreaterOrEqual(t, cap.providerCalls(PrimaryProviderName), 1,
		"passthrough must reach the primary provider in the chain")

	// Leg 2 (CONTENT): the provider received the user's instruction VERBATIM
	// and the handler returned its answer unmodified.
	assert.Equal(t, hxc350Marker, content,
		"passthrough must return the literal instruction's answer, not a debate essay")
	assert.Equal(t, hxc350Instruction, hxc350LastUserContent(msgs),
		"the provider must be handed the user's message verbatim (no injected preamble)")
	assert.GreaterOrEqual(t, calls, 1, "a provider must actually have been called")

	// Leg 3 (ENVELOPE): reports WHO ACTUALLY ANSWERED — not the hardcoded
	// ensemble label, and (since the 2026-09-09 operator decision on I1) not
	// the requested alias either.
	//
	// §11.4.120 RECONCILIATION, not a weakening: this assertion previously
	// read `assert.Equal(hxc350RequestModel, model)`. The operator decision
	// deliberately CHANGES that client-visible field, so the old assertion was
	// pinning behaviour the decision removed; it is rewritten to assert the
	// NEW mechanism rather than deleted or relaxed. The replacement is
	// STRICTLY STRONGER: it pins the exact answering identity AND adds an
	// explicit negative that the requested alias must NOT come back.
	assert.Equal(t, hxc350PrimaryLabel, model,
		"passthrough must report the provider/model that actually answered")
	assert.NotEqual(t, hxc350RequestModel, model,
		"reporting the requested alias back is exactly the defect the decision fixes")
	// Real timestamp, not a zero clock.
	created, _ := resp["created"].(float64)
	assert.Greater(t, created, float64(1_600_000_000), "created must be a real unix timestamp")
}

// TestPassthroughFlag_DefaultStillDebates is the load-bearing REGRESSION:
// a change that silently turns debate into pass-through for everyone is a
// worse defect than the one being fixed. Polarity-independent invariant.
func TestPassthroughFlag_DefaultStillDebates(t *testing.T) {
	h, _ := newHXC350Handler(t)

	code, resp := hxc350Post(t, h, hxc350SingleUserBody(nil)) // NO flag
	require.Equal(t, http.StatusOK, code)

	model, _ := resp["model"].(string)
	assert.Equal(t, hxc350EnsembleLabel, model,
		"DEFAULT (no flag) must STILL route to the debate/ensemble path")
}

// TestPassthroughFlag_ExplicitFalseStillDebates — an explicit false is the default.
func TestPassthroughFlag_ExplicitFalseStillDebates(t *testing.T) {
	h, _ := newHXC350Handler(t)

	code, resp := hxc350Post(t, h, hxc350SingleUserBody(map[string]any{"passthrough": false}))
	require.Equal(t, http.StatusOK, code)

	model, _ := resp["model"].(string)
	assert.Equal(t, hxc350EnsembleLabel, model,
		"passthrough:false must behave exactly like the default (debate)")
}

// TestPassthroughFlag_MultiTurnBypassPreserved — the pre-existing accidental
// bypass (drainage Finding #19) must keep working, flag or no flag.
func TestPassthroughFlag_MultiTurnBypassPreserved(t *testing.T) {
	h, cap := newHXC350Handler(t)

	code, resp := hxc350Post(t, h, map[string]any{
		"model": hxc350RequestModel,
		"messages": []map[string]string{
			{"role": "user", "content": "first turn"},
			{"role": "assistant", "content": "ok"},
			{"role": "user", "content": hxc350Instruction},
		},
	})
	require.Equal(t, http.StatusOK, code)

	model, _ := resp["model"].(string)
	// §11.4.120 reconciliation (see TestPassthroughFlag_SingleUserMessage leg
	// 3). The multi-turn bypass reaches the SAME provider chain, so it reports
	// the same answering identity — which is a sharper route oracle than the
	// old alias check: the alias was emitted by more than one route.
	assert.Equal(t, hxc350PrimaryLabel, model,
		"multi-turn bypass must still route to the provider chain and report who answered")
	assert.Equal(t, hxc350Marker, hxc350Content(t, resp),
		"multi-turn bypass must still be verbatim")
	_, msgs, _ := cap.snapshot()
	assert.Len(t, msgs, 3, "multi-turn bypass must forward the FULL conversation")
	assert.Equal(t, 0, cap.providerCalls(hxc350WitnessProviderName),
		"multi-turn bypass must go to the provider chain, not fan out to the ensemble")
}

// TestPassthroughFlag_ToolsBypassPreserved — the pre-existing tools bypass
// (processWithEnsemble "Smart Routing: Layer 1") must keep reaching a provider
// with tools attached.
func TestPassthroughFlag_ToolsBypassPreserved(t *testing.T) {
	h, cap := newHXC350Handler(t)

	code, _ := hxc350Post(t, h, hxc350SingleUserBody(map[string]any{
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "get_time",
				"description": "returns the time",
				"parameters":  map[string]any{"type": "object"},
			},
		}},
	}))
	require.Equal(t, http.StatusOK, code)

	calls, _, tools := cap.snapshot()
	assert.GreaterOrEqual(t, calls, 1, "tools bypass must reach a provider")
	assert.Equal(t, 1, tools, "tools bypass must forward the tools array to the provider")
}

// ---------------------------------------------------------------------------
// B1 — the streaming path (stream:true). This is the mode REAL CLI-agent
// clients use: 32 of the 49 challenge scripts that set `stream` set it to
// true (e.g. challenges/scripts/opencode_helixllm_hello_challenge.sh:54).
// ChatCompletions dispatches to handleStreamingChatCompletions BEFORE the
// pass-through check is reached, so a documented flag silently no-ops for
// its main consumer — the §11.4.201(6) false-null class.
// ---------------------------------------------------------------------------

// hxc350PostStream posts a streaming request and returns the raw SSE body.
func hxc350PostStream(t *testing.T, h *UnifiedHandler, body map[string]any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ChatCompletions(c)
	return w.Code, w.Body.String()
}

// TestPassthroughFlag_StreamingHonoursFlag — §11.4.115 polarity test for B1.
//
//	RED_MODE=1     — reproduces the defect on the pre-fix artifact: the
//	                 streaming dispatch never consults req.Passthrough, so the
//	                 SSE envelope carries the hardcoded ensemble label and the
//	                 provider is handed a debate-wrapped prompt.
//	RED_MODE unset — the standing GREEN guard: the flag is honoured.
func TestPassthroughFlag_StreamingHonoursFlag(t *testing.T) {
	h, cap := newHXC350Handler(t)

	code, body := hxc350PostStream(t, h, hxc350SingleUserBody(map[string]any{
		"passthrough": true,
		"stream":      true,
	}))
	require.Equal(t, http.StatusOK, code)

	if redMode() {
		// RED: the flag is IGNORED on the streaming path — the stream is
		// stamped with the hardcoded ensemble label, not the requested model.
		assert.Contains(t, body, `"model":"`+hxc350EnsembleLabel+`"`,
			"RED expects the pre-fix defect: stream:true ignores passthrough")
		return
	}

	// GREEN — the defect is ABSENT on the streaming path.
	// Leg 1 (ROUTE): the pass-through chain calls the PRIMARY provider only;
	// the debate/ensemble stream does not reach it verbatim at all.
	assert.GreaterOrEqual(t, cap.providerCalls(PrimaryProviderName), 1,
		"streaming passthrough must reach the primary provider")
	assert.Equal(t, 0, cap.providerCalls(hxc350WitnessProviderName),
		"streaming passthrough must NOT fan out to the ensemble witness provider")
	// Leg 2 (CONTENT): the provider was handed the instruction VERBATIM and
	// its literal answer reached the wire.
	_, msgs, _ := cap.snapshot()
	assert.Equal(t, hxc350Instruction, hxc350LastUserContent(msgs),
		"the provider must be handed the user's message verbatim (no debate preamble)")
	assert.Contains(t, body, hxc350Marker,
		"streaming passthrough must stream the literal answer, not a debate essay")
	// Leg 3 (ENVELOPE): the stream reports WHO ACTUALLY ANSWERED.
	//
	// §11.4.120 reconciliation: previously asserted the REQUESTED model. The
	// operator decision changed that field, so the assertion is rewritten to
	// the new mechanism — and strengthened with the two negatives that make it
	// falsifiable in both directions.
	assert.Contains(t, body, `"model":"`+hxc350PrimaryLabel+`"`,
		"streaming passthrough must report the provider/model that actually answered")
	assert.NotContains(t, body, `"model":"`+hxc350EnsembleLabel+`"`,
		"streaming passthrough must NOT emit the hardcoded ensemble label")
	assert.NotContains(t, body, `"model":"`+hxc350RequestModel+`"`,
		"streaming passthrough must NOT echo the requested alias back")
}

// TestPassthroughFlag_StreamingDefaultStillDebates is the load-bearing
// REGRESSION for B1: a streaming fix that turns every stream into a
// pass-through is a worse defect than the one being fixed.
func TestPassthroughFlag_StreamingDefaultStillDebates(t *testing.T) {
	h, _ := newHXC350Handler(t)

	code, body := hxc350PostStream(t, h, hxc350SingleUserBody(map[string]any{
		"stream": true, // NO passthrough flag
	}))
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `"model":"`+hxc350EnsembleLabel+`"`,
		"DEFAULT streaming (no flag) must STILL route to the debate/ensemble stream")
}

// TestPassthroughFlag_StreamingExplicitFalseStillDebates — explicit false is
// the default on the streaming path too.
func TestPassthroughFlag_StreamingExplicitFalseStillDebates(t *testing.T) {
	h, _ := newHXC350Handler(t)

	code, body := hxc350PostStream(t, h, hxc350SingleUserBody(map[string]any{
		"stream":      true,
		"passthrough": false,
	}))
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `"model":"`+hxc350EnsembleLabel+`"`,
		"stream + passthrough:false must behave exactly like the default (debate)")
}

// ---------------------------------------------------------------------------
// I3 — behaviour deltas between the pass-through route
// (processWithProviderChain) and the debate/ensemble route. Each delta is
// either FIXED and pinned here, or INTENDED and pinned here so it cannot
// change silently.
// ---------------------------------------------------------------------------

// TestPassthrough_ForceProviderHonoured — DELTA (a), FIXED.
//
// The ensemble route honours `force_provider`; processWithProviderChain had
// ZERO references to it, so under pass-through the field was silently dropped
// and the request went to whichever provider the chain preferred. A request
// field that is accepted, documented and then ignored is the same false-null
// class as the streaming flag in B1 — the caller cannot tell it did nothing.
func TestPassthrough_ForceProviderHonoured(t *testing.T) {
	h, cap := newHXC350Handler(t)

	code, resp := hxc350Post(t, h, hxc350SingleUserBody(map[string]any{
		"passthrough":    true,
		"force_provider": hxc350ForcedProviderName,
	}))
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 1, cap.providerCalls(hxc350ForcedProviderName),
		"force_provider must select the named provider on the pass-through route")
	assert.Equal(t, 0, cap.providerCalls(PrimaryProviderName),
		"force_provider must NOT fall through to the primary provider")
	assert.Equal(t, hxc350ForcedAnswer, hxc350Content(t, resp),
		"the forced provider's own answer must reach the caller")
}

// TestPassthrough_ForceProviderUnknownFallsBackToChain — the safe half of
// delta (a): an unknown force_provider must NOT hard-fail the request; the
// chain still answers. Without this, honouring the field turns a typo into an
// outage.
func TestPassthrough_ForceProviderUnknownFallsBackToChain(t *testing.T) {
	h, cap := newHXC350Handler(t)

	code, resp := hxc350Post(t, h, hxc350SingleUserBody(map[string]any{
		"passthrough":    true,
		"force_provider": "no-such-provider",
	}))
	require.Equal(t, http.StatusOK, code)
	assert.GreaterOrEqual(t, cap.providerCalls(PrimaryProviderName), 1,
		"an unknown force_provider must fall back to the normal chain, not 5xx")
	assert.Equal(t, hxc350Marker, hxc350Content(t, resp))
}

// TestPassthrough_HelixLLMDisabled_ChainFallsThrough — DELTA (e), RESOLVED as
// NOT-A-DEFECT, pinned here so the resolution cannot rot.
//
// The review left it UNCONFIRMED whether a disabled helixllm can still be
// registered, i.e. whether processWithProviderChain silently ignores
// USE_HELIX_LLM=false where processWithDirectProvider checks it explicitly.
// It cannot: ProviderRegistry stores the helixllm config with
// `Enabled: HelixLLMEnabledDefault()`, and BOTH construction paths that read
// that config (the lazy `createProviderFromConfig` inside GetProvider, and
// RegisterProviderFromConfig) go through `case "helixllm": if cfg.Enabled`,
// which returns an error instead of a provider — so RegisterProvider is never
// reached. ConfigureProvider additionally UNREGISTERS on !Enabled, and no
// discovery path ever names "helixllm". A disabled helixllm is therefore
// ABSENT FROM THE REGISTRY, which is precisely the state this test builds.
//
// The chain then respects the operator's opt-out structurally: GetProvider
// errors and the score-ordered fallback answers. The explicit
// HelixLLMEnabledDefault() check in processWithDirectProvider is belt-and-
// braces, not a delta the pass-through route is missing.
func TestPassthrough_HelixLLMDisabled_ChainFallsThrough(t *testing.T) {
	h, cap := newHXC350HandlerWithoutPrimary(t)

	code, resp := hxc350Post(t, h, hxc350SingleUserBody(map[string]any{"passthrough": true}))
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 0, cap.providerCalls(PrimaryProviderName),
		"a disabled helixllm is absent from the registry and must not be invoked")
	assert.GreaterOrEqual(t, cap.providerCalls(hxc350ForcedProviderName), 1,
		"the chain must fall through to the score-ordered fallback provider")
	assert.Equal(t, hxc350Marker, hxc350Content(t, resp),
		"the fallback provider must still receive the instruction verbatim")
}

// TestPassthrough_ToolResultTurnKeepsLoopBreaker — ORDERING, previously
// UNTESTED. The pass-through branch sits AFTER the tool-result guard in
// ChatCompletions, so a tool-result turn is synthesised without a new debate
// even when the caller also set passthrough. An ordering mutation that hoists
// the pass-through branch above the guard would re-open the
// debate -> tool_calls -> results -> debate loop the guard exists to break.
func TestPassthrough_ToolResultTurnKeepsLoopBreaker(t *testing.T) {
	h, _ := newHXC350Handler(t)

	code, resp := hxc350Post(t, h, map[string]any{
		"model":       hxc350RequestModel,
		"passthrough": true,
		"messages": []map[string]any{
			{"role": "user", "content": hxc350Instruction},
			{"role": "assistant", "content": "", "tool_calls": []map[string]any{{
				"id":       "call_1",
				"type":     "function",
				"function": map[string]any{"name": "get_time", "arguments": "{}"},
			}}},
			{"role": "tool", "tool_call_id": "call_1", "content": "12:00"},
		},
	})
	require.Equal(t, http.StatusOK, code)

	model, _ := resp["model"].(string)
	assert.Equal(t, hxc350EnsembleLabel, model,
		"a tool-result turn must be handled by the loop-breaking tool-result guard, "+
			"which runs BEFORE the pass-through branch — flag or no flag")
	id, _ := resp["id"].(string)
	assert.Contains(t, id, "chatcmpl-tool",
		"the response must come from the tool-result synthesiser, not the provider chain")
}

// TestPassthrough_StreamingToolResultTurnKeepsLoopBreaker — the streaming half
// of the same ordering invariant. The streaming tool-result guard runs far
// below (after SSE headers are committed), so the streaming pass-through
// branch expresses the ordering as an explicit precondition instead.
func TestPassthrough_StreamingToolResultTurnKeepsLoopBreaker(t *testing.T) {
	h, _ := newHXC350Handler(t)

	code, body := hxc350PostStream(t, h, map[string]any{
		"model":       hxc350RequestModel,
		"passthrough": true,
		"stream":      true,
		"messages": []map[string]any{
			{"role": "user", "content": hxc350Instruction},
			{"role": "assistant", "content": "", "tool_calls": []map[string]any{{
				"id":       "call_1",
				"type":     "function",
				"function": map[string]any{"name": "get_time", "arguments": "{}"},
			}}},
			{"role": "tool", "tool_call_id": "call_1", "content": "12:00"},
		},
	})
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `"model":"`+hxc350EnsembleLabel+`"`,
		"a streaming tool-result turn must reach the streaming tool-result guard, "+
			"not the pass-through chain — flag or no flag")
}

// TestPassthrough_StreamingForceProviderHonoured — the streaming half of
// delta (a). Honouring force_provider only on the non-streaming chain would
// re-create, at a second seam, exactly the asymmetry B1 exists to close: a
// field that works for stream:false and silently no-ops for stream:true.
func TestPassthrough_StreamingForceProviderHonoured(t *testing.T) {
	h, cap := newHXC350Handler(t)

	code, body := hxc350PostStream(t, h, hxc350SingleUserBody(map[string]any{
		"passthrough":    true,
		"stream":         true,
		"force_provider": hxc350ForcedProviderName,
	}))
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, 1, cap.providerCalls(hxc350ForcedProviderName),
		"force_provider must select the named provider on the streaming chain too")
	assert.Equal(t, 0, cap.providerCalls(PrimaryProviderName),
		"force_provider must NOT fall through to the primary provider")
	assert.Contains(t, body, hxc350ForcedAnswer,
		"the forced provider's own answer must reach the SSE stream")
}

// ---------------------------------------------------------------------------
// I1 — the response `model` field. OPERATOR DECISION (2026-09-09).
//
// The deferred question from the HXC-350 review was: what should a
// pass-through response put in `model`? The decision: THE PROVIDER/MODEL THAT
// ACTUALLY ANSWERED — never the alias that was requested.
//
// WHY, recorded here so the rationale cannot drift away from the guard:
//
//   - It matches what OpenAI itself does: ask for `gpt-4o`, get back
//     `gpt-4o-2024-08-06`. Reporting the resolved identity rather than the
//     request is the established behaviour of the API this endpoint imitates.
//   - It directly answers the standing complaint in the meta-repo tracker
//     (HXC-350, third paragraph): "All three routes report a single fixed
//     model name regardless of which one was requested, so a caller cannot
//     tell which route answered." Cited by ticket id, not line number
//     (§11.4.111) — the line had already drifted twice.
//   - It satisfies this file's own stated contract that "the model name IS the
//     contract" (openai_compatible.go, the model-routing comment block).
//
// ACCEPTED COST, documented rather than worked around: a client that
// string-compares the response `model` against the one it requested will now
// see a MISMATCH. That is expected. It is written into
// docs/api/API_REFERENCE.md under `passthrough`, and the negative assertions
// below pin it so it cannot be quietly reverted by re-echoing the alias.
//
// POLARITY (§11.4.115): RED_MODE=1 reproduces the pre-fix defect (the alias
// comes back); RED_MODE unset is the standing GREEN guard.
// ---------------------------------------------------------------------------

// TestPassthrough_ModelReportsAnsweringProviderIdentity — non-streaming half.
//
// The oracle is INDEPENDENT of the code under test (§11.4.245): the stub
// decides what identity it reports, and the assertion states the expected
// string literally. A handler that composed the label from anything other
// than the answering provider + that provider's own reported id fails.
func TestPassthrough_ModelReportsAnsweringProviderIdentity(t *testing.T) {
	h, cap := newHXC350Handler(t)

	code, resp := hxc350Post(t, h, hxc350SingleUserBody(map[string]any{"passthrough": true}))
	require.Equal(t, http.StatusOK, code)

	model, _ := resp["model"].(string)

	if redMode() {
		// RED on the pre-fix artifact: the label is the REQUESTED alias, so
		// the caller cannot tell which provider answered.
		assert.Equal(t, hxc350RequestModel, model,
			"RED expects the pre-fix defect: the response echoes the requested alias")
		return
	}

	// GREEN. The primary provider answered and reported its own model id.
	require.GreaterOrEqual(t, cap.providerCalls(PrimaryProviderName), 1,
		"precondition: the primary provider must be the one that answered")
	assert.Equal(t, hxc350PrimaryLabel, model,
		"the response must report the provider/model that actually answered")
	assert.NotEqual(t, hxc350RequestModel, model,
		"echoing the requested alias back is the defect this decision fixes")
	assert.NotEqual(t, hxc350EnsembleLabel, model,
		"and it must not be the hardcoded ensemble label either")
}

// TestPassthrough_StreamingModelReportsAnsweringProviderIdentity — streaming
// half. Present because the last review found `stream:true` silently ignoring
// the flag entirely: an identity reported on only one of the two halves is the
// same asymmetry at a new seam.
func TestPassthrough_StreamingModelReportsAnsweringProviderIdentity(t *testing.T) {
	h, cap := newHXC350Handler(t)

	code, body := hxc350PostStream(t, h, hxc350SingleUserBody(map[string]any{
		"passthrough": true,
		"stream":      true,
	}))
	require.Equal(t, http.StatusOK, code)

	if redMode() {
		assert.Contains(t, body, `"model":"`+hxc350RequestModel+`"`,
			"RED expects the pre-fix defect: the SSE envelope echoes the requested alias")
		return
	}

	require.GreaterOrEqual(t, cap.providerCalls(PrimaryProviderName), 1,
		"precondition: the primary provider must be the one that answered")
	assert.Contains(t, body, `"model":"`+hxc350PrimaryLabel+`"`,
		"every SSE chunk must report the provider/model that actually answered")
	assert.NotContains(t, body, `"model":"`+hxc350RequestModel+`"`,
		"the stream must not echo the requested alias back")
}

// TestPassthrough_StreamingWithToolsReportsAnsweringProviderIdentity — the
// THIRD emission path, and the one the "single funnel" claim did not cover.
//
// streamWithProviderChain delegates to streamToolCallViaNonStreaming whenever
// the request carries `tools`, and that function built its SSE envelopes from
// req.Model directly rather than through the shared labeller. The path is
// genuinely reachable under pass-through — handleStreamingChatCompletions
// routes `passthrough + stream:true` into streamWithProviderChain, which then
// hands any tools-bearing request straight to it — and tools-bearing streaming
// requests are precisely what real CLI-agent clients send. Leaving it on the
// alias would have made the decision hold for two of the three streaming
// shapes and silently no-op for the third.
func TestPassthrough_StreamingWithToolsReportsAnsweringProviderIdentity(t *testing.T) {
	h, cap := newHXC350Handler(t)

	code, body := hxc350PostStream(t, h, hxc350SingleUserBody(map[string]any{
		"passthrough": true,
		"stream":      true,
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "get_time",
				"description": "returns the time",
				"parameters":  map[string]any{"type": "object"},
			},
		}},
	}))
	require.Equal(t, http.StatusOK, code)

	if redMode() {
		assert.Contains(t, body, `"model":"`+hxc350RequestModel+`"`,
			"RED expects the pre-fix defect: the tools/SSE path echoes the requested alias")
		return
	}

	require.GreaterOrEqual(t, cap.providerCalls(PrimaryProviderName), 1,
		"precondition: the primary provider must be the one that answered")
	assert.Contains(t, body, `"model":"`+hxc350PrimaryLabel+`"`,
		"the tools/SSE path must report the provider/model that actually answered")
	assert.NotContains(t, body, `"model":"`+hxc350RequestModel+`"`,
		"the tools/SSE path must not echo the requested alias back")
}

// TestPassthrough_ProviderWithoutModelIdentityIsHonest — the case that decides
// whether this fix is real.
//
// Not every provider reports a model id. The forced stub deliberately sets no
// Metadata, so there is genuinely nothing to report for the model half. The
// two forbidden answers are (a) FABRICATING an id and (b) SILENTLY falling
// back to the requested alias — (b) would reinstate exactly the defect the
// decision fixes, and would do it precisely in the cases nobody looks at.
//
// The honest answer keeps the half that IS known (the provider actually
// answered, and its name is known at every call site) and marks the unknown
// half with a placeholder that cannot be mistaken for a model name.
func TestPassthrough_ProviderWithoutModelIdentityIsHonest(t *testing.T) {
	h, cap := newHXC350Handler(t)

	code, resp := hxc350Post(t, h, hxc350SingleUserBody(map[string]any{
		"passthrough":    true,
		"force_provider": hxc350ForcedProviderName,
	}))
	require.Equal(t, http.StatusOK, code)

	model, _ := resp["model"].(string)

	if redMode() {
		assert.Equal(t, hxc350RequestModel, model,
			"RED expects the pre-fix defect: the alias comes back regardless of who answered")
		return
	}

	require.Equal(t, 1, cap.providerCalls(hxc350ForcedProviderName),
		"precondition: the forced provider must be the one that answered")
	assert.Equal(t, hxc350ForcedLabel, model,
		"a provider that reports no model id must still be named, with the unknown "+
			"half marked explicitly")
	assert.NotEqual(t, hxc350RequestModel, model,
		"a silent fallback to the requested alias is the defect, not a graceful default")
	assert.Contains(t, model, hxc350ForcedProviderName,
		"the half that IS known — which provider answered — must survive")
}
