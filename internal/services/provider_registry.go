package services

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"digital.vasic.concurrency/pkg/safe"
	"golang.org/x/sync/semaphore"

	"dev.helix.agent/internal/auth/oauth_credentials"
	"dev.helix.agent/internal/config"
	"dev.helix.agent/internal/llm"
	"dev.helix.agent/internal/llm/providers/ai21"
	"dev.helix.agent/internal/llm/providers/anthropic"
	"dev.helix.agent/internal/llm/providers/anthropic_cu"
	"dev.helix.agent/internal/llm/providers/azure"
	"dev.helix.agent/internal/llm/providers/cerebras"
	"dev.helix.agent/internal/llm/providers/chutes"
	"dev.helix.agent/internal/llm/providers/claude"
	"dev.helix.agent/internal/llm/providers/cloudflare"
	"dev.helix.agent/internal/llm/providers/codestral"
	"dev.helix.agent/internal/llm/providers/cohere"
	"dev.helix.agent/internal/llm/providers/deepseek"
	"dev.helix.agent/internal/llm/providers/fireworks"
	"dev.helix.agent/internal/llm/providers/gemini"
	"dev.helix.agent/internal/llm/providers/generic"
	"dev.helix.agent/internal/llm/providers/githubmodels"
	"dev.helix.agent/internal/llm/providers/groq"
	"dev.helix.agent/internal/llm/providers/helixllm"
	"dev.helix.agent/internal/llm/providers/huggingface"
	"dev.helix.agent/internal/llm/providers/hyperbolic"
	"dev.helix.agent/internal/llm/providers/junie"
	"dev.helix.agent/internal/llm/providers/kilo"
	"dev.helix.agent/internal/llm/providers/kimi"
	"dev.helix.agent/internal/llm/providers/kimicode"
	"dev.helix.agent/internal/llm/providers/lmstudio"
	"dev.helix.agent/internal/llm/providers/mistral"
	"dev.helix.agent/internal/llm/providers/modal"
	"dev.helix.agent/internal/llm/providers/nia"
	"dev.helix.agent/internal/llm/providers/nlpcloud"
	"dev.helix.agent/internal/llm/providers/novita"
	"dev.helix.agent/internal/llm/providers/nvidia"
	"dev.helix.agent/internal/llm/providers/ollama"
	"dev.helix.agent/internal/llm/providers/openai"
	"dev.helix.agent/internal/llm/providers/openrouter"
	"dev.helix.agent/internal/llm/providers/perplexity"
	"dev.helix.agent/internal/llm/providers/publicai"
	"dev.helix.agent/internal/llm/providers/qwen"
	"dev.helix.agent/internal/llm/providers/replicate"
	"dev.helix.agent/internal/llm/providers/sambanova"
	"dev.helix.agent/internal/llm/providers/sarvam"
	"dev.helix.agent/internal/llm/providers/siliconflow"
	"dev.helix.agent/internal/llm/providers/together"
	"dev.helix.agent/internal/llm/providers/upstage"
	"dev.helix.agent/internal/llm/providers/venice"
	"dev.helix.agent/internal/llm/providers/vertex"
	"dev.helix.agent/internal/llm/providers/vulavula"
	"dev.helix.agent/internal/llm/providers/xai"
	"dev.helix.agent/internal/llm/providers/xiaomi"
	"dev.helix.agent/internal/llm/providers/zai"
	"dev.helix.agent/internal/llm/providers/zen"
	"dev.helix.agent/internal/llm/providers/zhipu"
	"dev.helix.agent/internal/models"
	"dev.helix.agent/internal/verifier"
	"github.com/sirupsen/logrus"
)

// ProviderRegistry manages LLM provider registration and configuration.
//
// Concurrency (CONST-029):
//   - All per-provider-name collections are kept in independent *safe.Store
//     instances: providers, circuitBreakers, concurrencySemaphores,
//     providerConfigs, providerHealth, activeRequests, initOnce.
//   - Each map is a flat index keyed by provider name with no cross-map
//     invariant that would require atomic multi-map transitions, so the
//     previous shared r.mu is no longer needed.
//   - startupVerifier is rarely-mutated and held behind an atomic.Pointer.
type ProviderRegistry struct {
	providers             *safe.Store[string, llm.LLMProvider]
	circuitBreakers       *safe.Store[string, *CircuitBreaker]
	concurrencySemaphores *safe.Store[string, *semaphore.Weighted]
	providerConfigs       *safe.Store[string, *ProviderConfig]             // Stores provider configurations
	providerHealth        *safe.Store[string, *ProviderVerificationResult] // Stores provider health verification results
	activeRequests        *safe.Store[string, *int64]                      // Atomic counters for active requests per provider
	config                *RegistryConfig
	ensemble              *EnsembleService
	requestService        *RequestService
	memory                *MemoryService
	discovery             *ProviderDiscovery                       // Auto-discovery service for environment-based provider detection
	scoreAdapter          *LLMsVerifierScoreAdapter                // LLMsVerifier score adapter for dynamic provider ordering
	startupVerifier       atomic.Pointer[verifier.StartupVerifier] // Unified startup verification (optional)
	drainTimeout          time.Duration                            // Timeout for graceful shutdown request draining
	autoDiscovery         bool                                     // Whether auto-discovery is enabled
	initSemaphore         *semaphore.Weighted                      // Semaphore to limit concurrent provider initialization
	initOnce              *safe.Store[string, *sync.Once]          // sync.Once per provider for thread-safe initialization
}

// ProviderConfig holds configuration for an LLM provider
type ProviderConfig struct {
	Name                  string            `json:"name"`
	Type                  string            `json:"type"`
	Enabled               bool              `json:"enabled"`
	APIKey                string            `json:"api_key"`
	BaseURL               string            `json:"base_url"`
	Models                []ModelConfig     `json:"models"`
	Timeout               time.Duration     `json:"timeout"`
	MaxRetries            int               `json:"max_retries"`
	MaxConcurrentRequests int               `json:"max_concurrent_requests"`
	HealthCheckURL        string            `json:"health_check_url"`
	Weight                float64           `json:"weight"`
	Tags                  []string          `json:"tags"`
	Capabilities          map[string]string `json:"capabilities"`
	CustomSettings        map[string]any    `json:"custom_settings"`
	ProjectID             string            `json:"project_id"` // For Google Vertex AI
	Location              string            `json:"location"`   // For Google Vertex AI
}

// ProviderHealthStatus represents the verified health status of a provider
type ProviderHealthStatus string

const (
	ProviderStatusUnknown     ProviderHealthStatus = "unknown"
	ProviderStatusHealthy     ProviderHealthStatus = "healthy"
	ProviderStatusRateLimited ProviderHealthStatus = "rate_limited"
	ProviderStatusAuthFailed  ProviderHealthStatus = "auth_failed"
	ProviderStatusUnhealthy   ProviderHealthStatus = "unhealthy"
)

// ProviderVerificationResult contains the result of verifying a provider
type ProviderVerificationResult struct {
	Provider     string               `json:"provider"`
	Name         string               `json:"name"` // Alias for Provider for compatibility
	Status       ProviderHealthStatus `json:"status"`
	Verified     bool                 `json:"verified"`
	Score        float64              `json:"score"` // LLMsVerifier score (0-10)
	ResponseTime time.Duration        `json:"response_time_ms"`
	Error        string               `json:"error,omitempty"`
	TestedAt     time.Time            `json:"tested_at"`
	VerifiedAt   time.Time            `json:"verified_at,omitempty"` // Alias for TestedAt
}

// ModelConfig holds configuration for a specific model
type ModelConfig struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Enabled      bool           `json:"enabled"`
	Weight       float64        `json:"weight"`
	Capabilities []string       `json:"capabilities"`
	CustomParams map[string]any `json:"custom_params"`
}

// RegistryConfig holds configuration for provider registry
type RegistryConfig struct {
	DefaultTimeout        time.Duration              `json:"default_timeout"`
	MaxRetries            int                        `json:"max_retries"`
	MaxConcurrentRequests int                        `json:"max_concurrent_requests"`
	HealthCheck           HealthCheckConfig          `json:"health_check"`
	CircuitBreaker        CircuitBreakerConfig       `json:"circuit_breaker"`
	Providers             map[string]*ProviderConfig `json:"providers"`
	Ensemble              *models.EnsembleConfig     `json:"ensemble"`
	Routing               *RoutingConfig             `json:"routing"`
	DisableAutoDiscovery  bool                       `json:"disable_auto_discovery"`
}

// HealthCheckConfig holds health check configuration
type HealthCheckConfig struct {
	Enabled          bool          `json:"enabled"`
	Interval         time.Duration `json:"interval"`
	Timeout          time.Duration `json:"timeout"`
	FailureThreshold int           `json:"failure_threshold"`
}

// RoutingConfig holds routing configuration
type RoutingConfig struct {
	Strategy string             `json:"strategy"`
	Weights  map[string]float64 `json:"weights"`
}

// CircuitBreakerConfig holds circuit breaker configuration
type CircuitBreakerConfig struct {
	Enabled          bool          `json:"enabled"`
	FailureThreshold int           `json:"failure_threshold"`
	RecoveryTimeout  time.Duration `json:"recovery_timeout"`
	SuccessThreshold int           `json:"success_threshold"`
}

// circuitBreakerProvider wraps an LLMProvider with circuit breaker functionality
type circuitBreakerProvider struct {
	provider              llm.LLMProvider
	circuitBreaker        *CircuitBreaker
	concurrencySemaphore  *semaphore.Weighted
	name                  string
	activeRequestsCounter *int64 // Atomic counter for active requests
	totalPermits          int64  // Total semaphore permits (max concurrent)
	acquiredPermits       int64  // Currently acquired permits (atomic)
}

