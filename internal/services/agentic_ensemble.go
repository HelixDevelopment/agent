package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"dev.helix.agent/internal/llm"
	"dev.helix.agent/internal/models"
)

// AgenticEnsemble orchestrates the AI ensemble as a unified LLM with
// dual-mode operation: tool-augmented reasoning and autonomous execution.
//
// In Reason mode the ensemble delegates to the debate service augmented
// with iterative tool resolution. In Execute mode the ensemble decomposes
// the request into tasks, dispatches them through an agent worker pool,
// verifies results, and synthesises a final response.
type AgenticEnsemble struct {
	debateService    *DebateService
	intentClassifier *LLMIntentClassifier
	toolExecutor     *IterativeToolExecutor
	planner          *ExecutionPlanner
	verifier         *VerificationDebate
	providerRegistry *ProviderRegistry
	config           AgenticEnsembleConfig
	logger           *logrus.Logger
}

// NewAgenticEnsemble creates an AgenticEnsemble with the given
// dependencies and configuration. Nil dependencies are tolerated; the
// orchestrator gracefully degrades when a subsystem is unavailable.
func NewAgenticEnsemble(
	debateService *DebateService,
	intentClassifier *LLMIntentClassifier,
	toolExecutor *IterativeToolExecutor,
	planner *ExecutionPlanner,
	verifier *VerificationDebate,
	providerRegistry *ProviderRegistry,
	config AgenticEnsembleConfig,
	logger *logrus.Logger,
) *AgenticEnsemble {
	if logger == nil {
		logger = logrus.New()
	}
	return &AgenticEnsemble{
		debateService:    debateService,
		intentClassifier: intentClassifier,
		toolExecutor:     toolExecutor,
		planner:          planner,
		verifier:         verifier,
		providerRegistry: providerRegistry,
		config:           config,
		logger:           logger,
	}
}

// Process is the main entry point. It classifies the request intent,
// selects the appropriate operating mode, and returns the ensemble
// result.
func (e *AgenticEnsemble) Process(
	ctx context.Context,
	req *models.LLMRequest,
) (*EnsembleResult, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}

	globalCtx, cancel := context.WithTimeout(ctx, e.config.GlobalTimeout)
	defer cancel()

	startTime := time.Now()

	mode := e.classifyMode(globalCtx, req)

	e.logger.WithFields(logrus.Fields{
		"mode":       mode.String(),
		"request_id": req.ID,
	}).Info("[AgenticEnsemble] Processing request")

	var result *EnsembleResult
	var err error

	switch mode {
	case AgenticModeExecute:
		if !e.config.EnableExecution {
			e.logger.Info(
				"[AgenticEnsemble] Execution disabled, " +
					"falling back to reason mode",
			)
			result, err = e.toolAugmentedDebate(globalCtx, req)
		} else {
			result, err = e.agenticExecutionLoop(globalCtx, req)
		}
	default:
		result, err = e.toolAugmentedDebate(globalCtx, req)
	}

	if err != nil {
		return nil, fmt.Errorf("agentic ensemble %s mode: %w", mode, err)
	}

	// Attach agentic metadata if not already present.
	if result != nil && result.Metadata != nil {
		if _, exists := result.Metadata["agentic"]; !exists {
			result.Metadata["agentic"] = &AgenticMetadata{
				Mode:            mode.String(),
				TotalDurationMs: time.Since(startTime).Milliseconds(),
				ProvenanceID:    uuid.New().String(),
			}
		}
	}

	return result, nil
}

// classifyMode uses the LLM intent classifier to determine whether the
// request is informational (reason) or actionable (execute).
func (e *AgenticEnsemble) classifyMode(
	ctx context.Context,
	req *models.LLMRequest,
) AgenticMode {
	userMessage := e.extractUserMessage(req)
	if userMessage == "" {
		return AgenticModeReason
	}

	if e.intentClassifier == nil {
		return AgenticModeReason
	}

	classification, err := e.intentClassifier.ClassifyIntentWithLLM(
		ctx, userMessage, "",
	)
	if err != nil {
		e.logger.WithError(err).Warn(
			"[AgenticEnsemble] Intent classification failed, " +
				"defaulting to reason mode",
		)
		return AgenticModeReason
	}

	if classification == nil {
		return AgenticModeReason
	}

	// Map intent classification to agentic mode.
	if classification.IsActionable &&
		(classification.Intent == IntentRequest ||
			classification.Intent == IntentConfirmation) {
		return AgenticModeExecute
	}

	return AgenticModeReason
}

