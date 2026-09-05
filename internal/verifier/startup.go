// Package verifier provides integration with LLMsVerifier for model verification,
// scoring, and health monitoring capabilities.
package verifier

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"dev.helix.agent/internal/auth/oauth_credentials"
	"dev.helix.agent/internal/llm"
	"dev.helix.agent/internal/llm/discovery"
	"dev.helix.agent/internal/llm/providers/claude"
	"dev.helix.agent/internal/llm/providers/junie"
	"dev.helix.agent/internal/llm/providers/qwen"
	"dev.helix.agent/internal/llm/providers/zen"
	"digital.vasic.concurrency/pkg/safe"
	"digital.vasic.llmsverifier/api_keys"
	"github.com/HelixDevelopment/helix_agent/Toolkit/Providers/Chutes"
	"github.com/sirupsen/logrus"
)

// helixLLMTLSConfig returns a tls.Config suitable for talking to a local
// HelixLLM instance that uses a self-signed certificate.
//
// Preference order (first match wins — all safe-by-default):
//
//  1. SSL_CERT_FILE env var — system-wide CA bundle path. CLAUDE.md's
//     HelixLLM TLS guide instructs operators to append the HelixLLM cert
//     to this bundle during boot, so most environments trust it natively.
//  2. HELIX_LLM_CERT_PATH env var — explicit override for the HelixLLM
//     server certificate (added to a fresh pool alongside system CAs).
//  3. System trust store only — works when the HelixLLM cert is already
//     trusted by the host.
//
// Only when the operator explicitly opts in via HELIX_LLM_TLS_SKIP_VERIFY=true
// does this function disable verification. This replaces the previous
// unconditional `InsecureSkipVerify: true` which violated
// CLAUDE.md's "NEVER use `curl -sk` or NODE_TLS_REJECT_UNAUTHORIZED=0"
// posture (BUGFIX #16).
func helixLLMTLSConfig() *tls.Config {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: getEnvBoolVerifier("HELIX_LLM_TLS_SKIP_VERIFY", false), //nolint:gosec // env-gated opt-in only
	}
	if cfg.InsecureSkipVerify {
		return cfg
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if caPath := os.Getenv("SSL_CERT_FILE"); caPath != "" {
		if pem, err := os.ReadFile(caPath); err == nil { //nolint:gosec // G304: env-supplied path is operator-controlled
			pool.AppendCertsFromPEM(pem)
		}
	}
	if certPath := os.Getenv("HELIX_LLM_CERT_PATH"); certPath != "" {
		if pem, err := os.ReadFile(certPath); err == nil { //nolint:gosec // G304: env-supplied path is operator-controlled
			pool.AppendCertsFromPEM(pem)
		}
	}
	cfg.RootCAs = pool
	return cfg
}

// getEnvBoolVerifier is a local helper to avoid importing a config package
// that would create a cycle. Matches the semantics of strconv.ParseBool.
func getEnvBoolVerifier(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "t", "true", "y", "yes", "on":
		return true
	case "0", "f", "false", "n", "no", "off":
		return false
	}
	return def
}

// StartupVerifier orchestrates the complete startup verification pipeline
// It is the single source of truth for all LLM provider information
type StartupVerifier struct {
	config          *StartupConfig
	verifierSvc     *VerificationService
	scoringSvc      *ScoringService
	enhancedScoring *EnhancedScoringService // Phase 1: 7-component scoring

	// Subscription detection (3-tier: API → rate limits → static)
	subscriptionDetector *SubscriptionDetector

	// Provider creation functions (dependency injection)
	providerFactory ProviderFactory

	// Instance creator for API key providers (creates LLMProvider for debate team)
	instanceCreator func(providerType, modelID string) llm.LLMProvider

	// OAuth credential reader
	oauthReader *oauth_credentials.OAuthCredentialReader

	// Callback for re-exporting CLI agent configs after verification
	onVerificationComplete func(ctx context.Context, result *StartupResult) error

	// Results (concurrent-safe via safe.Store/Slice; see CONST-029)
	providers       *safe.Store[string, *UnifiedProvider]
	rankedProviders *safe.Slice[*UnifiedProvider]
	debateTeam      *DebateTeamResult

	// State. mu (Pattern Zeta) serialises the compound "initialized +
	// debateTeam + lastVerifyAt" publish barrier in Verify() with all
	// readers in Get*. It no longer pairs with a bare map/slice field —
	// providers and rankedProviders are safe.* now.
	initialized  bool
	lastVerifyAt time.Time
	mu           sync.RWMutex

	log *logrus.Logger
}

// ProviderFactory is a function that creates LLM providers
type ProviderFactory func(providerType string, config ProviderCreateConfig) (llm.LLMProvider, error)

// ProviderCreateConfig contains configuration for creating a provider
type ProviderCreateConfig struct {
	APIKey     string
	BaseURL    string
	Model      string
	OAuthToken string
	Anonymous  bool
}

// NewStartupVerifier creates a new startup verifier
func NewStartupVerifier(cfg *StartupConfig, log *logrus.Logger) *StartupVerifier {
	if cfg == nil {
		cfg = DefaultStartupConfig()
	}
	// Normalize MaxConcurrency: callers who construct a StartupConfig
	// literal (integration tests, embeds) routinely forget this field
	// and leave it at its zero value. A zero-sized semaphore in
	// verifyProviders' parallel path blocks every goroutine forever
	// on `sem <- struct{}{}` — that was the integration-suite hang
	// fixed in 2026-04-11's remediation round.
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = DefaultStartupConfig().MaxConcurrency
	}
	if log == nil {
		log = logrus.New()
	}

	// Initialize services
	verifierCfg := DefaultConfig()
	verifierSvc := NewVerificationService(verifierCfg)
	scoringSvc, err := NewScoringService(verifierCfg)
	if err != nil {
		log.WithError(err).Warn("Failed to create scoring service, using default")
		scoringSvc = nil
	}

	// Initialize enhanced scoring service (Phase 1)
	enhancedScoring := NewEnhancedScoringService(scoringSvc)

	return &StartupVerifier{
		config:               cfg,
		verifierSvc:          verifierSvc,
		scoringSvc:           scoringSvc,
		enhancedScoring:      enhancedScoring,
		subscriptionDetector: NewSubscriptionDetector(log),
		providers:            safe.NewStore[string, *UnifiedProvider](),
		rankedProviders:      safe.NewSlice[*UnifiedProvider](),
		oauthReader:          oauth_credentials.NewOAuthCredentialReader(),
		log:                  log,
	}
}

// SetProviderFactory sets the provider factory function
func (sv *StartupVerifier) SetProviderFactory(factory ProviderFactory) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	sv.providerFactory = factory
}

// SetInstanceCreator sets the function used to create LLMProvider instances for
// API key providers after successful verification. This enables the debate team
// to use all verified providers, not just OAuth ones.
func (sv *StartupVerifier) SetInstanceCreator(fn func(providerType, modelID string) llm.LLMProvider) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	sv.instanceCreator = fn
}

// SetProviderFunc sets the function used by verification service to call LLMs
func (sv *StartupVerifier) SetProviderFunc(fn func(ctx context.Context, modelID, provider, prompt string) (string, error)) {
	sv.verifierSvc.SetProviderFunc(fn)
}

// SetTestMode enables/disables test mode (disables quality validation)
func (sv *StartupVerifier) SetTestMode(enabled bool) {
	sv.verifierSvc.SetTestMode(enabled)
}

// SetOnVerificationComplete sets a callback to be called after verification completes.
// This is used to trigger CLI agent config re-export after scoring is done.
func (sv *StartupVerifier) SetOnVerificationComplete(fn func(ctx context.Context, result *StartupResult) error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	sv.onVerificationComplete = fn
}

