package services

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"dev.helix.agent/internal/llm"
	"dev.helix.agent/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"
)

// =============================================================================
// Mock LLM Provider for Registry Tests
// =============================================================================

type registryUnitTestMockProvider struct {
	name           string
	completeFunc   func(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error)
	healthCheckErr error
	capabilities   *models.ProviderCapabilities
	callCount      int64
}

func newRegistryMockProvider(name string) *registryUnitTestMockProvider {
	return &registryUnitTestMockProvider{
		name: name,
		capabilities: &models.ProviderCapabilities{
			SupportsStreaming: true,
			SupportedFeatures: []string{"text"},
			SupportedModels:   []string{"test-model"},
		},
	}
}

func (m *registryUnitTestMockProvider) Complete(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error) {
	atomic.AddInt64(&m.callCount, 1)

	if m.completeFunc != nil {
		return m.completeFunc(ctx, req)
	}

	return &models.LLMResponse{
		Content:      "test response",
		ProviderName: m.name,
		Confidence:   0.9,
	}, nil
}

func (m *registryUnitTestMockProvider) CompleteStream(ctx context.Context, req *models.LLMRequest) (<-chan *models.LLMResponse, error) {
	ch := make(chan *models.LLMResponse, 1)
	ch <- &models.LLMResponse{Content: "stream response"}
	close(ch)
	return ch, nil
}

func (m *registryUnitTestMockProvider) HealthCheck() error {
	return m.healthCheckErr
}

func (m *registryUnitTestMockProvider) GetCapabilities() *models.ProviderCapabilities {
	if m.capabilities != nil {
		return m.capabilities
	}
	return &models.ProviderCapabilities{
		SupportsStreaming: true,
		SupportedFeatures: []string{"text"},
		SupportedModels:   []string{"test-model"},
	}
}

func (m *registryUnitTestMockProvider) ValidateConfig(config map[string]interface{}) (bool, []string) {
	return true, nil
}

func (m *registryUnitTestMockProvider) getCallCount() int {
	return int(atomic.LoadInt64(&m.callCount))
}

var _ llm.LLMProvider = (*registryUnitTestMockProvider)(nil)

// =============================================================================
// Registry Creation Tests
// =============================================================================

func TestProviderRegistryUnit_NewProviderRegistry(t *testing.T) {
	t.Run("with nil config", func(t *testing.T) {
		registry := NewProviderRegistryWithoutAutoDiscovery(nil, nil)

		require.NotNil(t, registry)
		assert.NotNil(t, registry.providers)
		assert.NotNil(t, registry.circuitBreakers)
		assert.NotNil(t, registry.config)
		assert.NotNil(t, registry.ensemble)
		assert.NotNil(t, registry.requestService)
		assert.NotNil(t, registry.providerConfigs)
		assert.NotNil(t, registry.providerHealth)
		assert.NotNil(t, registry.activeRequests)
		assert.NotNil(t, registry.initOnce)
		assert.NotNil(t, registry.initSemaphore)
	})

	t.Run("with custom config", func(t *testing.T) {
		cfg := &RegistryConfig{
			DefaultTimeout:        60 * time.Second,
			MaxRetries:            5,
			MaxConcurrentRequests: 20,
			HealthCheck: HealthCheckConfig{
				Enabled:          true,
				Interval:         30 * time.Second,
				Timeout:          5 * time.Second,
				FailureThreshold: 2,
			},
			CircuitBreaker: CircuitBreakerConfig{
				Enabled:          true,
				FailureThreshold: 3,
				RecoveryTimeout:  30 * time.Second,
				SuccessThreshold: 1,
			},
			Providers: make(map[string]*ProviderConfig),
			Ensemble: &models.EnsembleConfig{
				Strategy: "majority_vote",
			},
			Routing: &RoutingConfig{
				Strategy: "round_robin",
			},
		}

		registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

		require.NotNil(t, registry)
		assert.Equal(t, 60*time.Second, registry.config.DefaultTimeout)
		assert.Equal(t, 5, registry.config.MaxRetries)
		assert.Equal(t, 20, registry.config.MaxConcurrentRequests)
	})
}

