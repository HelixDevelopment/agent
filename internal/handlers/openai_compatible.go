package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"dev.helix.agent/internal/catalog"
	"dev.helix.agent/internal/config"
	"dev.helix.agent/internal/llm"
	"dev.helix.agent/internal/models"
	"dev.helix.agent/internal/services"
	"dev.helix.agent/internal/services/debate_integration"
	"dev.helix.agent/internal/skills"
	"dev.helix.agent/internal/utils"
	"digital.vasic.debate/comprehensive"
	"digital.vasic.debate/orchestrator"
)

// Provider name + model alias constants. Centralized so tests, the
// router, and any new code path use the same identifier — no string
// literals scattered across the package per the user directive
// "all fixes MUST BE universal! No hardcoding allowed!"
//
// PrimaryProviderName is the registry key for the local-first provider
// that the helix-llm chain tries first. Changing it here is the only
// edit needed to switch the chain's primary.
const (
	PrimaryProviderName = "helixllm"

	ModelHelixLLM           = "helix-llm"
	ModelHelixDebate        = "helix-debate"
	ModelHelixAgentDebate   = "helixagent-debate"
	ModelHelixAgentLLM      = "helixagent-llm"
	ModelHelixAgentEnsemble = "helixagent-ensemble"
)

// UnifiedHandler provides 100% OpenAI-compatible API with automatic ensemble support
type UnifiedHandler struct {
	providerRegistry        *services.ProviderRegistry
	config                  *config.Config
	dialogueFormatter       *services.DialogueFormatter
	debateTeamConfig        *services.DebateTeamConfig
	skillsIntegration       *skills.Integration
	intentRouter            *services.IntentBasedRouter
	orchestratorIntegration *debate_integration.ServiceIntegration
	agenticEnsemble         *services.AgenticEnsemble
	debateService           *services.DebateService
	showDebateDialogue      bool
	reportGenerator         *services.VerificationReportGenerator
	// catalogService, when wired (router.go), is the built catalog the
	// /v1/models facade annotates availability from (HA-F2-001, FR-019).
	// Nil means "no serving evidence wired" — the facade then reports
	// "unreported" and withholds dispatch rather than claiming usability.
	catalogService *catalog.CatalogService
}

// NewUnifiedHandler creates a new unified handler
func NewUnifiedHandler(registry *services.ProviderRegistry, cfg *config.Config) *UnifiedHandler {
	// Initialize dialogue formatter for AI debate presentation
	dialogueFormatter := services.NewDialogueFormatter(services.StyleTheater)

	// Create a default debate team config (can be replaced via SetDebateTeamConfig)
	// This ensures debate dialogue generation works even before full initialization
	var debateTeamConfig *services.DebateTeamConfig
	if registry != nil {
		debateTeamConfig = services.NewDebateTeamConfig(registry, nil, nil)
	}

	return &UnifiedHandler{
		providerRegistry:   registry,
		config:             cfg,
		dialogueFormatter:  dialogueFormatter,
		debateTeamConfig:   debateTeamConfig,
		showDebateDialogue: true, // Enable debate dialogue: show team members, positions, phases, voting
	}
}

// SetDebateTeamConfig sets the debate team configuration and registers characters
// This should be called after the debate team has been properly initialized with discovery
func (h *UnifiedHandler) SetDebateTeamConfig(teamConfig *services.DebateTeamConfig) {
	h.debateTeamConfig = teamConfig

	// Register debate team members as dialogue characters
	if teamConfig != nil {
		members := teamConfig.GetAllLLMs()
		logrus.WithField("member_count", len(members)).Info("Registering debate team characters")
		for _, member := range members {
			if member != nil {
				char := h.dialogueFormatter.RegisterCharacter(member)
				logrus.WithFields(logrus.Fields{
					"position":  member.Position,
					"role":      member.Role,
					"provider":  member.ProviderName,
					"model":     member.ModelName,
					"char_name": char.Name,
				}).Info("Registered character for debate dialogue")
			}
		}
	}
}

// SetSkillsIntegration sets the Skills integration for the handler
func (h *UnifiedHandler) SetSkillsIntegration(integration *skills.Integration) {
	h.skillsIntegration = integration
	logrus.WithField("integration_set", integration != nil).Info("Skills integration set")
}

// SetCatalogService wires the built catalog so the /v1/models facade can
// annotate pseudo-model usability and the serving layer's own options from
// real serving evidence (HA-F2-001, FR-019). A nil or unwired catalog is
// the honest "unreported" state — the facade withholds dispatch claims.
func (h *UnifiedHandler) SetCatalogService(svc *catalog.CatalogService) {
	h.catalogService = svc
	logrus.WithField("catalog_wired", svc != nil).Info("Catalog service set on unified handler")
}

func (h *UnifiedHandler) SetIntentBasedRouter(router *services.IntentBasedRouter) {
	h.intentRouter = router
	logrus.WithField("router_set", router != nil).Info("IntentBasedRouter set")
}

// SetDebateService sets the debate service for conducting debates
func (h *UnifiedHandler) SetDebateService(debateService *services.DebateService) {
	h.debateService = debateService
	logrus.WithField("debate_service_set", debateService != nil).Info("DebateService set")
}

// SetOrchestratorIntegration sets the NEW debate orchestrator integration
// This enables the 8-phase debate protocol (Dehallucination → SelfEvolvement → Proposal → Critique → Review → Optimization → Adversarial → Convergence)
func (h *UnifiedHandler) SetOrchestratorIntegration(integration *debate_integration.ServiceIntegration) {
	h.orchestratorIntegration = integration
	if integration != nil {
		logrus.WithFields(logrus.Fields{
			"orchestrator_set": true,
			"agent_count":      integration.GetOrchestrator().GetAgentPool().Size(),
		}).Info("NEW Debate Orchestrator integration set - will use 8-phase protocol instead of old ensemble")
	}
}

// SetAgenticEnsemble sets the AgenticEnsemble as the primary request processing
// path. When set, processWithEnsemble routes through the dual-mode orchestrator
// (Reason + Execute) before falling back to the debate orchestrator or legacy
// ensemble.
func (h *UnifiedHandler) SetAgenticEnsemble(ae *services.AgenticEnsemble) {
	h.agenticEnsemble = ae
	logrus.WithField("agentic_ensemble_set", ae != nil).Info("AgenticEnsemble set")
}

// SetReportGenerator sets the verification report generator for generating provider reports
func (h *UnifiedHandler) SetReportGenerator(generator *services.VerificationReportGenerator) {
	h.reportGenerator = generator
	if generator != nil {
		logrus.WithField("report_path", generator.GetReportPath()).Info("Verification report generator set")
	}
}

// isToolResultProcessingTurn determines if the current request is specifically for processing
// tool results (continuation of tool execution) vs a new user request.
//
// CRITICAL: This function prevents infinite loops by distinguishing:
//   - Tool results from CURRENT turn → Process directly (return true)
//   - NEW user message → AI Debate (return false)
//
// The key insight: When the client sends back tool results, those results should
// be synthesized into a final response WITHOUT triggering a new debate cycle.
// Only genuinely NEW user messages should start a new debate.
func (h *UnifiedHandler) isToolResultProcessingTurn(messages []OpenAIMessage) bool {
	if len(messages) == 0 {
		return false
	}

	// Find the last non-system message to determine the conversation state
	var lastNonSystemIdx int = -1
	var lastUserIdx int = -1
	var lastToolIdx int = -1

	for i, msg := range messages {
		if msg.Role == "system" {
			continue
		}
		lastNonSystemIdx = i

		switch msg.Role {
		case "user":
			lastUserIdx = i
		case "tool":
			lastToolIdx = i
		}
	}

	// No non-system messages - not a tool result turn
	if lastNonSystemIdx == -1 {
		return false
	}

	// If the last message is from the user, this is a NEW user request → Use AI Debate
	if lastUserIdx == lastNonSystemIdx {
		logrus.WithFields(logrus.Fields{
			"last_user_idx":    lastUserIdx,
			"last_tool_idx":    lastToolIdx,
			"last_non_sys_idx": lastNonSystemIdx,
		}).Debug("New user request detected - will use AI Debate")
		return false
	}

	// If the last message is a TOOL result, this is tool processing → Direct synthesis
	// This PREVENTS infinite loops: debate generates tool_calls → tools execute →
	// results come back → synthesize response (NO new debate)
	if lastToolIdx == lastNonSystemIdx {
		logrus.WithFields(logrus.Fields{
			"last_tool_idx":    lastToolIdx,
			"last_non_sys_idx": lastNonSystemIdx,
		}).Debug("Tool result processing turn - will synthesize without new debate")
		return true // Process directly, don't start new debate
	}

	return false
}

// OpenAIModelsResponse represents OpenAI models API response
type OpenAIModelsResponse struct {
	Object string        `json:"object"`
	Data   []OpenAIModel `json:"data"`
}

// OpenAIModel represents a model in OpenAI API format
type OpenAIModel struct {
	ID         string                  `json:"id"`
	Object     string                  `json:"object"`
	Created    int64                   `json:"created"`
	OwnedBy    string                  `json:"owned_by"`
	Permission []OpenAIModelPermission `json:"permission"`
	Root       string                  `json:"root"`
	Parent     *string                 `json:"parent"`
	// Availability / WithheldReason (FR-019, HA-F2-001) annotate the entry
	// with the serving layer's reported state: "serving", "withheld"
	// (+ withheld_reason) or "unreported". A usable entry carries NO
	// availability field — the permission block is its claim — so the
	// field never overstates. omitempty keeps the annotation off the wire
	// when absent.
	Availability   string `json:"availability,omitempty"`
	WithheldReason string `json:"withheld_reason,omitempty"`
}

// OpenAIModelPermission represents model permissions in OpenAI format
type OpenAIModelPermission struct {
	ID                 string  `json:"id"`
	Object             string  `json:"object"`
	Created            int64   `json:"created"`
	AllowCreateEngine  bool    `json:"allow_create_engine"`
	AllowSampling      bool    `json:"allow_sampling"`
	AllowLogprobs      bool    `json:"allow_logprobs"`
	AllowSearchIndices bool    `json:"allow_search_indices"`
	AllowView          bool    `json:"allow_view"`
	AllowFineTuning    bool    `json:"allow_fine_tuning"`
	Organization       string  `json:"organization"`
	Group              *string `json:"group"`
	IsBlocking         bool    `json:"is_blocking"`
}

// OpenAITool represents a tool definition in OpenAI format
// CRITICAL: This enables AI coding assistants (OpenCode, Claude Code, Qwen Code) to access codebases
type OpenAITool struct {
	Type     string             `json:"type"` // "function"
	Function OpenAIToolFunction `json:"function"`
}

// OpenAIToolFunction represents a function definition within a tool
type OpenAIToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
	Strict      *bool                  `json:"strict,omitempty"`
}

// OpenAIChatRequest represents OpenAI chat completion request
// IMPORTANT: Supports full OpenAI tool calling for AI coding assistants (OpenCode, Claude Code, Qwen Code)
type OpenAIChatRequest struct {
	Model            string             `json:"model"`
	Messages         []OpenAIMessage    `json:"messages"`
	MaxTokens        int                `json:"max_tokens,omitempty"`
	Temperature      float64            `json:"temperature,omitempty"`
	TopP             float64            `json:"top_p,omitempty"`
	Stream           bool               `json:"stream,omitempty"`
	Stop             []string           `json:"stop,omitempty"`
	PresencePenalty  float64            `json:"presence_penalty,omitempty"`
	FrequencyPenalty float64            `json:"frequency_penalty,omitempty"`
	LogitBias        map[string]float64 `json:"logit_bias,omitempty"`
	User             string             `json:"user,omitempty"`
	// OpenAI Tool Calling Support - CRITICAL for AI coding assistants to access codebase
	Tools      []OpenAITool `json:"tools,omitempty"`
	ToolChoice interface{}  `json:"tool_choice,omitempty"` // Can be "none", "auto", "required", or {"type":"function","function":{"name":"..."}}
	// Parallel tool calls support
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
	// HelixAgent extensions
	EnsembleConfig *models.EnsembleConfig `json:"ensemble_config,omitempty"`
	ForceProvider  string                 `json:"force_provider,omitempty"`
}

// OpenAIMessage represents a message in OpenAI chat format
type OpenAIMessage struct {
	Role         string              `json:"role"`
	Content      string              `json:"content"`
	Name         *string             `json:"name,omitempty"`
	FunctionCall *OpenAIFunctionCall `json:"function_call,omitempty"`
	ToolCalls    []OpenAIToolCall    `json:"tool_calls,omitempty"`
	ToolCallID   string              `json:"tool_call_id,omitempty"`
}

// OpenAIFunctionCall represents function call in OpenAI format
type OpenAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// OpenAIToolCall represents tool call in OpenAI format
type OpenAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function OpenAIFunctionCall `json:"function"`
}

// StreamingToolCall represents a tool call for streaming responses with Index
// CRITICAL: This enables actual tool execution by AI coding assistants like OpenCode
type StreamingToolCall struct {
	Index    int                `json:"index"`
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function OpenAIFunctionCall `json:"function"`
}

// DebatePositionResponse is defined in debate_visualization.go with enhanced fields:
// - Content, ToolCalls, Position (same as before)
// - ResponseTime, PrimaryProvider, PrimaryModel (timing tracking)
// - ActualProvider, ActualModel, UsedFallback (fallback tracking)
// - FallbackChain (full fallback attempt history)
// - Timestamp

// OpenAIChatResponse represents OpenAI chat completion response
type OpenAIChatResponse struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"`
	Created           int64          `json:"created"`
	Model             string         `json:"model"`
	Choices           []OpenAIChoice `json:"choices"`
	Usage             *OpenAIUsage   `json:"usage,omitempty"`
	SystemFingerprint string         `json:"system_fingerprint,omitempty"`
	// Agentic carries the agentic-ensemble provenance metadata
	// (mode, stages_completed, agents_spawned, per-subagent results)
	// when the request was processed by the dual-mode decompose→execute
	// engine. Omitted for non-agentic responses so plain chat is
	// unaffected.
	Agentic any `json:"agentic,omitempty"`
}

// OpenAIChoice represents a choice in OpenAI response
type OpenAIChoice struct {
	Index        int             `json:"index"`
	Message      OpenAIMessage   `json:"message"`
	FinishReason string          `json:"finish_reason"`
	Logprobs     *OpenAILogprobs `json:"logprobs,omitempty"`
}

// OpenAIUsage represents token usage in OpenAI format
type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAILogprobs represents log probabilities in OpenAI format
type OpenAILogprobs struct {
	TokenOffset map[string]float64 `json:"token_offset,omitempty"`
	TopLogprobs []OpenAILogprob    `json:"top_logprobs,omitempty"`
	TextOffset  int                `json:"text_offset,omitempty"`
}

// OpenAILogprob represents a log probability entry
type OpenAILogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
	Bytes   []byte  `json:"bytes,omitempty"`
	Offset  int     `json:"offset,omitempty"`
}

// RegisterOpenAIRoutes registers OpenAI-compatible routes
func (h *UnifiedHandler) RegisterOpenAIRoutes(r *gin.RouterGroup, auth gin.HandlerFunc) {
	// Apply auth middleware to protected routes
	protected := r.Group("").Use(auth)

	{
		// Chat completions - main endpoint for AI agents like OpenCode/Crush
		protected.POST("/chat/completions", h.ChatCompletions)
		protected.POST("/chat/completions/stream", h.ChatCompletionsStream)

		// Completions - legacy text completions
		protected.POST("/completions", h.Completions)
		protected.POST("/completions/stream", h.CompletionsStream)

		// Models endpoint - exposes all available models from all providers
		protected.GET("/models", h.Models)
	}
}

// validChatMessageRoles are the only role values accepted on incoming
// chat completion requests.
var validChatMessageRoles = map[string]bool{
	"system":    true,
	"user":      true,
	"assistant": true,
	"tool":      true,
	"function":  true, // legacy OpenAI function-calling, still accepted by some providers
	"developer": true, // OpenAI o1/o3 family
}

// validateChatRequest enforces structural rules that EVERY upstream
// provider in our chain enforces. Catching them here lets us return a
// specific, actionable 400 instead of a vague 502 after every provider
// has independently rejected the request.
//
// Checks (in order):
//   - messages array is present and non-empty
//   - each message has a role from the known set
//   - each message has either content OR (assistant-only) tool_calls
//   - role="tool" carries tool_call_id matched against a prior assistant
//     tool_call (orphaned-result detection)
//   - max_tokens >= 0 if present (negative rejected)
//   - temperature in [0, 2] if present
//   - top_p in [0, 1] if present
//   - presence_penalty / frequency_penalty in [-2, 2] if present
func validateChatRequest(req *OpenAIChatRequest) error {
	if req == nil {
		return nil
	}

	// 1. Messages array must be present and non-empty.
	if len(req.Messages) == 0 {
		return fmt.Errorf("messages: required, must be a non-empty array")
	}

	// 2. Numeric parameter ranges (omitempty zeroes collapse to "not set"
	// for these, so the lower bound 0 is the inclusive default).
	if req.MaxTokens < 0 {
		return fmt.Errorf("max_tokens: must be >= 0 (got %d)", req.MaxTokens)
	}
	if req.Temperature < 0 || req.Temperature > 2 {
		return fmt.Errorf("temperature: must be in [0, 2] (got %g)", req.Temperature)
	}
	if req.TopP < 0 || req.TopP > 1 {
		return fmt.Errorf("top_p: must be in [0, 1] (got %g)", req.TopP)
	}
	if req.PresencePenalty < -2 || req.PresencePenalty > 2 {
		return fmt.Errorf("presence_penalty: must be in [-2, 2] (got %g)", req.PresencePenalty)
	}
	if req.FrequencyPenalty < -2 || req.FrequencyPenalty > 2 {
		return fmt.Errorf("frequency_penalty: must be in [-2, 2] (got %g)", req.FrequencyPenalty)
	}

	// 3. Per-message validation + collect known tool_call IDs.
	knownIDs := make(map[string]bool)
	for i, msg := range req.Messages {
		if msg.Role == "" {
			return fmt.Errorf("messages[%d]: role is required", i)
		}
		if !validChatMessageRoles[msg.Role] {
			return fmt.Errorf(
				"messages[%d]: role %q is not one of system|user|assistant|tool|function|developer",
				i, msg.Role)
		}
		hasContent := msg.Content != ""
		hasToolCalls := len(msg.ToolCalls) > 0
		switch msg.Role {
		case "assistant":
			if !hasContent && !hasToolCalls {
				return fmt.Errorf("messages[%d]: assistant message must have content or tool_calls", i)
			}
			for _, tc := range msg.ToolCalls {
				if tc.ID != "" {
					knownIDs[tc.ID] = true
				}
			}
		case "tool":
			if !hasContent {
				return fmt.Errorf("messages[%d]: tool message must have content", i)
			}
		default:
			if !hasContent {
				return fmt.Errorf("messages[%d]: %s message must have content", i, msg.Role)
			}
		}
	}

	// 4. Tool-message specific: tool_call_id must reference a prior
	// assistant tool_calls[].id (orphaned-result detection).
	for i, msg := range req.Messages {
		if msg.Role != "tool" {
			continue
		}
		if msg.ToolCallID == "" {
			return fmt.Errorf(
				"messages[%d]: role=\"tool\" message is missing required tool_call_id field "+
					"(must reference a prior assistant tool_calls[].id)", i)
		}
		if len(knownIDs) > 0 && !knownIDs[msg.ToolCallID] {
			return fmt.Errorf(
				"messages[%d]: tool_call_id %q does not match any prior assistant tool_calls[].id "+
					"in this conversation (orphaned tool result)", i, msg.ToolCallID)
		}
	}
	return nil
}

// ChatCompletions handles OpenAI chat completions with automatic ensemble
// Supports both streaming and non-streaming modes based on the "stream" parameter
func (h *UnifiedHandler) ChatCompletions(c *gin.Context) {
	logrus.Info("[ENTRY] ChatCompletions handler called")

	// Parse OpenAI request
	var req OpenAIChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.sendOpenAIError(c, http.StatusBadRequest, "invalid_request", "Invalid request format", err.Error())
		return
	}

	// Pre-flight validation: catch malformed tool messages BEFORE
	// passing to the provider chain. Without this, requests with
	// missing tool_call_id propagate to every provider, all 4xx-reject
	// them, and the client sees a generic 503 "all providers failed"
	// — masking the real issue (CONST-032 reproduction:
	// challenges/scripts/opencode_tool_result_followup_challenge.sh
	// Test 3).
	if err := validateChatRequest(&req); err != nil {
		h.sendOpenAIError(c, http.StatusBadRequest, "invalid_request",
			err.Error(), "request validation failed before provider dispatch")
		return
	}

	// If streaming is requested, handle with SSE streaming
	if req.Stream {
		h.handleStreamingChatCompletions(c, &req)
		return
	}

	// Check model routing.
	// Canonical names per docs/api/API_REFERENCE.md and /v1/models response:
	//   - helixagent-debate (canonical for CLI configs)
	//   - helixagent-ensemble (semantic alias — same dispatch as -debate)
	//   - helixagent/helixagent-debate (provider-qualified form)
	//   - helix-debate, helixagent/helix-debate (legacy aliases)
	//   → AI Debate ensemble
	//   - helixagent-llm, helixagent/helixagent-llm (canonical)
	//   - helix-llm, helixagent/helix-llm (legacy aliases)
	//   → provider chain with HelixLLM-first fallback
	// Anything else → AI Debate ensemble (graceful default).
	modelToCheck := req.Model
	switch modelToCheck {
	case "",
		"helixagent-debate", "helixagent/helixagent-debate",
		"helixagent-ensemble", "helixagent/helixagent-ensemble",
		"helix-debate", "helixagent/helix-debate":
		logrus.Info("Model: helixagent-debate or default - routing to AI Debate ensemble")
	case "helixagent-llm", "helixagent/helixagent-llm",
		"helix-llm", "helixagent/helix-llm":
		logrus.Info("Model: helixagent-llm - routing to provider chain with fallback")
		h.processWithProviderChain(c, &req)
		return
	default:
		// Verify the model is supported by at least ONE registered provider
		// before falling through to the ensemble. Bogus / typo'd model
		// names that no provider recognises return HTTP 404 instead of
		// silently routing to the ensemble (which would produce a
		// completion that wasn't actually for the requested model).
		//
		// Fail-OPEN during cold-start / unreachable-discovery: when no
		// provider has populated SupportedModels yet (registry has zero
		// model data), we cannot honestly say "unknown" — we let the
		// request through and rely on the ensemble's own provider-level
		// handling to surface a real error if the model is bogus.
		if h.providerRegistry != nil {
			found, authoritative := h.providerRegistry.LookupModel(modelToCheck)
			switch {
			case !authoritative:
				logrus.Warnf("Model '%s' check skipped — provider registry has no model data yet (cold-start), allowing request through", modelToCheck)
			case !found:
				logrus.Warnf("Model '%s' not in canonical aliases and unknown to all registered providers — returning 404", modelToCheck)
				h.sendOpenAIError(c, http.StatusNotFound, "model_not_found",
					fmt.Sprintf("model '%s' is not supported by any registered provider", modelToCheck),
					"see GET /v1/models for the canonical model list, or use helixagent-debate / helixagent-llm")
				return
			default:
				logrus.Infof("Model '%s' recognised by at least one provider - using AI Debate ensemble", modelToCheck)
			}
		}
	}

	// Check if this is a tool result processing turn vs a new user request
	// CRITICAL: Tool results must be processed directly to prevent infinite loops
	// Only NEW user messages should trigger a fresh AI Debate cycle
	isToolResultTurn := h.isToolResultProcessingTurn(req.Messages)

	if isToolResultTurn {
		// Process tool results directly - synthesize into final response
		// This prevents: debate → tool_calls → results → debate → tool_calls... (infinite loop)
		logrus.Info("Processing tool results - synthesizing without new debate")
		toolResultResponse, toolResultLLM, err := h.processToolResultsWithLLM(c.Request.Context(), &req)
		if err != nil {
			logrus.WithError(err).Warn("Failed to process tool results with LLM")
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Failed to process tool results",
					"type":    "server_error",
					"code":    "TOOL_RESULT_PROCESSING_FAILED",
				},
			})
			return
		}

		toolResultUsage := usageFromResponse(toolResultLLM)

		// Return as OpenAI-compatible response
		response := OpenAIChatResponse{
			ID:                utils.SecureRandomID("chatcmpl-tool"), // D-20: crypto-random, collision-free
			Object:            "chat.completion",
			Created:           time.Now().Unix(),
			Model:             "helixagent-ensemble",
			SystemFingerprint: "fp_helixagent_v1",
			Choices: []OpenAIChoice{
				{
					Index: 0,
					Message: OpenAIMessage{
						Role:    "assistant",
						Content: toolResultResponse,
					},
					FinishReason: "stop",
				},
			},
			Usage: toolResultUsage,
		}
		c.JSON(http.StatusOK, response)
		return
	}

	// Multi-turn detection (drainage iter-2 / Finding #19): the debate
	// orchestrator's DebateConfig only carries a single Topic string —
	// it does NOT carry the full conversation history. So when the
	// client sends a multi-turn request (multiple user/assistant
	// alternations), the debate ensemble loses all prior context and
	// answers from a fresh state ("This conversation just started, I
	// don't have any information about you"). Until DebateConfig is
	// extended to carry message history through executeRound and the
	// per-participant prompt construction, multi-turn requests must
	// route to processWithProviderChain (a single provider that
	// natively preserves conversation context).
	multiTurnUserCount := 0
	for _, msg := range req.Messages {
		if msg.Role == "user" {
			multiTurnUserCount++
		}
	}
	if multiTurnUserCount > 1 {
		logrus.WithField("user_message_count", multiTurnUserCount).Info("Multi-turn request - routing to provider chain (debate doesn't carry conversation history; see drainage Finding #19)")
		h.processWithProviderChain(c, &req)
		return
	}

	// NEW user request → Full AI Debate ensemble
	logrus.Info("New user request - initiating AI Debate")

	// Generate request ID for skills tracking
	requestID := utils.SecureRandomID("openai") // D-20: crypto-random, collision-free

	// Process skills for this request
	var skillsCtx *skills.RequestContext
	var enhancedMessages []OpenAIMessage
	if h.skillsIntegration != nil {
		skillsCtx, enhancedMessages = h.processSkillsForRequest(c.Request.Context(), &req, requestID)
		// If skills were matched, use enhanced messages
		if skillsCtx != nil && len(skillsCtx.SkillsToApply) > 0 {
			// Create a copy of the request with enhanced messages
			enhancedReq := req
			enhancedReq.Messages = enhancedMessages
			// Use enhanced request for processing
			req = enhancedReq
			// Extract skill names for logging
			skillNames := make([]string, 0, len(skillsCtx.SkillsToApply))
			for _, skill := range skillsCtx.SkillsToApply {
				skillNames = append(skillNames, skill.Name)
			}
			logrus.WithFields(logrus.Fields{
				"request_id":   requestID,
				"skills_count": len(skillsCtx.SkillsToApply),
				"skills_names": skillNames,
			}).Info("Skills matched and applied to request")
		}
	}

	// Convert to internal request format
	internalReq := h.convertOpenAIChatRequest(&req, c)

	// Store skills context in provider-specific data for downstream use
	if skillsCtx != nil && len(skillsCtx.SkillsToApply) > 0 {
		if internalReq.ModelParams.ProviderSpecific == nil {
			internalReq.ModelParams.ProviderSpecific = make(map[string]any)
		}
		internalReq.ModelParams.ProviderSpecific["skills_context"] = skillsCtx
		internalReq.ModelParams.ProviderSpecific["skills_request_id"] = requestID
	}

	// Process with ensemble for best results
	result, err := h.processWithEnsemble(c.Request.Context(), internalReq, &req)
	if err != nil {
		// Complete skill execution with failure
		if skillsCtx != nil && len(skillsCtx.ActiveUsages) > 0 {
			h.skillsIntegration.CompleteRequest(requestID, false, err.Error())
		}
		h.sendCategorizedError(c, err)
		return
	}

	// Convert to OpenAI response format
	response := h.convertToOpenAIChatResponse(result, &req)

	// Add skills used metadata to response if skills were applied
	if skillsCtx != nil && len(skillsCtx.ActiveUsages) > 0 {
		// Complete skill executions with success
		completedUsages := h.skillsIntegration.CompleteRequest(requestID, true, "")
		if len(completedUsages) > 0 {
			// Build skills used section and add to response
			skillsMetadata := h.skillsIntegration.BuildSkillsUsedSection(completedUsages)
			if skillsMetadata != nil {
				// Convert response to map, add skills_used, then convert back
				// Since response is a struct, we need to add a field.
				// We'll convert to map via JSON marshal/unmarshal or add a field to the struct.
				// For now, we'll add a top-level field to the JSON response by using gin.H
				// We'll create a new response map with skills_used field.
				responseMap := make(map[string]any)
				// Convert response to map via JSON (simplest)
				jsonBytes, err := json.Marshal(response)
				if err != nil {
					// Can't marshal, just send original response without skills_used
					c.JSON(http.StatusOK, response)
					return
				}
				if err := json.Unmarshal(jsonBytes, &responseMap); err != nil {
					c.JSON(http.StatusOK, response)
					return
				}
				responseMap["skills_used"] = skillsMetadata
				c.JSON(http.StatusOK, responseMap)
				return
			}
		}
	}

	c.JSON(http.StatusOK, response)
}