// VerifyAllProviders runs the complete startup verification pipeline
func (sv *StartupVerifier) VerifyAllProviders(ctx context.Context) (*StartupResult, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	result := &StartupResult{
		StartedAt: time.Now(),
		Providers: make([]*UnifiedProvider, 0),
		Errors:    make([]StartupError, 0),
	}

	sv.log.Info("Starting LLMsVerifier unified startup verification pipeline")

	// Phase 1: Discover all providers
	sv.log.Info("Phase 1: Discovering providers from environment")
	discovered, err := sv.discoverProviders(ctx)
	if err != nil {
		result.Errors = append(result.Errors, StartupError{
			Phase:       "discovery",
			Error:       err.Error(),
			Recoverable: false,
		})
		return result, fmt.Errorf("provider discovery failed: %w", err)
	}
	result.TotalProviders = len(discovered)
	sv.log.WithField("count", len(discovered)).Info("Discovered providers")

	// Phase 2: Verify all providers in parallel
	sv.log.Info("Phase 2: Verifying providers")
	verified := sv.verifyProviders(ctx, discovered, result)
	sv.log.WithField("verified", len(verified)).Info("Verified providers")

	// Phase 2.5: Detect provider subscriptions
	sv.log.Info("Phase 2.5: Detecting provider subscriptions")
	sv.detectSubscriptions(ctx, verified)

	// Phase 3: Score all verified providers
	sv.log.Info("Phase 3: Scoring providers")
	scored := sv.scoreProviders(ctx, verified)
	sv.log.WithField("scored", len(scored)).Info("Scored providers")

	// Phase 4: Rank providers by score
	sv.log.Info("Phase 4: Ranking providers")
	sv.rankProviders(scored)

	// Phase 5: Select debate team
	sv.log.Info("Phase 5: Selecting AI Debate Team")
	debateTeam, err := sv.selectDebateTeam()
	if err != nil {
		result.Errors = append(result.Errors, StartupError{
			Phase:       "debate_team_selection",
			Error:       err.Error(),
			Recoverable: true,
		})
		sv.log.WithError(err).Warn("Failed to select complete debate team")
	}
	result.DebateTeam = debateTeam
	sv.debateTeam = debateTeam

	// Compile results
	result.CompletedAt = time.Now()
	result.DurationMs = result.CompletedAt.Sub(result.StartedAt).Milliseconds()
	result.RankedProviders = sv.rankedProviders.Snapshot()
	result.Providers = make([]*UnifiedProvider, 0, sv.providers.Len())
	sv.providers.Range(func(_ string, p *UnifiedProvider) bool {
		result.Providers = append(result.Providers, p)
		if p.AuthType == AuthTypeAPIKey {
			result.APIKeyProviders++
		} else if p.AuthType == AuthTypeOAuth {
			result.OAuthProviders++
		} else if p.AuthType == AuthTypeFree || p.AuthType == AuthTypeAnonymous {
			result.FreeProviders++
		}
		if p.Verified {
			result.VerifiedCount++
		} else {
			result.FailedCount++
		}

		// Count subscription types
		if p.Subscription != nil {
			result.SubscriptionDetectedCount++
			switch p.Subscription.Type {
			case SubTypeFree:
				result.FreeProviderCount++
			case SubTypeFreeCredits:
				result.FreeCreditProviderCount++
			case SubTypePayAsYouGo:
				result.PayAsYouGoProviderCount++
			case SubTypeFreeTier:
				result.FreeProviderCount++
			}
		}
		return true
	})

	sv.initialized = true
	sv.lastVerifyAt = time.Now()

	sv.log.WithFields(logrus.Fields{
		"total":    result.TotalProviders,
		"verified": result.VerifiedCount,
		"failed":   result.FailedCount,
		"oauth":    result.OAuthProviders,
		"api_key":  result.APIKeyProviders,
		"free":     result.FreeProviders,
		"duration": fmt.Sprintf("%dms", result.DurationMs),
	}).Info("Startup verification pipeline completed")

	// Call the verification complete callback if set (for CLI agent config re-export)
	if sv.onVerificationComplete != nil {
		sv.log.Info("Calling verification complete callback for CLI agent config re-export")
		if err := sv.onVerificationComplete(ctx, result); err != nil {
			sv.log.WithError(err).Warn("Verification complete callback failed")
		}
	}

	return result, nil
}

// discoverProviders discovers all available providers
func (sv *StartupVerifier) discoverProviders(ctx context.Context) ([]*ProviderDiscoveryResult, error) {
	discovered := make([]*ProviderDiscoveryResult, 0)
	seen := make(map[string]bool)

	faultyKeys, err := api_keys.ReadFaultyAPIKeys()
	if err != nil {
		sv.log.WithField("error", err).Warn("Failed to read faulty API keys")
	}

	unsupportedScanner := api_keys.NewEnvVarScanner()
	unsupportedKeys, err := unsupportedScanner.ScanEnvForUnsupportedKeys()
	if err != nil {
		sv.log.WithField("error", err).Warn("Failed to scan for unsupported API keys")
	}
	for keyName, _ := range unsupportedKeys {
		if err := api_keys.WriteUnsupportedAPIKey(keyName, "Environment variable detected but not recognized as supported provider"); err != nil {
			sv.log.WithField("key", keyName).Warn("Failed to write unsupported API key entry")
		} else {
			sv.log.WithField("key", keyName).Info("Recorded unsupported API key")
		}
	}

	isFaulty := func(providerType string) bool {
		apiKeyName := api_keys.GetProviderAPIKeyName(providerType)
		_, exists := faultyKeys[apiKeyName]
		return exists
	}

	getPriority := func(providerType string) int {
		if isFaulty(providerType) {
			return 1
		}
		return 0
	}

	providerTypes := make([]string, 0, len(SupportedProviders))
	for providerType := range SupportedProviders {
		providerTypes = append(providerTypes, providerType)
	}

	sort.Slice(providerTypes, func(i, j int) bool {
		prioI := getPriority(providerTypes[i])
		prioJ := getPriority(providerTypes[j])
		if prioI != prioJ {
			return prioI < prioJ
		}
		return providerTypes[i] < providerTypes[j]
	})

	sv.log.WithField("total", len(providerTypes)).Info("Provider discovery order determined (faulty keys will be checked last)")

	for _, providerType := range providerTypes {
		if seen[providerType] {
			continue // Already discovered via OAuth
		}

		info := SupportedProviders[providerType]
		for _, envVar := range info.EnvVars {
			apiKey := os.Getenv(envVar)
			if apiKey != "" && !isPlaceholder(apiKey) {
				// Try dynamic model discovery first
				models := info.Models // Default to static list
				if dynamicModels, err := sv.DiscoverModels(ctx, providerType, apiKey); err == nil && len(dynamicModels) > 0 {
					models = dynamicModels
					sv.log.WithFields(logrus.Fields{
						"provider": providerType,
						"count":    len(models),
					}).Info("Using dynamically discovered models")
				} else if len(info.Models) == 0 {
					// If no static models and discovery failed, log warning
					sv.log.WithFields(logrus.Fields{
						"provider": providerType,
						"error":    err,
					}).Warn("No models available (discovery failed and no static fallback)")
				}

				discovered = append(discovered, &ProviderDiscoveryResult{
					ID:          providerType,
					Type:        providerType,
					AuthType:    info.AuthType,
					Discovered:  true,
					Source:      "env",
					Credentials: maskAPIKey(apiKey),
					BaseURL:     info.BaseURL,
					Models:      models,
				})
				seen[providerType] = true
				sv.log.WithFields(logrus.Fields{
					"provider": providerType,
					"env_var":  envVar,
				}).Debug("Discovered API key provider")
				break
			}
		}
	}

	// 3. Discover free providers (always available)
	freeProviders := sv.discoverFreeProviders(ctx)
	for _, p := range freeProviders {
		if !seen[p.Type] {
			discovered = append(discovered, p)
			seen[p.Type] = true
			sv.log.WithFields(logrus.Fields{
				"provider": p.Type,
				"source":   p.Source,
			}).Debug("Discovered free provider")
		}
	}

	return discovered, nil
}

