// Package helixllm provides a HelixLLM provider for HelixAgent.
// It integrates the HelixLLM submodule as a first-class LLM provider,
// leveraging HelixLLM's OpenAI-compatible API and RAG capabilities.
package helixllm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"dev.helix.agent/internal/llm"
	"dev.helix.agent/internal/models"
	"dev.helix.agent/internal/netaddr"
)

const (
	// DefaultEndpoint is the HelixLLM gateway's default TLS serving endpoint
	// (the HelixLLM binary listens on :8443 with TLS by default —
	// HelixLLM internal/shared/config Server.Port default 8443). Exported so
	// consumers (e.g. the provider registry) reference a single source of
	// truth instead of re-hardcoding the host:port (CONST-045).
	DefaultEndpoint = "https://localhost:8443"
	// defaultEndpoint is retained as an internal alias for readability.
	defaultEndpoint = DefaultEndpoint

	// EnvEndpoint overrides the endpoint with any OpenAI-compatible base URL
	// (TLS gateway or plain-HTTP). EnvLocalOpenAIEndpoint is a first-class,
	// higher-precedence seam for pointing at a LOCAL plain-HTTP
	// OpenAI-compatible HelixLLM router (e.g. the max-perf llama.cpp image
	// serving /v1/chat/completions on a plain-HTTP port such as
	// http://localhost:8080). Neither is a hardcoded host — both are
	// operator-supplied (CONST-045).
	EnvEndpoint            = "HELIX_LLM_ENDPOINT"
	EnvLocalOpenAIEndpoint = "HELIX_LLM_LOCAL_OPENAI_ENDPOINT"

	// EnvHost / EnvPort are the CLIENT-side connection target that lets an
	// operator point HelixAgent at a plain-HTTP OpenAI-compatible HelixLLM /
	// llama.cpp router reachable over the LAN or VPN (not just localhost). They
	// compose the BASE endpoint http://${HELIX_LLM_HOST}:${HELIX_LLM_PORT} —
	// deliberately WITHOUT a trailing /v1, because Complete/CompleteStream/
	// GetModels append the OpenAI /v1/... path themselves (a trailing /v1 here
	// would double to /v1/v1 → 404, the load-bearing base-URL gotcha). They sit
	// BELOW the explicit endpoint seams in precedence, so an operator who already
	// sets HELIX_LLM_ENDPOINT or HELIX_LLM_LOCAL_OPENAI_ENDPOINT is unaffected.
	// No hardcoded reachable host — both default to localhost:18434 and are
	// operator-supplied (CONST-045). Example LAN use:
	//   HELIX_LLM_HOST=10.6.100.221 HELIX_LLM_PORT=18434 HELIX_LLM_API_KEY=<key>
	EnvHost = "HELIX_LLM_HOST"
	EnvPort = "HELIX_LLM_PORT"

	// defaultHost / defaultPort are the CLIENT-target fallbacks used ONLY when
	// the HELIX_LLM_HOST/HELIX_LLM_PORT composition is engaged and one half is
	// omitted. 18434 is the plain-HTTP OpenAI-compatible coder port.
	defaultHost = "localhost"
	defaultPort = "18434"

	defaultModel       = "helixllm-default"
	defaultTimeout     = 60 * time.Second
	chatEndpoint       = "/v1/chat/completions"
	embeddingsEndpoint = "/v1/embeddings"
	modelsEndpoint     = "/v1/models"
	healthEndpoint     = "/internal/health"

	// modelsCacheTTL bounds how long a live /v1/models listing is reused
	// before it is re-fetched (CONST-038 freshness window).
	modelsCacheTTL = 30 * time.Second
	// modelsListTimeout bounds a single listing call so a slow or hung
	// serving layer can never stall capability reporting (HA-F2-004).
	modelsListTimeout = 2 * time.Second
)

// normalizeBase makes an OpenAI-compatible BASE URL safe to concatenate with
// the hardcoded /v1/... paths (chatEndpoint / embeddingsEndpoint / modelsEndpoint).
// It trims surrounding whitespace, any trailing slash, and a single trailing
// "/v1" segment, so a base supplied either as "http://h:18434" OR
// "http://h:18434/v1" both resolve to "http://h:18434" — eliminating the
// double-"/v1/v1" 404 gotcha regardless of how the operator wrote the env var.
func normalizeBase(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return s
	}
	s = strings.TrimRight(s, "/")
	if strings.HasSuffix(s, "/v1") {
		s = strings.TrimRight(strings.TrimSuffix(s, "/v1"), "/")
	}
	return s
}

