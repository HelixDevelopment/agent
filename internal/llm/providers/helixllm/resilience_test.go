package helixllm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dev.helix.agent/internal/llm"
)

// WS-B resilience suite (spec 006): stress, chaos, concurrency, race, memory.
// All of these drive the REAL provider against a REAL httptest HTTP server —
// no mocked transport — so the code path under test is the shipped one.

const okChatResponse = `{"id":"x","object":"chat.completion","model":"m",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`

func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okChatResponse))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fastProvider(t *testing.T, endpoint string) *Provider {
	t.Helper()
	p := NewProvider(Config{Endpoint: endpoint, Model: "m", Timeout: 5 * time.Second})
	p.retryClient = llm.NewRetryableHTTPClient(p.httpClient, fastRetryConfig())
	return p
}

// T051 — stress: sustained sequential load, every request succeeds, latency
// recorded. The assertion is on SUCCESS (deterministic); p95 is reported so a
// regression is visible without making the suite machine-speed dependent.
func TestCompleteStressSustainedLoad(t *testing.T) {
	srv := okServer(t)
	p := fastProvider(t, srv.URL)

	const n = 200
	latencies := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		resp, err := p.Complete(context.Background(), chatRequest())
		if err != nil {
			t.Fatalf("request %d/%d failed: %v", i+1, n, err)
		}
		if resp == nil {
			t.Fatalf("request %d/%d returned nil response", i+1, n)
		}
		latencies = append(latencies, time.Since(start))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("stress: %d/%d succeeded; p50=%v p95=%v max=%v",
		n, n, latencies[n/2], latencies[(n*95)/100], latencies[n-1])
}

// T052 — chaos: the server accepts then abruptly drops the connection. The
// provider must fail CLEANLY (an error, no panic) after its bounded retries.
func TestCompleteChaosConnectionDrop(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close() // drop without a response
	}))
	defer srv.Close()

	p := fastProvider(t, srv.URL)
	_, err := p.Complete(context.Background(), chatRequest())
	if err == nil {
		t.Fatal("Complete against a dropped connection: want error, got nil")
	}
	if got := atomic.LoadInt32(&attempts); got < 2 {
		t.Fatalf("dropped connection was not retried: %d attempt(s)", got)
	}
}

// T053/T054 — concurrency + race: many simultaneous requests must all complete
// with no data race (this test is meaningful under `go test -race`).
func TestCompleteConcurrentRequests(t *testing.T) {
	srv := okServer(t)
	p := fastProvider(t, srv.URL)

	const workers = 32
	const perWorker = 5

	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				resp, err := p.Complete(context.Background(), chatRequest())
				if err != nil {
					errs <- fmt.Errorf("worker request: %w", err)
					return
				}
				if resp == nil {
					errs <- fmt.Errorf("worker got nil response")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Complete: %v", err)
	}
}

// T055 — memory: repeated requests must not leak goroutines.
func TestCompleteNoGoroutineLeak(t *testing.T) {
	srv := okServer(t)
	p := fastProvider(t, srv.URL)

	// Warm up so one-time setup goroutines are already accounted for.
	for i := 0; i < 5; i++ {
		if _, err := p.Complete(context.Background(), chatRequest()); err != nil {
			t.Fatalf("warmup: %v", err)
		}
	}
	settle()
	before := runtime.NumGoroutine()

	for i := 0; i < 100; i++ {
		if _, err := p.Complete(context.Background(), chatRequest()); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	settle()
	after := runtime.NumGoroutine()

	// Allow a small slack for runtime/transport bookkeeping; a real leak of one
	// goroutine per request would be ~100 here.
	if after > before+10 {
		t.Fatalf("goroutines grew from %d to %d over 100 requests", before, after)
	}
}

func settle() {
	for i := 0; i < 20; i++ {
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
	}
}