// DiscoverModels dynamically discovers available models for a provider.
//
// This eliminates the stale-list bug class (e.g. Chutes 2026-04-21 —
// commit a7bf125e) where a provider-access registry entry lists model
// IDs that no longer exist upstream, causing verification to probe
// models that 404 even when the API key is perfectly valid.
//
// Provider-specific dynamic discovery (Chutes via Toolkit) is tried first.
// All other providers fall through to the generic 3-tier `internal/llm/discovery`
// pipeline, which queries the provider's own `/v1/models` endpoint (Tier 1),
// falls back to models.dev (Tier 2), and finally to the hardcoded list in
// `ProviderTypeInfo.Models` / `ProviderAccessConfig` (Tier 3).
//
// Returns nil, nil when no dynamic discovery yields models — caller falls
// back to `ProviderTypeInfo.Models` per the contract at the call site.
func (sv *StartupVerifier) DiscoverModels(ctx context.Context, providerType string, apiKey string) ([]string, error) {
	// Provider-specific Toolkit integrations take precedence.
	switch providerType {
	case "chutes":
		// Hard-coded fallback models for Chutes (org/Model format).
		fallbackModels := []string{
			"deepseek-ai/DeepSeek-V3",
			"deepseek-ai/DeepSeek-R1",
		}

		// Try dynamic discovery via Toolkit first.
		chutesDisc := chutes.NewDiscovery(apiKey)
		modelInfos, err := chutesDisc.Discover(ctx)

		if err == nil && len(modelInfos) > 0 {
			models := make([]string, 0, len(modelInfos))
			for _, m := range modelInfos {
				models = append(models, m.ID)
			}
			sv.log.WithField("provider", providerType).WithField("models", len(models)).Info("Discovered models via Toolkit")
			return models, nil
		}

		// Fall through to generic 3-tier discovery below (Tier 1 will hit
		// llm.chutes.ai/v1/models directly).
		sv.log.WithFields(logrus.Fields{
			"provider": providerType,
			"error":    err,
		}).Debug("Chutes Toolkit discovery returned no models; falling back to generic 3-tier")

		return fallbackModels, nil
	}

	// Generic 3-tier discovery for all other providers (and as fallback for
	// provider-specific ones whose Toolkit path returned nothing).
	return sv.discoverModelsGeneric(ctx, providerType, apiKey)
}

// discoverModelsGeneric runs the generic 3-tier `internal/llm/discovery`
// pipeline for a provider using its `ProviderAccessRegistry.ModelsURL` and
// `ProviderTypeInfo.Models` fallback. Returns nil, nil when no dynamic
// source is configured — caller keeps the existing static list.
func (sv *StartupVerifier) discoverModelsGeneric(ctx context.Context, providerType, apiKey string) ([]string, error) {
	access := GetProviderAccessConfig(providerType)
	if access == nil || access.ModelsURL == "" {
		return nil, nil
	}

	cfg := discovery.ProviderConfig{
		ProviderName:   providerType,
		ModelsEndpoint: access.ModelsURL,
		APIKey:         apiKey,
	}

	// Apply provider-specific auth mechanism from the access config.
	if access.PrimaryAuth.HeaderName != "" {
		cfg.AuthHeader = access.PrimaryAuth.HeaderName
		cfg.AuthPrefix = access.PrimaryAuth.HeaderPrefix
	}
	if len(access.PrimaryAuth.ExtraHeaders) > 0 {
		cfg.ExtraHeaders = access.PrimaryAuth.ExtraHeaders
	}

	// Provider-specific response parsers for non-OpenAI-compatible formats.
	switch providerType {
	case "gemini":
		cfg.ResponseParser = discovery.ParseGeminiModelsResponse
	case "cohere":
		cfg.ResponseParser = discovery.ParseCohereModelsResponse
	case "ollama":
		cfg.ResponseParser = discovery.ParseOllamaModelsResponse
	case "replicate":
		cfg.ResponseParser = discovery.ParseReplicateModelsResponse
	case "zai":
		cfg.ResponseParser = discovery.ParseZAIModelsResponse
	}

	// models.dev catalogue hint — use the same key as the provider type.
	cfg.ModelsDevID = providerType

	// Static fallback from SupportedProviders — keeps current behaviour
	// when both Tier 1 and Tier 2 fail.
	if info, ok := SupportedProviders[providerType]; ok && len(info.Models) > 0 {
		cfg.FallbackModels = info.Models
	}

	d := discovery.NewDiscoverer(cfg)
	models := d.DiscoverModels()
	tier := d.GetDiscoveryTier()

	if len(models) == 0 {
		return nil, nil
	}

	sv.log.WithFields(logrus.Fields{
		"provider": providerType,
		"count":    len(models),
		"tier":     tier,
	}).Info("Discovered models via generic 3-tier discovery")

	return models, nil
}

// discoverOAuthProviders discovers OAuth-based providers
func (sv *StartupVerifier) discoverOAuthProviders(ctx context.Context) []*ProviderDiscoveryResult {
	var providers []*ProviderDiscoveryResult

	// Check Claude OAuth
	if sv.isClaudeOAuthEnabled() {
		creds, err := sv.oauthReader.ReadClaudeCredentials()
		if err == nil && creds != nil && creds.ClaudeAiOauth != nil {
			providers = append(providers, &ProviderDiscoveryResult{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
				ID:          "claude",
				Type:        "claude",
				AuthType:    AuthTypeOAuth,
				Discovered:  true,
				Source:      "oauth",
				Credentials: "OAuth (Claude Code)",
				BaseURL:     "https://api.anthropic.com/v1/messages",
				Models:      []string{"claude-opus-4-6", "claude-sonnet-4-6", "claude-opus-4-5-20251101", "claude-sonnet-4-5-20250929", "claude-haiku-4-5-20251001"},
			})
		}
	}

	// Check Qwen OAuth
	if sv.isQwenOAuthEnabled() {
		creds, err := sv.oauthReader.ReadQwenCredentials()
		if err == nil && creds != nil {
			providers = append(providers, &ProviderDiscoveryResult{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
				ID:          "qwen",
				Type:        "qwen",
				AuthType:    AuthTypeOAuth,
				Discovered:  true,
				Source:      "oauth",
				Credentials: "OAuth (Qwen Code)",
				BaseURL:     "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions",
				Models:      []string{"qwen-max", "qwen-plus", "qwen-turbo"},
			})
		}
	}

	// HelixLLM discovery (self-hosted via llama.cpp) — in main discoverProviders.
	// Local-first default (HA-F2-002): ON unless USE_HELIX_LLM is an explicit
	// false. Mirrors services.HelixLLMEnabledDefault(); the local helper is
	// used here because services imports verifier (import cycle).
	if getEnvBoolVerifier("USE_HELIX_LLM", true) {
		helixLLMURL := os.Getenv("HELIX_LLM_ENDPOINT")
		if helixLLMURL == "" {
			helixLLMURL = "https://localhost:8443"
		}

		helixLLMModels := sv.checkHelixLLMHealth(helixLLMURL)
		if len(helixLLMModels) > 0 {
			providers = append(providers, &ProviderDiscoveryResult{
				ID:          "helixllm",
				Type:        "helixllm",
				AuthType:    AuthTypeLocal,
				Discovered:  true,
				Source:      "auto",
				Credentials: "Local",
				BaseURL:     helixLLMURL,
				Models:      helixLLMModels,
			})
			sv.log.WithFields(logrus.Fields{
				"url":    helixLLMURL,
				"models": helixLLMModels,
			}).Info("HelixLLM discovered with available models")
		} else {
			sv.log.WithField("url", helixLLMURL).Warn("HelixLLM enabled but not reachable or no models available")
		}
	}

	return providers
}

