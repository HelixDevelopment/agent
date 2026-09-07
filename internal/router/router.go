package router

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"time"

	authadapter "dev.helix.agent/internal/adapters/auth"
	containeradapter "dev.helix.agent/internal/adapters/containers"
	helixllmadapter "dev.helix.agent/internal/adapters/helixllm"
	helixqaadapter "dev.helix.agent/internal/adapters/helixqa"
	"dev.helix.agent/internal/background"
	"dev.helix.agent/internal/benchmark"
	"dev.helix.agent/internal/browser"
	"dev.helix.agent/internal/cache"
	"dev.helix.agent/internal/catalog"
	"dev.helix.agent/internal/checkpoints"
	"dev.helix.agent/internal/clis"
	"dev.helix.agent/internal/config"
	"dev.helix.agent/internal/database"
	"dev.helix.agent/internal/ensemble/multi_instance"
	"dev.helix.agent/internal/features"
	"dev.helix.agent/internal/formatters"
	formattersproviders "dev.helix.agent/internal/formatters/providers"
	helixgraphql "dev.helix.agent/internal/graphql"
	"dev.helix.agent/internal/handlers"
	"dev.helix.agent/internal/llm"
	"dev.helix.agent/internal/llmops"
	"dev.helix.agent/internal/middleware"
	"dev.helix.agent/internal/models"
	"dev.helix.agent/internal/modelsdev"
	httpmetrics "dev.helix.agent/internal/observability/metrics"
	"dev.helix.agent/internal/search"
	"dev.helix.agent/internal/search/indexer"
	"dev.helix.agent/internal/services"
	"dev.helix.agent/internal/services/debate_integration"
	"dev.helix.agent/internal/skills"
	"dev.helix.agent/internal/templates"
	"dev.helix.agent/internal/verifier"
	"dev.helix.agent/internal/verifier/adapters"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"net/http/pprof"
	"os"
)

// RouterContext wraps the router with cleanup capabilities for background services
type RouterContext struct {
	Engine                  *gin.Engine
	protocolManager         *services.UnifiedProtocolManager
	oauthMonitor            *services.OAuthTokenMonitor
	oauthCredentialManager  *authadapter.OAuthCredentialManager // OAuth credential manager from auth adapter
	containerAdapter        *containeradapter.Adapter           // Container adapter for orchestration
	healthMonitor           *services.ProviderHealthMonitor
	concurrencyMonitor      *services.ConcurrencyMonitor
	concurrencyAlertManager *services.ConcurrencyAlertManager
	constitutionWatcher     *services.ConstitutionWatcher          // Constitution auto-update background service
	ProviderRegistry        *services.ProviderRegistry             // Exposed for StartupVerifier integration
	DebateTeamConfig        *services.DebateTeamConfig             // Exposed for re-initialization with StartupVerifier
	unifiedHandler          *handlers.UnifiedHandler               // For updating debate team display
	debateService           *services.DebateService                // For updating team config
	orchestratorIntegration *debate_integration.ServiceIntegration // Orchestrator integration for re-population
	CogneeService           *services.CogneeService                // Exposed for container adapter injection
	logger                  *logrus.Logger                         // For IntentBasedRouter creation
	intentBasedRouter       *services.IntentBasedRouter            // For re-initialization with StartupVerifier
	taskWorker              *background.InMemoryWorker             // Drains /v1/tasks queue (closes #task-worker-pool-wiring)
	ensembleSQLDB           *sql.DB                                // Optional Postgres pool for ensemble durability (closes #ensemble-db-wiring when reachable)
}

// Shutdown stops all background services started by the router
func (rc *RouterContext) Shutdown() {
	if rc.protocolManager != nil {
		rc.protocolManager.Stop()
	}
	if rc.oauthMonitor != nil {
		rc.oauthMonitor.Stop()
	}
	if rc.healthMonitor != nil {
		rc.healthMonitor.Stop()
	}
	if rc.concurrencyAlertManager != nil {
		rc.concurrencyAlertManager.Stop()
	}
	if rc.constitutionWatcher != nil {
		rc.constitutionWatcher.Disable()
	}
	if rc.debateService != nil {
		rc.debateService.StopConstitutionWatcher()
	}
	if rc.taskWorker != nil {
		rc.taskWorker.Stop()
	}
	if rc.ensembleSQLDB != nil {
		_ = rc.ensembleSQLDB.Close() //nolint:errcheck
	}
}

// ReinitializeDebateTeam re-initializes the DebateTeamConfig with the StartupVerifier.
// Call this after setting the StartupVerifier on ProviderRegistry to include OAuth providers.
func (rc *RouterContext) ReinitializeDebateTeam(ctx context.Context) error {
	if rc.DebateTeamConfig == nil || rc.ProviderRegistry == nil {
		return nil
	}

	// Get the StartupVerifier from the ProviderRegistry
	sv := rc.ProviderRegistry.GetStartupVerifier()
	if sv == nil {
		return nil // No StartupVerifier, keep existing team
	}

	// Set the StartupVerifier on DebateTeamConfig
	rc.DebateTeamConfig.SetStartupVerifier(sv)

	// Re-initialize the team with OAuth providers
	if err := rc.DebateTeamConfig.InitializeTeam(ctx); err != nil {
		return err
	}

	// Update handlers with new team config
	if rc.unifiedHandler != nil {
		rc.unifiedHandler.SetDebateTeamConfig(rc.DebateTeamConfig)

		// Also initialize IntentBasedRouter with StartupVerifier and LLM classifier
		if rc.intentBasedRouter == nil && rc.logger != nil {
			rc.intentBasedRouter = services.NewIntentBasedRouter(sv, rc.logger)
			if rc.ProviderRegistry != nil {
				llmClassifier := services.NewLLMIntentClassifier(rc.ProviderRegistry, rc.logger)
				rc.intentBasedRouter.SetLLMClassifier(llmClassifier)
			}
			rc.unifiedHandler.SetIntentBasedRouter(rc.intentBasedRouter)
			rc.logger.Info("IntentBasedRouter configured with LLM classifier for semantic intent routing")
		}
	}
	if rc.debateService != nil {
		rc.debateService.SetTeamConfig(rc.DebateTeamConfig)
	}

	// Re-populate the orchestrator agent pool with the updated debate team's verified providers
	if rc.orchestratorIntegration != nil {
		rc.orchestratorIntegration.PopulateFromDebateTeam(rc.DebateTeamConfig)
	}

	return nil
}

// SetupRouter creates and configures the main HTTP router.
// Note: Use SetupRouterWithContext for tests to ensure proper cleanup.
func SetupRouter(cfg *config.Config) *gin.Engine {
	ctx := SetupRouterWithContext(cfg)
	return ctx.Engine
}