// handleStreamingChatCompletions handles streaming chat completions with SSE
func (h *UnifiedHandler) handleStreamingChatCompletions(c *gin.Context, req *OpenAIChatRequest) {
	// Check model routing for streaming. Match the canonical set used
	// in ChatCompletions(): helixagent-llm (and legacy helix-llm) →
	// provider chain; default / helixagent-debate → ensemble stream.
	switch req.Model {
	case "helixagent-llm", "helixagent/helixagent-llm",
		"helix-llm", "helixagent/helix-llm":
		logrus.Info("Streaming: helixagent-llm - using provider chain with fallback")
		h.streamWithProviderChain(c, req)
		return
	}

	// Convert to internal request format
	internalReq := h.convertOpenAIChatRequest(req, c)

	// Create a cancellable context with timeout for the stream
	// Maximum 420 seconds (7 min) to allow for multiple provider fallbacks during tool result processing
	// With 6 providers at 60 seconds each, we need at least 360 seconds plus buffer
	ctx, cancel := context.WithTimeout(c.Request.Context(), 420*time.Second)

	// Detect output format based on client hints (User-Agent, Accept header, explicit format hint)
	// This ensures API clients (OpenCode, Crush, etc.) get clean Markdown without ANSI escape codes
	userAgent := c.GetHeader("User-Agent")
	acceptHeader := c.GetHeader("Accept")
	formatHint := c.GetHeader("X-Output-Format") // Allow explicit format override
	outputFormat := DetectOutputFormat(acceptHeader, userAgent, formatHint)
	defer cancel()

	// Generate stream ID early for potential orchestrator use
	streamID := utils.SecureRandomID("chatcmpl") // D-20: crypto-random, collision-free; one ID per stream, reused across chunks

	// 8-phase orchestrator DISABLED for streaming — it consistently times out after 5 minutes,
	// wastes resources, and then the fallback debate fires a DUPLICATE debate request.
	// The debate service with ConductDebate already makes real LLM calls through verified
	// providers and works correctly. The orchestrator will be re-enabled once optimized for
	// streaming latency requirements.
	// See: original orchestrator code used h.orchestratorIntegration.GetOrchestrator().ConductDebate()

	// INTENT DETECTION: Use LLM-based semantic classification to determine routing.
	// NO HARDCODED PATTERNS — supports any language, slang, or expression.
	// Only trivial social messages (greetings, confirmations) bypass the debate.
	// ALL substantive messages go through the 8-role debate ensemble for full transparency.
	lastUserMsg := ""
	for _, msg := range req.Messages {
		if msg.Role == "user" {
			lastUserMsg = msg.Content
		}
	}

	useDebate := true // Default: always debate for full transparency

	// Multi-turn detection (drainage iter-2 / Finding #19): the debate
	// orchestrator's DebateConfig only carries a single Topic string —
	// it does NOT carry the full conversation history. So when the
	// client sends a multi-turn request (multiple user/assistant
	// alternations), the debate ensemble loses all prior context and
	// answers from a fresh state ("This conversation just started, I
	// don't have any information about you"). Until DebateConfig is
	// extended to carry message history through executeRound and the
	// per-participant prompt construction, multi-turn requests must
	// route to a single provider that natively preserves conversation
	// context. This precedes the explicit-debate-model override below
	// because correctness > model-name contract for this case.
	userMessageCount := 0
	for _, msg := range req.Messages {
		if msg.Role == "user" {
			userMessageCount++
		}
	}
	multiTurn := userMessageCount > 1

	// Explicit-model override: when the client requests a debate-specific
	// model name (`helixagent-debate` or `helixagent-ensemble`), respect
	// that and force the ensemble path even for short / non-actionable
	// prompts. The model name IS the contract — a request asking for the
	// debate model must get the debate, otherwise the model name lies.
	// Drainage report 2026-04-25 Finding #9 root cause: the intent
	// classifier overrode `model=helixagent-debate` for trivial prompts,
	// producing single-provider responses that the consensus tests then
	// (correctly) flagged as malformed. Without this override the test
	// nondeterminism tracks classifier availability instead of the
	// product's documented behavior.
	requestedDebateExplicitly := req.Model == "helixagent-debate" || req.Model == "helixagent-ensemble"

	if multiTurn {
		useDebate = false
		logrus.WithFields(logrus.Fields{
			"user_message_count": userMessageCount,
			"requested_model":    req.Model,
		}).Info("[STREAMING] Multi-turn request — routing to single provider (debate doesn't carry conversation history; see drainage Finding #19)")
	} else if requestedDebateExplicitly {
		logrus.WithField("model", req.Model).Info("[STREAMING] Explicit debate-model request — bypassing intent classifier")
	} else if h.intentRouter != nil && h.intentRouter.GetLLMClassifier() != nil {
		// Use LLM intelligence to classify intent — works in any language
		classifyCtx, classifyCancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
		llmResult, llmErr := h.intentRouter.GetLLMClassifier().ClassifyIntentWithLLM(classifyCtx, lastUserMsg, "")
		classifyCancel()
		if llmErr == nil && llmResult != nil {
			// Only skip debate for trivial social messages (greetings, confirmations, refusals)
			isTrivial := llmResult.Intent == "confirmation" || llmResult.Intent == "refusal" || llmResult.Intent == "greeting"
			// Very short non-actionable messages (< 15 chars) are likely trivial
			if !isTrivial && len(strings.TrimSpace(lastUserMsg)) < 15 && !llmResult.IsActionable && llmResult.Confidence >= 0.8 {
				isTrivial = true
			}
			useDebate = !isTrivial
			logrus.WithFields(logrus.Fields{
				"use_debate": useDebate,
				"intent":     llmResult.Intent,
				"actionable": llmResult.IsActionable,
				"confidence": llmResult.Confidence,
				"message":    lastUserMsg[:min(50, len(lastUserMsg))],
			}).Info("[STREAMING] LLM-based routing decision")
		} else {
			logrus.WithError(llmErr).Warn("[STREAMING] LLM classification failed — defaulting to debate")
		}
	}
	logrus.WithFields(logrus.Fields{
		"use_debate":          useDebate,
		"explicit_debate_req": requestedDebateExplicitly,
		"requested_model":     req.Model,
		"message":             lastUserMsg[:min(50, len(lastUserMsg))],
	}).Info("[STREAMING] Final routing decision")

	// For SIMPLE messages: use strongest single provider for fast, direct response
	// CRITICAL: For greetings only, trim oversized system prompts from CLI agents
	// (OpenCode sends 16K+ token system context) to prevent code generation for "hello!"
	// For other simple queries (e.g. "do you see my codebase?"), keep the original system
	// context so the LLM knows about tool access and capabilities.
	if !useDebate && h.debateTeamConfig != nil {
		logrus.Info("[STREAMING] Simple message — using strongest single provider")

		// Detect if this is a pure greeting (hello, hi, hey, etc.)
		isGreeting := false
		msgLower := strings.ToLower(strings.TrimSpace(lastUserMsg))
		greetings := []string{"hello", "hi", "hey", "greetings", "good morning", "good afternoon",
			"good evening", "howdy", "yo", "sup", "thanks", "thank you", "thx", "bye", "goodbye"}
		for _, g := range greetings {
			if msgLower == g || msgLower == g+"!" || msgLower == g+"." {
				isGreeting = true
				break
			}
		}

		simpleReq := *internalReq
		if isGreeting && len(simpleReq.Messages) > 0 {
			// For greetings: use minimal system prompt to prevent code generation
			var trimmedMessages []models.Message
			trimmedMessages = append(trimmedMessages, models.Message{
				Role:    "system",
				Content: "You are HelixAgent, a helpful AI assistant powered by an AI Debate Ensemble. Respond naturally and conversationally to the user. You have access to the user's codebase through tools.",
			})
			for _, m := range simpleReq.Messages {
				if m.Role == "user" || m.Role == "assistant" {
					trimmedMessages = append(trimmedMessages, m)
				}
			}
			simpleReq.Messages = trimmedMessages
			logrus.WithField("greeting", msgLower).Info("[STREAMING] Trimmed system context for greeting")
		} else if len(simpleReq.Messages) > 0 {
			// For non-greeting simple queries: compress oversized system context
			// while preserving tool awareness dynamically from the original prompt.
			// OpenCode sends 70KB+ system context; most providers cap at 6-8K context.
			totalLen := 0
			for _, m := range simpleReq.Messages {
				totalLen += len(m.Content)
			}
			if totalLen > 6000 {
				var compressedMessages []models.Message
				// Extract tool names and key context from original system messages
				var toolMentions []string
				var clientName string
				for _, m := range simpleReq.Messages {
					if m.Role != "system" {
						continue
					}
					lower := strings.ToLower(m.Content)
					// Detect client name dynamically
					for _, name := range []string{"Claude Code", "OpenCode", "Qwen Code", "Crush", "KiloCode", "Junie", "Gemini"} {
						if strings.Contains(m.Content, name) {
							clientName = name
							break
						}
					}
					// Extract tool names dynamically from system prompt
					toolPatterns := []string{
						"read_file", "write_file", "edit_file", "search_files", "list_directory",
						"execute_command", "run_command", "grep", "glob", "bash",
						"Read", "Write", "Edit", "Grep", "Glob", "Bash", "LS",
					}
					for _, tp := range toolPatterns {
						if strings.Contains(m.Content, tp) || strings.Contains(lower, strings.ToLower(tp)) {
							toolMentions = append(toolMentions, tp)
						}
					}
				}
				// Deduplicate tool mentions
				seen := make(map[string]bool)
				var uniqueTools []string
				for _, t := range toolMentions {
					tl := strings.ToLower(t)
					if !seen[tl] {
						seen[tl] = true
						uniqueTools = append(uniqueTools, t)
					}
				}
				if clientName == "" {
					clientName = "AI coding assistant"
				}
				toolList := "various file and code tools"
				if len(uniqueTools) > 0 {
					toolList = strings.Join(uniqueTools, ", ")
				}

				compressedSystem := fmt.Sprintf(`You are HelixAgent, an AI coding assistant powered by an AI Debate Ensemble.

IMPORTANT CONTEXT:
- You are integrated with %s
- You have FULL ACCESS to the user's codebase through tools
- Available tools: %s
- When the user asks about their code, you CAN see and access their files
- NEVER say "I cannot see your codebase" — the tools handle file access
- Provide SPECIFIC, ACTIONABLE coding advice

Respond concisely and helpfully.`, clientName, toolList)

				compressedMessages = append(compressedMessages, models.Message{
					Role:    "system",
					Content: compressedSystem,
				})
				// Keep all user/assistant messages
				for _, m := range simpleReq.Messages {
					if m.Role == "user" || m.Role == "assistant" {
						compressedMessages = append(compressedMessages, m)
					}
				}
				simpleReq.Messages = compressedMessages
				logrus.WithFields(logrus.Fields{
					"original_len":    totalLen,
					"compressed_msgs": len(compressedMessages),
					"client":          clientName,
					"tools_found":     len(uniqueTools),
				}).Info("[STREAMING] Compressed oversized system context")
			}
		}

		for pos := services.PositionAnalyst; pos <= services.PositionMediator; pos++ {
			provider, model, err := h.debateTeamConfig.GetProviderForPosition(pos)
			if err != nil || provider == nil {
				continue
			}
			logrus.WithField("model", model).Info("[STREAMING] Direct provider response")
			if model != "" {
				simpleReq.ModelParams.Model = model
			}
			streamChan, streamErr := provider.CompleteStream(ctx, &simpleReq)
			if streamErr == nil && streamChan != nil {
				c.Header("Content-Type", "text/event-stream")
				c.Header("Cache-Control", "no-cache")
				c.Header("Connection", "keep-alive")
				c.Header("X-Accel-Buffering", "no")
				flusher, ok := c.Writer.(http.Flusher)
				if !ok {
					h.sendOpenAIError(c, http.StatusInternalServerError, "internal_error", "Streaming not supported", "")
					return
				}
				roleData, _ := json.Marshal(map[string]any{ //nolint:errcheck
					"id": streamID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
					"model": "helixagent-debate", "system_fingerprint": "fp_helixagent_v1",
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}},
				})
				_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", roleData)
				flusher.Flush()

				for resp := range streamChan {
					if resp == nil || (resp.Content == "" && resp.FinishReason == "") {
						continue
					}
					if resp.FinishReason == "stop" || resp.FinishReason == "STOP" {
						break
					}
					chunkData, _ := json.Marshal(map[string]any{ //nolint:errcheck
						"id": streamID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
						"model": "helixagent-debate", "system_fingerprint": "fp_helixagent_v1",
						"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": resp.Content}, "finish_reason": nil}},
					})
					_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", chunkData)
					flusher.Flush()
				}

				finishData, _ := json.Marshal(map[string]any{ //nolint:errcheck
					"id": streamID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
					"model": "helixagent-debate", "system_fingerprint": "fp_helixagent_v1",
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				})
				_, _ = fmt.Fprintf(c.Writer, "data: %s\n\ndata: [DONE]\n\n", finishData)
				flusher.Flush()
				return
			}
			logrus.WithError(streamErr).Warn("[STREAMING] Single provider failed, trying next")
		}
		logrus.Warn("[STREAMING] All single providers failed, falling through to debate")
	}

	// For COMPLEX messages: route through DebateService which provides:
	// - Multi-round consensus debate (each LLM sees all previous responses)
	// - Rounds continue until consensus (no member changes answer) or max rounds
	// - HelixMemory integration for persistent context across sessions
	// - HelixSpecifier auto-activation for large tasks (features, components)
	// - 3-tier routing: simple → direct, medium → debate consensus, large → SpecKit planning
	// - Tool execution after consensus
	if h.debateService != nil && h.debateTeamConfig != nil && ctx.Err() == nil {
		logrus.Info("[STREAMING] Complex message — routing through DebateService for multi-round consensus")

		topic := lastUserMsg
		if topic == "" {
			topic = "User Query"
		}

		debateID := utils.SecureRandomID("debate") // D-20: crypto-random, collision-free
		startTime := time.Now()

		// Set up SSE streaming
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		flusher, flusherOK := c.Writer.(http.Flusher)
		if !flusherOK {
			h.sendOpenAIError(c, http.StatusInternalServerError, "internal_error", "Streaming not supported", "")
			return
		}
		sseHeadersSent := false
		sendSSEChunk := func(content string) {
			if !sseHeadersSent {
				roleData, _ := json.Marshal(map[string]any{
					"id": streamID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
					"model": "helixagent-debate", "system_fingerprint": "fp_helixagent_v1",
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}},
				})
				_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", roleData)
				flusher.Flush()
				sseHeadersSent = true
			}
			chunkData, _ := json.Marshal(map[string]any{
				"id": streamID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
				"model": "helixagent-debate", "system_fingerprint": "fp_helixagent_v1",
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": content}, "finish_reason": nil}},
			})
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", chunkData)
			flusher.Flush()
		}

		// Stream team introduction
		teamIntro := h.generateDebateTeamIntroduction()
		if teamIntro != "" {
			for _, line := range strings.Split(teamIntro, "\n") {
				sendSSEChunk(line + "\n")
			}
			sendSSEChunk("\n---\n\n")
		}

		// Build DebateConfig for DebateService
		// DebateService handles: multi-round consensus, HelixMemory, HelixSpecifier routing
		verifiedLLMs := h.debateTeamConfig.GetVerifiedLLMs()
		debateParticipants := make([]services.ParticipantConfig, 0, len(verifiedLLMs))
		roles := []string{"Architect", "Generator", "Critic", "Tester", "Security", "Performance", "Validator"}
		for i, vllm := range verifiedLLMs {
			role := "Analyst"
			if i < len(roles) {
				role = roles[i]
			}
			debateParticipants = append(debateParticipants, services.ParticipantConfig{
				ParticipantID: fmt.Sprintf("p%d", i+1),
				Name:          role,
				Role:          strings.ToLower(role),
				LLMProvider:   vllm.ProviderName,
				LLMModel:      vllm.ModelName,
				MaxRounds:     2,
				Timeout:       45 * time.Second,
				Weight:        vllm.Score,
			})
		}

		debateConfig := &services.DebateConfig{
			DebateID:     debateID,
			Topic:        topic,
			Participants: debateParticipants,
			MaxRounds:    2,
			Timeout:      300 * time.Second,
			Strategy:     "confidence_weighted",
			Metadata:     map[string]any{"stream_id": streamID, "source": "openai_compatible"},
		}

		// Pass system context so debate participants know they are HelixAgent assistants
		var systemContext strings.Builder
		for _, m := range internalReq.Messages {
			if m.Role == "system" {
				// Truncate very long system messages (tool schemas) to avoid token limits
				content := m.Content
				if len(content) > 2000 {
					content = content[:2000] + "\n...[truncated]"
				}
				systemContext.WriteString(content + "\n")
			}
		}
		if systemContext.Len() > 0 {
			debateConfig.Metadata["system_context"] = systemContext.String()
		}

		// Pass conversation history
		var conversationContext []string
		for _, m := range internalReq.Messages {
			if m.Role == "user" || m.Role == "assistant" {
				conversationContext = append(conversationContext, fmt.Sprintf("[%s]: %s", m.Role, m.Content))
			}
		}
		if len(conversationContext) > 0 {
			debateConfig.Metadata["conversation_context"] = strings.Join(conversationContext, "\n")
		}

		// Execute debate through DebateService (handles multi-round consensus,
		// HelixSpecifier for big tasks, HelixMemory for persistence)
		sendSSEChunk("*Conducting multi-round AI debate for consensus...*\n\n")

		debateResult, debateErr := h.debateService.ConductDebate(ctx, debateConfig)

		if debateErr != nil {
			logrus.WithError(debateErr).Warn("[DEBATE] DebateService.ConductDebate failed, falling back to direct LLM")
			// Fallback: use the best available LLM for a direct response
			verifiedLLMs := h.debateTeamConfig.GetVerifiedLLMs()
			if len(verifiedLLMs) > 0 {
				bestLLM := verifiedLLMs[0]
				if bestLLM.Provider != nil {
					sendSSEChunk(fmt.Sprintf("*(direct response via %s/%s)*\n\n", bestLLM.ProviderName, bestLLM.ModelName))
					directReq := &models.LLMRequest{
						Messages:    internalReq.Messages,
						ModelParams: models.ModelParameters{Model: bestLLM.ModelName, MaxTokens: 4096},
					}
					directStream, directErr := bestLLM.Provider.CompleteStream(ctx, directReq)
					if directErr == nil && directStream != nil {
						for resp := range directStream {
							if resp == nil || resp.Content == "" {
								continue
							}
							if resp.FinishReason == "stop" || resp.FinishReason == "STOP" {
								break
							}
							sendSSEChunk(resp.Content)
						}
					}
				}
			}
		} else if debateResult != nil {
			// Stream the debate results: show each participant's response grouped by round
			currentRound := 0
			for _, resp := range debateResult.AllResponses {
				if resp.Round > currentRound {
					currentRound = resp.Round
					if currentRound > 1 {
						sendSSEChunk(fmt.Sprintf("\n**--- Round %d ---**\n\n", currentRound))
					}
				}
				name := resp.ParticipantName
				if name == "" {
					name = resp.Role
				}
				provider := resp.LLMProvider
				if provider == "" {
					provider = resp.LLMName
				}
				content := resp.Content
				if content == "" {
					content = resp.Response
				}
				if content != "" {
					sendSSEChunk(fmt.Sprintf("**%s** (%s)\n", name, provider))
					sendSSEChunk(content + "\n\n")
				}
			}

			// Show consensus status
			if debateResult.Consensus != nil {
				agreement := debateResult.Consensus.AgreementScore
				if agreement == 0 {
					agreement = debateResult.Consensus.AgreementLevel
				}
				if debateResult.Consensus.Reached || debateResult.Consensus.Achieved {
					sendSSEChunk(fmt.Sprintf("\n> Consensus reached (agreement: %.0f%%, rounds: %d)\n\n",
						agreement*100, debateResult.RoundsConducted))
				} else {
					sendSSEChunk(fmt.Sprintf("\n> Best agreement: %.0f%% after %d rounds\n\n",
						agreement*100, debateResult.RoundsConducted))
				}
				if debateResult.Consensus.Summary != "" {
					sendSSEChunk(debateResult.Consensus.Summary + "\n\n")
				}
			}

			// Stream the final synthesis
			sendSSEChunk("---\n\n**Final Decision**\n\n")
			if debateResult.Consensus != nil && debateResult.Consensus.FinalPosition != "" {
				sendSSEChunk(debateResult.Consensus.FinalPosition)
			} else if debateResult.BestResponse != nil {
				content := debateResult.BestResponse.Content
				if content == "" {
					content = debateResult.BestResponse.Response
				}
				sendSSEChunk(content)
			} else if len(debateResult.Participants) > 0 {
				last := debateResult.Participants[len(debateResult.Participants)-1]
				content := last.Content
				if content == "" {
					content = last.Response
				}
				sendSSEChunk(content)
			}
			sendSSEChunk("\n")

			logrus.WithFields(logrus.Fields{
				"debate_id":        debateID,
				"duration":         time.Since(startTime),
				"rounds":           debateResult.RoundsConducted,
				"total_responses":  len(debateResult.AllResponses),
				"consensus":        debateResult.Consensus != nil && (debateResult.Consensus.Reached || debateResult.Consensus.Achieved),
				"final_score":      debateResult.FinalScore,
				"memory_used":      debateResult.MemoryUsed,
				"specialized_role": debateResult.SpecializedRole,
			}).Info("[DEBATE] Multi-round consensus debate completed")
		}

		// Send SSE finish marker
		if sseHeadersSent {
			finishData, _ := json.Marshal(map[string]any{
				"id": streamID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
				"model": "helixagent-debate", "system_fingerprint": "fp_helixagent_v1",
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			})
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\ndata: [DONE]\n\n", finishData)
			flusher.Flush()
			return
		}
		h.sendOpenAIError(c, http.StatusInternalServerError, "internal_error", "Debate produced no output", "")
		return
	}

	// All debate logic is routed through DebateService.ConductDebate() above.
	// Legacy inline debate and comprehensive debate code removed — see git history.
	if false { // Dead code anchor — comprehensive package import
		_ = comprehensive.HasFallbackInvoker
	}

	// Fallback: Use the strongest verified provider for direct streaming
	streamChan, err := h.processWithEnsembleStream(ctx, internalReq, req)
	if err != nil {
		h.sendCategorizedError(c, err)
		return
	}

	// Set streaming headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("X-Accel-Buffering", "no") // Disable nginx buffering

	// Stream responses
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		h.sendOpenAIError(c, http.StatusInternalServerError, "internal_error", "Streaming not supported", "")
		return
	}

	// Track streaming state for OpenCode/Crush/HelixCode compatibility
	isFirstChunk := false
	sentFinalChunk := false // Track if we've already sent a finish_reason chunk
	// Note: streamID already generated above for orchestrator path

	// Client disconnect detection via request context cancellation.
	// c.Request.Context() is cancelled when the client disconnects,
	// making the deprecated http.CloseNotifier unnecessary.
	clientGone := c.Request.Context().Done()

	// Idle timeout ticker - if no data for 30 seconds, exit
	idleTimeout := time.NewTicker(30 * time.Second)
	defer idleTimeout.Stop()

	// Send first chunk with role (required by OpenCode)
	firstChunkResp := &models.LLMResponse{ID: streamID, CreatedAt: time.Now()}
	firstChunk := h.convertToOpenAIChatStreamResponse(firstChunkResp, req, true, streamID)
	if firstData, err := json.Marshal(firstChunk); err == nil {
		_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
		_, _ = c.Writer.Write(firstData)        //nolint:errcheck
		_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
		flusher.Flush()                         //nolint:errcheck
	}
	isFirstChunk = false

	// Check if this is a tool result processing turn vs a new user request
	// CRITICAL: Tool results must be processed directly to prevent infinite loops
	isToolResultTurn := h.isToolResultProcessingTurn(req.Messages)

	if isToolResultTurn {
		// Process tool results directly - synthesize into final response
		// This prevents: debate → tool_calls → results → debate → tool_calls... (infinite loop)
		logrus.Info("Streaming tool results - synthesizing without new debate")
		// Streaming emits no usage object (see sseChunkEnvelope), so the
		// provider response is intentionally discarded here.
		toolResultResponse, _, err := h.processToolResultsWithLLM(ctx, req)
		if err != nil {
			logrus.WithError(err).Warn("Failed to process tool results with LLM, using fallback response")
			fallbackResponse := h.generateFallbackToolResultsResponse(req)
			if fallbackResponse != "" {
				toolResultResponse = fallbackResponse
				err = nil
			} else {
				errContent := fmt.Sprintf("I encountered an issue processing the tool results: %v", err)
				errChunk := map[string]any{
					"id":                 streamID,
					"object":             "chat.completion.chunk",
					"created":            time.Now().Unix(),
					"model":              "helixagent-ensemble",
					"system_fingerprint": "fp_helixagent_v1",
					"choices": []map[string]any{
						{
							"index":         0,
							"delta":         map[string]any{"content": errContent},
							"logprobs":      nil,
							"finish_reason": nil,
						},
					},
				}
				if errData, marshalErr := json.Marshal(errChunk); marshalErr == nil {
					_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
					_, _ = c.Writer.Write(errData)          //nolint:errcheck
					_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
					flusher.Flush()                         //nolint:errcheck
				}
			}
		}
		if err == nil && toolResultResponse != "" {
			responseChunk := map[string]any{
				"id":                 streamID,
				"object":             "chat.completion.chunk",
				"created":            time.Now().Unix(),
				"model":              "helixagent-ensemble",
				"system_fingerprint": "fp_helixagent_v1",
				"choices": []map[string]any{
					{
						"index":         0,
						"delta":         map[string]any{"content": toolResultResponse},
						"logprobs":      nil,
						"finish_reason": nil,
					},
				},
			}
			if respData, err := json.Marshal(responseChunk); err == nil {
				_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
				_, _ = c.Writer.Write(respData)         //nolint:errcheck
				_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
				flusher.Flush()                         //nolint:errcheck
			}
		}

		// Send finish chunk and end
		finishChunk := map[string]any{
			"id":                 streamID,
			"object":             "chat.completion.chunk",
			"created":            time.Now().Unix(),
			"model":              "helixagent-ensemble",
			"system_fingerprint": "fp_helixagent_v1",
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         map[string]any{},
					"logprobs":      nil,
					"finish_reason": "stop",
				},
			},
		}
		if finishData, err := json.Marshal(finishChunk); err == nil {
			_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
			_, _ = c.Writer.Write(finishData)       //nolint:errcheck
			_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
			flusher.Flush()                         //nolint:errcheck
			logrus.Info("Tool results: sent finish_reason:stop chunk")
		}

		_, _ = c.Writer.Write([]byte("data: [DONE]\n\n")) //nolint:errcheck
		flusher.Flush()                                   //nolint:errcheck
		logrus.Info("Tool results: sent [DONE] - stream complete")
		return
	}

	// NEW user request → Full AI Debate ensemble
	logrus.Info("New streaming request - initiating AI Debate")

	// Stream AI Debate dialogue introduction before the actual response
	if h.showDebateDialogue && h.dialogueFormatter != nil && h.debateTeamConfig != nil {
		// Extract topic from the last user message
		// CRITICAL: Detect short follow-up responses (like "yes 1", "1", "ok") and expand them
		// with context from the previous assistant message that offered options
		topic := "User Query"
		var lastUserMessage string
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				lastUserMessage = req.Messages[i].Content
				break
			}
		}

		// Debug logging for topic extraction
		truncatedMsg := lastUserMessage
		if len(truncatedMsg) > 100 {
			truncatedMsg = truncatedMsg[:100]
		}
		logrus.WithFields(logrus.Fields{
			"message_count":     len(req.Messages),
			"last_user_message": truncatedMsg,
			"has_content":       lastUserMessage != "",
		}).Debug("Topic extraction from messages")

		// Check if this is a short follow-up response that needs context expansion
		expandedTopic := h.expandFollowUpResponse(lastUserMessage, req.Messages)
		if expandedTopic != "" {
			topic = expandedTopic
			logrus.WithFields(logrus.Fields{
				"original": lastUserMessage,
				"expanded": topic[:min(100, len(topic))] + "...",
			}).Info("Expanded follow-up response with conversation context")
		} else {
			topic = lastUserMessage
		}

		// CRITICAL: Sanitize the topic to remove system-level content like <system-reminder>
		// These are internal tags that should never be displayed to users
		preSanitizeTopic := topic
		topic = sanitizeDisplayContent(topic)

		// Debug logging for sanitization
		if topic != preSanitizeTopic {
			logrus.WithFields(logrus.Fields{
				"pre_sanitize_length":  len(preSanitizeTopic),
				"post_sanitize_length": len(topic),
				"topic_empty":          topic == "",
			}).Debug("Topic was modified by sanitization")
		}

		// FALLBACK: If topic is still empty, try to extract from the raw messages array
		// This handles cases where the Content field might be in a different format
		if topic == "" {
			logrus.Warn("Topic is empty after extraction, attempting fallback from messages")
			for i := len(req.Messages) - 1; i >= 0; i-- {
				if req.Messages[i].Role == "user" && req.Messages[i].Content != "" {
					topic = req.Messages[i].Content
					logrus.WithField("fallback_topic", topic[:min(50, len(topic))]).Info("Recovered topic from messages")
					break
				}
			}
			// If still empty, use a default
			if topic == "" {
				topic = "User request"
				logrus.Warn("Using default topic 'User request'")
			}
		}

		// Generate and stream debate dialogue introduction
		// Use format-aware introduction based on detected client type
		// DEFAULT: Use comprehensive debate system (8+ roles, 5 teams)
		// Set USE_LEGACY_DEBATE=true to use old 5-position debate system
		useLegacy := os.Getenv("USE_LEGACY_DEBATE") == "true"

		if useLegacy {
			// Legacy 5-position debate flow
			dialogueIntro := h.generateDebateDialogueIntroduction(topic, outputFormat)
			if dialogueIntro != "" {
				// Stream introduction in chunks for better rendering
				for _, line := range strings.Split(dialogueIntro, "\n") {
					if line == "" {
						line = "\n"
					} else {
						line = line + "\n"
					}
					introChunk := map[string]any{
						"id":                 streamID,
						"object":             "chat.completion.chunk",
						"created":            time.Now().Unix(),
						"model":              "helixagent-ensemble",
						"system_fingerprint": "fp_helixagent_v1",
						"choices": []map[string]any{
							{
								"index":         0,
								"delta":         map[string]any{"content": line},
								"logprobs":      nil,
								"finish_reason": nil,
							},
						},
					}
					if introData, err := json.Marshal(introChunk); err == nil {
						_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
						_, _ = c.Writer.Write(introData)        //nolint:errcheck
						_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
						flusher.Flush()                         //nolint:errcheck
					}
					// Small delay for visual effect
					time.Sleep(5 * time.Millisecond)
				}
			}

			// Stream REAL debate responses for each position (actual LLM calls)
			positions := []services.DebateTeamPosition{
				services.PositionAnalyst,
				services.PositionProposer,
				services.PositionCritic,
				services.PositionSynthesis,
				services.PositionMediator,
			}
			previousResponses := make(map[services.DebateTeamPosition]string)
			// CRITICAL: Collect tool_calls from ALL debate positions for ACTION PHASE
			collectedToolCalls := make([]StreamingToolCall, 0) //nolint:unused

			for _, pos := range positions {
				// Get member info for this position for the request indicator
				member := h.debateTeamConfig.GetTeamMember(pos)
				var memberProvider, memberModel string
				var memberRole services.DebateRole
				if member != nil {
					memberProvider = member.ProviderName
					memberModel = member.ModelName
					memberRole = member.Role
				} else {
					memberProvider = "unknown"
					memberModel = "unknown"
					memberRole = services.RoleAnalyst
				}

				// Stream REQUEST indicator: [A: Analyst] <--- Request sent to DeepSeek (deepseek-chat)
				// Uses format-aware indicator based on client type (ANSI for terminal, Markdown for API clients)
				requestIndicator := FormatRequestIndicatorForFormat(outputFormat, pos, memberRole, memberProvider, memberModel)
				if requestIndicator != "" {
					reqChunk := map[string]any{
						"id":                 streamID,
						"object":             "chat.completion.chunk",
						"created":            time.Now().Unix(),
						"model":              "helixagent-ensemble",
						"system_fingerprint": "fp_helixagent_v1",
						"choices": []map[string]any{
							{
								"index":         0,
								"delta":         map[string]any{"content": requestIndicator},
								"logprobs":      nil,
								"finish_reason": nil,
							},
						},
					}
					if reqData, err := json.Marshal(reqChunk); err == nil {
						_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
						_, _ = c.Writer.Write(reqData)          //nolint:errcheck
						_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
						flusher.Flush()                         //nolint:errcheck
					}
				}

				// Now get the REAL response from the LLM for this position
				// CRITICAL: Pass tools so LLM knows about available coding assistant capabilities
				// For legacy debate, pass empty string for comprehensive role
				debateResp, err := h.generateRealDebateResponse(ctx, pos, "", topic, previousResponses, req.Tools)
				var realResponse string
				var responseIndicator string

				if err != nil {
					// Log error but continue with fallback message
					logrus.WithError(err).WithField("position", pos).Warn("Failed to get real debate response, using fallback")
					realResponse = "Unable to provide analysis at this time."
					// Format error response indicator using format-aware version
					responseIndicator = FormatResponseIndicatorForFormat(outputFormat, pos, memberRole, 0)
				} else {
					realResponse = debateResp.Content

					// Format response indicator based on whether fallback was used
					if debateResp.UsedFallback {
						// CRITICAL: Extract error information from the fallback chain
						// to show the exact cause of the fallback in the response
						var primaryError string
						var primaryDuration time.Duration
						if len(debateResp.FallbackChain) > 0 {
							// Find the first failed attempt (primary provider failure)
							for _, attempt := range debateResp.FallbackChain {
								if !attempt.Success && attempt.Error != "" {
									primaryError = attempt.Error
									primaryDuration = attempt.Duration
									break
								}
							}
						}

						// Show fallback indicator WITH detailed error cause
						// This is the key improvement: CLI agents see WHY the fallback happened
						if primaryError != "" {
							// Use format-aware detailed error formatting
							responseIndicator = FormatFallbackWithErrorForFormat(outputFormat, memberRole,
								debateResp.PrimaryProvider, debateResp.PrimaryModel,
								debateResp.ActualProvider, debateResp.ActualModel,
								primaryError, 1, primaryDuration)

							// Log fallback event for observability
							logrus.WithFields(logrus.Fields{
								"position":         pos,
								"role":             memberRole,
								"primary_provider": debateResp.PrimaryProvider,
								"primary_model":    debateResp.PrimaryModel,
								"actual_provider":  debateResp.ActualProvider,
								"actual_model":     debateResp.ActualModel,
								"error":            primaryError,
								"error_category":   categorizeErrorString(primaryError),
								"chain_length":     len(debateResp.FallbackChain),
								"total_time":       debateResp.ResponseTime,
							}).Warn("Fallback triggered - detailed error in response")
						} else {
							// Fallback without captured error (shouldn't happen but be defensive)
							responseIndicator = FormatFallbackIndicatorForFormat(outputFormat, pos, memberRole, debateResp.ActualProvider, debateResp.ActualModel, debateResp.ResponseTime)
						}
					} else {
						// Normal response: [A: Analyst] ---> (450 ms)
						responseIndicator = FormatResponseIndicatorForFormat(outputFormat, pos, memberRole, debateResp.ResponseTime)
					}

					// CRITICAL: Collect tool_calls from this position for ACTION PHASE
					if len(debateResp.ToolCalls) > 0 {
						logrus.WithFields(logrus.Fields{
							"position":   pos,
							"tool_count": len(debateResp.ToolCalls),
						}).Info("<--- Collecting tool_calls from debate position --->")
						// Re-index tool calls to avoid conflicts
						for _, tc := range debateResp.ToolCalls {
							tc.Index = len(collectedToolCalls)
							collectedToolCalls = append(collectedToolCalls, tc)
						}
					}
				}

				// Store for context in subsequent positions
				previousResponses[pos] = realResponse

				// Stream RESPONSE indicator with timing: [A: Analyst] ---> (450 ms)
				if responseIndicator != "" {
					respIndChunk := map[string]any{
						"id":                 streamID,
						"object":             "chat.completion.chunk",
						"created":            time.Now().Unix(),
						"model":              "helixagent-ensemble",
						"system_fingerprint": "fp_helixagent_v1",
						"choices": []map[string]any{
							{
								"index":         0,
								"delta":         map[string]any{"content": responseIndicator},
								"logprobs":      nil,
								"finish_reason": nil,
							},
						},
					}
					if respIndData, err := json.Marshal(respIndChunk); err == nil {
						_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck // streaming write errors cannot be handled
						_, _ = c.Writer.Write(respIndData)      //nolint:errcheck // streaming write errors cannot be handled
						_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck // streaming write errors cannot be handled
						flusher.Flush()                         //nolint:errcheck
					}
				}

				// Stream the actual LLM response content
				// For terminal clients: dim/gray ANSI formatting for debate phase content
				// For API clients: clean Markdown quote block formatting
				responseContent := FormatPhaseContentForFormat(outputFormat, realResponse) + "\n\n"
				responseChunk := map[string]any{
					"id":                 streamID,
					"object":             "chat.completion.chunk",
					"created":            time.Now().Unix(),
					"model":              "helixagent-ensemble",
					"system_fingerprint": "fp_helixagent_v1",
					"choices": []map[string]any{
						{
							"index":         0,
							"delta":         map[string]any{"content": responseContent},
							"logprobs":      nil,
							"finish_reason": nil,
						},
					},
				}
				if responseData, err := json.Marshal(responseChunk); err == nil {
					_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck // streaming write errors cannot be handled
					_, _ = c.Writer.Write(responseData)     //nolint:errcheck // streaming write errors cannot be handled
					_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck // streaming write errors cannot be handled
					flusher.Flush()                         //nolint:errcheck
				}
			}

			// Stream conclusion with format-aware styling
			conclusion := h.generateDebateDialogueConclusion(outputFormat)
			if conclusion != "" {
				conclusionChunk := map[string]any{
					"id":                 streamID,
					"object":             "chat.completion.chunk",
					"created":            time.Now().Unix(),
					"model":              "helixagent-ensemble",
					"system_fingerprint": "fp_helixagent_v1",
					"choices": []map[string]any{
						{
							"index":         0,
							"delta":         map[string]any{"content": conclusion},
							"logprobs":      nil,
							"finish_reason": nil,
						},
					},
				}
				if conclusionData, err := json.Marshal(conclusionChunk); err == nil {
					_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck // streaming write errors cannot be handled
					_, _ = c.Writer.Write(conclusionData)   //nolint:errcheck // streaming write errors cannot be handled
					_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck // streaming write errors cannot be handled
					flusher.Flush()                         //nolint:errcheck
				}
			}

			// CRITICAL FIX: Generate FINAL SYNTHESIS based on all debate responses
			// This is the actual "consensus" that combines all 5 perspectives
			// IMPORTANT: Pass tools so synthesis knows about available coding assistant capabilities
			synthesisResponse, synthesisErr := h.generateFinalSynthesis(ctx, topic, previousResponses, req.Tools)
			if synthesisErr != nil {
				logrus.WithError(synthesisErr).Warn("Failed to generate final synthesis")
				synthesisResponse = "Based on the debate, a consensus could not be reached. Please consider the individual perspectives above."
			}

			// Stream the synthesis response as the consensus content
			// IMPORTANT: Final synthesis formatting adapts to client type:
			// - Terminal clients get ANSI bright white for visibility
			// - API clients (OpenCode, Crush) get clean Markdown without escape codes
			if synthesisResponse != "" {
				// Format with client-appropriate styling for final answer visibility
				formattedSynthesis := FormatFinalResponseForFormat(outputFormat, synthesisResponse) + "\n"
				synthesisChunk := map[string]any{
					"id":                 streamID,
					"object":             "chat.completion.chunk",
					"created":            time.Now().Unix(),
					"model":              "helixagent-ensemble",
					"system_fingerprint": "fp_helixagent_v1",
					"choices": []map[string]any{
						{
							"index":         0,
							"delta":         map[string]any{"content": formattedSynthesis},
							"logprobs":      nil,
							"finish_reason": nil,
						},
					},
				}
				if synthesisData, err := json.Marshal(synthesisChunk); err == nil {
					_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
					_, _ = c.Writer.Write(synthesisData)    //nolint:errcheck
					_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
					flusher.Flush()                         //nolint:errcheck
				}
			}

			// ACTION PHASE: If tools are available, execute tool_calls from debate OR generate new ones
			// This allows the AI coding assistant to actually USE the tools, not just talk about them
			// PRIORITY: 1) Use collected tool_calls from debate positions, 2) Generate via LLM analysis
			if len(req.Tools) > 0 {
				// CRITICAL FIX: Use collected tool_calls from debate positions if available
				var actionToolCalls []StreamingToolCall
				if len(collectedToolCalls) > 0 {
					logrus.WithFields(logrus.Fields{
						"collected_count": len(collectedToolCalls),
					}).Info("<--- ACTION PHASE: Using collected tool_calls from debate --->")
					actionToolCalls = collectedToolCalls
				} else {
					// Fallback: Generate tool_calls based on topic analysis
					logrus.Info("<--- ACTION PHASE: No debate tool_calls, analyzing topic for actions --->")
					actionToolCalls = h.generateActionToolCalls(ctx, topic, synthesisResponse, req.Tools, previousResponses, req.Messages)
				}

				if len(actionToolCalls) > 0 {
					// Stream action indicator to show tools are being invoked
					actionIndicator := fmt.Sprintf("\n\n<--- EXECUTING %d ACTION(S) --->\n", len(actionToolCalls))
					indicatorChunk := map[string]any{
						"id":                 streamID,
						"object":             "chat.completion.chunk",
						"created":            time.Now().Unix(),
						"model":              "helixagent-ensemble",
						"system_fingerprint": "fp_helixagent_v1",
						"choices": []map[string]any{
							{
								"index":         0,
								"delta":         map[string]any{"content": actionIndicator},
								"logprobs":      nil,
								"finish_reason": nil,
							},
						},
					}
					if indicatorData, err := json.Marshal(indicatorChunk); err == nil {
						_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
						_, _ = c.Writer.Write(indicatorData)    //nolint:errcheck
						_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
						flusher.Flush()                         //nolint:errcheck
					}

					// Stream the tool calls to the client
					for _, toolCall := range actionToolCalls {
						// Log each tool being invoked with indicators
						logrus.WithFields(logrus.Fields{
							"tool_name": toolCall.Function.Name,
							"tool_id":   toolCall.ID,
						}).Info("<--- Invoking tool --->")

						toolCallChunk := map[string]any{
							"id":                 streamID,
							"object":             "chat.completion.chunk",
							"created":            time.Now().Unix(),
							"model":              "helixagent-ensemble",
							"system_fingerprint": "fp_helixagent_v1",
							"choices": []map[string]any{
								{
									"index": 0,
									"delta": map[string]any{
										"tool_calls": []map[string]any{
											{
												"index": toolCall.Index,
												"id":    toolCall.ID,
												"type":  "function",
												"function": map[string]any{
													"name":      toolCall.Function.Name,
													"arguments": toolCall.Function.Arguments,
												},
											},
										},
									},
									"logprobs":      nil,
									"finish_reason": nil,
								},
							},
						}
						if toolCallData, err := json.Marshal(toolCallChunk); err == nil {
							_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
							_, _ = c.Writer.Write(toolCallData)     //nolint:errcheck
							_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
							flusher.Flush()                         //nolint:errcheck
						}
					}

					// Send finish_reason: tool_calls
					finishChunk := map[string]any{
						"id":                 streamID,
						"object":             "chat.completion.chunk",
						"created":            time.Now().Unix(),
						"model":              "helixagent-ensemble",
						"system_fingerprint": "fp_helixagent_v1",
						"choices": []map[string]any{
							{
								"index":         0,
								"delta":         map[string]any{},
								"logprobs":      nil,
								"finish_reason": "tool_calls",
							},
						},
					}
					if finishData, err := json.Marshal(finishChunk); err == nil {
						_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
						_, _ = c.Writer.Write(finishData)       //nolint:errcheck
						_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
						flusher.Flush()                         //nolint:errcheck
					}
					sentFinalChunk = true //nolint:ineffassign
					logrus.WithField("tool_calls_count", len(actionToolCalls)).Info("AI Debate: sent tool_calls with finish_reason:tool_calls")

					// CRITICAL: After sending tool_calls, immediately end the response
					// The client will execute the tools and send another request with results
					// Do NOT send any more content or footer - it confuses the tool calling protocol
					_, _ = c.Writer.Write([]byte("data: [DONE]\n\n")) //nolint:errcheck
					flusher.Flush()                                   //nolint:errcheck
					logrus.Info("AI Debate with tool_calls: sent [DONE] - stream complete")
					return
				}
			}

			// CRITICAL FIX: If we showed debate dialogue but no tool_calls were generated,
			// we've already streamed all content. Close the stream properly!
			// Without this, the code falls through to StreamLoop and waits forever.
			logrus.Info("Debate dialogue complete, no tool calls - finalizing stream")

			// Send response footer
			footer := h.generateResponseFooter()
			if footer != "" {
				footerChunk := map[string]any{
					"id":                 streamID,
					"object":             "chat.completion.chunk",
					"created":            time.Now().Unix(),
					"model":              "helixagent-ensemble",
					"system_fingerprint": "fp_helixagent_v1",
					"choices": []map[string]any{
						{
							"index":         0,
							"delta":         map[string]any{"content": footer},
							"logprobs":      nil,
							"finish_reason": nil,
						},
					},
				}
				if footerData, err := json.Marshal(footerChunk); err == nil {
					_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
					_, _ = c.Writer.Write(footerData)       //nolint:errcheck
					_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
					flusher.Flush()                         //nolint:errcheck
				}
			}

			// Send final chunk with finish_reason: stop
			finalChunk := map[string]any{
				"id":                 streamID,
				"object":             "chat.completion.chunk",
				"created":            time.Now().Unix(),
				"model":              "helixagent-ensemble",
				"system_fingerprint": "fp_helixagent_v1",
				"choices": []map[string]any{
					{
						"index":         0,
						"delta":         map[string]any{},
						"logprobs":      nil,
						"finish_reason": "stop",
					},
				},
			}
			if finalData, err := json.Marshal(finalChunk); err == nil {
				_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
				_, _ = c.Writer.Write(finalData)        //nolint:errcheck
				_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
				flusher.Flush()                         //nolint:errcheck
			}

			// Send [DONE] and return - stream is complete
			_, _ = c.Writer.Write([]byte("data: [DONE]\n\n")) //nolint:errcheck
			flusher.Flush()                                   //nolint:errcheck
			logrus.Info("Debate dialogue: sent [DONE] - stream complete")
			return
		}

		// DEFAULT: Comprehensive debate system (8+ roles, 5 teams)
		// This implements the research from docs/requests/debate/
		h.streamComprehensiveDebate(ctx, c, flusher, streamID, topic, outputFormat, req)
		return
	}