// discoverFreeProviders discovers free/anonymous providers
func (sv *StartupVerifier) discoverFreeProviders(ctx context.Context) []*ProviderDiscoveryResult {
	var providers []*ProviderDiscoveryResult

	if !sv.config.EnableFreeProviders {
		return providers
	}

	// Zen is always available (anonymous mode)
	// Updated 2026-01-29: Using dynamic model discovery from Zen API/CLI
	zenModels := zen.DiscoverFreeModels()
	sv.log.WithFields(logrus.Fields{
		"count":  len(zenModels),
		"models": zenModels,
	}).Info("Dynamically discovered Zen models")

	providers = append(providers, &ProviderDiscoveryResult{
		ID:          "zen",
		Type:        "zen",
		AuthType:    AuthTypeFree,
		Discovered:  true,
		Source:      "dynamic_discovery",
		Credentials: "Anonymous",
		BaseURL:     "https://opencode.ai/zen/v1/chat/completions",
		Models:      zenModels,
	})

	// Check if Ollama is explicitly enabled
	ollamaEnabled := os.Getenv("OLLAMA_ENABLED")
	if ollamaEnabled == "true" {
		ollamaURL := os.Getenv("OLLAMA_BASE_URL")
		if ollamaURL == "" {
			ollamaURL = "http://localhost:11434"
		}

		// Health check Ollama - only add if it's actually running
		ollamaModels := sv.checkOllamaHealth(ollamaURL)
		if len(ollamaModels) > 0 {
			providers = append(providers, &ProviderDiscoveryResult{
				ID:          "ollama",
				Type:        "ollama",
				AuthType:    AuthTypeLocal,
				Discovered:  true,
				Source:      "auto",
				Credentials: "Local",
				BaseURL:     ollamaURL,
				Models:      ollamaModels,
			})
			sv.log.WithFields(logrus.Fields{
				"url":    ollamaURL,
				"models": ollamaModels,
			}).Info("Ollama discovered with available models (OLLAMA_ENABLED=true)")
		} else {
			sv.log.WithField("url", ollamaURL).Warn("Ollama enabled but not running or no models available")
		}
	} else {
		sv.log.Info("Ollama disabled (set OLLAMA_ENABLED=true to enable)")
	}

	return providers
}

// checkOllamaHealth checks if Ollama is running and returns available models
func (sv *StartupVerifier) checkOllamaHealth(baseURL string) []string {
	// Ollama API endpoint for listing models
	tagsURL := strings.TrimSuffix(baseURL, "/") + "/api/tags"

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	//nolint:gosec // G704: outbound to configured Ollama provider URL from the verifier — intentional; SSRF defence applies only to user-supplied URLs
	resp, err := client.Get(tagsURL)
	if err != nil {
		sv.log.WithError(err).Debug("Failed to connect to Ollama")
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		sv.log.WithField("status", resp.StatusCode).Debug("Ollama returned non-OK status")
		return nil
	}

	// Parse the response to get model names
	var tagsResp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tagsResp); err != nil {
		sv.log.WithError(err).Debug("Failed to decode Ollama response")
		// If we can't parse but Ollama is running, return default model
		return []string{"llama3.2"}
	}

	if len(tagsResp.Models) == 0 {
		sv.log.Debug("Ollama running but no models available")
		return nil
	}

	// Extract model names
	models := make([]string, 0, len(tagsResp.Models))
	for _, m := range tagsResp.Models {
		if m.Name != "" {
			models = append(models, m.Name)
		}
	}

	return models
}

// checkHelixLLMHealth checks if HelixLLM is running and returns available models
func (sv *StartupVerifier) checkHelixLLMHealth(baseURL string) []string {
	modelsURL := strings.TrimSuffix(baseURL, "/") + "/v1/models"

	// Secure by default: load SSL_CERT_FILE or HELIX_LLM_CERT_PATH; only skip
	// verification when explicitly opted in via HELIX_LLM_TLS_SKIP_VERIFY=true.
	// See helixLLMTLSConfig above and BUGFIXES.md Issue #16.
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: helixLLMTLSConfig(),
		},
	}

	//nolint:gosec // G704: outbound to configured HelixLLM provider URL from the verifier — intentional; SSRF defence applies only to user-supplied URLs
	resp, err := client.Get(modelsURL)
	if err != nil {
		sv.log.WithError(err).Warn("Failed to connect to HelixLLM")
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		sv.log.WithField("status", resp.StatusCode).Debug("HelixLLM returned non-OK status")
		return nil
	}

	var modelsResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		sv.log.WithError(err).Debug("Failed to decode HelixLLM models response")
		return []string{"model.gguf"}
	}

	if len(modelsResp.Data) == 0 {
		sv.log.Debug("HelixLLM running but no models listed")
		return []string{"model.gguf"}
	}

	models := make([]string, 0, len(modelsResp.Data))
	for _, m := range modelsResp.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}

	return models
}

// verifyProviders verifies all discovered providers
func (sv *StartupVerifier) verifyProviders(ctx context.Context, discovered []*ProviderDiscoveryResult, result *StartupResult) []*UnifiedProvider {
	verified := make([]*UnifiedProvider, 0)

	if sv.config.ParallelVerification {
		// Parallel verification
		var wg sync.WaitGroup
		var mu sync.Mutex
		// Defensive: a zero or negative MaxConcurrency would create an
		// unbuffered semaphore that every goroutine blocks on forever.
		// NewStartupVerifier already normalises this, but any test that
		// mutates the config after construction should still be safe.
		maxConcurrency := sv.config.MaxConcurrency
		if maxConcurrency <= 0 {
			maxConcurrency = DefaultStartupConfig().MaxConcurrency
		}
		sem := make(chan struct{}, maxConcurrency)

		for _, disc := range discovered {
			wg.Add(1)
			go func(d *ProviderDiscoveryResult) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				provider, err := sv.verifyProvider(ctx, d)
				mu.Lock()
				defer mu.Unlock()

				if err != nil {
					result.Errors = append(result.Errors, StartupError{
						Provider:    d.Type,
						Phase:       "verification",
						Error:       err.Error(),
						Recoverable: true,
					})
					sv.log.WithError(err).WithField("provider", d.Type).Warn("Provider verification failed")
				}
				if provider != nil {
					verified = append(verified, provider)
					sv.providers.Put(provider.ID, provider)
				}
			}(disc)
		}
		wg.Wait()
	} else {
		// Sequential verification
		for _, disc := range discovered {
			provider, err := sv.verifyProvider(ctx, disc)
			if err != nil {
				result.Errors = append(result.Errors, StartupError{
					Provider:    disc.Type,
					Phase:       "verification",
					Error:       err.Error(),
					Recoverable: true,
				})
				sv.log.WithError(err).WithField("provider", disc.Type).Warn("Provider verification failed")
			}
			if provider != nil {
				verified = append(verified, provider)
				sv.providers.Put(provider.ID, provider)
			}
		}
	}

	return verified
}

// verifyProvider verifies a single provider
func (sv *StartupVerifier) verifyProvider(ctx context.Context, disc *ProviderDiscoveryResult) (*UnifiedProvider, error) {
	// Create timeout context
	verifyCtx, cancel := context.WithTimeout(ctx, sv.config.VerificationTimeout)
	defer cancel()

	provider := &UnifiedProvider{
		ID:       disc.ID,
		Name:     disc.Type,
		Type:     disc.Type,
		AuthType: disc.AuthType,
		BaseURL:  disc.BaseURL,
		Status:   StatusUnknown,
		Models:   make([]UnifiedModel, 0),
	}

	// Set default model
	if len(disc.Models) > 0 {
		provider.DefaultModel = disc.Models[0]
	}

	// Special handling by auth type
	switch disc.AuthType {
	case AuthTypeOAuth:
		return sv.verifyOAuthProvider(verifyCtx, provider, disc)
	case AuthTypeFree, AuthTypeAnonymous:
		return sv.verifyFreeProvider(verifyCtx, provider, disc)
	case AuthTypeLocal:
		return sv.verifyLocalProvider(verifyCtx, provider, disc)
	default:
		return sv.verifyAPIKeyProvider(verifyCtx, provider, disc)
	}
}

