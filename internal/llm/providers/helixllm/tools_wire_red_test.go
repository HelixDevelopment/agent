package helixllm

// HXC-349 — RED-baseline-on-the-broken-artifact + polarity switch (§11.4.115).
//
// DEFECT (reproduced on the pre-fix artifact): HelixAgent's OpenAI-compatible
// handler accepts a `tools` array, copies it into models.LLMRequest.Tools
// (internal/handlers/openai_compatible.go:2649-2660) and maps ToolCalls back
// (:2937-2974) — but the helixllm provider's own wire type
// (internal/llm/providers/helixllm/types.go) has NO Tools/ToolChoice field and
// its Message/Choice have NO ToolCalls field. The data therefore dies in
// translation: the request that leaves this provider for the upstream serving
// layer carries no tools at all, and any tool_calls the upstream returns are
// discarded. Measured end-to-end: prompt_tokens is IDENTICAL with and without
// a 15,770-byte tools array (42 == 42) — a zero delta proving the schema never
// reaches the model.
//
// POLARITY SWITCH (§11.4.115): one source, two roles.
//
//	RED_MODE=0 (DEFAULT, post-fix) — standing GREEN regression guard
//	                       asserting the defect is ABSENT. This is the
//	                       §11.4.135 permanent guard: it runs in the normal
//	                       suite and fails if the drop ever returns.
//	RED_MODE=1           — reproduce-and-assert-defect-PRESENT. This is the
//	                       original RED baseline; it PASSES on a pre-fix
//	                       artifact and FAILS on a fixed one.
//
// The default was RED_MODE=1 while the defect was live, and was flipped to 0
// when the fix landed — the §11.4.115 post-fix polarity flip. Both polarities
// were captured against BOTH artifacts before the flip (the full 2x2):
//
//	pre-fix  + RED_MODE=1 -> PASS (defect reproduced; wire body carried no
//	                               `tools` key at all)
//	pre-fix  + RED_MODE=0 -> FAIL (proves the GREEN assertions are real and
//	                               not tautologies)
//	post-fix + RED_MODE=0 -> PASS (defect absent; tools on the wire)
//	post-fix + RED_MODE=1 -> FAIL (defect genuinely gone)
//
// The tests drive the REAL Provider.Complete / Provider.CompleteStream against
// an httptest server that captures the exact bytes this provider puts on the
// wire — no mocking of the code under test, no assertion that merely agrees
// with the implementation (§11.4.245: the oracle is the OpenAI tool-calling
// contract, independent of this provider's code).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"dev.helix.agent/internal/models"
)

// redMode reports whether the suite runs in defect-reproduction polarity.
// Defaults to FALSE post-fix: the standing guard asserts the defect is ABSENT
// so a plain `go test ./...` is green and REGRESSES loudly if the drop returns
// (§11.4.115 post-fix flip + §11.4.135 permanent guard). Set RED_MODE=1 to
// re-run the original reproduction against a pre-fix artifact.
func redMode() bool {
	v := strings.TrimSpace(os.Getenv("RED_MODE"))
	if v == "" {
		return false
	}
	return v != "0" && !strings.EqualFold(v, "false")
}

// hxc349Tools is the fixture tool schema. Two distinct tools with distinct
// parameter names so a partial/garbled forward is detectable, not just a
// present/absent check.
func hxc349Tools() []models.Tool {
	return []models.Tool{
		{
			Type: "function",
			Function: models.ToolFunction{
				Name:        "hxc349_read_file",
				Description: "Read a file from the repository under test.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"hxc349_path_param": map[string]interface{}{
							"type":        "string",
							"description": "Absolute path to read.",
						},
					},
					"required": []interface{}{"hxc349_path_param"},
				},
			},
		},
		{
			Type: "function",
			Function: models.ToolFunction{
				Name:        "hxc349_run_command",
				Description: "Execute a shell command in the workspace.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"hxc349_cmd_param": map[string]interface{}{
							"type":        "string",
							"description": "Command line to execute.",
						},
					},
					"required": []interface{}{"hxc349_cmd_param"},
				},
			},
		},
	}
}

