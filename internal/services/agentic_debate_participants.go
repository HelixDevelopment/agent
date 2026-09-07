package services

import (
	"os"
	"strconv"
	"strings"

	"dev.helix.agent/internal/models"
)

// Ensemble debate participant wiring (spec 002 / HA-F2-003).
//
// WHY THIS FILE EXISTS. AgenticEnsemble.toolAugmentedDebate used to build a
// DebateConfig with NEITHER Participants NOR Metadata["source"]. Both
// omissions were load-bearing:
//
//   - No Metadata["source"] meant DebateService.ConductDebate did not take its
//     "OpenAI source detected, bypassing comprehensive system" branch, so the
//     request fell through to conductComprehensiveDebate — a path that is
//     STRUCTURALLY incapable of producing model-generated text (the upstream
//     digital.vasic.debate/comprehensive IntegrationManager builds its
//     orchestrator with a nil ProviderInvoker and no setter to change it, and
//     its DebateResponse carries no final-answer field at all).
//   - No Participants meant that even on the real path executeRound would have
//     iterated an empty roster and produced zero responses.
//
// Fixing only one of the two yields a different bug (a stub answer, or an
// empty one). This file supplies the roster; toolAugmentedDebate supplies the
// routing metadata.
//
// LOCAL-FIRST (spec 002, HA-F2-002). Everything served through HelixLLM runs
// locally; cloud providers are disabled by default behind HELIX_CLOUD_PROVIDERS.
// The roster therefore prefers the local serving provider and only falls back
// to a registry-ordered provider when the local one is unavailable. No host,
// port or URL appears here (CONST-045) — endpoint resolution stays inside the
// provider, which honours HELIX_LLM_LOCAL_OPENAI_ENDPOINT first.
const (
	// localFirstServingProvider is the registry NAME (not a host) of the local
	// llama.cpp/Colibri chain registered by registerDefaultProviders.
	localFirstServingProvider = "helixllm"

	// envDebateProvider lets an operator name the debate provider explicitly.
	// An explicitly named provider is never second-guessed.
	envDebateProvider = "HELIXAGENT_DEBATE_PROVIDER"

	// envDebateModel overrides the model used by debate participants.
	envDebateModel = "HELIXAGENT_DEBATE_MODEL"

	// envLocalModel is the local-first serving model already used by the
	// running service (see the helixllm provider).
	envLocalModel = "HELIX_LLM_LOCAL_MODEL"

	// envDebateParticipants bounds the roster size. More participants means a
	// richer debate but proportionally more model calls per round.
	envDebateParticipants = "HELIXAGENT_DEBATE_PARTICIPANTS"

	// defaultDebateParticipants is the roster size when unset: enough voices
	// for a genuine multi-perspective debate, few enough to keep latency sane
	// on a local model.
	defaultDebateParticipants = 3

	// envEnableComprehensiveDebate opts INTO the upstream comprehensive
	// orchestrator. It is off by default because that orchestrator has no LLM
	// provider wired and therefore cannot answer a prompt; see
	// debate_service_comprehensive.go and NewDebateServiceWithDeps.
	envEnableComprehensiveDebate = "HELIXAGENT_ENABLE_COMPREHENSIVE_DEBATE"
)

// totalDebateTokens sums the tokens actually reported by every participant
// call across every round of a debate.
//
// getParticipantResponse records each call's provider-reported count under
// ParticipantResponse.Metadata["tokens_used"]; DebateResult itself has no
// aggregate token field. AllResponses (not Participants) is summed because it
// holds every round, whereas Participants keeps only the latest response per
// participant. A participant whose provider reported nothing contributes 0 —
// the total is never padded or estimated (§11.4.6: report what was measured).
func totalDebateTokens(dr *DebateResult) int {
	_, _, total := DebateTokenTotals(dr)
	return total
}

// DebateTokenTotals sums what every participant's provider ACTUALLY
// reported, in all three directions.
//
// Why the split and not just the total: a debate's OpenAI-compatible
// envelope is built from the selected response's TokenSplit(), so a
// selected response carrying only an aggregate publishes
// `prompt_tokens: 0, completion_tokens: 0` beside a real `total_tokens`.
// That was the live shape measured 2026-09-07 on POST /v1/chat/completions
// with model=helixagent-debate: `{"prompt_tokens":0,"completion_tokens":0,
// "total_tokens":613}` next to the reply "Paris". The per-direction numbers
// were never missing from the providers — they were simply never summed,
// because ParticipantResponse.Metadata recorded only "tokens_used".
//
// Each participant contributes its OWN reported split, read through the
// same models.LLMResponse.TokenSplit() contract the rest of the codebase
// uses, so the two-naming-convention handling (OpenAI-shaped
// prompt/completion, Anthropic-shaped input/output) and the numeric-type
// tolerance are inherited rather than re-implemented here.
//
// A participant whose provider reported no split contributes 0 to both
// directions while still contributing its total — so callers can detect
// exactly that case by comparing prompt+completion against total, and MUST
// NOT publish a split that does not account for the whole total (see
// debateResultToEnsemble). Nothing here derives, halves, or otherwise
// invents a direction (§11.4.6).
func DebateTokenTotals(dr *DebateResult) (prompt, completion, total int) {
	if dr == nil {
		return 0, 0, 0
	}
	for _, r := range dr.AllResponses {
		if r.Metadata == nil {
			continue
		}
		// Reuse the canonical accessor by presenting this participant's
		// recorded numbers in the shape it reads.
		resp := &models.LLMResponse{
			TokensUsed: metadataTokenCount(r.Metadata, "tokens_used"),
			Metadata:   r.Metadata,
		}
		p, c, t := resp.TokenSplit()
		prompt += p
		completion += c
		total += t
	}
	return prompt, completion, total
}