// verifyOAuthProvider verifies an OAuth-based provider
// IMPORTANT: OAuth tokens from CLI tools (Claude Code, Qwen CLI) are often product-restricted
// and cannot be used for general API calls. The TrustOAuthOnFailure setting allows these
// providers to be trusted even when API verification fails, as the tokens ARE valid for their
// intended use (the CLI tools themselves route through proper authenticated channels).
func (sv *StartupVerifier) verifyOAuthProvider(ctx context.Context, provider *UnifiedProvider, disc *ProviderDiscoveryResult) (*UnifiedProvider, error) {
	sv.log.WithField("provider", provider.Type).Debug("Verifying OAuth provider")

	// Run verification through LLMsVerifier
	modelID := provider.DefaultModel
	if modelID == "" && len(disc.Models) > 0 {
		modelID = disc.Models[0]
	}

	result, err := sv.verifierSvc.VerifyModel(ctx, modelID, provider.Type)

	// Check if verification failed - either explicit error OR result.Verified == false
	// VerifyModel often returns err=nil but sets result.Verified=false when API calls fail
	verificationFailed := err != nil || (result != nil && !result.Verified)

	if verificationFailed {
		// OAuth providers are trusted even if verification fails
		// (CLI tokens are product-restricted and may not work for general API calls,
		// but they ARE valid for use with CLI agent plugins that route through their services)
		if sv.config.TrustOAuthOnFailure {
			errorMsg := "verification returned false"
			if err != nil {
				errorMsg = err.Error()
			} else if result != nil && result.ErrorMessage != "" {
				errorMsg = result.ErrorMessage
			}
			sv.log.WithFields(logrus.Fields{
				"provider": provider.Type,
				"error":    errorMsg,
				"reason":   "OAuth tokens are product-restricted but valid for CLI agent routing",
			}).Warn("OAuth verification failed, trusting CLI credentials")

			provider.Verified = true
			provider.Status = StatusHealthy
			provider.Score = 8.0 + sv.config.OAuthPriorityBoost // High default for OAuth
			provider.VerifiedAt = time.Now()
			provider.TestResults = map[string]bool{"oauth_trusted": true}
			provider.ErrorMessage = "" // Clear any error since we're trusting

			// Build models list - all models are considered verified via trust
			for _, modelID := range disc.Models {
				provider.Models = append(provider.Models, UnifiedModel{
					ID:       modelID,
					Name:     modelID,
					Provider: provider.Type,
					Verified: true,
					Score:    provider.Score,
				})
			}

			// CRITICAL: Create the CLI provider instance for debate team usage
			// Without this, provider.Instance is nil and debate responses fail
			provider.Instance = sv.createOAuthProviderInstance(provider.Type, provider.DefaultModel)
			if provider.Instance != nil {
				sv.log.WithFields(logrus.Fields{
					"provider": provider.Type,
					"model":    provider.DefaultModel,
				}).Info("Created CLI provider instance for OAuth provider")
			} else {
				sv.log.WithField("provider", provider.Type).Warn("Failed to create CLI provider instance - debate may fail")
			}

			return provider, nil
		}
		if err != nil {
			return nil, fmt.Errorf("OAuth provider verification failed: %w", err)
		}
		return nil, fmt.Errorf("OAuth provider verification failed: %s", result.ErrorMessage)
	}

	provider.Verified = result.Verified
	provider.VerifiedAt = time.Now()
	provider.Score = result.OverallScore/10.0 + sv.config.OAuthPriorityBoost // OAuth gets priority boost
	provider.ScoreSuffix = result.ScoreSuffix
	provider.CodeVisible = result.CodeVisible
	provider.TestResults = result.TestsMap

	if provider.Verified {
		provider.Status = StatusHealthy
	} else {
		provider.Status = StatusUnhealthy
		provider.ErrorMessage = result.ErrorMessage
	}

	// Populate failure tracking details from verification result
	populateFailureDetails(provider, result)

	// Build models list
	for _, modelID := range disc.Models {
		provider.Models = append(provider.Models, UnifiedModel{
			ID:       modelID,
			Name:     modelID,
			Provider: provider.Type,
			Verified: provider.Verified,
			Score:    provider.Score,
		})
	}

	// CRITICAL: Create the CLI provider instance for debate team usage
	// This enables OAuth providers to work in the debate team
	if provider.Verified {
		provider.Instance = sv.createOAuthProviderInstance(provider.Type, provider.DefaultModel)
		if provider.Instance != nil {
			sv.log.WithFields(logrus.Fields{
				"provider": provider.Type,
				"model":    provider.DefaultModel,
			}).Info("Created CLI provider instance for verified OAuth provider")
		}
	}

	return provider, nil
}

// createOAuthProviderInstance creates the appropriate CLI provider for an OAuth provider type
// This is CRITICAL for the debate team to work - without the Instance field set,
// debate responses will fail with "Unable to provide analysis"
func (sv *StartupVerifier) createOAuthProviderInstance(providerType, defaultModel string) llm.LLMProvider {
	switch providerType {
	case "claude":
		// Create Claude CLI provider for OAuth
		cliProvider := claude.NewClaudeCLIProviderWithModel(defaultModel)
		if cliProvider.IsCLIAvailable() {
			sv.log.WithField("provider", "claude").Debug("Claude CLI provider created successfully")
			return cliProvider
		}
		sv.log.Warn("Claude CLI not available - cannot create provider instance")
		return nil

	case "qwen":
		// Try ACP first (more powerful), then fall back to CLI
		if qwen.CanUseQwenACP() {
			acpProvider := qwen.NewQwenACPProviderWithModel(defaultModel)
			if acpProvider.IsAvailable() {
				sv.log.WithField("provider", "qwen").Debug("Qwen ACP provider created successfully")
				return acpProvider
			}
		}
		// Fall back to CLI provider
		cliProvider := qwen.NewQwenCLIProviderWithModel(defaultModel)
		if cliProvider.IsCLIAvailable() {
			sv.log.WithField("provider", "qwen").Debug("Qwen CLI provider created successfully")
			return cliProvider
		}
		sv.log.Warn("Qwen CLI/ACP not available - cannot create provider instance")
		return nil

	case "zen":
		// Create Zen HTTP provider (uses OpenCode HTTP server)
		httpProvider := zen.NewZenHTTPProviderWithModel(defaultModel)
		sv.log.WithField("provider", "zen").Debug("Zen HTTP provider created successfully")
		return httpProvider

	case "junie":
		// Create Junie provider (uses CLI or ACP)
		junieProvider := junie.NewJunieProviderWithModel(defaultModel)
		sv.log.WithField("provider", "junie").Debug("Junie provider created successfully")
		return junieProvider

	default:
		sv.log.WithField("provider", providerType).Warn("Unknown OAuth provider type - cannot create instance")
		return nil
	}
}

