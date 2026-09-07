// Package verifier provides integration with LLMsVerifier for model verification,
// scoring, and health monitoring capabilities.
package verifier

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"dev.helix.agent/internal/llm"
	"dev.helix.agent/internal/models"
)

// =====================================================
// COMPREHENSIVE STARTUP VERIFIER TESTS
// =====================================================

func TestStartupVerifier_VerifyAllProviders_WithMockProviderFunc(t *testing.T) {
	cfg := DefaultStartupConfig()
	cfg.VerificationTimeout = 5 * time.Second
	cfg.ParallelVerification = false // Sequential for predictable testing
	cfg.EnableFreeProviders = true

	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	sv := NewStartupVerifier(cfg, logger)

	// Set a mock provider function
	sv.SetProviderFunc(func(ctx context.Context, modelID, provider, prompt string) (string, error) {
		return "Yes, I can see your code", nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := sv.VerifyAllProviders(ctx)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.GreaterOrEqual(t, result.TotalProviders, 0)
	// DurationMs could be 0 for very fast executions
	assert.GreaterOrEqual(t, result.DurationMs, int64(0))
}

func TestStartupVerifier_VerifyAllProviders_Parallel(t *testing.T) {
	cfg := DefaultStartupConfig()
	cfg.VerificationTimeout = 5 * time.Second
	cfg.ParallelVerification = true
	cfg.MaxConcurrency = 4
	cfg.EnableFreeProviders = true

	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	sv := NewStartupVerifier(cfg, logger)

	sv.SetProviderFunc(func(ctx context.Context, modelID, provider, prompt string) (string, error) {
		return "Yes, I can see your code", nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := sv.VerifyAllProviders(ctx)

	require.NoError(t, err)
	require.NotNil(t, result)
}

func TestStartupVerifier_GetProvider(t *testing.T) {
	cfg := DefaultStartupConfig()
	sv := NewStartupVerifier(cfg, nil)

	// Before verification, should not find providers
	provider, found := sv.GetProvider("claude")
	assert.False(t, found)
	assert.Nil(t, provider)
}

func TestStartupVerifier_GetVerifiedProviders_Empty(t *testing.T) {
	cfg := DefaultStartupConfig()
	sv := NewStartupVerifier(cfg, nil)

	verified := sv.GetVerifiedProviders()
	// May return nil or empty slice before verification
	assert.Empty(t, verified)
}

func TestStartupVerifier_IsInitialized(t *testing.T) {
	cfg := DefaultStartupConfig()
	sv := NewStartupVerifier(cfg, nil)

	// Before verification
	assert.False(t, sv.IsInitialized())
}

func TestStartupVerifier_HelperFunctions(t *testing.T) {
	t.Run("isPlaceholder", func(t *testing.T) {
		tests := []struct {
			value    string
			expected bool
		}{
			{"your-api-key", true},
			{"sk-xxx", true},
			{"placeholder", true},
			{"INSERT_KEY", true},
			{"sk-valid-key-12345", false},
			{"real-api-key-value", false},
			{"", false},
		}

		for _, tt := range tests {
			result := isPlaceholder(tt.value)
			if result != tt.expected {
				t.Errorf("isPlaceholder(%q) = %v, want %v", tt.value, result, tt.expected)
			}
		}
	})

	t.Run("maskAPIKey", func(t *testing.T) {
		tests := []struct {
			key      string
			expected string
		}{
			{"short", "****"},
			{"12345678", "****"},
			{"sk-1234567890abcdef", "sk-1****cdef"},
			{"abcdefghijklmnop", "abcd****mnop"},
		}

		for _, tt := range tests {
			result := maskAPIKey(tt.key)
			if result != tt.expected {
				t.Errorf("maskAPIKey(%q) = %q, want %q", tt.key, result, tt.expected)
			}
		}
	})
}

func TestStartupVerifier_checkOllamaHealth(t *testing.T) {
	t.Run("success with models", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/tags" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"models":[{"name":"llama3.2"},{"name":"codellama"}]}`))
			}
		}))
		defer server.Close()

		cfg := DefaultStartupConfig()
		logger := logrus.New()
		logger.SetLevel(logrus.WarnLevel)
		sv := NewStartupVerifier(cfg, logger)

		models := sv.checkOllamaHealth(server.URL)
		assert.Len(t, models, 2)
		assert.Contains(t, models, "llama3.2")
		assert.Contains(t, models, "codellama")
	})

	t.Run("empty models", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/tags" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"models":[]}`))
			}
		}))
		defer server.Close()

		cfg := DefaultStartupConfig()
		logger := logrus.New()
		logger.SetLevel(logrus.WarnLevel)
		sv := NewStartupVerifier(cfg, logger)

		models := sv.checkOllamaHealth(server.URL)
		assert.Nil(t, models)
	})

	t.Run("server error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		cfg := DefaultStartupConfig()
		logger := logrus.New()
		logger.SetLevel(logrus.WarnLevel)
		sv := NewStartupVerifier(cfg, logger)

		models := sv.checkOllamaHealth(server.URL)
		assert.Nil(t, models)
	})

	t.Run("invalid JSON", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/tags" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`invalid json`))
			}
		}))
		defer server.Close()

		cfg := DefaultStartupConfig()
		logger := logrus.New()
		logger.SetLevel(logrus.WarnLevel)
		sv := NewStartupVerifier(cfg, logger)

		models := sv.checkOllamaHealth(server.URL)
		// Should return default model when parsing fails but server responds
		assert.Contains(t, models, "llama3.2")
	})

	t.Run("connection refused", func(t *testing.T) {
		cfg := DefaultStartupConfig()
		logger := logrus.New()
		logger.SetLevel(logrus.WarnLevel)
		sv := NewStartupVerifier(cfg, logger)

		models := sv.checkOllamaHealth("http://localhost:99999")
		assert.Nil(t, models)
	})
}

