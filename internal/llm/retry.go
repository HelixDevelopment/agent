package llm

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"time"
)

// RetryConfig defines retry behavior for LLM API calls
type RetryConfig struct {
	// MaxRetries is the maximum number of retry attempts (0 = no retries)
	MaxRetries int
	// InitialDelay is the initial delay before first retry
	InitialDelay time.Duration
	// MaxDelay is the maximum delay between retries
	MaxDelay time.Duration
	// Multiplier is the factor by which delay increases after each retry
	Multiplier float64
	// JitterFactor adds randomness to delays (0.0-1.0)
	JitterFactor float64
}

// DefaultRetryConfig returns sensible defaults for LLM API retry behavior
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:   3,
		InitialDelay: 1 * time.Second,
		MaxDelay:     30 * time.Second,
		Multiplier:   2.0,
		JitterFactor: 0.1,
	}
}

// RetryableFunc is a function that can be retried
type RetryableFunc func() (*http.Response, error)

// RetryResult contains the result of a retry operation
type RetryResult struct {
	Response   *http.Response
	Attempts   int
	LastError  error
	TotalDelay time.Duration
}

// IsRetryableStatusCode determines if an HTTP status code warrants a retry
func IsRetryableStatusCode(statusCode int) bool {
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

// IsRetryableError determines if an error warrants a retry
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}

	// Context cancelled or deadline exceeded - don't retry
	if err == context.Canceled || err == context.DeadlineExceeded {
		return false
	}

	// Network errors are generally retryable
	// This includes connection refused, timeout, DNS errors, etc.
	return true
}

// ExecuteWithRetry executes a function with retry logic and exponential backoff
func ExecuteWithRetry(ctx context.Context, config RetryConfig, fn RetryableFunc) (*RetryResult, error) {
	result := &RetryResult{
		Attempts: 0,
	}

	delay := config.InitialDelay

	for attempt := 0; attempt <= config.MaxRetries; attempt++ {
		result.Attempts = attempt + 1

		// Check context before making request
		select {
		case <-ctx.Done():
			result.LastError = ctx.Err()
			return result, fmt.Errorf("context cancelled before attempt %d: %w", attempt+1, ctx.Err())
		default:
		}

		// Execute the function
		resp, err := fn()

		// Success - return immediately
		if err == nil && resp != nil && !IsRetryableStatusCode(resp.StatusCode) {
			result.Response = resp
			return result, nil
		}

		// If we got a response with retryable status, close it before retrying
		if resp != nil && IsRetryableStatusCode(resp.StatusCode) {
			result.LastError = fmt.Errorf("HTTP %d: retryable server error", resp.StatusCode)
			_ = resp.Body.Close()
		} else if err != nil {
			result.LastError = err
		}

		// Check if we should retry
		shouldRetry := false
		if err != nil && IsRetryableError(err) {
			shouldRetry = true
		} else if resp != nil && IsRetryableStatusCode(resp.StatusCode) {
			shouldRetry = true
		}

		// Last attempt or non-retryable error - return
		if !shouldRetry || attempt >= config.MaxRetries {
			if result.LastError != nil {
				return result, fmt.Errorf("all %d attempts failed: %w", result.Attempts, result.LastError)
			}
			result.Response = resp
			return result, nil
		}

		// Calculate delay with jitter
		jitteredDelay := addJitter(delay, config.JitterFactor)

		// Wait before retry
		select {
		case <-ctx.Done():
			result.LastError = ctx.Err()
			return result, fmt.Errorf("context cancelled during backoff: %w", ctx.Err())
		case <-time.After(jitteredDelay):
			result.TotalDelay += jitteredDelay
		}

		// Increase delay for next retry (exponential backoff)
		delay = time.Duration(float64(delay) * config.Multiplier)
		if delay > config.MaxDelay {
			delay = config.MaxDelay
		}
	}

	return result, fmt.Errorf("max retries exceeded: %w", result.LastError)
}

// addJitter adds randomness to a duration
// Note: Using math/rand for jitter is acceptable - it doesn't require cryptographic randomness
func addJitter(d time.Duration, factor float64) time.Duration {
	if factor <= 0 {
		return d
	}

	// Calculate jitter range
	jitterRange := float64(d) * factor

	// Add random jitter (can be positive or negative)
	jitter := (rand.Float64() - 0.5) * 2 * jitterRange // #nosec G404 - jitter doesn't require cryptographic randomness

	result := time.Duration(float64(d) + jitter)
	if result < 0 {
		result = 0
	}

	return result
}

// CalculateBackoff calculates the backoff duration for a given attempt
func CalculateBackoff(attempt int, config RetryConfig) time.Duration {
	if attempt <= 0 {
		return config.InitialDelay
	}

	delay := float64(config.InitialDelay) * math.Pow(config.Multiplier, float64(attempt-1))

	if delay > float64(config.MaxDelay) {
		delay = float64(config.MaxDelay)
	}

	return addJitter(time.Duration(delay), config.JitterFactor)
}

// RetryableHTTPClient wraps an http.Client with retry logic
type RetryableHTTPClient struct {
	client *http.Client
	config RetryConfig
}

// NewRetryableHTTPClient creates a new RetryableHTTPClient
func NewRetryableHTTPClient(client *http.Client, config RetryConfig) *RetryableHTTPClient {
	if client == nil {
		client = &http.Client{
			Timeout: 60 * time.Second,
		}
	}
	return &RetryableHTTPClient{
		client: client,
		config: config,
	}
}

// Do executes an HTTP request with retry logic.
//
// # Replaying the body
//
// http.Request.Clone copies the body READER, not its contents. A retry that
// merely cloned the request therefore sent ContentLength=N with an EMPTY body:
// the server received a different, smaller request and the caller was told the
// retry succeeded. Measured failure — a dropped connection produced
//
//	all 3 attempts failed: Post "...": http: ContentLength=57 with Body length 0
//
// so each attempt now rewinds the body from req.GetBody().
//
// When a body cannot be rewound (GetBody is nil, e.g. an io.Pipe) and retries
// are enabled, Do REFUSES rather than sending an empty body on a retry: an
// honest error beats a silent wrong request. Buffering an unbounded body
// instead would trade a correctness bug for a memory one.
func (c *RetryableHTTPClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	hasBody := req.Body != nil && req.Body != http.NoBody
	if hasBody && c.config.MaxRetries > 0 && req.GetBody == nil {
		return nil, fmt.Errorf("retry: refusing to retry a request whose body cannot be replayed (GetBody is nil)")
	}

	result, err := ExecuteWithRetry(ctx, c.config, func() (*http.Response, error) {
		clonedReq := req.Clone(ctx)
		if hasBody {
			body, getErr := req.GetBody()
			if getErr != nil {
				return nil, fmt.Errorf("retry: rewinding request body: %w", getErr)
			}
			clonedReq.Body = body
		}
		//nolint:gosec // G704: retry wrapper for LLM provider calls — URL is supplied by the provider adapter, not the end user; SSRF defence applies at the adapter construction layer
		return c.client.Do(clonedReq)
	})

	if err != nil {
		return nil, err
	}

	return result.Response, nil
}

// GetAttempts returns the number of attempts from the last request
func (c *RetryableHTTPClient) GetConfig() RetryConfig {
	return c.config
}