// toolAugmentedDebate runs the existing debate service augmented with
// tool execution. The debate handles multi-provider reasoning internally;
// this method wraps it into an EnsembleResult.
func (e *AgenticEnsemble) toolAugmentedDebate(
	ctx context.Context,
	req *models.LLMRequest,
) (*EnsembleResult, error) {
	if e.debateService == nil {
		return nil, fmt.Errorf("debate service not configured")
	}

	topic := e.extractUserMessage(req)
	if topic == "" {
		topic = req.Prompt
	}

	debateID := fmt.Sprintf("ae-%s", uuid.New().String()[:8])

	// A debate without a participant roster produces nothing, and a debate
	// without Metadata["source"] is routed by ConductDebate into the
	// comprehensive system — which cannot produce model-generated text at all
	// (see agentic_debate_participants.go). Both must be supplied together.
	participants := e.buildDebateParticipants(topic)
	if len(participants) == 0 {
		return nil, fmt.Errorf(
			"debate unavailable: no LLM provider could be resolved for the ensemble " +
				"debate roster; configure a provider (or set " + envDebateProvider +
				") — refusing to return a synthesised answer")
	}

	config := &DebateConfig{
		DebateID:     debateID,
		Topic:        topic,
		Participants: participants,
		MaxRounds:    3,
		Timeout:      e.config.AgentTimeout,
		Strategy:     "confidence_weighted",
		// "source" routes ConductDebate to the real, provider-backed debate
		// (conductRealDebate) instead of the comprehensive stub path.
		Metadata: map[string]any{"source": "agentic_ensemble"},
	}

	debateResult, err := e.debateService.ConductDebate(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("debate failed: %w", err)
	}

	return e.debateResultToEnsemble(debateResult, AgenticModeReason), nil
}