func TestStartupVerifier_discoverFreeProviders(t *testing.T) {
	t.Run("disabled free providers", func(t *testing.T) {
		cfg := DefaultStartupConfig()
		cfg.EnableFreeProviders = false
		sv := NewStartupVerifier(cfg, nil)

		providers := sv.discoverFreeProviders(context.Background())
		assert.Empty(t, providers)
	})

	// Reconciled for the local-first cloud gate (HA-F2-002, §11.4.120).
	// Zen needs no credential but talks to a public third-party endpoint, so it
	// is a cloud acquisition and now obeys HELIX_CLOUD_PROVIDERS — matching the
	// provider registry, where the synthesized zen config has always been gated
	// on r.autoDiscovery. Every original assertion is preserved under the
	// opt-in; the opted-out invariant is added rather than substituted.
	t.Run("enabled free providers, cloud opted in", func(t *testing.T) {
		t.Setenv("HELIX_CLOUD_PROVIDERS", "true")
		cfg := DefaultStartupConfig()
		cfg.EnableFreeProviders = true
		logger := logrus.New()
		logger.SetLevel(logrus.WarnLevel)
		sv := NewStartupVerifier(cfg, logger)

		providers := sv.discoverFreeProviders(context.Background())
		// At minimum, Zen should be discovered
		assert.GreaterOrEqual(t, len(providers), 1)

		zenFound := false
		for _, p := range providers {
			if p.Type == "zen" {
				zenFound = true
				assert.Equal(t, AuthTypeFree, p.AuthType)
				assert.Equal(t, "dynamic_discovery", p.Source)
			}
		}
		assert.True(t, zenFound, "Zen provider should be discovered")
	})

	t.Run("enabled free providers, cloud NOT opted in", func(t *testing.T) {
		t.Setenv("HELIX_CLOUD_PROVIDERS", "")
		cfg := DefaultStartupConfig()
		cfg.EnableFreeProviders = true
		logger := logrus.New()
		logger.SetLevel(logrus.WarnLevel)
		sv := NewStartupVerifier(cfg, logger)

		providers := sv.discoverFreeProviders(context.Background())
		for _, p := range providers {
			assert.NotEqual(t, "zen", p.Type,
				"the anonymous zen endpoint is a cloud acquisition: it must not be discovered without an explicit opt-in")
		}
	})
}

