package models

import (
	"encoding/json"
	"math"
	"time"
)

// User represents a user in the system
type User struct {
	ID           string    `json:"id" db:"id"`
	Username     string    `json:"username" db:"username"`
	Email        string    `json:"email" db:"email"`
	PasswordHash string    `json:"-" db:"password_hash"`
	APIKey       string    `json:"api_key" db:"api_key"`
	Role         string    `json:"role" db:"role"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" db:"updated_at"`
}

type LLMProvider struct {
	ID           string                 `json:"id" db:"id"`
	Name         string                 `json:"name" db:"name"`
	Type         string                 `json:"type" db:"type"`
	APIKey       string                 `json:"-" db:"api_key"`
	BaseURL      string                 `json:"base_url" db:"base_url"`
	Model        string                 `json:"model" db:"model"`
	Weight       float64                `json:"weight" db:"weight"`
	Enabled      bool                   `json:"enabled" db:"enabled"`
	Config       map[string]interface{} `json:"config" db:"config"`
	HealthStatus string                 `json:"health_status" db:"health_status"`
	ResponseTime int64                  `json:"response_time" db:"response_time"`
	CreatedAt    time.Time              `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time              `json:"updated_at" db:"updated_at"`
	// Models.dev integration fields (from migration 002)
	ModelsDevProviderID *string    `json:"modelsdev_provider_id,omitempty" db:"modelsdev_provider_id"`
	TotalModels         int        `json:"total_models" db:"total_models"`
	EnabledModels       int        `json:"enabled_models" db:"enabled_models"`
	LastModelsSync      *time.Time `json:"last_models_sync,omitempty" db:"last_models_sync"`
}

// LLMRequest represents a request to an LLM provider
type LLMRequest struct {
	ID             string            `json:"id" db:"id"`
	SessionID      string            `json:"session_id" db:"session_id"`
	UserID         string            `json:"user_id" db:"user_id"`
	Prompt         string            `json:"prompt" db:"prompt"`
	Messages       []Message         `json:"messages" db:"messages"`
	ModelParams    ModelParameters   `json:"model_params" db:"model_params"`
	EnsembleConfig *EnsembleConfig   `json:"ensemble_config" db:"ensemble_config"`
	MemoryEnhanced bool              `json:"memory_enhanced" db:"memory_enhanced"`
	Memory         map[string]string `json:"memory" db:"memory"`
	Status         string            `json:"status" db:"status"`
	CreatedAt      time.Time         `json:"created_at" db:"created_at"`
	StartedAt      *time.Time        `json:"started_at" db:"started_at"`
	CompletedAt    *time.Time        `json:"completed_at" db:"completed_at"`
	RequestType    string            `json:"request_type" db:"request_type"`
	// Tools available for the LLM to call (OpenAI format)
	Tools []Tool `json:"tools,omitempty"`
	// ToolChoice specifies how the model should use tools ("none", "auto", "required", or specific tool)
	ToolChoice interface{} `json:"tool_choice,omitempty"`
}

// Tool represents a tool available for the LLM to call
type Tool struct {
	Type     string       `json:"type"` // Always "function" for now
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a function that can be called
type ToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type LLMResponse struct {
	ID             string                 `json:"id" db:"id"`
	RequestID      string                 `json:"request_id" db:"request_id"`
	ProviderID     string                 `json:"provider_id" db:"provider_id"`
	ProviderName   string                 `json:"provider_name" db:"provider_name"`
	Content        string                 `json:"content" db:"content"`
	Confidence     float64                `json:"confidence" db:"confidence"`
	TokensUsed     int                    `json:"tokens_used" db:"tokens_used"`
	ResponseTime   int64                  `json:"response_time" db:"response_time"`
	FinishReason   string                 `json:"finish_reason" db:"finish_reason"`
	Metadata       map[string]interface{} `json:"metadata" db:"metadata"`
	Selected       bool                   `json:"selected" db:"selected"`
	SelectionScore float64                `json:"selection_score" db:"selection_score"`
	CreatedAt      time.Time              `json:"created_at" db:"created_at"`
	// ToolCalls returned by the LLM when it wants to use tools
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// TokenSplit returns the REAL per-direction token counts the upstream
// provider reported, plus the total.
//
// Why this exists: LLMResponse carries only an aggregate TokensUsed,
// while MOST providers under internal/llm/providers also record the
// genuine per-direction counts in Metadata. Response shapers used to
// synthesise the split as TokensUsed/2 for BOTH directions, which
// (a) discarded those real numbers, (b) made prompt_tokens always
// equal completion_tokens, and (c) produced self-contradicting
// envelopes on odd totals (20 + 20 != 41). That is invented telemetry,
// not an estimate.
//
// Two metadata naming conventions are in use, and BOTH are read here —
// reading only the first would return honest-looking zeros while a real
// split sat available in the map, which is a subtler form of the same
// defect:
//   - OpenAI-shaped: "prompt_tokens" / "completion_tokens" — the large
//     majority (deepseek, mistral, groq, codestral, cerebras,
//     huggingface, helixllm, openrouter, azure, lmstudio, generic,
//     vertex, gemini_api, ollama, …).
//   - Anthropic-shaped: "input_tokens" / "output_tokens" — anthropic,
//     anthropic_cu, claude, cohere.
//
// Coverage caveats, so this comment does not overclaim:
//   - A provider that receives no usage object from its upstream
//     records no split, and this accessor then reports zeros for both
//     directions. That is the honest answer; the remedy is to record
//     the split in the provider, never to invent one here.
//   - Some CLI-backed providers (junie_cli, zen_cli, qwen_cli) write
//     character-length ESTIMATES under these same keys. This accessor
//     cannot distinguish an estimate from a measurement — it faithfully
//     reports whatever the provider recorded. "Real" below means "what
//     the provider recorded", not "independently measured by us".
//
// Contract (§11.4.6 — report what was reported, never invent):
//   - BOTH directions reported ⇒ return them verbatim. The parts are
//     authoritative, so total is the provider's own total when it
//     agrees with the sum, else the sum — the envelope always adds up.
//   - Exactly ONE direction reported, alongside an EXPLICIT
//     "total_tokens" in Metadata ⇒ derive the missing direction as
//     (total − known). This is arithmetic, not invention: the OpenAI
//     usage schema defines total_tokens = prompt_tokens +
//     completion_tokens (its own spec notes reasoning and
//     rejected-prediction tokens are still counted in the completion
//     total), so with two of three quantities known the third is
//     determined. Guarded by total >= known so a malformed total can
//     never produce a negative count.
//   - Exactly ONE direction reported, with only the struct's
//     TokensUsed aggregate ⇒ the REPORTED direction is returned
//     VERBATIM and the missing one is reported as 0 (= unknown).
//     Deriving the missing direction is deliberately NOT attempted
//     here, because TokensUsed carries no total-semantics guarantee:
//     the pre-2026-09-03 claude provider set it to the OUTPUT count,
//     and such rows are persisted and reloaded
//     (internal/database/response_repository.go). Deriving from one
//     would yield a wrong-but-plausible split — for a legacy row with
//     input=7 and TokensUsed=34 (output) it would publish 7/27/34
//     against a truth of 7/34/41, a fabricated 27. An explicit
//     "total_tokens" key is only ever written by a provider that
//     parsed a real usage object, so it IS a true total by
//     construction; the bare aggregate is not.
//     Erasing the reported direction to 0 is equally forbidden and was
//     the defect this branch used to carry: for that same legacy row it
//     published prompt=0 for a response whose prompt side WAS measured
//     at 7, replacing a real number with a false "not reported" — the
//     under-reporting class this accessor exists to end. Keeping the
//     measurement is strictly more information than erasing it and
//     strictly less invention than deriving from the aggregate.
//   - Total, in that partial case, is the largest value that cannot
//     contradict the parts: the explicit "total_tokens" when present and
//     >= the known part, else the TokensUsed aggregate when >= the known
//     part, else the known part itself. A malformed aggregate is
//     discarded — a total smaller than a part it contains is impossible,
//     so it is the untrustworthy aggregate that is dropped, never the
//     measurement. The envelope may therefore under-add (7 + 0 != 41)
//     because one direction is genuinely unknown; it can never
//     self-contradict.
//   - NEITHER reported ⇒ return (0, 0, TokensUsed). Zero means
//     "provider did not report this direction" and is OpenAI-legal;
//     it is the honest answer. NEVER a fabricated half.
//
// Numeric-type tolerance: Metadata is map[string]interface{}, so a
// value written as int in-process may arrive as float64 or
// json.Number after a JSON round-trip. All are accepted; anything
// unparseable is treated as absent rather than guessed at.
func (r *LLMResponse) TokenSplit() (prompt, completion, total int) {
	if r == nil {
		return 0, 0, 0
	}

	total = r.TokensUsed
	promptVal, promptOK := metadataFirstInt(r.Metadata, "prompt_tokens", "input_tokens")
	completionVal, completionOK := metadataFirstInt(r.Metadata, "completion_tokens", "output_tokens")

	switch {
	case promptOK && completionOK:
		prompt, completion = promptVal, completionVal
		// Parts are authoritative. Honour the provider's own total only
		// when it agrees with them; otherwise derive it so the emitted
		// envelope never contradicts itself.
		if totalVal, ok := metadataFirstInt(r.Metadata, "total_tokens"); ok && totalVal == prompt+completion {
			return prompt, completion, totalVal
		}
		return prompt, completion, prompt + completion

	case promptOK || completionOK:
		// Partial report. The reported direction is a REAL measurement
		// and is returned verbatim — it is never erased. Only the MISSING
		// direction is in question, and it is derived ONLY from an
		// EXPLICIT total_tokens key, never from the bare TokensUsed
		// aggregate — see the contract above for why the two are not
		// interchangeable.
		known := promptVal + completionVal // exactly one is non-zero here

		// Floor the total at the part we measured: a total smaller than
		// one of its own parts is impossible, so a smaller aggregate is
		// malformed and gets discarded — never the measurement (§11.4.6).
		total = known
		if r.TokensUsed >= known {
			total = r.TokensUsed
		}

		if totalVal, totalOK := metadataFirstInt(r.Metadata, "total_tokens"); totalOK && totalVal >= known {
			// An explicit, well-formed total is authoritative, and with
			// two of three quantities known the third is determined.
			if promptOK {
				return promptVal, totalVal - promptVal, totalVal
			}
			return totalVal - completionVal, completionVal, totalVal
		}

		// No usable explicit total: the missing direction stays unknown
		// (0), which is OpenAI-legal and reads as "not reported".
		return promptVal, completionVal, total

	default:
		// Provider reported only an aggregate. Report the directions as
		// unknown (0) rather than inventing halves.
		return 0, 0, total
	}
}

// metadataFirstInt returns the first of the given keys that carries a
// usable non-negative integer. Providers in this repo use two naming
// conventions for the same quantity (OpenAI-shaped `prompt_tokens` and
// Anthropic-shaped `input_tokens`), so callers pass the aliases in
// precedence order rather than picking one and silently ignoring data
// written under the other.
func metadataFirstInt(md map[string]interface{}, keys ...string) (int, bool) {
	for _, key := range keys {
		if n, ok := metadataInt(md, key); ok {
			return n, true
		}
	}
	return 0, false
}

// metadataInt reads a non-negative integer out of an
// interface{}-valued metadata map, tolerating the numeric types a
// value can take before and after a JSON round-trip. It reports
// ok=false for a missing key or an unparseable/negative value — the
// caller then treats the direction as unreported rather than guessing.
func metadataInt(md map[string]interface{}, key string) (int, bool) {
	if md == nil {
		return 0, false
	}
	raw, present := md[key]
	if !present || raw == nil {
		return 0, false
	}

	// Every branch below range-checks against MaxInt32 before
	// converting, matching the float branch. Without this an int64 or
	// uint64 wider than the platform int silently wraps to a negative
	// or nonsense value on a 32-bit build, and uint64 can exceed even a
	// 64-bit int. MaxInt32 sits far above any real token count, so a
	// value beyond it is malformed rather than large, and is reported
	// unusable instead of guessed at (§11.4.6).
	const maxToken = int64(math.MaxInt32)

	var n int
	switch v := raw.(type) {
	case int:
		if int64(v) > maxToken {
			return 0, false
		}
		n = v
	case int32:
		n = int(v)
	case int64:
		if v > maxToken {
			return 0, false
		}
		n = int(v)
	case uint:
		if uint64(v) > uint64(maxToken) {
			return 0, false
		}
		n = int(v)
	case uint32:
		if uint64(v) > uint64(maxToken) {
			return 0, false
		}
		n = int(v)
	case uint64:
		if v > uint64(maxToken) {
			return 0, false
		}
		n = int(v)
	case float32:
		// float32 widens exactly to float64; validate there.
		parsed, ok := exactNonNegativeIntFromFloat(float64(v))
		if !ok {
			return 0, false
		}
		n = parsed
	case float64:
		// A token count arriving as a float is only usable if it is a
		// finite, integral, in-range value. Bare int(v) was wrong twice
		// over: it silently TRUNCATED a fractional value (7.5 became 7,
		// a guess the doc-comment disclaims) and its out-of-range
		// behaviour is implementation-defined in the Go spec — amd64
		// yields MinInt64 while arm64 saturates to MaxInt64, which the
		// negative check would let through and then overflow on
		// prompt+completion. Validate before converting so the same
		// input is rejected identically on every architecture, matching
		// how json.Number("7.5") is already treated as absent.
		parsed, ok := exactNonNegativeIntFromFloat(v)
		if !ok {
			return 0, false
		}
		n = parsed
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return 0, false
		}
		if parsed > maxToken {
			return 0, false
		}
		n = int(parsed)
	default:
		return 0, false
	}

	if n < 0 {
		return 0, false
	}
	return n, true
}

// exactNonNegativeIntFromFloat converts a float-typed metadata value to
// an int ONLY when the value is finite, non-negative, integral, and
// within int32 range (comfortably above any real token count, and safe
// on 32-bit builds). Anything else is reported unusable so the caller
// treats the direction as unreported instead of guessing at a
// truncated or platform-dependent number (§11.4.6).
func exactNonNegativeIntFromFloat(v float64) (int, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	if v < 0 || v > float64(math.MaxInt32) {
		return 0, false
	}
	if v != math.Trunc(v) {
		return 0, false
	}
	return int(v), true
}

// ToolCall represents a tool call requested by the LLM
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // Always "function" for now
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction contains the function name and arguments to call
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string of arguments
}

type Message struct {
	Role      string                 `json:"role" db:"role"`
	Content   string                 `json:"content" db:"content"`
	Name      *string                `json:"name" db:"name"`
	ToolCalls map[string]interface{} `json:"tool_calls" db:"tool_calls"`
	// ToolCallID is the id of the assistant tool_call that this
	// message is responding to. REQUIRED by every upstream provider
	// (OpenAI, Cerebras, Mistral, …) when role="tool". Without it
	// providers reject the request with errors like
	// "messages.N.tool.tool_call_id: Field required" or
	// "Tool call id has to be defined." (CONST-032 reproduction:
	// challenges/scripts/opencode_tool_result_followup_challenge.sh).
	ToolCallID string `json:"tool_call_id,omitempty" db:"tool_call_id"`
	// AssistantToolCalls is the typed array of tool calls an assistant
	// message is invoking. REQUIRED on the assistant message that
	// PRECEDES a tool message — without it upstream providers (DeepSeek,
	// OpenAI, Cerebras, …) reject with "Messages with role 'tool' must
	// be a response to a preceding message with 'tool_calls'" (CONST-032
	// reproduction:
	// challenges/scripts/opencode_parallel_tool_calls_challenge.sh).
	// The legacy ToolCalls map field above is preserved for backward
	// compatibility but providers MUST read from this typed slice for
	// correct ordered emission.
	AssistantToolCalls []ToolCall `json:"-" db:"-"`
}

type ModelParameters struct {
	Model            string                 `json:"model" db:"model"`
	Temperature      float64                `json:"temperature" db:"temperature"`
	MaxTokens        int                    `json:"max_tokens" db:"max_tokens"`
	TopP             float64                `json:"top_p" db:"top_p"`
	StopSequences    []string               `json:"stop_sequences" db:"stop_sequences"`
	ProviderSpecific map[string]interface{} `json:"provider_specific" db:"provider_specific"`
}

type EnsembleConfig struct {
	Strategy            string   `json:"strategy" db:"strategy"`
	MinProviders        int      `json:"min_providers" db:"min_providers"`
	ConfidenceThreshold float64  `json:"confidence_threshold" db:"confidence_threshold"`
	FallbackToBest      bool     `json:"fallback_to_best" db:"fallback_to_best"`
	Timeout             int      `json:"timeout" db:"timeout"`
	PreferredProviders  []string `json:"preferred_providers" db:"preferred_providers"`
}

type UserSession struct {
	ID           string                 `json:"id" db:"id"`
	UserID       string                 `json:"user_id" db:"user_id"`
	SessionToken string                 `json:"session_token" db:"session_token"`
	Context      map[string]interface{} `json:"context" db:"context"`
	MemoryID     *string                `json:"memory_id" db:"memory_id"`
	Status       string                 `json:"status" db:"status"`
	RequestCount int                    `json:"request_count" db:"request_count"`
	LastActivity time.Time              `json:"last_activity" db:"last_activity"`
	ExpiresAt    time.Time              `json:"expires_at" db:"expires_at"`
	CreatedAt    time.Time              `json:"created_at" db:"created_at"`
}

type CogneeMemory struct {
	ID          string                 `json:"id" db:"id"`
	SessionID   *string                `json:"session_id" db:"session_id"`
	DatasetName string                 `json:"dataset_name" db:"dataset_name"`
	ContentType string                 `json:"content_type" db:"content_type"`
	Content     string                 `json:"content" db:"content"`
	VectorID    string                 `json:"vector_id" db:"vector_id"`
	GraphNodes  map[string]interface{} `json:"graph_nodes" db:"graph_nodes"`
	SearchKey   string                 `json:"search_key" db:"search_key"`
	CreatedAt   time.Time              `json:"created_at" db:"created_at"`
}

type MemorySource struct {
	DatasetName    string  `json:"dataset_name"`
	Content        string  `json:"content"`
	RelevanceScore float64 `json:"relevance_score"`
	SourceType     string  `json:"source_type"`
}

// ProviderCapabilities describes capabilities exposed by an LLM provider.
type ProviderCapabilities struct {
	SupportedModels         []string          `json:"supported_models"`
	SupportedFeatures       []string          `json:"supported_features"`
	SupportedRequestTypes   []string          `json:"supported_request_types"`
	SupportsStreaming       bool              `json:"supports_streaming"`
	SupportsFunctionCalling bool              `json:"supports_function_calling"`
	SupportsVision          bool              `json:"supports_vision"`
	Limits                  ModelLimits       `json:"limits"`
	Metadata                map[string]string `json:"metadata"`

	// LSP specific capabilities
	SupportsTools          bool `json:"supports_tools"`
	SupportsSearch         bool `json:"supports_search"`
	SupportsReasoning      bool `json:"supports_reasoning"`
	SupportsCodeCompletion bool `json:"supports_code_completion"`
	SupportsCodeAnalysis   bool `json:"supports_code_analysis"`
	SupportsRefactoring    bool `json:"supports_refactoring"`
}

// ModelLimits defines the operational limits of an LLM model.
type ModelLimits struct {
	MaxTokens             int `json:"max_tokens"`
	MaxInputLength        int `json:"max_input_length"`
	MaxOutputLength       int `json:"max_output_length"`
	MaxConcurrentRequests int `json:"max_concurrent_requests"`
}

// LSP-related types for Language Server Protocol integration

// CodeIntelligence represents comprehensive code intelligence from LSP
type CodeIntelligence struct {
	FilePath       string            `json:"file_path"`
	Diagnostics    []*Diagnostic     `json:"diagnostics"`
	Completions    []*CompletionItem `json:"completions"`
	Hover          *HoverInfo        `json:"hover"`
	Definitions    []*Location       `json:"definitions"`
	References     []*Location       `json:"references"`
	Symbols        []*SymbolInfo     `json:"symbols"`
	SemanticTokens *SemanticTokens   `json:"semantic_tokens"`
}

// Diagnostic represents a diagnostic message from LSP
type Diagnostic struct {
	Range              Range                          `json:"range"`
	Severity           int                            `json:"severity"`
	Code               string                         `json:"code"`
	Source             string                         `json:"source"`
	Message            string                         `json:"message"`
	RelatedInformation []DiagnosticRelatedInformation `json:"related_information"`
}

// DiagnosticRelatedInformation represents related diagnostic information
type DiagnosticRelatedInformation struct {
	Location Location `json:"location"`
	Message  string   `json:"message"`
}

// CompletionItem represents a completion item from LSP
type CompletionItem struct {
	Label         string `json:"label"`
	Kind          int    `json:"kind"`
	Detail        string `json:"detail"`
	Documentation string `json:"documentation"`
	InsertText    string `json:"insert_text"`
}

// HoverInfo represents hover information from LSP
type HoverInfo struct {
	Content  string `json:"content"`
	Language string `json:"language"`
}

// Location represents a location in a file
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Range represents a range in a text document
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Position represents a position in a text document
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// SymbolInfo represents symbol information from LSP
type SymbolInfo struct {
	Name          string        `json:"name"`
	Kind          int           `json:"kind"`
	Location      Location      `json:"location"`
	ContainerName string        `json:"container_name"`
	Children      []*SymbolInfo `json:"children"`
}

// SemanticTokens represents semantic tokens from LSP
type SemanticTokens struct {
	Data []int `json:"data"`
}

// WorkspaceEdit represents a workspace edit from LSP
type WorkspaceEdit struct {
	Changes map[string][]*TextEdit `json:"changes"`
}

// TextEdit represents a text edit
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}
