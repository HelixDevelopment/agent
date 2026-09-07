package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"

	"dev.helix.agent/internal/models"
)

// ---------------------------------------------------------------------------
// HA-CB-001 — circuit breaker stuck open against a HEALTHY backend.
//
// §11.4.115 RED-baseline-on-the-broken-artifact + polarity switch.
//
//	RED_MODE=1 (default) — reproduce and ASSERT THE DEFECT IS PRESENT on the
//	                       pre-fix artifact. These runs are the proof the
//	                       guard is real and not written to agree with a fix.
//	RED_MODE=0           — the standing GREEN regression guard: assert the
//	                       defect is ABSENT.
//
// One source, two roles (§11.4.146 STEP 1 -> STEP 2). The bug-catcher IS the
// regression-guard; no separate happy-path test substitutes for it.
//
// Forensic anchor (FACT, captured 2026-09-07 from journalctl --user -u
// helixagent.service): helixllm served real completions successfully from
// 13:07:41 to 13:08:57, then
//
//	13:08:56 level=error msg="Provider health alert triggered"
//	         last_error="health check returned status 404"
//	         message="Provider has failed 3 consecutive health checks"
//	         provider=helixllm
//
// and from 13:10:32 onward EVERY call returned "circuit breaker is open"
// against a backend that answers GET /v1/models with HTTP 200.
// ---------------------------------------------------------------------------

// The polarity switch itself is the package-shared redMode() helper defined in
// protocol_security_hxc221_red_test.go (RED_MODE=1 -> reproduce the defect;
// unset/0 -> the standing GREEN guard). Reused rather than redeclared so every
// §11.4.115 guard in this package flips on the same switch.

// stubProvider is a unit-test double (permitted here only — CONST-050(A)):
// it lets each of the three failure signals be driven independently so the
// test can prove WHICH signal trips the breaker.
type stubProvider struct {
	completeErr    error
	healthErr      error
	completeCalls  int
	healthCalls    int
	completeOKText string
}

func (s *stubProvider) Complete(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error) {
	s.completeCalls++
	if s.completeErr != nil {
		return nil, s.completeErr
	}
	return &models.LLMResponse{Content: s.completeOKText, TokensUsed: 7}, nil
}

func (s *stubProvider) CompleteStream(ctx context.Context, req *models.LLMRequest) (<-chan *models.LLMResponse, error) {
	ch := make(chan *models.LLMResponse)
	close(ch)
	return ch, nil
}

func (s *stubProvider) HealthCheck() error {
	s.healthCalls++
	return s.healthErr
}

func (s *stubProvider) GetCapabilities() *models.ProviderCapabilities {
	return &models.ProviderCapabilities{}
}

func (s *stubProvider) ValidateConfig(map[string]interface{}) (bool, []string) {
	return true, nil
}

// TestHACB001_ScatteredFailuresMustNotTripBreaker proves defect D2: onSuccess()
// never resets consecutiveFailures while the breaker is CLOSED, so the field
// named "consecutiveFailures" is in fact a LIFETIME CUMULATIVE counter. A
// provider that succeeds far more often than it fails still trips, because the
// scattered failures accumulate forever and never decay.
//
// This is why the live breaker opened with NO burst of failures in the log.
func TestHACB001_ScatteredFailuresMustNotTripBreaker(t *testing.T) {
	cb := NewCircuitBreaker(5, 2, time.Minute)

	// A healthy-but-imperfect provider: one failure for every success, never
	// 5 in a row. A correct consecutive-failure breaker must stay CLOSED.
	for i := 0; i < 10; i++ {
		_ = cb.Call(func() error { return errors.New("transient blip") })
		_ = cb.Call(func() error { return nil }) // real success
	}

	state := cb.GetState()
	if redMode() {
		assert.Equal(t, StateOpen, state,
			"RED: pre-fix, non-consecutive failures accumulate and trip the breaker")
		assert.GreaterOrEqual(t, cb.GetFailureCount(), 5,
			"RED: pre-fix, a success does not reset the failure counter")
	} else {
		assert.Equal(t, StateClosed, state,
			"GREEN: 10 successes interleaved with 10 failures, never 5 consecutive -> must stay CLOSED")
		assert.Equal(t, 0, cb.GetFailureCount(),
			"GREEN: a success must reset the consecutive-failure counter to 0")
	}
}

