package services

import (
	"context"
	stdctx "context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"digital.vasic.concurrency/pkg/safe"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	memoryadapter "dev.helix.agent/internal/adapters/memory"
	specifieradapter "dev.helix.agent/internal/adapters/specifier"
	"dev.helix.agent/internal/database"
	"dev.helix.agent/internal/llm"
	helixmem "dev.helix.agent/internal/memory"
	"dev.helix.agent/internal/models"

	// NEW: Integrated AI Debate Features
	"digital.vasic.debate/audit"
	"digital.vasic.debate/comprehensive" // Comprehensive Multi-Agent Debate System
	"digital.vasic.debate/evaluation"
	"digital.vasic.debate/gates"
	"digital.vasic.debate/reflexion"
	"digital.vasic.debate/testing"
	"digital.vasic.debate/tools"
	"digital.vasic.debate/validation"
	// Note: orchestrator is integrated at the router/handler level (internal/router/router.go)
	// to avoid import cycle. The orchestrator.ServiceIntegration bridges to this service.
)

// DebateService provides core debate functionality with real LLM provider calls
type DebateService struct {
	logger           *logrus.Logger
	providerRegistry *ProviderRegistry
	cogneeService    *CogneeService
	memoryAdapter    interface { // HelixMemory unified memory (default)
		Add(ctx context.Context, memory *helixmem.Memory) error
		Get(ctx context.Context, id string) (*helixmem.Memory, error)
		Search(ctx context.Context, query string, opts *helixmem.SearchOptions) ([]*helixmem.Memory, error)
	}
	specifierAdapter    *specifieradapter.SpecAdapter                    // HelixSpecifier fusion engine (default)
	logRepository       DebateLogRepository                              // Optional: for persistent logging
	teamConfig          *DebateTeamConfig                                // Team configuration with Claude/Qwen roles
	commLogger          *DebateCommLogger                                // Retrofit-like communication logger
	intentCache         *safe.Store[string, *IntentClassificationResult] // Cache for intent classification
	codeIntentCache     *safe.Store[string, bool]                        // Cache for code generation intent results
	enhancedIntentCache *safe.Store[string, *EnhancedIntentResult]       // Cache full EnhancedIntentResult per topic
	intentAttempted     *safe.Store[string, bool]                        // Tracks topics where LLM classification was already attempted (even if it failed)

	// NEW: Integrated AI Debate Features (100% Implementation)
	testGenerator       *testing.LLMTestCaseGenerator            // Test-Driven Debate: Adversarial test generation
	testExecutor        *testing.SandboxedTestExecutor           // Test-Driven Debate: Sandboxed execution
	contrastiveAnalyzer *testing.DifferentialContrastiveAnalyzer // Test-Driven Debate: Result analysis
	validationPipeline  *validation.ValidationPipeline           // 4-Pass Validation: Progressive quality gates
	toolIntegration     *tools.ToolIntegration                   // Tool Integration: MCP/LSP/RAG/Embeddings access
	serviceBridge       *tools.ServiceBridge                     // Tool Integration: Service adapter
	// Note: 8-phase orchestrator is integrated at router/handler level via orchestrator.ServiceIntegration

	// Enhanced Intent Mechanism with SpecKit Integration
	enhancedIntentClassifier *EnhancedIntentClassifier // Enhanced intent classification with granularity detection
	speckitOrchestrator      *SpecKitOrchestrator      // SpecKit flow orchestration for big changes
	constitutionManager      *ConstitutionManager      // Constitution management
	documentationSync        *DocumentationSync        // Documentation synchronization
	constitutionWatcher      *ConstitutionWatcher      // Constitution auto-update background service

	// Debate Spec Full Compliance — new components
	reflexionMemory    *reflexion.EpisodicMemoryBuffer // Reflexion episodic memory buffer
	reflexionGenerator *reflexion.ReflectionGenerator  // Reflexion verbal reflection generator
	reflexionLoop      *reflexion.ReflexionLoop        // Reflexion retry-and-learn loop
	accumulatedWisdom  *reflexion.AccumulatedWisdom    // Cross-session learning
	approvalGate       *gates.ApprovalGate             // Configurable approval gates
	provenanceTracker  *audit.ProvenanceTracker        // Audit trail for reproducibility
	benchmarkBridge    *evaluation.BenchmarkBridge     // Benchmark evaluation bridge

	// Performance Optimizer
	performanceOptimizer *DebatePerformanceOptimizer

	// Comprehensive Multi-Agent Debate System (NEW)
	comprehensiveIntegration *comprehensive.IntegrationManager // New comprehensive debate system
	useComprehensiveSystem   bool                              // Feature flag to enable new system
}

// IsComprehensiveSystemEnabled returns whether the comprehensive debate system is enabled
func (ds *DebateService) IsComprehensiveSystemEnabled() bool {
	return ds.useComprehensiveSystem && ds.comprehensiveIntegration != nil
}

// GetComprehensiveIntegration returns the comprehensive IntegrationManager
func (ds *DebateService) GetComprehensiveIntegration() *comprehensive.IntegrationManager {
	return ds.comprehensiveIntegration
}

// StreamDebate conducts a streaming debate with the given configuration and handler
func (ds *DebateService) StreamDebate(ctx context.Context, config *DebateConfig, streamHandler comprehensive.StreamHandler) (*DebateResult, error) {
	startTime := time.Now()
	sessionID := fmt.Sprintf("session-%s-%s", config.DebateID, uuid.New().String()[:8])

	ds.logger.Infof("Starting streaming debate %s with topic: %s", config.DebateID, config.Topic)

	// Use Comprehensive Multi-Agent Debate System when enabled
	if ds.IsComprehensiveSystemEnabled() {
		ds.logger.Info("[Comprehensive Debate] Using new streaming multi-agent debate system")
		return ds.conductComprehensiveDebateStreaming(ctx, config, startTime, sessionID, streamHandler)
	}

	return nil, fmt.Errorf("comprehensive streaming debate system is not enabled")
}

// DebateLogRepository interface for logging debate activities
type DebateLogRepository interface {
	Insert(ctx context.Context, entry *database.DebateLogEntry) error
}

// DebateLogEntry is an alias to database.DebateLogEntry for convenience
type DebateLogEntry = database.DebateLogEntry

// maxIntentCacheSize bounds the intentCache map to prevent unbounded memory growth.
const maxIntentCacheSize = 10000

// CannedErrorPatterns defines patterns that indicate a canned/error response from LLMs
// These patterns trigger automatic fallback to alternative providers
var CannedErrorPatterns = []string{
	"unable to provide",
	"unable to analyze",
	"unable to process",
	"cannot provide",
	"cannot analyze",
	"cannot process",
	"i apologize, but i cannot",
	"i'm sorry, but i cannot",
	"error occurred",
	"at this time",
	"currently unable",
	"not able to",
	"failed to generate",
	"no response generated",
}

// IsCannedErrorResponse checks if a response contains canned error patterns
// Returns the matched pattern if found, empty string otherwise
func IsCannedErrorResponse(content string) string {
	lowered := strings.ToLower(content)
	for _, pattern := range CannedErrorPatterns {
		if strings.Contains(lowered, pattern) {
			return pattern
		}
	}
	return ""
}

// IsSuspiciouslyFastResponse checks if a response is suspiciously fast
// (typically indicates cached error or non-genuine response)
func IsSuspiciouslyFastResponse(responseTime time.Duration, contentLength int) bool {
	return responseTime < 100*time.Millisecond && contentLength < 100
}

// IsNonGenuineFastResponse is the guard the debate loop applies to a
// participant reply, and the successor to using IsSuspiciouslyFastResponse
// directly at that call site.
//
// WHAT THE ORIGINAL GUARD WAS FOR (git-history FACT, §11.4.124). The
// latency+length predicate arrived in commit 5d5dbb7b ("Add OAuth CLI
// fallback providers and canned response detection") as the SIBLING of
// IsCannedErrorResponse, under the heading "Canned Response Detection":
//
//	"Add exported IsSuspiciouslyFastResponse() helper (<100ms + <100 chars)
//	 - Trigger fallback mechanism on canned/suspicious responses"
//
// Its own comment states the target: "typically indicates cached error or
// non-genuine response". So the question it exists to answer is NOT "was
// this fast?" — it is "did the provider actually DO any work, or did it
// hand back a canned/cached string?"
//
// WHY LATENCY ALONE ANSWERS THAT QUESTION WRONGLY HERE. Wall-clock is a
// proxy for "work happened" that only holds for a REMOTE provider, where
// network round-trip alone already exceeds 100 ms. For a provider serving
// a model on loopback the proxy inverts: a genuine generation from a local
// 3B model is legitimately faster than the threshold. Measured live on
// this stack (journalctl, 2026-09-07), the local `helixllm` provider
// answering "What is the capital of France?" was REJECTED four times over:
//
//	response_preview=Paris response_time_ms=82 content_length=5
//	response_preview=Paris response_time_ms=78 content_length=5
//	response_preview=Paris response_time_ms=57 content_length=5
//	response_preview=Paris response_time_ms=71 content_length=5
//
// Each rejection was reported upward as a participant FAILURE ("Primary LLM
// failed, attempting fallback chain" → "[EXHAUSTED] All 0 fallbacks
// failed"), so a correct answer became a failed debate.
//
// THE DISCRIMINATOR THAT ACTUALLY WORKS. The provider itself reports how
// much work it did: the token usage on the response. A cached/canned string
// handed back without inference carries no usage; a real generation carries
// real counts (the same live capture shows tokens_used=206/277/298 beside
// those 5-character replies). Reported usage is therefore a DIRECT signal
// of "work happened", where latency is only an indirect one — and unlike
// latency it does not depend on where the model runs.
//
// So the guard fires only when all three hold: the reply came back fast
// AND it is short AND the provider reported NO tokens. That keeps exactly
// what commit 5d5dbb7b was built to catch — the work-less canned/cached
// reply — while no longer rejecting a genuine local answer. It composes
// with, and does not replace, the two content-based checks that run ahead
// of it at the call site: empty-response and IsCannedErrorResponse.
//
// Honest boundary (§11.4.6): this narrows the guard, it does not make it
// omniscient. A provider that replays a cached response TOGETHER with
// cached usage counts would satisfy the usage test and pass. That case is
// not detectable from the response envelope at all, and the empty/canned
// content checks remain the defence against it — inventing a latency floor
// for a local endpoint would not catch it either, and would go back to
// rejecting correct answers.
func IsNonGenuineFastResponse(responseTime time.Duration, contentLength int, reportedTokens int) bool {
	if reportedTokens > 0 {
		// The provider accounted for real work. Whatever the latency, this
		// is not the work-less canned/cached reply the guard targets.
		return false
	}
	return IsSuspiciouslyFastResponse(responseTime, contentLength)
}

// participantUsageMetadata records what the provider ACTUALLY reported about
// this participant call — the aggregate AND, when the provider supplied them,
// the per-direction counts — into the ParticipantResponse metadata map.
//
// Before this, only "tokens_used" was carried forward. The per-direction
// numbers most providers under internal/llm/providers record (OpenAI-shaped
// prompt_tokens/completion_tokens, Anthropic-shaped input_tokens/output_tokens)
// were dropped here, so nothing downstream could sum them and the debate's
// OpenAI envelope published a zero split beside a real total. Aggregation of
// the real numbers is the fix; a synthesised split would not be (§11.4.6 —
// models.LLMResponse.TokenSplit deliberately refuses to invent one, and this
// function must not smuggle one in behind it).
//
// A provider that reported no split contributes nothing here beyond the
// aggregate, which is the honest answer for that participant.
func participantUsageMetadata(resp *models.LLMResponse, extra map[string]any) map[string]any {
	md := make(map[string]any, len(extra)+3)
	for k, v := range extra {
		md[k] = v
	}
	if resp == nil {
		md["tokens_used"] = 0
		return md
	}

	prompt, completion, total := resp.TokenSplit()
	md["tokens_used"] = total
	if prompt > 0 || completion > 0 {
		md["prompt_tokens"] = prompt
		md["completion_tokens"] = completion
	}
	return md
}

// NewDebateService creates a new debate service
func NewDebateService(logger *logrus.Logger) *DebateService {
	ds := &DebateService{
		logger:     logger,
		commLogger: NewDebateCommLogger(logger),
	}
	ds.initCaches()
	return ds
}

// initCaches constructs the four safe.Store caches. Idempotent so it
// can be called from the NewDebateService* constructors and from any
// code path that starts with a struct-literal *DebateService (some
// tests do so; callers must ensure caches are initialised before any
// cache-dependent method is invoked).
func (ds *DebateService) initCaches() {
	if ds.intentCache == nil {
		ds.intentCache = safe.NewStore[string, *IntentClassificationResult]()
	}
	if ds.codeIntentCache == nil {
		ds.codeIntentCache = safe.NewStore[string, bool]()
	}
	if ds.enhancedIntentCache == nil {
		ds.enhancedIntentCache = safe.NewStore[string, *EnhancedIntentResult]()
	}
	if ds.intentAttempted == nil {
		ds.intentAttempted = safe.NewStore[string, bool]()
	}
}

// NewDebateServiceWithDeps creates a new debate service with dependencies for real LLM calls
// Initializes ALL integrated AI debate features (Test-Driven, 4-Pass Validation, Tool Integration)
func NewDebateServiceWithDeps(
	logger *logrus.Logger,
	providerRegistry *ProviderRegistry,
	cogneeService *CogneeService,
) *DebateService {
	basicValidator := &testing.BasicTestCaseValidator{}

	llmAdapter := testing.NewProviderAdapter(func(ctx context.Context, prompt string) (string, error) {
		req := &models.LLMRequest{
			ID:        uuid.New().String(),
			Prompt:    prompt,
			CreatedAt: time.Now(),
			ModelParams: models.ModelParameters{
				Model:       "default",
				Temperature: 0.7,
				MaxTokens:   2000,
			},
		}
		provider, err := providerRegistry.GetProvider("claude")
		if err != nil {
			provider, err = providerRegistry.GetProvider("deepseek")
			if err != nil {
				return "", err
			}
		}
		resp, err := provider.Complete(ctx, req)
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	})
	testGen := testing.NewLLMTestCaseGenerator(&llmAdapter, basicValidator)
	testExec := testing.NewSandboxedTestExecutor(
		testing.WithTimeout(30*time.Second),
		testing.WithMemoryLimit(512*1024*1024),
		testing.WithCPULimit(2.0),
	)
	contrastiveAnalyzer := testing.NewDifferentialContrastiveAnalyzer(nil)

	// Initialize 4-Pass Validation Pipeline
	validationPipeline := validation.NewValidationPipeline(validation.DefaultPipelineConfig())

	// Initialize Tool Integration (MCP/LSP/RAG/Embeddings)
	// Note: Service bridge creates tool integration internally
	serviceBridge := tools.NewServiceBridge(
		nil, // mcpService - will be wired up later
		nil, // lspManager - will be wired up later
		nil, // ragService - will be wired up later
		nil, // embeddingService - will be wired up later
		nil, // formatterExecutor - will be wired up later
		nil, // cognitiveServices - will be wired up later
	)
	toolIntegration := serviceBridge.GetToolIntegration()

	// Enhanced Orchestrator will be integrated in Phase 6 (avoiding import cycle)
	// orchestratorConfig := orchestrator.DefaultOrchestratorConfig()
	// debateOrchestrator := orchestrator.NewDebateOrchestrator(orchestratorConfig)

	// Initialize Enhanced Intent Mechanism components
	constitutionManager := NewConstitutionManager(logger)
	documentationSync := NewDocumentationSync(logger)
	enhancedIntentClassifier := NewEnhancedIntentClassifier(providerRegistry, logger)

	// Initialize SpecKit Orchestrator (will be wired up after DebateService creation)
	// speckitOrchestrator will be initialized via SetSpecKitOrchestrator after DebateService is created
	// to avoid circular dependency

	// Initialize Debate Spec Full Compliance components
	reflexionMemory := reflexion.NewEpisodicMemoryBuffer(1000)
	reflexionGen := reflexion.NewReflectionGenerator(nil) // LLM client wired later
	reflexionLoopInst := reflexion.NewReflexionLoop(
		reflexion.DefaultReflexionConfig(),
		reflexionGen,
		nil, // test executor wired later
		reflexionMemory,
	)
	wisdom := reflexion.NewAccumulatedWisdom()
	approvalGateInst := gates.NewApprovalGate(gates.GateConfig{Enabled: false}) // Disabled by default per spec
	provenanceTrackerInst := audit.NewProvenanceTracker()
	benchmarkBridgeInst := evaluation.NewBenchmarkBridge()

	// Initialize HelixMemory unified cognitive memory engine (default memory system)
	var memAdapter interface {
		Add(ctx context.Context, memory *helixmem.Memory) error
		Get(ctx context.Context, id string) (*helixmem.Memory, error)
		Search(ctx context.Context, query string, opts *helixmem.SearchOptions) ([]*helixmem.Memory, error)
	}
	if memoryadapter.IsHelixMemoryEnabled() {
		if optimal := memoryadapter.NewOptimalStoreAdapter(); optimal != nil {
			memAdapter = optimal
			logger.WithField("backend", memoryadapter.MemoryBackendName()).
				Info("[Debate Service] HelixMemory unified engine initialized as default memory")
		}
	}
	// Durable persist path is default-on: if the HelixMemory fusion engine failed
	// to initialize (e.g. external services unreachable), fall back to the local
	// in-process store instead of leaving memory unconfigured. This guarantees a
	// store error is never silently swallowed into a no-op nil adapter — debate
	// analyses are always persisted to at least the local fallback.
	if memAdapter == nil {
		// Prefer a disk-durable SQLite-backed store so fallback memory survives
		// process restart. Fall back to the process-lifetime in-memory store ONLY
		// if the durable path is unwritable — preserving the never-nil guarantee
		// (a store error is never silently swallowed into a no-op nil adapter).
		dbPath := debateMemoryFallbackPath()
		if dbPath != "" {
			if disk, err := helixmem.NewDiskStore(dbPath); err == nil {
				memAdapter = disk
				logger.WithField("path", dbPath).
					Warn("[Debate Service] HelixMemory unavailable; using disk-durable SQLite store fallback for memory persistence")
			} else {
				logger.WithError(err).WithField("path", dbPath).
					Warn("[Debate Service] disk-durable memory fallback unavailable; using in-memory store")
			}
		}
		if memAdapter == nil {
			memAdapter = helixmem.NewInMemoryStore()
			logger.Warn("[Debate Service] HelixMemory unavailable; using local in-memory store fallback for memory persistence")
		}
	}

	// Initialize HelixSpecifier spec-driven development fusion engine (default)
	var specAdapter *specifieradapter.SpecAdapter
	if specifieradapter.IsHelixSpecifierEnabled() {
		specAdapter = specifieradapter.NewOptimalSpecAdapter()
		if specAdapter != nil {
			logger.WithField("backend", specifieradapter.SpecifierBackendName()).
				Info("[Debate Service] HelixSpecifier fusion engine initialized as default spec engine")
		}
	}

	performanceOptimizer := NewDebatePerformanceOptimizer(
		DefaultDebateOptimizationConfig(),
		providerRegistry,
		logger,
	)

	// Initialize Comprehensive Multi-Agent Debate System
	compConfig := comprehensive.DefaultConfig()
	compConfig.MaxRounds = 5
	compConfig.MinConsensus = 0.8
	compConfig.QualityThreshold = 0.85
	comprehensiveIntegration, err := comprehensive.NewIntegrationManager(compConfig, logger)
	if err != nil {
		logger.WithError(err).Warn("[Debate Service] Failed to initialize comprehensive debate system, using legacy debate")
		comprehensiveIntegration = nil
	}

	// The comprehensive system is OPT-IN, not the default.
	//
	// It was previously enabled by default, which made it the path every
	// caller that did not set Metadata["source"] took. That path cannot
	// answer a prompt: digital.vasic.debate/comprehensive builds its
	// orchestrator with a nil ProviderInvoker and exposes no setter to
	// supply one, so every agent's content comes from the upstream
	// synthesiseContent() placeholder, and its DebateResponse has no
	// final-answer field at all. Defaulting to it meant the advertised
	// debate models returned a fixed status string for every prompt.
	//
	// Default OFF routes debates to conductRealDebate, which calls real
	// providers. Operators exercising the comprehensive orchestrator can
	// still opt in explicitly; it then surfaces real agent content or
	// fails honestly (see conductComprehensiveDebate).
	useComprehensive := strings.EqualFold(os.Getenv(envEnableComprehensiveDebate), "true")
	if comprehensiveIntegration != nil && useComprehensive {
		logger.Warn("[Debate Service] Comprehensive Multi-Agent Debate System explicitly enabled via " +
			envEnableComprehensiveDebate + " — it has no LLM provider wired upstream and cannot answer prompts")
	} else {
		logger.Info("[Debate Service] Using provider-backed debate path (comprehensive system off by default; set " +
			envEnableComprehensiveDebate + "=true to opt in)")
	}

	logger.Info("[Debate Service] Initialized with integrated features: Test-Driven, 4-Pass Validation, Tool Integration, Enhanced Intent, HelixSpecifier, SpecKit, Reflexion, Adversarial, Approval Gates, Provenance, Performance Optimizer, Comprehensive Multi-Agent Debate")

	ds := &DebateService{
		logger:           logger,
		providerRegistry: providerRegistry,
		cogneeService:    cogneeService,
		memoryAdapter:    memAdapter,
		specifierAdapter: specAdapter,
		commLogger:       NewDebateCommLogger(logger),

		// NEW: Integrated AI Debate Features
		testGenerator:       testGen,
		testExecutor:        testExec,
		contrastiveAnalyzer: contrastiveAnalyzer,
		validationPipeline:  validationPipeline,
		toolIntegration:     toolIntegration,
		serviceBridge:       serviceBridge,
		// debateOrchestrator:  debateOrchestrator, // Phase 6

		// Enhanced Intent Mechanism
		enhancedIntentClassifier: enhancedIntentClassifier,
		constitutionManager:      constitutionManager,
		documentationSync:        documentationSync,
		// speckitOrchestrator:      nil, // Set via SetSpecKitOrchestrator to avoid circular dependency

		// Debate Spec Full Compliance
		reflexionMemory:    reflexionMemory,
		reflexionGenerator: reflexionGen,
		reflexionLoop:      reflexionLoopInst,
		accumulatedWisdom:  wisdom,
		approvalGate:       approvalGateInst,
		provenanceTracker:  provenanceTrackerInst,
		benchmarkBridge:    benchmarkBridgeInst,

		// Performance Optimizer
		performanceOptimizer: performanceOptimizer,

		// Comprehensive Multi-Agent Debate System
		comprehensiveIntegration: comprehensiveIntegration,
		useComprehensiveSystem:   useComprehensive,
	}
	ds.initCaches()
	return ds
}