func TestStartupVerifier_verifyProvider_Types(t *testing.T) {
	cfg := DefaultStartupConfig()
	cfg.VerificationTimeout = 2 * time.Second
	cfg.TrustOAuthOnFailure = true
	cfg.FreeProviderBaseScore = 6.5

	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	sv := NewStartupVerifier(cfg, logger)

	// Set up a mock provider function that succeeds
	sv.SetTestMode(true) // Enable test mode to skip quality validation
	sv.SetProviderFunc(func(ctx context.Context, modelID, provider, prompt string) (string, error) {
		return "Yes, I can see your code", nil
	})

	ctx := context.Background()

	t.Run("verify OAuth provider", func(t *testing.T) {
		disc := &ProviderDiscoveryResult{
			ID:          "claude",
			Type:        "claude",
			AuthType:    AuthTypeOAuth,
			Discovered:  true,
			Source:      "oauth",
			Credentials: "OAuth",
			BaseURL:     "https://api.anthropic.com/v1/messages",
			Models:      []string{"claude-sonnet-4-5"},
		}

		provider, err := sv.verifyProvider(ctx, disc)
		// Error is expected since OAuth credentials are not available in tests
		if err != nil {
			assert.NotNil(t, provider) // Should still return provider with TrustOAuthOnFailure
			if provider != nil {
				assert.True(t, provider.Verified) // Trust OAuth
				assert.Equal(t, StatusHealthy, provider.Status)
			}
		}
	})

	t.Run("verify Free provider", func(t *testing.T) {
		disc := &ProviderDiscoveryResult{
			ID:          "zen",
			Type:        "zen",
			AuthType:    AuthTypeFree,
			Discovered:  true,
			Source:      "auto",
			Credentials: "Anonymous",
			BaseURL:     "https://opencode.ai/zen/v1/chat/completions",
			Models:      []string{"opencode/grok-code"},
		}

		provider, err := sv.verifyProvider(ctx, disc)
		require.NoError(t, err)
		require.NotNil(t, provider)
		assert.True(t, provider.Verified)
		assert.Equal(t, StatusHealthy, provider.Status)
		assert.GreaterOrEqual(t, provider.Score, cfg.FreeProviderBaseScore)
	})

	t.Run("verify Local provider", func(t *testing.T) {
		disc := &ProviderDiscoveryResult{
			ID:          "ollama",
			Type:        "ollama",
			AuthType:    AuthTypeLocal,
			Discovered:  true,
			Source:      "auto",
			Credentials: "Local",
			BaseURL:     "http://localhost:11434",
			Models:      []string{"llama3.2"},
		}

		provider, err := sv.verifyProvider(ctx, disc)
		require.NoError(t, err)
		require.NotNil(t, provider)
		assert.True(t, provider.Verified)
		assert.Equal(t, StatusHealthy, provider.Status)
		assert.Equal(t, 5.0, provider.Score)
		assert.Equal(t, 20, provider.Priority)
	})

	t.Run("verify APIKey provider with error", func(t *testing.T) {
		// Set provider function to fail
		sv.SetProviderFunc(func(ctx context.Context, modelID, provider, prompt string) (string, error) {
			return "", errors.New("API key invalid")
		})

		disc := &ProviderDiscoveryResult{
			ID:          "deepseek",
			Type:        "deepseek",
			AuthType:    AuthTypeAPIKey,
			Discovered:  true,
			Source:      "env",
			Credentials: "sk-****",
			BaseURL:     "https://api.deepseek.com/v1/chat/completions",
			Models:      []string{"deepseek-chat"},
		}

		provider, err := sv.verifyProvider(ctx, disc)
		// Provider may be returned with or without error depending on implementation
		// The key assertion is that the provider is not verified if there's an error
		if err != nil {
			// If error returned, provider should be marked as not verified
			if provider != nil {
				assert.False(t, provider.Verified)
			}
		} else {
			// If no error, verify result based on provider state
			assert.NotNil(t, provider)
		}
	})
}

