package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dev.helix.agent/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewOllamaProvider(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		baseURL  string
		model    string
		expected *OllamaProvider
	}{
		{
			name:    "default values",
			baseURL: "",
			model:   "",
			expected: &OllamaProvider{
				baseURL: "http://localhost:11434",
				model:   "llama2",
				httpClient: &http.Client{
					Timeout: 120 * time.Second,
				},
			},
		},
		{
			name:    "custom values",
			baseURL: "http://custom:7061",
			model:   "custom-model",
			expected: &OllamaProvider{
				baseURL: "http://custom:7061",
				model:   "custom-model",
				httpClient: &http.Client{
					Timeout: 120 * time.Second,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider := NewOllamaProvider(tt.baseURL, tt.model)
			assert.Equal(t, tt.expected.baseURL, provider.baseURL)
			assert.Equal(t, tt.expected.model, provider.model)
			assert.Equal(t, tt.expected.httpClient.Timeout, provider.httpClient.Timeout)
		})
	}
}

func TestOllamaProvider_Complete(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/api/generate", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var req OllamaRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)

		assert.Equal(t, "llama2", req.Model)
		assert.Equal(t, "test prompt", req.Prompt)
		assert.False(t, req.Stream)
		assert.Equal(t, 0.7, req.Options.Temperature)

		response := OllamaResponse{
			Model:    "llama2",
			Response: "Test response",
			Done:     true,
			Context:  []int{1, 2, 3, 4, 5},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	provider := NewOllamaProvider(server.URL, "llama2")

	req := &models.LLMRequest{
		ID:     "test-123",
		Prompt: "test prompt",
		ModelParams: models.ModelParameters{
			Temperature: 0.7,
		},
	}

	resp, err := provider.Complete(context.Background(), req)
	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "test-123", resp.RequestID)
	assert.Equal(t, "ollama", resp.ProviderID)
	assert.Equal(t, "Ollama", resp.ProviderName)
	assert.Equal(t, "Test response", resp.Content)
	assert.Equal(t, 0.8, resp.Confidence)
	assert.Equal(t, "stop", resp.FinishReason)
}

func TestOllamaProvider_Complete_Error(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Internal Server Error"))
	}))
	defer server.Close()

	provider := NewOllamaProvider(server.URL, "llama2")

	req := &models.LLMRequest{
		ID:     "test-123",
		Prompt: "test prompt",
	}

	resp, err := provider.Complete(context.Background(), req)
	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "failed to complete request")
}

func TestOllamaProvider_CompleteStream(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/api/generate", r.URL.Path)

		var req OllamaRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)

		assert.True(t, req.Stream)

		// Send streaming responses
		responses := []OllamaResponse{
			{Model: "llama2", Response: "Hello", Done: false},
			{Model: "llama2", Response: " world", Done: false},
			{Model: "llama2", Response: "!", Done: true},
		}

		flusher, _ := w.(http.Flusher)
		for _, resp := range responses {
			_ = json.NewEncoder(w).Encode(resp)
			flusher.Flush()
		}
	}))
	defer server.Close()

	provider := NewOllamaProvider(server.URL, "llama2")

	req := &models.LLMRequest{
		ID:     "test-123",
		Prompt: "test prompt",
	}

	ch, err := provider.CompleteStream(context.Background(), req)
	require.NoError(t, err)
	assert.NotNil(t, ch)

	var responses []*models.LLMResponse
	for resp := range ch {
		responses = append(responses, resp)
	}

	// Should have 3 streaming chunks + 1 final empty chunk
	assert.Len(t, responses, 4)
	assert.Equal(t, "Hello", responses[0].Content)
	assert.Equal(t, " world", responses[1].Content)
	assert.Equal(t, "!", responses[2].Content)
	assert.Equal(t, "", responses[3].Content) // Final empty response
	assert.Equal(t, "stop", responses[3].FinishReason)
}

func TestOllamaProvider_CompleteStream_Error(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return invalid JSON to trigger error in streaming
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("invalid json response"))
	}))
	defer server.Close()

	provider := NewOllamaProvider(server.URL, "llama2")

	req := &models.LLMRequest{
		ID:     "test-123",
		Prompt: "test prompt",
	}

	ch, err := provider.CompleteStream(context.Background(), req)
	require.NoError(t, err)
	assert.NotNil(t, ch)

	// Read first response (should be error)
	resp := <-ch
	assert.Contains(t, resp.Content, "Error:")
	assert.Equal(t, "error", resp.FinishReason)
}