StreamLoop:
	for {
		select {
		case <-clientGone:
			// Client disconnected, stop streaming
			cancel()
			return
		case <-ctx.Done():
			// Context cancelled or timed out, exit gracefully
			break StreamLoop
		case <-idleTimeout.C:
			// No data received for 30 seconds, break to send DONE
			break StreamLoop
		case response, ok := <-streamChan:
			if !ok {
				// Channel closed, stream complete
				break StreamLoop
			}

			// Reset idle timeout on data received
			idleTimeout.Reset(30 * time.Second)

			// Skip empty content chunks (Issue #2840 fix for OpenCode)
			if response.Content == "" && response.FinishReason == "" {
				continue
			}

			// Track if this response already has finish_reason
			if response.FinishReason != "" {
				sentFinalChunk = true
			}

			// Convert to streaming format with consistent streamID
			streamResp := h.convertToOpenAIChatStreamResponse(response, req, isFirstChunk, streamID)

			// Send as Server-Sent Events
			data, err := json.Marshal(streamResp)
			if err != nil {
				continue // Skip malformed response
			}

			// Write data with error handling
			if _, err := c.Writer.Write([]byte("data: ")); err != nil {
				cancel()
				return
			}
			if _, err := c.Writer.Write(data); err != nil {
				cancel()
				return
			}
			if _, err := c.Writer.Write([]byte("\n\n")); err != nil {
				cancel()
				return
			}
			flusher.Flush() //nolint:errcheck
		}
	}

	// Send response footer before final chunk (if debate dialogue was shown)
	if h.showDebateDialogue {
		footer := h.generateResponseFooter()
		if footer != "" {
			footerChunk := map[string]any{
				"id":                 streamID,
				"object":             "chat.completion.chunk",
				"created":            time.Now().Unix(),
				"model":              "helixagent-ensemble",
				"system_fingerprint": "fp_helixagent_v1",
				"choices": []map[string]any{
					{
						"index":         0,
						"delta":         map[string]any{"content": footer},
						"logprobs":      nil,
						"finish_reason": nil,
					},
				},
			}
			if footerData, err := json.Marshal(footerChunk); err == nil {
				_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
				_, _ = c.Writer.Write(footerData)       //nolint:errcheck
				_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
				flusher.Flush()                         //nolint:errcheck
			}
		}
	}

	// Only send final chunk if we haven't already sent one with finish_reason
	if !sentFinalChunk {
		finalChunk := map[string]any{
			"id":                 streamID, // Same ID as all other chunks
			"object":             "chat.completion.chunk",
			"created":            time.Now().Unix(),
			"model":              "helixagent-ensemble",
			"system_fingerprint": "fp_helixagent_v1",
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         map[string]any{},
					"logprobs":      nil,
					"finish_reason": "stop",
				},
			},
		}
		finalData, _ := json.Marshal(finalChunk) //nolint:errcheck
		_, _ = c.Writer.Write([]byte("data: "))  //nolint:errcheck
		_, _ = c.Writer.Write(finalData)         //nolint:errcheck
		_, _ = c.Writer.Write([]byte("\n\n"))    //nolint:errcheck
		flusher.Flush()                          //nolint:errcheck
	}

	// Always send [DONE] to properly close the stream
	_, _ = c.Writer.Write([]byte("data: [DONE]\n\n")) //nolint:errcheck
	flusher.Flush()                                   //nolint:errcheck
}

// ChatCompletionsStream handles streaming OpenAI chat completions
func (h *UnifiedHandler) ChatCompletionsStream(c *gin.Context) {
	// Parse OpenAI request
	var req OpenAIChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.sendOpenAIError(c, http.StatusBadRequest, "invalid_request", "Invalid request format", err.Error())
		return
	}

	// Force streaming mode
	req.Stream = true

	// Convert to internal request format
	internalReq := h.convertOpenAIChatRequest(&req, c)

	// Create a cancellable context with timeout for the stream
	// Maximum 420 seconds (7 min) to allow for multiple provider fallbacks during tool result processing
	// With 6 providers at 60 seconds each, we need at least 360 seconds plus buffer
	ctx, cancel := context.WithTimeout(c.Request.Context(), 420*time.Second)
	defer cancel()

	// Process with ensemble streaming
	streamChan, err := h.processWithEnsembleStream(ctx, internalReq, &req)
	if err != nil {
		h.sendCategorizedError(c, err)
		return
	}

	// Set streaming headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("X-Accel-Buffering", "no") // Disable nginx buffering

	// Get flusher for proper SSE streaming
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		h.sendOpenAIError(c, http.StatusInternalServerError, "internal_error", "Streaming not supported", "")
		return
	}

	// Track streaming state for OpenCode/Crush/HelixCode compatibility
	isFirstChunk := false
	sentFinalChunk := false                      // Track if we've already sent a finish_reason chunk
	streamID := utils.SecureRandomID("chatcmpl") // D-20: crypto-random, collision-free. Consistent ID across all chunks

	// Client disconnect detection via request context cancellation.
	// c.Request.Context() is cancelled when the client disconnects,
	// making the deprecated http.CloseNotifier unnecessary.
	clientGone := c.Request.Context().Done()

	// Idle timeout ticker - if no data for 30 seconds, exit
	idleTimeout := time.NewTicker(30 * time.Second)
	defer idleTimeout.Stop()

	// Send first chunk with role (required by OpenCode)
	firstChunkResp := &models.LLMResponse{ID: streamID, CreatedAt: time.Now()}
	firstChunk := h.convertToOpenAIChatStreamResponse(firstChunkResp, &req, true, streamID)
	if firstData, err := json.Marshal(firstChunk); err == nil {
		_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
		_, _ = c.Writer.Write(firstData)        //nolint:errcheck
		_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
		flusher.Flush()                         //nolint:errcheck
	}
	isFirstChunk = false

StreamLoop:
	for {
		select {
		case <-clientGone:
			// Client disconnected, stop streaming
			cancel()
			return
		case <-ctx.Done():
			// Context cancelled or timed out, exit gracefully
			break StreamLoop
		case <-idleTimeout.C:
			// No data received for 30 seconds, break to send DONE
			break StreamLoop
		case response, ok := <-streamChan:
			if !ok {
				// Channel closed, stream complete
				break StreamLoop
			}

			// Reset idle timeout on data received
			idleTimeout.Reset(30 * time.Second)

			// Skip empty content chunks (Issue #2840 fix for OpenCode)
			if response.Content == "" && response.FinishReason == "" {
				continue
			}

			// Track if this response already has finish_reason
			if response.FinishReason != "" {
				sentFinalChunk = true
			}

			// Convert to streaming format with consistent streamID
			streamResp := h.convertToOpenAIChatStreamResponse(response, &req, isFirstChunk, streamID)

			// Send as Server-Sent Events
			data, err := json.Marshal(streamResp)
			if err != nil {
				continue // Skip malformed response
			}

			// Write data with error handling
			if _, err := c.Writer.Write([]byte("data: ")); err != nil {
				cancel()
				return
			}
			if _, err := c.Writer.Write(data); err != nil {
				cancel()
				return
			}
			if _, err := c.Writer.Write([]byte("\n\n")); err != nil {
				cancel()
				return
			}
			flusher.Flush() //nolint:errcheck
		}
	}

	// Only send final chunk if we haven't already sent one with finish_reason
	if !sentFinalChunk {
		finalChunk := map[string]any{
			"id":                 streamID, // Same ID as all other chunks
			"object":             "chat.completion.chunk",
			"created":            time.Now().Unix(),
			"model":              "helixagent-ensemble",
			"system_fingerprint": "fp_helixagent_v1",
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         map[string]any{},
					"logprobs":      nil,
					"finish_reason": "stop",
				},
			},
		}
		finalData, _ := json.Marshal(finalChunk) //nolint:errcheck
		_, _ = c.Writer.Write([]byte("data: "))  //nolint:errcheck
		_, _ = c.Writer.Write(finalData)         //nolint:errcheck
		_, _ = c.Writer.Write([]byte("\n\n"))    //nolint:errcheck
		flusher.Flush()                          //nolint:errcheck
	}

	// Always send [DONE] to properly close the stream
	_, _ = c.Writer.Write([]byte("data: [DONE]\n\n")) //nolint:errcheck
	flusher.Flush()                                   //nolint:errcheck
}