// verifyFreeProvider verifies a free/anonymous provider
// IMPORTANT: Each model is individually tested with strict validation to detect
// canned error responses like "Unable to provide analysis at this time".
// Only models that pass actual completion tests are marked as verified.
func (sv *StartupVerifier) verifyFreeProvider(ctx context.Context, provider *UnifiedProvider, disc *ProviderDiscoveryResult) (*UnifiedProvider, error) {
	sv.log.WithField("provider", provider.Type).Debug("Verifying free provider with strict model testing")

	// Initialize - we'll only mark as verified if at least one model passes
	provider.Verified = false
	provider.Status = StatusFailed
	provider.Score = sv.config.FreeProviderBaseScore
	provider.VerifiedAt = time.Now()
	provider.TestResults = map[string]bool{}

	verifiedModelCount := 0
	totalModels := len(disc.Models)

	// Verify each model individually with strict validation
	for _, modelID := range disc.Models {
		sv.log.WithFields(logrus.Fields{
			"provider": provider.Type,
			"model":    modelID,
		}).Debug("Testing model for canned error responses")

		// Try full verification with model completion test
		result, err := sv.verifierSvc.VerifyModel(ctx, modelID, provider.Type)

		modelVerified := false
		modelScore := sv.config.FreeProviderBaseScore

		if err != nil {
			sv.log.WithFields(logrus.Fields{
				"provider": provider.Type,
				"model":    modelID,
				"error":    err.Error(),
			}).Debug("Model verification failed")
		} else if result != nil {
			// Check if model passes response quality validation
			isCannedError, _ := IsCannedErrorResponse(result.LastResponse)
			if result.Verified && !isCannedError {
				modelVerified = true
				modelScore = result.OverallScore / 10.0
				if modelScore < sv.config.FreeProviderBaseScore {
					modelScore = sv.config.FreeProviderBaseScore
				}
				verifiedModelCount++
				provider.TestResults[modelID] = true
				sv.log.WithFields(logrus.Fields{
					"provider": provider.Type,
					"model":    modelID,
					"score":    modelScore,
				}).Info("Model passed verification")
			} else {
				sv.log.WithFields(logrus.Fields{
					"provider":     provider.Type,
					"model":        modelID,
					"verified":     result.Verified,
					"lastResponse": result.LastResponse,
				}).Warn("Model failed quality validation - possible canned error response")
			}
		}

		// Add model to list with actual verification status
		provider.Models = append(provider.Models, UnifiedModel{
			ID:       modelID,
			Name:     modelID,
			Provider: provider.Type,
			Verified: modelVerified,
			Score:    modelScore,
		})
	}

	// Provider is only verified if at least one model passes
	if verifiedModelCount > 0 {
		provider.Verified = true
		provider.Status = StatusHealthy
		// Calculate average score from verified models
		var totalScore float64
		var scoreCount int
		for _, m := range provider.Models {
			if m.Verified {
				totalScore += m.Score
				scoreCount++
			}
		}
		if scoreCount > 0 {
			provider.Score = totalScore / float64(scoreCount)
		}
		// Create provider instance for debate team usage
		if provider.Instance == nil && sv.instanceCreator != nil {
			provider.Instance = sv.instanceCreator(provider.Type, provider.DefaultModel)
			if provider.Instance != nil {
				sv.log.WithFields(logrus.Fields{
					"provider": provider.Type,
					"model":    provider.DefaultModel,
				}).Debug("Created provider instance for free provider")
			}
		}
	} else {
		provider.Status = StatusFailed
		provider.HealthCheckError = fmt.Sprintf("No models passed verification (0/%d)", totalModels)
		provider.FailureReason = fmt.Sprintf("no models passed verification (0/%d tested)", totalModels)
		provider.FailureCategory = FailureCategoryCannedResponse
	}

	sv.log.WithFields(logrus.Fields{
		"provider":        provider.Type,
		"verified":        provider.Verified,
		"verified_models": verifiedModelCount,
		"total_models":    totalModels,
		"score":           provider.Score,
	}).Info("Free provider verification complete")

	return provider, nil
}

// verifyLocalProvider verifies a local provider (Ollama)
func (sv *StartupVerifier) verifyLocalProvider(ctx context.Context, provider *UnifiedProvider, disc *ProviderDiscoveryResult) (*UnifiedProvider, error) {
	sv.log.WithField("provider", provider.Type).Debug("Verifying local provider")

	// Local providers have lowest priority
	provider.Verified = true
	provider.Status = StatusHealthy
	provider.Score = 5.0 // Low score, only fallback
	provider.VerifiedAt = time.Now()
	provider.TestResults = map[string]bool{"local_provider": true}
	provider.Priority = 20 // Lowest priority

	// Build models list
	for _, modelID := range disc.Models {
		provider.Models = append(provider.Models, UnifiedModel{
			ID:       modelID,
			Name:     modelID,
			Provider: provider.Type,
			Verified: true,
			Score:    provider.Score,
		})
	}

	return provider, nil
}

// verifyAPIKeyProvider verifies an API key-based provider
// IMPORTANT: Each model should be individually tested to ensure availability.
// Some providers list models in /v1/models that aren't actually available for all accounts.
func (sv *StartupVerifier) verifyAPIKeyProvider(ctx context.Context, provider *UnifiedProvider, disc *ProviderDiscoveryResult) (*UnifiedProvider, error) {
	sv.log.WithField("provider", provider.Type).Debug("Verifying API key provider with individual model testing")

	// Initialize provider
	provider.Verified = false
	provider.Status = StatusUnhealthy
	provider.Score = 0
	provider.TestResults = make(map[string]bool)

	verifiedModelCount := 0
	skippedDueToDeadline := 0
	var lastVerifyErr error
	var lastResult *ServiceVerificationResult

	// Test each model individually (like verifyFreeProvider does).
	//
	// Drainage report 2026-04-25 Finding #3: providers with many models
	// (e.g. Mistral with 55) exhaust the parent context's VerificationTimeout
	// before iterating the full list. Without the deadline check below, the
	// remaining models hit ctx.Err() inside the underlying HTTP client and
	// log misleading "API call failed duration=2µs error=context deadline
	// exceeded" messages — those weren't real API failures, just
	// post-deadline iterations. Breaking cleanly produces ONE summary log
	// instead of dozens of confusing per-model failures.
	for _, modelID := range disc.Models {
		if ctx.Err() != nil {
			skippedDueToDeadline = len(disc.Models) - len(provider.Models)
			break
		}
		sv.log.WithFields(logrus.Fields{
			"provider": provider.Type,
			"model":    modelID,
		}).Debug("Testing individual model")

		result, err := sv.verifierSvc.VerifyModel(ctx, modelID, provider.Type)
		if err != nil {
			sv.log.WithFields(logrus.Fields{
				"provider": provider.Type,
				"model":    modelID,
				"error":    err.Error(),
			}).Warn("Model verification failed")
			lastVerifyErr = err

			// Add model as unverified
			provider.Models = append(provider.Models, UnifiedModel{
				ID:       modelID,
				Name:     modelID,
				Provider: provider.Type,
				Verified: false,
				Score:    0,
			})
			provider.TestResults[modelID] = false
			continue
		}

		// Check if model actually passed verification
		if result != nil && result.Verified {
			verifiedModelCount++
			lastResult = result

			// Add model as verified
			provider.Models = append(provider.Models, UnifiedModel{
				ID:       modelID,
				Name:     modelID,
				Provider: provider.Type,
				Verified: true,
				Score:    result.OverallScore / 10.0,
			})
			provider.TestResults[modelID] = true

			// Set default model to first verified model
			if provider.DefaultModel == "" {
				provider.DefaultModel = modelID
			}

			sv.log.WithFields(logrus.Fields{
				"provider": provider.Type,
				"model":    modelID,
				"score":    result.OverallScore / 10.0,
			}).Info("Model verified successfully")
		} else {
			// Model returned but not verified (possible canned response or other issue)
			provider.Models = append(provider.Models, UnifiedModel{
				ID:       modelID,
				Name:     modelID,
				Provider: provider.Type,
				Verified: false,
				Score:    0,
			})
			provider.TestResults[modelID] = false

			reason := "verification returned false"
			if result != nil && result.ErrorMessage != "" {
				reason = result.ErrorMessage
			}
			sv.log.WithFields(logrus.Fields{
				"provider": provider.Type,
				"model":    modelID,
				"reason":   reason,
			}).Warn("Model not verified")
		}
	}

	// Surface the deadline-skip count once (rather than logging per-model
	// "API call failed" for every skipped model) — drainage report Finding #3.
	if skippedDueToDeadline > 0 {
		sv.log.WithFields(logrus.Fields{
			"provider":           provider.Type,
			"skipped_count":      skippedDueToDeadline,
			"verified_count":     verifiedModelCount,
			"total_models":       len(disc.Models),
			"verification_limit": sv.config.VerificationTimeout,
		}).Warn("Model verification budget exhausted; remaining models skipped — consider raising VerificationTimeout for providers with many models")
	}

	// Provider is only verified if at least one model passes
	if verifiedModelCount > 0 {
		provider.Verified = true
		provider.Status = StatusHealthy
		provider.VerifiedAt = time.Now()

		// Calculate average score from verified models
		var totalScore float64
		var scoreCount int
		for _, m := range provider.Models {
			if m.Verified {
				totalScore += m.Score
				scoreCount++
			}
		}
		if scoreCount > 0 {
			provider.Score = totalScore / float64(scoreCount)
		}

		// Use last verified result for additional fields
		if lastResult != nil {
			provider.CodeVisible = lastResult.CodeVisible
		}

		// Create provider instance for debate team usage
		if sv.instanceCreator != nil && provider.DefaultModel != "" {
			provider.Instance = sv.instanceCreator(provider.Type, provider.DefaultModel)
			if provider.Instance != nil {
				sv.log.WithFields(logrus.Fields{
					"provider": provider.Type,
					"model":    provider.DefaultModel,
				}).Debug("Created provider instance for API key provider")
			}
		}

		sv.log.WithFields(logrus.Fields{
			"provider":        provider.Type,
			"verified":        provider.Verified,
			"verified_models": verifiedModelCount,
			"total_models":    len(disc.Models),
			"score":           provider.Score,
		}).Info("API key provider verification complete")

		apiKeyName := api_keys.GetProviderAPIKeyName(provider.Type)
		if err := api_keys.RemoveFaultyAPIKey(apiKeyName); err != nil {
			sv.log.WithField("error", err).Warn("Failed to remove faulty API key entry")
		} else {
			sv.log.WithField("api_key", apiKeyName).Info("Removed previously faulty API key (now verified)")
		}
	} else {
		// No models passed verification
		provider.Status = StatusFailed
		if lastVerifyErr != nil {
			provider.ErrorMessage = lastVerifyErr.Error()
			provider.FailureReason = fmt.Sprintf("all models failed verification: %s", lastVerifyErr.Error())
		} else {
			provider.ErrorMessage = "no models passed verification"
			provider.FailureReason = fmt.Sprintf("no models passed verification (0/%d tested)", len(disc.Models))
		}
		provider.FailureCategory = FailureCategoryAPIError

		sv.log.WithFields(logrus.Fields{
			"provider":     provider.Type,
			"total_models": len(disc.Models),
			"error":        provider.ErrorMessage,
		}).Warn("API key provider verification failed - no verified models")

		apiKeyName := api_keys.GetProviderAPIKeyName(provider.Type)
		failureReason := provider.FailureReason
		if failureReason == "" {
			failureReason = "no models passed verification"
		}
		if err := api_keys.WriteFaultyAPIKey(apiKeyName, failureReason); err != nil {
			sv.log.WithField("error", err).Warn("Failed to write faulty API key entry")
		} else {
			sv.log.WithField("api_key", apiKeyName).Info("Recorded faulty API key")
		}
	}

	return provider, nil
}