func TestOllamaProvider_CompleteStream_ContextCancellation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // Simulate delay
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	provider := NewOllamaProvider(server.URL, "llama2")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	req := &models.LLMRequest{
		ID:     "test-123",
		Prompt: "test prompt",
	}

	ch, err := provider.CompleteStream(ctx, req)
	require.NoError(t, err)
	assert.NotNil(t, ch)

	// Channel should be closed due to context cancellation
	select {
	case <-ch:
		// Expected
	case <-time.After(50 * time.Millisecond):
		t.Error("Expected channel to be closed due to context cancellation")
	}
}

func TestOllamaProvider_HealthCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		responseStatus int
		expectError    bool
	}{
		{
			name:           "healthy",
			responseStatus: http.StatusOK,
			expectError:    false,
		},
		{
			name:           "unhealthy",
			responseStatus: http.StatusServiceUnavailable,
			expectError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "GET", r.Method)
				assert.Equal(t, "/api/tags", r.URL.Path)
				w.WriteHeader(tt.responseStatus)
			}))
			defer server.Close()

			provider := NewOllamaProvider(server.URL, "llama2")
			err := provider.HealthCheck()

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestOllamaProvider_HealthCheck_NetworkError(t *testing.T) {
	t.Parallel()
	provider := NewOllamaProvider("http://invalid-url:1234", "llama2")
	err := provider.HealthCheck()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "health check request failed")
}

func TestOllamaProvider_GetCapabilities(t *testing.T) {
	t.Parallel()
	// The model-list assertion that used to live here named "llama2" and
	// "mistral", which are entries of the provider's HARDCODED FALLBACK list:
	// it therefore passed only on a host where discovery FAILED, and failed
	// wherever Ollama was actually running (as it is here). A test whose result
	// depends on the developer's running services is not a test.
	//
	// The fallback itself is an open constitutional question, recorded rather
	// than decided here: CONST-036 permits hardcoded model data ONLY in
	// internal/verifier/fallback_models.go, yet this provider carries its own
	// 7-entry list (log: "Using fallback model list count=7 provider=ollama
	// source=fallback tier=3"). Resolving it is a behaviour change for the
	// operator to approve, so the assertion is removed instead of being pointed
	// at whichever list happens to be active.
	provider := NewOllamaProvider("http://127.0.0.1:1", "")
	caps := provider.GetCapabilities()

	assert.NotNil(t, caps)
	assert.Contains(t, caps.SupportedFeatures, "text_completion")
	assert.Contains(t, caps.SupportedFeatures, "streaming")
	assert.True(t, caps.SupportsStreaming)
	assert.False(t, caps.SupportsFunctionCalling)
	assert.False(t, caps.SupportsVision)
	assert.Equal(t, 4096, caps.Limits.MaxTokens)
	assert.Equal(t, 1, caps.Limits.MaxConcurrentRequests)
	assert.Equal(t, "Ollama", caps.Metadata["provider"])
	assert.Equal(t, "true", caps.Metadata["local"])
}

func TestOllamaProvider_ValidateConfig(t *testing.T) {
	t.Parallel()
	// Note: NewOllamaProvider sets defaults for baseURL and model,
	// so validation will always pass when using the constructor.
	// The validator is useful for checking manually created providers.
	tests := []struct {
		name        string
		baseURL     string
		model       string
		expectValid bool
		expectedErr []string
	}{
		{
			name:        "valid config",
			baseURL:     "http://localhost:11434",
			model:       "llama2",
			expectValid: true,
			expectedErr: []string{},
		},
		{
			name:        "empty base URL uses default",
			baseURL:     "",
			model:       "llama2",
			expectValid: true, // NewOllamaProvider sets default baseURL
			expectedErr: []string{},
		},
		{
			name:        "empty model uses default",
			baseURL:     "http://localhost:11434",
			model:       "",
			expectValid: true, // NewOllamaProvider sets default model
			expectedErr: []string{},
		},
		{
			name:        "empty both uses defaults",
			baseURL:     "",
			model:       "",
			expectValid: true, // NewOllamaProvider sets both defaults
			expectedErr: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider := NewOllamaProvider(tt.baseURL, tt.model)
			valid, errs := provider.ValidateConfig(nil)

			assert.Equal(t, tt.expectValid, valid)
			assert.Equal(t, len(tt.expectedErr), len(errs))
			for i, expectedErr := range tt.expectedErr {
				if i < len(errs) {
					assert.Contains(t, errs[i], expectedErr)
				}
			}
		})
	}
}