// Completions handles legacy text completions
func (h *UnifiedHandler) Completions(c *gin.Context) {
	// Convert to chat format and process
	var req struct {
		Model       string  `json:"model"`
		Prompt      string  `json:"prompt"`
		MaxTokens   int     `json:"max_tokens,omitempty"`
		Temperature float64 `json:"temperature,omitempty"`
		Stream      bool    `json:"stream,omitempty"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		h.sendOpenAIError(c, http.StatusBadRequest, "invalid_request", "Invalid request format", err.Error())
		return
	}

	// Convert to chat request and call directly
	h.ChatCompletions(c)
}

// CompletionsStream handles legacy streaming text completions
func (h *UnifiedHandler) CompletionsStream(c *gin.Context) {
	h.Completions(c) // Reuse completions logic
}

// Models returns only the HelixAgent virtual model
// HelixAgent exposes a single unified model that internally uses AI debate ensemble
// Backend provider models are implementation details and not exposed to clients
func (h *UnifiedHandler) Models(c *gin.Context) {
	// HA-F2-001 (FR-019): the ids below are dispatch selectors, NOT served
	// models. They may advertise sampling/dispatch ONLY when the wired
	// catalog confirms a serving backend behind them:
	//   - debate ids (helixagent-debate / helixagent-ensemble / helix-debate)
	//     require ANY serving model — an ensemble with nothing to ensemble
	//     is a bluff;
	//   - llm-chain ids (helixagent-llm / helix-llm) require a serving
	//     helixllm/* option — a HelixLLM-first chain with no serving
	//     HelixLLM is a bluff.
	// With no catalog wired nothing is confirmed: availability "unreported",
	// dispatch withheld (§11.4.6 — the absence of a serving claim is not a
	// serving claim). The ids stay listed so pre-validating CLI configs keep
	// resolving them (CONST-032 / challenge guards) — what changes is only
	// the honesty of the permission block.
	var entries []catalog.Entry
	catalogWired := h.catalogService != nil
	if catalogWired {
		entries = h.catalogService.Build()
	}
	serving := catalog.ServingModels(entries)

	llmChainUsable := false
	for _, e := range serving {
		if e.Provider == catalog.NameHelixLLM {
			llmChainUsable = true
			break
		}
	}
	debateUsable := len(serving) > 0

	now := time.Now().Unix()

	// annotate builds one facade entry. The availability annotation is
	// carried for the NON-usable states only — "unreported" (nothing
	// reported) or "withheld" + reason — so the field never overstates; a
	// usable entry states its usability through the permission block.
	annotate := func(id string, usable bool, availability, withheldReason string) OpenAIModel {
		return OpenAIModel{
			ID:      id,
			Object:  "model",
			Created: now,
			OwnedBy: "helixagent",
			Permission: []OpenAIModelPermission{{
				ID:                 id + "-permission",
				Object:             "model_permission",
				Created:            now,
				AllowCreateEngine:  usable,
				AllowSampling:      usable,
				AllowLogprobs:      usable,
				AllowSearchIndices: true,
				AllowView:          true,
				AllowFineTuning:    false,
				Organization:       "helixagent",
				IsBlocking:         false,
			}},
			Availability:   availability,
			WithheldReason: withheldReason,
			Root:           id,
			Parent:         nil,
		}
	}

	const (
		availabilityUnreported = "unreported"
		availabilityWithheld   = "withheld"
	)

	// pseudo annotates one dispatch selector. Usable → no availability
	// field (the permission block is the claim). Not usable → "unreported"
	// when nothing is wired at all, "withheld"+reason when the catalog is
	// wired but nothing is serving.
	pseudo := func(id string, usable bool) OpenAIModel {
		if usable {
			return annotate(id, true, "", "")
		}
		if !catalogWired {
			return annotate(id, false, availabilityUnreported, "")
		}
		return annotate(id, false, availabilityWithheld, "no_serving_backend")
	}

	data := []OpenAIModel{
		// Canonical names used in CLI configs and docs/api/API_REFERENCE.md.
		pseudo(ModelHelixAgentDebate, debateUsable),
		pseudo(ModelHelixAgentLLM, llmChainUsable),
		// Semantic alias for the debate ensemble (user-visible
		// "ensemble" terminology — same dispatch as helixagent-debate).
		pseudo(ModelHelixAgentEnsemble, debateUsable),
		// Legacy aliases retained so existing clients keep resolving.
		pseudo(ModelHelixDebate, debateUsable),
		pseudo(ModelHelixLLM, llmChainUsable),
	}

	// Append the serving layer's own helixllm options — serving, withheld
	// and unreported, each annotated — so the wire-visible /v1/models
	// listing matches what the serving layer is actually offering (FR-019).
	// UNCONFIRMED: how a [:<variant>] suffix on ModelIdentity is handled
	// downstream of this facade is not yet exercised; the wire id is the
	// entry Name (helixllm/<id>).
	for _, e := range catalog.HelixLLMOptions(entries) {
		usable := e.Availability.Usable()
		availability := availabilityUnreported
		reason := ""
		switch e.Availability {
		case catalog.AvailabilityServing:
			availability = "serving"
		case catalog.AvailabilityWithheld:
			availability = availabilityWithheld
			reason = string(e.WithheldReason)
		}
		data = append(data, annotate(e.Name, usable, availability, reason))
	}

	c.JSON(http.StatusOK, OpenAIModelsResponse{
		Object: "list",
		Data:   data,
	})
}

// Helper methods

// processSkillsForRequest processes skills for an OpenAI chat request.
// It extracts the last user message, matches skills, and enhances messages with skill instructions.
// Returns the skills request context (or nil if no skills integration or no matches) and the enhanced messages.
func (h *UnifiedHandler) processSkillsForRequest(ctx context.Context, req *OpenAIChatRequest, requestID string) (*skills.RequestContext, []OpenAIMessage) {
	// Check if skills integration is available
	if h.skillsIntegration == nil {
		return nil, req.Messages
	}

	// Find the last user message to use for skill matching
	var lastUserMessage string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUserMessage = req.Messages[i].Content
			break
		}
	}

	if lastUserMessage == "" {
		return nil, req.Messages
	}

	// Process skills for this request
	skillsCtx, err := h.skillsIntegration.ProcessRequest(ctx, requestID, lastUserMessage)
	if err != nil {
		logrus.WithError(err).Warn("Failed to process skills for request")
		return nil, req.Messages
	}

	// If no skills matched, return original messages
	if skillsCtx == nil || len(skillsCtx.SkillsToApply) == 0 {
		return nil, req.Messages
	}

	// Enhance the last user message with skill instructions
	// We'll create a new system message with skill instructions and insert it before the last user message
	enhancedMessages := make([]OpenAIMessage, 0, len(req.Messages)+1)
	skillInstructions := h.skillsIntegration.EnhancePromptWithSkills("", skillsCtx)

	// Find the index of the last user message
	lastUserIdx := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}

	if lastUserIdx >= 0 {
		// Copy all messages before the last user message
		enhancedMessages = append(enhancedMessages, req.Messages[:lastUserIdx]...)

		// Add system message with skill instructions before the user message
		if skillInstructions != "" {
			systemMsg := OpenAIMessage{
				Role:    "system",
				Content: skillInstructions,
			}
			enhancedMessages = append(enhancedMessages, systemMsg)
		}

		// Add the last user message and any messages after it
		enhancedMessages = append(enhancedMessages, req.Messages[lastUserIdx:]...)
	} else {
		// Should not happen, but fallback
		enhancedMessages = req.Messages
	}

	return skillsCtx, enhancedMessages
}

func (h *UnifiedHandler) convertOpenAIChatRequest(req *OpenAIChatRequest, c *gin.Context) *models.LLMRequest {
	// Generate request ID
	requestID := utils.SecureRandomID("openai") // D-20: crypto-random, collision-free

	// Get user info from context
	userID := "anonymous"
	if uid, exists := c.Get("user_id"); exists {
		if uidStr, ok := uid.(string); ok {
			userID = uidStr
		}
	}

	sessionID := fmt.Sprintf("session_%d_%s", time.Now().UnixNano(), utils.SecureRandomID("")) // D-20: crypto-random suffix, collision-free
	if sid, exists := c.Get("session_id"); exists {
		if sidStr, ok := sid.(string); ok {
			sessionID = sidStr
		}
	}

	// Convert messages
	messages := make([]models.Message, 0, len(req.Messages))
	for _, msg := range req.Messages {
		toolCalls := make(map[string]interface{})
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				toolCalls[tc.ID] = map[string]interface{}{
					"type":     tc.Type,
					"function": tc.Function,
				}
			}
		}

		// Typed array of tool calls — populated alongside the legacy
		// map so upstream providers emit them in the correct OpenAI
		// ordered format on the assistant message. Without this,
		// providers reject following tool messages as "no preceding
		// tool_calls" (CONST-032 reproduction:
		// challenges/scripts/opencode_parallel_tool_calls_challenge.sh).
		var assistantToolCalls []models.ToolCall
		if len(msg.ToolCalls) > 0 {
			assistantToolCalls = make([]models.ToolCall, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				assistantToolCalls = append(assistantToolCalls, models.ToolCall{
					ID:   tc.ID,
					Type: tc.Type,
					Function: models.ToolCallFunction{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
			}
		}

		messages = append(messages, models.Message{
			Role:               msg.Role,
			Content:            msg.Content,
			Name:               msg.Name,
			ToolCalls:          toolCalls,
			ToolCallID:         msg.ToolCallID,
			AssistantToolCalls: assistantToolCalls,
		})
	}

	// Set default ensemble config if not provided
	ensembleConfig := req.EnsembleConfig
	if ensembleConfig == nil {
		shouldUseEnsemble := true

		if h.intentRouter != nil {
			var lastUserMessage string
			for i := len(req.Messages) - 1; i >= 0; i-- {
				if req.Messages[i].Role == "user" {
					lastUserMessage = req.Messages[i].Content
					break
				}
			}

			if lastUserMessage != "" {
				shouldUseEnsemble = h.intentRouter.ShouldUseEnsemble(lastUserMessage, nil)
				logrus.WithFields(logrus.Fields{
					"message_preview":     truncateString(lastUserMessage, 50),
					"should_use_ensemble": shouldUseEnsemble,
				}).Debug("IntentBasedRouter decision")
			}
		}

		if shouldUseEnsemble {
			ensembleConfig = &models.EnsembleConfig{
				Strategy:            "confidence_weighted",
				MinProviders:        2,
				ConfidenceThreshold: 0.8,
				FallbackToBest:      true,
				Timeout:             30,
				PreferredProviders:  []string{},
			}
		} else {
			ensembleConfig = nil
			logrus.Debug("Using single provider (simple request detected)")
		}
	}

	// Convert tools to provider-specific format
	// CRITICAL: This enables tool calling for AI coding assistants
	var toolsData interface{}
	if len(req.Tools) > 0 {
		toolsData = req.Tools
		logrus.WithField("tool_count", len(req.Tools)).Debug("Passing tools to LLM request")
	}

	llmReq := &models.LLMRequest{
		ID:        requestID,
		SessionID: sessionID,
		UserID:    userID,
		Prompt:    "", // System prompt should be in messages
		Messages:  messages,
		ModelParams: models.ModelParameters{
			Model:         req.Model,
			Temperature:   req.Temperature,
			MaxTokens:     req.MaxTokens,
			TopP:          req.TopP,
			StopSequences: req.Stop,
			ProviderSpecific: map[string]any{
				"presence_penalty":    req.PresencePenalty,
				"frequency_penalty":   req.FrequencyPenalty,
				"logit_bias":          req.LogitBias,
				"user":                req.User,
				"force_provider":      req.ForceProvider,
				"tools":               toolsData,
				"tool_choice":         req.ToolChoice,
				"parallel_tool_calls": req.ParallelToolCalls,
			},
		},
		EnsembleConfig: ensembleConfig,
		MemoryEnhanced: true, // Enable memory for all requests
		Memory:         map[string]string{},
		Status:         "pending",
		CreatedAt:      time.Now(),
		RequestType:    "openai_chat",
	}

	// Populate Tools field directly on LLMRequest so that debate participants
	// and ensemble paths can forward tools without digging into ProviderSpecific.
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			llmReq.Tools = append(llmReq.Tools, models.Tool{
				Type: t.Type,
				Function: models.ToolFunction{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					Parameters:  t.Function.Parameters,
				},
			})
		}
		llmReq.ToolChoice = req.ToolChoice
	}

	return llmReq
}

func (h *UnifiedHandler) processWithEnsemble(ctx context.Context, req *models.LLMRequest, openaiReq *OpenAIChatRequest) (*services.EnsembleResult, error) {
	// Early guards: the handler is often constructed lazily and the
	// provider registry may be nil during boot or in unit tests that
	// exercise the error path. Fail fast with a clear error rather
	// than panicking on req.Tools or any subsequent dereference.
	if h == nil || h.providerRegistry == nil {
		return nil, fmt.Errorf("provider registry not available")
	}
	if req == nil {
		return nil, fmt.Errorf("provider registry not available: nil request")
	}

	// Smart routing: Layer 1 — tools present → direct provider
	if len(req.Tools) > 0 {
		logrus.WithField("tool_count", len(req.Tools)).
			Info("[Smart Routing] Tools detected — routing to direct provider")
		result, err := h.processWithDirectProvider(ctx, req, openaiReq)
		if err == nil {
			return result, nil
		}
		logrus.WithError(err).Warn("[Smart Routing] Direct provider failed, falling back to ensemble")
	}

	// DEBUG: Log entry point
	logrus.WithFields(logrus.Fields{
		"agentic_ensemble_set": h.agenticEnsemble != nil,
		"orchestrator_set":     h.orchestratorIntegration != nil,
		"debate_service_set":   h.debateService != nil,
	}).Debug("[DEBUG] processWithEnsemble called")

	// PRIMARY: AgenticEnsemble (dual-mode: reason + execute with full tool integration)
	if h.agenticEnsemble != nil {
		logrus.Info("[CODE PATH] Using AgenticEnsemble (unified dual-mode)")
		return h.agenticEnsemble.Process(ctx, req)
	}

	// FALLBACK 1: Debate orchestrator (8-phase debate only)
	if h.orchestratorIntegration != nil {
		logrus.Info("[CODE PATH] Using debate orchestrator fallback (processWithOrchestrator)")
		return h.processWithOrchestrator(ctx, req, openaiReq)
	}

	// FALLBACK 2: Legacy ensemble service
	logrus.Error("[CODE PATH] orchestratorIntegration is NIL - using OLD ensemble service (NOT using new debate system!)")

	// Check if provider registry is available
	if h.providerRegistry == nil {
		return nil, services.NewConfigurationError("provider registry not available", nil)
	}

	// Get ensemble service
	ensembleService := h.providerRegistry.GetEnsembleService()
	if ensembleService == nil {
		return nil, services.NewConfigurationError("ensemble service not available", nil)
	}

	// If specific provider requested, try to use it
	if openaiReq.ForceProvider != "" {
		provider, err := h.providerRegistry.GetProvider(openaiReq.ForceProvider)
		if err == nil {
			// Use single provider
			response, err := provider.Complete(ctx, req)
			if err != nil {
				// Categorize the provider error
				return nil, services.CategorizeError(err, openaiReq.ForceProvider)
			}

			// Create ensemble result with single provider
			return &services.EnsembleResult{
				Responses:    []*models.LLMResponse{response},
				Selected:     response,
				VotingMethod: "single_provider",
				Scores:       map[string]float64{response.ID: 1.0},
				Metadata: map[string]any{
					"forced_provider": openaiReq.ForceProvider,
					"total_providers": 1,
				},
			}, nil
		}
	}

	// Use ensemble for best results
	return ensembleService.RunEnsemble(ctx, req)
}

// processWithDirectProvider routes a request to a single provider instead of the
// multi-round debate ensemble. Used for tool-calling requests where deterministic
// single-provider execution is required.
func (h *UnifiedHandler) processWithDirectProvider(
	ctx context.Context,
	req *models.LLMRequest,
	openaiReq *OpenAIChatRequest,
) (*services.EnsembleResult, error) {
	if h.providerRegistry == nil {
		return nil, fmt.Errorf("provider registry not available for direct routing")
	}

	// Try HelixLLM first when configured as primary local inference
	if services.HelixLLMEnabledDefault() { // local-first default: ON unless explicit false (HA-F2-002)
		provider, err := h.providerRegistry.GetProvider(PrimaryProviderName)
		if err == nil {
			response, provErr := provider.Complete(ctx, req)
			if provErr == nil {
				logrus.Info("[Direct Provider] HelixLLM responded successfully")
				return &services.EnsembleResult{
					Responses:    []*models.LLMResponse{response},
					Selected:     response,
					VotingMethod: "direct_provider",
					Scores:       map[string]float64{response.ID: 1.0},
					Metadata: map[string]any{
						"provider":    PrimaryProviderName,
						"route":       "direct",
						"tools_count": len(req.Tools),
					},
				}, nil
			}
			logrus.WithError(provErr).Warn("[Direct Provider] HelixLLM failed, trying cloud providers")
		}
	}

	// Fallback: iterate providers ordered by verification score (highest first)
	providerNames := h.providerRegistry.ListProvidersOrderedByScore()
	for _, name := range providerNames {
		if name == PrimaryProviderName {
			continue // already tried or disabled
		}
		provider, err := h.providerRegistry.GetProvider(name)
		if err != nil {
			continue
		}
		response, err := provider.Complete(ctx, req)
		if err == nil {
			logrus.WithField("provider", name).Info("[Direct Provider] Cloud provider responded")
			return &services.EnsembleResult{
				Responses:    []*models.LLMResponse{response},
				Selected:     response,
				VotingMethod: "direct_provider",
				Scores:       map[string]float64{response.ID: 1.0},
				Metadata: map[string]any{
					"provider":    name,
					"route":       "direct_fallback",
					"tools_count": len(req.Tools),
				},
			}, nil
		}
		logrus.WithError(err).WithField("provider", name).
			Debug("[Direct Provider] Provider failed, trying next")
	}

	return nil, fmt.Errorf("no direct provider available for tool-calling request")
}

// processWithDirectProviderStream routes a streaming request to a single provider
// instead of the debate ensemble. Used for tool-calling requests.
func (h *UnifiedHandler) processWithDirectProviderStream(
	ctx context.Context,
	req *models.LLMRequest,
	openaiReq *OpenAIChatRequest,
) (<-chan *models.LLMResponse, error) {
	if h.providerRegistry == nil {
		return nil, fmt.Errorf("provider registry not available for direct streaming")
	}

	// Try HelixLLM first when configured as primary local inference
	if services.HelixLLMEnabledDefault() { // local-first default: ON unless explicit false (HA-F2-002)
		provider, err := h.providerRegistry.GetProvider(PrimaryProviderName)
		if err == nil {
			streamChan, provErr := provider.CompleteStream(ctx, req)
			if provErr == nil {
				logrus.Info("[Direct Provider Stream] HelixLLM streaming")
				return streamChan, nil
			}
			logrus.WithError(provErr).
				Warn("[Direct Provider Stream] HelixLLM failed, trying cloud providers")
		}
	}

	// Fallback: iterate providers ordered by verification score (highest first)
	providerNames := h.providerRegistry.ListProvidersOrderedByScore()
	for _, name := range providerNames {
		if name == PrimaryProviderName {
			continue
		}
		provider, err := h.providerRegistry.GetProvider(name)
		if err != nil {
			continue
		}
		streamChan, err := provider.CompleteStream(ctx, req)
		if err == nil {
			logrus.WithField("provider", name).
				Info("[Direct Provider Stream] Cloud provider streaming")
			return streamChan, nil
		}
		logrus.WithError(err).WithField("provider", name).
			Debug("[Direct Provider Stream] Provider failed, trying next")
	}

	return nil, fmt.Errorf("no direct provider available for streaming tool-calling request")
}

// processWithHelixLLM routes a non-streaming request directly to HelixLLM for fast local inference
func (h *UnifiedHandler) processWithHelixLLM(c *gin.Context, req *OpenAIChatRequest) {
	if h.providerRegistry == nil {
		h.sendOpenAIError(c, http.StatusServiceUnavailable, "provider_not_available", "HelixLLM provider not available", "provider registry not initialized")
		return
	}

	provider, err := h.providerRegistry.GetProvider(PrimaryProviderName)
	if err != nil {
		logrus.WithError(err).Warn("HelixLLM provider not found")
		h.sendOpenAIError(c, http.StatusServiceUnavailable, "provider_not_found", "HelixLLM provider not configured", err.Error())
		return
	}

	// Convert OpenAI request to internal format
	internalReq := h.convertOpenAIChatRequest(req, c)

	// Call HelixLLM directly
	response, err := provider.Complete(c.Request.Context(), internalReq)
	if err != nil {
		logrus.WithError(err).Warn("HelixLLM request failed")
		h.sendOpenAIError(c, http.StatusServiceUnavailable, "provider_error", "HelixLLM request failed", err.Error())
		return
	}

	// Convert response to OpenAI format
	openAIResp := h.convertSingleResponseToOpenAI(response, req.Model)
	c.JSON(http.StatusOK, openAIResp)
}

// streamWithHelixLLM routes a streaming request directly to HelixLLM
func (h *UnifiedHandler) streamWithHelixLLM(c *gin.Context, req *OpenAIChatRequest) {
	if h.providerRegistry == nil {
		h.sendOpenAIError(c, http.StatusServiceUnavailable, "provider_not_available", "HelixLLM provider not available", "provider registry not initialized")
		return
	}

	provider, err := h.providerRegistry.GetProvider(PrimaryProviderName)
	if err != nil {
		logrus.WithError(err).Warn("HelixLLM provider not found for streaming")
		h.sendOpenAIError(c, http.StatusServiceUnavailable, "provider_not_found", "HelixLLM provider not configured", err.Error())
		return
	}

	// Convert OpenAI request to internal format
	internalReq := h.convertOpenAIChatRequest(req, c)

	// Create streaming response
	streamChan, err := provider.CompleteStream(c.Request.Context(), internalReq)
	if err != nil {
		logrus.WithError(err).Warn("HelixLLM streaming request failed")
		h.sendOpenAIError(c, http.StatusServiceUnavailable, "provider_error", "HelixLLM streaming failed", err.Error())
		return
	}

	// Stream response to client
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	streamID := utils.SecureRandomID("chatcmpl") // D-20: crypto-random, collision-free; one ID per stream, reused across chunks
	for chunk := range streamChan {
		// Convert chunk to SSE format
		sseData := h.convertChunkToSSE(chunk, streamID, req.Model)
		c.Writer.Write([]byte(sseData))
		c.Writer.Flush()
	}
	c.Writer.Write([]byte("data: [DONE]\n\n"))
}

func (h *UnifiedHandler) convertSingleResponseToOpenAI(resp *models.LLMResponse, model string) OpenAIChatResponse {
	// Map internal ToolCalls onto the OpenAI message envelope. Without
	// this, responses with finish_reason="tool_calls" arrive at clients
	// (OpenCode, Claude Code, etc.) advertising a tool call but with an
	// empty/missing tool_calls array — the client knows the model wants
	// a tool but doesn't know which one or with what arguments, and
	// hangs forever waiting for follow-up that never arrives. CONST-032
	// reproduction:
	// challenges/scripts/opencode_tool_call_response_challenge.sh
	// observed this exact shape from helix-llm.
	openAIToolCalls := make([]OpenAIToolCall, 0, len(resp.ToolCalls))
	for _, tc := range resp.ToolCalls {
		// Default to "function" when an upstream omits the type field
		// (some providers return tool_calls with bare {id, function}).
		// OpenAI clients reject tool_calls with empty type as malformed.
		tcType := tc.Type
		if tcType == "" {
			tcType = "function"
		}
		openAIToolCalls = append(openAIToolCalls, OpenAIToolCall{
			ID:   tc.ID,
			Type: tcType,
			Function: OpenAIFunctionCall{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}

	// If the upstream marked finish_reason="tool_calls" but we have no
	// tool calls to surface, reclassify to "stop" so the client doesn't
	// spin waiting for tool-call payloads that don't exist. The
	// alternative — emitting tool_calls with no entries — is the bug
	// we're fixing; the alternative — leaving finish_reason="tool_calls"
	// with no array — is the bug shape OpenCode hangs on.
	finishReason := resp.FinishReason
	if finishReason == "tool_calls" && len(openAIToolCalls) == 0 {
		if strings.TrimSpace(resp.Content) != "" {
			finishReason = "stop"
		} else {
			// No content AND no tool calls: surface a clear error so
			// the client retries / fails loudly instead of hanging.
			finishReason = "error"
		}
	}

	msg := OpenAIMessage{
		Role:    "assistant",
		Content: resp.Content,
	}
	if len(openAIToolCalls) > 0 {
		msg.ToolCalls = openAIToolCalls
	}

	promptTokens, completionTokens, totalTokens := resp.TokenSplit()

	return OpenAIChatResponse{
		ID:                resp.ID,
		Object:            "chat.completion",
		Created:           time.Now().Unix(),
		Model:             model,
		SystemFingerprint: "fp_helixllm_v1",
		Choices: []OpenAIChoice{
			{
				Index:        0,
				Message:      msg,
				FinishReason: finishReason,
			},
		},
		// Report the REAL provider-supplied token split. Previously this
		// synthesised both directions as TokensUsed/2, which discarded
		// the genuine per-direction counts the provider already handed
		// us in resp.Metadata and emitted self-contradicting envelopes
		// on odd totals (measured 2026-09-03: 20 + 20 != 41). See
		// LLMResponse.TokenSplit — unreported directions come back as 0
		// (honest "not reported"), never as an invented half.
		Usage: &OpenAIUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
		},
	}
}

// sseChunkEnvelope mirrors the OpenAI chat.completion.chunk wire shape.
// Defined as a named type (rather than inline anonymous struct) so the
// nested types are reusable + serialize identically across the package.
//
// `delta.tool_calls` and `delta.content` are pointer-typed so that
// json.Marshal omits the field when the chunk doesn't carry that
// signal — text-only chunks emit `{"delta":{"content":"..."}}` with
// no tool_calls key, and tool_call deltas emit
// `{"delta":{"tool_calls":[...]}}` with no content key. This matches
// the OpenAI spec (clients accumulate fields across chunks rather than
// expecting both on every chunk).
type sseChunkEnvelope struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []sseChunkChoice `json:"choices"`
}
type sseChunkChoice struct {
	Index        int           `json:"index"`
	Delta        sseChunkDelta `json:"delta"`
	FinishReason interface{}   `json:"finish_reason"`
}
type sseChunkDelta struct {
	Content   *string                `json:"content,omitempty"`
	ToolCalls []sseChunkToolCallItem `json:"tool_calls,omitempty"`
}
type sseChunkToolCallItem struct {
	Index    int                  `json:"index"`
	ID       string               `json:"id,omitempty"`
	Type     string               `json:"type,omitempty"`
	Function sseChunkToolFunction `json:"function"`
}
type sseChunkToolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// convertChunkToSSE wraps a streaming chunk in the OpenAI SSE envelope.
//
// The original implementation used fmt.Sprintf with chunk.Content
// interpolated directly into a JSON string field — any content
// containing a `"`, `\`, control char, or newline produced malformed
// JSON that broke client parsers (CONST-032 reproduction:
// challenges/scripts/sse_chunk_json_validity_challenge.sh). The
// json.Marshal-based encoder fixed escape semantics.
//
// This version additionally surfaces chunk.ToolCalls into
// delta.tool_calls so streaming clients (OpenCode, Claude Code) can
// see the model invoking a tool — without this, models that stream
// tool_call deltas appear to the client as text-only ("I'll use the
// X tool to do Y") with no actual call ever delivered, and the
// client hangs forever waiting (CONST-032 reproduction:
// challenges/scripts/opencode_streaming_tool_call_challenge.sh).
func (h *UnifiedHandler) convertChunkToSSE(chunk *models.LLMResponse, streamID, model string) string {
	envelope := sseChunkEnvelope{
		ID:      streamID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []sseChunkChoice{{Index: 0}},
	}

	// Always include delta.content (pointer-typed so empty string is
	// distinguishable from "no content field"). Most legacy clients
	// expect a content field on every text chunk even when empty.
	contentCopy := chunk.Content
	envelope.Choices[0].Delta.Content = &contentCopy

	if len(chunk.ToolCalls) > 0 {
		items := make([]sseChunkToolCallItem, 0, len(chunk.ToolCalls))
		for i, tc := range chunk.ToolCalls {
			tcType := tc.Type
			if tcType == "" {
				tcType = "function"
			}
			items = append(items, sseChunkToolCallItem{
				Index: i,
				ID:    tc.ID,
				Type:  tcType,
				Function: sseChunkToolFunction{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
		envelope.Choices[0].Delta.ToolCalls = items
		// When emitting tool_calls and there's no content, drop the
		// content field entirely — OpenAI's wire spec doesn't include
		// content on a pure tool_call delta.
		if chunk.Content == "" {
			envelope.Choices[0].Delta.Content = nil
		}
	}

	b, err := json.Marshal(envelope)
	if err != nil {
		// json.Marshal failure means OOM (only realistic cause for
		// strings + slices). Fall back to a safe empty envelope so we
		// never emit a half-built line.
		return fmt.Sprintf(
			"data: {\"id\":%q,\"object\":\"chat.completion.chunk\",\"created\":%d,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"},\"finish_reason\":null}]}\n\n",
			streamID, time.Now().Unix(), model,
		)
	}
	return "data: " + string(b) + "\n\n"
}

// processWithProviderChain handles helix-llm requests with provider chain and fallback
// Tries: helixllm → best cloud provider → other cloud providers (ordered by score)
func (h *UnifiedHandler) processWithProviderChain(c *gin.Context, req *OpenAIChatRequest) {
	if h.providerRegistry == nil {
		h.sendOpenAIError(c, http.StatusServiceUnavailable, "provider_not_available", "Provider registry not available", "provider registry not initialized")
		return
	}

	internalReq := h.convertOpenAIChatRequest(req, c)

	// Try helixllm first (local inference)
	provider, err := h.providerRegistry.GetProvider(PrimaryProviderName)
	if err == nil {
		response, provErr := provider.Complete(c.Request.Context(), internalReq)
		if provErr == nil {
			logrus.Info("[Provider Chain] helixllm succeeded")
			openAIResp := h.convertSingleResponseToOpenAI(response, req.Model)
			c.JSON(http.StatusOK, openAIResp)
			return
		}
		logrus.WithError(provErr).Warn("[Provider Chain] helixllm failed, trying fallback providers")
	}

	// Fallback: try providers ordered by verification score
	providerNames := h.providerRegistry.ListProvidersOrderedByScore()
	for _, name := range providerNames {
		if name == PrimaryProviderName {
			continue
		}
		provider, err := h.providerRegistry.GetProvider(name)
		if err != nil {
			continue
		}
		response, err := provider.Complete(c.Request.Context(), internalReq)
		if err == nil {
			logrus.WithField("provider", name).Info("[Provider Chain] Fallback provider succeeded")
			openAIResp := h.convertSingleResponseToOpenAI(response, req.Model)
			c.JSON(http.StatusOK, openAIResp)
			return
		}
		logrus.WithError(err).WithField("provider", name).Debug("[Provider Chain] Fallback provider failed")
	}

	h.sendOpenAIError(c, http.StatusServiceUnavailable, "no_provider_available", "All providers failed", "no provider in the chain was able to handle the request")
}

// streamWithProviderChain handles streaming for helix-llm with provider chain and fallback
func (h *UnifiedHandler) streamWithProviderChain(c *gin.Context, req *OpenAIChatRequest) {
	if h.providerRegistry == nil {
		h.sendOpenAIError(c, http.StatusServiceUnavailable, "provider_not_available", "Provider registry not available", "")
		return
	}

	// CONST-032: when stream=true AND tools are present, the upstream
	// fallback chain (deepseek, huggingface, etc.) often does not
	// stream tool_call deltas — clients get text-only chunks even when
	// the model wants to invoke a tool, and hang waiting for tool
	// invocation that never arrives. The non-streaming path
	// (processWithProviderChain) is verified by
	// challenges/scripts/opencode_tool_call_response_challenge.sh to
	// produce a complete tool_calls envelope. Route stream+tools
	// requests through it and wrap the result in a single SSE frame
	// so the client sees a proper tool envelope.
	if len(req.Tools) > 0 {
		logrus.WithField("tool_count", len(req.Tools)).Info(
			"[Provider Chain Stream] tools present — routing through non-streaming path then SSE-wrapping")
		h.streamToolCallViaNonStreaming(c, req)
		return
	}

	internalReq := h.convertOpenAIChatRequest(req, c)
	streamID := utils.SecureRandomID("chatcmpl") // D-20: crypto-random, collision-free; one ID per stream, reused across chunks

	// Try helixllm first
	provider, err := h.providerRegistry.GetProvider(PrimaryProviderName)
	if err == nil {
		streamChan, provErr := provider.CompleteStream(c.Request.Context(), internalReq)
		if provErr == nil {
			logrus.Info("[Provider Chain Stream] helixllm streaming")
			if h.tryStreamWithContentCheck(c, streamChan, streamID, req.Model, PrimaryProviderName) {
				return
			}
			logrus.Warn("[Provider Chain Stream] helixllm produced no content — falling through")
		} else {
			logrus.WithError(provErr).Warn("[Provider Chain Stream] helixllm failed")
		}
	}

	// Fallback: try providers by score
	providerNames := h.providerRegistry.ListProvidersOrderedByScore()
	for _, name := range providerNames {
		if name == PrimaryProviderName {
			continue
		}
		provider, err := h.providerRegistry.GetProvider(name)
		if err != nil {
			continue
		}
		streamChan, err := provider.CompleteStream(c.Request.Context(), internalReq)
		if err != nil {
			continue
		}
		logrus.WithField("provider", name).Info("[Provider Chain Stream] Fallback streaming")
		if h.tryStreamWithContentCheck(c, streamChan, streamID, req.Model, name) {
			return
		}
		logrus.WithField("provider", name).Warn(
			"[Provider Chain Stream] produced no content — trying next provider")
	}

	h.sendOpenAIError(c, http.StatusServiceUnavailable, "no_provider_available", "All streaming providers failed", "")
}

// streamToolCallViaNonStreaming runs the request through the
// non-streaming provider chain (which correctly preserves tool_calls
// in the response — see convertSingleResponseToOpenAI) and frames the
// result as a single SSE chunk + [DONE]. Used when the client asked
// for streaming but the request carries `tools` — the cloud providers
// in our chain often don't stream tool_call deltas, so this guarantees
// the client receives a complete tool envelope instead of text-only
// chunks that talk ABOUT calling the tool.
func (h *UnifiedHandler) streamToolCallViaNonStreaming(c *gin.Context, req *OpenAIChatRequest) {
	internalReq := h.convertOpenAIChatRequest(req, c)
	streamID := utils.SecureRandomID("chatcmpl") // D-20: crypto-random, collision-free; one ID per stream, reused across chunks

	// Try helixllm first, then fallback chain — same priority as
	// non-streaming processWithProviderChain.
	var resp *models.LLMResponse
	tryProvider := func(name string) bool {
		provider, err := h.providerRegistry.GetProvider(name)
		if err != nil {
			logrus.WithError(err).WithField("provider", name).
				Warn("[Provider Chain Stream/Tools] GetProvider failed — provider not registered")
			return false
		}
		r, err := provider.Complete(c.Request.Context(), internalReq)
		if err != nil {
			// Bumped from Debug → Warn so operators can see WHY each
			// provider rejected the request. Without this every silent
			// failure rolls up into a generic "All providers failed"
			// 503 with no diagnostic information.
			logrus.WithError(err).WithField("provider", name).
				Warn("[Provider Chain Stream/Tools] provider failed")
			return false
		}
		resp = r
		logrus.WithField("provider", name).
			Info("[Provider Chain Stream/Tools] provider succeeded")
		return true
	}

	if !tryProvider(PrimaryProviderName) {
		for _, name := range h.providerRegistry.ListProvidersOrderedByScore() {
			if name == PrimaryProviderName {
				continue
			}
			if tryProvider(name) {
				break
			}
		}
	}

	if resp == nil {
		h.sendOpenAIError(c, http.StatusServiceUnavailable,
			"no_provider_available", "All providers failed for tool-call request", "")
		return
	}

	// Frame as SSE: emit one chunk carrying tool_calls (and content if
	// any), then a final chunk with finish_reason, then [DONE].
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	chunk := h.convertChunkToSSE(resp, streamID, req.Model)
	c.Writer.Write([]byte(chunk))
	c.Writer.Flush()

	// Final chunk with finish_reason — clients accumulate tool_calls
	// then this signals the end of message construction. Use the same
	// reclassification logic as convertSingleResponseToOpenAI: if
	// finish_reason claims tool_calls but none are present, reclassify
	// to stop/error so the client never sees the legacy hang shape.
	finishReason := resp.FinishReason
	if finishReason == "" {
		finishReason = "stop"
	}
	if finishReason == "tool_calls" && len(resp.ToolCalls) == 0 {
		if strings.TrimSpace(resp.Content) != "" {
			finishReason = "stop"
		} else {
			finishReason = "error"
		}
	}
	finalEnvelope := sseChunkEnvelope{
		ID:      streamID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []sseChunkChoice{{Index: 0, FinishReason: finishReason}},
	}
	if b, err := json.Marshal(finalEnvelope); err == nil {
		c.Writer.Write([]byte("data: " + string(b) + "\n\n"))
		c.Writer.Flush()
	}

	c.Writer.Write([]byte("data: [DONE]\n\n"))
	c.Writer.Flush()
}

// tryStreamWithContentCheck buffers the first chunk to verify the upstream
// produced ANY non-empty content before committing to that provider.
// Returns true if real content was streamed to the client (provider
// "won"); false if the upstream closed without content or with only
// empty chunks (caller should try the next provider in the chain).
//
// Without this guard, providers like Hugging Face that occasionally
// return a single empty content chunk on cold start propagate to the
// client as "empty SSE response" — which the user sees as the CLI
// hanging or showing no answer (Finding #50, reproduced by
// challenges/scripts/opencode_helixllm_hello_challenge.sh).
//
// The guard waits up to 5s for the first chunk. If the first chunk has
// non-empty content, headers + chunks are streamed normally. If the
// upstream closes with everything empty, no headers / no DONE marker
// are sent (the response is uncommitted) and the caller can try the
// next provider.
func (h *UnifiedHandler) tryStreamWithContentCheck(
	c *gin.Context,
	streamChan <-chan *models.LLMResponse,
	streamID, model, providerName string,
) bool {
	// Drain leading empty chunks until we find content OR the channel
	// closes / timeout expires.
	firstContent, ok := h.peekFirstContent(streamChan, c.Request.Context())
	if !ok {
		return false
	}

	// Real content exists — commit headers + replay first chunk + drain.
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	c.Writer.Write([]byte(h.convertChunkToSSE(firstContent, streamID, model)))
	c.Writer.Flush()

	for chunk := range streamChan {
		sseData := h.convertChunkToSSE(chunk, streamID, model)
		c.Writer.Write([]byte(sseData))
		c.Writer.Flush()
	}
	c.Writer.Write([]byte("data: [DONE]\n\n"))
	c.Writer.Flush()
	return true
}

// peekFirstContent reads chunks from streamChan, skipping any with
// empty signal, until it finds one with REAL content (text OR
// tool_calls) and returns that chunk + true. If the channel closes or
// the 5s timeout fires before any meaningful chunk, returns nil + false
// (caller should try the next provider — current chunk source produced
// no useful output).
//
// Tool-call chunks count as "real signal" too — without this, models
// that stream pure tool-invocation deltas (no preamble text) are
// treated as empty by the fall-through logic and routed away from a
// perfectly working provider.
func (h *UnifiedHandler) peekFirstContent(
	streamChan <-chan *models.LLMResponse,
	ctx context.Context,
) (*models.LLMResponse, bool) {
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case chunk, open := <-streamChan:
			if !open {
				return nil, false
			}
			if chunk == nil {
				continue
			}
			if strings.TrimSpace(chunk.Content) != "" {
				return chunk, true
			}
			if len(chunk.ToolCalls) > 0 {
				return chunk, true
			}
		case <-timeout.C:
			return nil, false
		case <-ctx.Done():
			return nil, false
		}
	}
}

// streamResponse helper to stream SSE response
func (h *UnifiedHandler) streamResponse(c *gin.Context, streamChan <-chan *models.LLMResponse, streamID, model string) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	for chunk := range streamChan {
		sseData := h.convertChunkToSSE(chunk, streamID, model)
		c.Writer.Write([]byte(sseData))
		c.Writer.Flush()
	}
	c.Writer.Write([]byte("data: [DONE]\n\n"))
}

func (h *UnifiedHandler) processWithEnsembleStream(ctx context.Context, req *models.LLMRequest, openaiReq *OpenAIChatRequest) (<-chan *models.LLMResponse, error) {
	// Early guards: same nil-safety as processWithEnsemble so a handler
	// constructed without a provider registry (unit tests, early boot)
	// cannot panic on req.Tools.
	if h == nil || h.providerRegistry == nil {
		return nil, fmt.Errorf("provider registry not available")
	}
	if req == nil {
		return nil, fmt.Errorf("provider registry not available: nil request")
	}

	// Smart routing: Layer 1 — tools present → direct provider stream
	if len(req.Tools) > 0 {
		logrus.WithField("tool_count", len(req.Tools)).
			Info("[Smart Routing] Tools detected — routing to direct provider stream")
		streamChan, err := h.processWithDirectProviderStream(ctx, req, openaiReq)
		if err == nil {
			return streamChan, nil
		}
		logrus.WithError(err).Warn("[Smart Routing] Direct provider stream failed, falling back to ensemble")
	}

	// Check if comprehensive debate streaming is available
	if h.debateService != nil && h.debateService.IsComprehensiveSystemEnabled() {
		logrus.Info("[Comprehensive Streaming] Using comprehensive multi-agent debate system")
		return h.processWithComprehensiveStream(ctx, req, openaiReq)
	}

	// CRITICAL: Use the verified debate team's strongest provider for streaming.
	// The old ensemble service uses weak/random providers — we must use the
	// score-sorted debate team to pick the strongest available model.
	if h.debateTeamConfig != nil {
		// Get the strongest provider from Position 1 (highest score)
		for pos := services.PositionAnalyst; pos <= services.PositionMediator; pos++ {
			provider, model, err := h.debateTeamConfig.GetProviderForPosition(pos)
			if err != nil || provider == nil {
				continue
			}

			logrus.WithFields(logrus.Fields{
				"position": pos,
				"model":    model,
			}).Info("[Streaming] Using verified debate team provider")

			// Set the model on the request
			if model != "" {
				req.ModelParams.Model = model
			}

			// Use the verified provider for streaming
			streamChan, err := provider.CompleteStream(ctx, req)
			if err == nil {
				return streamChan, nil
			}
			logrus.WithError(err).WithField("model", model).Warn("[Streaming] Verified provider failed, trying next position")
		}
		logrus.Warn("[Streaming] All debate team positions failed, falling back to ensemble")
	}

	// Fallback to ensemble service
	if h.providerRegistry == nil {
		return nil, services.NewConfigurationError("provider registry not available", nil)
	}

	ensembleService := h.providerRegistry.GetEnsembleService()
	if ensembleService == nil {
		return nil, services.NewConfigurationError("ensemble service not available", nil)
	}

	return ensembleService.RunEnsembleStream(ctx, req)
}

// processWithComprehensiveStream uses the comprehensive multi-agent debate system for streaming
func (h *UnifiedHandler) processWithComprehensiveStream(ctx context.Context, req *models.LLMRequest, openaiReq *OpenAIChatRequest) (<-chan *models.LLMResponse, error) {
	streamChan := make(chan *models.LLMResponse, 100)

	go func() {
		defer close(streamChan)

		// Extract topic from request — use prompt or last user message
		topic := req.Prompt
		if topic == "" {
			for _, msg := range req.Messages {
				if msg.Role == "user" {
					topic = msg.Content
				}
			}
		}
		if topic == "" {
			topic = "User Query"
		}

		debateConfig := services.DebateConfig{
			DebateID:  utils.SecureRandomID("comp"), // D-20: crypto-random, collision-free
			Topic:     topic,
			MaxRounds: 3,
			Metadata:  make(map[string]any),
		}

		// Create streaming handler that forwards to channel
		streamHandler := func(event *comprehensive.StreamEvent) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case streamChan <- h.convertStreamEventToLLMResponse(event):
				return nil
			}
		}

		// Run comprehensive streaming debate
		result, err := h.debateService.StreamDebate(ctx, &debateConfig, streamHandler)
		if err != nil {
			logrus.WithError(err).Error("[Comprehensive Streaming] Debate failed")
			return
		}

		// Send final result
		if result != nil && result.Consensus != nil {
			select {
			case <-ctx.Done():
				return
			case streamChan <- &models.LLMResponse{
				ID:           debateConfig.DebateID,
				Content:      result.Consensus.Summary,
				Confidence:   result.Consensus.Confidence,
				FinishReason: "stop",
				CreatedAt:    time.Now(),
				Metadata:     result.Metadata,
			}:
			}
		}
	}()

	return streamChan, nil
}

// convertStreamEventToLLMResponse converts a comprehensive stream event to LLM response
func (h *UnifiedHandler) convertStreamEventToLLMResponse(event *comprehensive.StreamEvent) *models.LLMResponse {
	content := ""
	if event.Agent != nil {
		content = fmt.Sprintf("[%s:%s] ", event.Team, event.Agent.Role)
	}
	content += event.Content

	return &models.LLMResponse{
		ID:        event.DebateID,
		Content:   content,
		CreatedAt: event.Timestamp,
		Metadata: map[string]any{
			"event_type": event.Type,
			"phase":      event.Phase,
			"team":       event.Team,
			"progress":   event.Progress,
			"agent_id":   event.Agent,
		},
	}
}

// processWithOrchestrator uses the debate service with configured debate team
// This leverages the working debate team (25 LLMs from verified providers)
func (h *UnifiedHandler) processWithOrchestrator(ctx context.Context, req *models.LLMRequest, openaiReq *OpenAIChatRequest) (*services.EnsembleResult, error) {
	logrus.WithFields(logrus.Fields{
		"orchestrator_integration": h.orchestratorIntegration != nil,
		"debate_service":           h.debateService != nil,
	}).Info("[CODE PATH] Entered processWithOrchestrator")

	// First try the NEW orchestrator if available
	if h.orchestratorIntegration != nil {
		logrus.Info("[CODE PATH] Attempting NEW orchestrator (8-phase protocol)")
		// Extract topic from request messages (OpenCode sends messages, not prompt)
		orchTopic := req.Prompt
		if orchTopic == "" {
			for _, msg := range req.Messages {
				if msg.Role == "user" {
					orchTopic = msg.Content
				}
			}
		}
		if orchTopic == "" {
			orchTopic = "User Query"
		}

		debateReq := &orchestrator.DebateRequest{
			Topic:        orchTopic,
			MaxRounds:    2,
			MinConsensus: 0.6,
		}

		// Issue #43: the 8-phase NEW orchestrator regularly hangs for ~60s
		// before failing, blowing through client curl/fetch default timeouts.
		// Bound it to 20s so the DebateService fallback has budget to
		// produce a real response within a reasonable client wall-clock.
		orchCtx, orchCancel := context.WithTimeout(ctx, 20*time.Second)
		debateResp, err := h.orchestratorIntegration.GetOrchestrator().ConductDebate(orchCtx, debateReq)
		orchCancel()
		if err == nil && debateResp != nil {
			// Extract the consensus response from the debate
			var finalContent string
			var confidence float64
			if debateResp.Consensus != nil {
				finalContent = debateResp.Consensus.Summary
				confidence = debateResp.Consensus.Confidence
			}

			// Check if consensus content is actually an error message
			if isErrorContent(finalContent) {
				finalContent = ""
			}

			// If no good consensus, extract from phase responses (last phase first)
			if finalContent == "" && len(debateResp.Phases) > 0 {
				for i := len(debateResp.Phases) - 1; i >= 0; i-- {
					for _, resp := range debateResp.Phases[i].Responses {
						if resp.Content != "" && !isErrorContent(resp.Content) {
							finalContent = resp.Content
							if confidence == 0 {
								confidence = resp.Confidence
							}
							break
						}
					}
					if finalContent != "" {
						break
					}
				}
			}

			// Refuse to return 200 OK with empty message.content from the
			// orchestrator path too — matches the debate-service path at
			// the bottom of this function. A 200 + empty body is
			// indistinguishable from a silent failure to every OpenAI
			// client downstream.
			if strings.TrimSpace(finalContent) == "" {
				logrus.WithFields(logrus.Fields{
					"phases":       len(debateResp.Phases),
					"participants": len(debateResp.Participants),
				}).Warn("[CODE PATH] NEW orchestrator returned empty content — surfacing as provider error")
				return nil, fmt.Errorf(
					"debate orchestrator produced no content (phases=%d, participants=%d) — all providers failed or returned empty",
					len(debateResp.Phases),
					len(debateResp.Participants),
				)
			}

			logrus.WithFields(logrus.Fields{
				"phases":       len(debateResp.Phases),
				"participants": len(debateResp.Participants),
				"content_len":  len(finalContent),
			}).Info("[CODE PATH] NEW orchestrator SUCCEEDED - returning result")

			finalResponse := &models.LLMResponse{
				ID:           debateResp.ID,
				Content:      finalContent,
				Confidence:   confidence,
				FinishReason: "stop",
				CreatedAt:    time.Now(),
			}

			return &services.EnsembleResult{
				Responses:    []*models.LLMResponse{finalResponse},
				Selected:     finalResponse,
				VotingMethod: "new_orchestrator_debate",
				Scores:       map[string]float64{"consensus": confidence},
				Metadata: map[string]any{
					"framework":     "new_debate_orchestrator",
					"phases":        len(debateResp.Phases),
					"participants":  len(debateResp.Participants),
					"documentation": "docs/requests/debate",
					"duration_ms":   debateResp.Duration.Milliseconds(),
				},
			}, nil
		}
		logrus.WithError(err).Warn("[CODE PATH] NEW orchestrator FAILED, trying debate service")
	}

	// Use the debate service with configured debate team (25 verified LLMs)
	// This is the working system that uses the debate team config
	if h.debateService == nil {
		logrus.Error("[CODE PATH] Debate service NOT AVAILABLE - using OLD ensemble (this is the bug!)")
		ensembleService := h.providerRegistry.GetEnsembleService()
		return ensembleService.RunEnsemble(ctx, req)
	}

	logrus.Info("[CODE PATH] Using DebateService with configured team (25 LLMs)")

	// Get participants from the verified debate team — NOT empty!
	// This ensures the debate uses the highest-scored, verified LLMs
	var participants []services.ParticipantConfig
	if h.debateTeamConfig != nil {
		participants = h.debateTeamConfig.GetParticipantConfigs()
		logrus.WithField("participant_count", len(participants)).Info("[CODE PATH] Populated participants from verified debate team")
	}
	if len(participants) == 0 {
		logrus.Warn("[CODE PATH] No participants from debate team config — debate service will use its own selection")
	}

	// Extract topic from request — use prompt or last user message
	topic := req.Prompt
	if topic == "" {
		// Get topic from last user message in the conversation
		for _, msg := range req.Messages {
			if msg.Role == "user" {
				topic = msg.Content
			}
		}
	}
	if topic == "" {
		topic = "User Query"
	}

	// Configure debate with the verified team participants.
	// Issue #43 continuation: pass source=openai_compatible metadata so
	// DebateService.ConductDebate bypasses the orphaned Comprehensive
	// stub (debate_service.go:697) and routes to conductRealDebate — the
	// same path the streaming handler has always used successfully.
	// Timeout must be > 0 — conductRealDebate calls
	// context.WithTimeout(ctx, config.Timeout), which immediately cancels
	// if Timeout is the zero value, producing an empty result set.
	//
	// Pack system prompt + prior turns into metadata.system_context so
	// conductRealDebate's executeRound (debate_service.go:1222) splices
	// them into each participant's system prompt. Without this the
	// debate sees only the last user message and loses multi-turn
	// context (multi_turn assertions in curl_api_testing_challenge
	// regressed because "My name is Alice" + "What is my name?"
	// collapsed to just "What is my name?"). executeRound currently
	// only consumes "system_context" from metadata — conversation
	// history must be packed into that same string so the participants
	// see it.
	debateMetadata := map[string]any{"source": "openai_compatible"}
	var combinedSystem strings.Builder
	for _, m := range req.Messages {
		if m.Role == "system" {
			content := m.Content
			if len(content) > 2000 {
				content = content[:2000] + "\n...[truncated]"
			}
			combinedSystem.WriteString(content + "\n")
		}
	}
	// Append prior conversation turns so the multi-turn context travels
	// with the system prompt. The final user-role message stays as the
	// Topic — the system prompt describes the *prior* dialogue only.
	hasPriorConversation := false
	for i, m := range req.Messages {
		if m.Role == "user" || m.Role == "assistant" {
			// Skip the very last user message — it is the Topic.
			if i == len(req.Messages)-1 && m.Role == "user" {
				continue
			}
			if !hasPriorConversation {
				combinedSystem.WriteString("\nPRIOR CONVERSATION (honor this context when answering):\n")
				hasPriorConversation = true
			}
			combinedSystem.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, m.Content))
		}
	}
	if combinedSystem.Len() > 0 {
		debateMetadata["system_context"] = combinedSystem.String()
	}

	debateConfig := services.DebateConfig{
		DebateID:     utils.SecureRandomID("debate"), // D-20: crypto-random, collision-free
		Topic:        topic,
		MaxRounds:    1,
		Timeout:      120 * time.Second,
		EnableCognee: false,
		Strategy:     "collaborative",
		Participants: participants,
		Tools:        req.Tools,
		ToolChoice:   req.ToolChoice,
		Metadata:     debateMetadata,
	}

	// Run the debate using the debate service (uses the configured 25 LLM team)
	debateResult, err := h.debateService.AutoConductDebate(ctx, &debateConfig)
	if err != nil {
		logrus.WithError(err).Error("[CODE PATH] Debate service FAILED - falling back to OLD ensemble")
		ensembleService := h.providerRegistry.GetEnsembleService()
		return ensembleService.RunEnsemble(ctx, req)
	}

	logrus.Info("[CODE PATH] DebateService SUCCEEDED - returning result")

	// Convert debate result to ensemble result.
	// Priority: real participant content > consensus summary.
	// The consensus.Summary field is formulaic ("Debate on 'X' with N responses")
	// and useless as a user-facing answer. Only fall back to it when every
	// participant produced an empty body, which is itself an anomaly worth
	// surfacing to the client.
	var finalContent string
	var confidence float64

	if debateResult.BestResponse != nil {
		finalContent = debateResult.BestResponse.Content
		if finalContent == "" {
			finalContent = debateResult.BestResponse.Response
		}
		confidence = debateResult.BestResponse.Confidence
	}
	if finalContent == "" && len(debateResult.AllResponses) > 0 {
		// Scan every response for the first non-empty body — prior code only
		// read index 0, which would be empty whenever the strongest provider
		// failed first and only weaker providers produced content.
		for i := range debateResult.AllResponses {
			r := debateResult.AllResponses[i]
			candidate := r.Content
			if candidate == "" {
				candidate = r.Response
			}
			if candidate != "" {
				finalContent = candidate
				confidence = r.Confidence
				break
			}
		}
	}
	if finalContent == "" && debateResult.Consensus != nil && debateResult.Consensus.Summary != "" {
		// Last resort — formulaic consensus summary is better than empty.
		finalContent = debateResult.Consensus.Summary
		confidence = debateResult.Consensus.Confidence
	}

	if confidence == 0 {
		confidence = debateResult.QualityScore
	}

	// Refuse to return a 200 OK with an empty content body. Every upstream
	// client (OpenCode, curl, OpenAI SDKs) treats an empty message.content
	// as a successful but useless completion, which masks provider failures
	// behind a false-positive. Returning an error here lets the client's
	// transient-failure / retry path fire.
	if strings.TrimSpace(finalContent) == "" {
		return nil, fmt.Errorf(
			"debate ensemble produced no content (participants=%d, responses=%d, rounds=%d) — all providers failed or returned empty",
			len(debateResult.Participants),
			len(debateResult.AllResponses),
			debateResult.TotalRounds,
		)
	}

	// Report the usage the debate really consumed. Without this the envelope
	// carried {0,0,0} beside a 200 on this path — the exact all-zero shape
	// that cannot be distinguished from "no model ran". The counts are SUMMED
	// from what each participant's provider reported (never derived, never
	// halved); the per-direction split is published only when it accounts for
	// the whole total, otherwise the total alone is reported.
	debatePrompt, debateCompletion, debateTotal := services.DebateTokenTotals(debateResult)
	finalResponse := &models.LLMResponse{
		ID:           debateResult.DebateID,
		Content:      finalContent,
		Confidence:   confidence,
		FinishReason: "stop",
		CreatedAt:    time.Now(),
		TokensUsed:   debateTotal,
		Metadata:     services.DebateUsageMetadata(debatePrompt, debateCompletion, debateTotal),
	}

	return &services.EnsembleResult{
		Responses:    []*models.LLMResponse{finalResponse},
		Selected:     finalResponse,
		VotingMethod: "debate_service_team",
		Scores:       map[string]float64{"debate_confidence": confidence},
		Metadata: map[string]any{
			"framework":     "debate_service_with_verified_team",
			"rounds":        debateResult.TotalRounds,
			"participants":  len(debateResult.Participants),
			"duration_ms":   debateResult.Duration.Milliseconds(),
			"team_size":     25,
			"documentation": "docs/requests/debate",
		},
	}, nil
}