// SetupRouterWithContext creates and configures the main HTTP router with cleanup support.
// Call Shutdown() on the returned RouterContext when done to stop background services.
func SetupRouterWithContext(cfg *config.Config) *RouterContext {
	rc := &RouterContext{}
	r := gin.New()

	// Middleware
	r.Use(gin.Logger())
	r.Use(gin.Recovery())

	// Feature flags middleware - detects agent capabilities and applies feature settings
	// This middleware enables/disables features like GraphQL, TOON, Brotli, HTTP/3
	// based on User-Agent detection and request headers/query params
	featureConfig := features.DefaultFeatureConfig()
	// Keep GraphQL OFF by default for OpenAI-compatible endpoints (backward compatibility)
	// Users can enable via X-Feature-GraphQL header or ?graphql=true query param
	featureConfig.OpenAIEndpointGraphQL = false
	featureMiddleware := features.Middleware(&features.MiddlewareConfig{
		Config:               featureConfig,
		Logger:               logrus.New(),
		EnableAgentDetection: true,
		StrictMode:           false, // Lenient mode for backward compatibility
		TrackUsage:           true,
	})
	r.Use(featureMiddleware)

	// Compression middleware that respects feature flags
	r.Use(func(c *gin.Context) {
		fc := features.GetFeatureContextFromGin(c)
		if fc.IsEnabled(features.FeatureBrotli) || fc.IsEnabled(features.FeatureGzip) {
			config := middleware.DefaultCompressionConfig()
			config.EnableBrotli = fc.IsEnabled(features.FeatureBrotli)
			config.EnableGzip = fc.IsEnabled(features.FeatureGzip)
			middleware.CompressionMiddleware(config)(c)
		} else {
			c.Next()
		}
	})

	// Concurrency limiter to prevent thundering herd (default: 100 in-flight)
	r.Use(middleware.ConcurrencyLimiter(100))

	// Global request body size limit (default 10 MiB; override via
	// MAX_REQUEST_BODY_BYTES env var). Rejects over-sized payloads
	// before they can exhaust the process allocator — a broad safety
	// net against memory-exhaustion DoS. Endpoints that need a larger
	// cap (bulk document ingest, RAG batch upload) can install their
	// own BodyLimit middleware with a higher value on their specific
	// route group.
	r.Use(middleware.BodyLimit(middleware.DefaultMaxRequestBodySize))

	// Per-handler Prometheus metrics (duration histogram, request counter, error counter)
	httpMetrics := httpmetrics.NewHTTPMetrics()
	r.Use(httpMetrics.Middleware())

	// Phase-3 memory-safety gauges. Registered once, idempotently; the
	// underlying singleton picks up live WorkerPool and GuardrailPipeline
	// accessors as those services initialize, and reports zero until
	// they do. Any error here is logged but not fatal — metrics are
	// observability, not a boot requirement.
	if _, err := httpmetrics.RegisterDefaultPhase3Metrics(); err != nil {
		log.Printf("Warning: failed to register Phase-3 metrics: %v", err)
	}

	// Add pprof debugging endpoints if enabled
	if os.Getenv("ENABLE_PPROF") == "true" {
		// Register pprof handlers
		r.GET("/debug/pprof/", gin.WrapH(http.HandlerFunc(pprof.Index)))
		r.GET("/debug/pprof/cmdline", gin.WrapH(http.HandlerFunc(pprof.Cmdline)))
		r.GET("/debug/pprof/profile", gin.WrapH(http.HandlerFunc(pprof.Profile)))
		r.GET("/debug/pprof/symbol", gin.WrapH(http.HandlerFunc(pprof.Symbol)))
		r.GET("/debug/pprof/trace", gin.WrapH(http.HandlerFunc(pprof.Trace)))
		// Additional pprof endpoints for specific profiles
		r.GET("/debug/pprof/goroutine", gin.WrapH(pprof.Handler("goroutine")))
		r.GET("/debug/pprof/heap", gin.WrapH(pprof.Handler("heap")))
		r.GET("/debug/pprof/threadcreate", gin.WrapH(pprof.Handler("threadcreate")))
		r.GET("/debug/pprof/block", gin.WrapH(pprof.Handler("block")))
		r.GET("/debug/pprof/mutex", gin.WrapH(pprof.Handler("mutex")))
	}

	// Initialize database with fallback to in-memory mode
	var db *database.PostgresDB
	standaloneMode := false

	pgDB, memoryDB, err := database.NewPostgresDBWithFallback(cfg)
	if err != nil {
		log.Printf("Database initialization failed: %v, using standalone mode", err)
		standaloneMode = true
	} else if memoryDB != nil {
		standaloneMode = true
		log.Println("Running in standalone mode (in-memory database)")
	} else {
		db = pgDB
	}

	// Initialize user service (only if we have a real database)
	var userService *services.UserService
	if db != nil {
		userService = services.NewUserService(db, cfg.Server.JWTSecret, 24*time.Hour)
	}
	// In standalone mode, userService will be nil - handled below

	// Initialize memory service
	memoryService := services.NewMemoryService(cfg)

	// Initialize services
	registryConfig := services.LoadRegistryConfigFromAppConfig(cfg)
	providerRegistry := services.NewProviderRegistry(registryConfig, memoryService)
	rc.ProviderRegistry = providerRegistry // Expose for StartupVerifier integration

	// Initialize shared logger
	logger := logrus.New()
	rc.logger = logger // Store for IntentBasedRouter creation

	// AUTOMATIC STARTUP SCORING: Score all 30+ providers and 900+ LLMs at startup
	// This ensures we always have up-to-date provider scores for optimal routing
	startupScoringConfig := services.DefaultStartupScoringConfig()
	startupScoringService := services.NewStartupScoringService(providerRegistry, startupScoringConfig, logger)
	startupScoringService.Run(context.Background())
	logger.Info("Automatic provider scoring initiated (running in background)")

	// Initialize model metadata repository (used by multiple services)
	// In standalone mode, pass nil pool - repository will use in-memory storage
	var modelMetadataRepo *database.ModelMetadataRepository
	if db != nil {
		modelMetadataRepo = database.NewModelMetadataRepository(db.GetPool(), logger)
	} else {
		modelMetadataRepo = database.NewModelMetadataRepository(nil, logger)
		logger.Info("Model metadata repository running in standalone mode")
	}

	// Initialize Redis client for caching if Redis is configured
	var redisClient *cache.RedisClient
	if cfg.Redis.Host != "" && cfg.Redis.Port != "" {
		redisClient = cache.NewRedisClient(cfg)
	}

	// Create cache factory and shared cache
	cacheFactory := services.NewCacheFactory(redisClient, logger)
	sharedCache := cacheFactory.CreateDefaultCache(30 * time.Minute)

	// Initialize Models.dev integration (if enabled)
	var modelMetadataHandler *handlers.ModelMetadataHandler
	if cfg.ModelsDev.Enabled {
		modelsDevClient := modelsdev.NewClient(&modelsdev.ClientConfig{
			APIKey:    cfg.ModelsDev.APIKey,
			BaseURL:   cfg.ModelsDev.BaseURL,
			Timeout:   30 * time.Second,
			UserAgent: "helix_agent/1.0",
		})

		modelMetadataCache := cacheFactory.CreateDefaultCache(cfg.ModelsDev.CacheTTL)

		modelMetadataService := services.NewModelMetadataService(
			modelsDevClient,
			modelMetadataRepo,
			modelMetadataCache,
			&services.ModelMetadataConfig{
				RefreshInterval:   cfg.ModelsDev.RefreshInterval,
				CacheTTL:          cfg.ModelsDev.CacheTTL,
				DefaultBatchSize:  cfg.ModelsDev.DefaultBatchSize,
				MaxRetries:        cfg.ModelsDev.MaxRetries,
				EnableAutoRefresh: cfg.ModelsDev.AutoRefresh,
			},
			logger,
		)

		modelMetadataHandler = handlers.NewModelMetadataHandler(modelMetadataService)

		logger.Info("Models.dev integration initialized successfully")
	}

	// Initialize unified OpenAI-compatible handler
	unifiedHandler := handlers.NewUnifiedHandler(providerRegistry, cfg)

	// Initialize Skills system
	skillConfig := skills.DefaultSkillConfig()
	skillConfig.SkillsDirectory = "skills" // Load from project skills/ directory
	skillService := skills.NewService(skillConfig)
	skillService.SetLogger(logger)
	if initErr := skillService.Initialize(context.Background()); initErr != nil {
		logger.WithError(initErr).Warn("Failed to initialize skills system, continuing without skills")
	} else {
		logger.WithField("skills_loaded", len(skillService.GetAllSkills())).Info("Skills system initialized")
	}
	skillsIntegration := skills.NewIntegration(skillService)
	skillsIntegration.SetLogger(logger)

	// Inject skills integration into unified handler
	unifiedHandler.SetSkillsIntegration(skillsIntegration)

	// Initialize Cognee service with all features enabled
	cogneeService := services.NewCogneeService(cfg, logger)
	rc.CogneeService = cogneeService

	// Enhance all LLM providers with Cognee capabilities
	// This wraps every provider with memory, graph reasoning, and context enhancement
	if cfg.Cognee.Enabled {
		if enhanceErr := services.EnhanceProviderRegistry(providerRegistry, cogneeService, logger); enhanceErr != nil {
			logger.WithError(enhanceErr).Warn("Failed to enhance providers with Cognee, continuing without enhancement")
		} else {
			logger.Info("All LLM providers enhanced with Cognee capabilities")
		}
	}

	// Initialize Cognee API handler with comprehensive features
	cogneeAPIHandler := handlers.NewCogneeAPIHandler(cogneeService, logger)

	// Initialize Embedding handler
	embeddingManager := services.NewEmbeddingManagerWithConfig(nil, sharedCache, logger, services.EmbeddingConfig{
		VectorProvider: "pgvector",
		Timeout:        30 * time.Second,
		CacheEnabled:   true,
	})
	embeddingHandler := handlers.NewEmbeddingHandler(embeddingManager, logger)

	// Initialize LSP handler
	lspManager := services.NewLSPManager(modelMetadataRepo, sharedCache, logger)
	lspHandler := handlers.NewLSPHandler(lspManager, logger)

	// Initialize MCP handler
	mcpHandler := handlers.NewMCPHandler(providerRegistry, &cfg.MCP)

	// Initialize ACP handler
	acpHandler := handlers.NewACPHandler(providerRegistry, logger)

	// Initialize Protocol handler (UnifiedProtocolManager implements ProtocolManagerInterface)
	protocolManager := services.NewUnifiedProtocolManager(modelMetadataRepo, sharedCache, logger)
	rc.protocolManager = protocolManager
	protocolHandler := handlers.NewProtocolHandler(protocolManager, logger)

	// Initialize Protocol SSE handler for MCP/ACP/LSP/Embeddings/Vision/Cognee
	protocolSSEHandler := handlers.NewProtocolSSEHandler(
		mcpHandler,
		acpHandler,
		lspHandler,
		embeddingHandler,
		cogneeAPIHandler,
		logger,
	)

	// Initialize auth middleware
	// In standalone mode, make auth optional with more skip paths
	var auth *middleware.AuthMiddleware
	if standaloneMode {
		log.Println("Running in standalone mode - authentication disabled for API endpoints")
		authConfig := middleware.AuthConfig{
			SecretKey:   cfg.Server.JWTSecret,
			TokenExpiry: 24 * time.Hour,
			Issuer:      "helixagent",
			SkipPaths:   []string{"/health", "/v1/health", "/metrics", "/v1/auth/login", "/v1/auth/register", "/v1/chat/completions", "/v1/completions", "/v1/messages", "/v1beta/models", "/v1/models", "/v1/ensemble", "/v1/acp", "/v1/vision", "/v1/mcp", "/v1/lsp", "/v1/embeddings", "/v1/cognee"},
			Required:    false,
		}
		auth, err = middleware.NewAuthMiddleware(authConfig, nil)
		if err != nil {
			log.Printf("Auth middleware not available in standalone mode: %v", err)
		}
	} else {
		authConfig := middleware.AuthConfig{
			SecretKey:   cfg.Server.JWTSecret,
			TokenExpiry: 24 * time.Hour,
			Issuer:      "helixagent",
			SkipPaths:   []string{"/health", "/v1/health", "/metrics", "/v1/auth/login", "/v1/auth/register", "/v1/acp", "/v1/vision", "/v1/mcp", "/v1/lsp", "/v1/embeddings", "/v1/cognee"},
			Required:    true,
		}
		auth, err = middleware.NewAuthMiddleware(authConfig, userService)
		if err != nil {
			log.Fatalf("Failed to initialize auth middleware: %v", err)
		}
	}

	// Initialize OAuth credential manager for providers using OAuth (Claude, Qwen).
	// Gated on the cloud opt-in — see newOAuthCredentialManager in
	// oauth_credentials.go for why file presence alone is not operator intent.
	oauthManager := newOAuthCredentialManager(context.Background(), standaloneMode, logger)

	// Initialize container adapter for container orchestration
	var containerAdapt *containeradapter.Adapter
	if !standaloneMode {
		containerAdapt, err = containeradapter.NewAdapterFromConfig(cfg)
		if err != nil {
			logger.WithError(err).Warn("Failed to initialize container adapter, continuing without container orchestration")
		} else {
			logger.Info("Container adapter initialized for container orchestration")
			rc.containerAdapter = containerAdapt
		}
	}

	// Initialize messaging adapter for Kafka/RabbitMQ
	if !standaloneMode && (cfg.Services.Kafka.Enabled || cfg.Services.RabbitMQ.Enabled) {
		// Messaging services are configured but initialization requires
		// specific broker implementations from the messaging module.
		// For now, log that messaging is enabled but adapter initialization
		// requires additional configuration.
		if cfg.Services.Kafka.Enabled {
			logger.WithField("url", cfg.Services.Kafka.ResolvedURL()).
				Info("Kafka messaging enabled - broker initialization requires messaging module configuration")
		}
		if cfg.Services.RabbitMQ.Enabled {
			logger.WithField("url", cfg.Services.RabbitMQ.ResolvedURL()).
				Info("RabbitMQ messaging enabled - broker initialization requires messaging module configuration")
		}
		logger.Info("To enable messaging adapter, configure broker.NewKafkaBroker or broker.NewRabbitMQBroker with the service URLs above")
	}

	// Health endpoints
	//
	// Both carry an explicit "service":"helixagent" identity field (§11.4.111
	// resolve-by-stable-identity, not by port/ordinal), and /v1/health
	// additionally emits a "providers" key — which is the field that actually
	// gates availability. Keep emitting BOTH.
	//
	// HISTORY — do not re-derive (§11.4.6): commit 3d96b4cf added the "service"
	// field and claimed that doing so "closes that false-GREEN vector at the
	// source". Commit 0367570e established verbatim that "that claim was
	// INCORRECT", and it must not be relied upon: checkHelixAgentHealth still
	// fell back to accepting any bare {"status":"healthy"} body when no
	// "service" field was present, and this repo's own mock LLM server
	// (challenges/codebase/mock_server/main.go) answers /health with EXACTLY
	// that shape and NO service field while also serving
	// /v1/chat/completions with fabricated replies — so a
	// HELIXAGENT_HOST/PORT pointed at the mock still satisfied the
	// availability gate, and tests would run, and PASS, against the mock
	// instead of the real server.
	//
	// What actually closes the vector lives in the CONSUMER, not here:
	// testutil.checkHelixAgentHealth (internal/testutil/infra.go) probes
	// /v1/health and REQUIRES the "providers" key, rejecting any payload
	// lacking it regardless of "status" or "service". The mock registers no
	// /v1/health handler at all, so it 404s and is rejected. The "service"
	// field is only a refinement applied AFTER that mandatory gate.
	//
	// CONSEQUENCE: do NOT weaken checkHelixAgentHealth's "providers"
	// requirement on the premise that emitting "service" here is sufficient —
	// that premise is the refuted claim above, and acting on it reopens the
	// mock-matching false-GREEN vector.
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "helixagent"})
	})

	r.GET("/v1/health", func(c *gin.Context) {
		// Enhanced health check with provider status.
		//
		// The verdict lives in healthVerdict() (health_verdict.go) so it is
		// unit-testable without booting the whole router — see the note there
		// about why the pre-existing /health tests could never have caught a
		// defect in this handler.
		//
		// The HTTP code stays 200 deliberately: testutil.checkHelixAgentHealth
		// (internal/testutil/infra.go:352) REQUIRES StatusOK and rejects the
		// agent outright otherwise, so emitting 5xx here would make every
		// integration run treat a provider-less dev agent as absent. The
		// truthful signal is carried in the body's "status" field, which that
		// consumer does not read while "service" is present (infra.go:377-379).
		health := providerRegistry.HealthCheck()
		status, healthyCount, unhealthyCount := healthVerdict(health)

		c.JSON(200, gin.H{
			"status":  status,
			"service": "helixagent",
			"providers": map[string]any{
				"total":     len(health),
				"healthy":   healthyCount,
				"unhealthy": unhealthyCount,
			},
			"timestamp": time.Now().Unix(),
		})
	})

	// Metrics endpoint - Prometheus metrics for monitoring
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Feature flags status endpoint - shows enabled features and usage stats
	r.GET("/v1/features", func(c *gin.Context) {
		fc := features.GetFeatureContextFromGin(c)
		tracker := features.GetUsageTracker()
		stats := tracker.GetStats()

		// Build feature stats map
		featureStats := make(map[string]gin.H)
		for _, stat := range stats {
			featureStats[string(stat.Feature)] = gin.H{
				"enabled_count":  stat.EnabledCount,
				"disabled_count": stat.DisabledCount,
				"total_requests": stat.TotalRequests,
			}
		}

		c.JSON(200, gin.H{
			"enabled_features":  fc.GetEnabledFeatures(),
			"disabled_features": fc.GetDisabledFeatures(),
			"agent_detected":    fc.AgentName,
			"transport":         fc.GetTransportProtocol(),
			"compression":       fc.GetCompressionMethod(),
			"streaming":         fc.GetStreamingMethod(),
			"source":            string(fc.Source),
			"usage_stats":       featureStats,
		})
	})

	// Feature flags configuration endpoint - shows all available features and defaults
	r.GET("/v1/features/available", func(c *gin.Context) {
		registry := features.GetRegistry()
		allFeatures := registry.ListFeatures()

		featureList := make([]gin.H, 0, len(allFeatures))
		for _, f := range allFeatures {
			info := registry.GetFeatureInfo(f)
			if info != nil {
				featureList = append(featureList, gin.H{
					"name":         string(f),
					"description":  info.Description,
					"category":     string(info.Category),
					"default":      info.DefaultValue,
					"header":       info.HeaderName,
					"query_param":  info.QueryParam,
					"dependencies": info.RequiresFeatures,
					"conflicts":    info.ConflictsWith,
				})
			}
		}

		c.JSON(200, gin.H{
			"features": featureList,
			"count":    len(featureList),
		})
	})

	// Agent capabilities endpoint - shows what features each CLI agent supports
	r.GET("/v1/features/agents", func(c *gin.Context) {
		agentCaps := features.ListAgentCapabilities()

		agents := make([]gin.H, 0, len(agentCaps))
		for _, cap := range agentCaps {
			supportedFeatures := make([]string, 0, len(cap.SupportedFeatures))
			for _, f := range cap.SupportedFeatures {
				supportedFeatures = append(supportedFeatures, string(f))
			}
			agents = append(agents, gin.H{
				"name":               cap.AgentName,
				"supported_features": supportedFeatures,
				"transport":          cap.TransportProtocol,
				"description":        cap.Notes,
			})
		}

		c.JSON(200, gin.H{
			"agents": agents,
			"count":  len(agents),
		})
	})

	// Authentication endpoints (skip in standalone mode if auth is nil)
	if auth != nil {
		authGroup := r.Group("/v1/auth")
		{
			authGroup.POST("/register", auth.Register)
			authGroup.POST("/login", auth.Login)
			authGroup.POST("/refresh", auth.Refresh)
			authGroup.POST("/logout", auth.Logout)
			authGroup.GET("/me", func(c *gin.Context) {
				c.JSON(200, auth.GetAuthInfo(c))
			})
		}
	} else {
		// Provide stub auth endpoints in standalone mode
		authGroup := r.Group("/v1/auth")
		{
			authGroup.POST("/register", func(c *gin.Context) {
				c.JSON(503, gin.H{"error": "Authentication disabled in standalone mode"})
			})
			authGroup.POST("/login", func(c *gin.Context) {
				c.JSON(503, gin.H{"error": "Authentication disabled in standalone mode"})
			})
			authGroup.POST("/refresh", func(c *gin.Context) {
				c.JSON(503, gin.H{"error": "Authentication disabled in standalone mode"})
			})
			authGroup.POST("/logout", func(c *gin.Context) {
				c.JSON(503, gin.H{"error": "Authentication disabled in standalone mode"})
			})
			authGroup.GET("/me", func(c *gin.Context) {
				c.JSON(503, gin.H{"error": "Authentication disabled in standalone mode"})
			})
		}
	}

	// API endpoints - single /v1 group with optional auth middleware
	// In standalone mode, auth middleware is not applied
	var protected *gin.RouterGroup
	if auth != nil && !standaloneMode {
		protected = r.Group("/v1", auth.Middleware([]string{
			"/health", "/v1/health", "/metrics",
			"/v1/models/metadata", "/v1/providers",
			"/v1/tasks",            // Background task queue - public for challenge tests
			"/v1/models",           // Model list - public for challenge tests
			"/v1/chat/completions", // Chat - required for challenges
			"/v1/completions",      // Completions - required for challenges
			"/v1/messages",         // Anthropic Messages translator (Finding #20)
			"/v1/acp",              // ACP endpoints - public for CLI agents
			"/v1/vision",           // Vision endpoints - public for CLI agents
			"/v1/mcp",              // MCP endpoints - public for CLI agents (OpenCode, Crush, etc.)
			"/v1/lsp",              // LSP endpoints - public for CLI agents
			"/v1/embeddings",       // Embeddings endpoints - public for CLI agents
			"/v1/cognee",           // Cognee endpoints - public for CLI agents
			"/v1/rag",              // RAG endpoints - public for CLI agents
			"/v1/formatters",       // Formatters endpoints - public for CLI agents
			"/v1/monitoring",       // Monitoring endpoints - public for CLI agents
			"/v1/protocols",        // Protocol endpoints - public for CLI agents
		}))
	} else {
		// Standalone mode: no auth middleware
		protected = r.Group("/v1")
	}

	// Models.dev endpoints
	if cfg.ModelsDev.Enabled && modelMetadataHandler != nil {
		protected.GET("/models/metadata", modelMetadataHandler.ListModels)
		protected.GET("/models/metadata/:id", modelMetadataHandler.GetModel)
		protected.GET("/models/metadata/:id/benchmarks", modelMetadataHandler.GetModelBenchmarks)
		protected.GET("/models/metadata/compare", modelMetadataHandler.CompareModels)
		protected.GET("/models/metadata/capability/:capability", modelMetadataHandler.GetModelsByCapability)
	}
	{
		// Register OpenAI-compatible routes for seamless integration
		// This handles /completions, /chat/completions, /models
		unifiedHandler.RegisterOpenAIRoutes(protected, func(c *gin.Context) {
			c.Next()
		})

		// Anthropic-compatible /v1/messages translator (Finding #20).
		// Lets Claude Code and other Anthropic-protocol CLIs talk to
		// HelixAgent's ensemble without a wrapper. Delegates to
		// ChatCompletions internally, then re-shapes the response.
		unifiedHandler.RegisterAnthropicRoutes(protected, func(c *gin.Context) {
			c.Next()
		})

		// Google Gemini-compatible /v1beta/models/:model:generateContent
		// translator (Finding #21). Same dispatch pattern as Anthropic;
		// registered on the root engine because Google's URL contract is
		// /v1beta/... outside HelixAgent's /v1 namespace.
		unifiedHandler.RegisterGoogleRoutes(r, func(c *gin.Context) {
			c.Next()
		})

		// Note: Legacy /completions and /chat/completions routes removed
		// to avoid duplicates with UnifiedHandler routes

		// Ensemble endpoints
		protected.POST("/ensemble/completions", func(c *gin.Context) {
			var req handlers.CompletionRequest
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			// Force ensemble mode
			if req.EnsembleConfig == nil {
				req.EnsembleConfig = &models.EnsembleConfig{
					Strategy:            "confidence_weighted",
					MinProviders:        2,
					ConfidenceThreshold: 0.8,
					FallbackToBest:      true,
					Timeout:             30,
					PreferredProviders:  []string{},
				}
			}

			// Create a basic internal request
			internalReq := &models.LLMRequest{
				ID:        "ensemble-" + time.Now().Format("20060102150405"),
				SessionID: "ensemble-session",
				UserID:    "anonymous",
				Prompt:    req.Prompt,
				ModelParams: models.ModelParameters{
					Model:            req.Model,
					Temperature:      req.Temperature,
					MaxTokens:        req.MaxTokens,
					TopP:             req.TopP,
					StopSequences:    req.Stop,
					ProviderSpecific: map[string]any{},
				},
				EnsembleConfig: req.EnsembleConfig,
				MemoryEnhanced: req.MemoryEnhanced,
				Memory:         map[string]string{},
				Status:         "pending",
				CreatedAt:      time.Now(),
				RequestType:    "ensemble",
			}

			// Convert messages
			messages := make([]models.Message, 0, len(req.Messages))
			for _, msg := range req.Messages {
				messages = append(messages, models.Message{
					Role:      msg.Role,
					Content:   msg.Content,
					Name:      msg.Name,
					ToolCalls: msg.ToolCalls,
				})
			}
			internalReq.Messages = messages

			// Process with ensemble
			ensembleService := providerRegistry.GetEnsembleService()
			result, err := ensembleService.RunEnsemble(c.Request.Context(), internalReq)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			// Per-member visibility: expose EVERY participating member's
			// response (content), the model it served, its score, and whether it
			// was selected — so a consuming UI can show the operator each
			// member's answer + which model it used, not just the final winner.
			// Decoupled (CONST-051(B)): no consuming-project context here, just
			// the engine's own EnsembleResult.Responses re-shaped to JSON.
			members := make([]gin.H, 0, len(result.Responses))
			nameScores := make(map[string]float64, len(result.Responses))
			for _, r := range result.Responses {
				if r == nil {
					continue
				}
				model := ""
				if r.Metadata != nil {
					if m, ok := r.Metadata["model"].(string); ok {
						model = m
					}
				}
				members = append(members, gin.H{
					"provider_name":   r.ProviderName,
					"model":           model,
					"content":         r.Content,
					"confidence":      r.Confidence,
					"selection_score": r.SelectionScore,
					"selected":        r.ProviderName == result.Selected.ProviderName,
				})
				// Attributable scores keyed by provider name (result.Scores may
				// be keyed by an opaque id; this name-keyed map is renderable
				// directly by a consumer).
				nameScores[r.ProviderName] = r.SelectionScore
			}

			// Return ensemble result with metadata
			c.JSON(200, gin.H{
				"id":      result.Selected.ID,
				"object":  "ensemble.completion",
				"created": result.Selected.CreatedAt.Unix(),
				"model":   result.Selected.ProviderName,
				"choices": []gin.H{
					{
						"index": 0,
						"message": gin.H{
							"role":    "assistant",
							"content": result.Selected.Content,
						},
						"finish_reason": result.Selected.FinishReason,
					},
				},
				"usage": ensembleUsage(result.Selected),
				"ensemble": gin.H{
					"voting_method":     result.VotingMethod,
					"responses_count":   len(result.Responses),
					"scores":            result.Scores,
					"name_scores":       nameScores,
					"members":           members,
					"metadata":          result.Metadata,
					"selected_provider": result.Selected.ProviderName,
					"selection_score":   result.Selected.SelectionScore,
				},
			})
		})

		// Unified exposure catalog (SP2 P2.1/P2.2): one root list joining the
		// AI-debate ensemble (+ presets), HelixLLM (when enabled), every
		// discovered provider, and every VERIFIED model as uniformly-named
		// selectable targets. Reuses the live provider registry (the :773
		// providers path) + the ensemble path (:676). The Verified source is
		// the already-populated StartupVerifier (the same one wired into the
		// debate team at :907) so /v1/catalog surfaces REAL Verified models
		// within the CONST-037 24h window; honest-empty of model entries when
		// the StartupVerifier is not wired (GetStartupVerifier() == nil),
		// CONST-036.
		{
			catalogSvc := catalog.New(newCatalogOptions(providerRegistry, logger))
			catalogHandler := catalog.NewHandler(catalogSvc)
			protected.GET("/catalog", catalogHandler.List)
			// HA-F2-001 (FR-019): the /v1/models facade annotates
			// pseudo-model usability and appends the serving layer's own
			// options from this same built catalog, so the wire-visible
			// listing reflects real serving evidence.
			unifiedHandler.SetCatalogService(catalogSvc)
		}

		// Provider management endpoints
		providerMgmtHandler := handlers.NewProviderManagementHandler(providerRegistry, logger)
		providerGroup := protected.Group("/providers")
		{
			providerGroup.GET("", func(c *gin.Context) {
				providers := providerRegistry.ListProviders()
				result := make([]gin.H, 0, len(providers))

				for _, name := range providers {
					provider, err := providerRegistry.GetProvider(name)
					if err == nil {
						capabilities := provider.GetCapabilities()
						result = append(result, gin.H{
							"name":                      name,
							"supported_models":          capabilities.SupportedModels,
							"supported_features":        capabilities.SupportedFeatures,
							"supports_streaming":        capabilities.SupportsStreaming,
							"supports_function_calling": capabilities.SupportsFunctionCalling,
							"supports_vision":           capabilities.SupportsVision,
							"metadata":                  capabilities.Metadata,
						})
					}
				}

				c.JSON(200, gin.H{
					"providers": result,
					"count":     len(result),
				})
			})

			// Provider verification endpoints (for debate group validation)
			// NOTE: These must come BEFORE /:id routes to avoid matching issues
			providerGroup.GET("/verification", providerMgmtHandler.GetAllProvidersVerification)
			providerGroup.POST("/verify", providerMgmtHandler.VerifyAllProviders)

			// Provider auto-discovery endpoints (automatic detection from .env API keys)
			// NOTE: These must come BEFORE /:id routes to avoid matching issues
			providerGroup.GET("/discovery", providerMgmtHandler.GetDiscoverySummary)
			providerGroup.POST("/discover", providerMgmtHandler.DiscoverAndVerifyProviders)
			providerGroup.POST("/rediscover", providerMgmtHandler.ReDiscoverProviders)
			providerGroup.GET("/best", providerMgmtHandler.GetBestProviders)

			// Provider CRUD operations (parameterized routes must come AFTER specific routes)
			providerGroup.POST("", providerMgmtHandler.AddProvider)
			providerGroup.GET("/:id", providerMgmtHandler.GetProvider)
			providerGroup.PUT("/:id", providerMgmtHandler.UpdateProvider)
			providerGroup.DELETE("/:id", providerMgmtHandler.DeleteProvider)

			// Provider-specific verification endpoints
			providerGroup.GET("/:id/verification", providerMgmtHandler.GetProviderVerification)
			providerGroup.POST("/:id/verify", providerMgmtHandler.VerifyProvider)

			providerGroup.GET("/:id/health", func(c *gin.Context) {
				name := c.Param("id")
				health := providerRegistry.HealthCheck()

				response := gin.H{
					"provider": name,
				}

				if err, exists := health[name]; exists {
					if err != nil {
						response["healthy"] = false
						response["error"] = err.Error()
						c.JSON(503, response)
					} else {
						response["healthy"] = true

						// Add circuit breaker information if available
						if cb := providerRegistry.GetCircuitBreaker(name); cb != nil {
							response["circuit_breaker"] = gin.H{
								"state":         cb.GetState().String(),
								"failure_count": cb.GetFailureCount(),
								"last_failure":  cb.GetLastFailure(),
							}
						}

						c.JSON(200, response)
					}
				} else {
					c.JSON(404, gin.H{"error": "provider not found"})
				}
			})
		}

		// Session management endpoints
		sessionHandler := handlers.NewSessionHandler(logger)
		sessionGroup := protected.Group("/sessions")
		{
			sessionGroup.POST("", sessionHandler.CreateSession)
			sessionGroup.GET("/:id", sessionHandler.GetSession)
			sessionGroup.DELETE("/:id", sessionHandler.TerminateSession)
			sessionGroup.GET("", sessionHandler.ListSessions)
		}

		// CLI Agent registry endpoints
		agentHandler := handlers.NewAgentHandler()
		agentGroup := protected.Group("/agents")
		{
			agentGroup.GET("", agentHandler.ListAgents)
			agentGroup.GET("/:name", agentHandler.GetAgent)
			agentGroup.GET("/protocol/:protocol", agentHandler.ListAgentsByProtocol)
			agentGroup.GET("/tool/:tool", agentHandler.ListAgentsByTool)
		}

		// Cognee endpoints - comprehensive API with all features
		cogneeAPIHandler.RegisterRoutes(protected)

		// AI Debate endpoints with Claude/Qwen team configuration
		// Initialize debate team configuration with provider discovery
		debateTeamConfig := services.NewDebateTeamConfig(
			providerRegistry,
			providerRegistry.GetDiscovery(),
			logger,
		)

		// CRITICAL: Set the StartupVerifier so that DebateTeamConfig uses
		// the unified verification pipeline instead of the legacy path.
		// Without this, OAuth providers (Claude, Qwen) won't be included!
		if sv := providerRegistry.GetStartupVerifier(); sv != nil {
			debateTeamConfig.SetStartupVerifier(sv)
			logger.Info("DebateTeamConfig configured with StartupVerifier (OAuth providers will be included)")

			// Initialize the debate team (Claude Sonnet/Opus for positions 1-2,
			// LLMsVerifier-scored providers for 3-5, Qwen as fallbacks)
			if err := debateTeamConfig.InitializeTeam(context.Background()); err != nil {
				logger.WithError(err).Warn("Failed to initialize debate team, some positions may be unfilled")
			}
		} else {
			logger.Warn("StartupVerifier not available - skipping debate team initialization (will be initialized later via ReinitializeDebateTeam)")
		}

		// 		// Set the debate team config on the unified handler for dialogue display
		unifiedHandler.SetDebateTeamConfig(debateTeamConfig)

		// Initialize IntentBasedRouter for intelligent ensemble routing
		// Simple messages (greetings, confirmations) → single provider
		// Complex requests (debug, refactor, implement) → full ensemble
		var intentRouter *services.IntentBasedRouter
		// llmClassifier is hoisted to the outer scope so the agentic
		// ensemble wiring below can route actionable prompts through the
		// decompose→execute pipeline via the same LLM-backed classifier.
		var llmClassifier *services.LLMIntentClassifier
		if sv := providerRegistry.GetStartupVerifier(); sv != nil {
			intentRouter = services.NewIntentBasedRouter(sv, logger)
			// Wire LLM-based intent classifier for real semantic understanding
			// Uses the strongest scored model for multilingual intent recognition
			llmClassifier = services.NewLLMIntentClassifier(providerRegistry, logger)
			intentRouter.SetLLMClassifier(llmClassifier)
			unifiedHandler.SetIntentBasedRouter(intentRouter)
			logger.Info("IntentBasedRouter configured with LLM classifier - semantic intent routing for all languages")
		} else {
			logger.Warn("StartupVerifier not available - IntentBasedRouter not initialized, all requests will use ensemble")
		}
		_ = intentRouter // Avoid unused variable warning

		// Store references for later re-initialization with StartupVerifier
		rc.DebateTeamConfig = debateTeamConfig
		rc.unifiedHandler = unifiedHandler
		// Note: rc.orchestratorIntegration is set below after CreateIntegration

		debateService := services.NewDebateServiceWithDeps(logger, providerRegistry, cogneeService)
		debateService.SetTeamConfig(debateTeamConfig) // Set the team configuration
		rc.debateService = debateService
		debateHandler := handlers.NewDebateHandler(debateService, nil, logger)

		// Wire up the NEW debate orchestrator framework (MANDATORY - per docs/requests/debate)
		// This implements: 5 positions × 3 LLMs = 15 agents
		// 8-phase protocol: Dehallucination → SelfEvolvement → Proposal → Critique → Review → Optimization → Adversarial → Convergence
		orchestratorIntegration := debate_integration.CreateIntegration(providerRegistry, logger)
		rc.orchestratorIntegration = orchestratorIntegration
		debateHandler.SetOrchestratorIntegration(orchestratorIntegration)

		// CRITICAL: Also set orchestrator on UnifiedHandler so chat completions use NEW debate system
		unifiedHandler.SetOrchestratorIntegration(orchestratorIntegration)

		// CRITICAL: Set debate service on UnifiedHandler so it can use the configured debate team
		unifiedHandler.SetDebateService(debateService)

		// CRITICAL: Wire the real dual-mode agentic ensemble (decompose →
		// execute, subagent-driven) as the PRIMARY chat-completions path so
		// it runs out-of-the-box (no opt-in flag). Without this the engine
		// at services.AgenticEnsemble is constructed nowhere and the gate in
		// processWithEnsemble (h.agenticEnsemble != nil) is always false — the
		// engine never runs. BuildAgenticEnsemble reuses the existing engine
		// (planner + verifier + classifier + debate service + registry);
		// EnableExecution defaults true so actionable prompts decompose and
		// dispatch subagents.
		agenticEnsemble := handlers.BuildAgenticEnsemble(
			debateService,
			llmClassifier,
			providerRegistry,
			logger,
		)
		unifiedHandler.SetAgenticEnsemble(agenticEnsemble)
		logger.Info("AgenticEnsemble wired as PRIMARY chat path (dual-mode decompose→execute, subagent-driven, out-of-the-box)")

		// Wire the comprehensive IntegrationManager so its agent pool gets populated
		if ci := debateService.GetComprehensiveIntegration(); ci != nil {
			orchestratorIntegration.SetComprehensiveIntegration(ci)
		}

		// Populate the orchestrator agent pool from the debate team's verified providers.
		// This ensures the orchestrator uses the same verified, scored, and filtered providers
		// (no Ollama/local, no nil instances) as the debate team config.
		orchestratorIntegration.PopulateFromDebateTeam(debateTeamConfig)

		// Initialize verification report generator for provider scores
		// Note: This will generate reports on-demand during debates
		reportGenerator := services.NewVerificationReportGenerator(
			nil, // verificationSvc - will be initialized on first use
			nil, // scoreAdapter - will be initialized on first use
			logger,
		)
		unifiedHandler.SetReportGenerator(reportGenerator)
		logger.WithField("report_path", reportGenerator.GetReportPath()).Info("Verification report generator initialized")

		// Log agent pool status for verification
		agentCount := orchestratorIntegration.GetOrchestrator().GetAgentPool().Size()
		logger.WithFields(logrus.Fields{
			"framework":     "new_orchestrator",
			"agent_count":   agentCount,
			"min_required":  3,
			"target_agents": 15, // 5 positions × 3 LLMs
		}).Info("NEW DEBATE ORCHESTRATOR FRAMEWORK ENABLED (per docs/requests/debate requirements)")

		// Initialize Constitution Watcher (auto-update Constitution on project changes)
		projectRoot := os.Getenv("PROJECT_ROOT")
		if projectRoot == "" {
			// Default to current working directory if not set
			if cwd, err := os.Getwd(); err == nil {
				projectRoot = cwd
			}
		}
		constitutionWatcherEnabled := os.Getenv("CONSTITUTION_WATCHER_ENABLED") == "true"
		debateService.InitializeConstitutionWatcher(projectRoot, constitutionWatcherEnabled)
		if constitutionWatcherEnabled {
			logger.WithField("project_root", projectRoot).Info("Constitution Watcher initialized")
		}

		// Inject real debate function into HelixSpecifier engine
		// (must happen after DebateService is fully constructed)
		debateService.InitializeHelixSpecifierDebate()

		debateHandler.RegisterRoutes(protected)

		// Add debate team configuration endpoint (PUBLIC - no auth required for debugging)
		r.GET("/v1/debates/team", func(c *gin.Context) {
			c.JSON(http.StatusOK, debateTeamConfig.GetTeamSummary())
		})

		// Add NEW orchestrator framework status endpoint
		protected.GET("/debates/orchestrator/status", func(c *gin.Context) {
			stats, err := orchestratorIntegration.GetStatistics(c.Request.Context())
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"framework":                "new_debate_orchestrator",
				"documentation_compliance": "docs/requests/debate requirements",
				"target_agents":            15, // 5 positions × 3 LLMs
				"statistics":               stats,
			})
		})

		// Initialize monitoring services
		oauthTokenMonitor := services.NewOAuthTokenMonitor(logger, services.DefaultOAuthTokenMonitorConfig())
		providerHealthMonitor := services.NewProviderHealthMonitor(providerRegistry, logger, services.DefaultProviderHealthMonitorConfig())
		concurrencyMonitor := services.NewConcurrencyMonitor(providerRegistry, logger, services.DefaultConcurrencyMonitorConfig())
		fallbackChainValidator := services.NewFallbackChainValidator(logger, debateTeamConfig)

		// Store monitors in RouterContext for cleanup
		rc.oauthMonitor = oauthTokenMonitor
		rc.oauthCredentialManager = oauthManager
		rc.healthMonitor = providerHealthMonitor
		rc.concurrencyMonitor = concurrencyMonitor

		// Initialize concurrency alert manager
		concurrencyAlertManager := services.NewConcurrencyAlertManager(services.LoadConcurrencyAlertManagerConfigFromEnv(), logger)
		concurrencyMonitor.AddAlertListener(concurrencyAlertManager.AsListener())
		rc.concurrencyAlertManager = concurrencyAlertManager

		// Start monitoring services in background
		go oauthTokenMonitor.Start(context.Background())
		go providerHealthMonitor.Start(context.Background())
		go concurrencyMonitor.Start(context.Background())
		go concurrencyAlertManager.Start(context.Background())

		// Start Constitution Watcher in background (if enabled)
		if constitutionWatcherEnabled {
			debateService.StartConstitutionWatcher(context.Background())
		}

		// Validate fallback chain on startup
		if result := fallbackChainValidator.Validate(); !result.Valid {
			logger.WithField("issues", len(result.Issues)).Warn("Fallback chain validation found issues")
		}

		// Register monitoring handler routes — wire real CircuitBreakerMonitor
		// with the default manager so /v1/monitoring/circuit-breakers serves
		// real state, not 503. CONST-035 §c "Completion".
		cbMgr := llm.NewDefaultCircuitBreakerManager()
		cbMonitor := services.NewCircuitBreakerMonitor(cbMgr, logger, services.DefaultCircuitBreakerMonitorConfig())
		monitoringHandler := handlers.NewMonitoringHandler(cbMonitor, oauthTokenMonitor, providerHealthMonitor, fallbackChainValidator, concurrencyMonitor, concurrencyAlertManager)
		monitoringHandler.RegisterRoutes(protected)
		protocolSSEHandler.SetMonitoringHandler(monitoringHandler)
		logger.Info("Monitoring endpoints registered at /v1/monitoring/*")

		// LSP endpoints
		lspGroup := protected.Group("/lsp")
		{
			lspGroup.GET("/servers", lspHandler.ListLSPServers)
			lspGroup.POST("/execute", lspHandler.ExecuteLSPRequest)
			lspGroup.POST("/sync", lspHandler.SyncLSPServers)
			lspGroup.GET("/stats", lspHandler.GetLSPStats)
		}

		// MCP endpoints
		mcpGroup := protected.Group("/mcp")
		{
			mcpGroup.GET("/capabilities", mcpHandler.MCPCapabilities)
			mcpGroup.GET("/tools", mcpHandler.MCPTools)
			mcpGroup.POST("/tools/call", mcpHandler.MCPToolsCall)
			mcpGroup.GET("/prompts", mcpHandler.MCPPrompts)
			mcpGroup.GET("/resources", mcpHandler.MCPResources)

			// MCP Tool Search endpoints
			mcpGroup.GET("/tools/search", mcpHandler.MCPToolSearch)
			mcpGroup.POST("/tools/search", mcpHandler.MCPToolSearch)
			mcpGroup.GET("/tools/suggestions", mcpHandler.MCPToolSuggestions)
			mcpGroup.GET("/adapters/search", mcpHandler.MCPAdapterSearch)
			mcpGroup.POST("/adapters/search", mcpHandler.MCPAdapterSearch)
			mcpGroup.GET("/categories", mcpHandler.MCPCategories)
			mcpGroup.GET("/stats", mcpHandler.MCPStats)
		}
		logger.Info("MCP Tool Search endpoints registered at /v1/mcp/tools/search, /v1/mcp/adapters/search")

		// Protocol endpoints
		protocolGroup := protected.Group("/protocols")
		{
			protocolGroup.POST("/execute", protocolHandler.ExecuteProtocolRequest)
			protocolGroup.GET("/servers", protocolHandler.ListProtocolServers)
			protocolGroup.GET("/metrics", protocolHandler.GetProtocolMetrics)
			protocolGroup.POST("/refresh", protocolHandler.RefreshProtocolServers)
			protocolGroup.POST("/configure", protocolHandler.ConfigureProtocols)
		}

		// Embedding endpoints
		embeddingGroup := protected.Group("/embeddings")
		{
			embeddingGroup.POST("/generate", embeddingHandler.GenerateEmbeddings)
			embeddingGroup.POST("/search", embeddingHandler.VectorSearch)
			// CONST-035 §c: SimilaritySearch was a contract bluff — the
			// handler was implemented and documented as
			// "POST /v1/embeddings/similarity" but never registered in
			// the router. Wired now (round 30) so the documented surface
			// matches what the binary serves. Functionally an alias for
			// VectorSearch sharing the same VectorSearchRequest shape.
			embeddingGroup.POST("/similarity", embeddingHandler.SimilaritySearch)
			embeddingGroup.POST("/index", embeddingHandler.IndexDocument)
			embeddingGroup.POST("/batch-index", embeddingHandler.BatchIndexDocuments)
			embeddingGroup.GET("/stats", embeddingHandler.GetEmbeddingStats)
			embeddingGroup.GET("/providers", embeddingHandler.ListEmbeddingProviders)
		}

		// RAG (Retrieval Augmented Generation) endpoints.
		//
		// Pipeline is intentionally nil: RAG requires both an embedding
		// model registry AND a reachable vector database (Chroma /
		// Qdrant / Weaviate). Wiring requires non-trivial config that
		// HelixAgent does not currently surface as env vars. Until that
		// config is added, every /v1/rag/* endpoint returns 503 with
		// {"status":"not_configured","details":{"error":"RAG pipeline
		// not initialized"}} — honest behavior, no bluff. To enable
		// RAG, construct rag.NewPipeline(cfg, embeddingRegistry), call
		// pipeline.Initialize(ctx), and pass the result here.
		ragHandler := handlers.NewRAGHandler(handlers.RAGHandlerConfig{
			Pipeline: nil,
			Logger:   logger,
		})
		ragGroup := protected.Group("/rag")
		{
			// Health and Stats
			ragGroup.GET("/health", ragHandler.Health)
			ragGroup.GET("/stats", ragHandler.Stats)

			// Document operations
			ragGroup.POST("/documents", ragHandler.IngestDocument)
			ragGroup.POST("/documents/batch", ragHandler.IngestDocuments)
			ragGroup.DELETE("/documents/:id", ragHandler.DeleteDocument)

			// Search operations
			ragGroup.POST("/search", ragHandler.Search)
			ragGroup.POST("/search/hybrid", ragHandler.HybridSearch)
			ragGroup.POST("/search/expanded", ragHandler.SearchWithExpansion)

			// Advanced RAG features
			ragGroup.POST("/rerank", ragHandler.ReRank)
			ragGroup.POST("/compress", ragHandler.CompressContext)
			ragGroup.POST("/expand", ragHandler.ExpandQuery)
			ragGroup.POST("/chunk", ragHandler.ChunkDocument)
		}
		protocolSSEHandler.SetRAGHandler(ragHandler)
		logger.Info("RAG endpoints registered at /v1/rag/*")

		// Ensemble session/team management endpoints — wire real
		// multi_instance.Coordinator + clis.InstanceManager + (when
		// reachable) Postgres *sql.DB for durable session persistence.
		//
		// CONST-035 §c: closed #ensemble-instance-manager-wiring (round
		// 24, in-memory) and #ensemble-db-wiring (round 26, this commit
		// — Postgres connection optional with graceful nil-fallback).
		//
		// When Postgres is reachable: sessions persist across restarts
		// via the existing INSERT/UPDATE statements in coordinator.go
		// and instance_manager.go that the round-24 nil-guards now run
		// instead of skipping.
		//
		// When Postgres is NOT reachable (test/dev/CI environments
		// without infra running): db == nil; the same nil-guards skip
		// persistence and the system falls back to in-memory mode. End
		// users see identical lifecycle behavior either way.
		ensembleSQLCtx, ensembleSQLCancel := context.WithTimeout(context.Background(), 5*time.Second)
		ensembleSQLDB, ensembleSQLErr := database.OpenSQLDB(ensembleSQLCtx)
		ensembleSQLCancel()
		if ensembleSQLErr != nil {
			logger.WithError(ensembleSQLErr).Info("Ensemble: Postgres unreachable; using in-memory session storage (durability disabled)")
		}
		ensembleInstanceMgr, ensembleInstanceMgrErr := clis.NewInstanceManager(ensembleSQLDB, nil)
		if ensembleInstanceMgrErr != nil {
			logger.WithError(ensembleInstanceMgrErr).Warn("Failed to init clis.InstanceManager; /v1/ensemble/sessions will return created_without_instances")
		}
		ensembleCoordinator := multi_instance.NewCoordinator(ensembleSQLDB, nil, ensembleInstanceMgr, nil)
		ensembleHandler := handlers.NewEnsembleHandler(ensembleCoordinator, logger)
		ensembleHandler.RegisterRoutes(protected)
		switch {
		case ensembleInstanceMgr != nil && ensembleSQLDB != nil:
			logger.Info("Ensemble session/team endpoints registered at /v1/ensemble/* (Coordinator + InstanceManager + Postgres *sql.DB all wired — durable)")
		case ensembleInstanceMgr != nil:
			logger.Info("Ensemble session/team endpoints registered at /v1/ensemble/* (Coordinator + InstanceManager wired in-memory; Postgres unreachable)")
		default:
			logger.Warn("Ensemble session/team endpoints registered at /v1/ensemble/* (Coordinator wired; InstanceManager init failed — sessions will be created_without_instances)")
		}
		// Track sql.DB on RouterContext for graceful Close() on shutdown.
		rc.ensembleSQLDB = ensembleSQLDB

		// Completion endpoints (skills-enhanced completion with intent routing)
		completionHandler := handlers.NewCompletionHandler(providerRegistry.GetRequestService())
		// CONST-036 / BLUFF-002: source /v1/completion/models from the live
		// provider registry (authoritative), never a hardcoded literal.
		completionHandler.SetModelSource(providerRegistry)
		completionHandler.SetSkillsIntegration(skillsIntegration)
		if intentRouter != nil {
			completionHandler.SetIntentBasedRouter(intentRouter)
		}
		completionGroup := protected.Group("/completion")
		{
			completionGroup.POST("", completionHandler.Complete)
			completionGroup.POST("/stream", completionHandler.CompleteStream)
			completionGroup.POST("/chat", completionHandler.Chat)
			completionGroup.POST("/chat/stream", completionHandler.ChatStream)
			completionGroup.GET("/models", completionHandler.Models)
		}
		logger.Info("Completion endpoints registered at /v1/completion/*")

		// ACP (Agent Communication Protocol) endpoints
		// Using acpHandler already created earlier
		acpHandler.RegisterRoutes(protected)
		logger.Info("ACP endpoints registered at /v1/acp/*")

		// Vision endpoints
		visionHandler := handlers.NewVisionHandler(providerRegistry, logger)
		visionHandler.RegisterRoutes(protected)
		logger.Info("Vision endpoints registered at /v1/vision/*")

		// Code Formatters endpoints (all public - formatters run locally and are safe)
		formattersRegistry, formattersExecutor, formattersHealth := initializeFormattersSystem(logger)
		if formattersRegistry != nil {
			formattersHandler := handlers.NewFormattersHandler(formattersRegistry, formattersExecutor, formattersHealth, logger)

			// All formatter endpoints are public (no sensitive operations)
			v1Public := r.Group("/v1")
			v1Public.POST("/format", formattersHandler.FormatCode)
			v1Public.POST("/format/batch", formattersHandler.FormatCodeBatch)
			v1Public.POST("/format/check", formattersHandler.CheckCode)
			v1Public.POST("/formatters/:name/validate-config", formattersHandler.ValidateConfig)
			v1Public.GET("/formatters", formattersHandler.ListFormatters)
			v1Public.GET("/formatters/detect", formattersHandler.DetectFormatter)
			v1Public.GET("/formatters/:name", formattersHandler.GetFormatter)
			v1Public.GET("/formatters/:name/health", formattersHandler.HealthCheckFormatter)

			protocolSSEHandler.SetFormattersHandler(formattersHandler)
			logger.Info("Code Formatters endpoints registered (all public)")
		} else {
			logger.Warn("Code Formatters system not available")
		}

		// Register Protocol SSE endpoints for MCP/ACP/LSP/Embeddings/Vision/Cognee
		// These endpoints handle SSE connections for CLI agent protocols (OpenCode, Crush, HelixCode)
		protocolSSEHandler.RegisterSSERoutes(protected)

		// Skills endpoints (skill registry integration)
		if skillsIntegration != nil {
			skillsHandler := handlers.NewSkillsHandler(skillsIntegration)
			skillsHandler.SetLogger(logger)
			skillsGroup := protected.Group("/skills")
			{
				skillsGroup.GET("", skillsHandler.ListSkills)
				skillsGroup.GET("/categories", skillsHandler.ListCategories)
				skillsGroup.GET("/:category", skillsHandler.GetSkillsByCategory)
				skillsGroup.POST("/match", skillsHandler.MatchSkills)
			}
			logger.Info("Skills endpoints registered at /v1/skills/*")
		}

		// Background Task endpoints — wire the in-memory TaskRepository so
		// /v1/tasks/* serves real responses, not 503. CONST-035 §c
		// "Completion": no stub gaps for storage/queue. The
		// InMemoryTaskRepository is process-local (tasks don't survive
		// restart). For durability swap the Postgres-backed
		// BackgroundTaskRepository in internal/database/ via the same
		// TaskRepository interface.
		//
		// #task-worker-pool-wiring CLOSED (round 22): the in-memory
		// drainer below polls the queue and transitions every task
		// pending → running → completed. Without registered executors,
		// tasks complete with `task.completed_noop` events; with executors
		// (RegisterExecutor) they run real work. Either way, the documented
		// state-machine progresses, closing the contract bluff where every
		// task stayed in "pending" forever.
		taskRepo := background.NewInMemoryTaskRepository()
		taskQueue := background.NewInMemoryTaskQueue(logger)
		backgroundTaskHandler := handlers.NewBackgroundTaskHandler(
			taskRepo, taskQueue, nil, nil, nil, nil, nil, nil, nil, nil, logger,
		)
		backgroundTaskHandler.RegisterRoutes(protected)
		taskWorker := background.NewInMemoryWorker(taskRepo, taskQueue, logger)
		taskWorker.Start(context.Background())
		rc.taskWorker = taskWorker // store for graceful shutdown
		logger.Info("Background task endpoints registered at /v1/tasks/* (real CRUD + drainer worker wired; tasks transition pending→running→completed)")

		// Discovery endpoints — wired with ProviderRegistry fallback so
		// /v1/discovery/models always returns real data (each provider's
		// SupportedModels) even when the optional verifier-driven
		// ModelDiscoveryService isn't configured. Other discovery
		// endpoints still 503 until the full service is wired.
		discoveryHandler := handlers.NewDiscoveryHandlerWithRegistry(
			nil, // ModelDiscoveryService — optional, not wired here
			providerRegistry,
			func(providerName string) []string {
				prov, err := providerRegistry.GetProvider(providerName)
				if err != nil || prov == nil {
					return nil
				}
				caps := prov.GetCapabilities()
				if caps == nil {
					return nil
				}
				return caps.SupportedModels
			},
		)
		discoveryGroup := protected.Group("/discovery")
		{
			discoveryGroup.GET("/models", discoveryHandler.GetDiscoveredModels)
			discoveryGroup.GET("/models/selected", discoveryHandler.GetSelectedModels)
			discoveryGroup.GET("/stats", discoveryHandler.GetDiscoveryStats)
			discoveryGroup.POST("/trigger", discoveryHandler.TriggerDiscovery)
			discoveryGroup.GET("/ensemble", discoveryHandler.GetEnsembleModels)
			discoveryGroup.GET("/debate-model", discoveryHandler.GetModelForDebate)
		}
		logger.Info("Discovery endpoints registered at /v1/discovery/* (registry fallback active)")

		// Scoring endpoints — wire a real verifier.ScoringService so endpoints
		// return 200 with computed scores, not 503. CONST-035 §c "Completion":
		// no stub / placeholder gaps that silently 503.
		scoringSvc, scoringErr := verifier.NewScoringService(nil)
		if scoringErr != nil {
			logger.WithError(scoringErr).Warn("Failed to initialise ScoringService; /v1/scoring/* will return 503")
		}
		scoringHandler := handlers.NewScoringHandler(scoringSvc)
		scoringGroup := protected.Group("/scoring")
		{
			scoringGroup.GET("/model/:model_id", scoringHandler.GetModelScore)
			scoringGroup.POST("/batch", scoringHandler.BatchCalculateScores)
			scoringGroup.GET("/top", scoringHandler.GetTopModels)
			scoringGroup.GET("/range", scoringHandler.GetModelsByScoreRange)
			scoringGroup.GET("/weights", scoringHandler.GetScoringWeights)
			scoringGroup.PUT("/weights", scoringHandler.UpdateScoringWeights)
			scoringGroup.GET("/model/:model_id/detail", scoringHandler.GetModelNameWithScore)
			scoringGroup.POST("/cache/invalidate", scoringHandler.InvalidateCache)
			scoringGroup.POST("/compare", scoringHandler.CompareModels)
		}
		if scoringSvc != nil {
			logger.Info("Scoring endpoints registered at /v1/scoring/* (real ScoringService wired)")
		} else {
			logger.Warn("Scoring endpoints registered at /v1/scoring/* (ScoringService init failed; will 503)")
		}

		// Verification endpoints — wire real verifier services so endpoints
		// return 200, not 503. CONST-035 §c "Completion": no stub gaps.
		verificationSvc := verifier.NewVerificationService(nil)
		healthSvcForVerification := verifier.NewHealthService(nil)
		extRegistry, extRegistryErr := adapters.NewExtendedProviderRegistry(nil)
		if extRegistryErr != nil {
			logger.WithError(extRegistryErr).Warn("Failed to init ExtendedProviderRegistry; /v1/verification/{models,health} will 503")
		}
		// Bridge LLM providerRegistry → ExtendedProviderRegistry so
		// /v1/verification/models returns real model list (not total=0).
		// CONST-035 §c "Completion": empty advertised list is a contract
		// bluff against docs/api/API_REFERENCE.md.
		if extRegistry != nil {
			provNames := providerRegistry.ListProviders()
			logger.Infof("ExtendedProviderRegistry bridge: importing %d provider(s) from providerRegistry", len(provNames))
			totalModels := 0
			for _, providerName := range provNames {
				p, err := providerRegistry.GetProvider(providerName)
				if err != nil || p == nil {
					continue
				}
				caps := p.GetCapabilities()
				modelList := []string{}
				if caps != nil {
					modelList = caps.SupportedModels
				}
				if regErr := extRegistry.RegisterProvider(context.Background(), providerName, providerName, "", "", modelList); regErr != nil {
					logger.WithError(regErr).Debugf("ExtendedProviderRegistry.RegisterProvider(%s) failed", providerName)
				} else {
					totalModels += len(modelList)
				}
			}
			logger.Infof("ExtendedProviderRegistry bridge: registered %d models across %d providers", totalModels, len(provNames))
		}
		verificationHandler := handlers.NewVerificationHandler(verificationSvc, scoringSvc, healthSvcForVerification, extRegistry)
		verificationGroup := protected.Group("/verification")
		{
			verificationGroup.POST("/model", verificationHandler.VerifyModel)
			verificationGroup.POST("/batch", verificationHandler.BatchVerify)
			verificationGroup.GET("/status", verificationHandler.GetVerificationStatus)
			verificationGroup.GET("/models", verificationHandler.GetVerifiedModels)
			verificationGroup.POST("/model/:model_id/reverify", verificationHandler.ReVerifyModel)
			verificationGroup.GET("/tests", verificationHandler.GetVerificationTests)
			verificationGroup.GET("/health", verificationHandler.GetVerificationHealth)
			verificationGroup.POST("/code-visibility", verificationHandler.TestCodeVisibility)
		}
		logger.Info("Verification endpoints registered at /v1/verification/* (real VerificationService wired)")

		// Health monitoring endpoints — wire real HealthService so endpoints
		// return 200, not 503. Bridge LLM providers from the providerRegistry
		// into the HealthService at boot so /v1/health/providers returns the
		// real provider list (not an empty list, which would be a contract
		// bluff against docs/api/API_REFERENCE.md §"GET /v1/health/providers").
		healthSvc := verifier.NewHealthService(nil)
		for _, providerName := range providerRegistry.ListProviders() {
			healthSvc.AddProvider(providerName, providerName)
		}
		healthHandler := handlers.NewHealthHandler(healthSvc)
		healthGroup := protected.Group("/health")
		{
			healthGroup.GET("/providers", healthHandler.GetAllProvidersHealth)
			healthGroup.GET("/providers/healthy", healthHandler.GetHealthyProviders)
			healthGroup.GET("/providers/fastest", healthHandler.GetFastestProvider)
			healthGroup.GET("/provider/:provider_id", healthHandler.GetProviderHealth)
			healthGroup.GET("/provider/:provider_id/latency", healthHandler.GetProviderLatency)
			healthGroup.GET("/provider/:provider_id/available", healthHandler.IsProviderAvailable)
			healthGroup.GET("/circuit-breakers", healthHandler.GetCircuitBreakerStatus)
			healthGroup.POST("/provider/:provider_id/success", healthHandler.RecordSuccess)
			healthGroup.POST("/provider/:provider_id/failure", healthHandler.RecordFailure)
			healthGroup.POST("/provider", healthHandler.AddProvider)
			healthGroup.DELETE("/provider/:provider_id", healthHandler.RemoveProvider)
			healthGroup.GET("/status", healthHandler.GetHealthServiceStatus)
		}
		logger.Info("Health monitoring endpoints registered at /v1/health/* (real HealthService wired)")

		// Agentic workflow endpoints (graph-based workflow orchestration)
		agenticHandler := handlers.NewAgenticHandler(logger)
		handlers.RegisterAgenticRoutes(protected, agenticHandler)
		logger.Info("Agentic workflow endpoints registered at /v1/agentic/*")

		// Planning algorithm endpoints (HiPlan, MCTS, Tree of Thoughts)
		planningHandler := handlers.NewPlanningHandler(logger)
		// Wire LLM RequestService so generateTaskBreakdown produces real
		// task-specific decomposition (not the 5-step template). Closes
		// #planning-llm-task-breakdown. Falls back to template when the
		// LLM is unreachable.
		planningHandler.SetRequestService(providerRegistry.GetRequestService())
		handlers.RegisterPlanningRoutes(protected, planningHandler)
		logger.Info("Planning algorithm endpoints registered at /v1/planning/*")

		// LLMOps endpoints — wire a real LLMOpsSystem so /v1/llmops/*
		// returns real data instead of 503. Constructor accepts nil config
		// (defaults) and Initialize wires up in-memory PromptRegistry,
		// AlertManager, etc.
		llmopsSystem := llmops.NewLLMOpsSystem(nil, logger)
		_ = llmopsSystem.Initialize() // best-effort
		llmopsHandler := handlers.NewLLMOpsHandler(llmopsSystem)
		handlers.RegisterLLMOpsRoutes(protected, llmopsHandler)
		logger.Info("LLMOps endpoints registered at /v1/llmops/* (real LLMOpsSystem wired)")

		// Benchmark endpoints — wire a real BenchmarkSystem so /v1/benchmark/*
		// returns real data instead of 503. The system uses defaults when
		// nil config is passed; provider adapter remains nil until a future
		// commit wires it to the registry.
		benchmarkSystem := benchmark.NewBenchmarkSystem(nil, logger)
		_ = benchmarkSystem.Initialize(nil) // best-effort; nil provider OK
		benchmarkHandler := handlers.NewBenchmarkHandler(benchmarkSystem)
		handlers.RegisterBenchmarkRoutes(protected, benchmarkHandler)
		logger.Info("Benchmark endpoints registered at /v1/benchmark/* (real BenchmarkSystem wired)")

		// QA endpoints — wire a real HelixQA adapter. New(nil) accepts
		// nil logger and uses a default. Initialize() opens the SQLite
		// memory store at "data/memory.db" — best-effort: handler still
		// works with adapter alone if Initialize fails (the adapter
		// gates DB-backed operations on the store being non-nil).
		qaAdapter := helixqaadapter.New(logger)
		if err := qaAdapter.Initialize(""); err != nil {
			logger.WithError(err).Warn("HelixQA adapter Initialize failed; some endpoints may degrade")
		}
		qaHandler := handlers.NewQAHandler(qaAdapter)
		handlers.RegisterQARoutes(protected, qaHandler)
		logger.Info("QA endpoints registered at /v1/qa/* (real HelixQA adapter wired)")

		// Semantic Search endpoints — vector-based code search with ChromaDB/Qdrant
		searchService, searchErr := initializeSearchService(cfg, logger, rc.containerAdapter)
		if searchErr != nil {
			logger.WithError(searchErr).Warn("Failed to initialize semantic search service, continuing without search")
		} else if searchService != nil {
			searchHandler := handlers.NewSearchHandler(searchService.Searcher, searchService.Indexer)
			searchHandler.RegisterRoutes(r)
			logger.Info("Semantic search endpoints registered at /v1/search/*")
		}

		// Context Templates endpoints — reusable prompt templates with Git integration
		templateManager, templateErr := templates.NewManager(templates.DefaultManagerConfig())
		if templateErr != nil {
			logger.WithError(templateErr).Warn("Failed to initialize template manager, continuing without templates")
		} else {
			templateHandler := handlers.NewTemplateHandler(templateManager)
			templateHandler.RegisterRoutes(r)
			logger.Info("Context template endpoints registered at /v1/templates/*")
		}

		// Checkpoints endpoints — workspace snapshots with Git state capture
		checkpointManager, checkpointErr := checkpoints.NewManager(".")
		if checkpointErr != nil {
			logger.WithError(checkpointErr).Warn("Failed to initialize checkpoint manager, continuing without checkpoints")
		} else {
			checkpointHandler := handlers.NewCheckpointHandler(checkpointManager)
			checkpointHandler.RegisterRoutes(r)
			logger.Info("Checkpoint endpoints registered at /v1/checkpoints/*")
		}

		// Browser Automation endpoints — Playwright-based web automation
		browserManager, browserErr := browser.NewManager(browser.DefaultConfig())
		if browserErr != nil {
			logger.WithError(browserErr).Warn("Failed to initialize browser manager, continuing without browser automation")
		} else {
			browserHandler := handlers.NewBrowserHandler(browserManager)
			browserHandler.RegisterRoutes(r)
			logger.Info("Browser automation endpoints registered at /v1/browser/*")
		}

		// GraphQL endpoint (feature-flagged, disabled by default for backward compatibility)
		if os.Getenv("GRAPHQL_ENABLED") == "true" {
			if err := helixgraphql.InitSchema(); err != nil {
				logger.WithError(err).Error("Failed to initialize GraphQL schema")
			} else {
				protected.POST("/graphql", func(c *gin.Context) {
					var reqBody struct {
						Query     string                 `json:"query"`
						Variables map[string]interface{} `json:"variables"`
					}
					if err := c.ShouldBindJSON(&reqBody); err != nil {
						c.JSON(http.StatusBadRequest, gin.H{"error": "invalid GraphQL request: " + err.Error()})
						return
					}
					result := helixgraphql.ExecuteQuery(reqBody.Query, reqBody.Variables)
					c.JSON(http.StatusOK, result)
				})
				logger.Info("GraphQL endpoint enabled at /v1/graphql")
			}
		}

		// Admin endpoints
		admin := protected.Group("/admin")
		admin.Use(auth.RequireAdmin())
		{
			admin.GET("/health/all", func(c *gin.Context) {
				health := providerRegistry.HealthCheck()
				c.JSON(200, gin.H{
					"provider_health": health,
					"timestamp":       time.Now().Unix(),
				})
			})

			// Models.dev admin endpoints (if enabled)
			if cfg.ModelsDev.Enabled && modelMetadataHandler != nil {
				admin.POST("/models/metadata/refresh", modelMetadataHandler.RefreshModels)
				admin.GET("/models/metadata/refresh/status", modelMetadataHandler.GetRefreshStatus)
			}
		}
	}

	rc.Engine = r
	return rc
}