// Complete wraps the provider's Complete method with circuit breaker protection
func (cbp *circuitBreakerProvider) Complete(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error) {
	// Acquire concurrency semaphore if present
	if cbp.concurrencySemaphore != nil {
		if err := cbp.concurrencySemaphore.Acquire(ctx, 1); err != nil {
			// Record acquisition error
			if ctx.Err() == context.DeadlineExceeded || ctx.Err() == context.Canceled {
				RecordAcquisitionTimeout(cbp.name)
			} else {
				RecordAcquisitionError(cbp.name)
			}
			return nil, err
		}
		// Update acquired permits count and metrics
		atomic.AddInt64(&cbp.acquiredPermits, 1)
		cbp.updateMetrics()
		defer func() {
			cbp.concurrencySemaphore.Release(1)
			atomic.AddInt64(&cbp.acquiredPermits, -1)
			cbp.updateMetrics()
		}()
	}

	// Increment active requests counter if present
	if cbp.activeRequestsCounter != nil {
		atomic.AddInt64(cbp.activeRequestsCounter, 1)
		defer atomic.AddInt64(cbp.activeRequestsCounter, -1)
	}

	var resp *models.LLMResponse
	var err error

	// Use circuit breaker if present, otherwise call directly
	if cbp.circuitBreaker != nil {
		err = cbp.circuitBreaker.Call(func() error {
			var callErr error
			resp, callErr = cbp.provider.Complete(ctx, req)
			return callErr
		})
	} else {
		resp, err = cbp.provider.Complete(ctx, req)
	}

	return resp, err
}

// CompleteStream wraps the provider's CompleteStream method with circuit breaker protection
func (cbp *circuitBreakerProvider) CompleteStream(ctx context.Context, req *models.LLMRequest) (<-chan *models.LLMResponse, error) {
	// Acquire concurrency semaphore if present
	if cbp.concurrencySemaphore != nil {
		if err := cbp.concurrencySemaphore.Acquire(ctx, 1); err != nil {
			// Record acquisition error
			if ctx.Err() == context.DeadlineExceeded || ctx.Err() == context.Canceled {
				RecordAcquisitionTimeout(cbp.name)
			} else {
				RecordAcquisitionError(cbp.name)
			}
			return nil, err
		}
		// Update acquired permits count and metrics
		atomic.AddInt64(&cbp.acquiredPermits, 1)
		cbp.updateMetrics()
		defer func() {
			cbp.concurrencySemaphore.Release(1)
			atomic.AddInt64(&cbp.acquiredPermits, -1)
			cbp.updateMetrics()
		}()
	}

	// Increment active requests counter if present
	if cbp.activeRequestsCounter != nil {
		atomic.AddInt64(cbp.activeRequestsCounter, 1)
		defer atomic.AddInt64(cbp.activeRequestsCounter, -1)
	}

	var stream <-chan *models.LLMResponse
	var err error

	// Use circuit breaker if present, otherwise call directly
	if cbp.circuitBreaker != nil {
		err = cbp.circuitBreaker.Call(func() error {
			var callErr error
			stream, callErr = cbp.provider.CompleteStream(ctx, req)
			return callErr
		})
	} else {
		stream, err = cbp.provider.CompleteStream(ctx, req)
	}

	return stream, err
}

// HealthCheck wraps the provider's HealthCheck method with circuit breaker protection
func (cbp *circuitBreakerProvider) HealthCheck() error {
	if cbp.circuitBreaker != nil {
		return cbp.circuitBreaker.Call(func() error {
			return cbp.provider.HealthCheck()
		})
	}
	return cbp.provider.HealthCheck()
}

// GetCapabilities delegates to the underlying provider
func (cbp *circuitBreakerProvider) GetCapabilities() *models.ProviderCapabilities {
	return cbp.provider.GetCapabilities()
}

// ValidateConfig delegates to the underlying provider
func (cbp *circuitBreakerProvider) ValidateConfig(config map[string]interface{}) (bool, []string) {
	return cbp.provider.ValidateConfig(config)
}

// updateMetrics updates concurrency metrics for this provider
func (cbp *circuitBreakerProvider) updateMetrics() {
	activeRequests := int64(0)
	if cbp.activeRequestsCounter != nil {
		activeRequests = atomic.LoadInt64(cbp.activeRequestsCounter)
	}
	UpdateConcurrencyMetrics(cbp.name, cbp.totalPermits, atomic.LoadInt64(&cbp.acquiredPermits), activeRequests)
}

func NewProviderRegistry(cfg *RegistryConfig, memory *MemoryService) *ProviderRegistry {
	// Local-first default (spec 002, HA-F2-002): cloud provider auto-discovery
	// is OFF unless the operator explicitly opted in via
	// HELIX_CLOUD_PROVIDERS=true. Previously this defaulted ON, which made
	// every env-credentialed cloud provider (and the credential-less anonymous
	// zen endpoint) reachable with zero operator action.
	enableAutoDiscovery := CloudProvidersOptedIn()
	if cfg != nil && cfg.DisableAutoDiscovery {
		enableAutoDiscovery = false
	}
	return newProviderRegistry(cfg, memory, enableAutoDiscovery)
}

// NewProviderRegistryWithoutAutoDiscovery creates a provider registry without auto-discovery
// This is useful for testing where you want to control exactly which providers are registered
func NewProviderRegistryWithoutAutoDiscovery(cfg *RegistryConfig, memory *MemoryService) *ProviderRegistry {
	return newProviderRegistry(cfg, memory, false)
}

func newProviderRegistry(cfg *RegistryConfig, memory *MemoryService, enableAutoDiscovery bool) *ProviderRegistry {
	if cfg == nil {
		cfg = getDefaultRegistryConfig()
	}

	// Initialize logger for registry
	logger := logrus.New()
	logger.SetLevel(logrus.InfoLevel)

	registry := &ProviderRegistry{
		providers:             safe.NewStore[string, llm.LLMProvider](),
		circuitBreakers:       safe.NewStore[string, *CircuitBreaker](),
		concurrencySemaphores: safe.NewStore[string, *semaphore.Weighted](),
		providerConfigs:       safe.NewStore[string, *ProviderConfig](),
		providerHealth:        safe.NewStore[string, *ProviderVerificationResult](),
		activeRequests:        safe.NewStore[string, *int64](),
		config:                cfg,
		memory:                memory,
		drainTimeout:          30 * time.Second, // Default 30 second drain timeout
		autoDiscovery:         enableAutoDiscovery,
		initSemaphore:         semaphore.NewWeighted(5), // Limit to 5 concurrent provider initializations
		initOnce:              safe.NewStore[string, *sync.Once](),
	}

	// Initialize ensemble service
	ensembleStrategy := "confidence_weighted"
	if cfg.Ensemble != nil {
		ensembleStrategy = cfg.Ensemble.Strategy
	}
	registry.ensemble = NewEnsembleService(ensembleStrategy, cfg.DefaultTimeout)

	// Initialize request service
	routingStrategy := "weighted"
	if cfg.Routing != nil {
		routingStrategy = cfg.Routing.Strategy
	}
	registry.requestService = NewRequestService(routingStrategy, registry.ensemble, memory)

	// Register providers from config file (backward compatibility)
	registry.registerDefaultProviders(cfg)

	// Auto-discover additional providers from environment variables
	// This supplements config file providers with any additional API keys found
	if enableAutoDiscovery {
		registry.initAutoDiscovery(logger)
	}

	return registry
}

// initAutoDiscovery initializes the auto-discovery service and discovers
// providers from env vars.
//
// HXC-274: this function used to call r.discovery.DiscoverProviders(),
// which — for every credential it found — invoked that provider's
// GetCapabilities(), and for ~40 of the ~45 supported provider types that
// method makes a real outbound HTTP call (the provider's own /v1/models
// endpoint, then models.dev on failure; see
// internal/llm/discovery.Discoverer.DiscoverModels and
// ProviderDiscovery.DiscoverProviderCredentials's doc for the full chain).
// Registry construction runs on every call to NewProviderRegistry —
// including the one in cmd/grpc-server/main.go's main(), before the
// listener is even serving — so a server start-up with live provider
// credentials in the environment was silently spending API quota and
// serially paying every provider's round-trip latency (observed ~18s
// across seven live-credentialed providers) before accepting its first
// request. DiscoverProviderCredentials returns the identical discovered-
// provider set (same names, same constructed clients, same registration
// behaviour below) with zero outbound calls; only Capabilities/
// SupportsModels are left unpopulated, and nothing in this function reads
// either field, so this is not a behavioural change for the caller.
//
// Do not change this back to DiscoverProviders() to "keep capabilities
// available at start-up" — that is precisely the eager-network-call default
// this fix removes. A caller that genuinely wants live capability/model
// data gets it by calling GetCapabilities() on the specific provider it
// cares about, or by driving the explicit, operator-triggered
// POST /v1/providers/rediscover path (internal/handlers/provider_management
// .go ReDiscoverProviders), which still calls DiscoverProviders() and is
// exactly the "explicitly asked to" opt-in HXC-274's acceptance criteria
// describes.
func (r *ProviderRegistry) initAutoDiscovery(logger *logrus.Logger) {
	if !r.autoDiscovery {
		return
	}

	// Create discovery service (verify on startup disabled - we'll do it on-demand)
	r.discovery = NewProviderDiscovery(logger, false)

	// Discover which providers have credentials present in the environment
	// WITHOUT making any outbound network call (HXC-274 — see doc above).
	discovered, err := r.discovery.DiscoverProviderCredentials()
	if err != nil {
		logger.WithError(err).Warn("Provider auto-discovery failed")
		return
	}

	if len(discovered) == 0 {
		logger.Info("No additional providers discovered from environment")
		return
	}

	// Register discovered providers that aren't already registered via config
	existingProviders := r.ListProviders()
	existingMap := make(map[string]bool)
	for _, name := range existingProviders {
		existingMap[name] = true
	}

	newProviders := 0
	for _, dp := range discovered {
		// Skip if already registered via config (config takes precedence)
		if existingMap[dp.Name] {
			logger.WithField("provider", dp.Name).Debug("Provider already registered via config, skipping auto-discovery")
			continue
		}

		// Register the discovered provider
		if dp.Provider != nil {
			if err := r.RegisterProvider(dp.Name, dp.Provider); err != nil {
				logger.WithError(err).WithField("provider", dp.Name).Warn("Failed to register auto-discovered provider")
			} else {
				newProviders++
				logger.WithFields(logrus.Fields{
					"provider": dp.Name,
					"type":     dp.Type,
					"env_var":  dp.APIKeyEnvVar,
				}).Info("Auto-discovered and registered provider from environment")
			}
		}
	}

	logger.WithFields(logrus.Fields{
		"discovered": len(discovered),
		"registered": newProviders,
		"skipped":    len(discovered) - newProviders,
	}).Info("Provider auto-discovery completed")

	// Initialize LLMsVerifier score adapter for dynamic provider ordering
	// This allows the ensemble to prioritize providers by their LLMsVerifier scores
	r.initScoreAdapter(logger)
}