// agenticExecutionLoop implements the full 7-stage autonomous execution
// pipeline: understand, plan, assign, execute, verify, synthesise,
// respond.
func (e *AgenticEnsemble) agenticExecutionLoop(
	ctx context.Context,
	req *models.LLMRequest,
) (*EnsembleResult, error) {
	startTime := time.Now()
	var stagesCompleted []string
	var allToolExecs []AgenticToolExecution

	// Stage 1: UNDERSTAND — run a tool-augmented debate to analyse the
	// request.
	e.logger.Info("[AgenticEnsemble] Stage 1: UNDERSTAND")
	understandResult, err := e.toolAugmentedDebate(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("understand stage: %w", err)
	}
	stagesCompleted = append(stagesCompleted, "understand")

	decision := e.extractContent(understandResult)

	// Stage 2: PLAN — decompose the understanding into tasks.
	e.logger.Info("[AgenticEnsemble] Stage 2: PLAN")
	tasks, planErr := e.planTasks(ctx, decision)
	if planErr != nil {
		e.logger.WithError(planErr).Warn(
			"[AgenticEnsemble] Planning failed, " +
				"returning understand result",
		)
		return understandResult, nil
	}
	stagesCompleted = append(stagesCompleted, "plan")

	if len(tasks) == 0 {
		return understandResult, nil
	}

	// Stage 3: ASSIGN — build dependency graph layers.
	e.logger.Info("[AgenticEnsemble] Stage 3: ASSIGN")
	layers, layerErr := e.buildLayers(tasks)
	if layerErr != nil {
		e.logger.WithError(layerErr).Warn(
			"[AgenticEnsemble] Layer build failed, " +
				"returning understand result",
		)
		return understandResult, nil
	}
	stagesCompleted = append(stagesCompleted, "assign")

	// Stage 4: EXECUTE — dispatch tasks through the worker pool.
	e.logger.Info("[AgenticEnsemble] Stage 4: EXECUTE")
	results, execErr := e.executeTasks(ctx, layers)
	if execErr != nil {
		return nil, fmt.Errorf("execute stage: %w", execErr)
	}
	stagesCompleted = append(stagesCompleted, "execute")

	// Collect tool executions from results.
	for _, r := range results {
		allToolExecs = append(allToolExecs, r.ToolCalls...)
	}

	// Stage 5: VERIFY — run verification debate on results.
	e.logger.Info("[AgenticEnsemble] Stage 5: VERIFY")
	verification := e.verifyResults(ctx, req, results)
	stagesCompleted = append(stagesCompleted, "verify")

	// Stage 6: SYNTHESISE — combine results into a coherent response.
	e.logger.Info("[AgenticEnsemble] Stage 6: SYNTHESISE")
	synthesised := e.synthesiseResults(results, verification)
	stagesCompleted = append(stagesCompleted, "synthesise")

	// Stage 7: RESPOND — build final EnsembleResult with metadata.
	e.logger.Info("[AgenticEnsemble] Stage 7: RESPOND")
	stagesCompleted = append(stagesCompleted, "respond")

	completedCount := 0
	for _, r := range results {
		if r.Error == nil {
			completedCount++
		}
	}

	toolSummary := e.buildToolSummary(allToolExecs)

	ensembleResult := &EnsembleResult{
		Selected: &models.LLMResponse{
			ID:           fmt.Sprintf("ae-%s", uuid.New().String()[:8]),
			Content:      synthesised,
			ProviderName: "agentic-ensemble",
			Confidence:   e.calculateConfidence(verification),
			ResponseTime: time.Since(startTime).Milliseconds(),
		},
		VotingMethod: "agentic_execution",
		Metadata: map[string]any{
			"agentic": &AgenticMetadata{
				Mode:            AgenticModeExecute.String(),
				StagesCompleted: stagesCompleted,
				AgentsSpawned:   len(results),
				TasksCompleted:  completedCount,
				ToolsInvoked:    toolSummary,
				TotalDurationMs: time.Since(startTime).Milliseconds(),
				ProvenanceID:    uuid.New().String(),
			},
		},
	}

	return ensembleResult, nil
}

// planTasks uses the execution planner to decompose a decision into
// tasks. When the planner is nil or unavailable it returns an empty
// list.
func (e *AgenticEnsemble) planTasks(
	ctx context.Context,
	decision string,
) ([]AgenticTask, error) {
	if e.planner == nil {
		return nil, fmt.Errorf("execution planner not configured")
	}

	completeFunc := e.getCompleteFunc()
	if completeFunc == nil {
		return nil, fmt.Errorf(
			"no LLM provider available for task decomposition",
		)
	}

	return e.planner.DecomposePlan(ctx, decision, completeFunc)
}

// buildLayers delegates to the planner's dependency graph builder.
func (e *AgenticEnsemble) buildLayers(
	tasks []AgenticTask,
) ([][]AgenticTask, error) {
	if e.planner == nil {
		return nil, fmt.Errorf("execution planner not configured")
	}
	return e.planner.BuildDependencyGraph(tasks)
}

// executeTasks dispatches tasks through the agent worker pool and
// collects results.
func (e *AgenticEnsemble) executeTasks(
	ctx context.Context,
	layers [][]AgenticTask,
) ([]AgenticResult, error) {
	pool := NewAgentWorkerPool(
		e.config.MaxConcurrentAgents,
		e.logger,
	)
	defer pool.Shutdown()

	completeFunc := e.getCompleteFunc()
	if completeFunc == nil {
		return nil, fmt.Errorf(
			"no LLM provider available for task execution",
		)
	}

	resultCh, err := pool.DispatchTasks(
		ctx,
		layers,
		completeFunc,
		e.toolExecutor,
		e.config.MaxIterationsPerAgent,
	)
	if err != nil {
		return nil, fmt.Errorf("dispatch tasks: %w", err)
	}

	var results []AgenticResult
	for r := range resultCh {
		results = append(results, r)
	}

	return results, nil
}