// initializeSearchService initializes the semantic search service with configuration
// newCatalogOptions assembles exactly the catalog the live GET /v1/catalog
// route serves.
//
// It exists as a named function rather than a literal inside the route so the
// composition itself is testable: the guard in this package builds the real
// options and asserts what the catalog does and does not advertise. A literal
// buried in a route is reachable only by booting the whole router, which is why
// the hardcoded HelixLLM id list this function no longer supplies went
// unnoticed for as long as it did.
//
// Note what is ABSENT: Options.HelixLLMModels. The HelixLLM section is sourced
// from the serving layer's live GET /v1/models and from nothing else. Supplying
// a fixed id here would put `helixllm/<id>` in front of users whether or not
// anything was serving it (BLUFF-002, CONST-036).
func newCatalogOptions(providerRegistry *services.ProviderRegistry, logger *logrus.Logger) catalog.Options {
	helixLLMEnabled := services.HelixLLMEnabledDefault() // local-first default: ON unless explicit false (HA-F2-002)

	opts := catalog.Options{
		Providers:       catalog.NewRegistryProviderSource(providerRegistry),
		EnsemblePresets: catalog.WiredEnsemblePresets(),
		HelixLLMEnabled: helixLLMEnabled,
		HelixLLM:        newHelixLLMCatalogSource(helixLLMEnabled, logger),
	}
	if providerRegistry != nil {
		opts.Verified = catalog.NewStartupVerifierSource(providerRegistry.GetStartupVerifier())
	}
	return opts
}

