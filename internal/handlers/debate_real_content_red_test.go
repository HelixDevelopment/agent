package handlers_test

// §11.4.115 RED-baseline-on-the-broken-artifact + polarity switch.
//
// DEFECT (measured 2026-09-07 against the running :7061 service): the
// advertised models `helixagent-debate` / `helixagent-ensemble` /
// `helix-debate` returned a BYTE-IDENTICAL fixed status string for
// completely different prompts, with `usage` all zeros:
//
//	prompt "What is 2+2? ..."            -> "Comprehensive debate completed with 3 rounds"
//	prompt "Name the capital of France"  -> "Comprehensive debate completed with 3 rounds"
//	usage: {"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}
//	total_duration_ms: 4 and 0  (no LLM call is physically possible)
//
// ROOT CAUSE (traced, not guessed):
//   - services.AgenticEnsemble.toolAugmentedDebate built a DebateConfig with
//     NEITHER Participants NOR Metadata["source"].
//   - DebateService.ConductDebate bypasses the comprehensive system ONLY when
//     Metadata["source"] is set, so the request fell through to
//     conductComprehensiveDebate.
//   - That path delegates to digital.vasic.debate/comprehensive, whose
//     IntegrationManager builds orchestrator.NewOrchestrator(nil, nil, ocfg)
//     with ZERO options, leaving the ProviderInvoker nil. Its
//     synthesiseContent() is self-labelled "deterministic-stub-content
//     awaiting provider wiring", and its DebateResponse carries NO final
//     answer field at all — so no real text can ever reach the caller.
//   - debate_service_comprehensive.go:72-73 then discards even that and
//     substitutes the fixed "Comprehensive debate completed with N rounds".
//
// THE FALSIFYING PROPERTY: two DIFFERENT prompts MUST produce two DIFFERENT
// responses. A stub cannot satisfy it; a real LLM cannot fail it.
//
// POLARITY SWITCH (one source, two roles):
//
//	RED_MODE=1 (default) — reproduce-and-assert-the-defect-is-PRESENT.
//	                       PASSES on the broken pre-fix artifact.
//	RED_MODE=0           — the standing GREEN regression guard: asserts the
//	                       defect is ABSENT (different prompts -> different
//	                       answers, and usage is really accounted).
//
// Run:
//
//	RED_MODE=1 ... -run TestDebateModelsReturnPromptDependentContent   (pre-fix)
//	RED_MODE=0 ... -run TestDebateModelsReturnPromptDependentContent   (post-fix)

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// debateEndpoint returns the base URL of the HelixAgent service under test.
// CONST-045: no hardcoded distribution host — overridable via env.
func debateEndpoint() string {
	if v := strings.TrimSpace(os.Getenv("HELIXAGENT_TEST_ENDPOINT")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:7061"
}

// redMode reports whether the test runs in defect-reproduction polarity.
// Default 1 (reproduce) per §11.4.115.
func redMode() bool {
	v := strings.TrimSpace(os.Getenv("RED_MODE"))
	if v == "" {
		return true
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

type chatResp struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// askDebate sends one prompt to the given model and returns the assistant
// content plus the reported total token usage.
func askDebate(t *testing.T, model, prompt string) (string, int) {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	// A real multi-round debate over a local model is not fast; allow for it.
	client := &http.Client{Timeout: 10 * time.Minute}
	req, err := http.NewRequest(http.MethodPost,
		debateEndpoint()+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions (model=%s): %v", model, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out chatResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response (model=%s, http=%d): %v", model, resp.StatusCode, err)
	}
	if len(out.Choices) == 0 {
		t.Fatalf("model=%s returned no choices (http=%d)", model, resp.StatusCode)
	}
	return out.Choices[0].Message.Content, out.Usage.TotalTokens
}

// requireServiceUp skips honestly (§11.4.3) when the service under test is not
// running — an unreachable service is NEVER a silent pass and NEVER a failure.
func requireServiceUp(t *testing.T) {
	t.Helper()
	host := strings.TrimPrefix(strings.TrimPrefix(debateEndpoint(), "http://"), "https://")
	conn, err := net.DialTimeout("tcp", host, 3*time.Second)
	if err != nil {
		t.Skipf("SKIP-OK: HelixAgent not reachable at %s (%v) — "+
			"start it and re-run; this is an honest skip, not a pass",
			debateEndpoint(), err)
	}
	_ = conn.Close()
}

// The three models this defect covers. All are advertised at GET /v1/models
// and are selectable as Claude Toolkit provider aliases.
var debateModels = []string{"helixagent-debate", "helixagent-ensemble", "helix-debate"}

// Two prompts with unmistakably different correct answers. If a response is
// genuinely produced from the prompt, these cannot collide.
const (
	promptA = "What is 2+2? Reply with just the number and nothing else."
	promptB = "What is the capital city of France? Reply with just the city name."
)

func TestDebateModelsReturnPromptDependentContent(t *testing.T) {
	requireServiceUp(t)

	for _, model := range debateModels {
		t.Run(model, func(t *testing.T) {
			answerA, usageA := askDebate(t, model, promptA)
			answerB, usageB := askDebate(t, model, promptB)

			t.Logf("model=%s\n  promptA -> %q (total_tokens=%d)\n  promptB -> %q (total_tokens=%d)",
				model, truncate(answerA), usageA, truncate(answerB), usageB)

			if redMode() {
				// RED polarity: assert the DEFECT IS PRESENT on this artifact.
				if answerA != answerB {
					t.Fatalf("RED_MODE=1 expected the defect (identical answers for "+
						"different prompts) but model=%s produced DIFFERENT answers.\n"+
						"  A=%q\n  B=%q\n"+
						"The defect appears fixed — re-run with RED_MODE=0 to use this "+
						"test as the standing regression guard.",
						model, truncate(answerA), truncate(answerB))
				}
				t.Logf("RED confirmed on model=%s: byte-identical answer %q for two "+
					"different prompts (usage totals %d/%d)",
					model, truncate(answerA), usageA, usageB)
				return
			}

			// GREEN polarity: the standing regression guard.
			if answerA == answerB {
				t.Fatalf("model=%s returned a BYTE-IDENTICAL answer for two different "+
					"prompts — the stub defect is present.\n  A=%q\n  B=%q",
					model, truncate(answerA), truncate(answerB))
			}
			for _, s := range []struct {
				label, text string
			}{{"A", answerA}, {"B", answerB}} {
				if strings.TrimSpace(s.text) == "" {
					t.Fatalf("model=%s answer %s is empty — an empty answer is not a fix",
						model, s.label)
				}
				// The upstream orchestrator self-labels its canned output; if that
				// marker ever reaches a user, no real LLM call was made.
				if strings.Contains(s.text, "[synthesised ") ||
					strings.Contains(s.text, "deterministic-stub-content") {
					t.Fatalf("model=%s answer %s carries the upstream stub marker: %q",
						model, s.label, truncate(s.text))
				}
				if strings.Contains(s.text, "Comprehensive debate completed with") {
					t.Fatalf("model=%s answer %s is the fixed status string, not content: %q",
						model, s.label, truncate(s.text))
				}
			}
			// All-zero usage alongside a 200 is itself a tell that no model ran.
			if usageA == 0 || usageB == 0 {
				t.Fatalf("model=%s reported zero total_tokens (A=%d B=%d) — a real "+
					"model call must account tokens", model, usageA, usageB)
			}
		})
	}
}

func truncate(s string) string {
	const max = 220
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return fmt.Sprintf("%s...[%d chars total]", s[:max], len(s))
}