// scoreProviders calculates scores for all providers using LLMsVerifier
// Phase 1: Uses enhanced 7-component scoring algorithm
func (sv *StartupVerifier) scoreProviders(ctx context.Context, providers []*UnifiedProvider) []*UnifiedProvider {
	for _, p := range providers {
		// Use enhanced scoring for each model if enhanced scoring is available
		if sv.enhancedScoring != nil {
			for i := range p.Models {
				model := &p.Models[i]
				enhancedResult, err := sv.enhancedScoring.CalculateEnhancedScore(ctx, model, p)
				if err == nil {
					// Update model with enhanced score
					model.Score = enhancedResult.OverallScore
					model.ScoreSuffix = enhancedResult.ScoreSuffix
					if model.Metadata == nil {
						model.Metadata = make(map[string]interface{})
					}
					model.Metadata["confidence_score"] = enhancedResult.ConfidenceScore
					model.Metadata["diversity_bonus"] = enhancedResult.DiversityBonus
					model.Metadata["specialization"] = enhancedResult.SpecializationTag
					model.Metadata["scoring_components"] = enhancedResult.Components

					sv.log.WithFields(logrus.Fields{
						"provider":       p.ID,
						"model":          model.ID,
						"score":          enhancedResult.OverallScore,
						"specialization": enhancedResult.SpecializationTag,
						"confidence":     enhancedResult.ConfidenceScore,
					}).Debug("Enhanced scoring calculated for model")
				}
			}

			// Update provider score to be the average of its models
			if len(p.Models) > 0 {
				var totalScore float64
				for _, m := range p.Models {
					totalScore += m.Score
				}
				p.Score = totalScore / float64(len(p.Models))
			}
		} else {
			// Fallback to basic scoring
			if p.Verified && p.Status == StatusHealthy {
				p.Score += 0.5
			}
		}

		// Cap score at 10.0
		if p.Score > 10.0 {
			p.Score = 10.0
		}
	}
	return providers
}

// rankProviders ranks providers by score (highest first)
// IMPORTANT: NO OAuth priority - all providers sorted purely by score (highest first)
func (sv *StartupVerifier) rankProviders(providers []*UnifiedProvider) {
	// Sort by score descending (highest first) - NO OAuth priority
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].Score > providers[j].Score
	})

	sv.rankedProviders.Replace(providers)
}

// selectDebateTeam selects the 15 LLMs for the AI debate team
// This function collects ALL LLMs sorted by score and populates all 15 positions
// (5 primary + 2-4 fallbacks each = up to 25 LLMs). If there are fewer unique LLMs available,
// the strongest LLMs are REUSED to fill all positions.
// IMPORTANT: NO OAuth priority - all providers sorted purely by score (highest first).
func (sv *StartupVerifier) selectDebateTeam() (*DebateTeamResult, error) {
	ranked := sv.rankedProviders.Snapshot()
	if len(ranked) == 0 {
		return nil, fmt.Errorf("no verified providers available for debate team")
	}

	team := &DebateTeamResult{
		Positions:     make([]*DebatePosition, sv.config.PositionCount),
		TotalLLMs:     sv.config.DebateTeamSize,
		MinScore:      sv.config.MinScore,
		SortedByScore: true, // NO OAuth priority - pure score-based sorting
		LLMReuseCount: 0,
		SelectedAt:    time.Now(),
	}

	roles := []string{"analyst", "proposer", "critic", "synthesis", "mediator"}

	// Collect ALL LLMs from ranked providers, sorted by score (already ranked)
	var allLLMs []*DebateLLM
	for _, provider := range ranked {
		if !provider.Verified || provider.Score < sv.config.MinScore {
			continue
		}

		for _, model := range provider.Models {
			allLLMs = append(allLLMs, &DebateLLM{
				Provider:     provider.Type,
				ProviderType: provider.Type,
				ModelID:      model.ID,
				ModelName:    model.Name,
				AuthType:     provider.AuthType,
				Score:        provider.Score,
				Verified:     provider.Verified,
				IsOAuth:      provider.AuthType == AuthTypeOAuth,
			})
		}
	}

	// Need at least 1 LLM to build a team
	if len(allLLMs) == 0 {
		return nil, fmt.Errorf("no verified LLMs available for debate team")
	}

	sv.log.WithField("available_llms", len(allLLMs)).Info("Building debate team from available LLMs (score-based only)")

	// Sort all LLMs PURELY by score descending (NO OAuth priority)
	sort.Slice(allLLMs, func(i, j int) bool {
		return allLLMs[i].Score > allLLMs[j].Score
	})

	// Helper function to get LLM at position with wraparound (for reuse)
	getLLMAtPosition := func(index int) *DebateLLM {
		// Wrap around to reuse strongest LLMs when index exceeds available LLMs
		wrappedIndex := index % len(allLLMs)
		original := allLLMs[wrappedIndex]
		// Create a copy to avoid modifying the original (independent instance)
		return &DebateLLM{
			Provider:     original.Provider,
			ProviderType: original.ProviderType,
			ModelID:      original.ModelID,
			ModelName:    original.ModelName,
			AuthType:     original.AuthType,
			Score:        original.Score,
			Verified:     original.Verified,
			IsOAuth:      original.IsOAuth,
		}
	}

	// Assign ALL positions using strongest LLMs with reuse
	// Each position has: 1 primary + FallbacksPerPosition fallbacks (2-4)
	llmIndex := 0
	reuseCount := 0
	for i := 0; i < sv.config.PositionCount; i++ {
		position := &DebatePosition{
			Position:  i + 1,
			Role:      roles[i],
			Fallbacks: make([]*DebateLLM, 0, sv.config.FallbacksPerPosition),
		}

		// Assign primary (strongest LLM available or reused)
		if llmIndex >= len(allLLMs) {
			reuseCount++
		}
		position.Primary = getLLMAtPosition(llmIndex)
		llmIndex++

		// Assign 2-4 fallbacks based on config
		for j := 0; j < sv.config.FallbacksPerPosition; j++ {
			if llmIndex >= len(allLLMs) {
				reuseCount++
			}
			position.Fallbacks = append(position.Fallbacks, getLLMAtPosition(llmIndex))
			llmIndex++
		}

		team.Positions[i] = position
	}

	// Count total LLMs selected
	totalSelected := 0
	for _, pos := range team.Positions {
		if pos.Primary != nil {
			totalSelected++
		}
		totalSelected += len(pos.Fallbacks)
	}
	team.TotalLLMs = totalSelected
	team.LLMReuseCount = reuseCount

	sv.log.WithFields(logrus.Fields{
		"total_llms":      totalSelected,
		"unique_llms":     len(allLLMs),
		"reuse_count":     reuseCount,
		"reuse_enabled":   reuseCount > 0,
		"strongest_llm":   allLLMs[0].ModelID,
		"strongest_score": allLLMs[0].Score,
		"sorting_method":  "score_only",
	}).Info("AI Debate Team selected - positions populated by score (no OAuth priority)")

	return team, nil
}