// initScoreAdapter initializes the LLMsVerifier score adapter and connects it to the ensemble
// This is the central point where LLMsVerifier becomes the heart of all provider validation
func (r *ProviderRegistry) initScoreAdapter(logger *logrus.Logger) {
	// Check if verifier is disabled via environment variable
	if os.Getenv("LLM_VERIFIER_DISABLED") == "true" {
		logger.Info("LLMsVerifier disabled via LLM_VERIFIER_DISABLED environment variable")
		return
	}

	// Create LLMsVerifier configuration with defaults
	verifierCfg := verifier.DefaultConfig()
	verifierCfg.Enabled = true

	// Create ScoringService for score calculations
	scoringService, err := verifier.NewScoringService(verifierCfg)
	if err != nil {
		logger.WithError(err).Warn("Failed to create LLMsVerifier scoring service, using fallback")
		scoringService = nil
	}

	// Create VerificationService for provider/model verification
	verificationService := verifier.NewVerificationService(verifierCfg)

	// Wire the verification service to use our registered providers for actual API calls
	// This allows LLMsVerifier to verify models using ProviderRegistry's providers
	verificationService.SetProviderFunc(func(ctx context.Context, modelID, provider, prompt string) (string, error) {
		p, exists := r.providers.Get(provider)
		if !exists {
			return "", fmt.Errorf("provider %s not registered", provider)
		}

		req := &models.LLMRequest{
			ID:        fmt.Sprintf("verify_%s_%d", modelID, time.Now().UnixNano()),
			SessionID: "llmsverifier",
			Prompt:    prompt,
			Messages: []models.Message{
				{Role: "user", Content: prompt},
			},
			ModelParams: models.ModelParameters{
				Model:       modelID,
				MaxTokens:   100,
				Temperature: 0.1,
			},
			Status:    "pending",
			CreatedAt: time.Now(),
		}

		resp, err := p.Complete(ctx, req)
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	})

	// Create score adapter with real services
	r.scoreAdapter = NewLLMsVerifierScoreAdapter(scoringService, verificationService, logger)

	// Connect to ensemble service for dynamic provider ordering
	if r.ensemble != nil && r.scoreAdapter != nil {
		r.ensemble.SetScoreProvider(r.scoreAdapter)
		logger.Info("LLMsVerifier score adapter connected to ensemble service for dynamic provider ordering")
	}

	logger.WithFields(logrus.Fields{
		"scoring_service":      scoringService != nil,
		"verification_service": verificationService != nil,
	}).Info("LLMsVerifier services initialized as central authority for provider validation")
}

// GetScoreAdapter returns the LLMsVerifier score adapter
func (r *ProviderRegistry) GetScoreAdapter() *LLMsVerifierScoreAdapter {
	return r.scoreAdapter
}

// UpdateProviderScore updates the LLMsVerifier score for a provider
// This should be called after provider verification
func (r *ProviderRegistry) UpdateProviderScore(provider, modelID string, score float64) {
	if r.scoreAdapter != nil {
		r.scoreAdapter.UpdateScore(provider, modelID, score)
	}
}

// GetDiscovery returns the provider discovery service
func (r *ProviderRegistry) GetDiscovery() *ProviderDiscovery {
	return r.discovery
}

// DiscoverAndVerifyProviders runs provider discovery and verification
// Returns a summary of discovered and verified providers
func (r *ProviderRegistry) DiscoverAndVerifyProviders(ctx context.Context) map[string]interface{} {
	if r.discovery == nil {
		return map[string]interface{}{
			"error":   "auto-discovery not initialized",
			"enabled": false,
		}
	}

	// Verify all discovered providers
	r.discovery.VerifyAllProviders(ctx)

	// Get and return summary
	summary := r.discovery.Summary()
	summary["auto_discovery_enabled"] = r.autoDiscovery

	return summary
}

// GetBestProvidersForDebate returns the best verified providers for the debate group
func (r *ProviderRegistry) GetBestProvidersForDebate(minProviders, maxProviders int) []string {
	if r.discovery == nil {
		// Fall back to healthy providers from verification
		return r.GetHealthyProviders()
	}

	bestProviders := r.discovery.GetDebateGroupProviders(minProviders, maxProviders)
	names := make([]string, 0, len(bestProviders))
	for _, p := range bestProviders {
		names = append(names, p.Name)
	}
	return names
}

// SetAutoDiscovery enables or disables auto-discovery
func (r *ProviderRegistry) SetAutoDiscovery(enabled bool) {
	r.autoDiscovery = enabled
}

// SetStartupVerifier sets the unified startup verifier
// When set, provider operations will delegate to the StartupVerifier
func (r *ProviderRegistry) SetStartupVerifier(sv *verifier.StartupVerifier) {
	r.startupVerifier.Store(sv)
}

// GetStartupVerifier returns the startup verifier if set
func (r *ProviderRegistry) GetStartupVerifier() *verifier.StartupVerifier {
	return r.startupVerifier.Load()
}

// InitializeFromStartupVerifier registers providers from the StartupVerifier's verified results
// This should be called after VerifyAllProviders completes on the StartupVerifier
func (r *ProviderRegistry) InitializeFromStartupVerifier() error {
	sv := r.startupVerifier.Load()
	if sv == nil {
		return fmt.Errorf("startup verifier not set")
	}

	logger := logrus.New()
	rankedProviders := sv.GetRankedProviders()

	registeredCount := 0
	for _, provider := range rankedProviders {
		if !provider.Verified || provider.Instance == nil {
			continue
		}

		// Register the provider
		if err := r.RegisterProvider(provider.Name, provider.Instance); err != nil {
			logger.WithFields(logrus.Fields{
				"provider": provider.Name,
				"error":    err.Error(),
			}).Warn("Failed to register provider from StartupVerifier")
			continue
		}

		// Update provider health status
		status := ProviderStatusHealthy
		if provider.Status == verifier.StatusDegraded {
			status = ProviderStatusUnhealthy
		} else if provider.Status == verifier.StatusRateLimited {
			status = ProviderStatusRateLimited
		} else if provider.Status == verifier.StatusAuthFailed {
			status = ProviderStatusAuthFailed
		}

		r.providerHealth.Put(provider.Name, &ProviderVerificationResult{
			Provider:     provider.Name,
			Name:         provider.Name,
			Status:       status,
			Verified:     provider.Verified,
			Score:        provider.Score,
			ResponseTime: 0, // Not tracked in unified provider
			TestedAt:     provider.VerifiedAt,
			VerifiedAt:   provider.VerifiedAt,
		})

		// Update LLMsVerifier score if score adapter is available
		if r.scoreAdapter != nil {
			r.scoreAdapter.UpdateScore(provider.Name, provider.DefaultModel, provider.Score)
		}

		registeredCount++
		logger.WithFields(logrus.Fields{
			"provider": provider.Name,
			"score":    provider.Score,
			"verified": provider.Verified,
			"auth":     provider.AuthType,
		}).Debug("Registered provider from StartupVerifier")
	}

	logger.WithFields(logrus.Fields{
		"total_providers": len(rankedProviders),
		"registered":      registeredCount,
	}).Info("Providers initialized from StartupVerifier")

	return nil
}

// GetVerifiedProvidersSummary returns a summary of all verified providers
// Uses StartupVerifier if available, otherwise falls back to discovery
func (r *ProviderRegistry) GetVerifiedProvidersSummary() map[string]interface{} {
	sv := r.startupVerifier.Load()

	if sv != nil {
		rankedProviders := sv.GetRankedProviders()
		providers := make([]map[string]interface{}, 0, len(rankedProviders))

		for _, p := range rankedProviders {
			providers = append(providers, map[string]interface{}{
				"name":      p.Name,
				"type":      p.Type,
				"auth_type": p.AuthType,
				"verified":  p.Verified,
				"score":     p.Score,
				"status":    p.Status,
				"models":    len(p.Models),
			})
		}

		return map[string]interface{}{
			"source":          "startup_verifier",
			"total_providers": len(rankedProviders),
			"providers":       providers,
		}
	}

	// Fall back to discovery
	if r.discovery != nil {
		return r.discovery.Summary()
	}

	return map[string]interface{}{
		"source":          "registry",
		"total_providers": len(r.ListProviders()),
		"providers":       r.ListProviders(),
	}
}