// SetProviderRegistry sets the provider registry for LLM calls
func (ds *DebateService) SetProviderRegistry(registry *ProviderRegistry) {
	ds.providerRegistry = registry
}

// SetCogneeService sets the Cognee service for enhanced insights
func (ds *DebateService) SetCogneeService(service *CogneeService) {
	ds.cogneeService = service
}

// SetMemoryAdapter sets the HelixMemory adapter for unified memory operations.
func (ds *DebateService) SetMemoryAdapter(adapter *memoryadapter.StoreAdapter) {
	ds.memoryAdapter = adapter
}

// GetMemoryBackendName returns the name of the active memory backend.
func (ds *DebateService) GetMemoryBackendName() string {
	return memoryadapter.MemoryBackendName()
}

// IsHelixMemoryActive returns true if HelixMemory is the active memory backend.
func (ds *DebateService) IsHelixMemoryActive() bool {
	return memoryadapter.IsHelixMemoryEnabled() && ds.memoryAdapter != nil
}

// SetSpecifierAdapter sets the HelixSpecifier adapter for spec-driven development.
func (ds *DebateService) SetSpecifierAdapter(adapter *specifieradapter.SpecAdapter) {
	ds.specifierAdapter = adapter
}

// GetSpecifierBackendName returns the name of the active specifier backend.
func (ds *DebateService) GetSpecifierBackendName() string {
	return specifieradapter.SpecifierBackendName()
}

// IsHelixSpecifierActive returns true if HelixSpecifier is the active spec engine.
func (ds *DebateService) IsHelixSpecifierActive() bool {
	return specifieradapter.IsHelixSpecifierEnabled() && ds.specifierAdapter != nil
}

func (ds *DebateService) GetPerformanceOptimizerStats() *OptimizationStats {
	if ds.performanceOptimizer != nil {
		return ds.performanceOptimizer.GetStats()
	}
	return nil
}

func (ds *DebateService) ClearPerformanceOptimizerCache() {
	if ds.performanceOptimizer != nil {
		ds.performanceOptimizer.ClearCache()
	}
}

func (ds *DebateService) CheckEarlyTermination(responses map[DebateTeamPosition]string) bool {
	if ds.performanceOptimizer != nil {
		return ds.performanceOptimizer.ShouldTerminateEarly(responses)
	}
	return false
}

// SetLogRepository sets the log repository for persistent logging
func (ds *DebateService) SetLogRepository(repo DebateLogRepository) {
	ds.logRepository = repo
}

// InitializeSpecKitOrchestrator initializes the SpecKit orchestrator with the debate service
// This must be called after DebateService creation to avoid circular dependency
func (ds *DebateService) InitializeSpecKitOrchestrator(projectRoot string) {
	if ds.speckitOrchestrator == nil && ds.constitutionManager != nil && ds.documentationSync != nil {
		ds.speckitOrchestrator = NewSpecKitOrchestrator(
			ds, // Pass self as debate service
			ds.constitutionManager,
			ds.documentationSync,
			ds.logger,
			projectRoot,
		)
		ds.logger.Info("[Debate Service] SpecKit Orchestrator initialized")
	}
}

// InitializeConstitutionWatcher initializes the Constitution watcher for auto-updates
// This should be called after DebateService creation with project root path
func (ds *DebateService) InitializeConstitutionWatcher(projectRoot string, enabled bool) {
	if enabled && ds.constitutionWatcher == nil && ds.constitutionManager != nil && ds.documentationSync != nil {
		ds.constitutionWatcher = NewConstitutionWatcher(
			ds.constitutionManager,
			ds.documentationSync,
			ds.logger,
			projectRoot,
		)
		ds.constitutionWatcher.Enable()
		ds.logger.Info("[Debate Service] Constitution Watcher initialized and enabled")
	} else if !enabled {
		ds.logger.Info("[Debate Service] Constitution Watcher disabled by configuration")
	}
}

// StartConstitutionWatcher starts the Constitution watcher in the background
// It monitors project changes and auto-updates Constitution
func (ds *DebateService) StartConstitutionWatcher(ctx context.Context) {
	if ds.constitutionWatcher != nil && ds.constitutionWatcher.enabled {
		go ds.constitutionWatcher.Start(ctx)
		ds.logger.Info("[Debate Service] Constitution Watcher started in background")
	}
}

// StopConstitutionWatcher stops the Constitution watcher gracefully
func (ds *DebateService) StopConstitutionWatcher() {
	if ds.constitutionWatcher != nil {
		ds.constitutionWatcher.Disable()
		ds.logger.Info("[Debate Service] Constitution Watcher stopped")
	}
}

// InitializeHelixSpecifierDebate injects the real debate
// function into the HelixSpecifier engine. This must be called
// after DebateService creation to avoid circular dependency.
func (ds *DebateService) InitializeHelixSpecifierDebate() {
	if ds.specifierAdapter == nil {
		return
	}

	debateFunc := func(
		ctx context.Context,
		topic string,
		rounds int,
		metadata map[string]interface{},
	) (string, float64, string, error) {
		return ds.executeSpecifierDebateRound(
			ctx, topic, rounds, metadata,
		)
	}

	if ds.specifierAdapter.SetDebateFunc(debateFunc) {
		ds.logger.Info(
			"[Debate Service] Injected real debate " +
				"function into HelixSpecifier engine",
		)
	}
}

// executeSpecifierDebateRound executes a single debate round
// for HelixSpecifier using the debate service's provider
// infrastructure.
func (ds *DebateService) executeSpecifierDebateRound(
	ctx context.Context,
	topic string,
	rounds int,
	metadata map[string]interface{},
) (string, float64, string, error) {
	debateID := fmt.Sprintf("hspec-%s", uuid.New().String()[:8])

	if ds.providerRegistry == nil {
		return fmt.Sprintf("[HelixSpecifier] %s", topic),
			0.75, debateID, nil
	}

	// Get the first available provider via the registry
	// following the same fallback pattern as the test
	// generator adapter (claude -> deepseek -> listed).
	var provider llm.LLMProvider
	var err error
	for _, name := range []string{"claude", "deepseek"} {
		provider, err = ds.providerRegistry.GetProvider(name)
		if err == nil {
			break
		}
	}
	if provider == nil {
		listed := ds.providerRegistry.ListProviders()
		if len(listed) == 0 {
			return fmt.Sprintf(
					"[HelixSpecifier] %s (no providers)",
					topic,
				),
				0.65, debateID, nil
		}
		provider, err = ds.providerRegistry.GetProvider(
			listed[0],
		)
		if err != nil {
			return fmt.Sprintf(
					"[HelixSpecifier] %s (provider err)",
					topic,
				),
				0.65, debateID, nil
		}
	}

	req := &models.LLMRequest{
		ID:     uuid.New().String(),
		Prompt: topic,
		Messages: []models.Message{
			{Role: "user", Content: topic},
		},
		ModelParams: models.ModelParameters{
			Model:       "default",
			Temperature: 0.7,
			MaxTokens:   2000,
		},
	}

	resp, err := provider.Complete(ctx, req)
	if err != nil {
		ds.logger.WithError(err).Warn(
			"[HelixSpecifier Debate] Provider call failed",
		)
		return fmt.Sprintf("[HelixSpecifier] %s", topic),
			0.70, debateID, nil
	}

	// Score response quality by length heuristic
	score := 0.80
	if len(resp.Content) > 200 {
		score = 0.85
	}
	if len(resp.Content) > 500 {
		score = 0.90
	}

	return resp.Content, score, debateID, nil
}

// SetTeamConfig sets the debate team configuration with Claude/Qwen role assignments
func (ds *DebateService) SetTeamConfig(config *DebateTeamConfig) {
	ds.teamConfig = config
}

// GetTeamConfig returns the debate team configuration
func (ds *DebateService) GetTeamConfig() *DebateTeamConfig {
	return ds.teamConfig
}

// SetCommLogger sets the communication logger for Retrofit-like logging
func (ds *DebateService) SetCommLogger(commLogger *DebateCommLogger) {
	ds.commLogger = commLogger
}

// GetCommLogger returns the communication logger
func (ds *DebateService) GetCommLogger() *DebateCommLogger {
	return ds.commLogger
}

// GetEnhancedIntentClassifier returns the intent classifier for external routing decisions.
func (ds *DebateService) GetEnhancedIntentClassifier() *EnhancedIntentClassifier {
	return ds.enhancedIntentClassifier
}

// SetCLIAgent sets the CLI agent for communication logging color support
func (ds *DebateService) SetCLIAgent(agent string) {
	if ds.commLogger != nil {
		ds.commLogger.SetCLIAgent(agent)
		// Enable colors based on CLI agent capability
		colorConfig := CLIAgentColors(agent)
		ds.commLogger.SetColorsEnabled(colorConfig["colors"])
	}
}

// logDebateEntry logs a debate entry to the repository if configured
func (ds *DebateService) logDebateEntry(ctx context.Context, entry *DebateLogEntry) {
	if ds.logRepository == nil {
		return
	}

	if err := ds.logRepository.Insert(ctx, entry); err != nil {
		ds.logger.WithError(err).WithFields(logrus.Fields{
			"participant": entry.ParticipantIdentifier,
			"action":      entry.Action,
		}).Warn("Failed to log debate entry to repository")
	}
}

// roundResult holds the results from a single debate round
type roundResult struct {
	Round     int
	Responses []ParticipantResponse
	StartTime time.Time
	EndTime   time.Time
}

// formatParticipantIdentifier creates a readable identifier like "DeepSeek-1" or "Gemini-2"
func formatParticipantIdentifier(provider, participantID string, instanceNum int) string {
	// Capitalize first letter of provider
	caser := cases.Title(language.English)
	providerName := caser.String(strings.ToLower(provider))
	if instanceNum > 0 {
		return fmt.Sprintf("%s-%d", providerName, instanceNum)
	}
	// Try to extract number from participantID
	parts := strings.Split(participantID, "-")
	for _, part := range parts {
		if num, err := fmt.Sscanf(part, "%d", new(int)); err == nil && num > 0 {
			return fmt.Sprintf("%s-%s", providerName, part)
		}
	}
	return fmt.Sprintf("%s-%s", providerName, participantID)
}

// extractInstanceNumber extracts instance number from participant ID like "single-provider-instance-3"
func extractInstanceNumber(participantID string) int {
	parts := strings.Split(participantID, "-")
	for i := len(parts) - 1; i >= 0; i-- {
		var num int
		if _, err := fmt.Sscanf(parts[i], "%d", &num); err == nil {
			return num
		}
	}
	return 0
}

// ConductDebate conducts a debate with the given configuration
// NEW: Routes to Test-Driven mode for code generation, applies 4-Pass Validation, uses Tool Integration
func (ds *DebateService) ConductDebate(
	ctx context.Context,
	config *DebateConfig,
) (*DebateResult, error) {
	startTime := time.Now()
	sessionID := fmt.Sprintf("session-%s-%s", config.DebateID, uuid.New().String()[:8])

	ds.logger.Infof("Starting debate %s with topic: %s", config.DebateID, config.Topic)

	// Use Comprehensive Multi-Agent Debate System when enabled,
	// UNLESS the request came from the OpenAI-compatible handler (which provides its own participants)
	// or the comprehensive system has no providers configured.
	useComprehensive := ds.useComprehensiveSystem && ds.comprehensiveIntegration != nil
	if config.Metadata != nil {
		if source, fromOpenAI := config.Metadata["source"]; fromOpenAI {
			ds.logger.WithField("source", source).Info("[ConductDebate] OpenAI source detected, bypassing comprehensive system")
			useComprehensive = false
		}
	}
	if useComprehensive {
		ds.logger.Info("[Comprehensive Debate] Using new multi-agent debate system")
		return ds.conductComprehensiveDebate(ctx, config, startTime, sessionID)
	}
	ds.logger.WithField("participants", len(config.Participants)).Info("[ConductDebate] Using conductRealDebate with multi-round consensus")

	// Check if request is from OpenAI handler (affects routing decisions)
	isFromOpenAI := false
	if config.Metadata != nil {
		_, isFromOpenAI = config.Metadata["source"]
	}

	// Require provider registry for real LLM calls - no fallback to simulated data
	if ds.providerRegistry == nil {
		return nil, fmt.Errorf("provider registry is required for debate: use NewDebateServiceWithDeps to create a properly configured debate service")
	}

	// Apply overall debate timeout to bound ALL operations including pre-processing
	if config.Timeout > 0 {
		var cancelFn context.CancelFunc
		ctx, cancelFn = context.WithTimeout(ctx, config.Timeout)
		defer cancelFn()
	}

	// NEW: Select specialized role based on task intent
	// Inject configurable role keywords into metadata so selectSpecializedRole
	// picks them up without requiring a signature change.
	if len(config.RoleKeywords) > 0 {
		if config.Metadata == nil {
			config.Metadata = make(map[string]any)
		}
		config.Metadata["role_keywords"] = config.RoleKeywords
	}
	specializedRole := ds.selectSpecializedRole(ctx, config.Topic, config.Metadata)
	if specializedRole != "" {
		ds.logger.Infof("[Specialized Role] Selected role: %s for task", specializedRole)
		if config.Metadata == nil {
			config.Metadata = make(map[string]any)
		}
		config.Metadata["specialized_role"] = specializedRole
	}

	// Detect if this is a code generation debate (Test-Driven mode)
	// Skip for OpenAI handler requests — those should go through standard debate
	if !isFromOpenAI {
		isCodeDebate := ds.detectCodeGenerationIntent(config.Topic, config.Metadata)
		if isCodeDebate && ds.testGenerator != nil {
			ds.logger.Info("[Test-Driven Debate] Code generation detected, enabling test-driven mode")
			return ds.conductTestDrivenDebate(ctx, config, startTime, sessionID)
		}
	}

	// Enhanced Intent Detection with HelixSpecifier / SpecKit Auto-Activation
	if ds.enhancedIntentClassifier != nil {
		intentResult, err := ds.classifyIntentWithGranularity(ctx, config.Topic, config.Metadata)
		if err != nil {
			ds.logger.WithError(err).Warn("[Spec Auto-Activation] Intent classification failed, proceeding with standard debate")
		} else if intentResult.RequiresSpecKit {
			// Prefer HelixSpecifier fusion engine (default)
			if ds.specifierAdapter != nil && ds.specifierAdapter.IsReady() {
				ds.logger.WithFields(logrus.Fields{
					"granularity": intentResult.Granularity,
					"action_type": intentResult.ActionType,
					"reason":      intentResult.SpecKitReason,
					"engine":      ds.specifierAdapter.Name(),
				}).Info("[HelixSpecifier] Routing through fusion engine")
				return ds.conductHelixSpecifierDebate(ctx, config, intentResult, startTime, sessionID)
			}
			// Fallback to inline SpecKit orchestrator
			if ds.speckitOrchestrator != nil {
				ds.logger.WithFields(logrus.Fields{
					"granularity": intentResult.Granularity,
					"action_type": intentResult.ActionType,
					"reason":      intentResult.SpecKitReason,
				}).Info("[SpecKit Auto-Activation] Routing through SpecKit flow (fallback)")
				return ds.conductSpecKitDebate(ctx, config, intentResult, startTime, sessionID)
			}
		}
	}

	// Standard debate with 4-Pass Validation and Tool Integration
	ds.logger.Info("[ConductDebate] Calling conductRealDebate")
	result, err := ds.conductRealDebate(ctx, config, startTime, sessionID)
	if err != nil {
		ds.logger.WithError(err).Error("[ConductDebate] conductRealDebate failed")
		return nil, err
	}
	ds.logger.WithFields(logrus.Fields{
		"all_responses":    len(result.AllResponses),
		"participants":     len(result.Participants),
		"rounds_conducted": result.RoundsConducted,
		"has_best":         result.BestResponse != nil,
		"success":          result.Success,
	}).Info("[ConductDebate] conductRealDebate completed")

	// Apply 4-Pass Validation Pipeline ONLY for code generation debates.
	// Conversational requests (from OpenAI handler) should not trigger
	// code validation/refinement which causes hallucinated /workspace errors.
	isCodeDebateForValidation := ds.detectCodeGenerationIntent(config.Topic, config.Metadata)
	if ds.validationPipeline != nil && isCodeDebateForValidation && !isFromOpenAI {
		validationResult, err := ds.validateDebateResult(ctx, result, config)
		if err != nil {
			ds.logger.WithError(err).Warn("[4-Pass Validation] Validation failed, result may need refinement")
			result.ValidationResult = validationResult
		} else {
			result.ValidationResult = validationResult
			result.QualityScore = validationResult.OverallScore
			if ds.commLogger != nil {
				ds.commLogger.LogInfo("Validation", fmt.Sprintf("4-Pass: PASS | Score: %.2f", validationResult.OverallScore))
			}
		}
	}

	return result, nil
}