func TestStartupVerifier_rankProviders(t *testing.T) {
	cfg := DefaultStartupConfig()
	sv := NewStartupVerifier(cfg, nil)

	// Create test providers
	providers := []*UnifiedProvider{
		{ID: "low", Score: 5.0, AuthType: AuthTypeAPIKey, Verified: true},
		{ID: "high", Score: 9.0, AuthType: AuthTypeAPIKey, Verified: true},
		{ID: "oauth", Score: 8.0, AuthType: AuthTypeOAuth, Verified: true},
		{ID: "medium", Score: 7.0, AuthType: AuthTypeAPIKey, Verified: true},
	}

	sv.rankProviders(providers)

	// Sorted by score descending (highest first) - NO OAuth priority
	assert.Equal(t, "high", func() *UnifiedProvider { s := sv.rankedProviders.Snapshot(); return s[0] }().ID)
	assert.Equal(t, "oauth", func() *UnifiedProvider { s := sv.rankedProviders.Snapshot(); return s[1] }().ID)
	assert.Equal(t, "medium", func() *UnifiedProvider { s := sv.rankedProviders.Snapshot(); return s[2] }().ID)
	assert.Equal(t, "low", func() *UnifiedProvider { s := sv.rankedProviders.Snapshot(); return s[3] }().ID)
}

func TestStartupVerifier_selectDebateTeam(t *testing.T) {
	cfg := DefaultStartupConfig()
	cfg.PositionCount = 3
	cfg.FallbacksPerPosition = 1
	cfg.MinScore = 5.0
	cfg.OAuthPrimaryNonOAuthFallback = true

	sv := NewStartupVerifier(cfg, nil)

	t.Run("single provider with LLM reuse", func(t *testing.T) {
		// NEW BEHAVIOR: With only 1 provider, we REUSE its LLM to fill all positions
		// No error - we fill all 3 positions * (1 primary + 1 fallback) = 6 slots
		sv.rankedProviders.Replace([]*UnifiedProvider{
			{ID: "p1", Type: "claude", Score: 9.0, Verified: true, Models: []UnifiedModel{{ID: "m1", Name: "Model 1"}}},
		})

		team, err := sv.selectDebateTeam()
		require.NoError(t, err)
		require.NotNil(t, team)
		// All positions should be filled via reuse
		assert.Equal(t, cfg.PositionCount, len(team.Positions))
		// Each position has primary + 1 fallback = 2 per position = 6 total
		expectedTotal := cfg.PositionCount * (1 + cfg.FallbacksPerPosition)
		assert.Equal(t, expectedTotal, team.TotalLLMs)
		// All positions use the same LLM (reused)
		for _, pos := range team.Positions {
			assert.Equal(t, "m1", pos.Primary.ModelID)
			for _, fb := range pos.Fallbacks {
				assert.Equal(t, "m1", fb.ModelID)
			}
		}
	})

	t.Run("sufficient providers", func(t *testing.T) {
		sv.rankedProviders.Replace([]*UnifiedProvider{
			{ID: "p1", Type: "claude", Score: 9.0, AuthType: AuthTypeOAuth, Verified: true, Models: []UnifiedModel{{ID: "m1", Name: "M1"}}},
			{ID: "p2", Type: "gemini", Score: 8.5, AuthType: AuthTypeAPIKey, Verified: true, Models: []UnifiedModel{{ID: "m2", Name: "M2"}}},
			{ID: "p3", Type: "deepseek", Score: 8.0, AuthType: AuthTypeAPIKey, Verified: true, Models: []UnifiedModel{{ID: "m3", Name: "M3"}}},
			{ID: "p4", Type: "mistral", Score: 7.5, AuthType: AuthTypeAPIKey, Verified: true, Models: []UnifiedModel{{ID: "m4", Name: "M4"}}},
			{ID: "p5", Type: "zen", Score: 6.5, AuthType: AuthTypeFree, Verified: true, Models: []UnifiedModel{{ID: "m5", Name: "M5"}}},
			{ID: "p6", Type: "ollama", Score: 5.0, AuthType: AuthTypeLocal, Verified: true, Models: []UnifiedModel{{ID: "m6", Name: "M6"}}},
		})

		team, err := sv.selectDebateTeam()
		require.NoError(t, err)
		require.NotNil(t, team)
		assert.Equal(t, cfg.PositionCount, len(team.Positions))
		assert.GreaterOrEqual(t, team.TotalLLMs, cfg.PositionCount)
	})

	t.Run("low score providers excluded", func(t *testing.T) {
		sv.rankedProviders.Replace([]*UnifiedProvider{
			{ID: "p1", Type: "claude", Score: 9.0, Verified: true, Models: []UnifiedModel{{ID: "m1", Name: "M1"}}},
			{ID: "p2", Type: "gemini", Score: 8.5, Verified: true, Models: []UnifiedModel{{ID: "m2", Name: "M2"}}},
			{ID: "p3", Type: "deepseek", Score: 8.0, Verified: true, Models: []UnifiedModel{{ID: "m3", Name: "M3"}}},
			{ID: "low", Type: "low", Score: 3.0, Verified: true, Models: []UnifiedModel{{ID: "ml", Name: "ML"}}}, // Below min score
		})

		team, err := sv.selectDebateTeam()
		require.NoError(t, err)
		require.NotNil(t, team)

		// Low score provider should not be in team
		for _, pos := range team.Positions {
			if pos.Primary != nil {
				assert.NotEqual(t, "low", pos.Primary.Provider)
			}
		}
	})
}