// GetRankedProviders returns providers ranked by score.
// Returns nil (not an empty slice) when Verify has not yet populated the
// result, preserving the pre-CONST-029 API contract.
func (sv *StartupVerifier) GetRankedProviders() []*UnifiedProvider {
	if sv.rankedProviders == nil {
		return nil
	}
	snap := sv.rankedProviders.Snapshot()
	if len(snap) == 0 {
		return nil
	}
	return snap
}

// GetDebateTeam returns the selected debate team
func (sv *StartupVerifier) GetDebateTeam() *DebateTeamResult {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	return sv.debateTeam
}

// GetProvider returns a specific provider by ID
func (sv *StartupVerifier) GetProvider(id string) (*UnifiedProvider, bool) {
	if sv.providers == nil {
		return nil, false
	}
	return sv.providers.Get(id)
}

// GetVerifiedProviders returns all verified providers
func (sv *StartupVerifier) GetVerifiedProviders() []*UnifiedProvider {
	if sv.providers == nil {
		return nil
	}
	var verified []*UnifiedProvider
	sv.providers.Range(func(_ string, p *UnifiedProvider) bool {
		if p.Verified {
			verified = append(verified, p)
		}
		return true
	})
	return verified
}

// IsInitialized returns whether the startup verification has completed
func (sv *StartupVerifier) IsInitialized() bool {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	return sv.initialized
}

// detectSubscriptions runs subscription detection for all verified providers.
// Uses 3-tier detection: API → rate limit headers → static fallback.
func (sv *StartupVerifier) detectSubscriptions(ctx context.Context, providers []*UnifiedProvider) {
	for _, p := range providers {
		// Attach access config from registry
		if accessCfg := GetProviderAccessConfig(p.Type); accessCfg != nil {
			p.AccessConfig = accessCfg
		}

		// Run subscription detection
		sub := sv.subscriptionDetector.DetectSubscription(ctx, p.Type, p.APIKey)
		if sub != nil {
			p.Subscription = sub
		}
	}

	sv.log.WithField("providers_detected", len(providers)).Info("Subscription detection complete")
}

// GetSubscriptionDetector returns the subscription detector for external use
func (sv *StartupVerifier) GetSubscriptionDetector() *SubscriptionDetector {
	return sv.subscriptionDetector
}

// Helper functions

func (sv *StartupVerifier) isClaudeOAuthEnabled() bool {
	env := os.Getenv("CLAUDE_CODE_USE_OAUTH_CREDENTIALS")
	return env == "true" || env == "1" || env == "yes"
}

func (sv *StartupVerifier) isQwenOAuthEnabled() bool {
	env := os.Getenv("QWEN_CODE_USE_OAUTH_CREDENTIALS")
	if env == "true" || env == "1" || env == "yes" {
		return true
	}
	// Auto-detect if credentials exist
	if env == "" || env == "auto" {
		_, err := sv.oauthReader.ReadQwenCredentials()
		return err == nil
	}
	return false
}

func isPlaceholder(value string) bool {
	placeholders := []string{
		"your-api-key", "sk-xxx", "xxx", "placeholder",
		"your_api_key", "api_key_here", "INSERT_KEY",
	}
	lower := strings.ToLower(value)
	for _, p := range placeholders {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

// buildFailureReason constructs a human-readable failure reason from verification results.
// It examines the test results, error messages, and last response to produce an actionable description.
func buildFailureReason(result *ServiceVerificationResult) string {
	if result == nil {
		return "verification returned nil result"
	}

	var parts []string

	// Count passed/failed tests
	passed := 0
	failed := 0
	var failedTests []string
	for _, t := range result.Tests {
		if t.Passed {
			passed++
		} else {
			failed++
			failedTests = append(failedTests, t.Name)
		}
	}

	// Check specific failure patterns
	if !result.CodeVisible && len(result.Tests) > 0 {
		parts = append(parts, "code visibility test failed")
	}

	if result.ErrorMessage != "" {
		parts = append(parts, result.ErrorMessage)
	}

	if result.LastResponse != "" {
		isCanned, reason := IsCannedErrorResponse(result.LastResponse)
		if isCanned {
			parts = append(parts, fmt.Sprintf("canned error response detected: %s", reason))
		}
	} else if len(result.Tests) > 0 {
		parts = append(parts, "model returned empty response")
	}

	if len(failedTests) > 0 && len(failedTests) <= 3 {
		parts = append(parts, fmt.Sprintf("failed tests: %s", strings.Join(failedTests, ", ")))
	}

	// Add test summary
	total := passed + failed
	if total > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d tests passed (score: %.1f)", passed, total, result.OverallScore))
	}

	if len(parts) == 0 {
		return "verification failed (unknown reason)"
	}

	return strings.Join(parts, ". ")
}

// categorizeFailure determines the failure category from verification results.
// Categories map to actionable remediation steps.
func categorizeFailure(result *ServiceVerificationResult) string {
	if result == nil {
		return FailureCategoryAPIError
	}

	// Check for empty response
	if result.LastResponse == "" && len(result.Tests) > 0 {
		return FailureCategoryEmptyResponse
	}

	// Check for canned/error response
	if result.LastResponse != "" {
		isCanned, _ := IsCannedErrorResponse(result.LastResponse)
		if isCanned {
			return FailureCategoryCannedResponse
		}
	}

	// Check error message patterns
	errMsg := strings.ToLower(result.ErrorMessage)
	if strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "deadline") {
		return FailureCategoryTimeout
	}
	if strings.Contains(errMsg, "auth") || strings.Contains(errMsg, "unauthorized") ||
		strings.Contains(errMsg, "forbidden") || strings.Contains(errMsg, "401") ||
		strings.Contains(errMsg, "403") {
		return FailureCategoryAuthError
	}

	// Check code visibility
	if !result.CodeVisible && result.OverallScore > 0 {
		return FailureCategoryCodeVisibility
	}

	// Check score threshold
	if result.OverallScore > 0 && result.OverallScore < 50 {
		return FailureCategoryScoreBelow
	}

	return FailureCategoryAPIError
}

// mapTestDetails converts internal TestResult slice to API-friendly ProviderTestDetail slice.
func mapTestDetails(tests []TestResult) []ProviderTestDetail {
	if len(tests) == 0 {
		return nil
	}

	details := make([]ProviderTestDetail, len(tests))
	for i, t := range tests {
		durationMs := t.CompletedAt.Sub(t.StartedAt).Milliseconds()
		if durationMs < 0 {
			durationMs = 0
		}
		details[i] = ProviderTestDetail{
			Name:       t.Name,
			Passed:     t.Passed,
			Score:      t.Score,
			Details:    t.Details,
			DurationMs: durationMs,
		}
	}
	return details
}

// truncateString truncates a string to maxLen characters, appending "..." if truncated.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// populateFailureDetails fills failure tracking fields on a provider from verification results.
func populateFailureDetails(provider *UnifiedProvider, result *ServiceVerificationResult) {
	if result == nil {
		return
	}

	provider.TestDetails = mapTestDetails(result.Tests)
	provider.VerificationMsg = result.Message
	provider.LastModelResponse = truncateString(result.LastResponse, 200)

	if !provider.Verified {
		provider.FailureReason = buildFailureReason(result)
		provider.FailureCategory = categorizeFailure(result)
	}
}