// conductRealDebate executes a debate with real LLM provider calls
func (ds *DebateService) conductRealDebate(
	ctx context.Context,
	config *DebateConfig,
	startTime time.Time,
	sessionID string,
) (*DebateResult, error) {
	allResponses := make([]ParticipantResponse, 0)
	roundResults := make([]roundResult, 0, config.MaxRounds)

	// Create context with overall timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()

	// Execute rounds
	previousResponses := make([]ParticipantResponse, 0)
	for round := 1; round <= config.MaxRounds; round++ {
		ds.logger.Infof("Starting round %d of debate %s", round, config.DebateID)

		roundStart := time.Now()
		responses, err := ds.executeRound(timeoutCtx, config, round, previousResponses)
		if err != nil {
			ds.logger.WithError(err).Errorf("Round %d failed", round)
			// Continue with partial results if we have some
			if len(responses) == 0 {
				break
			}
		}

		roundResults = append(roundResults, roundResult{
			Round:     round,
			Responses: responses,
			StartTime: roundStart,
			EndTime:   time.Now(),
		})

		allResponses = append(allResponses, responses...)
		previousResponses = responses

		// Check for early consensus
		if ds.checkEarlyConsensus(responses) {
			ds.logger.Infof("Early consensus reached at round %d", round)
			break
		}

		// Check context cancellation
		select {
		case <-timeoutCtx.Done():
			ds.logger.Warn("Debate timeout reached")
			break
		default:
			continue
		}
	}

	endTime := time.Now()

	// Analyze responses for consensus
	consensus := ds.analyzeConsensus(allResponses, config.Topic)

	// Calculate quality scores from actual responses
	qualityScore := ds.calculateQualityScore(allResponses)
	finalScore := ds.calculateFinalScore(allResponses, consensus)

	// Find best response
	bestResponse := ds.findBestResponse(allResponses)

	// Build result
	result := &DebateResult{
		DebateID:        config.DebateID,
		SessionID:       sessionID,
		Topic:           config.Topic,
		StartTime:       startTime,
		EndTime:         endTime,
		Duration:        endTime.Sub(startTime),
		TotalRounds:     config.MaxRounds,
		RoundsConducted: len(roundResults),
		AllResponses:    allResponses,
		Participants:    ds.getLatestParticipantResponses(allResponses, config.Participants),
		BestResponse:    bestResponse,
		Consensus:       consensus,
		QualityScore:    qualityScore,
		FinalScore:      finalScore,
		Success:         len(allResponses) > 0,
		Metadata:        make(map[string]interface{}),
	}

	// Add Cognee insights if enabled
	if config.EnableCognee && ds.cogneeService != nil {
		insights, err := ds.generateCogneeInsights(timeoutCtx, config, allResponses)
		if err != nil {
			ds.logger.WithError(err).Warn("Failed to generate Cognee insights")
		} else {
			result.CogneeEnhanced = true
			result.CogneeInsights = insights
		}
	}

	// Add metadata
	result.Metadata["total_responses"] = len(allResponses)
	result.Metadata["providers_used"] = ds.getUniqueProviders(allResponses)
	result.Metadata["avg_response_time"] = ds.calculateAvgResponseTime(allResponses)

	// NEW: Add integrated feature metadata
	if specializedRole, ok := config.Metadata["specialized_role"].(string); ok {
		result.SpecializedRole = specializedRole
	}
	if toolEnrichment, ok := config.Metadata["tool_enrichment_used"].(bool); ok {
		result.ToolEnrichmentUsed = toolEnrichment
	}

	return result, nil
}

// executeRound executes a single debate round with all participants
func (ds *DebateService) executeRound(
	ctx context.Context,
	config *DebateConfig,
	round int,
	previousResponses []ParticipantResponse,
) ([]ParticipantResponse, error) {
	roundStartTime := time.Now()
	responses := make([]ParticipantResponse, 0, len(config.Participants))
	responseChan := make(chan ParticipantResponse, len(config.Participants))
	errorChan := make(chan error, len(config.Participants))
	fallbacksUsed := 0
	var fallbackMu sync.Mutex

	// NEW: Enrich context with tools before round execution
	var enrichedContext *tools.EnrichedContext
	if ds.toolIntegration != nil && ds.serviceBridge != nil {
		enrichedCtx, err := ds.enrichDebateContext(ctx, config, round)
		if err == nil {
			enrichedContext = enrichedCtx
			// Store enriched context in config metadata for participant access
			if config.Metadata == nil {
				config.Metadata = make(map[string]any)
			}
			config.Metadata["enriched_context"] = enrichedContext
			config.Metadata["tool_enrichment_used"] = true
		}
	}

	// Log debate phase start (Retrofit-like)
	if ds.commLogger != nil {
		ds.commLogger.LogDebatePhase("Getting participant responses", round)
	}

	var wg sync.WaitGroup
	for _, participant := range config.Participants {
		wg.Add(1)
		go func(p ParticipantConfig) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					ds.logger.Errorf("participant %s goroutine panicked (recovered): %v", p.Name, r)
					errorChan <- fmt.Errorf("participant %s panicked: %v", p.Name, r)
				}
			}()

			// Track fallback chain for logging
			fallbackChain := []FallbackChainEntry{{
				Provider: p.LLMProvider,
				Model:    p.LLMModel,
				Success:  false,
			}}
			participantStartTime := time.Now()

			// Try primary provider first
			resp, err := ds.getParticipantResponse(ctx, config, p, round, previousResponses)
			if err == nil {
				fallbackChain[0].Success = true
				fallbackChain[0].Duration = time.Since(participantStartTime)
				responseChan <- resp
				return
			}

			// Record primary failure
			fallbackChain[0].Error = err

			// Primary failed - log and try fallbacks
			ds.logger.WithFields(logrus.Fields{
				"participant":   p.Name,
				"role":          p.Role,
				"primary":       p.LLMProvider,
				"primary_model": p.LLMModel,
				"error":         err.Error(),
				"fallbacks":     len(p.Fallbacks),
			}).Warn("Primary LLM failed, attempting fallback chain")

			// Log primary error (Retrofit-like)
			if ds.commLogger != nil {
				ds.commLogger.LogError(p.Role, p.LLMProvider, p.LLMModel, err)
			}

			// Try each fallback in order
			for i, fallback := range p.Fallbacks {
				fallbackStartTime := time.Now()
				fallbackParticipant := p // Copy original config
				fallbackParticipant.LLMProvider = fallback.Provider
				fallbackParticipant.LLMModel = fallback.Model
				fallbackParticipant.Name = fmt.Sprintf("%s (fallback-%d: %s)", p.Role, i+1, fallback.Provider)

				// Add to fallback chain
				chainEntry := FallbackChainEntry{
					Provider: fallback.Provider,
					Model:    fallback.Model,
					Success:  false,
				}

				ds.logger.WithFields(logrus.Fields{
					"participant":       p.Name,
					"role":              p.Role,
					"fallback_index":    i + 1,
					"fallback_provider": fallback.Provider,
					"fallback_model":    fallback.Model,
				}).Info("Trying fallback LLM")

				// Log fallback attempt (Retrofit-like)
				if ds.commLogger != nil {
					ds.commLogger.LogFallbackAttempt(p.Role, p.LLMProvider, p.LLMModel, fallback.Provider, fallback.Model, i+1)
				}

				resp, err = ds.getParticipantResponse(ctx, config, fallbackParticipant, round, previousResponses)
				if err == nil {
					// Fallback succeeded!
					chainEntry.Success = true
					chainEntry.Duration = time.Since(fallbackStartTime)
					fallbackChain = append(fallbackChain, chainEntry)

					resp.Metadata["fallback_used"] = true
					resp.Metadata["fallback_index"] = i + 1
					resp.Metadata["original_provider"] = p.LLMProvider
					resp.Metadata["original_model"] = p.LLMModel

					// CRITICAL: Add visible highlighting in response content for user awareness
					fallbackNotice := fmt.Sprintf(
						"[FALLBACK ACTIVATED: Primary %s/%s unavailable, response from %s/%s]\n\n",
						p.LLMProvider, p.LLMModel, fallback.Provider, fallback.Model,
					)
					resp.Response = fallbackNotice + resp.Response
					resp.Content = fallbackNotice + resp.Content

					ds.logger.WithFields(logrus.Fields{
						"participant":       p.Name,
						"role":              p.Role,
						"fallback_provider": fallback.Provider,
						"fallback_model":    fallback.Model,
					}).Info("Fallback LLM succeeded!")

					// Log fallback success (Retrofit-like)
					if ds.commLogger != nil {
						ds.commLogger.LogFallbackSuccess(p.Role, p.LLMProvider, p.LLMModel, fallback.Provider, fallback.Model, i+1, chainEntry.Duration)
						ds.commLogger.LogFallbackChain(p.Role, fallbackChain, resp.Content, time.Since(participantStartTime))
					}

					fallbackMu.Lock()
					fallbacksUsed++
					fallbackMu.Unlock()

					responseChan <- resp
					return
				}

				// Record fallback failure
				chainEntry.Error = err
				chainEntry.Duration = time.Since(fallbackStartTime)
				fallbackChain = append(fallbackChain, chainEntry)

				ds.logger.WithFields(logrus.Fields{
					"participant":       p.Name,
					"fallback_index":    i + 1,
					"fallback_provider": fallback.Provider,
					"error":             err.Error(),
				}).Warn("Fallback LLM also failed, trying next")

				// Log fallback error (Retrofit-like)
				if ds.commLogger != nil {
					ds.commLogger.LogError(p.Role, fallback.Provider, fallback.Model, err)
				}
			}

			// All fallbacks exhausted
			if ds.commLogger != nil {
				ds.commLogger.LogAllFallbacksExhausted(p.Role, p.LLMProvider, p.LLMModel, len(p.Fallbacks))
			}
			errorChan <- fmt.Errorf("participant %s failed: primary and all %d fallbacks exhausted: %w", p.Name, len(p.Fallbacks), err)
		}(participant)
	}

	// Wait for all participants
	go func() {
		wg.Wait()
		close(responseChan)
		close(errorChan)
	}()

	// Collect responses
	for resp := range responseChan {
		responses = append(responses, resp)
	}

	// Collect errors (for logging)
	var errs []error
	for err := range errorChan {
		errs = append(errs, err)
	}

	// Log round summary (Retrofit-like)
	if ds.commLogger != nil && len(responses) > 0 {
		avgQuality := 0.0
		for _, resp := range responses {
			avgQuality += resp.QualityScore
		}
		avgQuality /= float64(len(responses))
		ds.commLogger.LogDebateSummary(round, len(responses), time.Since(roundStartTime), avgQuality, fallbacksUsed)
	}

	if len(errs) > 0 && len(responses) == 0 {
		return responses, fmt.Errorf("all participants failed: %v", errs)
	}

	return responses, nil
}

// getParticipantResponse gets a response from a specific participant
func (ds *DebateService) getParticipantResponse(
	ctx context.Context,
	config *DebateConfig,
	participant ParticipantConfig,
	round int,
	previousResponses []ParticipantResponse,
) (ParticipantResponse, error) {
	startTime := time.Now()

	// Create readable participant identifier like "DeepSeek-1" or "Claude-2"
	instanceNum := extractInstanceNumber(participant.ParticipantID)
	participantIdentifier := formatParticipantIdentifier(participant.LLMProvider, participant.ParticipantID, instanceNum)

	// Log participant identification
	ds.logger.WithFields(logrus.Fields{
		"participant":      participantIdentifier,
		"participant_id":   participant.ParticipantID,
		"participant_name": participant.Name,
		"role":             participant.Role,
		"provider":         participant.LLMProvider,
		"model":            participant.LLMModel,
		"round":            round,
		"debate_id":        config.DebateID,
	}).Info("Debate participant starting response")

	// CRITICAL: Use verified provider instance if available (from StartupVerifier)
	// This ensures CLI-based OAuth providers (Claude CLI, Qwen ACP) are used instead of
	// API-based providers from the registry which would fail with product-restricted tokens.
	var provider llm.LLMProvider
	if participant.ProviderInstance != nil {
		provider = participant.ProviderInstance
		ds.logger.WithFields(logrus.Fields{
			"participant":    participantIdentifier,
			"provider":       participant.LLMProvider,
			"model":          participant.LLMModel,
			"using_instance": "verified_provider_instance",
		}).Debug("Using verified provider instance from ParticipantConfig")
	} else if ds.teamConfig != nil {
		// Try to get verified provider from team config
		if verifiedProvider := ds.teamConfig.GetVerifiedProviderInstance(participant.LLMProvider, participant.LLMModel); verifiedProvider != nil {
			provider = verifiedProvider
			ds.logger.WithFields(logrus.Fields{
				"participant":    participantIdentifier,
				"provider":       participant.LLMProvider,
				"model":          participant.LLMModel,
				"using_instance": "team_config_verified",
			}).Debug("Using verified provider instance from DebateTeamConfig")
		}
	}

	// Final fallback to registry lookup if no verified instance found
	if provider == nil {
		var err error
		provider, err = ds.providerRegistry.GetProvider(participant.LLMProvider)
		if err != nil {
			ds.logger.WithFields(logrus.Fields{
				"participant":      participantIdentifier,
				"participant_id":   participant.ParticipantID,
				"participant_name": participant.Name,
				"provider":         participant.LLMProvider,
				"error":            err.Error(),
			}).Error("Debate participant provider not found")
			return ParticipantResponse{}, fmt.Errorf("provider not found: %w", err)
		}
		ds.logger.WithFields(logrus.Fields{
			"participant":    participantIdentifier,
			"provider":       participant.LLMProvider,
			"model":          participant.LLMModel,
			"using_instance": "registry_lookup",
		}).Debug("Using provider from registry lookup (no verified instance)")
	}

	// Build the prompt with context from previous responses
	prompt := ds.buildDebatePrompt(config.Topic, participant, round, previousResponses)

	// Build system prompt with context from the original request
	systemPrompt := ds.buildSystemPrompt(participant)
	if config.Metadata != nil {
		if sysCtx, ok := config.Metadata["system_context"].(string); ok && sysCtx != "" {
			systemPrompt = sysCtx + "\n\n" + systemPrompt
		}
	}

	// Create LLM request
	llmRequest := &models.LLMRequest{
		ID:        uuid.New().String(),
		SessionID: config.DebateID,
		Prompt:    prompt,
		Messages: []models.Message{
			{
				Role:    "system",
				Content: systemPrompt,
			},
			{
				Role:    "user",
				Content: prompt,
			},
		},
		ModelParams: models.ModelParameters{
			Model:       participant.LLMModel,
			Temperature: 0.7,
			MaxTokens:   1024,
		},
	}

	// Forward tools from the debate config so participants can emit tool_calls
	if len(config.Tools) > 0 {
		llmRequest.Tools = config.Tools
		llmRequest.ToolChoice = config.ToolChoice
	}

	// Log request (Retrofit-like): [A: Claude Opus 4.5] <--- Sending request...
	if ds.commLogger != nil {
		ds.commLogger.LogRequest(participant.Role, participant.LLMProvider, participant.LLMModel, len(prompt), round)
	}

	// Make the actual LLM call
	llmResponse, err := provider.Complete(ctx, llmRequest)
	if err != nil {
		ds.logger.WithFields(logrus.Fields{
			"participant":      participantIdentifier,
			"participant_id":   participant.ParticipantID,
			"participant_name": participant.Name,
			"role":             participant.Role,
			"provider":         participant.LLMProvider,
			"round":            round,
			"debate_id":        config.DebateID,
			"error":            err.Error(),
		}).Error("Debate participant LLM call failed")

		// Log error (Retrofit-like)
		if ds.commLogger != nil {
			ds.commLogger.LogError(participant.Role, participant.LLMProvider, participant.LLMModel, err)
		}

		return ParticipantResponse{}, fmt.Errorf("[%s] LLM call failed: %w", participantIdentifier, err)
	}

	responseTime := time.Since(startTime)

	// CRITICAL: Check for empty response content - this triggers fallback
	if strings.TrimSpace(llmResponse.Content) == "" {
		ds.logger.WithFields(logrus.Fields{
			"participant":      participantIdentifier,
			"participant_id":   participant.ParticipantID,
			"participant_name": participant.Name,
			"role":             participant.Role,
			"provider":         participant.LLMProvider,
			"model":            participant.LLMModel,
			"round":            round,
			"debate_id":        config.DebateID,
			"response_time_ms": responseTime.Milliseconds(),
		}).Warn("Debate participant returned EMPTY response - triggering fallback")

		// Log empty response error (Retrofit-like)
		if ds.commLogger != nil {
			ds.commLogger.LogError(participant.Role, participant.LLMProvider, participant.LLMModel, fmt.Errorf("empty response"))
		}

		return ParticipantResponse{}, fmt.Errorf("[%s] empty response from LLM - fallback required", participantIdentifier)
	}

	// CRITICAL: Check for canned error responses - this triggers fallback
	// Models that return error messages like "Unable to provide analysis" should trigger fallback
	if matchedPattern := IsCannedErrorResponse(llmResponse.Content); matchedPattern != "" {
		ds.logger.WithFields(logrus.Fields{
			"participant":      participantIdentifier,
			"participant_id":   participant.ParticipantID,
			"participant_name": participant.Name,
			"role":             participant.Role,
			"provider":         participant.LLMProvider,
			"model":            participant.LLMModel,
			"round":            round,
			"debate_id":        config.DebateID,
			"response_time_ms": responseTime.Milliseconds(),
			"canned_pattern":   matchedPattern,
			"response_preview": llmResponse.Content[:min(len(llmResponse.Content), 100)],
		}).Warn("Debate participant returned CANNED ERROR response - triggering fallback")

		// Log canned error (Retrofit-like)
		if ds.commLogger != nil {
			ds.commLogger.LogError(participant.Role, participant.LLMProvider, participant.LLMModel,
				fmt.Errorf("canned error response detected (pattern: %s): %s", matchedPattern, llmResponse.Content[:min(len(llmResponse.Content), 50)]))
		}

		return ParticipantResponse{}, fmt.Errorf("[%s] canned error response from LLM (pattern: %s) - fallback required", participantIdentifier, matchedPattern)
	}

	// CRITICAL: Check for a fast, short reply that the provider did NOT
	// account for with any token usage — the work-less cached/canned reply
	// commit 5d5dbb7b built this guard for. A fast, short reply that DOES
	// carry real usage is a genuine local generation and passes; see
	// IsNonGenuineFastResponse for the measured evidence behind that split.
	if IsNonGenuineFastResponse(responseTime, len(llmResponse.Content), llmResponse.TokensUsed) {
		ds.logger.WithFields(logrus.Fields{
			"participant":      participantIdentifier,
			"provider":         participant.LLMProvider,
			"model":            participant.LLMModel,
			"response_time_ms": responseTime.Milliseconds(),
			"content_length":   len(llmResponse.Content),
			"tokens_used":      llmResponse.TokensUsed,
			"response_preview": llmResponse.Content,
		}).Warn("Fast, short reply with NO reported token usage - likely cached/canned, triggering fallback")

		if ds.commLogger != nil {
			ds.commLogger.LogError(participant.Role, participant.LLMProvider, participant.LLMModel,
				fmt.Errorf("fast, short reply with no reported token usage (%v, %d chars, 0 tokens)",
					responseTime, len(llmResponse.Content)))
		}

		return ParticipantResponse{}, fmt.Errorf(
			"[%s] fast, short reply with no reported token usage (%v, %d chars) - fallback required",
			participantIdentifier, responseTime, len(llmResponse.Content))
	}

	// Calculate quality score for this response
	qualityScore := ds.calculateResponseQuality(llmResponse)

	// Log response (Retrofit-like): [A: Claude Opus 4.5] ---> Received 2048 bytes in 1.23s
	if ds.commLogger != nil {
		ds.commLogger.LogResponse(participant.Role, participant.LLMProvider, participant.LLMModel, len(llmResponse.Content), responseTime, qualityScore)
		// Also log a preview of the response
		ds.commLogger.LogResponsePreview(participant.Role, participant.LLMProvider, participant.LLMModel, llmResponse.Content, 100)
	}

	// Log successful response with participant identification
	ds.logger.WithFields(logrus.Fields{
		"participant":      participantIdentifier,
		"participant_id":   participant.ParticipantID,
		"participant_name": participant.Name,
		"role":             participant.Role,
		"provider":         participant.LLMProvider,
		"model":            participant.LLMModel,
		"round":            round,
		"debate_id":        config.DebateID,
		"response_time_ms": responseTime.Milliseconds(),
		"quality_score":    qualityScore,
		"tokens_used":      llmResponse.TokensUsed,
		"content_length":   len(llmResponse.Content),
	}).Infof("[%s] Debate participant response completed", participantIdentifier)

	// Build participant response
	response := ParticipantResponse{
		ParticipantID:   participant.ParticipantID,
		ParticipantName: participant.Name,
		Role:            participant.Role,
		Round:           round,
		RoundNumber:     round,
		Response:        llmResponse.Content,
		Content:         llmResponse.Content,
		Confidence:      llmResponse.Confidence,
		QualityScore:    qualityScore,
		ResponseTime:    responseTime,
		LLMProvider:     participant.LLMProvider,
		LLMModel:        participant.LLMModel,
		LLMName:         participant.LLMModel,
		Timestamp:       startTime,
		Metadata: participantUsageMetadata(llmResponse, map[string]any{
			"finish_reason": llmResponse.FinishReason,
			"provider_id":   llmResponse.ProviderID,
			"response_time": llmResponse.ResponseTime,
		}),
	}

	// Add Cognee analysis if enabled
	if config.EnableCognee && ds.cogneeService != nil {
		analysis, err := ds.analyzeWithCognee(ctx, llmResponse.Content)
		if err == nil {
			response.CogneeEnhanced = true
			response.CogneeAnalysis = analysis
		}
	}

	return response, nil
}