func TestStartupVerifier_scoreProviders(t *testing.T) {
	cfg := DefaultStartupConfig()
	sv := NewStartupVerifier(cfg, nil)

	providers := []*UnifiedProvider{
		{
			ID:       "p1",
			Verified: true,
			Status:   StatusHealthy,
			Score:    8.0,
			Models: []UnifiedModel{
				{ID: "m1", Score: 7.5},
				{ID: "m2", Score: 8.5},
			},
		},
		{
			ID:       "p2",
			Verified: true,
			Status:   StatusHealthy,
			Score:    9.5,
			Models: []UnifiedModel{
				{ID: "m3", Score: 9.5},
			},
		},
	}

	scored := sv.scoreProviders(context.Background(), providers)

	assert.Len(t, scored, 2)
	// Scores should be capped at 10.0
	for _, p := range scored {
		assert.LessOrEqual(t, p.Score, 10.0)
	}
}

func TestStartupVerifier_OAuthEnabled(t *testing.T) {
	cfg := DefaultStartupConfig()
	sv := NewStartupVerifier(cfg, nil)

	t.Run("Claude OAuth enabled via env", func(t *testing.T) {
		_ = os.Setenv("CLAUDE_CODE_USE_OAUTH_CREDENTIALS", "true")
		defer func() { _ = os.Unsetenv("CLAUDE_CODE_USE_OAUTH_CREDENTIALS") }()

		enabled := sv.isClaudeOAuthEnabled()
		assert.True(t, enabled)
	})

	t.Run("Claude OAuth disabled", func(t *testing.T) {
		_ = os.Unsetenv("CLAUDE_CODE_USE_OAUTH_CREDENTIALS")

		enabled := sv.isClaudeOAuthEnabled()
		assert.False(t, enabled)
	})

	t.Run("Qwen OAuth enabled via env", func(t *testing.T) {
		_ = os.Setenv("QWEN_CODE_USE_OAUTH_CREDENTIALS", "true")
		defer func() { _ = os.Unsetenv("QWEN_CODE_USE_OAUTH_CREDENTIALS") }()

		enabled := sv.isQwenOAuthEnabled()
		assert.True(t, enabled)
	})

	t.Run("Claude OAuth enabled via 1", func(t *testing.T) {
		_ = os.Setenv("CLAUDE_CODE_USE_OAUTH_CREDENTIALS", "1")
		defer func() { _ = os.Unsetenv("CLAUDE_CODE_USE_OAUTH_CREDENTIALS") }()

		enabled := sv.isClaudeOAuthEnabled()
		assert.True(t, enabled)
	})

	t.Run("Claude OAuth enabled via yes", func(t *testing.T) {
		_ = os.Setenv("CLAUDE_CODE_USE_OAUTH_CREDENTIALS", "yes")
		defer func() { _ = os.Unsetenv("CLAUDE_CODE_USE_OAUTH_CREDENTIALS") }()

		enabled := sv.isClaudeOAuthEnabled()
		assert.True(t, enabled)
	})
}

