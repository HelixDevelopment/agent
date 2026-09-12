package helixllm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"dev.helix.agent/internal/llm"
	"dev.helix.agent/internal/models"
)

// T048 (spec 006 WS-B): route resilience for helixagent-llm / helixagent-debate.
//
// The provider must RETRY a transient server error on the non-streaming path,
// and must NOT retry the streaming path (replaying a partially-consumed stream
// would duplicate tokens rather than recover the request).

func fastRetryConfig() llm.RetryConfig {
	c := llm.DefaultRetryConfig()
	c.MaxRetries = 2
	c.InitialDelay = time.Millisecond
	c.MaxDelay = 2 * time.Millisecond
	c.JitterFactor = 0
	return c
}

func chatRequest() *models.LLMRequest {
	return &models.LLMRequest{Messages: []models.Message{{Role: "user", Content: "hi"}}}
}

func TestCompleteRetriesTransientServerError(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	p := NewProvider(Config{Endpoint: srv.URL, Model: "m", Timeout: 5 * time.Second})
	p.retryClient = llm.NewRetryableHTTPClient(p.httpClient, fastRetryConfig())

	resp, err := p.Complete(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("Complete after a transient 503: %v", err)
	}
	if resp == nil {
		t.Fatal("Complete returned a nil response and no error")
	}
	if got := atomic.LoadInt32(&attempts); got < 2 {
		t.Fatalf("server saw %d attempt(s), want >= 2: a transient 503 was not retried", got)
	}
}

func TestCompleteStreamDoesNotRetry(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := NewProvider(Config{Endpoint: srv.URL, Model: "m", Timeout: 5 * time.Second})
	// Even with retry configured, the streaming path must attempt exactly once:
	// retrying a stream is not a recovery, it is a duplicate generation.
	p.retryClient = llm.NewRetryableHTTPClient(p.httpClient, fastRetryConfig())

	_, err := p.CompleteStream(context.Background(), chatRequest())
	if err == nil {
		t.Fatal("CompleteStream on a 503: want error, got nil")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("streaming made %d attempt(s), want exactly 1", got)
	}
}
