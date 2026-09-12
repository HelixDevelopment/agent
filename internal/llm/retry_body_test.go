package llm_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dev.helix.agent/internal/llm"
)

// T048 (spec 006 WS-B): route resilience. Retrying a POST is only correct if
// the BODY is replayed on every attempt. `http.Request.Clone` copies the Body
// READER, not its contents, so without an explicit rewind a retried POST goes
// out with an empty body — the server sees a different, smaller request and the
// caller is told the retry "succeeded". That is the silent-corruption case this
// test pins.
func TestRetryableHTTPClientReplaysBodyOnRetry(t *testing.T) {
	const body = `{"model":"m","messages":[{"role":"user","content":"hello"}]}`

	var (
		mu       sync.Mutex
		seen     []string
		attempts int32
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, string(got))
		mu.Unlock()

		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg := llm.DefaultRetryConfig()
	cfg.MaxRetries = 2
	cfg.InitialDelay = time.Millisecond
	cfg.MaxDelay = 2 * time.Millisecond
	cfg.JitterFactor = 0

	client := llm.NewRetryableHTTPClient(srv.Client(), cfg)

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test

	if got := atomic.LoadInt32(&attempts); got < 2 {
		t.Fatalf("server saw %d attempt(s), want a retry after the 503", got)
	}

	mu.Lock()
	defer mu.Unlock()
	for i, got := range seen {
		if got != body {
			t.Errorf("attempt %d sent body %q, want the ORIGINAL body replayed on every attempt", i+1, got)
		}
	}
}