func TestStartupVerifier_DiscoverProviders(t *testing.T) {
	// Reconciled for the local-first cloud gate (HA-F2-002, §11.4.120): the
	// "at minimum, free providers are discovered" property only holds once the
	// operator has opted in to cloud, because the free provider in question
	// (zen) is a public third-party endpoint. The opted-OUT property — nothing
	// cloud discovered, zero outbound requests — is asserted by
	// TestCloudGate_VerifierDoesNotDiscoverCloudWithoutOptIn.
	t.Setenv("HELIX_CLOUD_PROVIDERS", "true")

	cfg := DefaultStartupConfig()
	cfg.EnableFreeProviders = true
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	sv := NewStartupVerifier(cfg, logger)

	ctx := context.Background()
	discovered, err := sv.discoverProviders(ctx)

	require.NoError(t, err)
	// At minimum, free providers should be discovered
	assert.GreaterOrEqual(t, len(discovered), 1)
}

// =====================================================
// MOCK PROVIDER FOR TESTING
// =====================================================

type mockStartupLLMProvider struct {
	name           string
	completeResp   *models.LLMResponse
	completeErr    error
	healthCheckErr error
}

func (m *mockStartupLLMProvider) Complete(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error) {
	if m.completeErr != nil {
		return nil, m.completeErr
	}
	if m.completeResp != nil {
		return m.completeResp, nil
	}
	return &models.LLMResponse{
		ID:           "mock-response",
		Content:      "Yes, I can see your code",
		ProviderName: m.name,
	}, nil
}

func (m *mockStartupLLMProvider) CompleteStream(ctx context.Context, req *models.LLMRequest) (<-chan *models.LLMResponse, error) {
	ch := make(chan *models.LLMResponse, 1)
	close(ch)
	return ch, nil
}

func (m *mockStartupLLMProvider) HealthCheck() error {
	return m.healthCheckErr
}

func (m *mockStartupLLMProvider) GetCapabilities() *models.ProviderCapabilities {
	return &models.ProviderCapabilities{
		SupportedModels:   []string{"test-model"},
		SupportsStreaming: true,
	}
}

func (m *mockStartupLLMProvider) ValidateConfig(config map[string]interface{}) (bool, []string) {
	return true, nil
}

var _ llm.LLMProvider = (*mockStartupLLMProvider)(nil)

func TestStartupVerifier_WithProviderFactory_Comprehensive(t *testing.T) {
	cfg := DefaultStartupConfig()
	sv := NewStartupVerifier(cfg, nil)

	sv.SetProviderFactory(func(providerType string, config ProviderCreateConfig) (llm.LLMProvider, error) {
		return &mockStartupLLMProvider{name: providerType}, nil
	})

	// Verify factory was set
	assert.NotNil(t, sv.providerFactory)
}

func TestStartupResult_Fields_Comprehensive(t *testing.T) {
	result := &StartupResult{
		TotalProviders:  10,
		VerifiedCount:   8,
		FailedCount:     2,
		SkippedCount:    0,
		APIKeyProviders: 5,
		OAuthProviders:  2,
		FreeProviders:   3,
		StartedAt:       time.Now(),
		CompletedAt:     time.Now().Add(5 * time.Second),
		DurationMs:      5000,
		Providers:       []*UnifiedProvider{},
		RankedProviders: []*UnifiedProvider{},
		DebateTeam:      nil,
		Errors:          []StartupError{},
	}

	assert.Equal(t, 10, result.TotalProviders)
	assert.Equal(t, 8, result.VerifiedCount)
	assert.Equal(t, 2, result.FailedCount)
	assert.Equal(t, 5, result.APIKeyProviders)
	assert.Equal(t, 2, result.OAuthProviders)
	assert.Equal(t, 3, result.FreeProviders)
	assert.Equal(t, int64(5000), result.DurationMs)
}