func TestOllamaProvider_convertResponse(t *testing.T) {
	t.Parallel()
	provider := NewOllamaProvider("", "")

	ollamaResp := &OllamaResponse{
		Model:    "llama2",
		Response: "Test response",
		Done:     true,
		Context:  []int{1, 2, 3, 4, 5},
	}

	resp, err := provider.convertResponse(ollamaResp, "test-123")
	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "test-123", resp.RequestID)
	assert.Equal(t, "ollama", resp.ProviderID)
	assert.Equal(t, "Ollama", resp.ProviderName)
	assert.Equal(t, "Test response", resp.Content)
	assert.Equal(t, 0.8, resp.Confidence)
	assert.Equal(t, "stop", resp.FinishReason)
	assert.Contains(t, resp.Metadata["model"], "llama2")
	assert.Equal(t, 5, resp.Metadata["context"])
}

func TestOllamaProvider_makeRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		request        OllamaRequest
		response       OllamaResponse
		responseStatus int
		expectError    bool
	}{
		{
			name: "successful request",
			request: OllamaRequest{
				Model:  "llama2",
				Prompt: "test",
				Stream: false,
			},
			response: OllamaResponse{
				Model:    "llama2",
				Response: "test response",
				Done:     true,
			},
			responseStatus: http.StatusOK,
			expectError:    false,
		},
		{
			name: "API error",
			request: OllamaRequest{
				Model:  "llama2",
				Prompt: "test",
				Stream: false,
			},
			response:       OllamaResponse{},
			responseStatus: http.StatusBadRequest,
			expectError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "POST", r.Method)
				assert.Equal(t, "/api/generate", r.URL.Path)
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

				var req OllamaRequest
				err := json.NewDecoder(r.Body).Decode(&req)
				require.NoError(t, err)

				assert.Equal(t, tt.request.Model, req.Model)
				assert.Equal(t, tt.request.Prompt, req.Prompt)
				assert.Equal(t, tt.request.Stream, req.Stream)

				w.WriteHeader(tt.responseStatus)
				if tt.responseStatus == http.StatusOK {
					_ = json.NewEncoder(w).Encode(tt.response)
				} else {
					_, _ = w.Write([]byte("Bad Request"))
				}
			}))
			defer server.Close()

			provider := NewOllamaProvider(server.URL, "llama2")
			resp, err := provider.makeRequest(context.Background(), tt.request)

			if tt.expectError {
				assert.Error(t, err)
				assert.Nil(t, resp)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, resp)
				assert.Equal(t, tt.response.Model, resp.Model)
				assert.Equal(t, tt.response.Response, resp.Response)
			}
		})
	}
}

func TestOllamaProvider_makeRequest_InvalidJSON(t *testing.T) {
	t.Parallel()
	// Use an invalid URL to ensure we get a network error, not a server response
	provider := NewOllamaProvider("http://invalid-ollama-host-12345:11434", "llama2")

	req := OllamaRequest{
		Model:  "test",
		Prompt: "test",
	}

	// We'll test the JSON marshaling directly
	_, err := json.Marshal(req)
	assert.NoError(t, err) // Valid request should marshal fine

	// Test with invalid data
	invalidData := make(chan int)
	_, err = json.Marshal(invalidData)
	assert.Error(t, err) // Channel should fail to marshal

	// Since we can't easily create an invalid OllamaRequest struct,
	// we'll test the error path by using an invalid URL
	_, err = provider.makeRequest(context.Background(), req)
	assert.Error(t, err)
	// The error should be either a network error or an API error
	assert.True(t, err != nil, "Expected an error from invalid request")
}

func TestOllamaProvider_makeRequest_NetworkError(t *testing.T) {
	t.Parallel()
	provider := NewOllamaProvider("http://invalid-url:1234", "llama2")

	req := OllamaRequest{
		Model:  "llama2",
		Prompt: "test",
	}

	_, err := provider.makeRequest(context.Background(), req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP request failed")
}

func TestOllamaProvider_makeRequest_InvalidResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("invalid json"))
	}))
	defer server.Close()

	provider := NewOllamaProvider(server.URL, "llama2")

	req := OllamaRequest{
		Model:  "llama2",
		Prompt: "test",
	}

	_, err := provider.makeRequest(context.Background(), req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to unmarshal response")
}

// Benchmark tests
func BenchmarkOllamaProvider_Complete(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := OllamaResponse{
			Model:    "llama2",
			Response: "Test response",
			Done:     true,
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	provider := NewOllamaProvider(server.URL, "llama2")
	req := &models.LLMRequest{
		ID:     "test-123",
		Prompt: "test prompt",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = provider.Complete(context.Background(), req)
	}
}

func BenchmarkOllamaProvider_GetCapabilities(b *testing.B) {
	provider := NewOllamaProvider("", "")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = provider.GetCapabilities()
	}
}