// verifyResults runs the verification debate on execution results and
// returns the verification result. When the verifier is nil it returns a
// default pass.
func (e *AgenticEnsemble) verifyResults(
	ctx context.Context,
	req *models.LLMRequest,
	results []AgenticResult,
) *AgenticVerificationResult {
	if e.verifier == nil || len(results) == 0 {
		return &AgenticVerificationResult{
			Approved:   true,
			Confidence: 0.5,
			Summary:    "Verification skipped (verifier not configured)",
		}
	}

	completeFunc := e.getCompleteFunc()
	if completeFunc == nil {
		return &AgenticVerificationResult{
			Approved:   true,
			Confidence: 0.5,
			Summary:    "Verification skipped (no LLM available)",
		}
	}

	originalRequest := e.extractUserMessage(req)
	if originalRequest == "" {
		originalRequest = req.Prompt
	}

	verification, err := e.verifier.Verify(
		ctx, originalRequest, results,
		func(
			vCtx context.Context,
			msgs []models.Message,
		) (*models.LLMResponse, error) {
			return completeFunc(vCtx, msgs)
		},
	)
	if err != nil {
		e.logger.WithError(err).Warn(
			"[AgenticEnsemble] Verification failed",
		)
		return &AgenticVerificationResult{
			Approved:   true,
			Confidence: 0.5,
			Issues:     []string{err.Error()},
			Summary:    "Verification encountered an error",
		}
	}

	return verification
}

// synthesiseResults combines agent results into a single coherent
// response string.
func (e *AgenticEnsemble) synthesiseResults(
	results []AgenticResult,
	verification *AgenticVerificationResult,
) string {
	if len(results) == 0 {
		return "No results were produced."
	}

	var parts []string
	for _, r := range results {
		if r.Error != nil {
			continue
		}
		if r.Content != "" {
			parts = append(parts, r.Content)
		}
	}

	if len(parts) == 0 {
		return "All agent tasks failed during execution."
	}

	synthesised := strings.Join(parts, "\n\n")

	if verification != nil && !verification.Approved &&
		len(verification.Issues) > 0 {
		synthesised += "\n\n[Verification notes: " +
			strings.Join(verification.Issues, "; ") + "]"
	}

	return synthesised
}

// getCompleteFunc returns a CompleteFunc backed by the best available
// tool-capable provider. Returns nil when no provider is available.
func (e *AgenticEnsemble) getCompleteFunc() CompleteFunc {
	provider := e.selectToolCapableProvider()
	if provider == nil {
		return nil
	}

	return func(
		ctx context.Context,
		messages []models.Message,
	) (*models.LLMResponse, error) {
		req := &models.LLMRequest{
			ID:       fmt.Sprintf("ae-call-%s", uuid.New().String()[:8]),
			Messages: messages,
			ModelParams: models.ModelParameters{
				Temperature: 0.7,
				MaxTokens:   4096,
			},
		}
		return provider.Complete(ctx, req)
	}
}

// selectToolCapableProvider finds the highest-scored healthy provider
// that supports tool calls. Falls back to any healthy provider when no
// tool-capable provider is found.
func (e *AgenticEnsemble) selectToolCapableProvider() llm.LLMProvider {
	if e.providerRegistry == nil {
		return nil
	}

	// Try providers ordered by score — prefer tool-capable.
	ordered := e.providerRegistry.ListProvidersOrderedByScore()

	var fallback llm.LLMProvider

	for _, name := range ordered {
		provider, err := e.providerRegistry.GetProvider(name)
		if err != nil {
			continue
		}

		caps := provider.GetCapabilities()
		if caps != nil && caps.SupportsTools {
			return provider
		}

		if fallback == nil {
			fallback = provider
		}
	}

	return fallback
}