func (r *ProviderRegistry) registerDefaultProviders(cfg *RegistryConfig) {
	// Store provider configurations for lazy loading
	// DeepSeek provider
	deepseekConfig := cfg.Providers["deepseek"]
	if deepseekConfig == nil {
		deepseekConfig = &ProviderConfig{
			Name:    "deepseek",
			Type:    "deepseek",
			Enabled: false, // Disabled by default - requires API key
			Models: []ModelConfig{{
				ID:      "deepseek-coder",
				Name:    "DeepSeek Coder",
				Enabled: true,
				Weight:  1.0,
			}},
		}
	}
	r.storeProviderConfig(deepseekConfig)

	// Claude provider
	claudeConfig := cfg.Providers["claude"]
	if claudeConfig == nil {
		claudeConfig = &ProviderConfig{
			Name:    "claude",
			Type:    "claude",
			Enabled: false, // Disabled by default - requires API key or OAuth
			Models: []ModelConfig{{
				ID:      "claude-3-sonnet-20240229",
				Name:    "Claude 3 Sonnet",
				Enabled: true,
				Weight:  1.0,
			}},
		}
	}
	r.storeProviderConfig(claudeConfig)

	// Gemini provider
	geminiConfig := cfg.Providers["gemini"]
	if geminiConfig == nil {
		geminiConfig = &ProviderConfig{
			Name:    "gemini",
			Type:    "gemini",
			Enabled: false, // Disabled by default - requires API key
			Models: []ModelConfig{{
				ID:      "gemini-pro",
				Name:    "Gemini Pro",
				Enabled: true,
				Weight:  1.0,
			}},
		}
	}
	r.storeProviderConfig(geminiConfig)

	// HelixLLM provider (submodule) — the LOCAL llama.cpp/Colibri chain.
	// Local-first default (spec 002, HA-F2-002): enabled unless the operator
	// explicitly opted out via USE_HELIX_LLM=false.
	helixllmConfig := cfg.Providers["helixllm"]
	if helixllmConfig == nil {
		helixllmConfig = &ProviderConfig{
			Name:    "helixllm",
			Type:    "helixllm",
			Enabled: HelixLLMEnabledDefault(),
			Models: []ModelConfig{{
				ID:      "helixllm-default",
				Name:    "HelixLLM Default",
				Enabled: true,
				Weight:  1.0,
			}},
		}
	}
	r.storeProviderConfig(helixllmConfig)

	// Qwen provider
	qwenConfig := cfg.Providers["qwen"]
	if qwenConfig == nil {
		qwenConfig = &ProviderConfig{
			Name:    "qwen",
			Type:    "qwen",
			Enabled: false, // Disabled by default - requires API key or OAuth
			Models: []ModelConfig{{
				ID:      "qwen-turbo",
				Name:    "Qwen Turbo",
				Enabled: true,
				Weight:  1.0,
			}},
		}
	}
	r.storeProviderConfig(qwenConfig)

	// OpenRouter provider
	openrouterConfig := cfg.Providers["openrouter"]
	if openrouterConfig == nil {
		openrouterConfig = &ProviderConfig{
			Name:    "openrouter",
			Type:    "openrouter",
			Enabled: false, // Disabled by default - requires API key
			Models: []ModelConfig{{
				ID:      "x-ai/grok-4",
				Name:    "Grok-4 via OpenRouter",
				Enabled: true,
				Weight:  1.3,
			}},
		}
	}
	r.storeProviderConfig(openrouterConfig)

	// Zen (OpenCode) — supports anonymous free models with fallback chain.
	//
	// Unlike every other synthesized default, Zen needs NO credential: enabling it
	// makes GetProvider("zen") lazily materialise a live provider that talks to a
	// public endpoint. That is an IMPLICIT provider acquisition, so it MUST obey
	// the same auto-discovery switch that gates every other implicit acquisition
	// (see the `r.autoDiscovery &&` guards on the Claude/Qwen OAuth paths in
	// createProviderFromConfig). Otherwise a registry built via
	// NewProviderRegistryWithoutAutoDiscovery — whose contract is that the caller
	// controls exactly which providers are registered — silently gains an
	// unconfigured provider and can route user prompts to it.
	//
	// This does NOT change out-of-the-box behaviour for the real application:
	// when auto-discovery is on, initAutoDiscovery discovers Zen anonymously and
	// registers a live instance regardless of this flag. An operator who wants
	// Zen in a no-auto-discovery registry supplies cfg.Providers["zen"]
	// explicitly, which takes precedence over this synthesized default.
	zenConfig := cfg.Providers["zen"]
	if zenConfig == nil {
		zenConfig = &ProviderConfig{
			Name:    "zen",
			Type:    "zen",
			Enabled: r.autoDiscovery,
			Models: []ModelConfig{
				{ID: "big-pickle", Name: "Big Pickle (Free)", Enabled: true, Weight: 1.0},
				{ID: "glm-5-free", Name: "GLM-5 Free", Enabled: true, Weight: 0.9},
				{ID: "kimi-k2", Name: "Kimi K2 (Paid)", Enabled: true, Weight: 1.1},
			},
		}
	}
	r.storeProviderConfig(zenConfig)

	logrus.Info("Stored provider configurations for lazy loading")
}

// storeProviderConfig stores a provider configuration for lazy initialization
func (r *ProviderRegistry) storeProviderConfig(cfg *ProviderConfig) {
	r.providerConfigs.Put(cfg.Name, cfg)

	if r.config == nil {
		r.config = &RegistryConfig{
			Providers: make(map[string]*ProviderConfig),
		}
	}
	if r.config.Providers == nil {
		r.config.Providers = make(map[string]*ProviderConfig)
	}
	r.config.Providers[cfg.Name] = cfg
}
func (r *ProviderRegistry) RegisterProvider(name string, provider llm.LLMProvider) error {
	if _, exists := r.providers.Get(name); exists {
		return fmt.Errorf("provider %s already registered", name)
	}

	// Create concurrency semaphore for this provider
	maxConcurrent := r.config.MaxConcurrentRequests
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	sem := semaphore.NewWeighted(int64(maxConcurrent))
	r.concurrencySemaphores.Put(name, sem)

	// Create circuit breaker if enabled
	var cb *CircuitBreaker
	if r.config.CircuitBreaker.Enabled {
		cb = NewCircuitBreaker(
			r.config.CircuitBreaker.FailureThreshold,
			r.config.CircuitBreaker.SuccessThreshold,
			r.config.CircuitBreaker.RecoveryTimeout,
		)
		r.circuitBreakers.Put(name, cb)
	}

	// Initialize atomic counter for active requests
	var counter int64
	r.activeRequests.Put(name, &counter)

	// Wrap provider with circuit breaker and concurrency semaphore
	wrappedProvider := &circuitBreakerProvider{
		provider:              provider,
		circuitBreaker:        cb,
		concurrencySemaphore:  sem,
		name:                  name,
		activeRequestsCounter: &counter,
		totalPermits:          int64(maxConcurrent),
		acquiredPermits:       0,
	}

	r.providers.Put(name, wrappedProvider)

	// Also register with ensemble and request services
	r.ensemble.RegisterProvider(name, &providerAdapter{provider: wrappedProvider})
	r.requestService.RegisterProvider(name, &providerAdapter{provider: wrappedProvider})

	// Update concurrency metrics after registration
	UpdateConcurrencyMetrics(name, int64(maxConcurrent), 0, 0)

	return nil
}

// GetCircuitBreaker returns the circuit breaker for a provider (for internal use)
func (r *ProviderRegistry) GetCircuitBreaker(name string) *CircuitBreaker {
	cb, _ := r.circuitBreakers.Get(name)
	return cb
}

func (r *ProviderRegistry) UnregisterProvider(name string) error {
	return r.unregisterProviderLocked(name)
}

// unregisterProviderLocked removes a provider. The name historically indicated
// the caller holds the registry mutex; after the CONST-029 migration the
// underlying stores provide their own serialisation.
func (r *ProviderRegistry) unregisterProviderLocked(name string) error {
	if _, exists := r.providers.Get(name); !exists {
		return fmt.Errorf("provider %s not found", name)
	}

	// Update metrics to zero before removing the provider
	UpdateConcurrencyMetrics(name, 0, 0, 0)

	r.providers.Delete(name)
	r.concurrencySemaphores.Delete(name)
	r.circuitBreakers.Delete(name)
	r.activeRequests.Delete(name)
	r.providerConfigs.Delete(name)
	r.initOnce.Delete(name)
	r.ensemble.RemoveProvider(name)
	r.requestService.RemoveProvider(name)

	return nil
}

func (r *ProviderRegistry) GetProvider(name string) (llm.LLMProvider, error) {
	// Fast path: provider already initialized
	if provider, exists := r.providers.Get(name); exists {
		return provider, nil
	}

	// Slow path: need to check config and possibly initialize
	// First, check if we have a configuration for this provider
	cfg, err := r.GetProviderConfig(name)
	if err != nil {
		return nil, fmt.Errorf("provider %s not found: %w", name, err)
	}

	// Ensure we have a single shared sync.Once for this provider.
	// PutIfAbsent makes the get-or-create atomic with no external lock.
	newOnce := &sync.Once{}
	once, _ := r.initOnce.PutIfAbsent(name, newOnce)

	// Use sync.Once to ensure only one goroutine initializes this provider
	var initErr error
	once.Do(func() {
		// Acquire semaphore to limit concurrent initializations
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := r.initSemaphore.Acquire(ctx, 1); err != nil {
			initErr = fmt.Errorf("failed to acquire initialization semaphore for provider %s: %w", name, err)
			return
		}
		defer r.initSemaphore.Release(1)

		// Double-check after acquiring semaphore
		if _, existsNow := r.providers.Get(name); existsNow {
			return // Another goroutine already initialized it
		}

		// Create provider from configuration
		newProvider, createErr := r.createProviderFromConfig(*cfg)
		if createErr != nil {
			initErr = createErr
			return
		}

		// Register the provider (with circuit breaker, etc.)
		initErr = r.RegisterProvider(name, newProvider)
	})

	if initErr != nil {
		// Clean up the sync.Once on error so retry might work
		r.initOnce.Delete(name)
		return nil, fmt.Errorf("failed to initialize provider %s: %w", name, initErr)
	}

	// Provider should now be initialized
	provider, exists := r.providers.Get(name)
	if !exists {
		return nil, fmt.Errorf("provider %s initialization failed", name)
	}

	return provider, nil
}

func (r *ProviderRegistry) ListProviders() []string {
	return r.providers.Keys()
}

// LookupModel reports whether ANY registered provider claims to support
// the given model name AND whether the registry has authoritative
// information to back that verdict (i.e. at least one provider has
// populated its SupportedModels list).
//
// Returns (found, authoritative):
//   - (true, true)   — at least one provider claims this model
//   - (false, true)  — registry has model data and none claim this name
//   - (false, false) — no provider has populated SupportedModels yet
//     (cold-start, discovery never ran, or every API
//     was unreachable at startup); caller should
//     fail-OPEN and let the request proceed
//
// The lookup is case-insensitive and matches both bare model IDs
// ("gpt-4") and provider-qualified IDs ("openai/gpt-4").
func (r *ProviderRegistry) LookupModel(name string) (found, authoritative bool) {
	if name == "" {
		return false, true // empty name is unambiguously not a known model
	}
	// Strip "provider/" prefix when present.
	bare := name
	if idx := strings.Index(name, "/"); idx >= 0 && idx+1 < len(name) {
		bare = name[idx+1:]
	}
	lowerName := strings.ToLower(name)
	lowerBare := strings.ToLower(bare)
	hasAnyModelData := false
	r.providers.Range(func(_ string, prov llm.LLMProvider) bool {
		if prov == nil {
			return true
		}
		caps := prov.GetCapabilities()
		if caps == nil {
			return true
		}
		if len(caps.SupportedModels) > 0 {
			hasAnyModelData = true
		}
		for _, m := range caps.SupportedModels {
			lm := strings.ToLower(m)
			if lm == lowerName || lm == lowerBare {
				found = true
				return false // stop iteration
			}
		}
		return true
	})
	return found, hasAnyModelData
}