// isErrorContent returns true if the content looks like an error message rather
// than a real LLM response. This handles cases where the consensus summary
// contains a provider error (e.g. "provider not found") instead of content.
func isErrorContent(content string) bool {
	if content == "" {
		return false
	}
	return strings.HasPrefix(content, "Error:") ||
		strings.HasPrefix(content, "error:") ||
		strings.Contains(content, "provider not found") ||
		strings.Contains(content, "failed to get provider") ||
		strings.Contains(content, "provider call failed")
}

func (h *UnifiedHandler) convertToOpenAIChatResponse(result *services.EnsembleResult, req *OpenAIChatRequest) *OpenAIChatResponse {
	selected := result.Selected

	// Convert usage information. Reports the REAL provider-supplied
	// split via LLMResponse.TokenSplit rather than halving the total
	// into both directions (which fabricated telemetry and could not
	// even sum correctly on odd totals). Unreported directions are 0.
	promptTokens, completionTokens, totalTokens := selected.TokenSplit()
	usage := &OpenAIUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
	}

	// Create choice
	choice := OpenAIChoice{
		Index: 0,
		Message: OpenAIMessage{
			Role:    "assistant",
			Content: selected.Content,
		},
		FinishReason: selected.FinishReason,
	}

	resp := &OpenAIChatResponse{
		ID:                selected.ID,
		Object:            "chat.completion",
		Created:           selected.CreatedAt.Unix(),
		Model:             "helixagent-ensemble", // Always show ensemble model
		Choices:           []OpenAIChoice{choice},
		Usage:             usage,
		SystemFingerprint: "fp_helixagent_ensemble",
	}

	// Surface agentic-ensemble provenance (mode / stages_completed /
	// agents_spawned / per-subagent results) so callers get positive
	// runtime evidence that the decompose→execute engine actually ran.
	// Without this the engine could run yet the user-visible response
	// would carry no proof (a §11.4.108 SOURCE→USER-VISIBLE gap).
	if result.Metadata != nil {
		if am, ok := result.Metadata["agentic"]; ok && am != nil {
			resp.Agentic = am
		}
	}

	return resp
}

// convertToOpenAIChatStreamResponse converts an LLM response to OpenAI streaming format
// isFirstChunk: true for the first chunk (includes role), false for subsequent chunks
// streamID: consistent ID across all chunks in the stream
func (h *UnifiedHandler) convertToOpenAIChatStreamResponse(resp *models.LLMResponse, req *OpenAIChatRequest, isFirstChunk bool, streamID string) map[string]any {
	// Build delta based on chunk type
	// Per OpenAI spec: role only in first chunk, content in subsequent chunks
	delta := map[string]any{}

	if isFirstChunk {
		// First chunk: include role, empty content
		delta["role"] = "assistant"
		delta["content"] = ""
	} else if resp.FinishReason != "" && resp.Content == "" {
		// Final chunk with NO content: empty delta (finish_reason indicates completion)
		// IMPORTANT: Only empty delta if there's no content to send
	} else if resp.FinishReason != "" && resp.Content != "" {
		// Final chunk WITH content: include both content AND finish_reason
		// CRITICAL FIX: Don't lose content when finish_reason is present!
		delta["content"] = resp.Content
	} else {
		// Content chunk: only include content (no role)
		delta["content"] = resp.Content
	}

	// Build choice with proper finish_reason
	// Critical for OpenCode/Crush/HelixCode compatibility
	choice := map[string]any{
		"index":    0,
		"delta":    delta,
		"logprobs": nil, // Required field per OpenAI spec
	}

	// finish_reason: null for intermediate, "stop" for final
	if resp.FinishReason != "" {
		choice["finish_reason"] = resp.FinishReason
	} else {
		choice["finish_reason"] = nil
	}

	// Use consistent stream ID across all chunks
	id := streamID
	if id == "" {
		id = resp.ID
	}

	return map[string]any{
		"id":                 id,
		"object":             "chat.completion.chunk",
		"created":            resp.CreatedAt.Unix(),
		"model":              "helixagent-ensemble",
		"system_fingerprint": "fp_helixagent_v1",
		"choices":            []map[string]any{choice},
	}
}

func (h *UnifiedHandler) sendOpenAIError(c *gin.Context, statusCode int, errorType, message, details string) {
	errorResp := map[string]any{
		"error": map[string]any{
			"message": fmt.Sprintf("%s: %s", message, details),
			"type":    errorType,
			"code":    statusCode,
		},
	}

	c.JSON(statusCode, errorResp)
}

// sendCategorizedError handles LLMServiceError with proper HTTP status codes
func (h *UnifiedHandler) sendCategorizedError(c *gin.Context, err error) {
	// Check if it's a categorized LLM service error
	if llmErr, ok := err.(*services.LLMServiceError); ok {
		response := llmErr.ToOpenAIError()

		// Add retry-after header if applicable
		if llmErr.RetryAfter > 0 {
			c.Header("Retry-After", fmt.Sprintf("%d", int(llmErr.RetryAfter.Seconds())))
		}

		c.JSON(llmErr.HTTPStatus, response)
		return
	}

	// Categorize unknown errors
	categorized := services.CategorizeError(err, "unknown")
	if categorized != nil {
		response := categorized.ToOpenAIError()
		if categorized.RetryAfter > 0 {
			c.Header("Retry-After", fmt.Sprintf("%d", int(categorized.RetryAfter.Seconds())))
		}
		c.JSON(categorized.HTTPStatus, response)
		return
	}

	// Fallback to generic 500 error
	h.sendOpenAIError(c, http.StatusInternalServerError, "internal_error", "An unexpected error occurred", err.Error())
}

// expandFollowUpResponse detects short follow-up responses (like "yes 1", "1", "ok", "yes")
// and expands them with context from the previous assistant message that offered options.
// This ensures AI Debate understands "yes 1." means "execute option 1 from your previous response".
//
// CRITICAL: This function uses Cognee-stored conversation history via the messages array.
// The conversation context from Cognee is automatically included in req.Messages by the client.
func (h *UnifiedHandler) expandFollowUpResponse(userMessage string, messages []OpenAIMessage) string {
	// Normalize user message
	normalized := strings.TrimSpace(strings.ToLower(userMessage))

	// Check if this looks like a short follow-up response
	// Patterns: "yes 1", "1", "1.", "yes", "ok", "ok 1", "sure 1", "do 1", "yes, 1", "option 1", etc.
	isShortFollowUp := false
	selectedOption := 0

	// Very short message (under 20 chars) that contains a number or affirmative
	// Must be VERY short to avoid false positives on questions like "What does the auth module do?"
	if len(normalized) < 20 {
		// Check for affirmative patterns - use word boundaries to avoid false matches
		// "do" alone is affirmative, but "do?" as part of a question is not
		affirmatives := []string{"yes", "ok", "okay", "sure", "please", "go", "proceed", "yep", "yeah", "y", "1", "2", "3", "4", "5"}
		for _, aff := range affirmatives {
			// Check for exact match or word at start/end
			if normalized == aff ||
				strings.HasPrefix(normalized, aff+" ") ||
				strings.HasPrefix(normalized, aff+",") ||
				strings.HasPrefix(normalized, aff+".") ||
				strings.HasSuffix(normalized, " "+aff) ||
				strings.HasSuffix(normalized, ","+aff) {
				isShortFollowUp = true
				break
			}
		}

		// Also check for "do" specifically but only as standalone word
		if !isShortFollowUp && (normalized == "do" || strings.HasPrefix(normalized, "do ")) {
			isShortFollowUp = true
		}

		// Extract option number if present
		for i := 1; i <= 9; i++ {
			numStr := fmt.Sprintf("%d", i)
			if strings.Contains(normalized, numStr) {
				selectedOption = i
				break
			}
		}
	}

	if !isShortFollowUp {
		return "" // Not a follow-up, use original message
	}

	// Find the most recent assistant message that contains numbered options
	var previousAssistantMessage string
	var previousContext string

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == "assistant" && msg.Content != "" {
			// Check if this message contains numbered options
			// Support both "1." and "1)" formats
			hasOptions := (strings.Contains(msg.Content, "1.") || strings.Contains(msg.Content, "1)")) &&
				(strings.Contains(msg.Content, "2.") || strings.Contains(msg.Content, "2)") ||
					strings.Contains(msg.Content, "Would you like"))

			if hasOptions {
				previousAssistantMessage = msg.Content
				break
			}

			// Also capture recent assistant context even without numbered options
			if previousContext == "" {
				previousContext = msg.Content
			}
		}
	}

	// If we found a message with options, expand the user's response
	if previousAssistantMessage != "" {
		// Try to extract the specific option the user selected
		var selectedOptionText string
		if selectedOption > 0 {
			// Parse the options from the previous message
			// Look for patterns like "1. Create an AGENTS.md" or "1) Create an AGENTS.md"
			lines := strings.Split(previousAssistantMessage, "\n")
			optionPrefix := fmt.Sprintf("%d.", selectedOption)
			optionPrefixAlt := fmt.Sprintf("%d)", selectedOption)

			for _, line := range lines {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, optionPrefix) || strings.HasPrefix(trimmed, optionPrefixAlt) {
					selectedOptionText = strings.TrimPrefix(trimmed, optionPrefix)
					selectedOptionText = strings.TrimPrefix(selectedOptionText, optionPrefixAlt)
					selectedOptionText = strings.TrimSpace(selectedOptionText)
					break
				}
			}
		}

		// Build expanded context
		var expanded strings.Builder
		expanded.WriteString("The user is responding to options I previously offered.\n\n")
		expanded.WriteString("PREVIOUS CONTEXT (what I offered):\n")

		// Include relevant part of previous message (last 500 chars or from "Would you like")
		relevantPart := previousAssistantMessage
		if idx := strings.LastIndex(previousAssistantMessage, "Would you like"); idx != -1 {
			relevantPart = previousAssistantMessage[idx:]
		} else if len(previousAssistantMessage) > 500 {
			relevantPart = "..." + previousAssistantMessage[len(previousAssistantMessage)-500:]
		}
		expanded.WriteString(relevantPart)
		expanded.WriteString("\n\n")

		expanded.WriteString("USER'S RESPONSE: ")
		expanded.WriteString(userMessage)
		expanded.WriteString("\n\n")

		if selectedOptionText != "" {
			expanded.WriteString("INTERPRETATION: The user selected option ")
			expanded.WriteString(fmt.Sprintf("%d", selectedOption))
			expanded.WriteString(": ")
			expanded.WriteString(selectedOptionText)
			expanded.WriteString("\n\n")
			expanded.WriteString("ACTION REQUIRED: Execute the selected option. Do NOT start a new discussion about the option - PERFORM the action.")
		} else {
			expanded.WriteString("INTERPRETATION: The user is confirming/agreeing with the previous suggestions.\n")
			expanded.WriteString("ACTION REQUIRED: Execute the most appropriate action based on context.")
		}

		logrus.WithFields(logrus.Fields{
			"original_message": userMessage,
			"selected_option":  selectedOption,
			"option_text":      selectedOptionText,
		}).Info("Detected follow-up response, expanding with context from conversation history")

		return expanded.String()
	}

	// If we have any previous context but no numbered options, still provide context
	if previousContext != "" && len(previousContext) > 100 {
		var expanded strings.Builder
		expanded.WriteString("The user is providing a follow-up to the previous response.\n\n")
		expanded.WriteString("PREVIOUS CONTEXT (summary):\n")
		if len(previousContext) > 300 {
			expanded.WriteString("...")
			expanded.WriteString(previousContext[len(previousContext)-300:])
		} else {
			expanded.WriteString(previousContext)
		}
		expanded.WriteString("\n\n")
		expanded.WriteString("USER'S FOLLOW-UP: ")
		expanded.WriteString(userMessage)
		return expanded.String()
	}

	return "" // No context to expand with
}

// generateDebateDialogueIntroduction creates the AI debate team conversation introduction
// This is displayed before the final response to show how the AI debate ensemble works
// Uses ANSI colors for terminal clients, clean Markdown for API clients
// When comprehensive system is enabled, shows 8 roles across 5 teams
func (h *UnifiedHandler) generateDebateDialogueIntroduction(topic string, format OutputFormat) string {
	// Check if comprehensive debate system is enabled
	if h.debateService != nil && h.debateService.IsComprehensiveSystemEnabled() {
		return h.generateComprehensiveDebateIntroduction(topic, format)
	}

	// Fall back to legacy 5-role debate team format
	if h.dialogueFormatter == nil || h.debateTeamConfig == nil {
		return ""
	}

	// Use format-aware introduction that adapts to the client type
	// GetPrimaryMembers returns only 5 primary members (one per position) with their Fallbacks populated
	members := h.debateTeamConfig.GetPrimaryMembers()
	return FormatDebateTeamIntroductionForFormat(format, topic, members)
}

// generateDebateTeamIntroduction creates a concise team overview for the 8-role debate
// showing each role's assigned LLM (round-robin from verified providers).
func (h *UnifiedHandler) generateDebateTeamIntroduction() string {
	if h.debateTeamConfig == nil {
		return ""
	}
	verifiedLLMs := h.debateTeamConfig.GetVerifiedLLMs()
	if len(verifiedLLMs) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## AI Debate Ensemble\n\n")
	sb.WriteString("| # | Role | Provider | Model |\n")
	sb.WriteString("|---|------|----------|-------|\n")

	roles := []string{"Architect", "Generator", "Critic", "Tester", "Security", "Performance", "Validator", "Moderator"}
	for i, role := range roles {
		llmIdx := i % len(verifiedLLMs)
		vllm := verifiedLLMs[llmIdx]
		sb.WriteString(fmt.Sprintf("| %d | **%s** | %s | %s |\n",
			i+1, role, vllm.ProviderName, vllm.ModelName))
	}
	sb.WriteString(fmt.Sprintf("\n> %d verified LLMs available for fallback\n\n", len(verifiedLLMs)))
	return sb.String()
}

// generateComprehensiveDebateIntroduction creates introduction for comprehensive debate system
// Shows 11 roles across 5 teams dynamically with models and fallbacks
func (h *UnifiedHandler) generateComprehensiveDebateIntroduction(topic string, format OutputFormat) string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString("# HelixAgent AI Debate Ensemble\n\n")

	// Show 11 debate roles from comprehensive system with actual models
	sb.WriteString("> 11 AI specialists collaborate to deliver optimal solutions.\n\n")

	// Add link to provider verification report if generator is available
	if h.reportGenerator != nil {
		// Generate the report file
		ctx := context.Background()
		reportPath, err := h.reportGenerator.GenerateReport(ctx)
		if err == nil {
			reportURL := h.reportGenerator.GetReportURL()
			sb.WriteString(fmt.Sprintf("📊 **[Provider Verification Report](%s)** - Full provider scores and model rankings\n\n", reportURL))
			logrus.WithFields(logrus.Fields{
				"report_path": reportPath,
				"report_url":  reportURL,
			}).Debug("Added provider verification report link to debate introduction")
		} else {
			logrus.WithError(err).Warning("Failed to generate provider verification report")
		}
	}

	// Topic
	topicDisplay := topic
	if len(topicDisplay) > 70 {
		topicDisplay = topicDisplay[:70] + "..."
	}
	sb.WriteString(fmt.Sprintf("**Topic:** %s\n\n", topicDisplay))

	sb.WriteString("---\n\n")
	sb.WriteString("## Debate Team (11 Roles)\n\n")

	// Show 11 comprehensive roles
	teams := []struct {
		name  string
		roles []struct {
			role string
			desc string
			pos  services.DebateTeamPosition
		}
	}{
		{"🏗️ Design", []struct {
			role string
			desc string
			pos  services.DebateTeamPosition
		}{
			{"Architect", "System design", services.PositionAnalyst},
			{"Moderator", "Debate facilitation", services.PositionProposer},
		}},
		{"💻 Implementation", []struct {
			role string
			desc string
			pos  services.DebateTeamPosition
		}{
			{"Generator", "Code generation", services.PositionCritic},
			{"Blue Team", "Defensive implementation", services.PositionSynthesis},
		}},
		{"🔍 Quality Assurance", []struct {
			role string
			desc string
			pos  services.DebateTeamPosition
		}{
			{"Critic", "Flaw identification", services.PositionMediator},
			{"Tester", "Test generation", services.PositionAnalyst},
			{"Validator", "Correctness verification", services.PositionProposer},
			{"Security", "Security analysis", services.PositionCritic},
			{"Performance", "Performance optimization", services.PositionSynthesis},
		}},
		{"🔴 Red Team", []struct {
			role string
			desc string
			pos  services.DebateTeamPosition
		}{
			{"Red Team", "Adversarial testing", services.PositionMediator},
		}},
		{"🔄 Refactoring", []struct {
			role string
			desc string
			pos  services.DebateTeamPosition
		}{
			{"Refactoring", "Code improvement", services.PositionAnalyst},
		}},
	}

	// Try to get actual models from debateTeamConfig
	if h.debateTeamConfig != nil {
		for _, team := range teams {
			sb.WriteString(fmt.Sprintf("### %s\n\n", team.name))
			sb.WriteString("| Role | Description | Model | Provider |\n")
			sb.WriteString("|------|-------------|-------|----------|\n")
			for _, r := range team.roles {
				// Get member for this specific position
				member := h.debateTeamConfig.GetTeamMember(r.pos)
				model := "best available"
				provider := "auto"
				if member != nil {
					model = member.ModelName
					provider = member.ProviderName
				}
				sb.WriteString(fmt.Sprintf("| **%s** | %s | %s | %s |\n", r.role, r.desc, model, provider))
			}
			sb.WriteString("\n")
		}
	} else {
		for _, team := range teams {
			sb.WriteString(fmt.Sprintf("### %s\n\n", team.name))
			sb.WriteString("| Role | Description | Status |\n")
			sb.WriteString("|------|-------------|--------|\n")
			for _, r := range team.roles {
				sb.WriteString(fmt.Sprintf("| **%s** | %s | 🟢 Active |\n", r.role, r.desc))
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\n---\n\n")
	sb.WriteString("## The Deliberation\n\n")

	return sb.String()
}

// getComprehensiveRoleName maps debate role to comprehensive role name
func getComprehensiveRoleName(role services.DebateRole) string {
	switch role {
	case services.RoleAnalyst:
		return "Analyst"
	case services.RoleProposer:
		return "Proposer"
	case services.RoleCritic:
		return "Critic"
	case services.RoleSynthesis:
		return "Synthesis"
	case services.RoleMediator:
		return "Mediator"
	default:
		return string(role)
	}
}

// getComprehensiveRoleNameFromPosition maps debate position to role name
func getComprehensiveRoleNameFromPosition(pos services.DebateTeamPosition) string {
	switch pos {
	case services.PositionAnalyst:
		return "Analyst"
	case services.PositionProposer:
		return "Proposer"
	case services.PositionCritic:
		return "Critic"
	case services.PositionSynthesis:
		return "Synthesis"
	case services.PositionMediator:
		return "Mediator"
	default:
		return fmt.Sprintf("Position%d", pos)
	}
}

// streamComprehensiveDebate streams the comprehensive debate using the working 5-position system
// that actually calls LLMs and shows fallback events with error reasons
func (h *UnifiedHandler) streamComprehensiveDebate(ctx context.Context, c *gin.Context, flusher http.Flusher, streamID string, topic string, outputFormat OutputFormat, req *OpenAIChatRequest) {
	logrus.Info("[Comprehensive Streaming] Starting comprehensive debate stream with real LLM calls")

	// Generate comprehensive introduction with models and fallbacks
	intro := h.generateComprehensiveDebateIntroduction(topic, outputFormat)

	// Stream introduction
	for _, line := range strings.Split(intro, "\n") {
		if line == "" {
			line = "\n"
		} else {
			line = line + "\n"
		}
		chunk := map[string]any{
			"id":                 streamID,
			"object":             "chat.completion.chunk",
			"created":            time.Now().Unix(),
			"model":              "helixagent-ensemble",
			"system_fingerprint": "fp_helixagent_v1",
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         map[string]any{"content": line},
					"logprobs":      nil,
					"finish_reason": nil,
				},
			},
		}
		if data, err := json.Marshal(chunk); err == nil {
			_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
			_, _ = c.Writer.Write(data)             //nolint:errcheck
			_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
			flusher.Flush()                         //nolint:errcheck
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Use comprehensive 11-role debate system
	// These are the 11 specialized roles from the comprehensive debate research
	positions := []struct {
		pos       services.DebateTeamPosition
		role      comprehensive.Role
		roleStr   string
		desc      string
		teamName  string
		teamEmoji string
	}{
		// Design Team (2 roles)
		{services.PositionAnalyst, comprehensive.RoleArchitect, "Architect", "System design and planning", "Design", "🏗️"},
		{services.PositionProposer, comprehensive.RoleModerator, "Moderator", "Debate facilitation", "Design", "🏗️"},
		// Implementation Team (2 roles)
		{services.PositionCritic, comprehensive.RoleGenerator, "Generator", "Code generation", "Implementation", "💻"},
		{services.PositionSynthesis, comprehensive.RoleBlueTeam, "Blue Team", "Defensive implementation", "Implementation", "💻"},
		// Quality Assurance Team (5 roles)
		{services.PositionMediator, comprehensive.RoleCritic, "Critic", "Flaw identification", "Quality Assurance", "🔍"},
		{services.PositionAnalyst, comprehensive.RoleTester, "Tester", "Test generation", "Quality Assurance", "🔍"},
		{services.PositionProposer, comprehensive.RoleValidator, "Validator", "Correctness verification", "Quality Assurance", "🔍"},
		{services.PositionCritic, comprehensive.RoleSecurity, "Security", "Security analysis", "Quality Assurance", "🔍"},
		{services.PositionSynthesis, comprehensive.RolePerformance, "Performance", "Performance optimization", "Quality Assurance", "🔍"},
		// Red Team (1 role) - Changed team name to "Adversarial" to avoid confusion
		{services.PositionMediator, comprehensive.RoleRedTeam, "Red Team", "Adversarial testing", "Adversarial", "🔴"},
		// Refactoring Team (1 role)
		{services.PositionAnalyst, comprehensive.RoleRefactoring, "Refactoring", "Code improvement", "Refactoring", "🔄"},
	}
	previousResponses := make(map[services.DebateTeamPosition]string)
	collectedToolCalls := make([]StreamingToolCall, 0)

	currentTeam := ""
	for _, p := range positions {
		// Emit team header if team changed - make it very clear and visible
		if p.teamName != currentTeam {
			currentTeam = p.teamName
			teamHeader := fmt.Sprintf("\n---\n\n## %s %s\n\n", p.teamEmoji, p.teamName)
			chunk := map[string]any{
				"id":                 streamID,
				"object":             "chat.completion.chunk",
				"created":            time.Now().Unix(),
				"model":              "helixagent-ensemble",
				"system_fingerprint": "fp_helixagent_v1",
				"choices": []map[string]any{
					{
						"index":         0,
						"delta":         map[string]any{"content": teamHeader},
						"logprobs":      nil,
						"finish_reason": nil,
					},
				},
			}
			if data, err := json.Marshal(chunk); err == nil {
				_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
				_, _ = c.Writer.Write(data)             //nolint:errcheck
				_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
				flusher.Flush()                         //nolint:errcheck
			}
		}

		pos := p.pos
		// Get member info for this position
		member := h.debateTeamConfig.GetTeamMember(pos)
		var memberProvider, memberModel string
		if member != nil {
			memberProvider = member.ProviderName
			memberModel = member.ModelName
		} else {
			memberProvider = "unknown"
			memberModel = "unknown"
		}

		// Stream REQUEST indicator - use roleStr from positions array
		requestIndicator := fmt.Sprintf("%s Requesting %s (%s)...\n", p.roleStr, memberModel, memberProvider)
		if requestIndicator != "" {
			reqChunk := map[string]any{
				"id":                 streamID,
				"object":             "chat.completion.chunk",
				"created":            time.Now().Unix(),
				"model":              "helixagent-ensemble",
				"system_fingerprint": "fp_helixagent_v1",
				"choices": []map[string]any{
					{
						"index":         0,
						"delta":         map[string]any{"content": requestIndicator},
						"logprobs":      nil,
						"finish_reason": nil,
					},
				},
			}
			if reqData, err := json.Marshal(reqChunk); err == nil {
				_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
				_, _ = c.Writer.Write(reqData)          //nolint:errcheck
				_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
				flusher.Flush()                         //nolint:errcheck
			}
		}

		// Get REAL response from LLM with fallback tracking
		// Pass the comprehensive role string (roleStr) so the prompt builder can use proper role names
		debateResp, err := h.generateRealDebateResponse(ctx, p.pos, p.roleStr, topic, previousResponses, req.Tools)
		var realResponse string
		var responseIndicator string

		if err != nil {
			// Stream fallback information with error reason
			fallbackInfo := h.formatFallbackInfo(outputFormat, pos, member, err)
			if fallbackInfo != "" {
				fallbackChunk := map[string]any{
					"id":                 streamID,
					"object":             "chat.completion.chunk",
					"created":            time.Now().Unix(),
					"model":              "helixagent-ensemble",
					"system_fingerprint": "fp_helixagent_v1",
					"choices": []map[string]any{
						{
							"index":         0,
							"delta":         map[string]any{"content": fallbackInfo},
							"logprobs":      nil,
							"finish_reason": nil,
						},
					},
				}
				if fbData, marshalErr := json.Marshal(fallbackChunk); marshalErr == nil {
					_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
					_, _ = c.Writer.Write(fbData)           //nolint:errcheck
					_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
					flusher.Flush()                         //nolint:errcheck
				}
			}

			// Fallback handling is already done inside generateRealDebateResponse
			// which has its own fallback logic
			realResponse = fmt.Sprintf("[Error: %v]", err)
			responseIndicator = fmt.Sprintf("> %s Response failed: %v\n\n", p.roleStr, err)
		} else {
			realResponse = debateResp.Content
			// Use ActualProvider and ActualModel which track what was actually used
			// Note: actualProvider and actualModel are not currently used
			// Use roleStr from positions array instead of memberRole
			responseIndicator = fmt.Sprintf("> %s Response received (%.1f s)\n\n", p.roleStr, debateResp.ResponseTime.Seconds())
			if len(debateResp.ToolCalls) > 0 {
				collectedToolCalls = append(collectedToolCalls, debateResp.ToolCalls...) //nolint:staticcheck
			}

			// If fallback was used, show the fallback chain
			if debateResp.UsedFallback && len(debateResp.FallbackChain) > 0 {
				fallbackDetails := h.formatFallbackChain(debateResp.FallbackChain)
				if fallbackDetails != "" {
					fbDetailChunk := map[string]any{
						"id":                 streamID,
						"object":             "chat.completion.chunk",
						"created":            time.Now().Unix(),
						"model":              "helixagent-ensemble",
						"system_fingerprint": "fp_helixagent_v1",
						"choices": []map[string]any{
							{
								"index":         0,
								"delta":         map[string]any{"content": fallbackDetails},
								"logprobs":      nil,
								"finish_reason": nil,
							},
						},
					}
					if fbData, err := json.Marshal(fbDetailChunk); err == nil {
						_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
						_, _ = c.Writer.Write(fbData)           //nolint:errcheck
						_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
						flusher.Flush()                         //nolint:errcheck
					}
				}
			}
		}

		// Stream response indicator (with duration or error)
		if responseIndicator != "" {
			respIndChunk := map[string]any{
				"id":                 streamID,
				"object":             "chat.completion.chunk",
				"created":            time.Now().Unix(),
				"model":              "helixagent-ensemble",
				"system_fingerprint": "fp_helixagent_v1",
				"choices": []map[string]any{
					{
						"index":         0,
						"delta":         map[string]any{"content": responseIndicator},
						"logprobs":      nil,
						"finish_reason": nil,
					},
				},
			}
			if respIndData, err := json.Marshal(respIndChunk); err == nil {
				_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
				_, _ = c.Writer.Write(respIndData)      //nolint:errcheck
				_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
				flusher.Flush()                         //nolint:errcheck
			}
		}

		// Store response for next position
		previousResponses[pos] = realResponse

		// Stream the actual debate response content
		if realResponse != "" {
			// Format as comprehensive debate response using the role string from positions array
			formattedResponse := fmt.Sprintf("\n**[%s]**\n\n%s\n\n", p.roleStr, realResponse)

			respChunk := map[string]any{
				"id":                 streamID,
				"object":             "chat.completion.chunk",
				"created":            time.Now().Unix(),
				"model":              "helixagent-ensemble",
				"system_fingerprint": "fp_helixagent_v1",
				"choices": []map[string]any{
					{
						"index":         0,
						"delta":         map[string]any{"content": formattedResponse},
						"logprobs":      nil,
						"finish_reason": nil,
					},
				},
			}
			if respData, err := json.Marshal(respChunk); err == nil {
				_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
				_, _ = c.Writer.Write(respData)         //nolint:errcheck
				_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
				flusher.Flush()                         //nolint:errcheck
			}
		}
	}

	// Stream consensus section
	consensusHeader := "\n---\n\n## Consensus\n\n"
	consensusChunk := map[string]any{
		"id":                 streamID,
		"object":             "chat.completion.chunk",
		"created":            time.Now().Unix(),
		"model":              "helixagent-ensemble",
		"system_fingerprint": "fp_helixagent_v1",
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{"content": consensusHeader},
				"logprobs":      nil,
				"finish_reason": nil,
			},
		},
	}
	if consData, err := json.Marshal(consensusChunk); err == nil {
		_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
		_, _ = c.Writer.Write(consData)         //nolint:errcheck
		_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
		flusher.Flush()                         //nolint:errcheck
	}

	// Generate final synthesis
	synthesis, err := h.generateFinalSynthesis(ctx, topic, previousResponses, req.Tools)
	if err != nil {
		logrus.WithError(err).Warn("Failed to generate synthesis")
		synthesis = h.generateFallbackSynthesis(previousResponses)
	}

	// Stream synthesis
	if synthesis != "" {
		synthChunk := map[string]any{
			"id":                 streamID,
			"object":             "chat.completion.chunk",
			"created":            time.Now().Unix(),
			"model":              "helixagent-ensemble",
			"system_fingerprint": "fp_helixagent_v1",
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         map[string]any{"content": synthesis},
					"logprobs":      nil,
					"finish_reason": nil,
				},
			},
		}
		if synthData, err := json.Marshal(synthChunk); err == nil {
			_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
			_, _ = c.Writer.Write(synthData)        //nolint:errcheck
			_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
			flusher.Flush()                         //nolint:errcheck
		}
	}

	// Stream footer
	footer := h.generateResponseFooter()
	if footer != "" {
		footerChunk := map[string]any{
			"id":                 streamID,
			"object":             "chat.completion.chunk",
			"created":            time.Now().Unix(),
			"model":              "helixagent-ensemble",
			"system_fingerprint": "fp_helixagent_v1",
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         map[string]any{"content": footer},
					"logprobs":      nil,
					"finish_reason": nil,
				},
			},
		}
		if footerData, err := json.Marshal(footerChunk); err == nil {
			_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
			_, _ = c.Writer.Write(footerData)       //nolint:errcheck
			_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
			flusher.Flush()                         //nolint:errcheck
		}
	}

	// Send finish chunk
	finishChunk := map[string]any{
		"id":                 streamID,
		"object":             "chat.completion.chunk",
		"created":            time.Now().Unix(),
		"model":              "helixagent-ensemble",
		"system_fingerprint": "fp_helixagent_v1",
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"logprobs":      nil,
				"finish_reason": "stop",
			},
		},
	}
	if data, err := json.Marshal(finishChunk); err == nil {
		_, _ = c.Writer.Write([]byte("data: ")) //nolint:errcheck
		_, _ = c.Writer.Write(data)             //nolint:errcheck
		_, _ = c.Writer.Write([]byte("\n\n"))   //nolint:errcheck
		flusher.Flush()                         //nolint:errcheck
	}

	_, _ = c.Writer.Write([]byte("data: [DONE]\n\n")) //nolint:errcheck
	flusher.Flush()                                   //nolint:errcheck

	logrus.Info("[Comprehensive Streaming] Stream complete with real LLM calls")
}

// formatFallbackInfo formats fallback information with error reason
func (h *UnifiedHandler) formatFallbackInfo(format OutputFormat, position services.DebateTeamPosition, member *services.DebateTeamMember, err error) string {
	if member == nil || member.Fallback == nil {
		return ""
	}

	var sb strings.Builder
	posName := getComprehensiveRoleName(member.Role)
	sb.WriteString(fmt.Sprintf("\n⚡ **%s Fallback Triggered**\n", posName))
	sb.WriteString(fmt.Sprintf("   Primary: %s/%s (0 ms)\n", member.ProviderName, member.ModelName))
	sb.WriteString(fmt.Sprintf("   ❓ Error: %v\n", err))
	sb.WriteString(fmt.Sprintf("   → Trying: %s/%s\n\n", member.Fallback.ProviderName, member.Fallback.ModelName))

	return sb.String()
}

// formatFallbackChain formats the fallback chain details
func (h *UnifiedHandler) formatFallbackChain(chain []FallbackAttempt) string {
	if len(chain) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n📋 **Fallback Chain:**\n")
	for _, attempt := range chain {
		status := "✅"
		if !attempt.Success {
			status = "❌"
		}
		sb.WriteString(fmt.Sprintf("   %s Attempt %d: %s/%s (%d ms)\n",
			status, attempt.AttemptNum, attempt.Provider, attempt.Model, attempt.Duration.Milliseconds()))
		if attempt.Error != "" {
			sb.WriteString(fmt.Sprintf("      Error: %s\n", attempt.Error))
		}
	}
	sb.WriteString("\n")
	return sb.String()
}

// generateFallbackSynthesis creates a synthesis from debate responses when LLM synthesis fails
func (h *UnifiedHandler) generateFallbackSynthesis(responses map[services.DebateTeamPosition]string) string {
	var sb strings.Builder
	sb.WriteString("Based on the debate:\n\n")

	for pos, content := range responses {
		if content != "" {
			posName := getComprehensiveRoleNameFromPosition(pos)
			sb.WriteString(fmt.Sprintf("**%s**: %s\n\n", posName, content))
		}
	}

	return sb.String()
}