// extractUserMessage retrieves the last user message from the request.
func (e *AgenticEnsemble) extractUserMessage(
	req *models.LLMRequest,
) string {
	if req == nil {
		return ""
	}

	// Walk messages in reverse to find the last user message.
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" && req.Messages[i].Content != "" {
			return req.Messages[i].Content
		}
	}

	return req.Prompt
}

// extractContent retrieves the selected response content from an
// EnsembleResult.
func (e *AgenticEnsemble) extractContent(
	result *EnsembleResult,
) string {
	if result == nil {
		return ""
	}
	if result.Selected != nil {
		return result.Selected.Content
	}
	if len(result.Responses) > 0 && result.Responses[0] != nil {
		return result.Responses[0].Content
	}
	return ""
}

// debateResultToEnsemble converts a DebateResult into an EnsembleResult.
func (e *AgenticEnsemble) debateResultToEnsemble(
	dr *DebateResult,
	mode AgenticMode,
) *EnsembleResult {
	if dr == nil {
		return &EnsembleResult{
			Metadata: make(map[string]any),
		}
	}

	// Tokens really consumed across every participant call in every round.
	// Without this the OpenAI-compatible envelope reported usage {0,0,0}
	// alongside a 200, which is itself a tell that no model ran
	// (convertToOpenAIChatResponse derives usage from Selected.TokenSplit()).
	debateTokens := totalDebateTokens(dr)

	var selectedResp *models.LLMResponse
	if dr.BestResponse != nil {
		selectedResp = &models.LLMResponse{
			ID:           dr.DebateID,
			Content:      dr.BestResponse.Content,
			ProviderName: dr.BestResponse.LLMProvider,
			Confidence:   dr.BestResponse.QualityScore,
			ResponseTime: dr.Duration.Milliseconds(),
			TokensUsed:   debateTokens,
		}
	} else if dr.Consensus != nil && dr.Consensus.FinalPosition != "" {
		selectedResp = &models.LLMResponse{
			ID:           dr.DebateID,
			Content:      dr.Consensus.FinalPosition,
			ProviderName: "consensus",
			Confidence:   dr.Consensus.Confidence,
			ResponseTime: dr.Duration.Milliseconds(),
			TokensUsed:   debateTokens,
		}
	}

	// Build participant-level responses.
	var responses []*models.LLMResponse
	for _, p := range dr.Participants {
		responses = append(responses, &models.LLMResponse{
			ID:           p.ParticipantID,
			Content:      p.Content,
			ProviderName: p.LLMProvider,
			Confidence:   p.QualityScore,
		})
	}

	return &EnsembleResult{
		Responses:    responses,
		Selected:     selectedResp,
		VotingMethod: "debate",
		Scores:       map[string]float64{"quality": dr.QualityScore},
		Metadata: map[string]any{
			"debate_id":    dr.DebateID,
			"session_id":   dr.SessionID,
			"total_rounds": dr.TotalRounds,
			"success":      dr.Success,
			"agentic": &AgenticMetadata{
				Mode:            mode.String(),
				StagesCompleted: []string{"debate"},
				TotalDurationMs: dr.Duration.Milliseconds(),
				ProvenanceID:    uuid.New().String(),
			},
		},
	}
}

// calculateConfidence derives a confidence score from the verification
// result.
func (e *AgenticEnsemble) calculateConfidence(
	v *AgenticVerificationResult,
) float64 {
	if v == nil {
		return 0.5
	}
	return v.Confidence
}

// buildToolSummary aggregates tool executions by protocol.
func (e *AgenticEnsemble) buildToolSummary(
	execs []AgenticToolExecution,
) []ToolInvocationSummary {
	if len(execs) == 0 {
		return nil
	}

	counts := make(map[string]int)
	for _, ex := range execs {
		counts[ex.Protocol]++
	}

	summaries := make([]ToolInvocationSummary, 0, len(counts))
	for protocol, count := range counts {
		summaries = append(summaries, ToolInvocationSummary{
			Protocol: protocol,
			Count:    count,
		})
	}
	return summaries
}