// resolveEndpoint picks the HelixLLM endpoint by precedence, without ever
// hardcoding a reachable host beyond the documented localhost default
// (CONST-045). Every branch is run through normalizeBase so no source can
// introduce the double-/v1 gotcha. Precedence:
//  1. explicit (cfg.Endpoint set by the caller / provider registry baseURL)
//  2. HELIX_LLM_LOCAL_OPENAI_ENDPOINT — the local plain-HTTP OpenAI router seam
//  3. HELIX_LLM_ENDPOINT — the general endpoint override (TLS gateway or any
//     OpenAI-compatible base URL)
//  4. HELIX_LLM_HOST / HELIX_LLM_PORT — the LAN/VPN client-target composition
//     http://${host|localhost}:${port|18434} (engaged when either is set)
//  5. DefaultEndpoint — the TLS :8443 gateway default (unchanged legacy behaviour)
func resolveEndpoint(explicit string) string {
	if explicit != "" {
		return normalizeBase(explicit)
	}
	if v := os.Getenv(EnvLocalOpenAIEndpoint); v != "" {
		return normalizeBase(v)
	}
	if v := os.Getenv(EnvEndpoint); v != "" {
		return normalizeBase(v)
	}
	// LAN/VPN client-target composition. Engaged when either HELIX_LLM_HOST or
	// HELIX_LLM_PORT is set; each missing half falls back to its default so an
	// operator can pin only the host (HELIX_LLM_HOST=10.6.100.221) and inherit
	// port 18434.
	host := strings.TrimSpace(os.Getenv(EnvHost))
	port := strings.TrimSpace(os.Getenv(EnvPort))
	if host != "" || port != "" {
		if host == "" {
			host = defaultHost
		}
		// A bind-all address is never a valid CLIENT connect target; map it to
		// localhost so a server-bind value (e.g. the .env.example
		// HELIX_LLM_HOST=0.0.0.0) leaking into the client process still yields a
		// reachable endpoint (§11.4.6 — safe, evidence-based normalisation).
		switch host {
		case "0.0.0.0", "::", "[::]":
			host = defaultHost
		}
		if port == "" {
			port = defaultPort
		}
		// HXC-286: naive "http://" + host + ":" + port breaks for an
		// unbracketed IPv6 HELIX_LLM_HOST (see
		// address_composition_red_test.go for the measured failure).
		// HelixLLM is a submodule/service DISTINCT from HelixAgent (see
		// this file's package doc), so this uses netaddr.BaseURLString —
		// bracket-safe, no default-substitution — rather than
		// helixendpoint.BaseURL, whose fallback host is documented
		// specifically for HelixAgent's own endpoint and would be the
		// wrong placeholder here.
		return normalizeBase(netaddr.BaseURLString("http", host, port))
	}
	return DefaultEndpoint
}

// Provider implements the LLMProvider interface for HelixLLM
type Provider struct {
	endpoint      string
	apiKey        string
	model         string
	timeout       time.Duration
	tlsSkipVerify bool
	useLlamaCpp   bool
	httpClient    *http.Client

	mu          sync.RWMutex
	initialized bool
	initErr     error
	initOnce    sync.Once

	// Live model listing cache (HA-F2-004). modelsMu serialises fetch+read;
	// modelsCache/modelsFetched hold the last successful GET /v1/models
	// result and are reused only within modelsCacheTTL.
	modelsMu      sync.Mutex
	modelsCache   []string
	modelsFetched time.Time
}