// generateRealDebateResponse calls the actual LLM for a position and returns the real response
// It tries the primary provider first, then automatically falls back to fallback providers if primary fails
// The tools parameter is passed to inform the LLM about available coding assistant capabilities
// The comprehensiveRoleStr parameter is the role name from the 11-role comprehensive system (e.g., "Architect", "Moderator")
// CRITICAL: Now returns *DebatePositionResponse to include tool_calls from LLM, not just content
func (h *UnifiedHandler) generateRealDebateResponse(ctx context.Context, position services.DebateTeamPosition, comprehensiveRoleStr string, topic string, previousResponses map[services.DebateTeamPosition]string, tools []OpenAITool) (*DebatePositionResponse, error) {
	overallStart := time.Now()

	if h.providerRegistry == nil || h.debateTeamConfig == nil {
		return nil, fmt.Errorf("provider registry or debate team config not available")
	}

	// Get the team member assigned to this position
	member := h.debateTeamConfig.GetTeamMember(position)
	if member == nil {
		return nil, fmt.Errorf("no LLM assigned to position %d", position)
	}

	// Build a role-specific system prompt with tool awareness (same for all attempts)
	// CRITICAL: Pass tools so LLM knows about available coding assistant capabilities
	// Pass comprehensiveRoleStr to use proper role names (Architect, Moderator, etc.) instead of legacy names
	systemPrompt := h.buildDebateRoleSystemPromptWithTools(position, comprehensiveRoleStr, member.Role, tools)

	// Build context from previous responses
	contextStr := ""
	if len(previousResponses) > 0 {
		contextStr = "\n\nPrevious contributions from other team members:\n"
		positionOrder := []services.DebateTeamPosition{
			services.PositionAnalyst,
			services.PositionProposer,
			services.PositionCritic,
			services.PositionSynthesis,
			services.PositionMediator,
		}
		for _, pos := range positionOrder {
			if resp, ok := previousResponses[pos]; ok && resp != "" {
				char := h.dialogueFormatter.GetCharacter(pos)
				if char != nil {
					contextStr += fmt.Sprintf("- %s: %s\n", char.Name, resp)
				}
			}
		}
	}

	// Create the base user prompt
	userPrompt := fmt.Sprintf("Topic: %s%s\n\nProvide your analysis in 2-3 sentences, focused on your role.", topic, contextStr)

	// Build the fallback chain: primary member followed by all fallbacks
	memberChain := make([]*services.DebateTeamMember, 0, 5)
	memberChain = append(memberChain, member)
	// Use Fallbacks array (preferred) over deprecated singular Fallback
	if len(member.Fallbacks) > 0 {
		for _, fb := range member.Fallbacks {
			if fb != nil && fb.IsActive {
				memberChain = append(memberChain, fb)
			}
		}
	} else if member.Fallback != nil {
		// Legacy support: walk the deprecated singular Fallback chain
		fb := member.Fallback
		for fb != nil {
			memberChain = append(memberChain, fb)
			fb = fb.Fallback
		}
	}

	attemptNum := 0
	maxAttempts := len(memberChain)
	var lastErr error

	// Track fallback chain for visualization
	fallbackChain := make([]FallbackAttempt, 0)
	primaryProvider := member.ProviderName
	primaryModel := member.ModelName

	for attemptNum < maxAttempts {
		currentMember := memberChain[attemptNum]
		attemptNum++
		attemptStart := time.Now()

		// Get the provider for this member
		provider, providerErr := h.getProviderForMember(currentMember)
		if providerErr != nil {
			fallbackChain = append(fallbackChain, FallbackAttempt{
				Provider:   currentMember.ProviderName,
				Model:      currentMember.ModelName,
				Success:    false,
				Error:      providerErr.Error(),
				Duration:   time.Since(attemptStart),
				AttemptNum: attemptNum,
			})
			logrus.WithFields(logrus.Fields{
				"position": position,
				"provider": currentMember.ProviderName,
				"model":    currentMember.ModelName,
				"attempt":  attemptNum,
				"is_oauth": currentMember.IsOAuth,
			}).WithError(providerErr).Warn("Failed to get provider, trying fallback")
			lastErr = providerErr
			attemptNum++
			continue
		}

		// Create the request with this member's model
		llmReq := &models.LLMRequest{
			// D-20: crypto-random suffix makes the ID unique-by-construction even
			// when the same position/attempt is retried within one coarse clock tick.
			ID:        fmt.Sprintf("debate-%d-%d-%d-%s", position, attemptNum, time.Now().UnixNano(), utils.SecureRandomID("")),
			SessionID: "debate-session",
			Prompt:    userPrompt,
			Messages: []models.Message{
				{Role: "system", Content: systemPrompt},
				{Role: "user", Content: userPrompt},
			},
			ModelParams: models.ModelParameters{
				Model:       currentMember.ModelName,
				Temperature: 0.7,
				MaxTokens:   512, // Increased to allow for tool call responses
			},
		}

		// CRITICAL: Pass tools to the LLM so it can actually call them
		// This enables the AI Debate to use tools like Read, Write, Glob, Grep, Bash
		if len(tools) > 0 {
			llmReq.Tools = h.convertOpenAIToolsToModelTools(tools)
			llmReq.ToolChoice = "auto" // Let the LLM decide when to use tools
			// Also pass via ProviderSpecific for backward compatibility
			llmReq.ModelParams.ProviderSpecific = map[string]interface{}{
				"tools":       tools,
				"tool_choice": "auto",
			}
		}

		// Call the LLM with a timeout
		// CRITICAL: OAuth providers (Claude, Qwen) need longer timeouts due to CLI overhead
		timeout := 20 * time.Second
		if currentMember.IsOAuth {
			timeout = 45 * time.Second // OAuth CLI needs more time
		}
		llmCtx, cancel := context.WithTimeout(ctx, timeout)
		resp, err := provider.Complete(llmCtx, llmReq)
		cancel()

		attemptDuration := time.Since(attemptStart)

		if err != nil {
			fallbackChain = append(fallbackChain, FallbackAttempt{
				Provider:   currentMember.ProviderName,
				Model:      currentMember.ModelName,
				Success:    false,
				Error:      err.Error(),
				Duration:   attemptDuration,
				AttemptNum: attemptNum,
			})
			logrus.WithFields(logrus.Fields{
				"position": position,
				"provider": currentMember.ProviderName,
				"model":    currentMember.ModelName,
				"attempt":  attemptNum,
				"is_oauth": currentMember.IsOAuth,
				"duration": attemptDuration,
			}).WithError(err).Warn("LLM call failed, trying fallback")
			lastErr = err
			attemptNum++
			continue
		}

		// Record successful attempt
		fallbackChain = append(fallbackChain, FallbackAttempt{
			Provider:   currentMember.ProviderName,
			Model:      currentMember.ModelName,
			Success:    true,
			Duration:   attemptDuration,
			AttemptNum: attemptNum,
		})

		// Success! Clean up the response
		content := strings.TrimSpace(resp.Content)
		content = strings.Trim(content, "\"")

		// CRITICAL: Convert LLM tool_calls to StreamingToolCall format
		// This enables the AI Debate system to actually USE tools, not just discuss them
		var toolCalls []StreamingToolCall
		if len(resp.ToolCalls) > 0 {
			logrus.WithFields(logrus.Fields{
				"position":        position,
				"tool_call_count": len(resp.ToolCalls),
				"provider":        currentMember.ProviderName,
			}).Info("<--- AI Debate: LLM returned tool_calls --->")

			for i, tc := range resp.ToolCalls {
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: i,
					ID:    tc.ID,
					Type:  tc.Type,
					Function: OpenAIFunctionCall{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
				logrus.WithFields(logrus.Fields{
					"position":  position,
					"tool_name": tc.Function.Name,
					"tool_id":   tc.ID,
				}).Info("<--- Tool call detected --->")
			}
		}

		usedFallback := attemptNum > 1
		if usedFallback {
			logrus.WithFields(logrus.Fields{
				"position":         position,
				"primary_provider": primaryProvider,
				"actual_provider":  currentMember.ProviderName,
				"model":            currentMember.ModelName,
				"attempt":          attemptNum,
				"total_duration":   time.Since(overallStart),
			}).Info("Debate response succeeded with fallback provider")
		}

		return &DebatePositionResponse{
			Content:         content,
			ToolCalls:       toolCalls,
			Position:        position,
			ResponseTime:    time.Since(overallStart),
			PrimaryProvider: primaryProvider,
			PrimaryModel:    primaryModel,
			ActualProvider:  currentMember.ProviderName,
			ActualModel:     currentMember.ModelName,
			UsedFallback:    usedFallback,
			FallbackChain:   fallbackChain,
			Timestamp:       time.Now(),
		}, nil
	}

	// All attempts failed
	return nil, fmt.Errorf("all providers failed for position %d after %d attempts, last error: %w", position, attemptNum, lastErr)
}

// getProviderForMember retrieves the LLM provider for a debate team member
func (h *UnifiedHandler) getProviderForMember(member *services.DebateTeamMember) (llm.LLMProvider, error) {
	if member.Provider != nil {
		// Use the embedded provider (from DebateTeamMember)
		return member.Provider, nil
	}

	// Fall back to registry lookup using the verified provider instance from debate team config
	if h.debateTeamConfig != nil {
		if verifiedProv := h.debateTeamConfig.GetVerifiedProviderInstance(member.ProviderName, member.ModelName); verifiedProv != nil {
			return verifiedProv, nil
		}
	}

	// Last resort: registry lookup
	regProvider, regErr := h.providerRegistry.GetProvider(member.ProviderName)
	if regErr != nil {
		return nil, fmt.Errorf("provider %s not found: %w", member.ProviderName, regErr)
	}

	// Convert services.LLMProvider to llm.LLMProvider
	if llmProv, ok := regProvider.(llm.LLMProvider); ok { //nolint:gosimple
		return llmProv, nil
	}

	return nil, fmt.Errorf("provider %s does not implement llm.LLMProvider", member.ProviderName)
}

// buildDebateRoleSystemPromptWithTools creates a system prompt with specific tool information
// This is CRITICAL for AI coding assistants - the LLM needs to know what tools are available
// The comprehensiveRoleStr is the role name from the 11-role comprehensive system (e.g., "Architect", "Moderator")
// If comprehensiveRoleStr is empty, falls back to legacy role names (Analyst, Proposer, etc.)
func (h *UnifiedHandler) buildDebateRoleSystemPromptWithTools(position services.DebateTeamPosition, comprehensiveRoleStr string, role services.DebateRole, tools []OpenAITool) string {
	// Build tool description if tools are available
	toolDescription := ""
	if len(tools) > 0 {
		toolNames := make([]string, 0, len(tools))
		for _, tool := range tools {
			if tool.Function.Name != "" {
				toolNames = append(toolNames, tool.Function.Name)
			}
		}
		if len(toolNames) > 0 {
			toolDescription = fmt.Sprintf(`
AVAILABLE TOOLS (these are available through the coding assistant interface):
- %s

The user's AI coding assistant (Claude Code, OpenCode, Qwen Code, etc.) has access to these tools.
When the user asks about their codebase, YOU CAN AND SHOULD provide specific guidance because
the tools above allow reading files, searching code, editing files, and running commands.
`, strings.Join(toolNames, "\n- "))
		}
	}

	// Context about being part of an AI coding assistant with tool access
	contextPrompt := fmt.Sprintf(`You are part of HelixAgent, an AI coding assistant that provides responses through an AI Debate Ensemble.

IMPORTANT CONTEXT:
- You are integrated with AI coding tools like Claude Code, OpenCode, and Qwen Code
- The user's coding assistant HAS FULL ACCESS to their codebase through tools
- The tools can: Read files, Write/Edit files, Search code (grep), List files (glob), Execute shell commands
- When the user asks about their code, the assistant CAN see and access their files
- NEVER say "I cannot see your codebase" - the tools handle file access
- Provide SPECIFIC, ACTIONABLE coding advice as if you can see their code
- If asked about specific files or code, give concrete guidance%s

You are participating in an AI debate ensemble. Provide a concise, focused response in 2-3 sentences. `, toolDescription)

	// If comprehensive role string is provided (11-role system), use it; otherwise fall back to legacy
	if comprehensiveRoleStr != "" {
		return contextPrompt + getComprehensiveRolePrompt(comprehensiveRoleStr)
	}

	// Legacy 5-role system fallback
	switch role {
	case services.RoleAnalyst:
		return contextPrompt + "Your role is THE ANALYST: Break down the problem systematically, identify key factors, and provide data-driven insights. For coding questions, analyze code structure, patterns, and potential issues."
	case services.RoleProposer:
		return contextPrompt + "Your role is THE PROPOSER: Present actionable solutions and approaches, focusing on practical implementation. For coding questions, suggest specific code changes or approaches."
	case services.RoleCritic:
		return contextPrompt + "Your role is THE CRITIC: Challenge assumptions, identify potential issues, edge cases, and weaknesses in proposed approaches. For coding questions, identify potential bugs, security issues, or performance concerns."
	case services.RoleSynthesis:
		return contextPrompt + "Your role is THE SYNTHESIZER: Combine insights from different perspectives into a cohesive understanding. For coding questions, synthesize the best approach from all perspectives."
	case services.RoleMediator:
		return contextPrompt + "Your role is THE MEDIATOR: Balance different viewpoints and guide toward consensus. For coding questions, recommend the most balanced and practical solution."
	default:
		return contextPrompt + "Contribute your perspective on the topic."
	}
}

// getComprehensiveRolePrompt returns the role-specific prompt for the 11-role comprehensive debate system
func getComprehensiveRolePrompt(roleStr string) string {
	rp := comprehensive.RolePrompts{}
	switch roleStr {
	case "Architect":
		return rp.Architect()
	case "Moderator":
		return rp.Moderator()
	case "Generator":
		return rp.Generator()
	case "Blue Team":
		return rp.BlueTeam()
	case "Critic":
		return rp.Critic()
	case "Tester":
		return rp.Tester()
	case "Validator":
		return rp.Validator()
	case "Security":
		return rp.Security()
	case "Performance":
		return rp.Performance()
	case "Red Team":
		return rp.RedTeam()
	case "Refactoring":
		return rp.Refactoring()
	default:
		return "Contribute your expertise to the debate."
	}
}

// generateDebateDialogueConclusion creates the conclusion section after debate
// Uses format-aware styling based on client type (ANSI for terminal, Markdown for API clients)
func (h *UnifiedHandler) generateDebateDialogueConclusion(format OutputFormat) string {
	// Use the format-aware consensus header
	return FormatConsensusHeaderForFormat(format)
}

// generateFinalSynthesis creates the final synthesized response based on all debate contributions
// This is the CRITICAL function that produces the actual CONSENSUS content
// IMPORTANT: tools parameter informs the synthesis about available coding assistant capabilities
func (h *UnifiedHandler) generateFinalSynthesis(ctx context.Context, topic string, debateResponses map[services.DebateTeamPosition]string, tools []OpenAITool) (string, error) {
	if h.providerRegistry == nil || h.debateTeamConfig == nil {
		return "", fmt.Errorf("provider registry or debate team config not available")
	}

	// Build tool description if tools are available
	toolDescription := ""
	if len(tools) > 0 {
		toolNames := make([]string, 0, len(tools))
		for _, tool := range tools {
			if tool.Function.Name != "" {
				toolNames = append(toolNames, tool.Function.Name)
			}
		}
		if len(toolNames) > 0 {
			toolDescription = fmt.Sprintf(`
AVAILABLE TOOLS (the user's coding assistant has access to these):
- %s
`, strings.Join(toolNames, "\n- "))
		}
	}

	// Build synthesis prompt that includes all debate contributions
	// IMPORTANT: Include context about AI coding assistant capabilities
	var promptBuilder strings.Builder
	promptBuilder.WriteString(fmt.Sprintf(`You are part of HelixAgent, an AI coding assistant that provides responses through an AI Debate Ensemble.

IMPORTANT CONTEXT:
- You are integrated with AI coding tools like Claude Code, OpenCode, and Qwen Code
- The user's coding assistant HAS FULL ACCESS to their codebase through tools
- Tools available: Read files, Write/Edit files, Search code (grep), List files (glob), Execute shell commands
- When the user asks about their code, the assistant CAN see and access their files
- NEVER say "I cannot see your codebase" - the tools handle file access
- Provide SPECIFIC, ACTIONABLE coding advice%s

You are tasked with synthesizing a final consensus response based on a multi-perspective AI debate.

`, toolDescription))
	promptBuilder.WriteString("ORIGINAL TOPIC/QUESTION:\n")
	promptBuilder.WriteString(topic)
	promptBuilder.WriteString("\n\nDEBATE CONTRIBUTIONS:\n\n")

	// Add each position's contribution
	positionNames := map[services.DebateTeamPosition]string{
		services.PositionAnalyst:   "THE ANALYST",
		services.PositionProposer:  "THE PROPOSER",
		services.PositionCritic:    "THE CRITIC",
		services.PositionSynthesis: "THE SYNTHESIZER",
		services.PositionMediator:  "THE MEDIATOR",
	}

	positionOrder := []services.DebateTeamPosition{
		services.PositionAnalyst,
		services.PositionProposer,
		services.PositionCritic,
		services.PositionSynthesis,
		services.PositionMediator,
	}

	for _, pos := range positionOrder {
		if response, ok := debateResponses[pos]; ok && response != "" {
			promptBuilder.WriteString(fmt.Sprintf("%s:\n%s\n\n", positionNames[pos], response))
		}
	}

	promptBuilder.WriteString("TASK:\n")
	promptBuilder.WriteString("Based on the above debate, provide a FINAL SYNTHESIZED RESPONSE that:\n")
	promptBuilder.WriteString("1. Directly answers the original topic/question\n")
	promptBuilder.WriteString("2. Incorporates the strongest insights from each perspective\n")
	promptBuilder.WriteString("3. Addresses concerns raised by the critic where valid\n")
	promptBuilder.WriteString("4. Provides a clear, actionable, and balanced conclusion\n")
	promptBuilder.WriteString("5. For coding questions, provide specific, actionable guidance the user can apply\n\n")
	promptBuilder.WriteString("Your response should be comprehensive yet concise (2-4 paragraphs). Focus on providing genuine value to the user. Remember: the user has access to their codebase through integrated tools - provide practical advice they can immediately apply.")

	synthesisPrompt := promptBuilder.String()

	// Get the Mediator's provider for synthesis (it's designed for consensus-building)
	mediatorMember := h.debateTeamConfig.GetTeamMember(services.PositionMediator)
	if mediatorMember == nil {
		// Fallback to any available provider
		mediatorMember = h.debateTeamConfig.GetTeamMember(services.PositionSynthesis)
	}
	if mediatorMember == nil {
		return "", fmt.Errorf("no suitable provider found for synthesis")
	}

	// Get provider using the same fallback mechanism as debate responses
	provider, err := h.getProviderForMember(mediatorMember)
	if err != nil {
		// Try fallbacks
		currentMember := mediatorMember.Fallback
		for currentMember != nil {
			provider, err = h.getProviderForMember(currentMember)
			if err == nil {
				break
			}
			currentMember = currentMember.Fallback
		}
		if provider == nil {
			return "", fmt.Errorf("all providers failed for synthesis: %w", err)
		}
	}

	// Create the synthesis request
	synthesisReq := &models.LLMRequest{
		Messages: []models.Message{
			{
				Role:    "system",
				Content: "You are a skilled synthesizer who combines multiple perspectives into clear, actionable conclusions.",
			},
			{
				Role:    "user",
				Content: synthesisPrompt,
			},
		},
		ModelParams: models.ModelParameters{
			MaxTokens:   1024,
			Temperature: 0.7,
		},
	}

	// Call the LLM with timeout
	synthesisCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := provider.Complete(synthesisCtx, synthesisReq)
	if err != nil {
		logrus.WithError(err).WithField("provider", mediatorMember.ProviderName).Warn("Synthesis provider failed, trying fallback")

		// Try fallback providers
		currentMember := mediatorMember.Fallback
		for currentMember != nil && err != nil {
			provider, provErr := h.getProviderForMember(currentMember)
			if provErr != nil {
				currentMember = currentMember.Fallback
				continue
			}

			resp, err = provider.Complete(synthesisCtx, synthesisReq)
			if err == nil {
				logrus.WithField("provider", currentMember.ProviderName).Info("Synthesis succeeded with fallback provider")
				break
			}

			logrus.WithError(err).WithField("provider", currentMember.ProviderName).Warn("Fallback synthesis provider also failed")
			currentMember = currentMember.Fallback
		}

		if err != nil {
			return "", fmt.Errorf("all synthesis providers failed: %w", err)
		}
	}

	return strings.TrimSpace(resp.Content), nil
}

// generateResponseFooter creates the finalization footer for every response
func (h *UnifiedHandler) generateResponseFooter() string {
	var sb strings.Builder

	sb.WriteString("\n\n")
	sb.WriteString("───────────────────────────────────────────────────────────────────────────────\n")
	sb.WriteString("                     ✨ Powered by HelixAgent AI Debate Ensemble ✨\n")
	sb.WriteString("                  Synthesized from 11 AI perspectives for optimal results\n")
	sb.WriteString("───────────────────────────────────────────────────────────────────────────────\n")

	return sb.String()
}

// generateActionToolCalls analyzes the debate synthesis and generates actual tool calls
// CRITICAL: This function enables AI coding assistants to execute tools, not just talk about them
// It analyzes the topic/question and synthesis to determine what tools should be called
// Now uses a hybrid approach: pattern matching for common cases, LLM-based for confirmations
func (h *UnifiedHandler) generateActionToolCalls(ctx context.Context, topic string, synthesis string, tools []OpenAITool, previousResponses map[services.DebateTeamPosition]string, messages []OpenAIMessage) []StreamingToolCall {
	if len(tools) == 0 {
		return nil
	}

	// Build a map of available tools by name
	availableTools := make(map[string]OpenAITool)
	for _, tool := range tools {
		if tool.Type == "function" {
			availableTools[tool.Function.Name] = tool
		}
	}

	var toolCalls []StreamingToolCall
	topicLower := strings.ToLower(topic)
	synthesisLower := strings.ToLower(synthesis)

	// Determine what tools to call based on topic and synthesis analysis
	// This enables the AI to actually take action, not just discuss what it could do

	// Case 1: Questions about seeing/accessing the codebase - use glob or read
	if containsAny(topicLower, []string{"see my codebase", "access the codebase", "view the codebase", "look at my code", "see my code", "access my code"}) {
		if tool, ok := availableTools["glob"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: `{"pattern": "**/*", "path": "."}`,
				},
			})
		} else if tool, ok := availableTools["Glob"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: `{"pattern": "**/*"}`,
				},
			})
		}
	}

	// Case 2: Questions about structure/architecture - use glob to explore
	if containsAny(topicLower, []string{"structure", "architecture", "layout", "organization", "how is.*organized"}) {
		if tool, ok := availableTools["glob"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: `{"pattern": "**/*.go"}`,
				},
			})
		} else if tool, ok := availableTools["Glob"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: `{"pattern": "**/*.go"}`,
				},
			})
		}
	}

	// Case 3: Search for specific content - use grep
	if containsAny(topicLower, []string{"search for", "find", "where is", "look for", "locate"}) {
		// Extract what to search for from the topic
		searchTerm := extractSearchTerm(topic)
		if searchTerm != "" {
			if tool, ok := availableTools["grep"]; ok {
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"pattern": "%s"}`, escapeJSONString(searchTerm)),
					},
				})
			} else if tool, ok := availableTools["Grep"]; ok {
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"pattern": "%s"}`, escapeJSONString(searchTerm)),
					},
				})
			}
		}
	}

	// Case 4: Read a specific file - use read
	if containsAny(topicLower, []string{"read ", "show me ", "what's in ", "contents of ", "open "}) {
		// Extract file path from the topic
		filePath := extractFilePath(topic)
		if filePath != "" {
			if tool, ok := availableTools["read"]; ok {
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"file_path": "%s"}`, escapeJSONString(filePath)),
					},
				})
			} else if tool, ok := availableTools["Read"]; ok {
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"file_path": "%s"}`, escapeJSONString(filePath)),
					},
				})
			}
		}
	}

	// Case 5: Execute command - use shell/bash
	if containsAny(topicLower, []string{"run ", "execute ", "command "}) {
		command := extractCommand(topic)
		if command != "" {
			desc := generateBashDescription(command)
			if tool, ok := availableTools["shell"]; ok {
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"command": "%s", "description": "%s"}`, escapeJSONString(command), escapeJSONString(desc)),
					},
				})
			} else if tool, ok := availableTools["Bash"]; ok {
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"command": "%s", "description": "%s"}`, escapeJSONString(command), escapeJSONString(desc)),
					},
				})
			}
		}
	}

	// Case 6: Create/Write file - use Write tool
	// CRITICAL: This enables the AI to actually CREATE files like AGENTS.md
	if containsAny(topicLower, []string{"create ", "write ", "generate ", "make ", "add "}) &&
		containsAny(topicLower, []string{".md", ".txt", ".json", ".yaml", ".yml", ".go", ".py", ".js", ".ts", "file", "document"}) {
		// Extract the file path from the topic
		filePath := extractCreateFilePath(topic)
		if filePath != "" {
			// Check for Write tool (case-insensitive)
			if tool, ok := availableTools["write"]; ok {
				// Extract or generate content from the synthesis
				content := extractFileContent(synthesis, filePath, topic)
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"file_path": "%s", "content": "%s"}`, escapeJSONString(filePath), escapeJSONString(content)),
					},
				})
			} else if tool, ok := availableTools["Write"]; ok {
				content := extractFileContent(synthesis, filePath, topic)
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"file_path": "%s", "content": "%s"}`, escapeJSONString(filePath), escapeJSONString(content)),
					},
				})
			}
		}
	}

	// Case 7: Git operations - use Git tool
	if containsAny(topicLower, []string{"git status", "git log", "git diff", "git commit", "git branch", "git checkout", "version control", "commit history", "commits", "branches"}) {
		operation := "status"
		if containsAny(topicLower, []string{"log", "history", "commits"}) {
			operation = "log"
		} else if containsAny(topicLower, []string{"diff", "changes", "difference"}) {
			operation = "diff"
		} else if containsAny(topicLower, []string{"branch", "branches"}) {
			operation = "branch"
		} else if containsAny(topicLower, []string{"commit"}) {
			operation = "commit"
		}
		desc := fmt.Sprintf("Git %s operation", operation)
		if tool, ok := availableTools["Git"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"operation": "%s", "description": "%s"}`, operation, desc),
				},
			})
		} else if tool, ok := availableTools["git"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"operation": "%s", "description": "%s"}`, operation, desc),
				},
			})
		}
	}

	// Case 8: Run tests - use Test tool
	if containsAny(topicLower, []string{"run test", "execute test", "test coverage", "run the tests", "unit test", "integration test", "pytest", "jest", "go test", "npm test"}) {
		testPath := "./..."
		coverage := containsAny(topicLower, []string{"coverage"})
		verbose := true
		if tool, ok := availableTools["Test"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"test_path": "%s", "coverage": %t, "verbose": %t, "description": "Run tests"}`, testPath, coverage, verbose),
				},
			})
		} else if tool, ok := availableTools["test"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"test_path": "%s", "coverage": %t, "verbose": %t, "description": "Run tests"}`, testPath, coverage, verbose),
				},
			})
		}
	}

	// Case 9: Lint code - use Lint tool
	if containsAny(topicLower, []string{"lint", "code quality", "style check", "golangci", "eslint", "pylint", "check style"}) {
		if tool, ok := availableTools["Lint"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: `{"fix": false, "description": "Run linter checks"}`,
				},
			})
		} else if tool, ok := availableTools["lint"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: `{"fix": false, "description": "Run linter checks"}`,
				},
			})
		}
	}

	// Case 10: Show diff - use Diff tool
	if containsAny(topicLower, []string{"show diff", "what changed", "show changes", "compare", "difference between"}) {
		if tool, ok := availableTools["Diff"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: `{"staged": false, "description": "Show file differences"}`,
				},
			})
		} else if tool, ok := availableTools["diff"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: `{"staged": false, "description": "Show file differences"}`,
				},
			})
		}
	}

	// Case 11: Directory tree - use TreeView tool
	if containsAny(topicLower, []string{"directory tree", "folder structure", "file tree", "tree view", "show tree", "project tree"}) {
		if tool, ok := availableTools["TreeView"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: `{"path": ".", "max_depth": 3, "description": "Show directory tree"}`,
				},
			})
		} else if tool, ok := availableTools["treeview"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: `{"path": ".", "max_depth": 3, "description": "Show directory tree"}`,
				},
			})
		} else if tool, ok := availableTools["tree"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: `{"path": ".", "max_depth": 3, "description": "Show directory tree"}`,
				},
			})
		}
	}

	// Case 12: File info - use FileInfo tool
	if containsAny(topicLower, []string{"file info", "file size", "file permissions", "file metadata", "file stats"}) {
		filePath := extractFilePath(topic)
		if filePath == "" {
			filePath = "."
		}
		if tool, ok := availableTools["FileInfo"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"file_path": "%s", "description": "Get file information"}`, escapeJSONString(filePath)),
				},
			})
		} else if tool, ok := availableTools["fileinfo"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"file_path": "%s", "description": "Get file information"}`, escapeJSONString(filePath)),
				},
			})
		}
	}

	// Case 13: Code symbols - use Symbols tool
	if containsAny(topicLower, []string{"symbols", "functions in", "classes in", "methods in", "list functions", "list classes", "code outline"}) {
		if tool, ok := availableTools["Symbols"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: `{"description": "List code symbols"}`,
				},
			})
		} else if tool, ok := availableTools["symbols"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: `{"description": "List code symbols"}`,
				},
			})
		}
	}

	// Case 14: Find references - use References tool
	if containsAny(topicLower, []string{"find references", "where is used", "usages of", "references to", "who calls", "callers of"}) {
		symbol := extractSymbolName(topic)
		if symbol != "" {
			if tool, ok := availableTools["References"]; ok {
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"symbol": "%s", "description": "Find references to %s"}`, escapeJSONString(symbol), escapeJSONString(symbol)),
					},
				})
			} else if tool, ok := availableTools["references"]; ok {
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"symbol": "%s", "description": "Find references to %s"}`, escapeJSONString(symbol), escapeJSONString(symbol)),
					},
				})
			} else if tool, ok := availableTools["refs"]; ok {
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"symbol": "%s", "description": "Find references to %s"}`, escapeJSONString(symbol), escapeJSONString(symbol)),
					},
				})
			}
		}
	}

	// Case 15: Go to definition - use Definition tool
	if containsAny(topicLower, []string{"go to definition", "definition of", "where is defined", "jump to", "navigate to"}) {
		symbol := extractSymbolName(topic)
		if symbol != "" {
			if tool, ok := availableTools["Definition"]; ok {
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"symbol": "%s", "description": "Find definition of %s"}`, escapeJSONString(symbol), escapeJSONString(symbol)),
					},
				})
			} else if tool, ok := availableTools["definition"]; ok {
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"symbol": "%s", "description": "Find definition of %s"}`, escapeJSONString(symbol), escapeJSONString(symbol)),
					},
				})
			} else if tool, ok := availableTools["goto"]; ok {
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: fmt.Sprintf(`{"symbol": "%s", "description": "Find definition of %s"}`, escapeJSONString(symbol), escapeJSONString(symbol)),
					},
				})
			}
		}
	}

	// Case 16: Pull requests - use PR tool
	if containsAny(topicLower, []string{"pull request", "pr ", "create pr", "list pr", "merge pr", "pull requests"}) {
		action := "list"
		if containsAny(topicLower, []string{"create", "open", "new"}) {
			action = "create"
		} else if containsAny(topicLower, []string{"merge"}) {
			action = "merge"
		} else if containsAny(topicLower, []string{"review"}) {
			action = "review"
		}
		if tool, ok := availableTools["PR"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"action": "%s", "description": "%s pull request"}`, action, action),
				},
			})
		} else if tool, ok := availableTools["pr"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"action": "%s", "description": "%s pull request"}`, action, action),
				},
			})
		} else if tool, ok := availableTools["pullrequest"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"action": "%s", "description": "%s pull request"}`, action, action),
				},
			})
		}
	}

	// Case 17: Issues - use Issue tool
	if containsAny(topicLower, []string{"issue", "bug report", "create issue", "list issues", "close issue"}) {
		action := "list"
		if containsAny(topicLower, []string{"create", "open", "new", "report"}) {
			action = "create"
		} else if containsAny(topicLower, []string{"close", "resolve"}) {
			action = "close"
		} else if containsAny(topicLower, []string{"comment"}) {
			action = "comment"
		}
		if tool, ok := availableTools["Issue"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"action": "%s", "description": "%s issue"}`, action, action),
				},
			})
		} else if tool, ok := availableTools["issue"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"action": "%s", "description": "%s issue"}`, action, action),
				},
			})
		}
	}

	// Case 18: CI/CD Workflows - use Workflow tool
	if containsAny(topicLower, []string{"workflow", "ci/cd", "pipeline", "github action", "ci status", "build status"}) {
		action := "status"
		if containsAny(topicLower, []string{"trigger", "run", "start"}) {
			action = "trigger"
		} else if containsAny(topicLower, []string{"list"}) {
			action = "list"
		} else if containsAny(topicLower, []string{"log", "output"}) {
			action = "logs"
		}
		if tool, ok := availableTools["Workflow"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"action": "%s", "description": "CI/CD workflow %s"}`, action, action),
				},
			})
		} else if tool, ok := availableTools["workflow"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"action": "%s", "description": "CI/CD workflow %s"}`, action, action),
				},
			})
		} else if tool, ok := availableTools["ci"]; ok {
			toolCalls = append(toolCalls, StreamingToolCall{
				Index: len(toolCalls),
				ID:    fmt.Sprintf("call_%s", generateToolCallID()),
				Type:  "function",
				Function: OpenAIFunctionCall{
					Name:      tool.Function.Name,
					Arguments: fmt.Sprintf(`{"action": "%s", "description": "CI/CD workflow %s"}`, action, action),
				},
			})
		}
	}

	// Case 19: Analysis of synthesis - if synthesis mentions specific tools, try to call them
	// This handles cases where the debate concluded that a specific tool should be used
	for toolName, tool := range availableTools {
		toolNameLower := strings.ToLower(toolName)
		if strings.Contains(synthesisLower, "use "+toolNameLower) ||
			strings.Contains(synthesisLower, "call "+toolNameLower) ||
			strings.Contains(synthesisLower, "invoke "+toolNameLower) ||
			strings.Contains(synthesisLower, toolNameLower+" tool") {
			// Check if we haven't already added this tool
			alreadyAdded := false
			for _, tc := range toolCalls {
				if tc.Function.Name == toolName {
					alreadyAdded = true
					break
				}
			}
			if !alreadyAdded {
				// Try to extract arguments from synthesis context
				args := extractToolArguments(toolName, synthesis)
				toolCalls = append(toolCalls, StreamingToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%s", generateToolCallID()),
					Type:  "function",
					Function: OpenAIFunctionCall{
						Name:      tool.Function.Name,
						Arguments: args,
					},
				})
			}
		}
	}

	// ALWAYS use LLM-based tool call generation when tools are available
	// NO hardcoded patterns - let the LLM intelligently decide what tools to call
	// based on the full conversation context, synthesis, and user intent
	if len(toolCalls) == 0 && len(tools) > 0 {
		truncatedTopic := topic
		if len(truncatedTopic) > 50 {
			truncatedTopic = truncatedTopic[:50]
		}
		logrus.WithFields(logrus.Fields{
			"topic":         truncatedTopic,
			"topic_empty":   topic == "",
			"message_count": len(messages),
		}).Info("Using LLM-based tool call generation")
		llmToolCalls := h.generateLLMBasedToolCalls(ctx, messages, tools, synthesis)
		if len(llmToolCalls) > 0 {
			toolCalls = append(toolCalls, llmToolCalls...)
			logrus.WithField("tool_count", len(llmToolCalls)).Info("LLM generated tool calls")
		} else {
			logrus.WithFields(logrus.Fields{
				"topic_empty":     topic == "",
				"synthesis_empty": synthesis == "",
			}).Debug("LLM-based tool generation returned empty")
		}
	}

	logrus.WithFields(logrus.Fields{
		"topic":           topic[:min(50, len(topic))],
		"tool_count":      len(toolCalls),
		"available_tools": len(availableTools),
	}).Debug("Generated action tool calls from debate synthesis")

	// CRITICAL: Validate all tool calls before returning
	// This prevents sending tool calls with missing/invalid arguments to clients
	validatedToolCalls := validateAndFilterToolCalls(toolCalls)
	if len(validatedToolCalls) != len(toolCalls) {
		logrus.WithFields(logrus.Fields{
			"original_count":  len(toolCalls),
			"validated_count": len(validatedToolCalls),
		}).Warn("Some tool calls were filtered due to invalid arguments")
	}

	return validatedToolCalls
}

// containsAny checks if text contains any of the patterns
func containsAny(text string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}