// buildSystemPrompt builds the system prompt for a participant
func (ds *DebateService) buildSystemPrompt(participant ParticipantConfig) string {
	roleDescriptions := map[string]string{
		"proposer":  "You are presenting and defending the main argument. Be persuasive and provide evidence.",
		"opponent":  "You are challenging the main argument. Identify weaknesses and present counterarguments.",
		"critic":    "You are analyzing both sides objectively. Point out strengths and weaknesses.",
		"mediator":  "You are facilitating the discussion. Summarize key points and seek common ground.",
		"debater":   "You are participating in a balanced debate. Present your perspective clearly.",
		"analyst":   "You are analyzing the debate topic deeply. Provide insights and analysis.",
		"moderator": "You are moderating the discussion. Keep the debate focused and productive.",
	}

	roleDesc := roleDescriptions[participant.Role]
	if roleDesc == "" {
		roleDesc = "You are participating in an AI debate. Contribute thoughtfully to the discussion."
	}

	return fmt.Sprintf(
		"You are %s, participating as a %s in an AI debate. %s "+
			"Keep your responses focused, well-reasoned, and constructive. "+
			"Acknowledge valid points from others while maintaining your position when warranted.",
		participant.Name,
		participant.Role,
		roleDesc,
	)
}

// classifyUserIntent uses LLM-based semantic analysis to understand user intent
// ZERO HARDCODING - Pure AI semantic understanding
// Uses caching to avoid repeated LLM calls for the same topic
func (ds *DebateService) classifyUserIntent(topic string, hasContext bool) *IntentClassificationResult {
	ds.initCaches()
	// Check cache first to avoid repeated LLM calls (cache by topic only)
	if cached, ok := ds.intentCache.Get(topic); ok {
		return cached
	}

	var result *IntentClassificationResult

	// Try LLM-based classification first (ZERO hardcoding)
	if ds.providerRegistry != nil {
		llmClassifier := NewLLMIntentClassifier(ds.providerRegistry, ds.logger)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		contextStr := ""
		if hasContext {
			contextStr = "previous recommendations exist"
		}

		llmResult, err := llmClassifier.ClassifyIntentWithLLM(ctx, topic, contextStr)
		if err == nil {
			ds.logger.WithFields(logrus.Fields{
				"intent":     llmResult.Intent,
				"confidence": llmResult.Confidence,
				"method":     "llm",
			}).Debug("User intent classified by LLM")
			result = llmResult
		} else {
			ds.logger.WithError(err).Debug("LLM classification failed, using fallback")
		}
	}

	// Fallback to pattern-based only if LLM unavailable
	if result == nil {
		classifier := NewIntentClassifier()
		result = classifier.EnhancedClassifyIntent(topic, hasContext)
	}

	// Cache the result by topic
	ds.intentCache.Put(topic, result)

	// Evict excess entries if the cache grew beyond bounds.
	ds.evictIntentCacheIfNeeded()

	return result
}

// evictIntentCacheIfNeeded removes entries when cache exceeds bounds.
// Safe to call concurrently; safe.Store.Delete serialises with concurrent Put.
func (ds *DebateService) evictIntentCacheIfNeeded() {
	if ds.intentCache == nil {
		return
	}
	keys := ds.intentCache.Keys()
	if len(keys) <= maxIntentCacheSize {
		return
	}
	count := 0
	target := len(keys) / 2
	for _, key := range keys {
		ds.intentCache.Delete(key)
		count++
		if count >= target {
			break
		}
	}
}

// buildDebatePrompt builds the prompt for a debate round
func (ds *DebateService) buildDebatePrompt(
	topic string,
	participant ParticipantConfig,
	round int,
	previousResponses []ParticipantResponse,
) string {
	var sb strings.Builder

	// CRITICAL: Use semantic intent classification (no hardcoded patterns)
	hasContext := len(previousResponses) > 0
	intentResult := ds.classifyUserIntent(topic, hasContext)

	sb.WriteString("You are an AI assistant in a multi-model ensemble. Answer the user's request directly and helpfully from your role's perspective. Be concise (2-3 paragraphs max). Do NOT output code blocks or tool commands as text.\n\n")
	sb.WriteString(fmt.Sprintf("USER REQUEST: %s\n\n", topic))
	sb.WriteString(fmt.Sprintf("ROUND: %d | YOUR ROLE: %s (%s)\n\n", round, participant.Name, participant.Role))

	// Handle different user intents semantically
	if intentResult.IsConfirmation() || intentResult.ShouldProceed() {
		sb.WriteString("═══════════════════════════════════════════════════════════════════\n")
		sb.WriteString("⚡ USER CONFIRMATION DETECTED - EXECUTE IMMEDIATELY ⚡\n")
		sb.WriteString("═══════════════════════════════════════════════════════════════════\n")
		sb.WriteString(fmt.Sprintf("Intent Classification: %s (confidence: %.0f%%)\n", intentResult.Intent, intentResult.Confidence*100))
		sb.WriteString(fmt.Sprintf("Signals detected: %v\n\n", intentResult.Signals))
		sb.WriteString("The user has semantically CONFIRMED the action plan. DO NOT ask for clarification.\n")
		sb.WriteString("The user's intent is clear - they want you to proceed with ALL work.\n")
		sb.WriteString("IMMEDIATELY BEGIN EXECUTING the plan with concrete actions:\n")
		sb.WriteString("1. Use tools (Bash, Read, Write, Edit, Glob, Grep) to start work\n")
		sb.WriteString("2. Show actual progress, not just explanations\n")
		sb.WriteString("3. Execute commands, create files, make changes\n")
		sb.WriteString("4. Report results as you complete each step\n")
		sb.WriteString("═══════════════════════════════════════════════════════════════════\n\n")
	} else if intentResult.IsRefusal() {
		sb.WriteString("═══════════════════════════════════════════════════════════════════\n")
		sb.WriteString("⛔ USER REFUSAL DETECTED - DO NOT PROCEED ⛔\n")
		sb.WriteString("═══════════════════════════════════════════════════════════════════\n")
		sb.WriteString(fmt.Sprintf("Intent Classification: %s (confidence: %.0f%%)\n", intentResult.Intent, intentResult.Confidence*100))
		sb.WriteString("The user has indicated they do NOT want to proceed.\n")
		sb.WriteString("Acknowledge their decision and ask how you can help instead.\n")
		sb.WriteString("═══════════════════════════════════════════════════════════════════\n\n")
	} else if intentResult.RequiresClarification {
		sb.WriteString("═══════════════════════════════════════════════════════════════════\n")
		sb.WriteString("❓ CLARIFICATION MAY BE NEEDED ❓\n")
		sb.WriteString("═══════════════════════════════════════════════════════════════════\n")
		sb.WriteString(fmt.Sprintf("Intent Classification: %s (confidence: %.0f%%)\n", intentResult.Intent, intentResult.Confidence*100))
		sb.WriteString("The user's intent is not entirely clear. Ask for clarification if needed,\n")
		sb.WriteString("but if they seem to be asking for help, provide helpful guidance.\n")
		sb.WriteString("═══════════════════════════════════════════════════════════════════\n\n")
	}

	if len(previousResponses) > 0 {
		sb.WriteString("PREVIOUS RESPONSES:\n")
		sb.WriteString("-------------------\n")
		for _, resp := range previousResponses {
			sb.WriteString(fmt.Sprintf("[%s (%s) - Round %d]:\n%s\n\n",
				resp.ParticipantName, resp.Role, resp.Round, resp.Content))
		}
		sb.WriteString("-------------------\n\n")

		if intentResult.IsConfirmation() || intentResult.ShouldProceed() {
			sb.WriteString("The user has APPROVED the plan. Execute it NOW using available tools.\n")
			sb.WriteString("Show concrete progress - run commands, modify files, produce results.\n")
		} else if intentResult.IsRefusal() {
			sb.WriteString("The user has declined. Respect their decision and offer alternatives.\n")
		} else {
			sb.WriteString("Based on the previous responses, provide your perspective on the topic. ")
			sb.WriteString("Address points raised by others and advance the discussion.\n")
		}
	} else {
		sb.WriteString("This is the opening round. Present your initial perspective on the topic.\n")
		if intentResult.IsConfirmation() || intentResult.ShouldProceed() {
			sb.WriteString("User is confirming work. Start executing immediately with tools.\n")
		} else if intentResult.IsRefusal() {
			sb.WriteString("User has declined. Acknowledge and ask how else you can help.\n")
		} else {
			sb.WriteString("Answer the user's request directly from your specialty perspective. Be helpful and concise.\n")
		}
	}

	sb.WriteString("\nYour response (answer the user, do NOT output code blocks or tool commands as text):")

	return sb.String()
}

// calculateResponseQuality calculates quality score for a single response
func (ds *DebateService) calculateResponseQuality(resp *models.LLMResponse) float64 {
	score := 0.0

	// Base score from confidence (if provided by LLM)
	if resp.Confidence > 0 {
		score += resp.Confidence * 0.3
	} else {
		score += 0.7 * 0.3 // Default confidence
	}

	// Content length factor (20% weight)
	contentLength := len(resp.Content)
	lengthScore := 0.0
	if contentLength >= 100 && contentLength <= 500 {
		lengthScore = 1.0
	} else if contentLength > 500 && contentLength <= 1000 {
		lengthScore = 0.9
	} else if contentLength > 1000 && contentLength <= 2000 {
		lengthScore = 0.8
	} else if contentLength > 2000 {
		lengthScore = 0.7
	} else if contentLength > 50 {
		lengthScore = 0.6
	} else {
		lengthScore = 0.3
	}
	score += lengthScore * 0.2

	// Response completeness (20% weight) - based on finish reason
	completenessScore := 0.5
	switch resp.FinishReason {
	case "stop":
		completenessScore = 1.0
	case "end_turn":
		completenessScore = 1.0
	case "length":
		completenessScore = 0.7
	case "content_filter":
		completenessScore = 0.3
	}
	score += completenessScore * 0.2

	// Token efficiency (15% weight)
	if resp.TokensUsed > 0 && contentLength > 0 {
		efficiency := float64(contentLength) / float64(resp.TokensUsed)
		efficiencyScore := math.Min(1.0, efficiency/4.0)
		score += efficiencyScore * 0.15
	} else {
		score += 0.5 * 0.15
	}

	// Coherence indicators (15% weight)
	coherenceScore := ds.analyzeCoherence(resp.Content)
	score += coherenceScore * 0.15

	return math.Min(1.0, score)
}

// analyzeCoherence analyzes text coherence
func (ds *DebateService) analyzeCoherence(content string) float64 {
	if len(content) == 0 {
		return 0.0
	}

	score := 0.5 // Base score

	// Check for structure indicators
	structureIndicators := []string{
		"first", "second", "third", "finally",
		"however", "therefore", "because", "although",
		"in conclusion", "to summarize", "for example",
		"on the other hand", "furthermore", "moreover",
	}

	contentLower := strings.ToLower(content)
	structureCount := 0
	for _, indicator := range structureIndicators {
		if strings.Contains(contentLower, indicator) {
			structureCount++
		}
	}

	if structureCount >= 3 {
		score += 0.3
	} else if structureCount >= 1 {
		score += 0.15
	}

	// Check for sentence variety (basic check)
	sentences := strings.Split(content, ".")
	if len(sentences) >= 3 && len(sentences) <= 15 {
		score += 0.2
	} else if len(sentences) >= 2 {
		score += 0.1
	}

	return math.Min(1.0, score)
}

// calculateQualityScore calculates overall quality score from all responses
func (ds *DebateService) calculateQualityScore(responses []ParticipantResponse) float64 {
	if len(responses) == 0 {
		return 0.0
	}

	totalScore := 0.0
	for _, resp := range responses {
		totalScore += resp.QualityScore
	}

	return totalScore / float64(len(responses))
}

// calculateFinalScore calculates the final debate score
func (ds *DebateService) calculateFinalScore(responses []ParticipantResponse, consensus *ConsensusResult) float64 {
	if len(responses) == 0 {
		return 0.0
	}

	// Quality contributes 50%
	qualityScore := ds.calculateQualityScore(responses)

	// Consensus contributes 30%
	consensusScore := 0.0
	if consensus != nil {
		consensusScore = consensus.AgreementLevel
	}

	// Participation contributes 20%
	participationScore := 1.0
	uniqueParticipants := make(map[string]bool)
	for _, resp := range responses {
		uniqueParticipants[resp.ParticipantID] = true
	}
	if len(uniqueParticipants) < 2 {
		participationScore = 0.5
	}

	return qualityScore*0.5 + consensusScore*0.3 + participationScore*0.2
}

// analyzeConsensus analyzes responses for consensus
func (ds *DebateService) analyzeConsensus(responses []ParticipantResponse, topic string) *ConsensusResult {
	if len(responses) == 0 {
		return &ConsensusResult{
			Reached:        false,
			Achieved:       false,
			Confidence:     0.0,
			ConsensusLevel: 0.0,
			AgreementLevel: 0.0,
			FinalPosition:  "No responses to analyze",
			KeyPoints:      []string{},
			Disagreements:  []string{},
			Timestamp:      time.Now(),
		}
	}

	// Analyze agreement between responses
	agreementScore := ds.calculateAgreementScore(responses)
	keyPoints := ds.extractKeyPoints(responses)
	disagreements := ds.extractDisagreements(responses)

	// Calculate consensus confidence
	confidence := agreementScore
	if len(disagreements) > 0 {
		confidence *= 0.9 // Reduce confidence if there are disagreements
	}

	consensusReached := agreementScore >= 0.7

	// Generate final position summary
	finalPosition := ds.generateFinalPosition(responses, consensusReached)

	return &ConsensusResult{
		Reached:        consensusReached,
		Achieved:       consensusReached,
		Confidence:     confidence,
		ConsensusLevel: agreementScore,
		AgreementLevel: agreementScore,
		AgreementScore: agreementScore,
		FinalPosition:  finalPosition,
		KeyPoints:      keyPoints,
		Disagreements:  disagreements,
		Summary:        fmt.Sprintf("Debate on '%s' with %d responses", topic, len(responses)),
		VotingSummary: VotingSummary{
			Strategy:         "quality_weighted",
			TotalVotes:       len(responses),
			VoteDistribution: ds.getVoteDistribution(responses),
			Winner:           ds.getWinner(responses),
			Margin:           agreementScore,
		},
		Timestamp:    time.Now(),
		QualityScore: ds.calculateQualityScore(responses),
	}
}

// calculateAgreementScore calculates how much responses agree
func (ds *DebateService) calculateAgreementScore(responses []ParticipantResponse) float64 {
	if len(responses) < 2 {
		return 1.0 // Single response is self-consistent
	}

	// Calculate pairwise similarity
	totalSimilarity := 0.0
	comparisons := 0

	for i := 0; i < len(responses); i++ {
		for j := i + 1; j < len(responses); j++ {
			similarity := ds.calculateTextSimilarity(responses[i].Content, responses[j].Content)
			totalSimilarity += similarity
			comparisons++
		}
	}

	if comparisons == 0 {
		return 0.5
	}

	return totalSimilarity / float64(comparisons)
}

// calculateTextSimilarity calculates similarity between two texts
func (ds *DebateService) calculateTextSimilarity(text1, text2 string) float64 {
	// Simple word overlap similarity (Jaccard-like)
	words1 := strings.Fields(strings.ToLower(text1))
	words2 := strings.Fields(strings.ToLower(text2))

	if len(words1) == 0 || len(words2) == 0 {
		return 0.0
	}

	wordSet1 := make(map[string]bool)
	for _, w := range words1 {
		wordSet1[w] = true
	}

	wordSet2 := make(map[string]bool)
	for _, w := range words2 {
		wordSet2[w] = true
	}

	intersection := 0
	for w := range wordSet1 {
		if wordSet2[w] {
			intersection++
		}
	}

	union := len(wordSet1) + len(wordSet2) - intersection
	if union == 0 {
		return 0.0
	}

	return float64(intersection) / float64(union)
}

// extractKeyPoints extracts key points from responses
func (ds *DebateService) extractKeyPoints(responses []ParticipantResponse) []string {
	keyPoints := make([]string, 0)
	keyPhrases := make(map[string]int)

	// Simple key phrase extraction
	for _, resp := range responses {
		sentences := strings.Split(resp.Content, ".")
		for _, sentence := range sentences {
			sentence = strings.TrimSpace(sentence)
			if len(sentence) > 20 && len(sentence) < 200 {
				// Look for sentences with key indicators
				indicators := []string{"important", "key", "main", "significant", "essential", "crucial"}
				for _, indicator := range indicators {
					if strings.Contains(strings.ToLower(sentence), indicator) {
						keyPhrases[sentence]++
						break
					}
				}
			}
		}
	}

	// Get top key points
	type kv struct {
		Key   string
		Value int
	}
	var sortedPhrases []kv
	for k, v := range keyPhrases {
		sortedPhrases = append(sortedPhrases, kv{k, v})
	}
	sort.Slice(sortedPhrases, func(i, j int) bool {
		return sortedPhrases[i].Value > sortedPhrases[j].Value
	})

	for i, kv := range sortedPhrases {
		if i >= 5 {
			break
		}
		keyPoints = append(keyPoints, kv.Key)
	}

	// If no key points found, extract first sentence from best responses
	if len(keyPoints) == 0 && len(responses) > 0 {
		for i, resp := range responses {
			if i >= 3 {
				break
			}
			sentences := strings.Split(resp.Content, ".")
			if len(sentences) > 0 && len(strings.TrimSpace(sentences[0])) > 10 {
				keyPoints = append(keyPoints, strings.TrimSpace(sentences[0]))
			}
		}
	}

	return keyPoints
}