// Config holds configuration for the HelixLLM provider
type Config struct {
	Endpoint      string
	APIKey        string
	Model         string
	Timeout       time.Duration
	TLSSkipVerify bool
	// UseLlamaCpp is an ADVISORY, HelixAgent-side hint. When false, this
	// provider sends the X-Helix-LLM-Use-LlamaCpp: false HTTP header on chat
	// requests. Sourced from HELIX_LLM_USE_LLAMACPP env var (default false).
	//
	// CONTRACT NOTE (verified against the HelixLLM submodule, github.com/
	// HelixDevelopment/HelixLLM): the current HelixLLM gateway does NOT read
	// this header (its handlers consume only Accept-Language / Authorization /
	// Accept). The AUTHORITATIVE toggle for HelixLLM's embedded llama.cpp
	// backend is SERVER-SIDE — HelixLLM's own config field LlamaServerEmbed
	// (env HELIX_LLAMA_SERVER_EMBEDDED, default true), set on the HelixLLM
	// process, NOT communicated per-request from HelixAgent. The header is
	// retained here for forward-compatibility (harmless if unread) but MUST
	// NOT be relied upon to change backend routing on today's HelixLLM.
	UseLlamaCpp bool
}

// NewProvider creates a new HelixLLM provider
func NewProvider(cfg Config) *Provider {
	// Resolve the endpoint via the documented precedence (explicit →
	// local plain-HTTP OpenAI seam → general env override → TLS default).
	// No hardcoded reachable host beyond the localhost default (CONST-045).
	cfg.Endpoint = resolveEndpoint(cfg.Endpoint)
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.TLSSkipVerify || getEnvBool("HELIX_LLM_TLS_SKIP_VERIFY", false),
		},
	}

	useLlamaCpp := cfg.UseLlamaCpp
	if v := os.Getenv("HELIX_LLM_USE_LLAMACPP"); v != "" {
		useLlamaCpp = strings.EqualFold(v, "true") || v == "1"
	}

	return &Provider{
		endpoint:      cfg.Endpoint,
		apiKey:        cfg.APIKey,
		model:         cfg.Model,
		timeout:       cfg.Timeout,
		tlsSkipVerify: cfg.TLSSkipVerify,
		useLlamaCpp:   useLlamaCpp,
		httpClient: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: transport,
		},
	}
}

// Endpoint returns the resolved HelixLLM base URL this provider will call.
// Useful for logging the effective endpoint after env/default resolution.
func (p *Provider) Endpoint() string { return p.endpoint }

// NewProviderFromEnv creates a new HelixLLM provider from environment variables
func NewProviderFromEnv() *Provider {
	return NewProvider(Config{
		Endpoint:      os.Getenv("HELIX_LLM_ENDPOINT"),
		APIKey:        os.Getenv("HELIX_LLM_API_KEY"),
		Model:         os.Getenv("HELIX_LLM_MODEL"),
		TLSSkipVerify: os.Getenv("HELIX_LLM_TLS_SKIP_VERIFY") == "true",
	})
}

// buildMessages translates internal messages onto the OpenAI wire shape,
// shared by Complete and CompleteStream so the two paths cannot drift.
//
// HXC-349: this now also forwards the tool-loop fields. AssistantToolCalls is
// the authoritative typed slice on models.Message (its legacy untyped
// ToolCalls map is explicitly documented there as backward-compat only and is
// NOT read here — reading it would emit unordered tool calls). ToolCallID is
// forwarded verbatim so a role="tool" reply binds to the call it answers.
func buildMessages(in []models.Message) []Message {
	out := make([]Message, 0, len(in))
	for _, msg := range in {
		role := msg.Role
		if role == "" {
			role = "user"
		}
		m := Message{
			Role:       role,
			Content:    msg.Content,
			ToolCallID: msg.ToolCallID,
		}
		for _, tc := range msg.AssistantToolCalls {
			m.ToolCalls = append(m.ToolCalls, toolCallFromModel(tc))
		}
		out = append(out, m)
	}
	return out
}

// toolCallFromModel maps an internal tool call onto the wire shape.
func toolCallFromModel(tc models.ToolCall) ToolCall {
	typ := tc.Type
	if typ == "" {
		typ = "function"
	}
	return ToolCall{
		ID:   tc.ID,
		Type: typ,
		Function: ToolCallFunction{
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		},
	}
}

