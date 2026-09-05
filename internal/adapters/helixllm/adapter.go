// Package helixllm provides integration adapter for HelixLLM submodule
// using the Containers module for container orchestration.
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
	"time"

	containeradapter "dev.helix.agent/internal/adapters/containers"
	"digital.vasic.containers/pkg/compose"
)

const (
	defaultEndpoint     = "https://localhost:8443"
	defaultTimeout      = 30 * time.Second
	healthCheckInterval = 10 * time.Second
	composeFileName     = "docker-compose.helixllm.yml"
)

// Adapter provides integration with HelixLLM submodule
type Adapter struct {
	endpoint      string
	apiKey        string
	httpClient    *http.Client
	enabled       bool
	tlsSkipVerify bool
	containers    *containeradapter.Adapter
}

// Config holds HelixLLM adapter configuration
type Config struct {
	Endpoint      string
	APIKey        string
	Enabled       bool
	TLSSkipVerify bool
	Timeout       time.Duration
	Containers    *containeradapter.Adapter
}

// NewAdapter creates a new HelixLLM adapter
func NewAdapter(cfg Config) (*Adapter, error) {
	if cfg.Endpoint == "" {
		cfg.Endpoint = getEnv("HELIX_LLM_ENDPOINT", defaultEndpoint)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	// HA-F2-003: Enabled is caller-stated intent — there is deliberately NO
	// env fallback here. A zero Config means "not requested", not "enable
	// because USE_HELIX_LLM is unset". The local-first default
	// (services.HelixLLMEnabledDefault) is applied by the wiring layer that
	// decides whether to construct this adapter at all (router.go passes
	// Enabled: true explicitly); an adapter that exists is one the wiring
	// decided to create.

	// Secure-by-default: match the behaviour already implemented in
	// internal/llm/providers/helixllm/provider.go and documented in
	// CLAUDE.md. The previous `true` default contradicted the spec and
	// caused G402 to fire on the baseline scan.
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.TLSSkipVerify || getEnvBool("HELIX_LLM_TLS_SKIP_VERIFY", false), //nolint:gosec // G402: opt-in via config or env; default is secure
		},
	}

	return &Adapter{
		endpoint:      cfg.Endpoint,
		apiKey:        cfg.APIKey,
		enabled:       cfg.Enabled,
		tlsSkipVerify: cfg.TLSSkipVerify,
		httpClient: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: transport,
		},
		containers: cfg.Containers,
	}, nil
}

// Start starts HelixLLM services using the Containers module
func (a *Adapter) Start(ctx context.Context) error {
	if !a.enabled {
		return nil
	}

	if a.containers == nil {
		return fmt.Errorf("containers adapter not configured")
	}

	return a.containers.ComposeUp(ctx, composeFileName, "")
}

// Stop stops HelixLLM services using the Containers module
func (a *Adapter) Stop(ctx context.Context) error {
	if !a.enabled {
		return nil
	}

	if a.containers == nil {
		return fmt.Errorf("containers adapter not configured")
	}

	return a.containers.ComposeDown(ctx, composeFileName, "")
}

// Status returns the status of HelixLLM services
func (a *Adapter) Status(ctx context.Context) ([]compose.ServiceStatus, error) {
	if !a.enabled {
		return nil, fmt.Errorf("helixllm integration is disabled")
	}

	if a.containers == nil {
		return nil, fmt.Errorf("containers adapter not configured")
	}

	return a.containers.ComposeStatus(ctx, composeFileName)
}

// IsEnabled returns whether HelixLLM integration is enabled
func (a *Adapter) IsEnabled() bool {
	return a.enabled
}

// Health checks if HelixLLM is healthy
func (a *Adapter) Health(ctx context.Context) (*HealthResponse, error) {
	if !a.enabled {
		return nil, fmt.Errorf("helixllm integration is disabled")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", a.endpoint+"/internal/health", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create health request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health check returned status %d", resp.StatusCode)
	}

	var health HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return nil, fmt.Errorf("failed to decode health response: %w", err)
	}

	return &health, nil
}

// ChatCompletion sends a chat completion request to HelixLLM
func (a *Adapter) ChatCompletion(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	if !a.enabled {
		return nil, fmt.Errorf("helixllm integration is disabled")
	}

	endpoint := fmt.Sprintf("%s/v1/chat/completions", a.endpoint)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if a.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("request returned status %d: %s", resp.StatusCode, string(body))
	}

	var result ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// Embed sends an embedding request to HelixLLM
func (a *Adapter) Embed(ctx context.Context, req *EmbeddingRequest) (*EmbeddingResponse, error) {
	if !a.enabled {
		return nil, fmt.Errorf("helixllm integration is disabled")
	}

	endpoint := fmt.Sprintf("%s/v1/embeddings", a.endpoint)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if a.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("request returned status %d: %s", resp.StatusCode, string(body))
	}

	var result EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// KnowledgeIngest ingests documents into HelixLLM knowledge base
func (a *Adapter) KnowledgeIngest(ctx context.Context, req *KnowledgeIngestRequest) (*KnowledgeIngestResponse, error) {
	if !a.enabled {
		return nil, fmt.Errorf("helixllm integration is disabled")
	}

	endpoint := fmt.Sprintf("%s/internal/knowledge/ingest", a.endpoint)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if a.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("request returned status %d: %s", resp.StatusCode, string(body))
	}

	var result KnowledgeIngestResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// KnowledgeQuery queries HelixLLM knowledge base
func (a *Adapter) KnowledgeQuery(ctx context.Context, req *KnowledgeQueryRequest) (*KnowledgeQueryResponse, error) {
	if !a.enabled {
		return nil, fmt.Errorf("helixllm integration is disabled")
	}

	endpoint := fmt.Sprintf("%s/internal/knowledge/query", a.endpoint)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if a.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("request returned status %d: %s", resp.StatusCode, string(body))
	}

	var result KnowledgeQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// AgentChat sends an agent chat request to HelixLLM
func (a *Adapter) AgentChat(ctx context.Context, req *AgentChatRequest) (*AgentChatResponse, error) {
	if !a.enabled {
		return nil, fmt.Errorf("helixllm integration is disabled")
	}

	endpoint := fmt.Sprintf("%s/v1/agents/chat", a.endpoint)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if a.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("request returned status %d: %s", resp.StatusCode, string(body))
	}

	var result AgentChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// GetModels retrieves available models from HelixLLM
func (a *Adapter) GetModels(ctx context.Context) (*ModelsResponse, error) {
	if !a.enabled {
		return nil, fmt.Errorf("helixllm integration is disabled")
	}

	endpoint := fmt.Sprintf("%s/v1/models", a.endpoint)

	httpReq, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Accept", "application/json")
	if a.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("request returned status %d: %s", resp.StatusCode, string(body))
	}

	var result ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// Helper functions
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		return value == "true" || value == "1" || value == "yes"
	}
	return defaultValue
}