// captureServer returns an httptest server that records the raw request body
// of the first POST it receives and replies with respBody.
func captureServer(t *testing.T, respBody string) (*httptest.Server, *[]byte) {
	t.Helper()
	captured := new([]byte)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("capture server: read body: %v", err)
		}
		if len(*captured) == 0 {
			*captured = b
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

func newTestProvider(t *testing.T, endpoint string) *Provider {
	t.Helper()
	clearHelixLLMEndpointEnv(t)
	return NewProvider(Config{
		Endpoint: endpoint,
		Model:    "hxc349-test-model",
		Timeout:  10 * time.Second,
	})
}

const hxc349PlainResponse = `{"id":"c1","object":"chat.completion","created":1,"model":"hxc349-test-model",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"4"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`

// hxc349ToolCallResponse is what an upstream that HONOURS tools returns.
const hxc349ToolCallResponse = `{"id":"c2","object":"chat.completion","created":1,"model":"hxc349-test-model",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"",` +
	`"tool_calls":[{"id":"call_hxc349_1","type":"function",` +
	`"function":{"name":"hxc349_read_file","arguments":"{\"hxc349_path_param\":\"/tmp/x\"}"}}]},` +
	`"finish_reason":"tool_calls"}],` +
	`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

// TestHXC349_ToolsReachProviderWire is the primary reproduction: a request
// carrying tools MUST arrive at the provider's outbound wire with those tools
// intact. This is the exact layer the operator's end-to-end zero-token-delta
// measurement points at.
func TestHXC349_ToolsReachProviderWire(t *testing.T) {
	srv, captured := captureServer(t, hxc349PlainResponse)
	p := newTestProvider(t, srv.URL)

	req := &models.LLMRequest{
		Prompt:   "What is 2+2?",
		Messages: []models.Message{{Role: "user", Content: "What is 2+2?"}},
		ModelParams: models.ModelParameters{
			Model:     "hxc349-test-model",
			MaxTokens: 32,
		},
		Tools:      hxc349Tools(),
		ToolChoice: "required",
	}

	_, err := p.Complete(context.Background(), req)
	require.NoError(t, err, "provider must reach the capture server")
	require.NotEmpty(t, *captured, "capture server recorded no request body — positive control failed (§11.4.273)")

	var wire map[string]interface{}
	require.NoError(t, json.Unmarshal(*captured, &wire), "outbound body must be valid JSON")

	// Positive control (§11.4.273): the capture genuinely sees this request's
	// own fields. If this fails, an empty `tools` result proves nothing.
	require.Equal(t, "hxc349-test-model", wire["model"],
		"positive control: captured body must be THIS request")

	body := string(*captured)
	toolsPresent := wire["tools"] != nil
	choicePresent := wire["tool_choice"] != nil

	if redMode() {
		// RED_MODE=1: assert the DEFECT is present on the broken artifact.
		assert.False(t, toolsPresent,
			"RED baseline expects the defect: `tools` should be ABSENT from the provider's outbound body")
		assert.False(t, choicePresent,
			"RED baseline expects the defect: `tool_choice` should be ABSENT from the provider's outbound body")
		assert.NotContains(t, body, "hxc349_read_file",
			"RED baseline expects the defect: tool names should not reach the wire")
		t.Logf("HXC-349 RED: outbound body (%d bytes) = %s", len(body), body)
		return
	}

	// RED_MODE=0: standing GREEN guard — the defect must be ABSENT.
	require.True(t, toolsPresent, "GREEN: `tools` MUST be present on the provider's outbound body")
	require.True(t, choicePresent, "GREEN: `tool_choice` MUST be present on the provider's outbound body")

	toolsArr, ok := wire["tools"].([]interface{})
	require.True(t, ok, "GREEN: `tools` must serialise as a JSON array")
	require.Len(t, toolsArr, 2, "GREEN: both tools must be forwarded")

	// Full-fidelity check: names, types, descriptions AND nested parameter
	// property names must survive translation — not merely "an array exists".
	assert.Contains(t, body, "hxc349_read_file")
	assert.Contains(t, body, "hxc349_run_command")
	assert.Contains(t, body, "hxc349_path_param")
	assert.Contains(t, body, "hxc349_cmd_param")
	assert.Contains(t, body, "Execute a shell command in the workspace.")
	assert.Equal(t, "required", wire["tool_choice"])

	first, ok := toolsArr[0].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "function", first["type"])
	fn, ok := first["function"].(map[string]interface{})
	require.True(t, ok, "GREEN: each tool must carry a `function` object")
	assert.Equal(t, "hxc349_read_file", fn["name"])
	assert.NotNil(t, fn["parameters"], "GREEN: the JSON-Schema parameters must survive")
}

// TestHXC349_ToolCallsParsedBackFromUpstream: when the upstream RETURNS a
// tool_calls envelope, this provider must surface it on models.LLMResponse.
// Without it the handler's map-back at :2937-2974 has nothing to map.
func TestHXC349_ToolCallsParsedBackFromUpstream(t *testing.T) {
	srv, captured := captureServer(t, hxc349ToolCallResponse)
	p := newTestProvider(t, srv.URL)

	resp, err := p.Complete(context.Background(), &models.LLMRequest{
		Prompt:      "Read /tmp/x",
		Messages:    []models.Message{{Role: "user", Content: "Read /tmp/x"}},
		ModelParams: models.ModelParameters{Model: "hxc349-test-model"},
		Tools:       hxc349Tools(),
	})
	require.NoError(t, err)
	require.NotEmpty(t, *captured, "positive control: server must have been called")

	if redMode() {
		assert.Empty(t, resp.ToolCalls,
			"RED baseline expects the defect: upstream tool_calls are discarded by this provider")
		assert.Empty(t, resp.FinishReason,
			"RED baseline expects the defect: finish_reason is not propagated")
		t.Logf("HXC-349 RED: parsed response ToolCalls=%d FinishReason=%q", len(resp.ToolCalls), resp.FinishReason)
		return
	}

	require.Len(t, resp.ToolCalls, 1, "GREEN: the upstream tool call MUST be surfaced")
	tc := resp.ToolCalls[0]
	assert.Equal(t, "call_hxc349_1", tc.ID)
	assert.Equal(t, "function", tc.Type)
	assert.Equal(t, "hxc349_read_file", tc.Function.Name)
	assert.Contains(t, tc.Function.Arguments, "hxc349_path_param",
		"GREEN: the raw argument JSON must survive verbatim")
	assert.Equal(t, "tool_calls", resp.FinishReason,
		"GREEN: finish_reason must be propagated so the handler can build a correct envelope")
}

// TestHXC349_StreamToolsReachProviderWire: the streaming path builds its own
// ChatCompletionRequest and drops tools identically.
func TestHXC349_StreamToolsReachProviderWire(t *testing.T) {
	srv, captured := captureServer(t, hxc349PlainResponse)
	p := newTestProvider(t, srv.URL)

	ch, err := p.CompleteStream(context.Background(), &models.LLMRequest{
		Prompt:      "What is 2+2?",
		Messages:    []models.Message{{Role: "user", Content: "What is 2+2?"}},
		ModelParams: models.ModelParameters{Model: "hxc349-test-model"},
		Tools:       hxc349Tools(),
		ToolChoice:  "auto",
	})
	require.NoError(t, err)
	for range ch { // drain so the stream goroutine finishes
	}
	require.NotEmpty(t, *captured, "positive control: server must have been called")

	var wire map[string]interface{}
	require.NoError(t, json.Unmarshal(*captured, &wire))
	require.Equal(t, true, wire["stream"], "positive control: this must be the STREAMING request")

	if redMode() {
		assert.Nil(t, wire["tools"], "RED baseline expects the defect: streaming path drops `tools`")
		assert.Nil(t, wire["tool_choice"], "RED baseline expects the defect: streaming path drops `tool_choice`")
		t.Logf("HXC-349 RED (stream): outbound body = %s", string(*captured))
		return
	}

	require.NotNil(t, wire["tools"], "GREEN: streaming path MUST forward `tools`")
	assert.Equal(t, "auto", wire["tool_choice"])
	assert.Contains(t, string(*captured), "hxc349_run_command")
}

// ---------------------------------------------------------------------------
// REGRESSION GUARDS — polarity-INDEPENDENT. These must hold in BOTH modes,
// before and after the fix. A request with no tools must be byte-for-byte
// unaffected, and streaming (the one capability this provider currently
// declares) must keep working.
// ---------------------------------------------------------------------------

// TestHXC349_Regression_NoToolsRequestUnaffected: omitting tools must leave the
// wire free of any tools/tool_choice keys — `omitempty` must genuinely elide
// them, so no upstream sees a spurious empty array.
func TestHXC349_Regression_NoToolsRequestUnaffected(t *testing.T) {
	srv, captured := captureServer(t, hxc349PlainResponse)
	p := newTestProvider(t, srv.URL)

	resp, err := p.Complete(context.Background(), &models.LLMRequest{
		Prompt:      "What is 2+2?",
		Messages:    []models.Message{{Role: "user", Content: "What is 2+2?"}},
		ModelParams: models.ModelParameters{Model: "hxc349-test-model", MaxTokens: 32},
	})
	require.NoError(t, err)
	require.NotEmpty(t, *captured)

	var wire map[string]interface{}
	require.NoError(t, json.Unmarshal(*captured, &wire))

	assert.Nil(t, wire["tools"], "no-tools request must not emit a `tools` key in EITHER polarity")
	assert.Nil(t, wire["tool_choice"], "no-tools request must not emit a `tool_choice` key in EITHER polarity")
	assert.NotContains(t, string(*captured), "tool", "no tool-related key may leak into a plain request")

	// The plain completion path itself must be undisturbed.
	assert.Equal(t, "4", resp.Content)
	assert.Equal(t, 11, resp.TokensUsed)
	assert.Equal(t, float64(10), toFloat(resp.Metadata["prompt_tokens"]))
	assert.Empty(t, resp.ToolCalls, "a no-tools request must never invent tool calls")
}

// TestHXC349_Regression_StreamingStillWorks: streaming is the ONE capability
// this provider declares today. It must keep delivering content chunks.
func TestHXC349_Regression_StreamingStillWorks(t *testing.T) {
	// handleStream decodes CONCATENATED JSON objects off the body (it does not
	// parse `data:` SSE framing) — emit exactly what that parser consumes so
	// the test exercises the real code path.
	chunks := `{"id":"s1","model":"hxc349-test-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hel"}}],"usage":{"total_tokens":1}}` +
		`{"id":"s1","model":"hxc349-test-model","choices":[{"index":0,"message":{"role":"assistant","content":"lo"}}],"usage":{"total_tokens":2}}`
	srv, captured := captureServer(t, chunks)
	p := newTestProvider(t, srv.URL)

	ch, err := p.CompleteStream(context.Background(), &models.LLMRequest{
		Prompt:      "hi",
		Messages:    []models.Message{{Role: "user", Content: "hi"}},
		ModelParams: models.ModelParameters{Model: "hxc349-test-model"},
	})
	require.NoError(t, err)

	var got strings.Builder
	n := 0
	for c := range ch {
		require.Nil(t, c.Metadata["error"], "no stream decode error expected: %v", c.Metadata["error"])
		got.WriteString(c.Content)
		n++
	}
	require.NotEmpty(t, *captured)
	assert.Equal(t, 2, n, "both stream chunks must be delivered")
	assert.Equal(t, "Hello", got.String(), "streamed content must reassemble intact")
}

// toFloat normalises a JSON-decoded numeric metadata value for comparison.
func toFloat(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return -1
	}
}