// HelixLLM catalog-source wiring.
//
// These two budgets are the whole of the failure policy for the live listing,
// and each answers a specific way an in-request network call can go wrong.

const (
	// helixLLMListTimeout caps ONE listing call. /v1/catalog is a JSON handler
	// a user waits on, and the serving layer is a local or LAN service, so a
	// listing that has not answered in this long is not going to answer usefully.
	// Exceeding it fails the fetch, which contributes NO options — never a
	// remembered or invented list.
	helixLLMListTimeout = 3 * time.Second

	// helixLLMListTTL caps how OLD any served listing may be. It is set inside
	// the CONST-038 60s accuracy window rather than picked for convenience: a
	// model that stops being served is dropped from the catalog within this
	// bound. Raising it past 60s would put the catalog outside that window.
	helixLLMListTTL = 30 * time.Second
)

// newHelixLLMCatalogSource wires the serving layer's live GET /v1/models into
// the catalog, and returns nil whenever it cannot.
//
// nil is a load-bearing return value, not an error swallow: catalog.Options
// treats a nil HelixLLMSource as "nothing reported", which emits no model
// entries. That is the correct answer in every case this function declines —
// the integration is off, or no adapter could be built — because in none of
// them has anything confirmed that a model is running.
//
// The failure policy, stated once:
//
//   - SLOW: each listing is bounded by helixLLMListTimeout, so an unreachable
//     HelixLLM costs a catalog request that long ONCE per TTL window, never the
//     adapter's full transport timeout and never per request.
//   - FAILED: a failed listing yields no options AND discards the cached one
//     (see catalog.CachedLister). A catalog answered from a listing that just
//     failed would assert models are running on no evidence.
//   - CACHED: successes are reused for at most helixLLMListTTL, so the answer
//     is cheap but never older than the CONST-038 window.
func newHelixLLMCatalogSource(enabled bool, logger *logrus.Logger) catalog.HelixLLMSource {
	if !enabled {
		return nil
	}

	adapter, err := helixllmadapter.NewAdapter(helixllmadapter.Config{
		Enabled: true,
		// Bound the transport itself as well as the context. Either alone
		// would do; both together mean no configuration path can leave a
		// catalog request waiting on the adapter's 30s default.
		Timeout: helixLLMListTimeout,
	})
	if err != nil {
		// Report it and contribute nothing. Falling back to a fixed id list
		// here is precisely the defect this wiring removes.
		if logger != nil {
			logger.WithError(err).Warn(
				"HelixLLM catalog source unavailable: adapter could not be built; " +
					"the catalog will list no HelixLLM models rather than assume any")
		}
		return nil
	}

	lister := func(ctx context.Context) (*helixllmadapter.ModelsResponse, error) {
		fetchCtx, cancel := context.WithTimeout(ctx, helixLLMListTimeout)
		defer cancel()
		return adapter.GetModels(fetchCtx)
	}

	return catalog.NewHelixLLMSource(
		context.Background(),
		catalog.CachedLister(lister, helixLLMListTTL),
	)
}