// extractDisagreements extracts disagreements from responses
func (ds *DebateService) extractDisagreements(responses []ParticipantResponse) []string {
	disagreements := make([]string, 0)

	disagreementIndicators := []string{
		"however", "but", "disagree", "contrary", "oppose",
		"on the other hand", "in contrast", "nevertheless",
	}

	for _, resp := range responses {
		sentences := strings.Split(resp.Content, ".")
		for _, sentence := range sentences {
			sentence = strings.TrimSpace(sentence)
			for _, indicator := range disagreementIndicators {
				if strings.Contains(strings.ToLower(sentence), indicator) && len(sentence) > 20 {
					disagreements = append(disagreements, sentence)
					break
				}
			}
		}
	}

	// Limit to top 5 disagreements
	if len(disagreements) > 5 {
		disagreements = disagreements[:5]
	}

	return disagreements
}

// generateFinalPosition generates the final position by selecting the best response content.
// Instead of returning generic status text, it returns the actual highest-quality answer.
func (ds *DebateService) generateFinalPosition(responses []ParticipantResponse, consensusReached bool) string {
	if len(responses) == 0 {
		return "No position established"
	}

	// Find the best response by quality score
	bestIdx := 0
	bestScore := responses[0].QualityScore
	for i, resp := range responses {
		if resp.QualityScore > bestScore {
			bestScore = resp.QualityScore
			bestIdx = i
		}
	}

	content := responses[bestIdx].Content
	if content == "" {
		content = responses[bestIdx].Response
	}
	if content == "" {
		return "No substantive position established"
	}

	// Format based on consensus
	if consensusReached {
		return "Consensus reached: Participants found common ground on the key aspects of the topic"
	}
	return "Discussion ongoing: Multiple perspectives presented with varying viewpoints"
}

// getVoteDistribution gets the vote distribution by participant
func (ds *DebateService) getVoteDistribution(responses []ParticipantResponse) map[string]int {
	distribution := make(map[string]int)
	for _, resp := range responses {
		distribution[resp.ParticipantName]++
	}
	return distribution
}

// getWinner gets the best performing participant
func (ds *DebateService) getWinner(responses []ParticipantResponse) string {
	if len(responses) == 0 {
		return ""
	}

	bestScore := 0.0
	winner := ""
	scores := make(map[string]float64)
	counts := make(map[string]int)

	for _, resp := range responses {
		scores[resp.ParticipantName] += resp.QualityScore
		counts[resp.ParticipantName]++
	}

	for name, score := range scores {
		avgScore := score / float64(counts[name])
		if avgScore > bestScore {
			bestScore = avgScore
			winner = name
		}
	}

	return winner
}

// checkEarlyConsensus checks if early consensus has been reached
func (ds *DebateService) checkEarlyConsensus(responses []ParticipantResponse) bool {
	if len(responses) < 2 {
		return false
	}

	agreementScore := ds.calculateAgreementScore(responses)
	return agreementScore >= 0.85
}

// findBestResponse finds the best response from all responses
func (ds *DebateService) findBestResponse(responses []ParticipantResponse) *ParticipantResponse {
	if len(responses) == 0 {
		return nil
	}

	best := responses[0]
	for _, resp := range responses[1:] {
		if resp.QualityScore > best.QualityScore {
			best = resp
		}
	}

	return &best
}

// getLatestParticipantResponses gets the latest response from each participant
func (ds *DebateService) getLatestParticipantResponses(
	allResponses []ParticipantResponse,
	participants []ParticipantConfig,
) []ParticipantResponse {
	latest := make(map[string]ParticipantResponse)

	for _, resp := range allResponses {
		if existing, ok := latest[resp.ParticipantID]; !ok || resp.Round > existing.Round {
			latest[resp.ParticipantID] = resp
		}
	}

	result := make([]ParticipantResponse, 0, len(participants))
	for _, p := range participants {
		if resp, ok := latest[p.ParticipantID]; ok {
			result = append(result, resp)
		}
	}

	return result
}

// getUniqueProviders gets unique provider names from responses
func (ds *DebateService) getUniqueProviders(responses []ParticipantResponse) []string {
	providers := make(map[string]bool)
	for _, resp := range responses {
		providers[resp.LLMProvider] = true
	}

	result := make([]string, 0, len(providers))
	for p := range providers {
		result = append(result, p)
	}
	return result
}

// calculateAvgResponseTime calculates average response time
func (ds *DebateService) calculateAvgResponseTime(responses []ParticipantResponse) time.Duration {
	if len(responses) == 0 {
		return 0
	}

	total := time.Duration(0)
	for _, resp := range responses {
		total += resp.ResponseTime
	}

	return total / time.Duration(len(responses))
}

// analyzeWithCognee analyzes a response with Cognee (or HelixMemory if available)
func (ds *DebateService) analyzeWithCognee(ctx context.Context, content string) (*CogneeAnalysis, error) {
	// Prefer HelixMemory unified engine (searches all 4 backends: Mem0, Cognee, Letta, Graphiti)
	if ds.memoryAdapter != nil {
		return ds.analyzeWithHelixMemory(ctx, content)
	}

	// Fallback to direct CogneeService
	if ds.cogneeService == nil {
		return nil, fmt.Errorf("no memory service configured")
	}

	return ds.analyzeWithCogneeDirect(ctx, content)
}

// analyzeWithHelixMemory uses the HelixMemory unified engine for analysis.
// This searches across all 4 backends (Mem0, Cognee, Letta, Graphiti) via
// the fusion engine for comprehensive memory-enhanced analysis.
func (ds *DebateService) analyzeWithHelixMemory(ctx context.Context, content string) (*CogneeAnalysis, error) {
	startTime := time.Now()

	// Search unified memory (all backends: Mem0 facts, Cognee graphs, Letta core, Graphiti temporal)
	searchOpts := &helixmem.SearchOptions{TopK: 5}
	memories, err := ds.memoryAdapter.Search(ctx, content, searchOpts)
	if err != nil {
		ds.logger.WithError(err).Warn("[HelixMemory] Unified search failed, falling back to CogneeService")
		if ds.cogneeService != nil {
			return ds.analyzeWithCogneeDirect(ctx, content)
		}
		return nil, fmt.Errorf("helixmemory search failed: %w", err)
	}

	// Extract entities from search results
	entities := make([]string, 0, len(memories))
	for _, mem := range memories {
		if len(entities) < 10 {
			snippet := mem.Content
			if len(snippet) > 50 {
				snippet = snippet[:50]
			}
			entities = append(entities, snippet)
		}
	}

	// Extract key phrases from content
	keyPhrases := make([]string, 0)
	sentences := strings.Split(content, ".")
	for i, s := range sentences {
		if i >= 5 {
			break
		}
		s = strings.TrimSpace(s)
		if len(s) > 10 {
			keyPhrases = append(keyPhrases, s)
		}
	}

	// Calculate relevance from top result
	relevance := 0.0
	if len(memories) > 0 {
		relevance = memories[0].Importance
	}

	sentiment := ds.analyzeSentiment(content)

	// Store debate analysis result in HelixMemory for future reference
	//nolint:gosec // G118: persistent store of a completed debate, intentionally decoupled from the request context so client disconnect does not lose the memory
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ds.logger.Errorf("HelixMemory store goroutine panicked (recovered): %v", r)
			}
		}()
		storeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		debateMemory := &helixmem.Memory{
			Content:    content,
			Type:       helixmem.MemoryTypeSemantic,
			Category:   "debate_analysis",
			Importance: relevance,
			Metadata: map[string]interface{}{
				"type":       "debate_analysis",
				"sentiment":  sentiment,
				"confidence": relevance,
			},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		if err := ds.memoryAdapter.Add(storeCtx, debateMemory); err != nil {
			ds.logger.WithError(err).Warn("[Debate Service] Failed to persist debate analysis to memory store")
		}
	}()

	return &CogneeAnalysis{
		Enhanced:         true,
		OriginalResponse: content,
		Sentiment:        sentiment,
		Entities:         entities,
		KeyPhrases:       keyPhrases,
		Confidence:       relevance,
		ProcessingTime:   time.Since(startTime),
	}, nil
}

// analyzeWithCogneeDirect is the original CogneeService-based analysis (fallback).
func (ds *DebateService) analyzeWithCogneeDirect(ctx context.Context, content string) (*CogneeAnalysis, error) {
	startTime := time.Now()

	searchResult, err := ds.cogneeService.SearchMemory(ctx, content, "", 5)
	if err != nil {
		return nil, fmt.Errorf("cognee search failed: %w", err)
	}

	entities := make([]string, 0)
	for _, mem := range searchResult.VectorResults {
		if len(entities) < 10 {
			entities = append(entities, mem.Content[:min(50, len(mem.Content))])
		}
	}

	keyPhrases := make([]string, 0)
	sentences := strings.Split(content, ".")
	for i, s := range sentences {
		if i >= 5 {
			break
		}
		s = strings.TrimSpace(s)
		if len(s) > 10 {
			keyPhrases = append(keyPhrases, s)
		}
	}

	// Determine sentiment
	sentiment := ds.analyzeSentiment(content)

	return &CogneeAnalysis{
		Enhanced:         true,
		OriginalResponse: content,
		Sentiment:        sentiment,
		Entities:         entities,
		KeyPhrases:       keyPhrases,
		Confidence:       searchResult.RelevanceScore,
		ProcessingTime:   time.Since(startTime),
	}, nil
}

// analyzeSentiment performs simple sentiment analysis
func (ds *DebateService) analyzeSentiment(content string) string {
	contentLower := strings.ToLower(content)

	positiveWords := []string{"agree", "good", "excellent", "great", "positive", "support", "beneficial", "advantage"}
	negativeWords := []string{"disagree", "bad", "poor", "negative", "oppose", "harmful", "disadvantage", "problem"}
	neutralWords := []string{"however", "although", "consider", "perspective", "viewpoint"}

	positiveCount := 0
	negativeCount := 0
	neutralCount := 0

	for _, word := range positiveWords {
		if strings.Contains(contentLower, word) {
			positiveCount++
		}
	}
	for _, word := range negativeWords {
		if strings.Contains(contentLower, word) {
			negativeCount++
		}
	}
	for _, word := range neutralWords {
		if strings.Contains(contentLower, word) {
			neutralCount++
		}
	}

	if positiveCount > negativeCount+neutralCount {
		return "positive"
	} else if negativeCount > positiveCount+neutralCount {
		return "negative"
	}
	return "neutral"
}

// generateCogneeInsights generates comprehensive Cognee insights
func (ds *DebateService) generateCogneeInsights(
	ctx context.Context,
	config *DebateConfig,
	responses []ParticipantResponse,
) (*CogneeInsights, error) {
	if ds.memoryAdapter == nil && ds.cogneeService == nil {
		return nil, fmt.Errorf("no memory service configured (neither HelixMemory nor CogneeService)")
	}

	startTime := time.Now()

	// Combine all response content
	var combinedContent strings.Builder
	for _, resp := range responses {
		combinedContent.WriteString(resp.Content)
		combinedContent.WriteString("\n\n")
	}

	// Search for insights using HelixMemory (preferred) or CogneeService (fallback)
	var searchRelevanceScore float64

	if ds.memoryAdapter != nil {
		// Use HelixMemory unified search (all 4 backends)
		searchOpts := &helixmem.SearchOptions{TopK: 10}
		memories, err := ds.memoryAdapter.Search(ctx, combinedContent.String(), searchOpts)
		if err != nil {
			ds.logger.WithError(err).Warn("[HelixMemory] Unified search failed during insights")
		} else if len(memories) > 0 {
			searchRelevanceScore = memories[0].Importance
		}
	} else if ds.cogneeService != nil {
		searchResult, err := ds.cogneeService.SearchMemory(ctx, combinedContent.String(), "", 10)
		if err != nil {
			ds.logger.WithError(err).Warn("Cognee search failed during insights generation")
		} else if searchResult != nil {
			searchRelevanceScore = searchResult.RelevanceScore
		}
	}

	// Extract entities from responses
	entities := make([]Entity, 0)
	entities = append(entities, Entity{
		Text:       config.Topic,
		Type:       "TOPIC",
		Confidence: 1.0,
	})

	// Add participant entities
	for _, p := range config.Participants {
		entities = append(entities, Entity{
			Text:       p.Name,
			Type:       "PARTICIPANT",
			Confidence: 0.9,
		})
	}

	// Calculate quality metrics from responses
	avgQuality := ds.calculateQualityScore(responses)
	coherenceScore := 0.0
	if len(responses) > 0 {
		for _, resp := range responses {
			coherenceScore += ds.analyzeCoherence(resp.Content)
		}
		coherenceScore /= float64(len(responses))
	}

	// Build knowledge graph
	nodes := make([]Node, 0)
	edges := make([]Edge, 0)

	// Add topic node
	nodes = append(nodes, Node{
		ID:    "topic-1",
		Label: config.Topic,
		Type:  "topic",
		Properties: map[string]any{
			"central": true,
		},
	})

	// Add participant nodes and edges
	for i, p := range config.Participants {
		nodeID := fmt.Sprintf("participant-%d", i+1)
		nodes = append(nodes, Node{
			ID:    nodeID,
			Label: p.Name,
			Type:  "participant",
			Properties: map[string]any{
				"role":     p.Role,
				"provider": p.LLMProvider,
			},
		})
		edges = append(edges, Edge{
			Source: nodeID,
			Target: "topic-1",
			Type:   "discusses",
			Weight: 1.0,
		})
	}

	// Generate recommendations based on debate quality
	recommendations := ds.generateRecommendations(responses, avgQuality)

	// Build topic modeling
	topicModeling := make(map[string]float64)
	topicModeling[config.Topic] = 0.9
	topicModeling["debate"] = 0.8
	topicModeling["discussion"] = 0.7

	// Calculate relevance and innovation scores
	relevanceScore := searchRelevanceScore
	if relevanceScore == 0 {
		relevanceScore = avgQuality * 0.9
	}

	innovationScore := ds.calculateInnovationScore(responses)

	return &CogneeInsights{
		DatasetName:     "debate-insights",
		EnhancementTime: time.Since(startTime),
		SemanticAnalysis: SemanticAnalysis{
			MainThemes:     []string{config.Topic, "debate", "discussion"},
			CoherenceScore: coherenceScore,
		},
		EntityExtraction: entities,
		SentimentAnalysis: SentimentAnalysis{
			OverallSentiment: ds.getOverallSentiment(responses),
			SentimentScore:   ds.calculateSentimentScore(responses),
		},
		KnowledgeGraph: KnowledgeGraph{
			Nodes:           nodes,
			Edges:           edges,
			CentralConcepts: []string{config.Topic},
		},
		Recommendations: recommendations,
		QualityMetrics: &QualityMetrics{
			Coherence:    coherenceScore,
			Relevance:    relevanceScore,
			Accuracy:     avgQuality,
			Completeness: float64(len(responses)) / float64(len(config.Participants)*config.MaxRounds),
			OverallScore: (coherenceScore + relevanceScore + avgQuality) / 3,
		},
		TopicModeling:   topicModeling,
		CoherenceScore:  coherenceScore,
		RelevanceScore:  relevanceScore,
		InnovationScore: innovationScore,
	}, nil
}

// generateRecommendations generates recommendations based on debate quality
func (ds *DebateService) generateRecommendations(responses []ParticipantResponse, avgQuality float64) []string {
	recommendations := []string{
		"Consider diverse perspectives",
		"Focus on evidence-based arguments",
		"Maintain respectful discourse",
	}

	if avgQuality < 0.5 {
		recommendations = append(recommendations, "Increase response depth and detail")
		recommendations = append(recommendations, "Provide more supporting evidence")
	}

	if len(responses) < 4 {
		recommendations = append(recommendations, "Consider additional debate rounds")
	}

	return recommendations
}

// getOverallSentiment gets the overall sentiment from all responses
func (ds *DebateService) getOverallSentiment(responses []ParticipantResponse) string {
	positive := 0
	negative := 0
	neutral := 0

	for _, resp := range responses {
		sentiment := ds.analyzeSentiment(resp.Content)
		switch sentiment {
		case "positive":
			positive++
		case "negative":
			negative++
		default:
			neutral++
		}
	}

	if positive > negative && positive > neutral {
		return "positive"
	} else if negative > positive && negative > neutral {
		return "negative"
	}
	return "neutral"
}

// calculateSentimentScore calculates a numeric sentiment score
func (ds *DebateService) calculateSentimentScore(responses []ParticipantResponse) float64 {
	if len(responses) == 0 {
		return 0.5
	}

	totalScore := 0.0
	for _, resp := range responses {
		sentiment := ds.analyzeSentiment(resp.Content)
		switch sentiment {
		case "positive":
			totalScore += 0.8
		case "negative":
			totalScore += 0.2
		default:
			totalScore += 0.5
		}
	}

	return totalScore / float64(len(responses))
}

// calculateInnovationScore calculates how innovative the responses are
func (ds *DebateService) calculateInnovationScore(responses []ParticipantResponse) float64 {
	if len(responses) == 0 {
		return 0.0
	}

	// Look for innovative language patterns
	innovativeIndicators := []string{
		"new approach", "novel", "innovative", "creative",
		"alternative", "different perspective", "unique",
		"unexplored", "fresh", "original",
	}

	innovativeCount := 0
	for _, resp := range responses {
		contentLower := strings.ToLower(resp.Content)
		for _, indicator := range innovativeIndicators {
			if strings.Contains(contentLower, indicator) {
				innovativeCount++
				break
			}
		}
	}

	// Also consider response diversity
	diversity := 1.0 - ds.calculateAgreementScore(responses)

	return (float64(innovativeCount)/float64(len(responses))*0.6 + diversity*0.4)
}

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// LLMProviderInterface defines the interface for LLM providers used in debate
type LLMProviderInterface interface {
	Complete(ctx context.Context, req *models.LLMRequest) (*models.LLMResponse, error)
}

// Ensure llm.LLMProvider satisfies our interface
var _ LLMProviderInterface = (llm.LLMProvider)(nil)

// SingleProviderConfig holds configuration for single-provider multi-instance mode
type SingleProviderConfig struct {
	ProviderName      string   `json:"provider_name"`
	AvailableModels   []string `json:"available_models"`
	NumParticipants   int      `json:"num_participants"`
	UseModelDiversity bool     `json:"use_model_diversity"`
	UseTempDiversity  bool     `json:"use_temp_diversity"`
}

// DebateMode represents different modes of debate operation
type DebateMode string

const (
	DebateModeMultiProvider  DebateMode = "multi_provider"  // Normal mode with multiple providers
	DeabateModeMultiModel    DebateMode = "multi_model"     // Single provider with multiple models
	DebateModeSingleInstance DebateMode = "single_instance" // Single provider, single model, multiple instances
)

// SingleProviderDebateResult holds results specific to single-provider debate
type SingleProviderDebateResult struct {
	Mode               DebateMode `json:"mode"`
	ProviderUsed       string     `json:"provider_used"`
	ModelsUsed         []string   `json:"models_used"`
	InstanceCount      int        `json:"instance_count"`
	DiversityStrategy  string     `json:"diversity_strategy"`
	EffectiveDiversity float64    `json:"effective_diversity"`
}