// IsKnownModel is a thin wrapper around LookupModel that returns true
// when the registry has authoritative data AND the model is found.
// Callers that need cold-start fail-open behavior should use
// LookupModel directly.
func (r *ProviderRegistry) IsKnownModel(name string) bool {
	found, authoritative := r.LookupModel(name)
	return found && authoritative
}

// ListProvidersOrderedByScore returns providers ordered by their LLMsVerifier scores (highest first)
// CRITICAL: This enables dynamic provider selection based on real verification results
// Providers without scores are placed at the end with a default score of 5.0
func (r *ProviderRegistry) ListProvidersOrderedByScore() []string {
	type providerScore struct {
		name  string
		score float64
	}

	// Collect all providers with their scores
	var scored []providerScore
	r.providers.Range(func(name string, _ llm.LLMProvider) bool {
		score := 5.0 // Default score for unverified providers
		if r.scoreAdapter != nil {
			if s, found := r.scoreAdapter.GetProviderScore(name); found {
				score = s
			}
		}
		// Also check health - verified healthy providers get a bonus
		if health, exists := r.providerHealth.Get(name); exists && health.Verified && health.Status == ProviderStatusHealthy {
			score += 0.5 // Small bonus for verified healthy providers
		}
		scored = append(scored, providerScore{name: name, score: score})
		return true
	})

	// Sort by score descending (highest first)
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	// Extract names in sorted order
	result := make([]string, len(scored))
	for i, ps := range scored {
		result[i] = ps.name
	}

	return result
}

func (r *ProviderRegistry) GetEnsembleService() *EnsembleService {
	return r.ensemble
}

func (r *ProviderRegistry) GetRequestService() *RequestService {
	return r.requestService
}

func (r *ProviderRegistry) ConfigureProvider(name string, config *ProviderConfig) error {
	if _, exists := r.providers.Get(name); !exists {
		return fmt.Errorf("provider %s not found", name)
	}

	// If disabling the provider, unregister it
	if !config.Enabled {
		return r.unregisterProviderLocked(name)
	}

	// Store or update the configuration in memory
	// Make a copy to avoid external modification
	storedConfig := &ProviderConfig{
		Name:           config.Name,
		Type:           config.Type,
		Enabled:        config.Enabled,
		APIKey:         config.APIKey,
		BaseURL:        config.BaseURL,
		Timeout:        config.Timeout,
		MaxRetries:     config.MaxRetries,
		HealthCheckURL: config.HealthCheckURL,
		Weight:         config.Weight,
		Tags:           make([]string, len(config.Tags)),
		Capabilities:   make(map[string]string),
		CustomSettings: make(map[string]any),
	}

	// Copy slices and maps
	copy(storedConfig.Tags, config.Tags)
	for k, v := range config.Capabilities {
		storedConfig.Capabilities[k] = v
	}
	for k, v := range config.CustomSettings {
		storedConfig.CustomSettings[k] = v
	}

	// Copy models
	if len(config.Models) > 0 {
		storedConfig.Models = make([]ModelConfig, len(config.Models))
		for i, m := range config.Models {
			storedConfig.Models[i] = ModelConfig{
				ID:           m.ID,
				Name:         m.Name,
				Enabled:      m.Enabled,
				Weight:       m.Weight,
				Capabilities: make([]string, len(m.Capabilities)),
				CustomParams: make(map[string]any),
			}
			copy(storedConfig.Models[i].Capabilities, m.Capabilities)
			for k, v := range m.CustomParams {
				storedConfig.Models[i].CustomParams[k] = v
			}
		}
	}

	r.providerConfigs.Put(name, storedConfig)

	return nil
}

func (r *ProviderRegistry) GetProviderConfig(name string) (*ProviderConfig, error) {
	// First check if provider is registered OR has stored config — tests
	// expect "not found" when neither is true, while the lazy-init caller
	// in GetProvider reaches here with only the config present.
	_, providerExists := r.providers.Get(name)
	storedConfig, configExists := r.providerConfigs.Get(name)
	if !providerExists && !configExists {
		return nil, fmt.Errorf("provider %s not found", name)
	}

	// Return stored configuration if available
	if configExists {
		// Return a copy to prevent external modification
		configCopy := &ProviderConfig{
			Name:           storedConfig.Name,
			Type:           storedConfig.Type,
			Enabled:        storedConfig.Enabled,
			APIKey:         storedConfig.APIKey,
			BaseURL:        storedConfig.BaseURL,
			Timeout:        storedConfig.Timeout,
			MaxRetries:     storedConfig.MaxRetries,
			HealthCheckURL: storedConfig.HealthCheckURL,
			Weight:         storedConfig.Weight,
			Tags:           make([]string, len(storedConfig.Tags)),
			Capabilities:   make(map[string]string),
			CustomSettings: make(map[string]any),
		}
		copy(configCopy.Tags, storedConfig.Tags)
		for k, v := range storedConfig.Capabilities {
			configCopy.Capabilities[k] = v
		}
		for k, v := range storedConfig.CustomSettings {
			configCopy.CustomSettings[k] = v
		}
		if len(storedConfig.Models) > 0 {
			configCopy.Models = make([]ModelConfig, len(storedConfig.Models))
			for i, m := range storedConfig.Models {
				configCopy.Models[i] = ModelConfig{
					ID:           m.ID,
					Name:         m.Name,
					Enabled:      m.Enabled,
					Weight:       m.Weight,
					Capabilities: make([]string, len(m.Capabilities)),
					CustomParams: make(map[string]any),
				}
				copy(configCopy.Models[i].Capabilities, m.Capabilities)
				for k, v := range m.CustomParams {
					configCopy.Models[i].CustomParams[k] = v
				}
			}
		}
		return configCopy, nil
	}

	// Return default config if no stored configuration exists
	return &ProviderConfig{
		Name:    name,
		Enabled: true,
	}, nil
}

func (r *ProviderRegistry) HealthCheck() map[string]error {
	providers := r.providers.Snapshot()

	results := make(map[string]error)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for name, provider := range providers {
		wg.Add(1)
		go func(name string, provider llm.LLMProvider) {
			defer wg.Done()

			err := provider.HealthCheck()

			mu.Lock()
			results[name] = err
			mu.Unlock()
		}(name, provider)
	}

	wg.Wait()
	return results
}

// providerAdapter adapts llm.LLMProvider to services.LLMProvider interface
type providerAdapter struct {
	provider llm.LLMProvider
}

func (a *providerAdapter) Complete(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error) {
	return a.provider.Complete(ctx, req)
}

func (a *providerAdapter) CompleteStream(ctx context.Context, req *models.LLMRequest) (<-chan *models.LLMResponse, error) {
	return a.provider.CompleteStream(ctx, req)
}

func getDefaultRegistryConfig() *RegistryConfig {
	return &RegistryConfig{
		DefaultTimeout:        30 * time.Second,
		MaxRetries:            3,
		MaxConcurrentRequests: 10,
		HealthCheck: HealthCheckConfig{
			Enabled:          true,
			Interval:         60 * time.Second,
			Timeout:          10 * time.Second,
			FailureThreshold: 3,
		},
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:          true,
			FailureThreshold: 5,
			RecoveryTimeout:  60 * time.Second,
			SuccessThreshold: 2,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy:            "confidence_weighted",
			MinProviders:        2,
			ConfidenceThreshold: 0.8,
			FallbackToBest:      true,
			Timeout:             30,
			PreferredProviders:  []string{},
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
			Weights:  make(map[string]float64),
		},
	}
}

// LoadRegistryConfigFromAppConfig converts application config to registry config
func LoadRegistryConfigFromAppConfig(appConfig *config.Config) *RegistryConfig {
	cfg := getDefaultRegistryConfig()

	// Override with application config if provided
	if appConfig != nil {
		if appConfig.LLM.DefaultTimeout > 0 {
			cfg.DefaultTimeout = appConfig.LLM.DefaultTimeout
		}

		if appConfig.LLM.MaxRetries > 0 {
			cfg.MaxRetries = appConfig.LLM.MaxRetries
		}

		if appConfig.LLM.DisableAutoDiscovery {
			cfg.DisableAutoDiscovery = true
		}
	}

	// Load provider configurations from environment variables
	// Providers are only enabled if their API key is configured

	deepseekKey := os.Getenv("DEEPSEEK_API_KEY")
	cfg.Providers["deepseek"] = &ProviderConfig{
		Name:    "deepseek",
		Type:    "deepseek",
		Enabled: deepseekKey != "",
		Models: []ModelConfig{{
			ID:      getEnvOrDefault("DEEPSEEK_MODEL", "deepseek-coder"),
			Name:    "DeepSeek Coder",
			Enabled: true,
			Weight:  1.0,
		}},
		APIKey:  deepseekKey,
		BaseURL: os.Getenv("DEEPSEEK_BASE_URL"),
		Timeout: cfg.DefaultTimeout,
		Weight:  1.0,
	}

	claudeKey := os.Getenv("ANTHROPIC_API_KEY")
	cfg.Providers["claude"] = &ProviderConfig{
		Name:    "claude",
		Type:    "claude",
		Enabled: claudeKey != "",
		Models: []ModelConfig{{
			ID:      getEnvOrDefault("CLAUDE_MODEL", "claude-3-sonnet-20240229"),
			Name:    "Claude 3 Sonnet",
			Enabled: true,
			Weight:  1.0,
		}},
		APIKey:  claudeKey,
		BaseURL: os.Getenv("ANTHROPIC_BASE_URL"),
		Timeout: cfg.DefaultTimeout,
		Weight:  1.0,
	}

	geminiKey := os.Getenv("GEMINI_API_KEY")
	cfg.Providers["gemini"] = &ProviderConfig{
		Name:    "gemini",
		Type:    "gemini",
		Enabled: geminiKey != "",
		Models: []ModelConfig{{
			ID:      getEnvOrDefault("GEMINI_MODEL", "gemini-pro"),
			Name:    "Gemini Pro",
			Enabled: true,
			Weight:  1.0,
		}},
		APIKey:  geminiKey,
		BaseURL: os.Getenv("GEMINI_BASE_URL"),
		Timeout: cfg.DefaultTimeout,
		Weight:  1.0,
	}

	qwenKey := os.Getenv("QWEN_API_KEY")
	cfg.Providers["qwen"] = &ProviderConfig{
		Name:    "qwen",
		Type:    "qwen",
		Enabled: qwenKey != "",
		Models: []ModelConfig{{
			ID:      getEnvOrDefault("QWEN_MODEL", "qwen-turbo"),
			Name:    "Qwen Turbo",
			Enabled: true,
			Weight:  1.0,
		}},
		APIKey:  qwenKey,
		BaseURL: os.Getenv("QWEN_BASE_URL"),
		Timeout: cfg.DefaultTimeout,
		Weight:  1.0,
	}

	openrouterKey := os.Getenv("OPENROUTER_API_KEY")
	cfg.Providers["openrouter"] = &ProviderConfig{
		Name:    "openrouter",
		Type:    "openrouter",
		Enabled: openrouterKey != "",
		Models: []ModelConfig{{
			ID:      getEnvOrDefault("OPENROUTER_MODEL", "x-ai/grok-4"),
			Name:    "Grok-4 via OpenRouter",
			Enabled: true,
			Weight:  1.3,
		}},
		APIKey:  openrouterKey,
		Timeout: cfg.DefaultTimeout,
		Weight:  1.3,
	}

	return cfg
}