func initializeSearchService(cfg *config.Config, logger *logrus.Logger, containerAdapter *containeradapter.Adapter) (*search.Service, error) {
	chromadbPort, _ := strconv.Atoi(cfg.Services.ChromaDB.Port)
	if chromadbPort == 0 {
		chromadbPort = 8000
	}

	qdrantPort, _ := strconv.Atoi(cfg.Services.Qdrant.Port)
	if qdrantPort == 0 {
		qdrantPort = 6333
	}

	searchConfig := search.ServiceConfig{
		Enabled:          true,
		EmbedderType:     "local", // Use local embedder by default (deterministic, no API key needed)
		VectorStoreType:  "chroma",
		ChromaHost:       cfg.Services.ChromaDB.Host,
		ChromaPort:       chromadbPort,
		QdrantHost:       cfg.Services.Qdrant.Host,
		QdrantPort:       qdrantPort,
		CollectionName:   "code_embeddings",
		ContainerAdapter: containerAdapter,
		ComposeFile:      "docker-compose.yml",
		IndexerConfig: indexer.Config{
			RootPath:        ".",
			IncludePatterns: []string{"*.go", "*.py", "*.js", "*.ts", "*.rs", "*.java", "*.cpp", "*.c", "*.h"},
			ExcludePatterns: []string{"vendor/", "node_modules/", ".git/", "*.pb.go", "*_test.go", "dist/", "build/"},
			ChunkSize:       50,
			ChunkOverlap:    10,
			MaxFileSize:     1024 * 1024, // 1MB
			IndexOnStartup:  false,       // Don't index on startup to avoid slowdown
			WatchFiles:      false,       // Disable file watching by default
		},
	}

	// Override with environment variables if set
	if os.Getenv("SEARCH_ENABLED") == "false" {
		searchConfig.Enabled = false
	}
	if embedderType := os.Getenv("SEARCH_EMBEDDER_TYPE"); embedderType != "" {
		searchConfig.EmbedderType = embedderType
	}
	if vectorStore := os.Getenv("SEARCH_VECTOR_STORE"); vectorStore != "" {
		searchConfig.VectorStoreType = vectorStore
	}

	// Get OpenAI key if using OpenAI embedder
	if searchConfig.EmbedderType == "openai" {
		searchConfig.OpenAIKey = os.Getenv("OPENAI_API_KEY")
		if searchConfig.OpenAIKey == "" {
			logger.Warn("OpenAI embedder configured but OPENAI_API_KEY not set, falling back to local embedder")
			searchConfig.EmbedderType = "local"
		}
	}

	return search.NewService(searchConfig, logger)
}