// TestHACB001_BreakerMustRecoverWithoutRestart proves the user-visible symptom:
// a breaker facing a HEALTHY backend must serve traffic and recover on its own.
// A restart is not a fix — it already regressed once in production.
//
// Honest boundary (§11.4.6, established by measurement, not assumption): the
// pre-fix breaker CAN recover in isolation — trip it, let the cooldown elapse,
// feed it two successes, and it closes. A first draft of this guard asserted an
// unconditional never-recovers defect and did NOT reproduce. So the production
// "never recovers" symptom is NOT the cooldown logic on its own.
//
// It is the INTERACTION that is fatal, and this test reproduces it faithfully:
// the health probe ticks every 30s and ALWAYS fails (404 on the wrong path),
// while the recovery cooldown is 60s. The always-failing probe therefore fires
// roughly twice per cooldown window and re-opens the breaker before two
// consecutive successes can ever accumulate. Real traffic is locked out forever
// against a backend that is answering perfectly.
//
// Ratio preserved below (tick = cooldown/2), timings scaled for test speed.
func TestHACB001_BreakerMustRecoverWithoutRestart(t *testing.T) {
	const cooldown = 60 * time.Millisecond // production: 60s
	const healthTick = 30 * time.Millisecond // production: 30s

	stub := &stubProvider{
		completeErr:    nil, // the backend is HEALTHY for real work
		completeOKText: "real model output",
		healthErr:      errors.New("health check returned status 404"),
	}

	cb := NewCircuitBreaker(5, 2, cooldown)
	var active int64
	cbp := &circuitBreakerProvider{
		provider:              stub,
		circuitBreaker:        cb,
		concurrencySemaphore:  semaphore.NewWeighted(10),
		name:                  "helixllm",
		activeRequestsCounter: &active,
		totalPermits:          10,
	}

	// Run the two loops the live service runs: a periodic health probe and
	// real user traffic, over several cooldown windows.
	trafficOK, trafficFail := 0, 0
	for i := 0; i < 12; i++ {
		_ = cbp.HealthCheck() // the 30s tick — always 404
		time.Sleep(healthTick)
		if _, err := cbp.Complete(context.Background(), &models.LLMRequest{
			ID:     "user-traffic",
			Prompt: "What is 2+2?",
		}); err != nil {
			trafficFail++
		} else {
			trafficOK++
		}
	}

	if redMode() {
		// Honest boundary (§11.4.6): measurement shows the pre-fix breaker
		// starves recovery but does not refuse EVERY request in this harness —
		// whichever caller wins the race at a cooldown boundary decides. Some
		// requests slip through between the boundary and the next probe.
		// The defect asserted is therefore the one that is actually true and
		// actually reproduces: real traffic is refused AT ALL against a
		// backend that is answering every call perfectly.
		assert.Positive(t, trafficFail,
			"RED: pre-fix, an always-failing 30s health probe against a 60s cooldown "+
				"refuses real user traffic despite a fully healthy backend")
	} else {
		assert.Zero(t, trafficFail,
			"GREEN: a healthy backend must serve every request regardless of health-probe noise")
		assert.Equal(t, 12, trafficOK, "GREEN: all real traffic must succeed")
		assert.Equal(t, StateClosed, cb.GetState(),
			"GREEN: the breaker must be CLOSED against a healthy backend, with no restart")
	}
}

// TestHACB001_HealthProbeMustNotPoisonTrafficBreaker proves defect D1, the
// root cause of the live incident: circuitBreakerProvider.HealthCheck() routes
// the DIAGNOSTIC health probe through the SAME breaker that gates USER TRAFFIC.
//
// The helixllm provider probes /internal/health — a HelixLLM-gateway path. The
// provider is pointed at a plain OpenAI-compatible server (the coder at :18434)
// which 404s that path while serving /v1/chat/completions perfectly. Every
// 30s health tick therefore records a failure against a provider that is
// demonstrably healthy for real work.
func TestHACB001_HealthProbeMustNotPoisonTrafficBreaker(t *testing.T) {
	stub := &stubProvider{
		// Real traffic ALWAYS succeeds — the backend is healthy.
		completeErr:    nil,
		completeOKText: "real model output",
		// The health probe ALWAYS fails — wrong path on this backend (404).
		healthErr: errors.New("health check returned status 404"),
	}

	cb := NewCircuitBreaker(5, 2, time.Minute)
	var active int64
	cbp := &circuitBreakerProvider{
		provider:              stub,
		circuitBreaker:        cb,
		concurrencySemaphore:  semaphore.NewWeighted(10),
		name:                  "helixllm",
		activeRequestsCounter: &active,
		totalPermits:          10,
	}

	// 30s tick x 5 = the ~2.5 minutes the live service took to trip.
	for i := 0; i < 5; i++ {
		_ = cbp.HealthCheck()
	}

	// Now a real user request against the healthy backend.
	resp, err := cbp.Complete(context.Background(), &models.LLMRequest{
		ID:     "user-request-after-health-probes",
		Prompt: "What is 2+2?",
	})

	if redMode() {
		require.Error(t, err,
			"RED: pre-fix, 5 failing health probes lock out real traffic")
		assert.Contains(t, err.Error(), "circuit breaker is open",
			"RED: the exact error the operator observed")
		assert.Equal(t, 0, stub.completeCalls,
			"RED: the healthy backend was never even called")
	} else {
		require.NoError(t, err,
			"GREEN: diagnostic health-probe failures must NOT gate user traffic")
		require.NotNil(t, resp)
		assert.Equal(t, "real model output", resp.Content,
			"GREEN: the real backend must be reached and its real content returned")
		assert.Equal(t, StateClosed, cb.GetState(),
			"GREEN: health-probe failures must not open the traffic breaker")
		assert.Equal(t, 1, stub.completeCalls,
			"GREEN: the healthy backend must actually be called")
	}
}

// TestHACB001_RealTrafficFailuresStillTripBreaker is the NEGATIVE control.
// The fix must not disable the breaker: genuine consecutive failures on the
// TRAFFIC path must still open it. Without this, "fixing" the bug by gutting
// the breaker would pass the tests above — the classic §1.1 bluff.
//
// This assertion holds in BOTH polarities: it is behaviour the fix preserves.
func TestHACB001_RealTrafficFailuresStillTripBreaker(t *testing.T) {
	stub := &stubProvider{
		completeErr: errors.New("connection refused"),
		healthErr:   nil,
	}

	cb := NewCircuitBreaker(3, 2, time.Minute)
	var active int64
	cbp := &circuitBreakerProvider{
		provider:              stub,
		circuitBreaker:        cb,
		concurrencySemaphore:  semaphore.NewWeighted(10),
		name:                  "helixllm",
		activeRequestsCounter: &active,
		totalPermits:          10,
	}

	for i := 0; i < 3; i++ {
		_, _ = cbp.Complete(context.Background(), &models.LLMRequest{ID: "x"})
	}

	assert.Equal(t, StateOpen, cb.GetState(),
		"a genuinely failing BACKEND must still open the breaker — the fix must not disable protection")

	_, err := cbp.Complete(context.Background(), &models.LLMRequest{ID: "y"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "circuit breaker is open")
}