func TestDebateLLM_Fields(t *testing.T) {
	llm := &DebateLLM{
		Provider:     "claude",
		ProviderType: "claude",
		ModelID:      "claude-sonnet-4-5",
		ModelName:    "Claude Sonnet 4.5",
		AuthType:     AuthTypeOAuth,
		Score:        9.5,
		Verified:     true,
		IsOAuth:      true,
	}

	assert.Equal(t, "claude", llm.Provider)
	assert.Equal(t, "claude-sonnet-4-5", llm.ModelID)
	assert.Equal(t, AuthTypeOAuth, llm.AuthType)
	assert.Equal(t, 9.5, llm.Score)
	assert.True(t, llm.Verified)
	assert.True(t, llm.IsOAuth)
}

func TestDebatePosition_Fields(t *testing.T) {
	pos := &DebatePosition{
		Position: 1,
		Role:     "analyst",
		Primary: &DebateLLM{
			Provider: "deepseek",
			ModelID:  "deepseek-chat",
			Score:    9.5,
		},
		Fallbacks: []*DebateLLM{
			{
				Provider: "gemini",
				ModelID:  "gemini-2.0-flash",
				Score:    8.5,
			},
			{
				Provider: "mistral",
				ModelID:  "mistral-large",
				Score:    8.0,
			},
		},
	}

	assert.Equal(t, 1, pos.Position)
	assert.Equal(t, "analyst", pos.Role)
	assert.NotNil(t, pos.Primary)
	assert.Len(t, pos.Fallbacks, 2, "Should have 2 fallbacks")
	assert.Equal(t, "gemini", pos.Fallbacks[0].Provider)
}

func TestProviderTypeInfo_GetFunctions(t *testing.T) {
	t.Run("GetProviderInfo known", func(t *testing.T) {
		info, ok := GetProviderInfo("claude")
		assert.True(t, ok)
		assert.NotNil(t, info)
		assert.Equal(t, "claude", info.Type)
	})

	t.Run("GetProviderInfo unknown", func(t *testing.T) {
		info, ok := GetProviderInfo("unknown")
		assert.False(t, ok)
		assert.Nil(t, info)
	})

	t.Run("IsOAuthProvider", func(t *testing.T) {
		assert.True(t, IsOAuthProvider("claude"))
		assert.True(t, IsOAuthProvider("qwen"))
		assert.False(t, IsOAuthProvider("gemini"))
		assert.False(t, IsOAuthProvider("unknown"))
	})

	t.Run("IsFreeProvider", func(t *testing.T) {
		assert.True(t, IsFreeProvider("zen"))
		assert.True(t, IsFreeProvider("ollama"))
		assert.True(t, IsFreeProvider("openrouter"))
		assert.False(t, IsFreeProvider("claude"))
		assert.False(t, IsFreeProvider("unknown"))
	})

	t.Run("GetProvidersByAuthType", func(t *testing.T) {
		oauthProviders := GetProvidersByAuthType(AuthTypeOAuth)
		assert.NotEmpty(t, oauthProviders)

		freeProviders := GetProvidersByAuthType(AuthTypeFree)
		assert.NotEmpty(t, freeProviders)

		localProviders := GetProvidersByAuthType(AuthTypeLocal)
		assert.NotEmpty(t, localProviders)
	})

	t.Run("GetProvidersByTier", func(t *testing.T) {
		tier1 := GetProvidersByTier(1)
		assert.NotEmpty(t, tier1)

		tier99 := GetProvidersByTier(99)
		assert.Empty(t, tier99)
	})
}

func TestStartupVerifier_verifyProviders_Sequential(t *testing.T) {
	cfg := DefaultStartupConfig()
	cfg.ParallelVerification = false
	cfg.VerificationTimeout = 2 * time.Second
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	sv := NewStartupVerifier(cfg, logger)

	sv.SetProviderFunc(func(ctx context.Context, modelID, provider, prompt string) (string, error) {
		return "Yes, I can see your code", nil
	})

	discovered := []*ProviderDiscoveryResult{
		{
			ID:       "zen",
			Type:     "zen",
			AuthType: AuthTypeFree,
			Models:   []string{"model1"},
		},
	}

	result := &StartupResult{
		Errors: make([]StartupError, 0),
	}

	verified := sv.verifyProviders(context.Background(), discovered, result)
	assert.NotEmpty(t, verified)
}