// getEnvOrDefault returns the environment variable value or a default
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// VerifyProvider tests a provider with an actual API call and returns the verification result
// This is a critical function that ensures providers actually work before being used in the ensemble
func (r *ProviderRegistry) VerifyProvider(ctx context.Context, providerName string) *ProviderVerificationResult {
	start := time.Now()
	result := &ProviderVerificationResult{
		Provider: providerName,
		Status:   ProviderStatusUnknown,
		Verified: false,
		TestedAt: time.Now(),
	}

	// Get the provider
	provider, exists := r.providers.Get(providerName)
	if !exists {
		result.Status = ProviderStatusUnhealthy
		result.Error = "provider not registered"
		return result
	}

	// Create a simple test request
	testReq := &models.LLMRequest{
		ID:        fmt.Sprintf("verify_%s_%d", providerName, time.Now().UnixNano()),
		SessionID: "verification",
		Prompt:    "Say OK",
		Messages: []models.Message{
			{Role: "user", Content: "Say OK"},
		},
		ModelParams: models.ModelParameters{
			MaxTokens:   5,
			Temperature: 0.1,
		},
		Status:    "pending",
		CreatedAt: time.Now(),
	}

	// Create context with timeout for verification
	verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Make the actual API call
	resp, err := provider.Complete(verifyCtx, testReq)
	result.ResponseTime = time.Since(start)

	if err != nil {
		errStr := err.Error()

		// Categorize the error
		switch {
		case containsAny(errStr, "429", "quota", "rate", "RESOURCE_EXHAUSTED"):
			result.Status = ProviderStatusRateLimited
			result.Error = "rate limited or quota exceeded"
		case containsAny(errStr, "401", "403", "unauthorized", "invalid", "authentication", "API_KEY"):
			result.Status = ProviderStatusAuthFailed
			result.Error = "authentication failed or invalid API key"
		default:
			result.Status = ProviderStatusUnhealthy
			result.Error = errStr
		}
		return result
	}

	// Check for valid response
	if resp != nil && resp.Content != "" {
		result.Status = ProviderStatusHealthy
		result.Verified = true
	} else {
		result.Status = ProviderStatusUnhealthy
		result.Error = "empty response from provider"
	}

	// Store the result
	r.providerHealth.Put(providerName, result)

	return result
}

// VerifyAllProviders verifies all registered providers and returns their status
func (r *ProviderRegistry) VerifyAllProviders(ctx context.Context) map[string]*ProviderVerificationResult {
	results := make(map[string]*ProviderVerificationResult)

	providerNames := r.providers.Keys()

	// Verify providers concurrently
	var wg sync.WaitGroup
	resultsChan := make(chan *ProviderVerificationResult, len(providerNames))

	for _, name := range providerNames {
		wg.Add(1)
		go func(providerName string) {
			defer wg.Done()
			result := r.VerifyProvider(ctx, providerName)
			resultsChan <- result
		}(name)
	}

	// Wait for all verifications to complete
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Collect results
	for result := range resultsChan {
		results[result.Provider] = result
	}

	return results
}

// GetProviderHealth returns the last verification result for a provider
func (r *ProviderRegistry) GetProviderHealth(providerName string) *ProviderVerificationResult {
	h, _ := r.providerHealth.Get(providerName)
	return h
}

// GetAllProviderHealth returns all provider health verification results
func (r *ProviderRegistry) GetAllProviderHealth() map[string]*ProviderVerificationResult {
	return r.providerHealth.Snapshot()
}

// IsProviderHealthy returns true if the provider has been verified as healthy
func (r *ProviderRegistry) IsProviderHealthy(providerName string) bool {
	health, exists := r.providerHealth.Get(providerName)
	if !exists {
		return false // Not verified yet, assume unhealthy
	}
	return health.Status == ProviderStatusHealthy && health.Verified
}

// GetHealthyProviders returns a list of providers that have been verified as healthy
func (r *ProviderRegistry) GetHealthyProviders() []string {
	healthy := make([]string, 0)
	r.providerHealth.Range(func(name string, health *ProviderVerificationResult) bool {
		if health.Status == ProviderStatusHealthy && health.Verified {
			healthy = append(healthy, name)
		}
		return true
	})
	return healthy
}