// toolCallsToModel maps wire tool calls back onto the internal shape so the
// handler's existing map-back (openai_compatible.go convertSingleResponseToOpenAI)
// has something to render. Defaults a missing type to "function": some
// upstreams return bare {id, function} and an empty type is rejected by
// OpenAI clients as malformed.
func toolCallsToModel(in []ToolCall) []models.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]models.ToolCall, 0, len(in))
	for _, tc := range in {
		typ := tc.Type
		if typ == "" {
			typ = "function"
		}
		out = append(out, models.ToolCall{
			ID:   tc.ID,
			Type: typ,
			Function: models.ToolCallFunction{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}
	return out
}

// buildTools translates internal tool declarations onto the OpenAI wire shape.
// Returns nil for an empty input so `omitempty` elides the key entirely —
// a no-tools request stays byte-identical to its pre-HXC-349 form.
func buildTools(in []models.Tool) []Tool {
	if len(in) == 0 {
		return nil
	}
	out := make([]Tool, 0, len(in))
	for _, t := range in {
		typ := t.Type
		if typ == "" {
			// OpenAI clients reject a tool with an empty type as malformed;
			// "function" is the only type the schema currently defines.
			typ = "function"
		}
		out = append(out, Tool{
			Type: typ,
			Function: ToolFunction{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		})
	}
	return out
}

// Complete implements the LLMProvider interface
func (p *Provider) Complete(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error) {
	if err := p.initialize(); err != nil {
		return nil, err
	}

	model := req.ModelParams.Model
	if model == "" {
		model = p.model
	}

	messages := buildMessages(req.Messages)

	chatReq := ChatCompletionRequest{
		Model:       model,
		Messages:    messages,
		Stream:      false,
		Temperature: req.ModelParams.Temperature,
		MaxTokens:   req.ModelParams.MaxTokens,
		TopP:        req.ModelParams.TopP,
		Tools:       buildTools(req.Tools),
		ToolChoice:  req.ToolChoice,
	}

	endpoint := p.endpoint + chatEndpoint
	body, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	// Advisory X-Helix-LLM-Use-LlamaCpp header (forward-compat only). The
	// current HelixLLM gateway does NOT read this header — the authoritative
	// llama.cpp toggle is server-side (HELIX_LLAMA_SERVER_EMBEDDED). See the
	// Config.UseLlamaCpp doc for the verified contract. Sent when false so a
	// future HelixLLM version could honour it; harmless if unread.
	if !p.useLlamaCpp {
		httpReq.Header.Set("X-Helix-LLM-Use-LlamaCpp", "false")
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var chatResp ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("no response from model")
	}

	// HXC-349: surface the upstream tool call + finish_reason. Previously
	// both were parsed off the wire into Choice/Message and then silently
	// dropped here, so a model that asked to call a tool reached the handler
	// as plain prose and the handler's map-back had nothing to map.
	choice := chatResp.Choices[0]
	return &models.LLMResponse{
		Content:      choice.Message.Content,
		TokensUsed:   chatResp.Usage.TotalTokens,
		FinishReason: choice.FinishReason,
		ToolCalls:    toolCallsToModel(choice.Message.ToolCalls),
		Metadata: map[string]interface{}{
			"model":             chatResp.Model,
			"provider":          "helixllm",
			"prompt_tokens":     chatResp.Usage.PromptTokens,
			"completion_tokens": chatResp.Usage.CompletionTokens,
		},
	}, nil
}

// CompleteStream implements the LLMProvider interface for streaming
func (p *Provider) CompleteStream(ctx context.Context, req *models.LLMRequest) (<-chan *models.LLMResponse, error) {
	if err := p.initialize(); err != nil {
		return nil, err
	}

	model := req.ModelParams.Model
	if model == "" {
		model = p.model
	}

	messages := buildMessages(req.Messages)

	chatReq := ChatCompletionRequest{
		Model:       model,
		Messages:    messages,
		Stream:      true,
		Temperature: req.ModelParams.Temperature,
		MaxTokens:   req.ModelParams.MaxTokens,
		TopP:        req.ModelParams.TopP,
		Tools:       buildTools(req.Tools),
		ToolChoice:  req.ToolChoice,
	}

	endpoint := p.endpoint + chatEndpoint
	body, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	resultChan := make(chan *models.LLMResponse)
	go p.handleStream(resp, resultChan)

	return resultChan, nil
}

func (p *Provider) handleStream(resp *http.Response, resultChan chan<- *models.LLMResponse) {
	defer close(resultChan)
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	for {
		var streamResp ChatCompletionResponse
		if err := decoder.Decode(&streamResp); err != nil {
			if err == io.EOF {
				return
			}
			resultChan <- &models.LLMResponse{
				Metadata: map[string]interface{}{
					"error": fmt.Sprintf("stream decode error: %v", err),
				},
			}
			return
		}

		if len(streamResp.Choices) > 0 {
			// HXC-349: carry tool calls + finish_reason on stream chunks too,
			// read off the same Message field this loop already reads Content
			// from. NOTE (§11.4.6, honest gap): this decoder consumes
			// concatenated JSON objects and reads Choice.Message, not
			// Choice.Delta — an upstream that emits true SSE `data:` frames
			// with incremental Delta tool-call fragments is NOT reassembled
			// here. That pre-existing limitation is unchanged by this fix; the
			// handler deliberately routes stream+tools through the
			// non-streaming path (streamToolCallViaNonStreaming) for exactly
			// this reason.
			choice := streamResp.Choices[0]
			resultChan <- &models.LLMResponse{
				Content:      choice.Message.Content,
				TokensUsed:   streamResp.Usage.TotalTokens,
				FinishReason: choice.FinishReason,
				ToolCalls:    toolCallsToModel(choice.Message.ToolCalls),
				Metadata: map[string]interface{}{
					"model": streamResp.Model,
				},
			}
		}
	}
}

// HealthCheck implements the LLMProvider interface
func (p *Provider) HealthCheck() error {
	if err := p.initialize(); err != nil {
		return err
	}

	// Probe the HelixLLM gateway's own health path first.
	resp, err := p.httpClient.Get(p.endpoint + healthEndpoint)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	// HA-CB-001: healthEndpoint ("/internal/health") is a HelixLLM-GATEWAY
	// path. This provider is also pointed at plain OpenAI-compatible servers
	// via HELIX_LLM_LOCAL_OPENAI_ENDPOINT (e.g. a llama.cpp/vLLM-style server),
	// which do not implement it and answer 404/405 while serving
	// /v1/chat/completions perfectly. Reporting such a backend "unhealthy" is a
	// false negative: it made the provider health monitor lie, and — before the
	// paired fix in circuitBreakerProvider.HealthCheck — it permanently opened
	// the traffic circuit breaker against a fully working backend.
	//
	// So when the gateway path is merely ABSENT, fall back to the
	// OpenAI-compatible liveness surface this provider already relies on for
	// GetModels. Any other status (5xx, 401, 429, ...) is a real health signal
	// and is reported as-is — we do not fail open.
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}

	modelsResp, modelsErr := p.httpClient.Get(p.endpoint + modelsEndpoint)
	if modelsErr != nil {
		return fmt.Errorf("health check failed: %s absent (status %d) and %s unreachable: %w",
			healthEndpoint, resp.StatusCode, modelsEndpoint, modelsErr)
	}
	defer modelsResp.Body.Close()

	if modelsResp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned status %d (%s absent, status %d)",
			modelsResp.StatusCode, healthEndpoint, resp.StatusCode)
	}

	return nil
}

