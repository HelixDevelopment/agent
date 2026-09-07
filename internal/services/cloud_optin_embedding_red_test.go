package services

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HA-F2-002 (review remediation) — the FOURTH implicit cloud-acquisition path,
// found by enumerating every place an API key becomes a constructed client or
// an outbound request.
//
// NewEmbeddingManager (the zero-arg constructor wired at
// router.go -> NewUnifiedProtocolManager) read OPENAI_API_KEY straight from the
// environment, so a caller hitting the standard, wired route
// POST /v1/protocols/execute {"protocol_type":"embedding"} caused a real,
// billed POST to https://api.openai.com/v1/embeddings purely because a key was
// present — with no cloud opt-in anywhere in the picture.
//
// The observable here is the ROUTE, not a flag: the manager's http.Client is
// swapped for a counting RoundTripper, so the test asserts on whether an
// outbound request is actually issued. (The endpoint URL is hardcoded, so a
// transport hook is the only way to observe it without touching the network.)
//
//	RED_MODE=1   — assert the DEFECT IS PRESENT (pre-fix tree): one outbound
//	               request attempted from an ambient key.
//	default (=0) — the standing GREEN guard: zero outbound requests, and the
//	               local embedding fallback still returns a usable result.
//
// §11.4.115 RED-baseline-on-the-broken-artifact + polarity switch.
func TestCloudGate_AmbientOpenAIKeyDoesNotSendEmbeddingsToTheCloud(t *testing.T) {
	setupCloudGateEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-fake-openai-key-for-test-only")

	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	manager := NewEmbeddingManager(nil, nil, log)
	require.NotNil(t, manager)

	counter := &countingRoundTripper{}
	manager.httpClient = &http.Client{Transport: counter}

	resp, err := manager.GenerateEmbedding(context.Background(), "local-first embedding probe")
	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.NotEmpty(t, resp.Embeddings, "an embedding must still be produced — locally")

	outbound := atomic.LoadInt64(&counter.calls)

	if os.Getenv("RED_MODE") == "1" {
		assert.Positive(t, outbound,
			"RED_MODE=1: defect — an ambient OPENAI_API_KEY must send embedding text to api.openai.com with zero operator opt-in")
		assert.Equal(t, "api.openai.com", counter.lastHost,
			"RED_MODE=1: defect — the outbound request must target the OpenAI endpoint")
		return
	}

	assert.Zero(t, outbound,
		"an ambient OPENAI_API_KEY must NOT send embedding text to a cloud endpoint while HELIX_CLOUD_PROVIDERS is unset")
}

// TestCloudGate_EmbeddingOptInStillReachesTheCloud is the other polarity: an
// operator who opted in keeps cloud embeddings.
func TestCloudGate_EmbeddingOptInStillReachesTheCloud(t *testing.T) {
	if os.Getenv("RED_MODE") == "1" {
		t.Skip("SKIP-OK: opt-in seam is asserted by the GREEN guard; the RED evidence is the defect reproduction above")
	}
	setupCloudGateEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-fake-openai-key-for-test-only")
	t.Setenv("HELIX_CLOUD_PROVIDERS", "true")

	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	manager := NewEmbeddingManager(nil, nil, log)
	require.NotNil(t, manager)

	counter := &countingRoundTripper{}
	manager.httpClient = &http.Client{Transport: counter}

	_, err := manager.GenerateEmbedding(context.Background(), "opted-in embedding probe")
	require.NoError(t, err)

	assert.Positive(t, atomic.LoadInt64(&counter.calls),
		"with HELIX_CLOUD_PROVIDERS=true the operator asked for cloud: the OpenAI embedding call must still be attempted")
	assert.Equal(t, "api.openai.com", counter.lastHost)
}

// countingRoundTripper records outbound requests and answers them locally, so
// the assertion is about intent-to-call and never touches the network.
type countingRoundTripper struct {
	calls    int64
	lastHost string
}

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt64(&c.calls, 1)
	c.lastHost = req.URL.Host
	return &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"intercepted by test"}}`)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}