// validateAndFilterToolCalls validates all tool calls and filters out invalid ones
// CRITICAL: This ensures all tool calls have valid arguments before being sent to clients
// This prevents errors like "filePath: expected string, received undefined"
func validateAndFilterToolCalls(toolCalls []StreamingToolCall) []StreamingToolCall {
	if len(toolCalls) == 0 {
		return toolCalls
	}

	// Define required fields for each tool type (using snake_case as per schema)
	toolRequiredFields := map[string][]string{
		// Filesystem tools
		"read":  {"file_path"},
		"Read":  {"file_path"},
		"write": {"file_path", "content"},
		"Write": {"file_path", "content"},
		"edit":  {"file_path", "old_string", "new_string"},
		"Edit":  {"file_path", "old_string", "new_string"},
		"glob":  {"pattern"},
		"Glob":  {"pattern"},
		"grep":  {"pattern"},
		"Grep":  {"pattern"},
		// Core tools
		"bash":  {"command", "description"},
		"Bash":  {"command", "description"},
		"shell": {"command", "description"},
		"task":  {"prompt", "description", "subagent_type"},
		"Task":  {"prompt", "description", "subagent_type"},
		// Version control
		"git":  {"operation", "description"},
		"Git":  {"operation", "description"},
		"diff": {"description"},
		"Diff": {"description"},
		// Testing
		"test": {"description"},
		"Test": {"description"},
		"lint": {"description"},
		"Lint": {"description"},
		// Code intelligence
		"treeview":   {"description"},
		"TreeView":   {"description"},
		"fileinfo":   {"file_path", "description"},
		"FileInfo":   {"file_path", "description"},
		"symbols":    {"description"},
		"Symbols":    {"description"},
		"references": {"symbol", "description"},
		"References": {"symbol", "description"},
		"definition": {"symbol", "description"},
		"Definition": {"symbol", "description"},
		// Workflow tools
		"pr":       {"action", "description"},
		"PR":       {"action", "description"},
		"issue":    {"action", "description"},
		"Issue":    {"action", "description"},
		"workflow": {"action", "description"},
		"Workflow": {"action", "description"},
		// Web tools
		"webfetch":  {"url", "prompt"},
		"WebFetch":  {"url", "prompt"},
		"websearch": {"query"},
		"WebSearch": {"query"},
	}

	var validToolCalls []StreamingToolCall

	for _, tc := range toolCalls {
		toolName := tc.Function.Name
		requiredFields, hasRequirements := toolRequiredFields[toolName]

		// If no requirements defined, pass through (but log warning)
		if !hasRequirements {
			logrus.WithField("tool", toolName).Debug("No validation rules for tool, passing through")
			validToolCalls = append(validToolCalls, tc)
			continue
		}

		// Parse the arguments JSON
		var args map[string]interface{}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			logrus.WithFields(logrus.Fields{
				"tool":      toolName,
				"arguments": tc.Function.Arguments,
				"error":     err.Error(),
			}).Warn("Failed to parse tool call arguments, skipping")
			continue
		}

		// Check all required fields
		isValid := true
		missingFields := []string{}
		emptyFields := []string{}

		for _, field := range requiredFields {
			val, exists := args[field]
			if !exists {
				isValid = false
				missingFields = append(missingFields, field)
				continue
			}

			// Check for empty strings
			if strVal, ok := val.(string); ok && strings.TrimSpace(strVal) == "" {
				isValid = false
				emptyFields = append(emptyFields, field)
			}

			// Check for nil values
			if val == nil {
				isValid = false
				missingFields = append(missingFields, field)
			}
		}

		if isValid {
			validToolCalls = append(validToolCalls, tc)
		} else {
			logrus.WithFields(logrus.Fields{
				"tool":           toolName,
				"missing_fields": missingFields,
				"empty_fields":   emptyFields,
				"arguments":      tc.Function.Arguments,
			}).Warn("Tool call has invalid arguments, filtering out")
		}
	}

	return validToolCalls
}

// generateToolCallID generates a cryptographically secure unique ID for a tool call
func generateToolCallID() string {
	return utils.SecureRandomID("call")
}

// escapeJSONString escapes a string for safe inclusion in JSON
func escapeJSONString(s string) string {
	// Basic JSON escaping
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\t", "\\t")
	return s
}

// Pre-compiled patterns for sanitizeDisplayContent — called on every response.
var (
	sanitizeSystemReminderRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	sanitizeCommandNameRe    = regexp.MustCompile(`(?s)<command-name>.*?</command-name>`)
	sanitizeContextRe        = regexp.MustCompile(`(?s)<context>.*?</context>`)
	sanitizeExcessNewlinesRe = regexp.MustCompile(`\n{3,}`)

	// Pre-compiled patterns for extractSearchPatternFromContext
	searchDoubleQuoteRe = regexp.MustCompile(`"([^"]+)"`)
	searchSingleQuoteRe = regexp.MustCompile(`'([^']+)'`)

	// Pre-compiled patterns for parseEmbeddedFunctionCalls
	embeddedFuncRe  = regexp.MustCompile(`(?s)<function=(\w+)>(.*?)</function>`)
	embeddedParamRe = regexp.MustCompile(`(?s)<parameter=(\w+)>(.*?)</parameter>`)
	embeddedFCRe    = regexp.MustCompile(`(?s)<function_call\s+name="(\w+)">(.*?)</function_call>`)
)

// sanitizeDisplayContent removes system-level tags that should not be displayed to users
// This includes <system-reminder>, <command-name>, and similar tags that are for internal use
func sanitizeDisplayContent(content string) string {
	if content == "" {
		return content
	}

	content = sanitizeSystemReminderRe.ReplaceAllString(content, "")
	content = sanitizeCommandNameRe.ReplaceAllString(content, "")
	content = sanitizeContextRe.ReplaceAllString(content, "")
	content = sanitizeExcessNewlinesRe.ReplaceAllString(content, "\n\n")
	content = strings.TrimSpace(content)

	return content
}

// convertOpenAIToolsToModelTools converts OpenAI format tools to models.Tool format
func (h *UnifiedHandler) convertOpenAIToolsToModelTools(tools []OpenAITool) []models.Tool {
	result := make([]models.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool.Type == "function" {
			modelTool := models.Tool{
				Type: "function",
				Function: models.ToolFunction{
					Name:        tool.Function.Name,
					Description: tool.Function.Description,
					Parameters:  tool.Function.Parameters,
				},
			}
			result = append(result, modelTool)
		}
	}
	return result
}

// convertModelToolCallsToStreamingToolCalls converts models.ToolCall to StreamingToolCall
func (h *UnifiedHandler) convertModelToolCallsToStreamingToolCalls(toolCalls []models.ToolCall) []StreamingToolCall {
	result := make([]StreamingToolCall, 0, len(toolCalls))
	for i, tc := range toolCalls {
		result = append(result, StreamingToolCall{
			Index: i,
			ID:    tc.ID,
			Type:  tc.Type,
			Function: OpenAIFunctionCall{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}
	return result
}

// generateLLMBasedToolCalls makes an actual LLM call with tools enabled to get proper tool_calls
// CRITICAL: This function enables real tool execution based on LLM decisions, not pattern matching
// It sends the full conversation context to the LLM and lets the model decide which tools to call
func (h *UnifiedHandler) generateLLMBasedToolCalls(ctx context.Context, messages []OpenAIMessage, tools []OpenAITool, synthesis string) []StreamingToolCall {
	if h.providerRegistry == nil || h.debateTeamConfig == nil || len(tools) == 0 {
		return nil
	}

	// Build tool descriptions for the system prompt
	var toolDescriptions strings.Builder
	toolDescriptions.WriteString("You have access to the following tools:\n\n")
	for _, tool := range tools {
		if tool.Type == "function" {
			toolDescriptions.WriteString(fmt.Sprintf("- %s: %s\n", tool.Function.Name, tool.Function.Description))
			if tool.Function.Parameters != nil {
				if props, ok := tool.Function.Parameters["properties"].(map[string]interface{}); ok {
					toolDescriptions.WriteString("  Parameters:\n")
					for name, details := range props {
						if detailsMap, ok := details.(map[string]interface{}); ok {
							desc := ""
							if d, ok := detailsMap["description"].(string); ok {
								desc = d
							}
							toolDescriptions.WriteString(fmt.Sprintf("    - %s: %s\n", name, desc))
						}
					}
				}
			}
			toolDescriptions.WriteString("\n")
		}
	}

	// Build the system prompt for tool selection
	systemPrompt := fmt.Sprintf(`You are an AI coding assistant that can execute tools to help users with their tasks.

%s
IMPORTANT INSTRUCTIONS:
1. Based on the conversation context and synthesis, determine what tool(s) should be called
2. You MUST use tools to take action - do not just describe what you would do
3. If the user has confirmed (e.g., "yes", "proceed", "go ahead"), immediately execute the planned action
4. If exploring a codebase, start with Glob to list files, then Read to examine specific files
5. For search queries, use Grep to find patterns in code
6. For writing files, use Write with the full file path and content
7. For running commands, use Bash with appropriate commands

The AI Debate Team has synthesized the following consensus:
%s

Based on this context, call the appropriate tool(s) to execute the planned action.`, toolDescriptions.String(), synthesis)

	// Build the user prompt from conversation context
	var userPromptBuilder strings.Builder
	userPromptBuilder.WriteString("Conversation context:\n\n")
	for i, msg := range messages {
		if msg.Role == "user" || msg.Role == "assistant" {
			prefix := "User"
			if msg.Role == "assistant" {
				prefix = "Assistant"
			}
			// Truncate long messages
			content := msg.Content
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			userPromptBuilder.WriteString(fmt.Sprintf("[%d] %s: %s\n\n", i+1, prefix, content))
		}
	}
	userPromptBuilder.WriteString("\nBased on this conversation, call the appropriate tool(s) NOW to help the user.")

	// Get the mediator (or first available) member for the tool call
	mediatorMember := h.debateTeamConfig.GetTeamMember(services.PositionMediator)
	if mediatorMember == nil {
		// Try other positions
		positions := []services.DebateTeamPosition{
			services.PositionSynthesis,
			services.PositionAnalyst,
			services.PositionProposer,
			services.PositionCritic,
		}
		for _, pos := range positions {
			mediatorMember = h.debateTeamConfig.GetTeamMember(pos)
			if mediatorMember != nil {
				break
			}
		}
	}
	if mediatorMember == nil {
		logrus.Warn("No provider available for LLM-based tool call generation")
		return nil
	}

	// Get provider
	provider, err := h.getProviderForMember(mediatorMember)
	if err != nil {
		// Try fallbacks
		currentMember := mediatorMember.Fallback
		for currentMember != nil {
			provider, err = h.getProviderForMember(currentMember)
			if err == nil {
				break
			}
			currentMember = currentMember.Fallback
		}
		if provider == nil {
			logrus.WithError(err).Warn("Failed to get provider for LLM-based tool calls")
			return nil
		}
	}

	// Create the LLM request with tools enabled
	llmReq := &models.LLMRequest{
		ID:        utils.SecureRandomID("tool-call"), // D-20: crypto-random, collision-free
		SessionID: "tool-call-session",
		Prompt:    userPromptBuilder.String(),
		Messages: []models.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPromptBuilder.String()},
		},
		ModelParams: models.ModelParameters{
			Model:       mediatorMember.ModelName,
			Temperature: 0.3, // Lower temperature for more deterministic tool selection
			MaxTokens:   1024,
		},
		Tools:      h.convertOpenAIToolsToModelTools(tools),
		ToolChoice: "auto", // Let the LLM decide when to use tools
	}

	// Also pass via ProviderSpecific for backward compatibility with providers that check there
	llmReq.ModelParams.ProviderSpecific = map[string]interface{}{
		"tools":       tools,
		"tool_choice": "auto",
	}

	// Call the LLM with timeout
	// CRITICAL: OAuth providers need longer timeouts
	timeout := 20 * time.Second
	if mediatorMember.IsOAuth {
		timeout = 45 * time.Second
	}
	llmCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := provider.Complete(llmCtx, llmReq)
	if err != nil {
		logrus.WithError(err).WithField("provider", mediatorMember.ProviderName).Warn("LLM tool call generation failed")
		return nil
	}

	// Check if the response contains tool_calls
	if len(resp.ToolCalls) > 0 {
		logrus.WithFields(logrus.Fields{
			"provider":        mediatorMember.ProviderName,
			"tool_calls":      len(resp.ToolCalls),
			"first_tool_name": resp.ToolCalls[0].Function.Name,
		}).Info("LLM generated tool calls successfully")
		return h.convertModelToolCallsToStreamingToolCalls(resp.ToolCalls)
	}

	// No tool calls in response - the LLM decided not to use tools
	logrus.WithField("provider", mediatorMember.ProviderName).Debug("LLM did not generate any tool calls")
	return nil
}

// extractSearchTerm extracts the search term from a query
func extractSearchTerm(topic string) string {
	// Simple extraction - look for patterns like "search for X" or "find X"
	patterns := []string{
		"search for ",
		"find ",
		"look for ",
		"locate ",
		"where is ",
	}
	topicLower := strings.ToLower(topic)
	for _, pattern := range patterns {
		idx := strings.Index(topicLower, pattern)
		if idx != -1 {
			remaining := topic[idx+len(pattern):]
			// Take until end of sentence or special character
			endIdx := strings.IndexAny(remaining, ".,?!;:")
			if endIdx != -1 {
				return strings.TrimSpace(remaining[:endIdx])
			}
			return strings.TrimSpace(remaining)
		}
	}
	return ""
}

// extractSymbolName extracts a symbol (function/class/variable) name from a query
func extractSymbolName(topic string) string {
	// Look for symbol patterns
	patterns := []string{
		"definition of ",
		"references to ",
		"references of ",
		"usages of ",
		"callers of ",
		"who calls ",
		"where is ",
		"find ",
		"symbol ",
		"function ",
		"method ",
		"class ",
		"variable ",
	}
	topicLower := strings.ToLower(topic)
	for _, pattern := range patterns {
		idx := strings.Index(topicLower, pattern)
		if idx != -1 {
			remaining := topic[idx+len(pattern):]
			// Take until whitespace, punctuation, or end
			words := strings.Fields(remaining)
			if len(words) > 0 {
				// Clean up the symbol name - remove quotes and special chars
				symbol := strings.Trim(words[0], "\"'`,.:;!?(){}[]")
				if symbol != "" {
					return symbol
				}
			}
		}
	}
	return ""
}

// extractFilePath extracts a file path from a query
func extractFilePath(topic string) string {
	// Look for file path patterns
	patterns := []string{
		"read ",
		"show me ",
		"what's in ",
		"contents of ",
		"open ",
		"file ",
	}
	topicLower := strings.ToLower(topic)
	for _, pattern := range patterns {
		idx := strings.Index(topicLower, pattern)
		if idx != -1 {
			remaining := topic[idx+len(pattern):]
			// Take until whitespace or end
			endIdx := strings.IndexAny(remaining, " \t\n")
			if endIdx != -1 {
				return strings.TrimSpace(remaining[:endIdx])
			}
			return strings.TrimSpace(remaining)
		}
	}
	return ""
}

// extractCommand extracts a command from a query
func extractCommand(topic string) string {
	// Look for command patterns
	patterns := []string{
		"run ",
		"execute ",
		"command ",
	}
	topicLower := strings.ToLower(topic)
	for _, pattern := range patterns {
		idx := strings.Index(topicLower, pattern)
		if idx != -1 {
			remaining := topic[idx+len(pattern):]
			// Take until end of line
			endIdx := strings.Index(remaining, "\n")
			if endIdx != -1 {
				return strings.TrimSpace(remaining[:endIdx])
			}
			return strings.TrimSpace(remaining)
		}
	}
	return ""
}

// extractCreateFilePath extracts a file path from a create/write/generate request
func extractCreateFilePath(topic string) string {
	// Look for file creation patterns
	patterns := []string{
		"create ",
		"write ",
		"generate ",
		"make ",
		"add ",
	}
	topicLower := strings.ToLower(topic)

	for _, pattern := range patterns {
		idx := strings.Index(topicLower, pattern)
		if idx != -1 {
			remaining := topic[idx+len(pattern):]
			// Look for file path-like patterns (words with extensions or paths)
			words := strings.Fields(remaining)
			for _, word := range words {
				// Clean up the word (remove quotes, commas, etc.)
				word = strings.Trim(word, "\"'`,.:;!?")
				// Check if it looks like a file path or has an extension
				if strings.Contains(word, ".") || strings.Contains(word, "/") {
					// Handle relative paths and filenames
					if !strings.HasPrefix(word, "/") && !strings.HasPrefix(word, "./") {
						// Add ./ for relative paths if it doesn't start with a path
						if !strings.Contains(word, "/") {
							return "./" + word
						}
					}
					return word
				}
				// Special handling for common file names without context
				wordLower := strings.ToLower(word)
				if wordLower == "agents" || wordLower == "readme" || wordLower == "changelog" {
					// Check if next word has extension
					wordsIdx := strings.Index(remaining, word)
					if wordsIdx != -1 {
						afterWord := remaining[wordsIdx+len(word):]
						if strings.HasPrefix(strings.TrimSpace(strings.ToLower(afterWord)), ".md") {
							return "./" + word + ".md"
						}
					}
					return "./" + word + ".md"
				}
			}
		}
	}

	// Also look for explicit file mentions
	filePatterns := []string{"file ", "named ", "called "}
	for _, pattern := range filePatterns {
		idx := strings.Index(topicLower, pattern)
		if idx != -1 {
			remaining := topic[idx+len(pattern):]
			words := strings.Fields(remaining)
			if len(words) > 0 {
				word := strings.Trim(words[0], "\"'`,.:;!?")
				if strings.Contains(word, ".") {
					if !strings.HasPrefix(word, "/") && !strings.HasPrefix(word, "./") {
						return "./" + word
					}
					return word
				}
			}
		}
	}

	return ""
}

// extractFileContent extracts or generates content for a file from the synthesis
func extractFileContent(synthesis, filePath, topic string) string {
	// Check if synthesis already contains markdown-like content that could be file content
	// Look for code blocks or structured content in the synthesis

	// First, try to find content between code blocks
	if idx := strings.Index(synthesis, "```"); idx != -1 {
		afterStart := synthesis[idx+3:]
		// Skip the language identifier if present
		if nlIdx := strings.Index(afterStart, "\n"); nlIdx != -1 {
			afterStart = afterStart[nlIdx+1:]
		}
		if endIdx := strings.Index(afterStart, "```"); endIdx != -1 {
			return strings.TrimSpace(afterStart[:endIdx])
		}
	}

	// If the synthesis describes what should be in the file, use the synthesis as basis
	// Look for file description patterns
	descPatterns := []string{
		"the file should contain",
		"should include",
		"will contain",
		"should have",
		"content should be",
		"document should describe",
	}

	synthesisLower := strings.ToLower(synthesis)
	for _, pattern := range descPatterns {
		if idx := strings.Index(synthesisLower, pattern); idx != -1 {
			// Use the synthesis from this point as a guide
			break
		}
	}

	// Generate appropriate default content based on file type
	filePathLower := strings.ToLower(filePath)
	topicLower := strings.ToLower(topic)

	if strings.HasSuffix(filePathLower, "agents.md") || strings.Contains(topicLower, "agents") {
		return generateAgentsMDContent(synthesis, topic)
	}

	if strings.HasSuffix(filePathLower, "readme.md") {
		return generateReadmeMDContent(synthesis, topic)
	}

	// Default: use a cleaned-up version of the synthesis
	// Remove conversational phrases and format as document content
	content := cleanSynthesisForFile(synthesis)
	if content == "" {
		content = fmt.Sprintf("# %s\n\nGenerated based on: %s\n\n%s",
			strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath)),
			topic,
			synthesis)
	}

	return content
}

// generateAgentsMDContent generates content for an AGENTS.md file
func generateAgentsMDContent(synthesis, topic string) string {
	var content strings.Builder

	content.WriteString("# AGENTS.md\n\n")
	content.WriteString("This file provides guidance to AI coding agents working with this codebase.\n\n")

	content.WriteString("## Project Overview\n\n")
	content.WriteString("Based on analysis of the codebase:\n\n")

	// Extract relevant information from synthesis
	if synthesis != "" {
		// Clean and format the synthesis
		cleaned := cleanSynthesisForFile(synthesis)
		if cleaned != "" {
			content.WriteString(cleaned)
			content.WriteString("\n\n")
		}
	}

	content.WriteString("## Key Guidelines\n\n")
	content.WriteString("- Follow existing code patterns and conventions\n")
	content.WriteString("- Maintain consistent formatting and style\n")
	content.WriteString("- Write clear, concise code with appropriate comments\n")
	content.WriteString("- Update tests when modifying functionality\n\n")

	content.WriteString("## Important Files\n\n")
	content.WriteString("- See project structure for key entry points\n")
	content.WriteString("- Configuration files in project root\n\n")

	return content.String()
}

// generateReadmeMDContent generates content for a README.md file
func generateReadmeMDContent(synthesis, topic string) string {
	var content strings.Builder

	// Use topic for title if available, otherwise use generic title
	title := "Project"
	if topic != "" {
		title = extractTitleFromTopic(topic)
	}

	content.WriteString(fmt.Sprintf("# %s\n\n", title))
	content.WriteString("## Description\n\n")

	if synthesis != "" {
		cleaned := cleanSynthesisForFile(synthesis)
		if cleaned != "" {
			content.WriteString(cleaned)
			content.WriteString("\n\n")
		}
	} else if topic != "" {
		content.WriteString(fmt.Sprintf("This project addresses: %s\n\n", topic))
	}

	content.WriteString("## Getting Started\n\n")
	content.WriteString("### Prerequisites\n\n")
	content.WriteString("Ensure you have the required dependencies installed for your development environment.\n\n")
	content.WriteString("### Installation\n\n")
	content.WriteString("1. Clone the repository\n")
	content.WriteString("2. Install dependencies\n")
	content.WriteString("3. Configure your environment\n")
	content.WriteString("4. Run the application\n\n")

	content.WriteString("### Usage\n\n")
	if topic != "" {
		content.WriteString(fmt.Sprintf("This project can be used to %s.\n\n", strings.ToLower(topic)))
	} else {
		content.WriteString("Refer to the documentation for usage instructions.\n\n")
	}

	content.WriteString("## Contributing\n\n")
	content.WriteString("Contributions are welcome. Please follow these guidelines:\n\n")
	content.WriteString("1. Fork the repository\n")
	content.WriteString("2. Create a feature branch\n")
	content.WriteString("3. Make your changes\n")
	content.WriteString("4. Submit a pull request\n\n")

	content.WriteString("## License\n\n")
	content.WriteString("See LICENSE file for details.\n")

	return content.String()
}

// extractTitleFromTopic extracts a clean title from the topic string
func extractTitleFromTopic(topic string) string {
	// Remove common prefixes
	result := strings.ToLower(topic)
	result = strings.TrimPrefix(result, "create ")
	result = strings.TrimPrefix(result, "write ")
	result = strings.TrimPrefix(result, "generate ")
	result = strings.TrimPrefix(result, "make ")

	// Remove file references
	result = strings.TrimSuffix(result, " readme")
	result = strings.TrimSuffix(result, " readme.md")
	result = strings.TrimSuffix(result, ".md")

	// Capitalize first letter of each word
	words := strings.Fields(result)
	for i, word := range words {
		if len(word) > 0 {
			words[i] = strings.ToUpper(string(word[0])) + word[1:]
		}
	}
	result = strings.Join(words, " ")

	// If still empty or too short, use default
	if len(result) < 3 {
		return "Project"
	}

	// Truncate if too long
	if len(result) > 50 {
		result = result[:50] + "..."
	}

	return result
}

// cleanSynthesisForFile cleans synthesis text for use as file content
func cleanSynthesisForFile(synthesis string) string {
	// Remove common conversational phrases
	removePatterns := []string{
		"Based on my analysis",
		"I would suggest",
		"I recommend",
		"Let me explain",
		"Here's what I found",
		"In summary",
		"To summarize",
		"The consensus is",
	}

	result := synthesis
	for _, pattern := range removePatterns {
		result = strings.ReplaceAll(result, pattern, "")
	}

	// Clean up multiple newlines
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}

	return strings.TrimSpace(result)
}

// extractToolArguments attempts to extract appropriate arguments for a tool from context
func extractToolArguments(toolName string, context string) string {
	toolNameLower := strings.ToLower(toolName)
	switch toolNameLower {
	case "glob":
		// Default glob pattern
		return `{"pattern": "**/*"}`
	case "grep":
		// Try to extract a search pattern from context
		pattern := extractSearchPatternFromContext(context)
		if pattern == "" {
			pattern = ".*"
		}
		return fmt.Sprintf(`{"pattern": "%s"}`, escapeJSONString(pattern))
	case "read":
		path := extractFilePathFromContext(context)
		if path == "" {
			path = "README.md"
		}
		return fmt.Sprintf(`{"file_path": "%s"}`, escapeJSONString(path))
	case "write":
		path := extractFilePathFromContext(context)
		if path == "" {
			path = "output.md"
		}
		content := extractContentForFile(context, path)
		return fmt.Sprintf(`{"file_path": "%s", "content": "%s"}`, escapeJSONString(path), escapeJSONString(content))
	case "edit":
		path := extractFilePathFromContext(context)
		if path == "" {
			path = "file.txt"
		}
		return fmt.Sprintf(`{"file_path": "%s", "old_string": "", "new_string": ""}`, escapeJSONString(path))
	case "bash", "shell":
		// CRITICAL: Bash tool REQUIRES both command AND description fields
		cmd := extractCommandFromContext(context)
		if cmd == "" {
			cmd = "echo 'Ready to execute'"
		}
		desc := generateBashDescription(cmd)
		return fmt.Sprintf(`{"command": "%s", "description": "%s"}`, escapeJSONString(cmd), escapeJSONString(desc))
	case "ls":
		return `{"path": "."}`
	case "webfetch":
		return `{"url": "https://example.com", "prompt": "Summarize the page content"}`
	case "websearch":
		return `{"query": "search term"}`
	case "task":
		return `{"prompt": "Task description", "description": "Task summary", "subagent_type": "general-purpose"}`
	// ============================================
	// NEW TOOLS - Version Control
	// ============================================
	case "git":
		operation := "status"
		description := "Check git status"
		contextLower := strings.ToLower(context)
		if strings.Contains(contextLower, "commit") {
			operation = "commit"
			description = "Create git commit"
		} else if strings.Contains(contextLower, "push") {
			operation = "push"
			description = "Push changes to remote"
		} else if strings.Contains(contextLower, "pull") {
			operation = "pull"
			description = "Pull changes from remote"
		} else if strings.Contains(contextLower, "branch") {
			operation = "branch"
			description = "Manage branches"
		} else if strings.Contains(contextLower, "checkout") {
			operation = "checkout"
			description = "Checkout branch or file"
		} else if strings.Contains(contextLower, "merge") {
			operation = "merge"
			description = "Merge branches"
		} else if strings.Contains(contextLower, "diff") {
			operation = "diff"
			description = "Show differences"
		} else if strings.Contains(contextLower, "log") {
			operation = "log"
			description = "Show commit history"
		} else if strings.Contains(contextLower, "stash") {
			operation = "stash"
			description = "Stash changes"
		}
		return fmt.Sprintf(`{"operation": "%s", "description": "%s"}`, operation, escapeJSONString(description))
	case "diff":
		mode := "working"
		contextLower := strings.ToLower(context)
		if strings.Contains(contextLower, "staged") {
			mode = "staged"
		} else if strings.Contains(contextLower, "branch") {
			mode = "branch"
		}
		return fmt.Sprintf(`{"mode": "%s", "description": "Show git diff"}`, mode)
	// ============================================
	// NEW TOOLS - Testing & Linting
	// ============================================
	case "test":
		testPath := "./..."
		coverage := false
		description := "Run tests"
		contextLower := strings.ToLower(context)
		if strings.Contains(contextLower, "coverage") {
			coverage = true
			description = "Run tests with coverage"
		}
		if strings.Contains(contextLower, "unit") {
			testPath = "./internal/..."
			description = "Run unit tests"
		} else if strings.Contains(contextLower, "integration") {
			testPath = "./tests/integration/..."
			description = "Run integration tests"
		}
		return fmt.Sprintf(`{"test_path": "%s", "coverage": %t, "verbose": true, "description": "%s"}`, testPath, coverage, escapeJSONString(description))
	case "lint":
		autoFix := false
		contextLower := strings.ToLower(context)
		if strings.Contains(contextLower, "fix") {
			autoFix = true
		}
		return fmt.Sprintf(`{"path": "./...", "linter": "auto", "auto_fix": %t, "description": "Run code linting"}`, autoFix)
	// ============================================
	// NEW TOOLS - File Intelligence
	// ============================================
	case "treeview", "tree":
		return `{"path": ".", "max_depth": 3, "show_hidden": false, "description": "Display directory tree"}`
	case "fileinfo":
		path := extractFilePathFromContext(context)
		if path == "" {
			path = "README.md"
		}
		return fmt.Sprintf(`{"file_path": "%s", "include_stats": true, "include_git": false, "description": "Get file information"}`, escapeJSONString(path))
	// ============================================
	// NEW TOOLS - Code Intelligence
	// ============================================
	case "symbols":
		path := extractFilePathFromContext(context)
		if path == "" {
			path = "."
		}
		return fmt.Sprintf(`{"file_path": "%s", "recursive": false, "description": "Extract code symbols"}`, escapeJSONString(path))
	case "references", "refs":
		symbol := extractSearchPatternFromContext(context)
		if symbol == "" {
			symbol = "main"
		}
		return fmt.Sprintf(`{"symbol": "%s", "include_declaration": true, "description": "Find symbol references"}`, escapeJSONString(symbol))
	case "definition", "goto":
		symbol := extractSearchPatternFromContext(context)
		if symbol == "" {
			symbol = "main"
		}
		return fmt.Sprintf(`{"symbol": "%s", "description": "Find symbol definition"}`, escapeJSONString(symbol))
	// ============================================
	// NEW TOOLS - Workflow
	// ============================================
	case "pr", "pullrequest":
		action := "list"
		description := "List pull requests"
		contextLower := strings.ToLower(context)
		if strings.Contains(contextLower, "create") {
			action = "create"
			description = "Create pull request"
		} else if strings.Contains(contextLower, "merge") {
			action = "merge"
			description = "Merge pull request"
		} else if strings.Contains(contextLower, "view") {
			action = "view"
			description = "View pull request"
		}
		return fmt.Sprintf(`{"action": "%s", "description": "%s"}`, action, escapeJSONString(description))
	case "issue":
		action := "list"
		description := "List issues"
		contextLower := strings.ToLower(context)
		if strings.Contains(contextLower, "create") {
			action = "create"
			description = "Create issue"
		} else if strings.Contains(contextLower, "close") {
			action = "close"
			description = "Close issue"
		} else if strings.Contains(contextLower, "view") {
			action = "view"
			description = "View issue"
		}
		return fmt.Sprintf(`{"action": "%s", "description": "%s"}`, action, escapeJSONString(description))
	case "workflow", "ci":
		action := "list"
		description := "List workflows"
		contextLower := strings.ToLower(context)
		if strings.Contains(contextLower, "run") {
			action = "run"
			description = "Run workflow"
		} else if strings.Contains(contextLower, "view") {
			action = "view"
			description = "View workflow run"
		} else if strings.Contains(contextLower, "cancel") {
			action = "cancel"
			description = "Cancel workflow run"
		}
		return fmt.Sprintf(`{"action": "%s", "description": "%s"}`, action, escapeJSONString(description))
	default:
		// For unknown tools, return empty object - the caller should handle appropriately
		return "{}"
	}
}

// Pre-compiled patterns for file path extraction.
var (
	filePathDoubleQuoteRe = regexp.MustCompile(`"([^"]+\.[a-zA-Z0-9]+)"`)
	filePathSingleQuoteRe = regexp.MustCompile(`'([^']+\.[a-zA-Z0-9]+)'`)
)

// extractFilePathFromContext extracts a file path from context text
func extractFilePathFromContext(context string) string {
	// Look for quoted paths first
	for _, re := range []*regexp.Regexp{filePathDoubleQuoteRe, filePathSingleQuoteRe} {
		matches := re.FindStringSubmatch(context)
		if len(matches) > 1 {
			return matches[1]
		}
	}

	// Look for common file extensions
	words := strings.Fields(context)
	for _, word := range words {
		word = strings.Trim(word, "\"'`,.:;!?()[]")
		if strings.Contains(word, ".") {
			ext := strings.ToLower(filepath.Ext(word))
			validExts := []string{".go", ".py", ".js", ".ts", ".md", ".txt", ".json", ".yaml", ".yml", ".sh", ".java", ".kt", ".rs"}
			for _, validExt := range validExts {
				if ext == validExt {
					return word
				}
			}
		}
	}

	return ""
}

// extractSearchPatternFromContext extracts a search pattern from context
func extractSearchPatternFromContext(context string) string {
	searchPatterns := []*regexp.Regexp{searchDoubleQuoteRe, searchSingleQuoteRe}
	for _, re := range searchPatterns {
		matches := re.FindStringSubmatch(context)
		if len(matches) > 1 && len(matches[1]) > 2 {
			return matches[1]
		}
	}

	// Look for pattern-like text after keywords
	keywords := []string{"for ", "pattern ", "term ", "string "}
	contextLower := strings.ToLower(context)
	for _, kw := range keywords {
		if idx := strings.Index(contextLower, kw); idx != -1 {
			remaining := context[idx+len(kw):]
			words := strings.Fields(remaining)
			if len(words) > 0 {
				return strings.Trim(words[0], "\"'`,.:;!?")
			}
		}
	}

	return ""
}

// extractCommandFromContext extracts a command from context text
func extractCommandFromContext(context string) string {
	contextLower := strings.ToLower(context)

	// Check for specific test commands
	if strings.Contains(contextLower, "run test") || strings.Contains(contextLower, "execute test") {
		if strings.Contains(contextLower, "go") {
			return "go test -v ./..."
		}
		if strings.Contains(contextLower, "npm") || strings.Contains(contextLower, "node") || strings.Contains(contextLower, "javascript") {
			return "npm test"
		}
		if strings.Contains(contextLower, "python") || strings.Contains(contextLower, "pytest") {
			return "pytest -v"
		}
		return "make test || go test -v ./... || npm test || pytest -v"
	}

	// Check for build commands
	if strings.Contains(contextLower, "build") || strings.Contains(contextLower, "compile") {
		if strings.Contains(contextLower, "go") {
			return "go build ./..."
		}
		if strings.Contains(contextLower, "npm") || strings.Contains(contextLower, "node") {
			return "npm run build"
		}
		return "make build || go build ./... || npm run build"
	}

	// Look for commands in backticks (reuse pre-compiled pattern)
	matches := mdInlineCodeRegex.FindStringSubmatch(context)
	if len(matches) > 1 {
		return matches[1]
	}

	return ""
}