// GetModels performs a live, bounded GET /v1/models against the provider's
// endpoint and returns the ids the serving layer actually reports, in server
// order. It is the sole source for SupportedModels (HA-F2-004, CONST-036):
// nothing here invents a model id. On any failure (unreachable, non-200,
// malformed body) it returns the error and the empty list — capabilities
// fail CLOSED, never to a fabricated placeholder.
func (p *Provider) GetModels(ctx context.Context) ([]string, error) {
	if err := p.initialize(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint+modelsEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create models request: %w", err)
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list models returned status %d", resp.StatusCode)
	}

	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode models list: %w", err)
	}

	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// servedModels returns the cached live listing, refreshing it when the cache
// is empty or older than modelsCacheTTL. A failed refresh keeps the last
// good listing for its TTL (bounded staleness per CONST-038); a provider
// that never listed successfully yields an honest-empty slice — never the
// placeholder defaultModel as a "supported" claim.
//
// modelsMu is held across the fetch; GetModels does not re-acquire it, so
// there is no deadlock, and the per-call timeout caps the hold at
// modelsListTimeout.
func (p *Provider) servedModels() []string {
	p.modelsMu.Lock()
	defer p.modelsMu.Unlock()

	if !p.modelsFetched.IsZero() && time.Since(p.modelsFetched) < modelsCacheTTL {
		return p.modelsCache
	}

	ctx, cancel := context.WithTimeout(context.Background(), modelsListTimeout)
	defer cancel()
	ids, err := p.GetModels(ctx)
	if err == nil {
		p.modelsCache = ids
		p.modelsFetched = time.Now()
	}
	return p.modelsCache
}

