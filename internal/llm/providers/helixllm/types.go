package helixllm

// ChatCompletionRequest represents an OpenAI-compatible chat completion request
type ChatCompletionRequest struct {
	Model            string    `json:"model"`
	Messages         []Message `json:"messages"`
	Stream           bool      `json:"stream,omitempty"`
	Temperature      float64   `json:"temperature,omitempty"`
	MaxTokens        int       `json:"max_tokens,omitempty"`
	TopP             float64   `json:"top_p,omitempty"`
	FrequencyPenalty float64   `json:"frequency_penalty,omitempty"`
	PresencePenalty  float64   `json:"presence_penalty,omitempty"`
	Stop             []string  `json:"stop,omitempty"`

	// HXC-349: OpenAI tool-calling passthrough. The handler
	// (internal/handlers/openai_compatible.go) accepts a `tools` array and
	// copies it onto models.LLMRequest.Tools, but this wire type previously
	// had NO Tools field — so the schema died in translation here and never
	// reached the serving layer. Measured symptom: prompt_tokens IDENTICAL
	// with and without a 15,770-byte tools array (42 == 42), a zero delta
	// proving the model never saw the schema; `tool_choice:"required"` also
	// returned prose with HTTP 200 instead of a tool call.
	//
	// `omitempty` on both is load-bearing: a request with no tools MUST NOT
	// emit `tools` / `tool_choice` keys at all, so upstreams that reject an
	// empty array (or treat a present-but-null tool_choice as a constraint)
	// see a byte-identical request to the pre-fix one.
	Tools []Tool `json:"tools,omitempty"`
	// ToolChoice is "none" | "auto" | "required" | {"type":"function",...}
	// and is passed through opaquely — this provider does not interpret it.
	ToolChoice interface{} `json:"tool_choice,omitempty"`
}

// Tool is one OpenAI-format tool the model may call.
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction is a tool's name + JSON-Schema parameter description.
type ToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// ToolCall is a tool invocation requested by the model (response side) or
// replayed back to it on a follow-up turn (request side).
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries the called name and its raw JSON argument string.
// Arguments stays a string: it is the model's verbatim JSON payload and
// re-encoding it through a map would reorder keys and lose fidelity.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message represents a chat message
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`

	// HXC-349: tool-loop fields. ToolCalls appears on an assistant message
	// (both as a RESPONSE from the model and when that turn is replayed back
	// on the follow-up request); ToolCallID appears on a role="tool" message
	// answering a specific call. Both are REQUIRED by upstream providers for
	// a multi-turn tool loop — without them a follow-up is rejected with
	// "messages.N.tool.tool_call_id: Field required" or "Messages with role
	// 'tool' must be a response to a preceding message with 'tool_calls'"
	// (the failure modes already documented on models.Message). Forwarding
	// `tools` without these would send a schema the model can act on but
	// leave the client unable to return the result — a half-wired loop.
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ChatCompletionResponse represents an OpenAI-compatible chat completion response
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice represents a completion choice
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message,omitempty"`
	Delta        Message `json:"delta,omitempty"`
	FinishReason string  `json:"finish_reason,omitempty"`
}

// Usage represents token usage
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// EmbeddingRequest represents an embedding request
type EmbeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// EmbeddingResponse represents an embedding response
type EmbeddingResponse struct {
	Object string          `json:"object"`
	Data   []EmbeddingData `json:"data"`
	Model  string          `json:"model"`
	Usage  Usage           `json:"usage"`
}

// EmbeddingData represents a single embedding
type EmbeddingData struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// ModelsResponse represents the models list response
type ModelsResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// ModelInfo represents a single model's information
type ModelInfo struct {
	ID           string          `json:"id"`
	Object       string          `json:"object"`
	Created      int64           `json:"created"`
	OwnedBy      string          `json:"owned_by"`
	Capabilities map[string]bool `json:"capabilities,omitempty"`
}

// HealthResponse represents the health check response
type HealthResponse struct {
	Status    string          `json:"status"`
	Version   string          `json:"version"`
	Mode      string          `json:"mode"`
	Uptime    string          `json:"uptime"`
	Services  map[string]bool `json:"services"`
	Timestamp int64           `json:"timestamp"`
}