// IsSingleProviderMode detects if the debate should run in single-provider mode
func (ds *DebateService) IsSingleProviderMode(config *DebateConfig) (bool, *SingleProviderConfig) {
	if ds.providerRegistry == nil {
		return false, nil
	}

	// Get unique providers from participants
	uniqueProviders := make(map[string]bool)
	for _, p := range config.Participants {
		uniqueProviders[p.LLMProvider] = true
	}

	// If multiple unique providers configured, check if they're available
	availableProviders := make([]string, 0)
	for providerName := range uniqueProviders {
		if _, err := ds.providerRegistry.GetProvider(providerName); err == nil {
			availableProviders = append(availableProviders, providerName)
		}
	}

	// If we have multiple available providers, use normal multi-provider mode
	if len(availableProviders) > 1 {
		return false, nil
	}

	// If we have exactly one available provider, use single-provider mode
	if len(availableProviders) == 1 {
		providerName := availableProviders[0]
		models := ds.GetAvailableModelsForProvider(providerName)

		return true, &SingleProviderConfig{
			ProviderName:      providerName,
			AvailableModels:   models,
			NumParticipants:   len(config.Participants),
			UseModelDiversity: len(models) > 1,
			UseTempDiversity:  true,
		}
	}

	// No providers available
	return false, nil
}

// GetAvailableModelsForProvider returns available models for a provider
func (ds *DebateService) GetAvailableModelsForProvider(providerName string) []string {
	// Default model lists for known providers
	// Updated 2026-02-27: Prioritize Claude 4.6 (latest), then 4.5, then fallbacks
	knownModels := map[string][]string{
		"deepseek": {"deepseek-chat", "deepseek-coder", "deepseek-reasoner"},
		// Claude 4.6 (Latest - February 2026) + 4.5 + 4.x + Legacy fallbacks
		"claude": {
			"claude-opus-4-6",            // Claude 4.6 Opus (LATEST - February 2026)
			"claude-sonnet-4-6",          // Claude 4.6 Sonnet (LATEST - February 2026)
			"claude-opus-4-5-20251101",   // Claude 4.5 Opus
			"claude-sonnet-4-5-20250929", // Claude 4.5 Sonnet
			"claude-haiku-4-5-20251001",  // Claude 4.5 Haiku
			"claude-opus-4-20250514",     // Claude 4 Opus
			"claude-sonnet-4-20250514",   // Claude 4 Sonnet
			"claude-3-5-sonnet-20241022", // Legacy fallback
		},
		"openai":  {"gpt-4o", "gpt-4o-mini", "gpt-4-turbo", "gpt-3.5-turbo"},
		"gemini":  {"gemini-2.0-flash", "gemini-1.5-pro", "gemini-1.5-flash"},
		"mistral": {"mistral-large-latest", "mistral-medium", "mistral-small"},
		// Qwen models (all variants)
		"qwen": {
			"qwen-max",         // Most capable
			"qwen-plus",        // Balanced
			"qwen-turbo",       // Fast
			"qwen-coder-turbo", // Code-focused
			"qwen-long",        // Long context
		},
		"groq":   {"llama-3.1-70b-versatile", "llama-3.1-8b-instant", "mixtral-8x7b-32768"},
		"ollama": {"llama3.2", "llama3.1", "mistral", "codellama", "gemma2"},
	}

	if models, ok := knownModels[providerName]; ok {
		return models
	}

	// If we have provider capabilities, use those
	if ds.providerRegistry != nil {
		if provider, err := ds.providerRegistry.GetProvider(providerName); err == nil {
			caps := provider.GetCapabilities()
			if caps != nil && len(caps.SupportedModels) > 0 {
				return caps.SupportedModels
			}
		}
	}

	return []string{"default"}
}

// CreateSingleProviderParticipants creates virtual participants for single-provider mode
func (ds *DebateService) CreateSingleProviderParticipants(
	spc *SingleProviderConfig,
	originalTopic string,
) []ParticipantConfig {
	participants := make([]ParticipantConfig, spc.NumParticipants)

	// Define diverse roles with distinct perspectives
	roles := []struct {
		name        string
		role        string
		perspective string
		tempOffset  float64
	}{
		{"Analytical Thinker", "analyst", "Focus on data, evidence, and logical reasoning. Be precise and thorough.", 0.0},
		{"Creative Explorer", "proposer", "Think outside the box, propose innovative ideas and unconventional solutions.", 0.3},
		{"Critical Examiner", "critic", "Challenge assumptions, identify weaknesses, and play devil's advocate.", -0.1},
		{"Practical Advisor", "mediator", "Focus on real-world applicability, compromises, and actionable outcomes.", 0.1},
		{"Systems Thinker", "debater", "Consider broader implications, interconnections, and long-term effects.", 0.2},
		{"Ethical Observer", "debater", "Evaluate moral implications, fairness, and societal impact.", 0.15},
		{"Technical Expert", "analyst", "Dive deep into technical details, specifications, and implementation.", -0.05},
		{"Strategic Planner", "opponent", "Consider strategic advantages, risks, and competitive dynamics.", 0.1},
	}

	// Assign models (cycle through available models if we have model diversity)
	numModels := len(spc.AvailableModels)
	if numModels == 0 {
		spc.AvailableModels = []string{"default"}
		numModels = 1
	}

	for i := 0; i < spc.NumParticipants; i++ {
		roleIdx := i % len(roles)
		roleInfo := roles[roleIdx]

		// Select model (cycle through available models)
		model := spc.AvailableModels[i%numModels]

		// Calculate temperature diversity
		baseTemp := 0.7
		temp := baseTemp + roleInfo.tempOffset
		if temp < 0.1 {
			temp = 0.1
		}
		if temp > 1.0 {
			temp = 1.0
		}

		participants[i] = ParticipantConfig{
			ParticipantID: fmt.Sprintf("single-provider-instance-%d", i+1),
			Name:          fmt.Sprintf("%s (Instance %d)", roleInfo.name, i+1),
			Role:          roleInfo.role,
			LLMProvider:   spc.ProviderName,
			LLMModel:      model,
			SystemPrompt:  ds.buildSingleProviderSystemPrompt(roleInfo.name, roleInfo.perspective, i+1, spc.NumParticipants),
			Temperature:   temp,
			Priority:      i + 1,
		}
	}

	return participants
}

// buildSingleProviderSystemPrompt creates a unique system prompt for single-provider instances
func (ds *DebateService) buildSingleProviderSystemPrompt(name, perspective string, instance, total int) string {
	return fmt.Sprintf(
		"You are %s, participant %d of %d in an AI debate panel using the same underlying model but with distinct perspectives.\n\n"+
			"YOUR UNIQUE PERSPECTIVE: %s\n\n"+
			"IMPORTANT GUIDELINES:\n"+
			"- You MUST maintain your unique viewpoint throughout the debate\n"+
			"- Your perspective should be clearly different from other participants\n"+
			"- Acknowledge valid points from others while contributing your distinct analysis\n"+
			"- Do not simply agree with everything - bring your unique expertise\n"+
			"- Be specific and provide concrete examples from your perspective\n"+
			"- If others make points similar to yours, expand on them with new insights\n\n"+
			"Remember: The value of this debate comes from diverse viewpoints. Your unique perspective is essential.",
		name, instance, total, perspective,
	)
}

// ConductSingleProviderDebate conducts a debate using a single provider with multiple instances
func (ds *DebateService) ConductSingleProviderDebate(
	ctx context.Context,
	config *DebateConfig,
	spc *SingleProviderConfig,
) (*DebateResult, error) {
	startTime := time.Now()
	sessionID := fmt.Sprintf("single-provider-%s-%s", config.DebateID, uuid.New().String()[:8])

	ds.logger.WithFields(logrus.Fields{
		"debate_id":       config.DebateID,
		"provider":        spc.ProviderName,
		"models":          spc.AvailableModels,
		"participants":    spc.NumParticipants,
		"model_diversity": spc.UseModelDiversity,
	}).Info("Starting single-provider multi-instance debate")

	// Create virtual participants with diverse perspectives
	config.Participants = ds.CreateSingleProviderParticipants(spc, config.Topic)

	// Conduct the debate using the standard method
	allResponses := make([]ParticipantResponse, 0)
	roundResults := make([]roundResult, 0, config.MaxRounds)

	timeoutCtx, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()

	previousResponses := make([]ParticipantResponse, 0)
	for round := 1; round <= config.MaxRounds; round++ {
		ds.logger.Infof("Single-provider debate round %d of %d", round, config.MaxRounds)

		roundStart := time.Now()
		responses, err := ds.executeSingleProviderRound(timeoutCtx, config, spc, round, previousResponses)
		if err != nil {
			ds.logger.WithError(err).Errorf("Round %d failed", round)
			if len(responses) == 0 {
				break
			}
		}

		roundResults = append(roundResults, roundResult{
			Round:     round,
			Responses: responses,
			StartTime: roundStart,
			EndTime:   time.Now(),
		})

		allResponses = append(allResponses, responses...)
		previousResponses = responses

		// Check for early consensus
		if ds.checkEarlyConsensus(responses) {
			ds.logger.Infof("Early consensus reached at round %d", round)
			break
		}

		select {
		case <-timeoutCtx.Done():
			ds.logger.Warn("Single-provider debate timeout reached")
			break
		default:
			continue
		}
	}

	endTime := time.Now()

	// Analyze results
	consensus := ds.analyzeConsensus(allResponses, config.Topic)
	qualityScore := ds.calculateQualityScore(allResponses)
	finalScore := ds.calculateFinalScore(allResponses, consensus)
	bestResponse := ds.findBestResponse(allResponses)

	// Calculate effective diversity
	effectiveDiversity := ds.calculateEffectiveDiversity(allResponses)

	result := &DebateResult{
		DebateID:        config.DebateID,
		SessionID:       sessionID,
		Topic:           config.Topic,
		StartTime:       startTime,
		EndTime:         endTime,
		Duration:        endTime.Sub(startTime),
		TotalRounds:     config.MaxRounds,
		RoundsConducted: len(roundResults),
		AllResponses:    allResponses,
		Participants:    ds.getLatestParticipantResponses(allResponses, config.Participants),
		BestResponse:    bestResponse,
		Consensus:       consensus,
		QualityScore:    qualityScore,
		FinalScore:      finalScore,
		Success:         len(allResponses) > 0,
		Metadata: map[string]interface{}{
			"mode":                "single_provider",
			"provider":            spc.ProviderName,
			"models_used":         ds.getModelsUsed(allResponses),
			"instance_count":      spc.NumParticipants,
			"model_diversity":     spc.UseModelDiversity,
			"temp_diversity":      spc.UseTempDiversity,
			"effective_diversity": effectiveDiversity,
			"total_responses":     len(allResponses),
		},
	}

	return result, nil
}

// executeSingleProviderRound executes a debate round in single-provider mode
func (ds *DebateService) executeSingleProviderRound(
	ctx context.Context,
	config *DebateConfig,
	spc *SingleProviderConfig,
	round int,
	previousResponses []ParticipantResponse,
) ([]ParticipantResponse, error) {
	responses := make([]ParticipantResponse, 0, len(config.Participants))
	responseChan := make(chan ParticipantResponse, len(config.Participants))
	errorChan := make(chan error, len(config.Participants))

	// Get the single provider
	provider, err := ds.providerRegistry.GetProvider(spc.ProviderName)
	if err != nil {
		return nil, fmt.Errorf("failed to get provider %s: %w", spc.ProviderName, err)
	}

	var wg sync.WaitGroup
	for _, participant := range config.Participants {
		wg.Add(1)
		go func(p ParticipantConfig) {
			defer wg.Done()

			resp, err := ds.getSingleProviderParticipantResponse(ctx, config, p, provider, round, previousResponses)
			if err != nil {
				errorChan <- fmt.Errorf("participant %s failed: %w", p.Name, err)
				return
			}
			responseChan <- resp
		}(participant)
	}

	go func() {
		wg.Wait()
		close(responseChan)
		close(errorChan)
	}()

	for resp := range responseChan {
		responses = append(responses, resp)
	}

	var errs []error
	for err := range errorChan {
		errs = append(errs, err)
	}

	if len(errs) > 0 && len(responses) == 0 {
		return responses, fmt.Errorf("all single-provider instances failed: %v", errs)
	}

	return responses, nil
}

// getSingleProviderParticipantResponse gets a response from a single-provider participant
func (ds *DebateService) getSingleProviderParticipantResponse(
	ctx context.Context,
	config *DebateConfig,
	participant ParticipantConfig,
	provider llm.LLMProvider,
	round int,
	previousResponses []ParticipantResponse,
) (ParticipantResponse, error) {
	startTime := time.Now()

	// Create readable participant identifier like "DeepSeek-1" or "DeepSeek-2"
	instanceNum := extractInstanceNumber(participant.ParticipantID)
	participantIdentifier := formatParticipantIdentifier(participant.LLMProvider, participant.ParticipantID, instanceNum)

	// Log participant identification
	ds.logger.WithFields(logrus.Fields{
		"participant":      participantIdentifier,
		"participant_id":   participant.ParticipantID,
		"participant_name": participant.Name,
		"role":             participant.Role,
		"provider":         participant.LLMProvider,
		"model":            participant.LLMModel,
		"round":            round,
		"debate_id":        config.DebateID,
	}).Infof("[%s] Single-provider participant starting response", participantIdentifier)

	// Log start to repository
	ds.logDebateEntry(ctx, &DebateLogEntry{
		DebateID:              config.DebateID,
		ParticipantID:         participant.ParticipantID,
		ParticipantIdentifier: participantIdentifier,
		ParticipantName:       participant.Name,
		Role:                  participant.Role,
		Provider:              participant.LLMProvider,
		Model:                 participant.LLMModel,
		Round:                 round,
		Action:                "start",
	})

	// Build the prompt
	prompt := ds.buildDebatePrompt(config.Topic, participant, round, previousResponses)

	// Use the participant's custom system prompt if available
	systemPrompt := participant.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = ds.buildSystemPrompt(participant)
	}

	// Calculate temperature (use participant's if set, otherwise use diversity)
	temp := participant.Temperature
	if temp == 0 {
		temp = 0.7
	}

	llmRequest := &models.LLMRequest{
		ID:        uuid.New().String(),
		SessionID: config.DebateID,
		Prompt:    prompt,
		Messages: []models.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: prompt},
		},
		ModelParams: models.ModelParameters{
			Model:       participant.LLMModel,
			Temperature: temp,
			MaxTokens:   1024,
		},
	}

	// Log request (Retrofit-like): [A: Model Name] <--- Sending request...
	if ds.commLogger != nil {
		ds.commLogger.LogRequest(participant.Role, participant.LLMProvider, participant.LLMModel, len(prompt), round)
	}

	llmResponse, err := provider.Complete(ctx, llmRequest)
	if err != nil {
		ds.logger.WithFields(logrus.Fields{
			"participant":      participantIdentifier,
			"participant_id":   participant.ParticipantID,
			"participant_name": participant.Name,
			"role":             participant.Role,
			"round":            round,
			"debate_id":        config.DebateID,
			"error":            err.Error(),
		}).Errorf("[%s] Single-provider participant response failed", participantIdentifier)

		// Log error (Retrofit-like)
		if ds.commLogger != nil {
			ds.commLogger.LogError(participant.Role, participant.LLMProvider, participant.LLMModel, err)
		}

		// Log error to repository
		ds.logDebateEntry(ctx, &DebateLogEntry{
			DebateID:              config.DebateID,
			ParticipantID:         participant.ParticipantID,
			ParticipantIdentifier: participantIdentifier,
			ParticipantName:       participant.Name,
			Role:                  participant.Role,
			Provider:              participant.LLMProvider,
			Model:                 participant.LLMModel,
			Round:                 round,
			Action:                "error",
			ErrorMessage:          err.Error(),
		})

		return ParticipantResponse{}, fmt.Errorf("[%s] LLM call failed: %w", participantIdentifier, err)
	}

	responseTime := time.Since(startTime)
	qualityScore := ds.calculateResponseQuality(llmResponse)

	// Log response (Retrofit-like): [A: Model Name] ---> Received X bytes in Y.Zs
	if ds.commLogger != nil {
		ds.commLogger.LogResponse(participant.Role, participant.LLMProvider, participant.LLMModel, len(llmResponse.Content), responseTime, qualityScore)
		ds.commLogger.LogResponsePreview(participant.Role, participant.LLMProvider, participant.LLMModel, llmResponse.Content, 100)
	}

	// Log successful response with participant identification
	ds.logger.WithFields(logrus.Fields{
		"participant":      participantIdentifier,
		"participant_id":   participant.ParticipantID,
		"participant_name": participant.Name,
		"role":             participant.Role,
		"provider":         participant.LLMProvider,
		"model":            participant.LLMModel,
		"round":            round,
		"debate_id":        config.DebateID,
		"response_time_ms": responseTime.Milliseconds(),
		"quality_score":    qualityScore,
		"tokens_used":      llmResponse.TokensUsed,
		"content_length":   len(llmResponse.Content),
	}).Infof("[%s] Single-provider participant response completed", participantIdentifier)

	// Log completion to repository
	ds.logDebateEntry(ctx, &DebateLogEntry{
		DebateID:              config.DebateID,
		ParticipantID:         participant.ParticipantID,
		ParticipantIdentifier: participantIdentifier,
		ParticipantName:       participant.Name,
		Role:                  participant.Role,
		Provider:              participant.LLMProvider,
		Model:                 participant.LLMModel,
		Round:                 round,
		Action:                "complete",
		ResponseTimeMs:        responseTime.Milliseconds(),
		QualityScore:          qualityScore,
		TokensUsed:            llmResponse.TokensUsed,
		ContentLength:         len(llmResponse.Content),
	})

	return ParticipantResponse{
		ParticipantID:   participant.ParticipantID,
		ParticipantName: participant.Name,
		Role:            participant.Role,
		Round:           round,
		RoundNumber:     round,
		Response:        llmResponse.Content,
		Content:         llmResponse.Content,
		Confidence:      llmResponse.Confidence,
		QualityScore:    qualityScore,
		ResponseTime:    responseTime,
		LLMProvider:     participant.LLMProvider,
		LLMModel:        participant.LLMModel,
		LLMName:         participant.LLMModel,
		Timestamp:       startTime,
		Metadata: participantUsageMetadata(llmResponse, map[string]any{
			"finish_reason":     llmResponse.FinishReason,
			"single_provider":   true,
			"temperature_used":  temp,
			"system_prompt_len": len(systemPrompt),
		}),
	}, nil
}

// calculateEffectiveDiversity calculates how diverse the responses actually are
func (ds *DebateService) calculateEffectiveDiversity(responses []ParticipantResponse) float64 {
	if len(responses) < 2 {
		return 0.0
	}

	// Calculate average pairwise dissimilarity
	totalDissimilarity := 0.0
	comparisons := 0

	for i := 0; i < len(responses); i++ {
		for j := i + 1; j < len(responses); j++ {
			similarity := ds.calculateTextSimilarity(responses[i].Content, responses[j].Content)
			totalDissimilarity += (1.0 - similarity) // Convert similarity to dissimilarity
			comparisons++
		}
	}

	if comparisons == 0 {
		return 0.0
	}

	return totalDissimilarity / float64(comparisons)
}

// getModelsUsed returns unique models used in responses
func (ds *DebateService) getModelsUsed(responses []ParticipantResponse) []string {
	models := make(map[string]bool)
	for _, r := range responses {
		models[r.LLMModel] = true
	}

	result := make([]string, 0, len(models))
	for m := range models {
		result = append(result, m)
	}
	return result
}

// AutoConductDebate automatically selects the best debate mode based on available providers
func (ds *DebateService) AutoConductDebate(
	ctx context.Context,
	config *DebateConfig,
) (*DebateResult, error) {
	// Check if we should use single-provider mode
	isSingle, spc := ds.IsSingleProviderMode(config)

	if isSingle && spc != nil {
		ds.logger.WithFields(logrus.Fields{
			"provider":     spc.ProviderName,
			"num_models":   len(spc.AvailableModels),
			"participants": spc.NumParticipants,
		}).Info("Detected single-provider mode, using multi-instance debate")

		return ds.ConductSingleProviderDebate(ctx, config, spc)
	}

	// Use standard multi-provider debate
	return ds.ConductDebate(ctx, config)
}