// generateBashDescription generates a human-readable description for a bash command
// This is REQUIRED by the Bash tool schema
// IMPORTANT: Order matters! More specific patterns MUST be checked BEFORE general patterns
func generateBashDescription(cmd string) string {
	cmdLower := strings.ToLower(cmd)

	// Coverage commands - MUST check before "test" since coverage commands often contain "test"
	if strings.Contains(cmdLower, "coverprofile") || strings.Contains(cmdLower, "coverage") {
		return "Generate test coverage report"
	}

	// Git commands - MUST check before "test" since git commands may contain "test" in commit messages
	if strings.Contains(cmdLower, "git ") || strings.HasPrefix(cmdLower, "git") {
		if strings.Contains(cmdLower, "status") {
			return "Check git status"
		}
		if strings.Contains(cmdLower, "commit") {
			return "Create git commit"
		}
		if strings.Contains(cmdLower, "push") {
			return "Push changes to remote"
		}
		if strings.Contains(cmdLower, "pull") {
			return "Pull changes from remote"
		}
		if strings.Contains(cmdLower, "clone") {
			return "Clone git repository"
		}
		if strings.Contains(cmdLower, "checkout") {
			return "Switch git branch"
		}
		if strings.Contains(cmdLower, "branch") {
			return "Manage git branches"
		}
		if strings.Contains(cmdLower, "merge") {
			return "Merge git branches"
		}
		if strings.Contains(cmdLower, "rebase") {
			return "Rebase git commits"
		}
		if strings.Contains(cmdLower, "diff") {
			return "Show git diff"
		}
		if strings.Contains(cmdLower, "log") {
			return "Show git log"
		}
		if strings.Contains(cmdLower, "add") {
			return "Stage files for commit"
		}
		return "Execute git command"
	}

	// Lint commands - check before build since some lint commands contain "build"
	if strings.Contains(cmdLower, "lint") || strings.Contains(cmdLower, "golangci") {
		return "Run linter"
	}

	// Build commands
	if strings.Contains(cmdLower, "build") || strings.Contains(cmdLower, "compile") {
		if strings.Contains(cmdLower, "go build") {
			return "Build Go project"
		}
		if strings.Contains(cmdLower, "npm") {
			return "Build npm project"
		}
		if strings.Contains(cmdLower, "make") {
			return "Build project using make"
		}
		if strings.Contains(cmdLower, "docker") {
			return "Build Docker image"
		}
		return "Build project"
	}

	// Test commands - after coverage and git since those may contain "test"
	if strings.Contains(cmdLower, "test") {
		if strings.Contains(cmdLower, "go test") {
			return "Run Go tests"
		}
		if strings.Contains(cmdLower, "npm test") {
			return "Run npm tests"
		}
		if strings.Contains(cmdLower, "pytest") {
			return "Run Python tests"
		}
		if strings.Contains(cmdLower, "jest") {
			return "Run Jest tests"
		}
		if strings.Contains(cmdLower, "mocha") {
			return "Run Mocha tests"
		}
		return "Run tests"
	}

	// Docker commands
	if strings.Contains(cmdLower, "docker") {
		if strings.Contains(cmdLower, "compose") {
			return "Execute docker-compose command"
		}
		if strings.Contains(cmdLower, "run") {
			return "Run Docker container"
		}
		if strings.Contains(cmdLower, "stop") {
			return "Stop Docker container"
		}
		if strings.Contains(cmdLower, "ps") {
			return "List Docker containers"
		}
		return "Execute Docker command"
	}

	// Package manager commands
	if strings.Contains(cmdLower, "npm") || strings.Contains(cmdLower, "yarn") || strings.Contains(cmdLower, "pnpm") {
		if strings.Contains(cmdLower, "install") {
			return "Install npm dependencies"
		}
		if strings.Contains(cmdLower, "run") {
			return "Run npm script"
		}
		return "Execute npm command"
	}

	if strings.Contains(cmdLower, "pip") {
		if strings.Contains(cmdLower, "install") {
			return "Install Python packages"
		}
		return "Execute pip command"
	}

	if strings.Contains(cmdLower, "go mod") || strings.Contains(cmdLower, "go get") {
		return "Manage Go modules"
	}

	// Make commands
	if strings.HasPrefix(cmdLower, "make") {
		return "Execute make target"
	}

	// Echo commands
	if strings.HasPrefix(cmdLower, "echo") {
		return "Print message"
	}

	// List commands
	if strings.HasPrefix(cmdLower, "ls") {
		return "List directory contents"
	}

	// Directory commands
	if strings.HasPrefix(cmdLower, "cd") {
		return "Change directory"
	}

	if strings.HasPrefix(cmdLower, "mkdir") {
		return "Create directory"
	}

	if strings.HasPrefix(cmdLower, "rm") {
		return "Remove files or directories"
	}

	if strings.HasPrefix(cmdLower, "cp") {
		return "Copy files"
	}

	if strings.HasPrefix(cmdLower, "mv") {
		return "Move or rename files"
	}

	// Curl/wget commands
	if strings.HasPrefix(cmdLower, "curl") || strings.HasPrefix(cmdLower, "wget") {
		return "Make HTTP request"
	}

	// Default: use first part of command as description
	parts := strings.Fields(cmd)
	if len(parts) > 0 {
		return fmt.Sprintf("Execute %s command", parts[0])
	}

	return "Execute shell command"
}

// extractContentForFile generates appropriate content for a file based on its name and context
func extractContentForFile(context string, fileName string) string {
	fileNameLower := strings.ToLower(fileName)

	if strings.Contains(fileNameLower, "agents.md") {
		return generateAgentsContent(context)
	}
	if strings.Contains(fileNameLower, "readme.md") {
		return generateReadmeContent(context)
	}
	if strings.Contains(fileNameLower, "testing_plan.md") || strings.Contains(fileNameLower, "test_plan.md") {
		return generateTestingPlanContent(context)
	}
	if strings.Contains(fileNameLower, "changelog.md") {
		return generateChangelogContent(context)
	}

	// Default: extract any code blocks or structured content from context
	if idx := strings.Index(context, "```"); idx != -1 {
		afterStart := context[idx+3:]
		if nlIdx := strings.Index(afterStart, "\n"); nlIdx != -1 {
			afterStart = afterStart[nlIdx+1:]
		}
		if endIdx := strings.Index(afterStart, "```"); endIdx != -1 {
			return strings.TrimSpace(afterStart[:endIdx])
		}
	}

	return fmt.Sprintf("# %s\n\nGenerated content based on analysis.\n\n%s",
		strings.TrimSuffix(fileName, filepath.Ext(fileName)),
		cleanSynthesisForFile(context))
}

// generateAgentsContent generates AGENTS.md content
func generateAgentsContent(context string) string {
	return `# AGENTS.md

This file provides guidance to AI coding agents working with this codebase.

## Project Overview

This project contains code that AI agents should understand before making modifications.

## Key Guidelines

- Follow existing code patterns and conventions
- Maintain consistent formatting and style
- Write clear, concise code with appropriate comments
- Update tests when modifying functionality
- Run tests before committing changes

## Important Files

See the project structure for key entry points and configuration files.

## Testing Requirements

- All new code should have corresponding tests
- Run the test suite before submitting changes
- Ensure code coverage remains high

## Code Style

- Follow the existing code style in the project
- Use meaningful variable and function names
- Keep functions focused and modular
`
}

// generateReadmeContent generates README.md content
func generateReadmeContent(context string) string {
	return `# Project README

## Overview

This project...

## Installation

` + "```bash\n# Installation steps\n```" + `

## Usage

` + "```bash\n# Usage examples\n```" + `

## Contributing

See CONTRIBUTING.md for guidelines.

## License

See LICENSE file.
`
}

// generateTestingPlanContent generates testing plan content
func generateTestingPlanContent(context string) string {
	return `# Testing Plan

## Overview

This document outlines the testing strategy for the project.

## Test Categories

### Unit Tests
- Test individual functions and methods
- Mock external dependencies
- Aim for high code coverage

### Integration Tests
- Test component interactions
- Verify API endpoints
- Database operations

### End-to-End Tests
- Full workflow testing
- User scenario validation

## Running Tests

` + "```bash\n# Run all tests\nmake test\n\n# Run with coverage\nmake test-coverage\n```" + `

## Coverage Goals

- Minimum 80% code coverage
- 100% coverage on critical paths

## Test Schedule

- Unit tests: On every commit
- Integration tests: On pull requests
- E2E tests: Before releases
`
}

// generateChangelogContent generates changelog content
func generateChangelogContent(context string) string {
	return `# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added
- New features

### Changed
- Updates to existing features

### Fixed
- Bug fixes

### Removed
- Removed features

## [1.0.0] - YYYY-MM-DD

### Added
- Initial release
`
}

// processToolResultsWithLLM processes tool results by making a direct LLM call
// CRITICAL: This enables proper handling of tool results from CLI agents like OpenCode
// Instead of just acknowledging, it actually generates useful insights from the tool output
//
// Returns the synthesised content AND the provider's own *models.LLMResponse.
// The response is returned (rather than discarded, as it was before
// 2026-09-03) so callers can report the provider's REAL token usage via
// LLMResponse.TokenSplit. Previously the caller estimated usage from the
// length of the reply string — `PromptTokens: len(reply)/4` charged the
// PROMPT by the RESPONSE size, which is not an approximation of anything.
// Callers that emit no usage object may discard it with `_`.
func (h *UnifiedHandler) processToolResultsWithLLM(ctx context.Context, req *OpenAIChatRequest) (string, *models.LLMResponse, error) {
	// Build system prompt for tool result processing
	systemPrompt := `You are HelixAgent, an expert AI coding assistant with FULL ACCESS to the user's codebase through tools.

CRITICAL INSTRUCTIONS:
1. You have just received TOOL RESULTS from executing tools on the user's codebase
2. You MUST analyze these results and provide SPECIFIC, ACTIONABLE insights
3. If the user asked you to CREATE or MODIFY files, you MUST use the appropriate tools (Write, Edit) to do so
4. If you see file listings (from Glob), describe the project structure and key files
5. If you see file contents (from Read), analyze the code and provide insights
6. If you see search results (from Grep), explain what was found and where
7. NEVER say "I cannot see your codebase" - you CAN and HAVE seen it via tools
8. Be SPECIFIC - reference actual file names, line numbers, and code patterns from the results
9. If the task requires WRITING code or files, DO IT - don't just describe what you would do

When asked to create files like AGENTS.md:
- Actually CREATE the file using the Write tool
- Include all requested information based on what you found in the codebase
- Follow the user's specifications exactly`

	// Convert messages to internal format for LLM call
	var messages []models.Message
	messages = append(messages, models.Message{
		Role:    "system",
		Content: systemPrompt,
	})

	// Add all conversation messages including tool results
	// IMPORTANT: Convert tool messages to user messages to avoid provider-specific format issues
	for _, msg := range req.Messages {
		// Skip system messages (we already added our own)
		if msg.Role == "system" {
			continue
		}

		message := models.Message{
			Role:    msg.Role,
			Content: msg.Content,
		}

		// Convert tool result messages to user messages with context
		// This avoids provider-specific tool message format requirements
		if msg.Role == "tool" || msg.ToolCallID != "" {
			message.Role = "user"
			message.Content = fmt.Sprintf("TOOL EXECUTION RESULT:\n```\n%s\n```\n\nPlease analyze this tool output and provide specific insights based on the data.", msg.Content)
		}

		// Convert assistant messages with tool_calls to plain assistant messages
		// Include the tool call info in the content
		if len(msg.ToolCalls) > 0 {
			var toolCallsStr strings.Builder
			if msg.Content != "" {
				toolCallsStr.WriteString(msg.Content)
				toolCallsStr.WriteString("\n\n")
			}
			toolCallsStr.WriteString("I executed the following tools:\n")
			for _, tc := range msg.ToolCalls {
				toolCallsStr.WriteString(fmt.Sprintf("- %s with arguments: %s\n", tc.Function.Name, tc.Function.Arguments))
			}
			message.Content = toolCallsStr.String()
		}

		messages = append(messages, message)
	}

	// Create internal request with ModelParams
	internalReq := &models.LLMRequest{
		Messages: messages,
		ModelParams: models.ModelParameters{
			MaxTokens:   4096,
			Temperature: 0.7,
		},
	}

	// Add tools if available so LLM can make additional tool calls if needed
	if len(req.Tools) > 0 {
		internalReq.ModelParams.ProviderSpecific = map[string]interface{}{
			"tools": req.Tools,
		}
	}

	// Get providers dynamically ordered by LLMsVerifier scores (highest first)
	// CRITICAL: NO hardcoded fallback chains - all ordering is based on real verification results
	providers := h.providerRegistry.ListProvidersOrderedByScore()
	if len(providers) == 0 {
		return "", nil, fmt.Errorf("no providers available - check your configuration")
	}

	logrus.WithField("providers_ordered", providers).Debug("Using dynamically ordered providers for tool result processing")

	var lastErr error
	var successfulProvider string

	for _, providerName := range providers {
		// Check if parent context is already cancelled
		select {
		case <-ctx.Done():
			logrus.Warn("Parent context cancelled during tool result processing")
			// Return what we have or a helpful message
			if lastErr != nil {
				return "", nil, fmt.Errorf("context cancelled while processing tool results: %w", lastErr)
			}
			return "", nil, fmt.Errorf("context cancelled before tool results could be processed")
		default:
		}

		provider, err := h.providerRegistry.GetProvider(providerName)
		if err != nil || provider == nil {
			logrus.WithField("provider", providerName).Debug("Provider not available for tool results")
			continue
		}

		// Create a per-provider timeout context (60 seconds per provider)
		// This prevents one slow provider from exhausting the entire timeout
		providerCtx, providerCancel := context.WithTimeout(ctx, 60*time.Second)

		logrus.WithField("provider", providerName).Debug("Attempting tool result processing")

		resp, err := provider.Complete(providerCtx, internalReq)
		providerCancel() // Always cancel to release resources

		if err != nil {
			logrus.WithError(err).WithField("provider", providerName).Debug("Tool result LLM call failed, trying next")
			lastErr = err
			continue
		}

		if resp == nil || resp.Content == "" {
			logrus.WithField("provider", providerName).Debug("Empty response from provider, trying next")
			lastErr = fmt.Errorf("empty response from %s", providerName)
			continue
		}

		successfulProvider = providerName //nolint:ineffassign
		logrus.WithFields(logrus.Fields{
			"provider":    providerName,
			"content_len": len(resp.Content),
		}).Info("Tool result processed successfully")

		// CRITICAL: Check for embedded function calls in the response
		// Some LLMs output <function=write>...</function> style text instead of proper tool_calls
		// We need to parse and execute these embedded calls
		content, executedTools := h.processEmbeddedFunctionCalls(ctx, resp.Content)
		if len(executedTools) > 0 {
			logrus.WithFields(logrus.Fields{
				"tools_executed": len(executedTools),
				"provider":       providerName,
			}).Info("Executed embedded function calls from LLM response")
		}

		return content, resp, nil
	}

	// If all providers failed, return a helpful error message
	logrus.WithError(lastErr).WithFields(logrus.Fields{
		"successful_provider": successfulProvider,
		"providers_tried":     len(providers),
	}).Error("All providers failed to process tool results")
	if lastErr != nil {
		return "", nil, fmt.Errorf("all %d dynamically-ordered providers failed to process tool results (last error: %w). Check provider API keys and LLMsVerifier scores", len(providers), lastErr)
	}
	return "", nil, fmt.Errorf("no providers available to process tool results - check your configuration and LLMsVerifier")
}

// EmbeddedFunctionCall represents a function call parsed from LLM response text
type EmbeddedFunctionCall struct {
	Name       string
	Parameters map[string]string
	RawContent string
}

// processEmbeddedFunctionCalls detects and executes embedded function calls in LLM responses
// This handles cases where LLMs output <function=write>...</function> style text instead of proper tool_calls
func (h *UnifiedHandler) processEmbeddedFunctionCalls(ctx context.Context, content string) (string, []string) {
	var executedTools []string

	// Parse embedded function calls from the content
	calls := parseEmbeddedFunctionCalls(content)
	if len(calls) == 0 {
		return content, executedTools
	}

	// Execute each embedded function call
	cleanedContent := content
	for _, call := range calls {
		result, err := h.executeEmbeddedFunctionCall(ctx, call)
		if err != nil {
			logrus.WithError(err).WithField("function", call.Name).Warn("Failed to execute embedded function call")
			// Replace the function call with an error message
			errorMsg := fmt.Sprintf("\n\n⚠️ Failed to execute %s: %v\n\n", call.Name, err)
			cleanedContent = strings.Replace(cleanedContent, call.RawContent, errorMsg, 1)
		} else {
			// Replace the function call with a success message
			successMsg := fmt.Sprintf("\n\n✅ Successfully executed %s: %s\n\n", call.Name, result)
			cleanedContent = strings.Replace(cleanedContent, call.RawContent, successMsg, 1)
			executedTools = append(executedTools, call.Name)
		}
	}

	// Strip any remaining unparsed tool tags that weren't matched by our patterns
	cleanedContent = stripUnparsedToolTags(cleanedContent)

	return cleanedContent, executedTools
}

// stripUnparsedToolTags removes any remaining <bash>, <read>, etc. tags that weren't parsed
// This prevents raw tool XML from appearing in the dialogue output
// Also handles scripting language tags: ruby, python, php, perl, etc.
func stripUnparsedToolTags(content string) string {
	// Tool and command tags to strip
	toolTags := []string{
		// Core tool tags
		"bash", "shell", "read", "write", "edit", "glob", "grep",
		"find", "cat", "ls", "cd", "mkdir", "rm", "mv", "cp",
		"function", "function_call", "command", "execute", "run", "code",
		// Scripting language tags
		"python", "ruby", "php", "perl", "node", "nodejs", "javascript", "js",
		"typescript", "ts", "go", "golang", "rust", "java", "kotlin", "scala",
		"swift", "csharp", "cs", "cpp", "c", "sql", "powershell", "ps1",
		"lua", "r", "julia", "haskell", "elixir", "clojure", "lisp",
		// Script execution tags
		"script", "exec", "terminal", "console", "sh", "zsh", "fish", "cmd",
	}

	result := content
	for _, tag := range toolTags {
		// Match opening and closing tags with any content (case-insensitive)
		// Pattern matches: <tag>...</tag> or <TAG>...</TAG> or <Tag>...</Tag>
		pattern := regexp.MustCompile(`(?si)<` + tag + `[^>]*>(.*?)</` + tag + `>`)
		// Replace with just the inner content (without the tags)
		result = pattern.ReplaceAllString(result, "$1")

		// Also strip standalone tags like <bash> without closing
		standalonePattern := regexp.MustCompile(`(?i)</?` + tag + `[^>]*>`)
		result = standalonePattern.ReplaceAllString(result, "")
	}

	// Convert XML-style code blocks to proper markdown if they contain code
	// Pattern: code content that looks like it should be in a code block
	result = convertXMLCodeToMarkdown(result)

	// Clean up excessive whitespace and newlines left by tag removal
	result = sanitizeExcessNewlinesRe.ReplaceAllString(result, "\n\n")

	return result
}

// convertXMLCodeToMarkdown converts any remaining XML-like code blocks to proper markdown
func convertXMLCodeToMarkdown(content string) string {
	result := content

	// Languages that should be formatted as code blocks
	languages := []string{
		"python", "ruby", "php", "perl", "javascript", "typescript", "go",
		"rust", "java", "kotlin", "scala", "swift", "csharp", "cpp", "c",
		"sql", "bash", "shell", "powershell", "lua", "r", "julia", "haskell",
	}

	for _, lang := range languages {
		// Match ```lang ... ``` that might be malformed
		// Also match code that was in XML tags and now needs formatting
		malformedPattern := regexp.MustCompile(`(?si)` + "`{0,3}" + lang + `\s*\n(.*?)\n` + "`{0,3}")
		result = malformedPattern.ReplaceAllStringFunc(result, func(match string) string {
			// Extract the code content
			inner := regexp.MustCompile(`(?si)` + "`{0,3}" + lang + `\s*\n(.*?)\n` + "`{0,3}").FindStringSubmatch(match)
			if len(inner) >= 2 {
				code := strings.TrimSpace(inner[1])
				if code != "" {
					return fmt.Sprintf("\n```%s\n%s\n```\n", lang, code)
				}
			}
			return match
		})
	}

	return result
}

// parseEmbeddedFunctionCalls parses function calls from LLM response text
// Supports multiple formats:
// - <function=write><parameter=path>...</parameter><parameter=content>...</parameter></function>
// - <function_call name="write"><path>...</path><content>...</content></function_call>
// - ```function:write ... ```
func parseEmbeddedFunctionCalls(content string) []EmbeddedFunctionCall {
	var calls []EmbeddedFunctionCall

	funcMatches := embeddedFuncRe.FindAllStringSubmatch(content, -1)
	for _, match := range funcMatches {
		if len(match) >= 3 {
			call := EmbeddedFunctionCall{
				Name:       match[1],
				Parameters: make(map[string]string),
				RawContent: match[0],
			}
			paramMatches := embeddedParamRe.FindAllStringSubmatch(match[2], -1)
			for _, pm := range paramMatches {
				if len(pm) >= 3 {
					call.Parameters[pm[1]] = strings.TrimSpace(pm[2])
				}
			}
			calls = append(calls, call)
		}
	}

	fcMatches := embeddedFCRe.FindAllStringSubmatch(content, -1)
	for _, match := range fcMatches {
		if len(match) >= 3 {
			call := EmbeddedFunctionCall{
				Name:       match[1],
				Parameters: make(map[string]string),
				RawContent: match[0],
			}
			// Parse inner elements as parameters using predefined tag names
			// Go's regexp doesn't support backreferences
			innerContent := match[2]
			paramTags := []string{"path", "file_path", "filepath", "content", "data", "text", "pattern", "old_string", "new_string", "command"}
			for _, paramTag := range paramTags {
				paramPattern := regexp.MustCompile(`(?s)<` + paramTag + `>(.*?)</` + paramTag + `>`)
				paramMatches := paramPattern.FindStringSubmatch(innerContent)
				if len(paramMatches) >= 2 {
					call.Parameters[paramTag] = strings.TrimSpace(paramMatches[1])
				}
			}
			calls = append(calls, call)
		}
	}

	// Pattern 3: Simple XML format with Write/Edit/Read tags
	// <Write><file_path>...</file_path><content>...</content></Write>
	// Also handles lowercase variants: <bash>, <read>, etc.
	// Go's regexp doesn't support backreferences, so we check each tag separately
	// Note: Only use one case variant per tag since regex is case-insensitive
	simpleTags := []string{"Write", "Edit", "Read", "Glob", "Grep", "Bash", "shell"}
	for _, tag := range simpleTags {
		// Use case-insensitive matching with (?i) flag - handles both <Write> and <write>
		tagPattern := regexp.MustCompile(`(?si)<` + tag + `>(.*?)</` + tag + `>`)
		tagMatches := tagPattern.FindAllStringSubmatch(content, -1)
		for _, match := range tagMatches {
			if len(match) >= 2 {
				call := EmbeddedFunctionCall{
					Name:       strings.ToLower(tag),
					Parameters: make(map[string]string),
					RawContent: match[0],
				}
				// Parse inner elements as parameters using a simple approach
				// Match <tagname>content</tagname> patterns
				innerContent := match[1]
				paramTags := []string{"file_path", "filepath", "path", "content", "data", "text", "pattern", "old_string", "new_string", "command"}
				for _, paramTag := range paramTags {
					paramPattern := regexp.MustCompile(`(?s)<` + paramTag + `>(.*?)</` + paramTag + `>`)
					paramMatches := paramPattern.FindStringSubmatch(innerContent)
					if len(paramMatches) >= 2 {
						call.Parameters[paramTag] = strings.TrimSpace(paramMatches[1])
					}
				}
				calls = append(calls, call)
			}
		}
	}

	return calls
}

// executeEmbeddedFunctionCall executes a parsed function call
func (h *UnifiedHandler) executeEmbeddedFunctionCall(ctx context.Context, call EmbeddedFunctionCall) (string, error) {
	funcName := strings.ToLower(call.Name)

	switch funcName {
	case "write":
		return h.executeWriteFunction(ctx, call)
	case "edit":
		return h.executeEditFunction(ctx, call)
	case "read":
		return h.executeReadFunction(ctx, call)
	case "glob":
		return h.executeGlobFunction(ctx, call)
	case "grep":
		return h.executeGrepFunction(ctx, call)
	case "bash", "shell":
		return h.executeBashFunction(ctx, call)
	default:
		return "", fmt.Errorf("unsupported function: %s", call.Name)
	}
}

// executeWriteFunction writes a file to disk
func (h *UnifiedHandler) executeWriteFunction(ctx context.Context, call EmbeddedFunctionCall) (string, error) {
	// Get file path - try various parameter names
	filePath := getParam(call.Parameters, "path", "file_path", "filepath", "file")
	if filePath == "" {
		return "", fmt.Errorf("missing file path parameter")
	}

	// Validate path for traversal attacks (G304 security fix)
	if !utils.ValidatePath(filePath) {
		return "", fmt.Errorf("invalid file path: contains path traversal or dangerous characters")
	}

	// Get content
	content := getParam(call.Parameters, "content", "data", "text", "body")
	if content == "" {
		return "", fmt.Errorf("missing content parameter")
	}

	// Resolve relative paths
	if !filepath.IsAbs(filePath) {
		// Get current working directory or use a default
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		filePath = filepath.Join(cwd, filePath)
	}

	// Validate the resolved path again (G304 security fix - belt and suspenders)
	if !utils.ValidatePath(filePath) {
		return "", fmt.Errorf("invalid resolved file path: contains path traversal or dangerous characters")
	}

	// Create parent directories if needed
	dir := filepath.Dir(filePath)
	// #nosec G301 -- user workspace directories use standard 0755 permissions for usability
	if err := os.MkdirAll(dir, 0750); err != nil {
		return "", fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	// Write the file
	// #nosec G304 - filePath is validated by utils.ValidatePath for path traversal and dangerous characters
	// #nosec G306 - user workspace files use standard 0644 permissions for usability
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to write file %s: %w", filePath, err)
	}

	logrus.WithField("file", filePath).Info("Successfully wrote file from embedded function call")
	return fmt.Sprintf("File written: %s (%d bytes)", filePath, len(content)), nil
}

// executeEditFunction edits an existing file
func (h *UnifiedHandler) executeEditFunction(ctx context.Context, call EmbeddedFunctionCall) (string, error) {
	filePath := getParam(call.Parameters, "path", "file_path", "filepath", "file")
	if filePath == "" {
		return "", fmt.Errorf("missing file path parameter")
	}

	// Validate path for traversal attacks (G304 security fix)
	if !utils.ValidatePath(filePath) {
		return "", fmt.Errorf("invalid file path: contains path traversal or dangerous characters")
	}

	oldString := getParam(call.Parameters, "old_string", "old", "search", "find")
	newString := getParam(call.Parameters, "new_string", "new", "replace", "replacement")

	if oldString == "" {
		return "", fmt.Errorf("missing old_string parameter")
	}

	// Resolve path
	if !filepath.IsAbs(filePath) {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		filePath = filepath.Join(cwd, filePath)
	}

	// Validate the resolved path again (G304 security fix - belt and suspenders)
	if !utils.ValidatePath(filePath) {
		return "", fmt.Errorf("invalid resolved file path: contains path traversal or dangerous characters")
	}

	// Read existing content
	// #nosec G304 - filePath is validated by utils.ValidatePath for path traversal and dangerous characters
	existingContent, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read file %s: %w", filePath, err)
	}

	// Replace content
	newContent := strings.Replace(string(existingContent), oldString, newString, -1)

	// Write back
	// #nosec G304 - filePath is validated by utils.ValidatePath for path traversal and dangerous characters
	// #nosec G306 - user workspace files use standard 0644 permissions for usability
	if err := os.WriteFile(filePath, []byte(newContent), 0644); err != nil {
		return "", fmt.Errorf("failed to write file %s: %w", filePath, err)
	}

	return fmt.Sprintf("File edited: %s", filePath), nil
}

// executeReadFunction reads a file (returns content for display, not execution)
func (h *UnifiedHandler) executeReadFunction(ctx context.Context, call EmbeddedFunctionCall) (string, error) {
	filePath := getParam(call.Parameters, "path", "file_path", "filepath", "file")
	if filePath == "" {
		return "", fmt.Errorf("missing file path parameter")
	}

	// Validate path for traversal attacks (G304 security fix)
	if !utils.ValidatePath(filePath) {
		return "", fmt.Errorf("invalid file path: contains path traversal or dangerous characters")
	}

	if !filepath.IsAbs(filePath) {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		filePath = filepath.Join(cwd, filePath)
	}

	// Validate the resolved path again (G304 security fix - belt and suspenders)
	if !utils.ValidatePath(filePath) {
		return "", fmt.Errorf("invalid resolved file path: contains path traversal or dangerous characters")
	}

	// #nosec G304 - filePath is validated by utils.ValidatePath for path traversal and dangerous characters
	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read file %s: %w", filePath, err)
	}

	return fmt.Sprintf("Read %d bytes from %s", len(content), filePath), nil
}

// executeGlobFunction finds files matching a pattern
func (h *UnifiedHandler) executeGlobFunction(ctx context.Context, call EmbeddedFunctionCall) (string, error) {
	pattern := getParam(call.Parameters, "pattern", "glob", "path")
	if pattern == "" {
		return "", fmt.Errorf("missing pattern parameter")
	}

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("glob failed: %w", err)
	}

	return fmt.Sprintf("Found %d files matching %s", len(matches), pattern), nil
}

// executeGrepFunction searches for patterns in files
func (h *UnifiedHandler) executeGrepFunction(ctx context.Context, call EmbeddedFunctionCall) (string, error) {
	pattern := getParam(call.Parameters, "pattern", "search", "query", "regex")
	if pattern == "" {
		return "", fmt.Errorf("missing pattern parameter")
	}

	// Get optional path parameter
	searchPath := getParam(call.Parameters, "path", "directory", "dir", "folder")
	if searchPath == "" {
		searchPath = "." // Default to current directory
	}

	// Validate search path for traversal attacks (G304 security fix)
	if !utils.ValidatePath(searchPath) {
		return "", fmt.Errorf("invalid search path: contains path traversal or dangerous characters")
	}

	// Compile regex pattern
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("invalid regex pattern: %w", err)
	}

	var results []string
	maxResults := 100 // Limit results to prevent overwhelming responses

	// Walk the directory and search files
	err = filepath.WalkDir(searchPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // Skip files we can't access
		}

		// Skip directories and non-regular files
		if d.IsDir() {
			// Skip common non-source directories
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || name == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}

		// Only search text-like files
		ext := strings.ToLower(filepath.Ext(path))
		textExtensions := map[string]bool{
			".go": true, ".py": true, ".js": true, ".ts": true, ".jsx": true, ".tsx": true,
			".java": true, ".c": true, ".cpp": true, ".h": true, ".hpp": true,
			".rs": true, ".rb": true, ".php": true, ".swift": true, ".kt": true,
			".md": true, ".txt": true, ".json": true, ".yaml": true, ".yml": true,
			".xml": true, ".html": true, ".css": true, ".sql": true, ".sh": true,
		}
		if !textExtensions[ext] {
			return nil
		}

		// Read file content
		// Note: path comes from filepath.WalkDir which walks from a user-provided searchPath
		// The searchPath is already validated at function entry, and WalkDir only returns
		// paths within the search tree, so this is safe from path traversal
		// #nosec G304 - path is constrained to searchPath tree via filepath.WalkDir
		content, err := os.ReadFile(path) // #nosec G122 -- path constrained by filepath.WalkDir root; sandboxing enforced at the walker construction
		if err != nil {
			return nil // Skip files we can't read
		}

		// Search for pattern
		lines := strings.Split(string(content), "\n")
		for lineNum, line := range lines {
			if len(results) >= maxResults {
				break
			}
			if re.MatchString(line) {
				// Format: file:line:content
				result := fmt.Sprintf("%s:%d: %s", path, lineNum+1, strings.TrimSpace(line))
				if len(result) > 200 {
					result = result[:200] + "..."
				}
				results = append(results, result)
			}
		}

		return nil
	})

	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}

	if len(results) == 0 {
		return fmt.Sprintf("No matches found for pattern: %s", pattern), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d matches for pattern '%s':\n\n", len(results), pattern))
	for _, r := range results {
		sb.WriteString(r)
		sb.WriteString("\n")
	}

	if len(results) >= maxResults {
		sb.WriteString(fmt.Sprintf("\n... (showing first %d results)", maxResults))
	}

	return sb.String(), nil
}

// executeBashFunction executes a shell command (with safety restrictions)
func (h *UnifiedHandler) executeBashFunction(ctx context.Context, call EmbeddedFunctionCall) (string, error) {
	// For security, we don't execute arbitrary bash commands from embedded function calls
	command := getParam(call.Parameters, "command", "cmd", "script")
	return "", fmt.Errorf("bash execution from embedded function calls is disabled for security - command was: %s", command)
}

// getParam gets a parameter value by trying multiple possible keys
func getParam(params map[string]string, keys ...string) string {
	for _, key := range keys {
		if val, ok := params[key]; ok && val != "" {
			return val
		}
		// Also try lowercase version
		if val, ok := params[strings.ToLower(key)]; ok && val != "" {
			return val
		}
	}
	return ""
}

// generateFallbackToolResultsResponse generates a fallback response when LLM processing fails
// This extracts useful information from the tool results and generates a helpful message
func (h *UnifiedHandler) generateFallbackToolResultsResponse(req *OpenAIChatRequest) string {
	var response strings.Builder
	response.WriteString("I received the tool results. Here's what I found:\n\n")

	// Process each message looking for tool results
	for _, msg := range req.Messages {
		if msg.Role == "tool" || msg.ToolCallID != "" {
			content := msg.Content
			if content == "" {
				continue
			}

			// Analyze the content to provide useful information
			lines := strings.Split(content, "\n")
			if len(lines) > 0 {
				// Check if it looks like a file listing (from Glob)
				if strings.Contains(content, ".go") || strings.Contains(content, ".ts") || strings.Contains(content, ".py") {
					response.WriteString("**File Listing:**\n")
					fileCount := 0
					for _, line := range lines {
						line = strings.TrimSpace(line)
						if line != "" && (strings.Contains(line, ".") || strings.HasPrefix(line, "/")) {
							fileCount++
							if fileCount <= 20 { // Limit to first 20 files
								response.WriteString(fmt.Sprintf("- %s\n", line))
							}
						}
					}
					if fileCount > 20 {
						response.WriteString(fmt.Sprintf("- ... and %d more files\n", fileCount-20))
					}
					response.WriteString("\n")
				} else if len(content) < 2000 {
					// For smaller content, include it directly
					response.WriteString("**Tool Output:**\n```\n")
					response.WriteString(content)
					response.WriteString("\n```\n\n")
				} else {
					// For large content, summarize
					response.WriteString(fmt.Sprintf("**Tool Output:** (Received %d bytes of data)\n\n", len(content)))
				}
			}
		}

		// Look for the original user request to understand what was asked
		if msg.Role == "user" && !strings.Contains(msg.Content, "TOOL EXECUTION RESULT") {
			// This is likely the original user question
			userRequest := strings.ToLower(msg.Content)

			// Check if user asked to create a file
			if strings.Contains(userRequest, "create") && strings.Contains(userRequest, "agents.md") {
				response.WriteString("\n**Note:** To create the AGENTS.md file, I would need the LLM providers to be available to generate the content. ")
				response.WriteString("Please check your API configurations and try again.\n")
			}
		}
	}

	if response.Len() == 0 {
		return "" // No useful fallback could be generated
	}

	return response.String()
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// usageFromResponse builds an OpenAI `usage` envelope from a provider
// response's REAL reported token counts.
//
// Extracted as a named function so the tool-result turn's envelope is
// directly unit-testable (see TestUsageFromResponse). It previously
// estimated the whole envelope from the reply's STRING LENGTH:
//
//	PromptTokens:     len(toolResultResponse) / 4
//	CompletionTokens: len(toolResultResponse) / 4
//	TotalTokens:      len(toolResultResponse) / 2
//
// which charged the PROMPT by the RESPONSE size — a figure describing
// neither direction — and made prompt always equal completion.
//
// models.LLMResponse.TokenSplit reports what the provider measured, or
// honest zeros when it reported nothing (§11.4.6). Its nil-receiver
// branch means a nil response degrades to an all-zero envelope
// ("not reported"), never to an invented one, and never a panic.
func usageFromResponse(resp *models.LLMResponse) *OpenAIUsage {
	promptTokens, completionTokens, totalTokens := resp.TokenSplit()
	return &OpenAIUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
	}
}