func TestProviderRegistryUnit_NewProviderRegistry_WithAutoDiscovery(t *testing.T) {
	// CONST-035 anti-bluff: a unit test for the constructor must not
	// silently perform live provider auto-discovery against the dev
	// host's real API keys (16+ s of work, HTTPS calls). Scrub every
	// recognised provider env-var so the auto-discovery code path
	// executes against a clean environment and returns in
	// milliseconds.
	clearProviderEnvVarsForTest(t)

	// HA-F2-002: cloud auto-discovery is default-OFF; this test exercises
	// the discovery-enabled path, so opt in explicitly. The property under
	// test (the code path executes and returns fast against a clean env) is
	// unchanged.
	t.Setenv("HELIX_CLOUD_PROVIDERS", "true")
	cfg := &RegistryConfig{
		DefaultTimeout:       30 * time.Second,
		MaxRetries:           3,
		DisableAutoDiscovery: false,
		Providers:            make(map[string]*ProviderConfig),
	}

	registry := NewProviderRegistry(cfg, nil)

	require.NotNil(t, registry)
	// Auto-discovery code path executed; with env vars cleared
	// it discovered nothing and returned in milliseconds.
}

// =============================================================================
// Provider Registration Tests
// =============================================================================

func TestProviderRegistryUnit_RegisterProvider(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout:        30 * time.Second,
		MaxConcurrentRequests: 10,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:          true,
			FailureThreshold: 5,
			RecoveryTimeout:  60 * time.Second,
			SuccessThreshold: 2,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("register new provider", func(t *testing.T) {
		provider := newRegistryMockProvider("test-provider")
		err := registry.RegisterProvider("test-provider", provider)

		assert.NoError(t, err)

		registered, err := registry.GetProvider("test-provider")
		assert.NoError(t, err)
		assert.NotNil(t, registered)
	})

	t.Run("register duplicate provider fails", func(t *testing.T) {
		provider := newRegistryMockProvider("duplicate-provider")
		err := registry.RegisterProvider("duplicate-provider", provider)
		assert.NoError(t, err)

		err = registry.RegisterProvider("duplicate-provider", provider)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "already registered")
	})

	t.Run("register provider with circuit breaker", func(t *testing.T) {
		provider := newRegistryMockProvider("cb-provider")
		err := registry.RegisterProvider("cb-provider", provider)
		assert.NoError(t, err)

		// Circuit breaker should be created
		cb := registry.GetCircuitBreaker("cb-provider")
		assert.NotNil(t, cb)
	})

	t.Run("register provider without circuit breaker", func(t *testing.T) {
		cfg2 := &RegistryConfig{
			DefaultTimeout: 30 * time.Second,
			CircuitBreaker: CircuitBreakerConfig{
				Enabled: false,
			},
			Providers: make(map[string]*ProviderConfig),
			Ensemble: &models.EnsembleConfig{
				Strategy: "confidence_weighted",
			},
			Routing: &RoutingConfig{
				Strategy: "weighted",
			},
		}
		registry2 := NewProviderRegistryWithoutAutoDiscovery(cfg2, nil)

		provider := newRegistryMockProvider("no-cb-provider")
		err := registry2.RegisterProvider("no-cb-provider", provider)
		assert.NoError(t, err)

		// Circuit breaker should be nil
		cb := registry2.GetCircuitBreaker("no-cb-provider")
		assert.Nil(t, cb)
	})
}