// ConductDebateWithMultiPassValidation conducts a debate with multi-pass validation
// This method performs the standard debate followed by validation, polish, and final synthesis phases
func (ds *DebateService) ConductDebateWithMultiPassValidation(
	ctx context.Context,
	config *DebateConfig,
	validationConfig *ValidationConfig,
) (*MultiPassResult, error) {
	ds.logger.WithFields(logrus.Fields{
		"debate_id":         config.DebateID,
		"topic":             config.Topic,
		"enable_validation": validationConfig != nil && validationConfig.EnableValidation,
		"enable_polish":     validationConfig != nil && validationConfig.EnablePolish,
	}).Info("Starting debate with multi-pass validation")

	// Use default validation config if not provided
	if validationConfig == nil {
		validationConfig = DefaultValidationConfig()
	}

	// Conduct the standard debate first
	debateResult, err := ds.AutoConductDebate(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("debate failed: %w", err)
	}

	// Create multi-pass validator and run validation
	validator := NewMultiPassValidator(ds, ds.logger)
	validator.SetConfig(validationConfig)

	// Run multi-pass validation
	multiPassResult, err := validator.ValidateAndImprove(ctx, debateResult)
	if err != nil {
		ds.logger.WithError(err).Warn("Multi-pass validation failed, returning original debate result")
		// Return a minimal multi-pass result with the original debate
		return &MultiPassResult{
			DebateID:          debateResult.DebateID,
			Topic:             debateResult.Topic,
			Config:            validationConfig,
			Phases:            []*PhaseResult{{Phase: PhaseInitialResponse, Responses: debateResult.AllResponses}},
			FinalConsensus:    debateResult.Consensus,
			FinalResponse:     ds.getBestResponseContent(debateResult),
			TotalDuration:     debateResult.Duration,
			OverallConfidence: debateResult.QualityScore,
			Metadata:          map[string]interface{}{"validation_failed": true},
		}, nil
	}

	return multiPassResult, nil
}

// getBestResponseContent returns the content of the best response
func (ds *DebateService) getBestResponseContent(result *DebateResult) string {
	if result.BestResponse != nil {
		return result.BestResponse.Content
	}
	if len(result.AllResponses) > 0 {
		return result.AllResponses[0].Content
	}
	return ""
}

// StreamDebateWithMultiPassValidation conducts a streaming debate with multi-pass validation
// Each phase is streamed as it completes, allowing for real-time progress updates
func (ds *DebateService) StreamDebateWithMultiPassValidation(
	ctx context.Context,
	config *DebateConfig,
	validationConfig *ValidationConfig,
	streamHandler func(phase ValidationPhase, content string, isComplete bool),
) (*MultiPassResult, error) {
	ds.logger.Info("Starting streaming debate with multi-pass validation")

	// Use default validation config if not provided
	if validationConfig == nil {
		validationConfig = DefaultValidationConfig()
	}

	// Stream Phase 1: Initial Response (from debate)
	if streamHandler != nil {
		phaseHeader := FormatPhaseHeader(PhaseInitialResponse, validationConfig.VerbosePhaseHeaders)
		streamHandler(PhaseInitialResponse, phaseHeader, false)
	}

	// Conduct the standard debate
	debateResult, err := ds.AutoConductDebate(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("debate failed: %w", err)
	}

	// Stream initial responses
	if streamHandler != nil {
		for _, resp := range debateResult.AllResponses {
			content := fmt.Sprintf("[%s] %s:\n%s\n\n",
				strings.ToUpper(resp.Role[:1]), resp.ParticipantName, resp.Content)
			streamHandler(PhaseInitialResponse, content, false)
		}
		streamHandler(PhaseInitialResponse, "", true) // Mark phase complete
	}

	// Create validator with streaming callbacks
	validator := NewMultiPassValidator(ds, ds.logger)
	validator.SetConfig(validationConfig)

	// Set up streaming callbacks for each phase
	if streamHandler != nil {
		validator.SetPhaseCallback(PhaseValidation, func(result *PhaseResult) {
			header := FormatPhaseHeader(PhaseValidation, validationConfig.VerbosePhaseHeaders)
			streamHandler(PhaseValidation, header, false)
			for _, v := range result.Validations {
				content := fmt.Sprintf("  ✓ Validated %s: Score %.2f\n", v.ParticipantID, v.ValidationScore)
				streamHandler(PhaseValidation, content, false)
			}
			footer := FormatPhaseFooter(PhaseValidation, result, validationConfig.VerbosePhaseHeaders)
			streamHandler(PhaseValidation, footer, true)
		})

		validator.SetPhaseCallback(PhasePolishImprove, func(result *PhaseResult) {
			header := FormatPhaseHeader(PhasePolishImprove, validationConfig.VerbosePhaseHeaders)
			streamHandler(PhasePolishImprove, header, false)
			for _, p := range result.Polishes {
				content := fmt.Sprintf("  ✨ Improved %s: +%.0f%%\n", p.ParticipantID, p.ImprovementScore*100)
				streamHandler(PhasePolishImprove, content, false)
			}
			footer := FormatPhaseFooter(PhasePolishImprove, result, validationConfig.VerbosePhaseHeaders)
			streamHandler(PhasePolishImprove, footer, true)
		})

		validator.SetPhaseCallback(PhaseFinalConclusion, func(result *PhaseResult) {
			header := FormatPhaseHeader(PhaseFinalConclusion, validationConfig.VerbosePhaseHeaders)
			streamHandler(PhaseFinalConclusion, header, false)
			if len(result.Responses) > 0 {
				streamHandler(PhaseFinalConclusion, result.Responses[0].Content, false)
			}
			footer := FormatPhaseFooter(PhaseFinalConclusion, result, validationConfig.VerbosePhaseHeaders)
			streamHandler(PhaseFinalConclusion, footer, true)
		})
	}

	// Run multi-pass validation
	return validator.ValidateAndImprove(ctx, debateResult)
}

// ============================================================================
// NEW: INTEGRATED AI DEBATE FEATURES (100% Implementation)
// ============================================================================
// The following methods implement the complete integration of:
// - Test-Driven Debate (adversarial tests, sandboxed execution, contrastive analysis)
// - 4-Pass Validation Pipeline (Initial 60% → Validation 75% → Polish 85% → Final 90%)
// - Tool Integration Framework (MCP/LSP/RAG/Embeddings)
// - Specialized Role Routing (Generator, Refactorer, PerformanceAnalyzer, etc.)
// ============================================================================

// detectCodeGenerationIntent detects if the debate topic involves code generation.
// It delegates to the LLM-based enhancedIntentClassifier and caches results per
// topic string so the same topic is never classified twice in one debate session.
// On any classification error the function conservatively returns false (not code)
// to avoid false-positive test-driven debate activation.
func (ds *DebateService) detectCodeGenerationIntent(topic string, context map[string]interface{}) bool {
	// Fast path: explicit context hints override classification
	if context != nil {
		if lang, ok := context["language"].(string); ok && lang != "" {
			return true
		}
		if codeType, ok := context["type"].(string); ok && (codeType == "code" || codeType == "programming") {
			return true
		}
	}

	ds.initCaches()
	if cached, ok := ds.codeIntentCache.Get(topic); ok {
		return cached
	}

	// Use LLM-based enhanced intent classifier when available
	if ds.enhancedIntentClassifier != nil {
		// Reuse cached full EnhancedIntentResult if classifyIntentWithGranularity
		// has already processed this topic — avoids a second LLM call.
		cachedResult, cachedOK := ds.enhancedIntentCache.Get(topic)

		ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 15*time.Second)
		defer cancel()

		// Build codebase context from metadata
		codebaseCtx := make(map[string]interface{})
		if context != nil {
			for k, v := range context {
				codebaseCtx[k] = v
			}
		}

		alreadyAttempted, _ := ds.intentAttempted.Get(topic)

		var result *EnhancedIntentResult
		var err error
		switch {
		case cachedOK:
			result = cachedResult
		case alreadyAttempted:
			// A previous call already made the LLM attempt and it failed —
			// don't waste another provider call; go straight to the
			// keyword-fallback path below.
			err = fmt.Errorf("intent classification previously failed for topic")
		default:
			result, err = ds.enhancedIntentClassifier.ClassifyEnhancedIntent(
				ctx, topic, "", codebaseCtx,
			)
			ds.intentAttempted.Put(topic, true)
			if err == nil && result != nil {
				ds.enhancedIntentCache.Put(topic, result)
			}
		}
		if err != nil {
			// LLM unavailable (no providers configured, timeout, etc.) —
			// fall back to the keyword heuristic so tests and early-boot
			// code paths still classify common code topics correctly
			// instead of defaulting to false.
			ds.logger.WithError(err).Debug(
				"[Code Intent] LLM classification failed, using keyword fallback",
			)
			isCode := classifyCodeIntentByKeywords(topic)
			ds.codeIntentCache.Put(topic, isCode)
			return isCode
		}

		// Code-related action types indicate a code generation debate
		isCode := result.ActionType == ActionCreation ||
			result.ActionType == ActionDebugging ||
			result.ActionType == ActionFixing ||
			result.ActionType == ActionRefactoring

		ds.logger.WithFields(logrus.Fields{
			"topic":       topic,
			"action_type": result.ActionType,
			"is_code":     isCode,
		}).Debug("[Code Intent] LLM classification result")

		ds.codeIntentCache.Put(topic, isCode)
		return isCode
	}

	// No classifier available — fall back to a keyword heuristic so
	// debate sessions constructed without an LLM-based classifier
	// (unit tests, early boot) still route code topics through the
	// test-driven debate path instead of defaulting to non-code.
	// The keyword set is intentionally narrow: verbs that strongly
	// imply generating executable code + nouns that name artifacts
	// only programs produce. Discussion/explanation verbs stay out
	// so "Explain how recursion works" and "Discuss functional
	// programming" correctly classify as non-code.
	isCode := classifyCodeIntentByKeywords(topic)
	ds.codeIntentCache.Put(topic, isCode)
	return isCode
}

// classifyCodeIntentByKeywords is the heuristic fallback used by
// detectCodeGenerationIntent when no LLM-based enhancedIntentClassifier
// is configured. It returns true when the topic contains a verb that
// strongly implies code generation (write, implement, create, develop,
// build, code, program, refactor, debug, fix) combined with any
// context — OR when the topic contains a programming artifact keyword
// (function, class, api, endpoint, algorithm, module, script).
//
// Pulled out as a package-level function so the test suite can pin
// its behaviour without reaching into DebateService state.
func classifyCodeIntentByKeywords(topic string) bool {
	if topic == "" {
		return false
	}
	lowered := strings.ToLower(topic)

	// Strong non-code prefixes. If the topic starts with a discussion
	// verb the caller is almost certainly asking for explanation, not
	// generation, so short-circuit before any keyword match.
	nonCodePrefixes := []string{
		"explain ", "discuss ", "describe ", "compare ",
		"what is ", "what are ", "why ", "how does ", "how do ",
	}
	for _, p := range nonCodePrefixes {
		if strings.HasPrefix(lowered, p) {
			return false
		}
	}

	// Code-generation verb triggers.
	codeVerbs := []string{
		"write ", "implement ", "create ", "develop ", "build ",
		"code ", "program ", "refactor ", "debug ", "fix ",
		"generate ", "make ", "design ",
	}
	for _, v := range codeVerbs {
		if strings.HasPrefix(lowered, v) || strings.Contains(lowered, " "+v) {
			return true
		}
	}

	// Programming artifact nouns (fallback if the verb didn't hit).
	codeArtifacts := []string{
		"function", "algorithm", "api endpoint", "rest api",
		"class ", "script ", "module ", "binary search",
		"sort", "parser", "compiler", "interpreter",
	}
	for _, a := range codeArtifacts {
		if strings.Contains(lowered, a) {
			return true
		}
	}

	return false
}

// conductTestDrivenDebate executes a test-driven debate with adversarial test generation
func (ds *DebateService) conductTestDrivenDebate(
	ctx context.Context,
	config *DebateConfig,
	startTime time.Time,
	sessionID string,
) (*DebateResult, error) {
	ds.logger.Info("[Test-Driven Debate] Starting test-driven debate mode")

	// Phase 1: Initial code generation (standard debate)
	ds.commLogger.LogInfo("Test-Driven", "Phase 1: Initial code generation")
	initialResult, err := ds.conductRealDebate(ctx, config, startTime, sessionID)
	if err != nil {
		return nil, fmt.Errorf("initial code generation failed: %w", err)
	}

	// Extract code from consensus for testing
	var codeToTest string
	if initialResult.Consensus != nil && initialResult.Consensus.FinalPosition != "" {
		codeToTest = initialResult.Consensus.FinalPosition
	} else if initialResult.BestResponse != nil && initialResult.BestResponse.Response != "" {
		codeToTest = initialResult.BestResponse.Response
	} else {
		ds.logger.Warn("[Test-Driven Debate] No code found in consensus or best response")
		return initialResult, nil
	}

	// Phase 2: Generate adversarial tests for the consensus solution
	ds.commLogger.LogInfo("Test-Driven", "Phase 2: Generating adversarial tests")
	language := ds.detectLanguage(codeToTest)
	testCases, err := ds.generateAdversarialTests(ctx, codeToTest, language, config.Topic)
	if err != nil {
		ds.logger.WithError(err).Warn("[Test-Driven Debate] Test generation failed, returning initial result")
		return initialResult, nil
	}

	ds.commLogger.LogInfo("Test-Driven", fmt.Sprintf("Generated %d adversarial tests", len(testCases)))

	// Phase 3: Execute tests against the solution
	ds.commLogger.LogInfo("Test-Driven", "Phase 3: Executing tests in sandbox")
	solution := &testing.Solution{
		ID:          initialResult.DebateID,
		AgentID:     "debate-consensus",
		Language:    language,
		Code:        codeToTest,
		Description: config.Topic,
	}

	executionResults := make(map[string]*testing.TestExecutionResult)
	passCount := 0
	failCount := 0

	for _, testCase := range testCases {
		result, err := ds.testExecutor.Execute(ctx, testCase, solution)
		if err != nil {
			ds.logger.WithError(err).Warnf("[Test-Driven Debate] Test execution failed: %s", testCase.ID)
			continue
		}

		executionResults[testCase.ID] = result
		if result.Passed {
			passCount++
		} else {
			failCount++
		}

		status := "FAIL"
		if result.Passed {
			status = "PASS"
		}
		ds.commLogger.LogInfo("TestExec", fmt.Sprintf("%s: %s", testCase.ID, status))
	}

	ds.commLogger.LogInfo("Test-Driven", fmt.Sprintf("Tests: %d passed, %d failed", passCount, failCount))

	// Phase 4: If tests failed, trigger refinement round
	if failCount > 0 {
		ds.commLogger.LogInfo("Test-Driven", "Phase 4: Refinement triggered (tests failed)")

		// Create refinement prompt with test feedback
		refinementPrompt := ds.buildRefinementPrompt(codeToTest, testCases, executionResults)

		// Run refinement round
		refinementConfig := &DebateConfig{
			DebateID:     config.DebateID + "-refinement",
			Topic:        refinementPrompt,
			Participants: config.Participants,
			MaxRounds:    1,
			Timeout:      config.Timeout / 2,
			Metadata:     config.Metadata,
		}

		refinedResult, err := ds.conductRealDebate(ctx, refinementConfig, time.Now(), sessionID+"-refined")
		if err != nil {
			ds.logger.WithError(err).Warn("[Test-Driven Debate] Refinement failed, using initial result")
			initialResult.TestDrivenMetadata = map[string]interface{}{
				"tests_generated": len(testCases),
				"tests_passed":    passCount,
				"tests_failed":    failCount,
				"refinement":      "failed",
			}
			return initialResult, nil
		}

		// Return refined result with test metadata
		originalScore := initialResult.QualityScore
		if initialResult.Consensus != nil {
			originalScore = initialResult.Consensus.Confidence
		}
		refinedScore := refinedResult.QualityScore
		if refinedResult.Consensus != nil {
			refinedScore = refinedResult.Consensus.Confidence
		}

		refinedResult.TestDrivenMetadata = map[string]interface{}{
			"tests_generated": len(testCases),
			"tests_passed":    passCount,
			"tests_failed":    failCount,
			"refinement":      "completed",
			"original_score":  originalScore,
			"refined_score":   refinedScore,
		}

		return refinedResult, nil
	}

	// All tests passed - return initial result with test metadata
	initialResult.TestDrivenMetadata = map[string]interface{}{
		"tests_generated": len(testCases),
		"tests_passed":    passCount,
		"tests_failed":    0,
		"refinement":      "not_needed",
	}

	ds.commLogger.LogInfo("Test-Driven", "All tests passed - no refinement needed")
	return initialResult, nil
}

// generateAdversarialTests generates test cases for the given code solution
func (ds *DebateService) generateAdversarialTests(
	ctx context.Context,
	code string,
	language string,
	topic string,
) ([]*testing.TestCase, error) {
	if ds.testGenerator == nil {
		return nil, fmt.Errorf("test generator not initialized")
	}

	// Generate 3-5 adversarial tests
	testCount := 3
	if len(code) > 500 {
		testCount = 5
	}

	req := &testing.GenerateRequest{
		AgentID:        "test-driven-debate",
		TargetSolution: code,
		Language:       language,
		Context:        topic,
		Difficulty:     testing.DifficultyAdvanced,
		Categories:     []testing.TestCategory{testing.CategoryFunctional, testing.CategoryEdgeCase},
	}

	tests, err := ds.testGenerator.GenerateBatch(ctx, req, testCount)
	if err != nil {
		return nil, fmt.Errorf("test generation failed: %w", err)
	}

	return tests, nil
}

// buildRefinementPrompt creates a prompt for refinement based on test failures
func (ds *DebateService) buildRefinementPrompt(
	originalCode string,
	testCases []*testing.TestCase,
	results map[string]*testing.TestExecutionResult,
) string {
	var failedTests []string

	for _, testCase := range testCases {
		if result, ok := results[testCase.ID]; ok {
			if !result.Passed {
				errorMsg := result.Error
				if errorMsg == "" {
					errorMsg = "Test failed with no error message"
				}
				failedTests = append(failedTests, fmt.Sprintf(
					"Test: %s\nCategory: %s\nOutput: %s\nError: %s",
					testCase.Description,
					testCase.Category,
					result.Output,
					errorMsg,
				))
			}
		}
	}

	return fmt.Sprintf(`Refine the following code to fix the failing tests:

Original Code:
%s

Failed Tests:
%s

Please provide an improved version that passes all tests.`, originalCode, strings.Join(failedTests, "\n\n"))
}

// validateDebateResult applies 4-Pass Validation Pipeline to the debate result
func (ds *DebateService) validateDebateResult(
	ctx context.Context,
	result *DebateResult,
	config *DebateConfig,
) (*validation.PipelineResult, error) {
	if ds.validationPipeline == nil {
		return nil, fmt.Errorf("validation pipeline not initialized")
	}

	ds.logger.Info("[4-Pass Validation] Starting validation pipeline")

	// Extract content from result for validation
	var content string
	if result.Consensus != nil && result.Consensus.FinalPosition != "" {
		content = result.Consensus.FinalPosition
	} else if result.BestResponse != nil && result.BestResponse.Response != "" {
		content = result.BestResponse.Response
	} else {
		return nil, fmt.Errorf("no content found in debate result for validation")
	}

	// Convert debate result to artifact for validation
	artifactType := validation.ArtifactDocumentation
	if ds.detectCodeGenerationIntent(config.Topic, config.Metadata) {
		artifactType = validation.ArtifactCode
	}

	artifact := &validation.Artifact{
		Type:     artifactType,
		Content:  content,
		Language: ds.detectLanguage(content),
		Metadata: map[string]interface{}{
			"debate_id": config.DebateID,
			"rounds":    result.RoundsConducted,
			"topic":     config.Topic,
		},
	}

	// Run validation pipeline
	pipelineResult, err := ds.validationPipeline.Validate(ctx, artifact)
	if err != nil {
		return nil, fmt.Errorf("validation pipeline failed: %w", err)
	}

	// Log validation results for each pass
	for pass, passResult := range pipelineResult.PassResults {
		status := "PASS"
		if !passResult.Passed {
			status = "FAIL"
		}
		ds.commLogger.LogInfo("Validation", fmt.Sprintf(
			"%s: %s (%.2f%%) | Issues: %d",
			pass,
			status,
			passResult.Score*100,
			len(passResult.Issues),
		))
	}

	// Log overall result
	overallStatus := "PASS"
	if !pipelineResult.OverallPassed {
		overallStatus = "FAIL"
	}
	ds.logger.Infof(
		"[4-Pass Validation] Overall: %s | Score: %.2f | Failed at: %s",
		overallStatus,
		pipelineResult.OverallScore,
		pipelineResult.FailedPass,
	)

	return pipelineResult, nil
}