// initializeFormattersSystem initializes the code formatters system with default configuration
func initializeFormattersSystem(logger *logrus.Logger) (*formatters.FormatterRegistry, *formatters.FormatterExecutor, *formatters.HealthChecker) {
	// Create configuration
	cfg := formatters.DefaultConfig()
	cfg.Enabled = true
	cfg.DefaultTimeout = 30 * time.Second
	cfg.CacheEnabled = true
	cfg.CacheTTL = 5 * time.Minute
	cfg.Metrics = true
	cfg.Tracing = false

	// Initialize the formatters system
	registry, executor, health, err := formatters.InitializeFormattersSystem(cfg, logger)
	if err != nil {
		logger.WithError(err).Error("Failed to initialize formatters system")
		return nil, nil, nil
	}

	// Register all available formatters
	if err := formattersproviders.RegisterAllFormatters(registry, logger); err != nil {
		logger.WithError(err).Warn("Some formatters failed to register")
	}

	logger.WithFields(logrus.Fields{
		"formatters_count": len(registry.List()),
		"cache_enabled":    cfg.CacheEnabled,
		"metrics_enabled":  cfg.Metrics,
	}).Info("Formatters system initialized successfully")

	return registry, executor, health
}

// ensembleUsage builds the `usage` envelope for the ensemble endpoint
// from the selected response's REAL provider-reported token counts.
//
// Extracted from the inline handler closure so it is directly
// unit-testable. That matters for more than tidiness: while the
// envelope was built inline, the only coverage of it was an
// `assert.Contains(response, "usage")` presence check, and an
// independent review demonstrated that reinstating the old fabricated
// `TokensUsed / 2` split at this site went completely undetected. A
// named function gets a real §1.1 guard (see TestEnsembleUsage).
//
// Never fabricates: models.LLMResponse.TokenSplit reports the
// provider's measured split, or honest zeros when the provider
// reported none, and its nil-receiver branch keeps a nil selection
// from panicking here.
func ensembleUsage(selected *models.LLMResponse) gin.H {
	promptTokens, completionTokens, totalTokens := selected.TokenSplit()
	return gin.H{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      totalTokens,
	}
}