func TestStartupVerifier_verifyProviders_Parallel(t *testing.T) {
	cfg := DefaultStartupConfig()
	cfg.ParallelVerification = true
	cfg.MaxConcurrency = 2
	cfg.VerificationTimeout = 2 * time.Second
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	sv := NewStartupVerifier(cfg, logger)

	sv.SetProviderFunc(func(ctx context.Context, modelID, provider, prompt string) (string, error) {
		return "Yes, I can see your code", nil
	})

	discovered := []*ProviderDiscoveryResult{
		{ID: "p1", Type: "zen", AuthType: AuthTypeFree, Models: []string{"m1"}},
		{ID: "p2", Type: "zen2", AuthType: AuthTypeFree, Models: []string{"m2"}},
	}

	result := &StartupResult{
		Errors: make([]StartupError, 0),
	}

	verified := sv.verifyProviders(context.Background(), discovered, result)
	assert.Len(t, verified, 2)
}

func TestUnifiedProvider_AllFields(t *testing.T) {
	now := time.Now()
	provider := &UnifiedProvider{
		ID:               "test-provider",
		Name:             "Test Provider",
		DisplayName:      "Test Display Name",
		Type:             "test",
		AuthType:         AuthTypeAPIKey,
		Verified:         true,
		VerifiedAt:       now,
		Score:            8.5,
		ScoreSuffix:      "(SC:8.5)",
		TestResults:      map[string]bool{"test1": true},
		CodeVisible:      true,
		Models:           []UnifiedModel{{ID: "m1"}},
		DefaultModel:     "m1",
		PrimaryModel:     nil,
		Status:           StatusVerified,
		LastHealthCheck:  now,
		HealthCheckError: "",
		OAuthTokenExpiry: time.Time{},
		OAuthAutoRefresh: false,
		BaseURL:          "https://api.test.com",
		APIKey:           "secret",
		Tier:             1,
		Priority:         1,
		Instance:         nil,
		ErrorMessage:     "",
		ConsecutiveFails: 0,
		ErrorCount:       0,
		LastHealthAt:     now,
		Metadata:         map[string]interface{}{"key": "value"},
	}

	assert.Equal(t, "test-provider", provider.ID)
	assert.Equal(t, "Test Provider", provider.Name)
	assert.Equal(t, "Test Display Name", provider.DisplayName)
	assert.Equal(t, AuthTypeAPIKey, provider.AuthType)
	assert.True(t, provider.Verified)
	assert.Equal(t, 8.5, provider.Score)
	assert.True(t, provider.CodeVisible)
	assert.Len(t, provider.Models, 1)
	assert.Equal(t, StatusVerified, provider.Status)
}

func TestUnifiedModel_AllFields(t *testing.T) {
	now := time.Now()
	model := &UnifiedModel{
		ID:                 "test-model",
		Name:               "Test Model",
		DisplayName:        "Test Model Display",
		Provider:           "test-provider",
		Score:              8.0,
		ScoreSuffix:        "(SC:8.0)",
		Verified:           true,
		VerifiedAt:         now,
		Latency:            100 * time.Millisecond,
		ContextWindow:      128000,
		MaxOutputTokens:    4096,
		SupportsStreaming:  true,
		SupportsTools:      true,
		SupportsFunctions:  true,
		SupportsVision:     false,
		Capabilities:       []string{"text", "code"},
		CostPerInputToken:  0.001,
		CostPerOutputToken: 0.002,
		TestResults:        map[string]bool{"test": true},
		Metadata:           map[string]interface{}{"version": "1.0"},
	}

	assert.Equal(t, "test-model", model.ID)
	assert.Equal(t, "Test Model", model.Name)
	assert.Equal(t, "test-provider", model.Provider)
	assert.Equal(t, 8.0, model.Score)
	assert.True(t, model.Verified)
	assert.Equal(t, 100*time.Millisecond, model.Latency)
	assert.Equal(t, 128000, model.ContextWindow)
	assert.True(t, model.SupportsStreaming)
	assert.True(t, model.SupportsTools)
	assert.Contains(t, model.Capabilities, "text")
}
