package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"

	"dev.helix.agent/internal/llm/discovery"
	"dev.helix.agent/internal/models"
)

// RetryConfig defines retry behavior for API calls
type RetryConfig struct {
	MaxRetries   int
	InitialDelay time.Duration
	MaxDelay     time.Duration
	Multiplier   float64
}

// DefaultRetryConfig returns sensible defaults for Ollama API retry behavior
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:   3,
		InitialDelay: 1 * time.Second,
		MaxDelay:     30 * time.Second,
		Multiplier:   2.0,
	}
}

// OllamaProvider implements the LLMProvider interface for local Ollama models
type OllamaProvider struct {
	baseURL     string
	model       string
	httpClient  *http.Client
	retryConfig RetryConfig
	discoverer  *discovery.Discoverer
}

// OllamaRequest represents a request to the Ollama API
type OllamaRequest struct {
	Model   string        `json:"model"`
	Prompt  string        `json:"prompt,omitempty"`
	Stream  bool          `json:"stream,omitempty"`
	Options OllamaOptions `json:"options,omitempty"`
}

// OllamaOptions represents options for Ollama generation
type OllamaOptions struct {
	Temperature float64  `json:"temperature,omitempty"`
	TopP        float64  `json:"top_p,omitempty"`
	MaxTokens   int      `json:"num_predict,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

// OllamaResponse represents a response from the Ollama API
type OllamaResponse struct {
	Model    string `json:"model"`
	Response string `json:"response"`
	Done     bool   `json:"done"`
	Context  []int  `json:"context,omitempty"`
	// Real token counts from Ollama's final (done) response object.
	// Ollama names the input side prompt_eval_count and the generated
	// side eval_count — verified against the current official API
	// reference (github.com/ollama/ollama docs/api.md, POST
	// /api/generate, accessed 2026-09-03). These were previously not
	// parsed at all, which is why the response fell back to estimating
	// token usage from the reply's character length.
	PromptEvalCount int `json:"prompt_eval_count,omitempty"`
	EvalCount       int `json:"eval_count,omitempty"`
}

// NewOllamaProvider creates a new Ollama provider instance
func NewOllamaProvider(baseURL, model string) *OllamaProvider {
	return NewOllamaProviderWithRetry(baseURL, model, DefaultRetryConfig())
}

// NewOllamaProviderWithRetry creates a new Ollama provider instance with custom retry config
func NewOllamaProviderWithRetry(baseURL, model string, retryConfig RetryConfig) *OllamaProvider {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "llama2"
	}

	p := &OllamaProvider{
		baseURL: baseURL,
		model:   model,
		httpClient: &http.Client{
			Timeout: 120 * time.Second, // Ollama can be slow for first requests
		},
		retryConfig: retryConfig,
	}

	p.discoverer = discovery.NewDiscoverer(discovery.ProviderConfig{
		ProviderName:   "ollama",
		ModelsEndpoint: baseURL + "/api/tags",
		ModelsDevID:    "ollama",
		APIKey:         "local",
		ResponseParser: discovery.ParseOllamaModelsResponse,
		// FallbackModels is a DEGRADATION PATH, not a model catalogue, and is
		// recorded here as a PERMITTED exception to CONST-036 (operator
		// decision 2026-09-13). CONST-036 forbids hardcoded model lists because
		// they silently override the single source of truth; this list is used
		// ONLY when live discovery fails (see discovery.NewDiscoverer and the
		// "source=fallback" log line), so it cannot override a successful
		// discovery — the discovered set always wins. A host whose Ollama is
		// unreachable still needs SOME way to describe the provider, and an
		// empty list would make the provider unusable rather than honest.
		// TestOllamaFallbackOnlyOnDiscoveryFailure pins that ordering.
		FallbackModels: []string{
			"llama2",
			"llama2:13b",
			"llama2:70b",
			"codellama",
			"mistral",
			"vicuna",
			"orca-mini",
		},
	})

	return p
}

// Complete implements the LLMProvider interface
func (o *OllamaProvider) Complete(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error) {
	ollamaReq := OllamaRequest{
		Model:  o.model,
		Prompt: req.Prompt,
		Stream: false,
		Options: OllamaOptions{
			Temperature: req.ModelParams.Temperature,
			TopP:        req.ModelParams.TopP,
			MaxTokens:   req.ModelParams.MaxTokens,
			Stop:        req.ModelParams.StopSequences,
		},
	}

	resp, err := o.makeRequest(ctx, ollamaReq)
	if err != nil {
		return nil, fmt.Errorf("failed to complete request: %w", err)
	}

	return o.convertResponse(resp, req.ID)
}

// CompleteStream implements streaming completion
func (o *OllamaProvider) CompleteStream(ctx context.Context, req *models.LLMRequest) (<-chan *models.LLMResponse, error) {
	ch := make(chan *models.LLMResponse)

	go func() {
		defer close(ch)

		ollamaReq := OllamaRequest{
			Model:  o.model,
			Prompt: req.Prompt,
			Stream: true,
			Options: OllamaOptions{
				Temperature: req.ModelParams.Temperature,
				TopP:        req.ModelParams.TopP,
				MaxTokens:   req.ModelParams.MaxTokens,
				Stop:        req.ModelParams.StopSequences,
			},
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", o.baseURL+"/api/generate", nil)
		if err != nil {
			select {
			case ch <- &models.LLMResponse{
				RequestID:    req.ID,
				ProviderID:   "ollama",
				ProviderName: "Ollama",
				Content:      fmt.Sprintf("Error: %v", err),
				Confidence:   0.0,
				FinishReason: "error",
				CreatedAt:    time.Now(),
			}:
			case <-ctx.Done():
			}
			return
		}

		jsonData, err := json.Marshal(ollamaReq)
		if err != nil {
			select {
			case ch <- &models.LLMResponse{
				RequestID:    req.ID,
				ProviderID:   "ollama",
				ProviderName: "Ollama",
				Content:      fmt.Sprintf("Error: %v", err),
				Confidence:   0.0,
				FinishReason: "error",
				CreatedAt:    time.Now(),
			}:
			case <-ctx.Done():
			}
			return
		}

		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Body = io.NopCloser(bytes.NewBuffer(jsonData))

		response, err := o.httpClient.Do(httpReq)
		if err != nil {
			select {
			case ch <- &models.LLMResponse{
				RequestID:    req.ID,
				ProviderID:   "ollama",
				ProviderName: "Ollama",
				Content:      fmt.Sprintf("Error: %v", err),
				Confidence:   0.0,
				FinishReason: "error",
				CreatedAt:    time.Now(),
			}:
			case <-ctx.Done():
			}
			return
		}
		defer func() { _ = response.Body.Close() }()

		decoder := json.NewDecoder(response.Body)
		fullContent := ""

		for {
			var streamResp OllamaResponse
			if err := decoder.Decode(&streamResp); err != nil {
				if err == io.EOF {
					break
				}
				// Send error response and exit on decode error
				select {
				case ch <- &models.LLMResponse{
					RequestID:    req.ID,
					ProviderID:   "ollama",
					ProviderName: "Ollama",
					Content:      fmt.Sprintf("Error: failed to decode response: %v", err),
					Confidence:   0.0,
					FinishReason: "error",
					CreatedAt:    time.Now(),
				}:
				case <-ctx.Done():
				}
				return
			}

			fullContent += streamResp.Response

			chunkResp := &models.LLMResponse{
				RequestID:      req.ID,
				ProviderID:     "ollama",
				ProviderName:   "Ollama",
				Content:        streamResp.Response,
				Confidence:     0.8,
				TokensUsed:     1,
				ResponseTime:   time.Now().UnixMilli(),
				FinishReason:   "",
				Selected:       false,
				SelectionScore: 0.0,
				CreatedAt:      time.Now(),
			}

			select {
			case ch <- chunkResp:
			case <-ctx.Done():
				return
			}

			if streamResp.Done {
				// The done-chunk carries Ollama's real token counts;
				// prefer them over the character-length estimate, and
				// record the split under the canonical keys so a
				// consumer of this final chunk sees measured numbers
				// rather than a guess. Falls back to the estimate only
				// when Ollama reported no counts.
				finalTokens := len(fullContent) / 4
				var finalMeta map[string]interface{}
				if streamResp.PromptEvalCount > 0 || streamResp.EvalCount > 0 {
					finalTokens = streamResp.PromptEvalCount + streamResp.EvalCount
					finalMeta = map[string]interface{}{
						"prompt_tokens":     streamResp.PromptEvalCount,
						"completion_tokens": streamResp.EvalCount,
						"total_tokens":      finalTokens,
					}
				}

				// Send final response
				finalResp := &models.LLMResponse{
					RequestID:      req.ID,
					ProviderID:     "ollama",
					ProviderName:   "Ollama",
					Content:        "",
					Confidence:     0.8,
					TokensUsed:     finalTokens,
					ResponseTime:   time.Now().UnixMilli(),
					FinishReason:   "stop",
					Metadata:       finalMeta,
					Selected:       false,
					SelectionScore: 0.0,
					CreatedAt:      time.Now(),
				}
				select {
				case ch <- finalResp:
				case <-ctx.Done():
					return
				}
				break
			}
		}
	}()

	return ch, nil
}

// HealthCheck implements health checking for the Ollama provider
func (o *OllamaProvider) HealthCheck() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", o.baseURL+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("failed to create health check request: %w", err)
	}

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("health check request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed with status: %d", resp.StatusCode)
	}

	return nil
}

// GetCapabilities returns the capabilities of the Ollama provider
func (o *OllamaProvider) GetCapabilities() *models.ProviderCapabilities {
	return &models.ProviderCapabilities{
		SupportedModels: o.discoverer.DiscoverModels(),
		SupportedFeatures: []string{
			"text_completion",
			"chat",
			"streaming",
		},
		SupportedRequestTypes: []string{
			"text_completion",
			"chat",
		},
		SupportsStreaming:       true,
		SupportsFunctionCalling: false,
		SupportsVision:          false,
		Limits: models.ModelLimits{
			MaxTokens:             4096,
			MaxInputLength:        4096,
			MaxOutputLength:       4096,
			MaxConcurrentRequests: 1, // Ollama typically handles one request at a time
		},
		Metadata: map[string]string{
			"provider":     "Ollama",
			"model_family": "Local Models",
			"api_version":  "v1",
			"local":        "true",
		},
	}
}

// ValidateConfig validates the provider configuration
func (o *OllamaProvider) ValidateConfig(config map[string]interface{}) (bool, []string) {
	var errors []string

	if o.baseURL == "" {
		errors = append(errors, "base URL is required")
	}

	if o.model == "" {
		errors = append(errors, "model is required")
	}

	return len(errors) == 0, errors
}

// convertResponse converts Ollama API response to internal format
func (o *OllamaProvider) convertResponse(resp *OllamaResponse, requestID string) (*models.LLMResponse, error) {
	metadata := map[string]interface{}{
		"model":   resp.Model,
		"context": len(resp.Context),
	}

	// Prefer Ollama's REAL token counts over a character-length guess.
	// TokensUsed previously held len(response)/4 — an estimate of the
	// OUTPUT side only, presented as a total. Now it is the true total
	// when Ollama reported the counts, and the per-direction split is
	// recorded under the canonical keys models.LLMResponse.TokenSplit
	// reads, so the OpenAI-compatible `usage` envelope carries measured
	// numbers. When Ollama reports nothing (older versions, or a
	// non-final chunk) the estimate remains as the only figure
	// available and no split is claimed.
	tokensUsed := len(resp.Response) / 4
	if resp.PromptEvalCount > 0 || resp.EvalCount > 0 {
		tokensUsed = resp.PromptEvalCount + resp.EvalCount
		metadata["prompt_tokens"] = resp.PromptEvalCount
		metadata["completion_tokens"] = resp.EvalCount
		metadata["total_tokens"] = tokensUsed
	}

	return &models.LLMResponse{
		ID:             fmt.Sprintf("ollama-%d", time.Now().Unix()),
		RequestID:      requestID,
		ProviderID:     "ollama",
		ProviderName:   "Ollama",
		Content:        resp.Response,
		Confidence:     0.8, // Ollama doesn't provide confidence scores
		TokensUsed:     tokensUsed,
		ResponseTime:   time.Now().UnixMilli(),
		FinishReason:   "stop",
		Metadata:       metadata,
		Selected:       false,
		SelectionScore: 0.0,
		CreatedAt:      time.Now(),
	}, nil
}

// makeRequest sends a request to the Ollama API with retry logic
func (o *OllamaProvider) makeRequest(ctx context.Context, req OllamaRequest) (*OllamaResponse, error) {
	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	var lastErr error
	delay := o.retryConfig.InitialDelay

	for attempt := 0; attempt <= o.retryConfig.MaxRetries; attempt++ {
		// Check context before making request
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled: %w", ctx.Err())
		default:
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", o.baseURL+"/api/generate", bytes.NewBuffer(jsonData))
		if err != nil {
			return nil, fmt.Errorf("failed to create HTTP request: %w", err)
		}

		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := o.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("HTTP request failed: %w", err)
			if attempt < o.retryConfig.MaxRetries {
				o.waitWithJitter(ctx, delay)
				delay = o.nextDelay(delay)
				continue
			}
			return nil, lastErr
		}

		// Check for retryable status codes
		if isRetryableStatus(resp.StatusCode) && attempt < o.retryConfig.MaxRetries {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d: retryable error", resp.StatusCode)
			o.waitWithJitter(ctx, delay)
			delay = o.nextDelay(delay)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read response body: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Ollama API returned status %d: %s", resp.StatusCode, string(body))
		}

		var ollamaResp OllamaResponse
		if err := json.Unmarshal(body, &ollamaResp); err != nil {
			return nil, fmt.Errorf("failed to unmarshal response: %w", err)
		}

		return &ollamaResp, nil
	}

	return nil, fmt.Errorf("all %d retry attempts failed: %w", o.retryConfig.MaxRetries+1, lastErr)
}

// isRetryableStatus returns true for HTTP status codes that warrant a retry
func isRetryableStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusTooManyRequests, // 429 - Rate limited
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	default:
		return false
	}
}

// waitWithJitter waits for the specified duration plus random jitter
func (o *OllamaProvider) waitWithJitter(ctx context.Context, delay time.Duration) {
	// Add 10% jitter - using math/rand is acceptable for non-security jitter
	jitter := time.Duration(rand.Float64() * 0.1 * float64(delay)) // #nosec G404 - jitter doesn't require cryptographic randomness
	select {
	case <-ctx.Done():
	case <-time.After(delay + jitter):
	}
}

// nextDelay calculates the next delay using exponential backoff
func (o *OllamaProvider) nextDelay(currentDelay time.Duration) time.Duration {
	nextDelay := time.Duration(float64(currentDelay) * o.retryConfig.Multiplier)
	if nextDelay > o.retryConfig.MaxDelay {
		nextDelay = o.retryConfig.MaxDelay
	}
	return nextDelay
}