// containsAny checks if the string contains any of the substrings (case-insensitive)
func containsAny(s string, substrs ...string) bool {
	sLower := strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(sLower, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

// createProviderFromConfig creates a provider instance from configuration without registering it.
func (r *ProviderRegistry) createProviderFromConfig(cfg ProviderConfig) (llm.LLMProvider, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("provider name is required")
	}
	model := getFirstModel(cfg.Models)
	baseURL := cfg.BaseURL

	switch cfg.Type {
	case "claude":
		// Try API key first (direct API access), then CLI proxy for OAuth
		if cfg.Enabled && cfg.APIKey != "" {
			// API key available - use direct API access
			provider := claude.NewClaudeProvider(cfg.APIKey, baseURL, model)
			logrus.WithField("provider", cfg.Name).Info("Created Claude provider with API key (direct API access)")
			return provider, nil
		} else if r.autoDiscovery && oauth_credentials.IsClaudeOAuthEnabled() {
			// OAuth credentials present - use CLI proxy
			credReader := oauth_credentials.GetGlobalReader()
			if credReader.HasValidClaudeCredentials() {
				// Check if Claude CLI is available
				cliProvider := claude.NewClaudeCLIProviderWithModel(model)
				if cliProvider.IsCLIAvailable() {
					logrus.WithField("provider", cfg.Name).Info("Created Claude provider with CLI proxy (OAuth via claude command)")
					return cliProvider, nil
				} else {
					logrus.WithField("provider", cfg.Name).Warn("Claude OAuth credentials found but Claude CLI not available")
				}
			}
		}
		return nil, fmt.Errorf("Claude provider not available: no API key or valid OAuth credentials")

	case "deepseek":
		if cfg.Enabled && cfg.APIKey != "" {
			return deepseek.NewDeepSeekProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("DeepSeek provider not available: API key missing or disabled")

	case "gemini":
		if cfg.Enabled {
			config := gemini.DefaultGeminiUnifiedConfig()
			if cfg.APIKey != "" {
				config.APIKey = cfg.APIKey
			}
			if model != "" {
				config.Model = model
			}
			if baseURL != "" {
				config.BaseURL = baseURL
			}
			provider := gemini.NewGeminiUnifiedProvider(config)
			if config.APIKey != "" {
				logrus.WithField("provider", cfg.Name).Info("Created Gemini provider with API key (direct API access)")
			} else if gemini.IsGeminiCLIInstalled() {
				logrus.WithField("provider", cfg.Name).Info("Created Gemini provider with CLI proxy")
			}
			return provider, nil
		}
		return nil, fmt.Errorf("Gemini provider not available: disabled")

	case "helixllm":
		if cfg.Enabled {
			// HELIX_LLM_USE_LLAMACPP defaults to false — when the local
			// llama.cpp container isn't running (the common case during
			// development), HelixLLM should rely on its cloud chain
			// (Chutes, OpenRouter, HuggingFace, Nvidia, Cerebras,
			// SambaNova, Together) instead of timing out on a local
			// backend that doesn't exist. Operators who want llama.cpp
			// in the rotation set HELIX_LLM_USE_LLAMACPP=true.
			useLlamaCpp := strings.EqualFold(os.Getenv("HELIX_LLM_USE_LLAMACPP"), "true")
			// Endpoint is left to the provider's resolveEndpoint precedence
			// (CONST-045: no hardcoded host here). Explicit config baseURL wins;
			// otherwise HELIX_LLM_LOCAL_OPENAI_ENDPOINT (local plain-HTTP OpenAI
			// router) → HELIX_LLM_ENDPOINT (general override) → the TLS :8443
			// default. To reach a local plain-HTTP HelixLLM router an operator
			// sets HELIX_LLM_LOCAL_OPENAI_ENDPOINT=http://<host>:<port> (or
			// HELIX_LLM_ENDPOINT), no code change required.
			config := helixllm.Config{
				Endpoint:      baseURL, // empty ⇒ provider resolves via env/default
				APIKey:        cfg.APIKey,
				Model:         model,
				TLSSkipVerify: os.Getenv("HELIX_LLM_TLS_SKIP_VERIFY") == "true",
				UseLlamaCpp:   useLlamaCpp,
			}
			provider := helixllm.NewProvider(config)
			logrus.WithFields(logrus.Fields{
				"provider":     cfg.Name,
				"use_llamacpp": useLlamaCpp,
				"endpoint":     provider.Endpoint(),
			}).Info("Created HelixLLM provider")
			return provider, nil
		}
		return nil, fmt.Errorf("HelixLLM provider not available: disabled via USE_HELIX_LLM=false (the local chain is on by default; unset USE_HELIX_LLM or set it to any value other than an explicit false to re-enable)")

	case "qwen":
		// Try API key first (direct API access), then ACP, then CLI proxy for OAuth
		if cfg.Enabled && cfg.APIKey != "" {
			// API key available - use direct API access
			provider := qwen.NewQwenProvider(cfg.APIKey, baseURL, model)
			logrus.WithField("provider", cfg.Name).Info("Created Qwen provider with API key (direct API access)")
			return provider, nil
		} else if r.autoDiscovery && oauth_credentials.IsQwenOAuthEnabled() {
			// OAuth credentials present - try ACP first (more powerful), then CLI proxy
			credReader := oauth_credentials.GetGlobalReader()
			if credReader.HasValidQwenCredentials() {
				// Try ACP first (more powerful - sessions, streaming, etc.)
				if qwen.CanUseQwenACP() {
					acpProvider := qwen.NewQwenACPProviderWithModel(model)
					if acpProvider.IsAvailable() {
						logrus.WithField("provider", cfg.Name).Info("Created Qwen provider with ACP (Agent Communication Protocol)")
						return acpProvider, nil
					}
				}
				// Fall back to CLI proxy if ACP not available
				cliProvider := qwen.NewQwenCLIProviderWithModel(model)
				if cliProvider.IsCLIAvailable() {
					logrus.WithField("provider", cfg.Name).Info("Created Qwen provider with CLI proxy (OAuth via qwen command)")
					return cliProvider, nil
				} else {
					logrus.WithField("provider", cfg.Name).Warn("Qwen OAuth credentials found but Qwen CLI not available")
				}
			}
		}
		return nil, fmt.Errorf("Qwen provider not available: no API key or valid OAuth credentials")

	case "openrouter":
		if cfg.Enabled && cfg.APIKey != "" {
			return openrouter.NewSimpleOpenRouterProviderWithBaseURL(cfg.APIKey, baseURL), nil
		}
		return nil, fmt.Errorf("OpenRouter provider not available: API key missing or disabled")

	case "mistral":
		if cfg.Enabled && cfg.APIKey != "" {
			return mistral.NewMistralProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Mistral provider not available: API key missing or disabled")

	case "zai":
		if cfg.Enabled && cfg.APIKey != "" {
			return zai.NewZAIProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("ZAI provider not available: API key missing or disabled")

	case "zen":
		if cfg.Enabled {
			if cfg.APIKey != "" {
				return zen.NewZenProvider(cfg.APIKey, baseURL, model), nil
			} else {
				// Anonymous mode (free)
				return zen.NewZenProviderAnonymous(model), nil
			}
		}
		return nil, fmt.Errorf("Zen provider not available: disabled")

	case "cerebras":
		if cfg.Enabled && cfg.APIKey != "" {
			return cerebras.NewCerebrasProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Cerebras provider not available: API key missing or disabled")

	case "publicai":
		if cfg.Enabled && cfg.APIKey != "" {
			return publicai.NewPublicAIProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Public AI provider not available: API key missing or disabled")

	case "codestral":
		if cfg.Enabled && cfg.APIKey != "" {
			return codestral.NewCodestralProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Codestral provider not available: API key missing or disabled")

	case "nvidia":
		if cfg.Enabled && cfg.APIKey != "" {
			return nvidia.NewNvidiaProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("NVIDIA provider not available: API key missing or disabled")

	case "ollama":
		if cfg.Enabled && baseURL != "" {
			return ollama.NewOllamaProvider(baseURL, model), nil
		}
		return nil, fmt.Errorf("Ollama provider not available: base URL missing or disabled")

	case "openai":
		if cfg.Enabled && cfg.APIKey != "" {
			return openai.NewProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("OpenAI provider not available: API key missing or disabled")

	case "anthropic":
		if cfg.Enabled && cfg.APIKey != "" {
			return anthropic.NewProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Anthropic provider not available: API key missing or disabled")

	case "cohere":
		if cfg.Enabled && cfg.APIKey != "" {
			return cohere.NewProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Cohere provider not available: API key missing or disabled")

	case "fireworks":
		if cfg.Enabled && cfg.APIKey != "" {
			return fireworks.NewProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Fireworks provider not available: API key missing or disabled")

	case "groq":
		if cfg.Enabled && cfg.APIKey != "" {
			return groq.NewProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Groq provider not available: API key missing or disabled")

	case "huggingface":
		if cfg.Enabled && cfg.APIKey != "" {
			return huggingface.NewProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("HuggingFace provider not available: API key missing or disabled")

	case "perplexity":
		if cfg.Enabled && cfg.APIKey != "" {
			return perplexity.NewProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Perplexity provider not available: API key missing or disabled")

	case "replicate":
		if cfg.Enabled && cfg.APIKey != "" {
			return replicate.NewProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Replicate provider not available: API key missing or disabled")

	case "together":
		if cfg.Enabled && cfg.APIKey != "" {
			return together.NewProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Together provider not available: API key missing or disabled")

	case "xai":
		if cfg.Enabled && cfg.APIKey != "" {
			return xai.NewProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("XAI provider not available: API key missing or disabled")

	case "ai21":
		if cfg.Enabled && cfg.APIKey != "" {
			return ai21.NewProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("AI21 provider not available: API key missing or disabled")

	case "chutes":
		if cfg.Enabled && cfg.APIKey != "" {
			return chutes.NewProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Chutes provider not available: API key missing or disabled")

	case "github-models":
		if cfg.Enabled && cfg.APIKey != "" {
			return githubmodels.NewGitHubModelsProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("GitHub Models provider not available: API key missing or disabled")

	case "cloudflare":
		if cfg.Enabled && cfg.APIKey != "" {
			accountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
			return cloudflare.NewCloudflareProvider(cfg.APIKey, accountID, baseURL, model), nil
		}
		return nil, fmt.Errorf("Cloudflare provider not available: API key missing or disabled")

	case "kimi":
		if cfg.Enabled && cfg.APIKey != "" {
			return kimi.NewKimiProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Kimi provider not available: API key missing or disabled")

	case "xiaomi":
		if cfg.Enabled && cfg.APIKey != "" {
			return xiaomi.NewXiaomiProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Xiaomi MiMo provider not available: API key missing or disabled")

	case "kimi-code", "kimicode":
		if cfg.Enabled {
			if kimicode.CanUseKimiCodeCLI() {
				return kimicode.NewKimiCodeCLIProvider(kimicode.DefaultKimiCodeCLIConfig()), nil
			}
			return nil, fmt.Errorf("Kimi Code CLI provider not available: OAuth credentials not configured or CLI not authenticated")
		}
		return nil, fmt.Errorf("Kimi Code provider not available: disabled")

	case "sambanova":
		if cfg.Enabled && cfg.APIKey != "" {
			return sambanova.NewSambaNovaProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("SambaNova provider not available: API key missing or disabled")

	case "upstage":
		if cfg.Enabled && cfg.APIKey != "" {
			return upstage.NewUpstageProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Upstage provider not available: API key missing or disabled")

	case "sarvam":
		if cfg.Enabled && cfg.APIKey != "" {
			return sarvam.NewSarvamProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Sarvam provider not available: API key missing or disabled")

	case "zhipu":
		if cfg.Enabled && cfg.APIKey != "" {
			return zhipu.NewZhipuProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Zhipu provider not available: API key missing or disabled")

	case "kilo":
		if cfg.Enabled && cfg.APIKey != "" {
			return kilo.NewKiloProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Kilo provider not available: API key missing or disabled")

	case "modal":
		if cfg.Enabled && cfg.APIKey != "" {
			apiKeyID := os.Getenv("MODAL_API_KEY_ID")
			return modal.NewModalProvider(cfg.APIKey, apiKeyID, baseURL, model), nil
		}
		return nil, fmt.Errorf("Modal provider not available: API key missing or disabled")

	case "nia":
		if cfg.Enabled && cfg.APIKey != "" {
			return nia.NewNiaProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Nia provider not available: API key missing or disabled")

	case "nlpcloud":
		if cfg.Enabled && cfg.APIKey != "" {
			return nlpcloud.NewNLPCloudProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("NLPCloud provider not available: API key missing or disabled")

	case "vulavula":
		if cfg.Enabled && cfg.APIKey != "" {
			return vulavula.NewVulavulaProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Vulavula provider not available: API key missing or disabled")

	case "siliconflow":
		if cfg.Enabled && cfg.APIKey != "" {
			return siliconflow.NewSiliconFlowProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("SiliconFlow provider not available: API key missing or disabled")

	case "hyperbolic":
		if cfg.Enabled && cfg.APIKey != "" {
			return hyperbolic.NewHyperbolicProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Hyperbolic provider not available: API key missing or disabled")

	case "novita":
		if cfg.Enabled && cfg.APIKey != "" {
			return novita.NewNovitaProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Novita provider not available: API key missing or disabled")

	case "junie":
		if cfg.Enabled {
			junieConfig := junie.DefaultJunieConfig()
			if cfg.APIKey != "" {
				junieConfig.APIKey = cfg.APIKey
			}
			if model != "" {
				junieConfig.Model = model
			}
			if baseURL != "" {
				junieConfig.PreferredMethod = baseURL
			}
			return junie.NewJunieProvider(junieConfig), nil
		}
		return nil, fmt.Errorf("Junie provider not available: disabled")

	case "venice":
		if cfg.Enabled && cfg.APIKey != "" {
			return venice.NewProvider(cfg.APIKey, baseURL, model), nil
		}
		return nil, fmt.Errorf("Venice provider not available: API key missing or disabled")

	default:
		return nil, fmt.Errorf("unsupported provider type: %s", cfg.Type)
	}
}

// RegisterProviderFromConfig creates and registers a provider from configuration
func (r *ProviderRegistry) RegisterProviderFromConfig(cfg ProviderConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("provider name is required")
	}

	// Create provider based on type
	var provider llm.LLMProvider
	model := getFirstModel(cfg.Models)
	baseURL := cfg.BaseURL

	switch cfg.Type {
	case "claude":
		provider = claude.NewClaudeProvider(cfg.APIKey, baseURL, model)
	case "deepseek":
		provider = deepseek.NewDeepSeekProvider(cfg.APIKey, baseURL, model)
	case "gemini":
		provider = gemini.NewGeminiProvider(cfg.APIKey, baseURL, model)
	case "qwen":
		provider = qwen.NewQwenProvider(cfg.APIKey, baseURL, model)
	case "openrouter":
		provider = openrouter.NewSimpleOpenRouterProviderWithBaseURL(cfg.APIKey, baseURL)
	case "github-models":
		provider = githubmodels.NewGitHubModelsProvider(cfg.APIKey, baseURL, model)
	case "lmstudio":
		provider = lmstudio.NewProvider(baseURL, model)
	case "together":
		provider = together.NewProvider(cfg.APIKey, baseURL, model)
	case "azure-openai":
		provider = azure.NewProvider(baseURL, cfg.Name, cfg.APIKey)
	case "cohere":
		provider = cohere.NewProvider(cfg.APIKey, baseURL, model)
	case "replicate":
		provider = replicate.NewProvider(cfg.APIKey, baseURL, model)
	case "ai21":
		provider = ai21.NewProvider(cfg.APIKey, baseURL, model)
	case "anthropic-cu":
		provider = anthropic_cu.NewProvider(anthropic_cu.Config{
			APIKey: cfg.APIKey,
			Model:  model,
		})
	case "vertex":
		provider = vertex.NewProvider(vertex.Config{
			ProjectID: cfg.ProjectID,
			Location:  cfg.Location,
			Model:     model,
			APIKey:    cfg.APIKey,
		})
	case "generic", "hyper":
		genericCfg := generic.Config{
			APIKey:  cfg.APIKey,
			BaseURL: cfg.BaseURL,
			Model:   model,
			Name:    cfg.Name,
		}
		provider = generic.NewProvider(genericCfg)
	default:
		return fmt.Errorf("unsupported provider type: %s", cfg.Type)
	}

	// Store the config. r.config itself is not a shared collection (it's
	// the immutable configuration object passed in at construction), so
	// plain map assignment remains safe with no additional synchronisation.
	r.config.Providers[cfg.Name] = &cfg

	// Register the provider
	return r.RegisterProvider(cfg.Name, provider)
}

// UpdateProvider updates a provider's configuration
func (r *ProviderRegistry) UpdateProvider(name string, cfg ProviderConfig) error {
	if _, exists := r.providers.Get(name); !exists {
		return fmt.Errorf("provider %s not found", name)
	}

	// Update stored config
	if existingConfig, exists := r.config.Providers[name]; exists {
		if cfg.APIKey != "" {
			existingConfig.APIKey = cfg.APIKey
		}
		if cfg.BaseURL != "" {
			existingConfig.BaseURL = cfg.BaseURL
		}
		if cfg.Weight != 0 {
			existingConfig.Weight = cfg.Weight
		}
		if len(cfg.Models) > 0 {
			existingConfig.Models = cfg.Models
		}
		existingConfig.Enabled = cfg.Enabled
	}

	return nil
}

// RemoveProvider removes a provider with optional force flag
// If force is false and there are active requests, it will attempt graceful shutdown
// by waiting for requests to drain up to the configured drain timeout
func (r *ProviderRegistry) RemoveProvider(name string, force bool) error {
	if _, exists := r.providers.Get(name); !exists {
		return fmt.Errorf("provider %s not found", name)
	}

	// Check for active requests
	counter, hasCounter := r.activeRequests.Get(name)
	if !force && hasCounter && counter != nil {
		activeCount := atomic.LoadInt64(counter)
		if activeCount > 0 {
			if err := r.drainProviderRequests(name); err != nil {
				return fmt.Errorf("provider %s has active requests and drain failed: %w", name, err)
			}
		}
	}

	// Remove provider and associated data
	r.providers.Delete(name)
	delete(r.config.Providers, name)
	r.providerConfigs.Delete(name)
	r.circuitBreakers.Delete(name)
	r.activeRequests.Delete(name)

	r.ensemble.RemoveProvider(name)
	r.requestService.RemoveProvider(name)

	return nil
}

// drainProviderRequests waits for active requests to complete up to the drain timeout
func (r *ProviderRegistry) drainProviderRequests(name string) error {
	counter, exists := r.activeRequests.Get(name)
	drainTimeout := r.drainTimeout

	if !exists || counter == nil {
		return nil
	}

	deadline := time.Now().Add(drainTimeout)
	pollInterval := 100 * time.Millisecond

	for time.Now().Before(deadline) {
		activeCount := atomic.LoadInt64(counter)
		if activeCount <= 0 {
			return nil
		}
		time.Sleep(pollInterval)
	}

	// Check one final time
	finalCount := atomic.LoadInt64(counter)
	if finalCount > 0 {
		return fmt.Errorf("timeout waiting for %d active requests to complete", finalCount)
	}

	return nil
}

// IncrementActiveRequests increments the active request counter for a provider
// Returns false if the provider doesn't exist
func (r *ProviderRegistry) IncrementActiveRequests(name string) bool {
	counter, exists := r.activeRequests.Get(name)
	if !exists || counter == nil {
		return false
	}

	atomic.AddInt64(counter, 1)
	return true
}

// DecrementActiveRequests decrements the active request counter for a provider
// Returns false if the provider doesn't exist
func (r *ProviderRegistry) DecrementActiveRequests(name string) bool {
	counter, exists := r.activeRequests.Get(name)
	if !exists || counter == nil {
		return false
	}

	atomic.AddInt64(counter, -1)
	return true
}

// GetActiveRequestCount returns the number of active requests for a provider
// Returns -1 if the provider doesn't exist
func (r *ProviderRegistry) GetActiveRequestCount(name string) int64 {
	counter, exists := r.activeRequests.Get(name)
	if !exists || counter == nil {
		return -1
	}

	return atomic.LoadInt64(counter)
}

// GetConcurrencyStats returns concurrency statistics for a provider
func (r *ProviderRegistry) GetConcurrencyStats(name string) (*ConcurrencyStats, error) {
	// Check if provider exists
	provider, exists := r.providers.Get(name)
	if !exists {
		return nil, fmt.Errorf("provider %s not found", name)
	}

	// Type assert to circuitBreakerProvider to access concurrency fields
	cbp, ok := provider.(*circuitBreakerProvider)
	if !ok {
		// Provider is not wrapped with circuit breaker (shouldn't happen for registered providers)
		return &ConcurrencyStats{
			Provider:         name,
			HasSemaphore:     false,
			TotalPermits:     0,
			AcquiredPermits:  0,
			ActiveRequests:   0,
			AvailablePermits: 0,
			SemaphoreExists:  false,
		}, nil
	}

	// Get active requests count
	activeRequests := int64(0)
	if cbp.activeRequestsCounter != nil {
		activeRequests = atomic.LoadInt64(cbp.activeRequestsCounter)
	}

	// Get acquired permits
	acquiredPermits := atomic.LoadInt64(&cbp.acquiredPermits)
	totalPermits := cbp.totalPermits
	hasSemaphore := cbp.concurrencySemaphore != nil

	return &ConcurrencyStats{
		Provider:          name,
		HasSemaphore:      hasSemaphore,
		TotalPermits:      totalPermits,
		AcquiredPermits:   acquiredPermits,
		ActiveRequests:    activeRequests,
		AvailablePermits:  totalPermits - acquiredPermits,
		SemaphoreExists:   hasSemaphore,
		SemaphoreCapacity: totalPermits,
	}, nil
}

// GetAllConcurrencyStats returns concurrency statistics for all registered providers
func (r *ProviderRegistry) GetAllConcurrencyStats() map[string]*ConcurrencyStats {
	stats := make(map[string]*ConcurrencyStats)
	r.providers.Range(func(name string, provider llm.LLMProvider) bool {
		cbp, ok := provider.(*circuitBreakerProvider)
		if !ok {
			return true
		}

		// Get active requests count
		activeRequests := int64(0)
		if cbp.activeRequestsCounter != nil {
			activeRequests = atomic.LoadInt64(cbp.activeRequestsCounter)
		}

		// Get acquired permits
		acquiredPermits := atomic.LoadInt64(&cbp.acquiredPermits)
		totalPermits := cbp.totalPermits
		hasSemaphore := cbp.concurrencySemaphore != nil

		stats[name] = &ConcurrencyStats{
			Provider:          name,
			HasSemaphore:      hasSemaphore,
			TotalPermits:      totalPermits,
			AcquiredPermits:   acquiredPermits,
			ActiveRequests:    activeRequests,
			AvailablePermits:  totalPermits - acquiredPermits,
			SemaphoreExists:   hasSemaphore,
			SemaphoreCapacity: totalPermits,
		}
		return true
	})
	return stats
}

// ConcurrencyStats represents concurrency statistics for a provider
type ConcurrencyStats struct {
	Provider          string `json:"provider"`
	HasSemaphore      bool   `json:"has_semaphore"`
	TotalPermits      int64  `json:"total_permits"`
	AcquiredPermits   int64  `json:"acquired_permits"`
	ActiveRequests    int64  `json:"active_requests"`
	AvailablePermits  int64  `json:"available_permits"`
	SemaphoreExists   bool   `json:"semaphore_exists"`
	SemaphoreCapacity int64  `json:"semaphore_capacity"`
}

// SetDrainTimeout sets the timeout for graceful shutdown request draining
func (r *ProviderRegistry) SetDrainTimeout(timeout time.Duration) {
	r.drainTimeout = timeout
}

// getFirstModel returns the first model ID from a list of models
func getFirstModel(models []ModelConfig) string {
	if len(models) > 0 {
		return models[0].ID
	}
	return ""
}

// GetKnownProviderTypes returns provider types that have implementations in the codebase
// DYNAMIC: This reads from providerMappings in provider_discovery.go instead of hardcoding
// This ensures new provider implementations are automatically recognized
func (r *ProviderRegistry) GetKnownProviderTypes() []string {
	seen := make(map[string]bool)
	types := make([]string, 0)

	// Get types from provider mappings (these are the implementations we have)
	for _, mapping := range providerMappings {
		if mapping.ProviderType != "" && !seen[mapping.ProviderType] {
			seen[mapping.ProviderType] = true
			types = append(types, mapping.ProviderType)
		}
	}

	// Also include types from currently configured providers
	for _, cfg := range r.config.Providers {
		if cfg.Type != "" && !seen[cfg.Type] {
			seen[cfg.Type] = true
			types = append(types, cfg.Type)
		}
	}

	return types
}
