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

// A body that cannot be rewound (GetBody nil — an io.Pipe, say) must be
// REFUSED when retries are enabled, not retried blind: a blind retry would send
// ContentLength=N with an empty body and report success.
func TestRetryableHTTPClientRefusesNonReplayableBody(t *testing.T) {
	cfg := llm.DefaultRetryConfig()
	cfg.MaxRetries = 3
	cfg.InitialDelay = time.Millisecond

	client := llm.NewRetryableHTTPClient(nil, cfg)

	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:1/never-dialed", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.GetBody = nil // simulate a streamed/non-replayable body

	if _, err := client.Do(context.Background(), req); err == nil {
		t.Fatal("Do with a non-replayable body and retries enabled: want a refusal, got nil")
	} else if !strings.Contains(err.Error(), "cannot be replayed") {
		t.Fatalf("refusal error = %v, want it to name the non-replayable body", err)
	}
}

// recordingTransport is a custom RoundTripper, which means net/http's OWN body
// rewind (transport.rewindBody, used by the default Transport) never runs. That
// makes the WRAPPER's GetBody rewind load-bearing: remove it and the second
// attempt receives an empty body, which this test catches. The httptest-based
// test above cannot catch that, because on its path the stdlib rewinds for us.
type recordingTransport struct {
	mu       sync.Mutex
	bodies   []string
	attempts int32
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	got, _ := io.ReadAll(r.Body)
	rt.mu.Lock()
	rt.bodies = append(rt.bodies, string(got))
	rt.mu.Unlock()

	status := http.StatusOK
	statusText := "200 OK"
	payload := `{"ok":true}`
	if atomic.AddInt32(&rt.attempts, 1) == 1 {
		status = http.StatusServiceUnavailable
		statusText = "503 Service Unavailable"
		payload = ""
	}
	return &http.Response{
		StatusCode: status,
		Status:     statusText,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(payload)),
		Request:    r,
	}, nil
}

func TestRetryableHTTPClientRewindIsLoadBearing(t *testing.T) {
	const body = `{"model":"m","messages":[]}`

	rt := &recordingTransport{}
	cfg := llm.DefaultRetryConfig()
	cfg.MaxRetries = 1
	cfg.InitialDelay = time.Millisecond
	cfg.MaxDelay = 2 * time.Millisecond
	cfg.JitterFactor = 0

	client := llm.NewRetryableHTTPClient(&http.Client{Transport: rt}, cfg)

	req, err := http.NewRequest(http.MethodPost, "http://example.invalid/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := client.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.bodies) < 2 {
		t.Fatalf("transport saw %d attempt(s), want a retry after the 503", len(rt.bodies))
	}
	for i, got := range rt.bodies {
		if got != body {
			t.Errorf("attempt %d sent body %q, want the ORIGINAL body replayed (wrapper rewind is load-bearing)", i+1, got)
		}
	}
}

// F1 regression: with retries disabled, a non-replayable body must still make
// its single attempt. An earlier revision guarded the refusal on MaxRetries>0
// but called req.GetBody() unconditionally, panicking on a nil GetBody here.
func TestRetryableHTTPClientZeroRetriesNonReplayableBodyDoesNotPanic(t *testing.T) {
	cfg := llm.DefaultRetryConfig()
	cfg.MaxRetries = 0

	rt := &recordingTransport{}
	client := llm.NewRetryableHTTPClient(&http.Client{Transport: rt}, cfg)

	req, err := http.NewRequest(http.MethodPost, "http://example.invalid/v1/chat/completions", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.GetBody = nil // simulate a non-replayable body

	// Must not panic. The single attempt is made; the 503 is surfaced as an error.
	if _, err := client.Do(context.Background(), req); err == nil {
		t.Fatal("want the single attempt's 503 surfaced as an error, got nil")
	}
	if got := atomic.LoadInt32(&rt.attempts); got != 1 {
		t.Fatalf("attempts = %d, want exactly 1 when retries are disabled", got)
	}
}