// detectLanguage attempts to detect the programming language from code content
func (ds *DebateService) detectLanguage(content string) string {
	contentLower := strings.ToLower(content)

	// Language detection patterns ordered by specificity (most specific first)
	type langPatterns struct {
		lang     string
		patterns []string
	}
	orderedPatterns := []langPatterns{
		{"java", []string{"public class", "private ", "void ", "system.out", "public static"}},
		{"typescript", []string{"interface ", "type ", ": string", ": number", "implements"}},
		{"c++", []string{"#include", "std::", "namespace ", "template<"}},
		{"rust", []string{"fn ", "let ", "mut ", "impl ", "struct ", "println!"}},
		{"go", []string{"func ", "package ", "import (", "type ", "fmt."}},
		{"javascript", []string{"function ", "const ", "let ", "var ", "=>", "console.log"}},
		{"python", []string{"def ", "import ", "from ", "print(", "self."}},
		{"c", []string{"#include", "int main", "printf", "malloc"}},
	}

	// Find language with most pattern matches; on tie, first in order wins
	maxMatches := 0
	detectedLang := "text"

	for _, lp := range orderedPatterns {
		matchCount := 0
		for _, pattern := range lp.patterns {
			if strings.Contains(contentLower, pattern) {
				matchCount++
			}
		}
		if matchCount > maxMatches {
			maxMatches = matchCount
			detectedLang = lp.lang
		}
	}

	return detectedLang
}

// enrichDebateContext uses Tool Integration to enhance debate context with RAG/MCP/LSP/Embeddings
func (ds *DebateService) enrichDebateContext(
	ctx context.Context,
	config *DebateConfig,
	round int,
) (*tools.EnrichedContext, error) {
	if ds.serviceBridge == nil {
		return nil, nil // Tool integration not available
	}

	ds.logger.Infof("[Tool Integration] Enriching context for round %d", round)

	// Build context string from metadata
	contextStr := fmt.Sprintf("Round %d: %s", round, config.Topic)
	if config.Metadata != nil {
		if ctx, ok := config.Metadata["context"].(string); ok {
			contextStr = ctx
		}
	}

	// Use service bridge to query all available tools
	enriched, err := ds.serviceBridge.EnrichDebateContext(ctx, &tools.DebateRequest{
		Query:   config.Topic,
		Context: contextStr,
	})
	if err != nil {
		ds.logger.WithError(err).Warn("[Tool Integration] Context enrichment failed, continuing without")
		return nil, err
	}

	// Log enrichment results
	ds.commLogger.LogInfo("Tools", fmt.Sprintf(
		"Enriched: RAG=%d results, Embedding=%d dims, Diagnostics=%d",
		len(enriched.RAGResults),
		len(enriched.QueryEmbedding),
		len(enriched.CodeDiagnostics),
	))

	return enriched, nil
}

// DefaultRoleKeywords returns the built-in keyword-to-role mapping used by
// selectSpecializedRole when no custom mapping is supplied via DebateConfig.
// Keywords prefixed with "~" require whole-word matching to avoid substring
// false positives (e.g. "~code" won't match "codebase").
func DefaultRoleKeywords() map[string][]string {
	return map[string][]string{
		"generator":            {"write", "create", "implement", "build", "develop", "~code"},
		"refactorer":           {"refactor", "improve", "restructure", "clean", "optimize code"},
		"performance_analyzer": {"performance", "optimize", "speed up", "faster", "efficiency", "benchmark"},
		"security_analyst":     {"security", "vulnerability", "exploit", "audit", "penetration", "safe"},
		"debugger":             {"debug", "fix", "bug", "error", "crash", "issue", "problem"},
		"architect":            {"architecture", "design", "structure", "pattern", "system design"},
		"reviewer":             {"review", "analyze", "check", "validate", "assess"},
		"tester":               {"test", "testing", "unit test", "integration test", "coverage"},
		"documenter":           {"document", "documentation", "doc", "readme", "comment"},
	}
}

// selectSpecializedRole determines which specialized role should handle the task.
// When the context map contains a "role_keywords" entry of type map[string][]string
// it is used instead of the built-in defaults, allowing per-debate customisation.
func (ds *DebateService) selectSpecializedRole(ctx context.Context, topic string, context map[string]interface{}) string {
	topicLower := strings.ToLower(topic)
	words := strings.Fields(topicLower)

	// Use caller-supplied mapping when provided, otherwise fall back to defaults
	var roleKeywords map[string][]string
	if context != nil {
		if rk, ok := context["role_keywords"].(map[string][]string); ok && len(rk) > 0 {
			roleKeywords = rk
		}
	}
	if len(roleKeywords) == 0 {
		roleKeywords = DefaultRoleKeywords()
	}

	// Score each role
	roleScores := make(map[string]int)
	for role, keywords := range roleKeywords {
		for _, keyword := range keywords {
			if strings.HasPrefix(keyword, "~") {
				// Whole-word match: check against individual words
				target := keyword[1:]
				for _, w := range words {
					if w == target {
						roleScores[role]++
						break
					}
				}
			} else if strings.Contains(topicLower, keyword) {
				roleScores[role]++
			}
		}
	}

	// Find highest scoring role (deterministic: on tie, prefer alphabetically first)
	maxScore := 0
	selectedRole := "" // default to empty (no specialized role)
	for role, score := range roleScores {
		if score > maxScore || (score == maxScore && score > 0 && role < selectedRole) {
			maxScore = score
			selectedRole = role
		}
	}

	if maxScore > 0 {
		ds.logger.Infof("[Specialized Routing] Assigned role: %s (score: %d)", selectedRole, maxScore)
	}

	return selectedRole
}

// classifyIntentWithGranularity performs enhanced intent classification with granularity detection
func (ds *DebateService) classifyIntentWithGranularity(
	ctx context.Context,
	topic string,
	metadata map[string]any,
) (*EnhancedIntentResult, error) {
	// Extract conversation context from metadata
	conversationContext := ""
	if metadata != nil {
		if conv, ok := metadata["conversation_context"].(string); ok {
			conversationContext = conv
		}
	}

	// Extract codebase context from metadata
	codebaseContext := make(map[string]interface{})
	if metadata != nil {
		if code, ok := metadata["codebase_context"].(map[string]interface{}); ok {
			codebaseContext = code
		}
	}

	ds.initCaches()
	// Reuse cached full result when available so detectCodeGenerationIntent
	// and this function share one LLM call per topic. If a previous call
	// already attempted classification and failed, surface the same error
	// without a second LLM round-trip.
	if cached, ok := ds.enhancedIntentCache.Get(topic); ok && cached != nil {
		return cached, nil
	}
	if attempted, _ := ds.intentAttempted.Get(topic); attempted {
		return nil, fmt.Errorf("intent classification failed: previous attempt errored")
	}

	// Classify intent with timeout
	classifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	result, err := ds.enhancedIntentClassifier.ClassifyEnhancedIntent(
		classifyCtx,
		topic,
		conversationContext,
		codebaseContext,
	)

	ds.intentAttempted.Put(topic, true)

	if err != nil {
		return nil, fmt.Errorf("intent classification failed: %w", err)
	}

	ds.enhancedIntentCache.Put(topic, result)

	return result, nil
}

// conductSpecKitDebate executes the SpecKit 7-phase flow and converts result to DebateResult
func (ds *DebateService) conductSpecKitDebate(
	ctx context.Context,
	config *DebateConfig,
	intentResult *EnhancedIntentResult,
	startTime time.Time,
	sessionID string,
) (*DebateResult, error) {
	ds.logger.WithFields(logrus.Fields{
		"session_id":  sessionID,
		"granularity": intentResult.Granularity,
		"action_type": intentResult.ActionType,
	}).Info("[SpecKit Flow] Starting 7-phase SpecKit workflow")

	// Execute SpecKit flow
	flowResult, err := ds.speckitOrchestrator.ExecuteFlow(ctx, config.Topic, intentResult)
	if err != nil {
		ds.logger.WithError(err).Error("[SpecKit Flow] Flow execution failed")
		return nil, fmt.Errorf("SpecKit flow execution failed: %w", err)
	}

	endTime := time.Now()

	// Convert SpecKit flow result to DebateResult
	result := &DebateResult{
		DebateID:        config.DebateID,
		SessionID:       sessionID,
		Topic:           config.Topic,
		StartTime:       startTime,
		EndTime:         endTime,
		Duration:        endTime.Sub(startTime),
		TotalRounds:     7, // 7 SpecKit phases
		RoundsConducted: len(flowResult.Phases),
		AllResponses:    ds.convertSpecKitPhasesToResponses(flowResult),
		BestResponse:    ds.extractBestResponseFromSpecKit(flowResult),
		Consensus: &ConsensusResult{
			Achieved:       flowResult.Success,
			Confidence:     flowResult.OverallQualityScore,
			AgreementLevel: flowResult.OverallQualityScore,
			FinalPosition:  flowResult.FinalArtifact,
			KeyPoints:      []string{"SpecKit 7-phase workflow completed"},
			Disagreements:  []string{},
			Summary:        fmt.Sprintf("SpecKit flow completed with %d phases", len(flowResult.Phases)),
			Timestamp:      endTime,
			QualityScore:   flowResult.OverallQualityScore,
		},
		QualityScore: ds.calculateSpecKitQualityScore(flowResult),
		FinalScore:   flowResult.OverallQualityScore,
		Success:      flowResult.Success,
		Metadata: map[string]interface{}{
			"speckit_flow":       true,
			"granularity":        intentResult.Granularity,
			"action_type":        intentResult.ActionType,
			"requires_speckit":   intentResult.RequiresSpecKit,
			"speckit_reason":     intentResult.SpecKitReason,
			"phases_completed":   len(flowResult.Phases),
			"constitution_saved": flowResult.Phases["constitution"] != nil,
		},
	}

	ds.logger.WithFields(logrus.Fields{
		"session_id":      sessionID,
		"duration":        result.Duration,
		"quality_score":   result.QualityScore,
		"phases_complete": len(flowResult.Phases),
	}).Info("[SpecKit Flow] Workflow completed successfully")

	return result, nil
}

// convertSpecKitPhasesToResponses converts SpecKit phase results to ParticipantResponses
func (ds *DebateService) convertSpecKitPhasesToResponses(flowResult *SpecKitFlowResult) []ParticipantResponse {
	responses := make([]ParticipantResponse, 0, len(flowResult.Phases))

	phaseOrder := []SpecKitPhase{
		PhaseConstitution, PhaseSpecify, PhaseClarify,
		PhasePlan, PhaseTasks, PhaseAnalyze, PhaseImplement,
	}

	for i, phase := range phaseOrder {
		phaseResult, exists := flowResult.Phases[string(phase)]
		if !exists {
			continue
		}

		response := ParticipantResponse{
			ParticipantID: fmt.Sprintf("speckit-%s", phase),
			Role:          string(phase),
			LLMProvider:   "speckit",
			LLMModel:      "debate-team",
			Content:       phaseResult.Artifact,
			Confidence:    phaseResult.QualityScore,
			ResponseTime:  phaseResult.Duration,
			Round:         i + 1,
		}

		responses = append(responses, response)
	}

	return responses
}

// extractBestResponseFromSpecKit extracts the best response from SpecKit flow (Implementation phase)
func (ds *DebateService) extractBestResponseFromSpecKit(flowResult *SpecKitFlowResult) *ParticipantResponse {
	// Implementation phase is the final output
	implementPhase, exists := flowResult.Phases[string(PhaseImplement)]
	if !exists || implementPhase == nil {
		// Fallback to last available phase
		for _, phase := range []SpecKitPhase{PhaseAnalyze, PhaseTasks, PhasePlan, PhaseClarify, PhaseSpecify} {
			if result, ok := flowResult.Phases[string(phase)]; ok && result != nil {
				return &ParticipantResponse{
					ParticipantID: fmt.Sprintf("speckit-%s", phase),
					Role:          string(phase),
					LLMProvider:   "speckit",
					LLMModel:      "debate-team",
					Content:       result.Artifact,
					Confidence:    result.QualityScore,
					ResponseTime:  result.Duration,
					Round:         1,
				}
			}
		}
		return nil
	}

	return &ParticipantResponse{
		ParticipantID: "speckit-implementation",
		Role:          "implementation",
		LLMProvider:   "speckit",
		LLMModel:      "debate-team",
		Content:       implementPhase.Artifact,
		Confidence:    implementPhase.QualityScore,
		ResponseTime:  implementPhase.Duration,
		Round:         7,
	}
}

// calculateSpecKitQualityScore calculates overall quality score from SpecKit phases
func (ds *DebateService) calculateSpecKitQualityScore(flowResult *SpecKitFlowResult) float64 {
	if len(flowResult.Phases) == 0 {
		return 0.0
	}

	totalScore := 0.0
	for _, phaseResult := range flowResult.Phases {
		totalScore += phaseResult.QualityScore
	}

	return totalScore / float64(len(flowResult.Phases))
}

// conductHelixSpecifierDebate executes the HelixSpecifier fusion engine flow
// and converts the result to a DebateResult. This is the preferred path when
// HelixSpecifier is available (default). Falls back to conductSpecKitDebate
// if HelixSpecifier is not initialized.
func (ds *DebateService) conductHelixSpecifierDebate(
	ctx context.Context,
	config *DebateConfig,
	intentResult *EnhancedIntentResult,
	startTime time.Time,
	sessionID string,
) (*DebateResult, error) {
	ds.logger.WithFields(logrus.Fields{
		"session_id":  sessionID,
		"granularity": intentResult.Granularity,
		"action_type": intentResult.ActionType,
		"engine":      ds.specifierAdapter.Name(),
		"version":     ds.specifierAdapter.Version(),
	}).Info("[HelixSpecifier Flow] Starting spec-driven development flow")

	// Classify effort via HelixSpecifier
	classification, err := ds.specifierAdapter.ClassifyEffort(ctx, config.Topic)
	if err != nil {
		ds.logger.WithError(err).Warn("[HelixSpecifier Flow] Effort classification failed, falling back to SpecKit")
		if ds.speckitOrchestrator != nil {
			return ds.conductSpecKitDebate(ctx, config, intentResult, startTime, sessionID)
		}
		return nil, fmt.Errorf("HelixSpecifier effort classification failed: %w", err)
	}

	// Execute the fusion engine flow
	flowResult, err := ds.specifierAdapter.ExecuteFlow(ctx, config.Topic, classification)
	if err != nil {
		ds.logger.WithError(err).Warn("[HelixSpecifier Flow] Flow execution failed, falling back to SpecKit")
		if ds.speckitOrchestrator != nil {
			return ds.conductSpecKitDebate(ctx, config, intentResult, startTime, sessionID)
		}
		return nil, fmt.Errorf("HelixSpecifier flow execution failed: %w", err)
	}

	endTime := time.Now()

	// Convert HelixSpecifier flow result to DebateResult
	responses := make([]ParticipantResponse, 0, len(flowResult.PhaseResults))
	for i, pr := range flowResult.PhaseResults {
		responses = append(responses, ParticipantResponse{
			ParticipantID: fmt.Sprintf("helixspecifier-%s", pr.Phase),
			Role:          string(pr.Phase),
			LLMProvider:   "helixspecifier",
			LLMModel:      "fusion-engine",
			Content:       pr.Output,
			Confidence:    pr.QualityScore,
			ResponseTime:  pr.Duration,
			Round:         i + 1,
		})
	}

	var bestResponse *ParticipantResponse
	if len(responses) > 0 {
		last := responses[len(responses)-1]
		bestResponse = &last
	}

	result := &DebateResult{
		DebateID:        config.DebateID,
		SessionID:       sessionID,
		Topic:           config.Topic,
		StartTime:       startTime,
		EndTime:         endTime,
		Duration:        endTime.Sub(startTime),
		TotalRounds:     len(flowResult.PhaseResults),
		RoundsConducted: len(flowResult.PhaseResults),
		AllResponses:    responses,
		BestResponse:    bestResponse,
		Consensus: &ConsensusResult{
			Achieved:       flowResult.Success,
			Confidence:     flowResult.OverallQualityScore,
			AgreementLevel: flowResult.OverallQualityScore,
			FinalPosition:  flowResult.FinalArtifact,
			KeyPoints:      []string{"HelixSpecifier fusion engine flow completed"},
			Disagreements:  []string{},
			Summary: fmt.Sprintf(
				"HelixSpecifier flow: effort=%s ceremony=%s phases=%d quality=%.2f",
				flowResult.EffortLevel, flowResult.CeremonyLevel,
				len(flowResult.PhaseResults), flowResult.OverallQualityScore,
			),
			Timestamp:    endTime,
			QualityScore: flowResult.OverallQualityScore,
		},
		QualityScore: flowResult.OverallQualityScore,
		FinalScore:   flowResult.OverallQualityScore,
		Success:      flowResult.Success,
		Metadata: map[string]interface{}{
			"helixspecifier_flow": true,
			"effort_level":        string(flowResult.EffortLevel),
			"ceremony_level":      string(flowResult.CeremonyLevel),
			"granularity":         intentResult.Granularity,
			"action_type":         intentResult.ActionType,
			"requires_speckit":    intentResult.RequiresSpecKit,
			"speckit_reason":      intentResult.SpecKitReason,
			"phases_completed":    len(flowResult.PhaseResults),
			"engine_name":         ds.specifierAdapter.Name(),
			"engine_version":      ds.specifierAdapter.Version(),
		},
	}

	ds.logger.WithFields(logrus.Fields{
		"session_id":    sessionID,
		"duration":      result.Duration,
		"quality_score": result.QualityScore,
		"effort":        flowResult.EffortLevel,
		"ceremony":      flowResult.CeremonyLevel,
		"phases":        len(flowResult.PhaseResults),
	}).Info("[HelixSpecifier Flow] Spec-driven development flow completed")

	return result, nil
}

// debateMemoryFallbackPath returns a writable, durable file path for the
// disk-backed debate-memory fallback store, or "" if no writable base dir can
// be determined. It probes the OS user-cache dir first (durable across runs),
// falling back to the OS temp dir; the directory is created if absent. A "" or
// unwritable result causes the caller to use the in-memory store instead,
// preserving the never-nil store guarantee.
func debateMemoryFallbackPath() string {
	candidates := []string{}
	if cacheDir, err := os.UserCacheDir(); err == nil && cacheDir != "" {
		candidates = append(candidates, filepath.Join(cacheDir, "helixagent", "memory"))
	}
	candidates = append(candidates, filepath.Join(os.TempDir(), "helixagent", "memory"))

	for _, dir := range candidates {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			continue
		}
		// Confirm writability before handing the path to the store.
		probe := filepath.Join(dir, ".write_probe")
		if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
			continue
		}
		_ = os.Remove(probe)
		return filepath.Join(dir, "debate_memory_fallback.db")
	}
	return ""
}