func TestProviderRegistryUnit_UnregisterProvider(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("unregister existing provider", func(t *testing.T) {
		provider := newRegistryMockProvider("unregister-provider")
		_ = registry.RegisterProvider("unregister-provider", provider)

		err := registry.UnregisterProvider("unregister-provider")
		assert.NoError(t, err)

		_, err = registry.GetProvider("unregister-provider")
		assert.Error(t, err)
	})

	t.Run("unregister non-existent provider fails", func(t *testing.T) {
		err := registry.UnregisterProvider("non-existent")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestProviderRegistryUnit_RemoveProvider(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("remove with force", func(t *testing.T) {
		provider := newRegistryMockProvider("force-remove")
		_ = registry.RegisterProvider("force-remove", provider)

		err := registry.RemoveProvider("force-remove", true)
		assert.NoError(t, err)

		_, err = registry.GetProvider("force-remove")
		assert.Error(t, err)
	})

	t.Run("remove without force - no active requests", func(t *testing.T) {
		provider := newRegistryMockProvider("graceful-remove")
		_ = registry.RegisterProvider("graceful-remove", provider)

		err := registry.RemoveProvider("graceful-remove", false)
		assert.NoError(t, err)
	})

	t.Run("remove non-existent provider", func(t *testing.T) {
		err := registry.RemoveProvider("non-existent", true)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

// =============================================================================
// Provider Lookup Tests
// =============================================================================

func TestProviderRegistryUnit_GetProvider(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("get existing provider", func(t *testing.T) {
		provider := newRegistryMockProvider("get-provider")
		_ = registry.RegisterProvider("get-provider", provider)

		result, err := registry.GetProvider("get-provider")
		assert.NoError(t, err)
		assert.NotNil(t, result)
	})

	t.Run("get non-existent provider fails", func(t *testing.T) {
		result, err := registry.GetProvider("non-existent")
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestProviderRegistryUnit_ListProviders(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("empty registry", func(t *testing.T) {
		providers := registry.ListProviders()
		assert.Empty(t, providers)
	})

	t.Run("with registered providers", func(t *testing.T) {
		_ = registry.RegisterProvider("provider1", newRegistryMockProvider("provider1"))
		_ = registry.RegisterProvider("provider2", newRegistryMockProvider("provider2"))

		providers := registry.ListProviders()
		assert.Len(t, providers, 2)
		assert.Contains(t, providers, "provider1")
		assert.Contains(t, providers, "provider2")
	})
}

func TestProviderRegistryUnit_ListProvidersOrderedByScore(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("empty registry", func(t *testing.T) {
		providers := registry.ListProvidersOrderedByScore()
		assert.Empty(t, providers)
	})

	t.Run("providers without score adapter", func(t *testing.T) {
		_ = registry.RegisterProvider("provider1", newRegistryMockProvider("provider1"))
		_ = registry.RegisterProvider("provider2", newRegistryMockProvider("provider2"))

		providers := registry.ListProvidersOrderedByScore()
		assert.Len(t, providers, 2)
	})
}

// =============================================================================
// Provider Configuration Tests
// =============================================================================

func TestProviderRegistryUnit_ConfigureProvider(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("configure existing provider", func(t *testing.T) {
		provider := newRegistryMockProvider("config-provider")
		_ = registry.RegisterProvider("config-provider", provider)

		newConfig := &ProviderConfig{
			Name:    "config-provider",
			Enabled: true,
			APIKey:  "new-api-key",
			Weight:  1.5,
		}

		err := registry.ConfigureProvider("config-provider", newConfig)
		assert.NoError(t, err)

		storedConfig, err := registry.GetProviderConfig("config-provider")
		assert.NoError(t, err)
		assert.Equal(t, "new-api-key", storedConfig.APIKey)
		assert.Equal(t, 1.5, storedConfig.Weight)
	})

	t.Run("configure non-existent provider fails", func(t *testing.T) {
		newConfig := &ProviderConfig{
			Name:    "non-existent",
			Enabled: true,
		}

		err := registry.ConfigureProvider("non-existent", newConfig)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("disable provider", func(t *testing.T) {
		provider := newRegistryMockProvider("disable-provider")
		_ = registry.RegisterProvider("disable-provider", provider)

		newConfig := &ProviderConfig{
			Name:    "disable-provider",
			Enabled: false,
		}

		err := registry.ConfigureProvider("disable-provider", newConfig)
		assert.NoError(t, err)

		_, err = registry.GetProvider("disable-provider")
		assert.Error(t, err)
	})
}

func TestProviderRegistryUnit_GetProviderConfig(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("get existing provider config", func(t *testing.T) {
		provider := newRegistryMockProvider("get-config-provider")
		_ = registry.RegisterProvider("get-config-provider", provider)

		storedConfig, err := registry.GetProviderConfig("get-config-provider")
		assert.NoError(t, err)
		assert.NotNil(t, storedConfig)
		assert.True(t, storedConfig.Enabled)
	})

	t.Run("get non-existent provider config fails", func(t *testing.T) {
		storedConfig, err := registry.GetProviderConfig("non-existent")
		assert.Error(t, err)
		assert.Nil(t, storedConfig)
	})
}

func TestProviderRegistryUnit_UpdateProvider(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("update existing provider", func(t *testing.T) {
		provider := newRegistryMockProvider("update-provider")
		_ = registry.RegisterProvider("update-provider", provider)

		updateConfig := ProviderConfig{
			Name:    "update-provider",
			Enabled: true,
			APIKey:  "updated-api-key",
			Weight:  2.0,
		}

		err := registry.UpdateProvider("update-provider", updateConfig)
		assert.NoError(t, err)
	})

	t.Run("update non-existent provider fails", func(t *testing.T) {
		updateConfig := ProviderConfig{
			Name: "non-existent",
		}

		err := registry.UpdateProvider("non-existent", updateConfig)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

// =============================================================================
// Health Check Tests
// =============================================================================

func TestProviderRegistryUnit_HealthCheck(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("health check all providers", func(t *testing.T) {
		healthyProvider := newRegistryMockProvider("healthy")
		unhealthyProvider := newRegistryMockProvider("unhealthy")
		unhealthyProvider.healthCheckErr = errors.New("health check failed")

		_ = registry.RegisterProvider("healthy", healthyProvider)
		_ = registry.RegisterProvider("unhealthy", unhealthyProvider)

		results := registry.HealthCheck()

		assert.Len(t, results, 2)
		assert.Nil(t, results["healthy"])
		assert.NotNil(t, results["unhealthy"])
	})

	t.Run("empty registry", func(t *testing.T) {
		emptyRegistry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)
		results := emptyRegistry.HealthCheck()
		assert.Empty(t, results)
	})
}

func TestProviderRegistryUnit_VerifyProvider(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("verify healthy provider", func(t *testing.T) {
		provider := newRegistryMockProvider("verify-healthy")
		_ = registry.RegisterProvider("verify-healthy", provider)

		result := registry.VerifyProvider(context.Background(), "verify-healthy")

		assert.NotNil(t, result)
		assert.Equal(t, "verify-healthy", result.Provider)
		assert.True(t, result.Verified)
		assert.Equal(t, ProviderStatusHealthy, result.Status)
	})

	t.Run("verify non-existent provider", func(t *testing.T) {
		result := registry.VerifyProvider(context.Background(), "non-existent")

		assert.NotNil(t, result)
		assert.Equal(t, "non-existent", result.Provider)
		assert.False(t, result.Verified)
		assert.Equal(t, ProviderStatusUnhealthy, result.Status)
	})

	t.Run("verify rate limited provider", func(t *testing.T) {
		provider := newRegistryMockProvider("verify-rate-limited")
		provider.completeFunc = func(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error) {
			return nil, errors.New("429 rate limit exceeded")
		}
		_ = registry.RegisterProvider("verify-rate-limited", provider)

		result := registry.VerifyProvider(context.Background(), "verify-rate-limited")

		assert.NotNil(t, result)
		assert.Equal(t, ProviderStatusRateLimited, result.Status)
	})

	t.Run("verify auth failed provider", func(t *testing.T) {
		provider := newRegistryMockProvider("verify-auth-failed")
		provider.completeFunc = func(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error) {
			return nil, errors.New("401 unauthorized: invalid API key")
		}
		_ = registry.RegisterProvider("verify-auth-failed", provider)

		result := registry.VerifyProvider(context.Background(), "verify-auth-failed")

		assert.NotNil(t, result)
		assert.Equal(t, ProviderStatusAuthFailed, result.Status)
	})
}

func TestProviderRegistryUnit_VerifyAllProviders(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	_ = registry.RegisterProvider("provider1", newRegistryMockProvider("provider1"))
	_ = registry.RegisterProvider("provider2", newRegistryMockProvider("provider2"))

	results := registry.VerifyAllProviders(context.Background())

	assert.Len(t, results, 2)
	assert.NotNil(t, results["provider1"])
	assert.NotNil(t, results["provider2"])
}

// =============================================================================
// Provider Health Status Tests
// =============================================================================

func TestProviderRegistryUnit_GetProviderHealth(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("get health for unverified provider", func(t *testing.T) {
		health := registry.GetProviderHealth("unverified")
		assert.Nil(t, health)
	})

	t.Run("get health after verification", func(t *testing.T) {
		provider := newRegistryMockProvider("verified-provider")
		_ = registry.RegisterProvider("verified-provider", provider)

		_ = registry.VerifyProvider(context.Background(), "verified-provider")

		health := registry.GetProviderHealth("verified-provider")
		assert.NotNil(t, health)
		assert.Equal(t, "verified-provider", health.Provider)
	})
}

func TestProviderRegistryUnit_GetAllProviderHealth(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("empty registry", func(t *testing.T) {
		health := registry.GetAllProviderHealth()
		assert.Empty(t, health)
	})

	t.Run("with verified providers", func(t *testing.T) {
		provider := newRegistryMockProvider("health-provider")
		_ = registry.RegisterProvider("health-provider", provider)
		_ = registry.VerifyProvider(context.Background(), "health-provider")

		health := registry.GetAllProviderHealth()
		assert.Len(t, health, 1)
	})
}

func TestProviderRegistryUnit_IsProviderHealthy(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("unverified provider", func(t *testing.T) {
		healthy := registry.IsProviderHealthy("unverified")
		assert.False(t, healthy)
	})

	t.Run("verified healthy provider", func(t *testing.T) {
		provider := newRegistryMockProvider("healthy-check")
		_ = registry.RegisterProvider("healthy-check", provider)
		_ = registry.VerifyProvider(context.Background(), "healthy-check")

		healthy := registry.IsProviderHealthy("healthy-check")
		assert.True(t, healthy)
	})
}

func TestProviderRegistryUnit_GetHealthyProviders(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("no healthy providers", func(t *testing.T) {
		healthy := registry.GetHealthyProviders()
		assert.Empty(t, healthy)
	})

	t.Run("with healthy providers", func(t *testing.T) {
		provider := newRegistryMockProvider("healthy-provider")
		_ = registry.RegisterProvider("healthy-provider", provider)
		_ = registry.VerifyProvider(context.Background(), "healthy-provider")

		healthy := registry.GetHealthyProviders()
		assert.Len(t, healthy, 1)
		assert.Contains(t, healthy, "healthy-provider")
	})
}

// =============================================================================
// Service Getter Tests
// =============================================================================

func TestProviderRegistryUnit_GetEnsembleService(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		Providers:      make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	ensemble := registry.GetEnsembleService()
	assert.NotNil(t, ensemble)
}

func TestProviderRegistryUnit_GetRequestService(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		Providers:      make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
		Routing: &RoutingConfig{
			Strategy: "weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	requestService := registry.GetRequestService()
	assert.NotNil(t, requestService)
}

// =============================================================================
// Circuit Breaker Tests
// =============================================================================

func TestProviderRegistryUnit_GetCircuitBreaker(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:          true,
			FailureThreshold: 5,
			RecoveryTimeout:  60 * time.Second,
			SuccessThreshold: 2,
		},
		Providers: make(map[string]*ProviderConfig),
		Ensemble: &models.EnsembleConfig{
			Strategy: "confidence_weighted",
		},
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("existing circuit breaker", func(t *testing.T) {
		provider := newRegistryMockProvider("cb-test")
		_ = registry.RegisterProvider("cb-test", provider)

		cb := registry.GetCircuitBreaker("cb-test")
		assert.NotNil(t, cb)
	})

	t.Run("non-existent circuit breaker", func(t *testing.T) {
		cb := registry.GetCircuitBreaker("non-existent")
		assert.Nil(t, cb)
	})
}

// =============================================================================
// ContainsAny Helper Tests
// =============================================================================

func TestProviderRegistryUnit_ContainsAny(t *testing.T) {
	tests := []struct {
		s        string
		substrs  []string
		expected bool
	}{
		{"error 429 occurred", []string{"429", "500"}, true},
		{"authentication failed", []string{"401", "403"}, false},
		{"API_KEY invalid", []string{"api_key"}, true},
		{"", []string{"test"}, false},
	}

	for _, tt := range tests {
		result := containsAny(tt.s, tt.substrs...)
		assert.Equal(t, tt.expected, result)
	}
}

// =============================================================================
// Registry Config Tests
// =============================================================================

func TestProviderRegistryUnit_GetDefaultRegistryConfig(t *testing.T) {
	cfg := getDefaultRegistryConfig()

	assert.NotNil(t, cfg)
	assert.Equal(t, 30*time.Second, cfg.DefaultTimeout)
	assert.Equal(t, 3, cfg.MaxRetries)
	assert.Equal(t, 10, cfg.MaxConcurrentRequests)
	assert.True(t, cfg.HealthCheck.Enabled)
	assert.True(t, cfg.CircuitBreaker.Enabled)
	assert.NotNil(t, cfg.Providers)
	assert.NotNil(t, cfg.Ensemble)
	assert.NotNil(t, cfg.Routing)
}

// =============================================================================
// Score Adapter Tests
// =============================================================================

func TestProviderRegistryUnit_GetScoreAdapter(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		Providers:      make(map[string]*ProviderConfig),
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	// Initially nil
	adapter := registry.GetScoreAdapter()
	assert.Nil(t, adapter)
}

func TestProviderRegistryUnit_UpdateProviderScore(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		Providers:      make(map[string]*ProviderConfig),
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	// Should not panic with nil adapter
	registry.UpdateProviderScore("test-provider", "model-1", 8.5)
}

// =============================================================================
// Auto Discovery Tests
// =============================================================================

func TestProviderRegistryUnit_SetAutoDiscovery(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		Providers:      make(map[string]*ProviderConfig),
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	registry.SetAutoDiscovery(true)
	assert.True(t, registry.autoDiscovery)

	registry.SetAutoDiscovery(false)
	assert.False(t, registry.autoDiscovery)
}

func TestProviderRegistryUnit_GetDiscovery(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		Providers:      make(map[string]*ProviderConfig),
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	discovery := registry.GetDiscovery()
	assert.Nil(t, discovery) // Not initialized without auto-discovery
}

// =============================================================================
// Verified Providers Summary Tests
// =============================================================================

func TestProviderRegistryUnit_GetVerifiedProvidersSummary(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		Providers:      make(map[string]*ProviderConfig),
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	summary := registry.GetVerifiedProvidersSummary()

	assert.NotNil(t, summary)
	assert.Contains(t, summary, "source")
	assert.Contains(t, summary, "total_providers")
}

// =============================================================================
// Best Providers for Debate Tests
// =============================================================================

func TestProviderRegistryUnit_GetBestProvidersForDebate(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		Providers:      make(map[string]*ProviderConfig),
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("without discovery", func(t *testing.T) {
		// Register and verify a provider
		provider := newRegistryMockProvider("debate-provider")
		_ = registry.RegisterProvider("debate-provider", provider)
		_ = registry.VerifyProvider(context.Background(), "debate-provider")

		providers := registry.GetBestProvidersForDebate(2, 4)
		// Falls back to healthy providers
		assert.NotNil(t, providers)
	})
}

// =============================================================================
// Circuit Breaker Provider Tests
// =============================================================================

func TestProviderRegistryUnit_CircuitBreakerProvider_Complete(t *testing.T) {
	mockProvider := newRegistryMockProvider("cb-wrap")
	cb := NewCircuitBreaker(5, 2, 60*time.Second)
	// Create weighted semaphore
	weightedSem := semaphore.NewWeighted(10)

	var counter int64

	cbProvider := &circuitBreakerProvider{
		provider:              mockProvider,
		circuitBreaker:        cb,
		concurrencySemaphore:  weightedSem,
		name:                  "cb-wrap",
		activeRequestsCounter: &counter,
		totalPermits:          10,
	}

	ctx := context.Background()
	req := &models.LLMRequest{
		Prompt: "test",
	}

	resp, err := cbProvider.Complete(ctx, req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestCircuitBreakerProvider_Complete_NoCircuitBreaker(t *testing.T) {
	mockProvider := newRegistryMockProvider("no-cb")
	weightedSem := semaphore.NewWeighted(10)
	var counter int64

	cbProvider := &circuitBreakerProvider{
		provider:              mockProvider,
		circuitBreaker:        nil,
		concurrencySemaphore:  weightedSem,
		name:                  "no-cb",
		activeRequestsCounter: &counter,
		totalPermits:          10,
	}

	ctx := context.Background()
	req := &models.LLMRequest{
		Prompt: "test",
	}

	resp, err := cbProvider.Complete(ctx, req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestProviderRegistryUnit_CircuitBreakerProvider_CompleteStream(t *testing.T) {
	mockProvider := newRegistryMockProvider("cb-stream")
	weightedSem := semaphore.NewWeighted(10)
	var counter int64
	cb := NewCircuitBreaker(5, 2, 60*time.Second)

	cbProvider := &circuitBreakerProvider{
		provider:              mockProvider,
		circuitBreaker:        cb,
		concurrencySemaphore:  weightedSem,
		name:                  "cb-stream",
		activeRequestsCounter: &counter,
		totalPermits:          10,
	}

	ctx := context.Background()
	req := &models.LLMRequest{
		Prompt: "test",
	}

	stream, err := cbProvider.CompleteStream(ctx, req)

	require.NoError(t, err)
	assert.NotNil(t, stream)
}

func TestProviderRegistryUnit_CircuitBreakerProvider_HealthCheck(t *testing.T) {
	mockProvider := newRegistryMockProvider("cb-health")
	weightedSem := semaphore.NewWeighted(10)
	var counter int64
	cb := NewCircuitBreaker(5, 2, 60*time.Second)

	cbProvider := &circuitBreakerProvider{
		provider:              mockProvider,
		circuitBreaker:        cb,
		concurrencySemaphore:  weightedSem,
		name:                  "cb-health",
		activeRequestsCounter: &counter,
		totalPermits:          10,
	}

	err := cbProvider.HealthCheck()
	assert.NoError(t, err)
}

func TestProviderRegistryUnit_CircuitBreakerProvider_GetCapabilities(t *testing.T) {
	mockProvider := newRegistryMockProvider("cb-caps")
	weightedSem := semaphore.NewWeighted(10)
	var counter int64

	cbProvider := &circuitBreakerProvider{
		provider:              mockProvider,
		concurrencySemaphore:  weightedSem,
		name:                  "cb-caps",
		activeRequestsCounter: &counter,
		totalPermits:          10,
	}

	caps := cbProvider.GetCapabilities()
	assert.NotNil(t, caps)
}

func TestProviderRegistryUnit_CircuitBreakerProvider_ValidateConfig(t *testing.T) {
	mockProvider := newRegistryMockProvider("cb-validate")
	weightedSem := semaphore.NewWeighted(10)
	var counter int64

	cbProvider := &circuitBreakerProvider{
		provider:              mockProvider,
		concurrencySemaphore:  weightedSem,
		name:                  "cb-validate",
		activeRequestsCounter: &counter,
		totalPermits:          10,
	}

	valid, errs := cbProvider.ValidateConfig(map[string]interface{}{})
	assert.True(t, valid)
	assert.Empty(t, errs)
}

// =============================================================================
// Drain Provider Requests Tests
// =============================================================================

func TestProviderRegistryUnit_DrainProviderRequests(t *testing.T) {
	cfg := &RegistryConfig{
		DefaultTimeout: 30 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: false,
		},
		Providers: make(map[string]*ProviderConfig),
	}
	registry := NewProviderRegistryWithoutAutoDiscovery(cfg, nil)

	t.Run("provider with no counter", func(t *testing.T) {
		err := registry.drainProviderRequests("non-existent")
		assert.NoError(t, err)
	})
}