// metadataTokenCount reads a non-negative token count out of a participant
// metadata map, tolerating the numeric types a value can take before and
// after a JSON round-trip. An absent or unparseable value is 0 — reported
// as "not reported", never guessed at.
func metadataTokenCount(md map[string]any, key string) int {
	if md == nil {
		return 0
	}
	switch v := md[key].(type) {
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	}
	return 0
}

// resolveDebateProvider returns the provider name the ensemble debate should
// use and whether one could be resolved at all.
//
// Precedence (each step verified against the registry — never assumed):
//  1. HELIXAGENT_DEBATE_PROVIDER, when it resolves.
//  2. The local-first serving provider, when enabled and it resolves.
//  3. The highest-scored registry provider that resolves.
//
// A provider that does not resolve is skipped rather than returned, so the
// caller never builds a roster pointing at a provider the registry cannot hand
// out (which would fail later as an opaque per-participant error).
func (e *AgenticEnsemble) resolveDebateProvider() (string, bool) {
	if e.providerRegistry == nil {
		return "", false
	}

	resolves := func(name string) bool {
		if name == "" {
			return false
		}
		_, err := e.providerRegistry.GetProvider(name)
		return err == nil
	}

	if explicit := strings.TrimSpace(os.Getenv(envDebateProvider)); explicit != "" {
		if resolves(explicit) {
			return explicit, true
		}
		e.logger.WithField("provider", explicit).
			Warn("[AgenticEnsemble] " + envDebateProvider + " names a provider the registry cannot resolve; falling back")
	}

	if HelixLLMEnabledDefault() && resolves(localFirstServingProvider) {
		return localFirstServingProvider, true
	}

	for _, name := range e.providerRegistry.ListProvidersOrderedByScore() {
		if resolves(name) {
			return name, true
		}
	}
	return "", false
}

// debateParticipantCount returns the configured roster size, clamped to a sane
// range. A non-numeric or out-of-range value falls back to the default rather
// than failing the request.
func debateParticipantCount() int {
	n := defaultDebateParticipants
	if raw := strings.TrimSpace(os.Getenv(envDebateParticipants)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 2 && parsed <= 7 {
			n = parsed
		}
	}
	return n
}

// debateModelsFor returns the model list participants should use for the given
// provider. For the local-first provider the operator-configured local model
// wins, because the generic capability list is not authoritative for it.
func (e *AgenticEnsemble) debateModelsFor(provider string) []string {
	if explicit := strings.TrimSpace(os.Getenv(envDebateModel)); explicit != "" {
		return []string{explicit}
	}
	if provider == localFirstServingProvider {
		if local := strings.TrimSpace(os.Getenv(envLocalModel)); local != "" {
			return []string{local}
		}
	}
	if e.debateService != nil {
		if models := e.debateService.GetAvailableModelsForProvider(provider); len(models) > 0 {
			return models
		}
	}
	return nil
}

// buildDebateParticipants assembles the participant roster for an
// ensemble-driven debate, or nil when no provider can be resolved.
//
// Roster construction is delegated to the existing, already-tested
// CreateSingleProviderParticipants, which assigns each instance a distinct
// role, perspective and temperature (§11.4.74 — extend, don't reimplement).
// Returning nil is deliberate: the caller must surface "no provider" honestly
// rather than fabricate a debate.
func (e *AgenticEnsemble) buildDebateParticipants(topic string) []ParticipantConfig {
	if e.debateService == nil {
		return nil
	}

	provider, ok := e.resolveDebateProvider()
	if !ok {
		e.logger.Warn("[AgenticEnsemble] no LLM provider resolvable for debate; " +
			"cannot build a participant roster")
		return nil
	}

	models := e.debateModelsFor(provider)
	if len(models) == 0 {
		e.logger.WithField("provider", provider).
			Warn("[AgenticEnsemble] provider resolved but no model could be determined for debate")
		return nil
	}

	participants := e.debateService.CreateSingleProviderParticipants(&SingleProviderConfig{
		ProviderName:      provider,
		AvailableModels:   models,
		NumParticipants:   debateParticipantCount(),
		UseModelDiversity: len(models) > 1,
		UseTempDiversity:  true,
	}, topic)

	e.logger.WithFields(map[string]any{
		"provider":     provider,
		"models":       models,
		"participants": len(participants),
	}).Info("[AgenticEnsemble] debate participant roster built from real providers")

	return participants
}