// GetCapabilities implements the LLMProvider interface.
//
// HA-F2-004 (CONST-036/CONST-040 class): SupportedModels comes ONLY from the
// live serving layer via servedModels. Capability flags carry only what this
// provider's code evidences: SupportsStreaming is backed by the real
// CompleteStream implementation. Flags with no serving-layer or code
// evidence here (reasoning; code completion/analysis/refactoring; embeddings
// — the endpoint constant has no calling method) are reported FALSE/absent
// rather than asserted true.
//
// HXC-349 — why "tools" is STILL absent even though the plumbing now works.
// The previous reason given here ("ChatCompletionRequest has no Tools field")
// is now STALE: this provider DOES forward tools/tool_choice and DOES parse
// tool_calls back. It is deliberately NOT advertised because the capability
// is not end-to-end functional against the serving layer measured on
// 2026-09-08:
//
//   - Tools genuinely REACH the model. Same message ± a tool schema, measured
//     directly against the local OpenAI-compatible backend:
//     prompt_tokens 40 -> 182 (one small tool) and 42 -> 3229 (12 tools).
//     A real, large, repeatable delta — the schema is in the prompt.
//   - The model REASONS about them correctly (it named the right function
//     with the right arguments).
//   - But the response NEVER carries a structured tool_calls array:
//     tool_calls was null and finish_reason was "stop"/"length" — never
//     "tool_calls" — across four attempts (fenced-JSON, fenced-XML, bare
//     JSON, and tool_choice:"required"). The call comes back as prose.
//
// An OpenAI client (Claude Code, OpenCode, …) therefore still cannot execute
// a tool against this backend. Advertising "tools" would convert an honest
// gap into a false capability claim — the exact §11.4 bluff class this
// provider's capability reporting exists to prevent. UNCONFIRMED (§11.4.6):
// whether the missing extraction is llama.cpp's tool-call parser or the
// model's non-conforming output format — the server does run with --jinja,
// but the model emitted no <tool_call> markers in any probe, so the two
// causes were not separable. Re-evaluate (and only then flip this flag) when
// a serving model returns a real structured tool_calls array.
func (p *Provider) GetCapabilities() *models.ProviderCapabilities {
	return &models.ProviderCapabilities{
		SupportedModels:       p.servedModels(),
		SupportedFeatures:     []string{"streaming"},
		SupportedRequestTypes: []string{"chat", "completion"},
		SupportsStreaming:     true,
		SupportsVision:        false,
		Limits: models.ModelLimits{
			MaxTokens:             8192,
			MaxInputLength:        4096,
			MaxOutputLength:       4096,
			MaxConcurrentRequests: 100,
		},
		Metadata: map[string]string{
			"provider_name": "helixllm",
			"default_model": p.model,
		},
		SupportsSearch: false,
	}
}

// ValidateConfig implements the LLMProvider interface
func (p *Provider) ValidateConfig(config map[string]interface{}) (bool, []string) {
	var errors []string

	if endpoint, ok := config["endpoint"].(string); ok && endpoint == "" {
		errors = append(errors, "endpoint cannot be empty")
	}

	return len(errors) == 0, errors
}

// initialize performs lazy initialization of the provider
func (p *Provider) initialize() error {
	p.initOnce.Do(func() {
		p.initErr = p.doInitialize()
		p.initialized = true
	})
	return p.initErr
}

func (p *Provider) doInitialize() error {
	if p.endpoint == "" {
		return fmt.Errorf("helixllm endpoint not configured")
	}
	return nil
}

// Ensure Provider implements LLMProvider interface
var _ llm.LLMProvider = (*Provider)(nil)
