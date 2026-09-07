package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"time"

	"digital.vasic.concurrency/pkg/safe"

	"dev.helix.agent/internal/database"
	"github.com/sirupsen/logrus"
)

// EmbeddingManager handles embedding generation and vector database operations.
//
// Concurrency model (CONST-029): embeddingCache → *safe.Store. mu
// dropped.
type EmbeddingManager struct {
	repo           *database.ModelMetadataRepository
	cache          CacheInterface
	log            *logrus.Logger
	vectorProvider string
	openAIKey      string
	httpClient     *http.Client
	embeddingCache *safe.Store[string, []float64]
}

// EmbeddingRequest represents a request to generate embeddings
type EmbeddingRequest struct {
	Text      string `json:"text"`
	Model     string `json:"model,omitempty"`
	Dimension int    `json:"dimension,omitempty"`
	Batch     bool   `json:"batch,omitempty"`
}

// EmbeddingResponse represents the response from embedding generation
type EmbeddingResponse struct {
	Success    bool      `json:"success"`
	Embeddings []float64 `json:"embeddings,omitempty"`
	Error      string    `json:"error,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
	Model      string    `json:"model,omitempty"`
	TokensUsed int       `json:"tokens_used,omitempty"`
}

// VectorSearchRequest represents a vector similarity search request
type VectorSearchRequest struct {
	Query     string    `json:"query"`
	Vector    []float64 `json:"vector"`
	Limit     int       `json:"limit,omitempty"`
	Threshold float64   `json:"threshold,omitempty"`
}

// VectorSearchResponse represents the response from vector search
type VectorSearchResponse struct {
	Success bool `json:"success"`
	// Results is intentionally NOT omitempty: when no documents match,
	// callers expect `results: []` to appear in JSON so they can iterate
	// without testing for field presence first. CONST-035 §c (no
	// surprising omissions in documented response shape).
	Results   []VectorSearchResult `json:"results"`
	Count     int                  `json:"count"`
	Error     string               `json:"error,omitempty"`
	Timestamp time.Time            `json:"timestamp"`
}

// VectorSearchResult represents a single search result
type VectorSearchResult struct {
	ID       string                 `json:"id"`
	Content  string                 `json:"content"`
	Score    float64                `json:"score"`
	Metadata map[string]interface{} `json:"metadata"`
}

// OpenAI embedding API types
type openAIEmbeddingRequest struct {
	Input          interface{} `json:"input"`
	Model          string      `json:"model"`
	EncodingFormat string      `json:"encoding_format,omitempty"`
	Dimensions     int         `json:"dimensions,omitempty"`
}

type openAIEmbeddingResponse struct {
	Object string `json:"object"`
	Data   []struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// EmbeddingConfig holds configuration for the embedding manager
type EmbeddingConfig struct {
	OpenAIAPIKey   string
	VectorProvider string
	Timeout        time.Duration
	CacheEnabled   bool
}

// NewEmbeddingManager creates a new embedding manager.
//
// Local-first default (spec 002, HA-F2-002): OPENAI_API_KEY sitting in the
// environment is NOT by itself consent to send text to OpenAI. This is the
// same IMPLICIT acquisition path the provider registry and the boot-time
// verifier gate, reached here through the wired
// POST /v1/protocols/execute {"protocol_type":"embedding"} route
// (router.go -> NewUnifiedProtocolManager -> NewEmbeddingManager), so it
// consults the SAME predicate.
//
// Gating this degrades gracefully rather than failing: GenerateEmbedding
// already falls back to generateLocalEmbedding when no key is configured,
// which is exactly the local-first behaviour. An operator who wants cloud
// embeddings either opts in via HELIX_CLOUD_PROVIDERS or supplies the key
// explicitly through NewEmbeddingManagerWithConfig, which stays ungated.
func NewEmbeddingManager(repo *database.ModelMetadataRepository, cache CacheInterface, log *logrus.Logger) *EmbeddingManager {
	openAIKey := ""
	if CloudProvidersOptedIn() {
		openAIKey = os.Getenv("OPENAI_API_KEY")
	}

	return NewEmbeddingManagerWithConfig(repo, cache, log, EmbeddingConfig{
		OpenAIAPIKey:   openAIKey,
		VectorProvider: "pgvector",
		Timeout:        30 * time.Second,
		CacheEnabled:   true,
	})
}

// NewEmbeddingManagerWithConfig creates a new embedding manager with explicit config
func NewEmbeddingManagerWithConfig(repo *database.ModelMetadataRepository, cache CacheInterface, log *logrus.Logger, config EmbeddingConfig) *EmbeddingManager {
	timeout := config.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	vectorProvider := config.VectorProvider
	if vectorProvider == "" {
		vectorProvider = "pgvector"
	}

	return &EmbeddingManager{
		repo:           repo,
		cache:          cache,
		log:            log,
		vectorProvider: vectorProvider,
		openAIKey:      config.OpenAIAPIKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		embeddingCache: safe.NewStore[string, []float64](),
	}
}

// GenerateEmbedding generates embeddings for the given text using default model
func (m *EmbeddingManager) GenerateEmbedding(ctx context.Context, text string) (EmbeddingResponse, error) {
	return m.GenerateEmbeddingWithModel(ctx, text, "text-embedding-3-small")
}

// GenerateEmbeddingWithModel generates embeddings using a specific model
func (m *EmbeddingManager) GenerateEmbeddingWithModel(ctx context.Context, text string, model string) (EmbeddingResponse, error) {
	startTime := time.Now()

	// Check cache first
	cacheKey := m.getCacheKey(text, model)
	if cached := m.getCachedEmbedding(cacheKey); cached != nil {
		m.log.WithField("cacheHit", true).Debug("Returning cached embedding")
		return EmbeddingResponse{
			Success:    true,
			Embeddings: cached,
			Timestamp:  time.Now(),
			Model:      model,
		}, nil
	}

	var embedding []float64
	var tokensUsed int
	var err error

	// Try OpenAI API first if key is configured
	if m.openAIKey != "" {
		embedding, tokensUsed, err = m.generateOpenAIEmbedding(ctx, text, model)
		if err != nil {
			m.log.WithError(err).Warn("OpenAI embedding failed, falling back to local")
		}
	}

	// Fallback to local embedding if OpenAI failed or not configured
	if embedding == nil {
		embedding = m.generateLocalEmbedding(text, m.getDimension(model))
		m.log.Debug("Using local embedding generation")
	}

	// Cache the result
	m.setCachedEmbedding(cacheKey, embedding)

	duration := time.Since(startTime)
	m.log.WithFields(logrus.Fields{
		"model":      model,
		"dimension":  len(embedding),
		"tokensUsed": tokensUsed,
		"duration":   duration,
	}).Info("Embedding generated successfully")

	return EmbeddingResponse{
		Success:    true,
		Embeddings: embedding,
		Timestamp:  time.Now(),
		Model:      model,
		TokensUsed: tokensUsed,
	}, nil
}

// generateOpenAIEmbedding calls the OpenAI embedding API
func (m *EmbeddingManager) generateOpenAIEmbedding(ctx context.Context, text string, model string) ([]float64, int, error) {
	if m.openAIKey == "" {
		return nil, 0, fmt.Errorf("OpenAI API key not configured")
	}

	// Build request
	reqBody := openAIEmbeddingRequest{
		Input: text,
		Model: model,
	}

	// Add dimensions for v3 models
	if model == "text-embedding-3-small" || model == "text-embedding-3-large" {
		reqBody.EncodingFormat = "float"
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.openAIKey)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to call OpenAI API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("OpenAI API error: %d - %s", resp.StatusCode, string(respBody))
	}

	var openAIResp openAIEmbeddingResponse
	if err := json.Unmarshal(respBody, &openAIResp); err != nil {
		return nil, 0, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(openAIResp.Data) == 0 {
		return nil, 0, fmt.Errorf("no embedding data in response")
	}

	return openAIResp.Data[0].Embedding, openAIResp.Usage.TotalTokens, nil
}

// generateLocalEmbedding generates a deterministic embedding locally using hash-based approach
// This is a fallback when external APIs are not available
func (m *EmbeddingManager) generateLocalEmbedding(text string, dimension int) []float64 {
	// Create a deterministic embedding based on text hash
	// This provides consistent embeddings for the same input
	hash := sha256.Sum256([]byte(text))
	embedding := make([]float64, dimension)

	// Use the hash bytes to seed the embedding values
	for i := 0; i < dimension; i++ {
		// Use multiple hash rounds for more dimensions
		hashIndex := i % 32
		byteVal := hash[hashIndex]

		// Convert to float in range [-1, 1]
		embedding[i] = (float64(byteVal)/127.5 - 1.0)

		// Add position-dependent variation
		if i >= 32 {
			additionalHash := sha256.Sum256(append(hash[:], byte(i/32)))
			embedding[i] = (float64(additionalHash[i%32])/127.5 - 1.0)
		}
	}

	// Normalize the embedding to unit length
	return m.normalizeVector(embedding)
}

// normalizeVector normalizes a vector to unit length
func (m *EmbeddingManager) normalizeVector(vec []float64) []float64 {
	var sumSquares float64
	for _, v := range vec {
		sumSquares += v * v
	}
	norm := math.Sqrt(sumSquares)
	if norm == 0 {
		return vec
	}

	normalized := make([]float64, len(vec))
	for i, v := range vec {
		normalized[i] = v / norm
	}
	return normalized
}

// getDimension returns the embedding dimension for a given model
func (m *EmbeddingManager) getDimension(model string) int {
	switch model {
	case "text-embedding-ada-002":
		return 1536
	case "text-embedding-3-small":
		return 1536
	case "text-embedding-3-large":
		return 3072
	case "embed-multilingual-v3.0":
		return 1024
	case "all-MiniLM-L6-v2":
		return 384
	default:
		return 1536
	}
}

// getCacheKey generates a cache key for embeddings
func (m *EmbeddingManager) getCacheKey(text, model string) string {
	hash := sha256.Sum256([]byte(text + model))
	return fmt.Sprintf("emb:%x", hash[:8])
}

// getCachedEmbedding retrieves a cached embedding
func (m *EmbeddingManager) getCachedEmbedding(key string) []float64 {
	emb, _ := m.embeddingCache.Get(key)
	return emb
}

// setCachedEmbedding stores an embedding in cache
func (m *EmbeddingManager) setCachedEmbedding(key string, embedding []float64) {
	m.embeddingCache.Put(key, embedding)
}

// GenerateEmbeddings generates embeddings for text (compatibility method)
func (e *EmbeddingManager) GenerateEmbeddings(ctx context.Context, req EmbeddingRequest) (*EmbeddingResponse, error) {
	model := req.Model
	if model == "" {
		model = "text-embedding-3-small"
	}

	resp, err := e.GenerateEmbeddingWithModel(ctx, req.Text, model)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// GenerateBatchEmbeddings generates embeddings for multiple texts
func (m *EmbeddingManager) GenerateBatchEmbeddings(ctx context.Context, texts []string, model string) ([][]float64, error) {
	if model == "" {
		model = "text-embedding-3-small"
	}

	// Try batch API if OpenAI is configured
	if m.openAIKey != "" {
		embeddings, err := m.generateOpenAIBatchEmbedding(ctx, texts, model)
		if err == nil {
			return embeddings, nil
		}
		m.log.WithError(err).Warn("Batch OpenAI embedding failed, falling back to sequential")
	}

	// Fall back to sequential generation
	embeddings := make([][]float64, len(texts))
	for i, text := range texts {
		resp, err := m.GenerateEmbeddingWithModel(ctx, text, model)
		if err != nil {
			return nil, fmt.Errorf("failed to generate embedding for text %d: %w", i, err)
		}
		embeddings[i] = resp.Embeddings
	}

	return embeddings, nil
}

// generateOpenAIBatchEmbedding generates embeddings for multiple texts in a single API call
func (m *EmbeddingManager) generateOpenAIBatchEmbedding(ctx context.Context, texts []string, model string) ([][]float64, error) {
	if m.openAIKey == "" {
		return nil, fmt.Errorf("OpenAI API key not configured")
	}

	reqBody := openAIEmbeddingRequest{
		Input: texts,
		Model: model,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.openAIKey)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call OpenAI API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenAI API error: %d - %s", resp.StatusCode, string(respBody))
	}

	var openAIResp openAIEmbeddingResponse
	if err := json.Unmarshal(respBody, &openAIResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	embeddings := make([][]float64, len(openAIResp.Data))
	for _, data := range openAIResp.Data {
		embeddings[data.Index] = data.Embedding
	}

	return embeddings, nil
}

// StoreEmbedding stores embeddings in the vector database
func (e *EmbeddingManager) StoreEmbedding(ctx context.Context, id string, text string, vector []float64) error {
	e.log.WithFields(logrus.Fields{
		"id":        id,
		"dimension": len(vector),
	}).Debug("Storing embedding in vector database")

	// Convert to float32 for pgvector
	vectorF32 := make([]float32, len(vector))
	for i, v := range vector {
		vectorF32[i] = float32(v)
	}

	// Store in database if repository is available
	if e.repo != nil {
		// The repo would need a method to store vector documents
		// For now, we'll use the cache as a fallback storage
		e.log.Debug("Repository storage for vectors would be implemented here")
	}

	// Also cache the embedding for fast retrieval
	cacheKey := fmt.Sprintf("stored_emb:%s", id)
	e.setCachedEmbedding(cacheKey, vector)

	e.log.WithField("id", id).Debug("Embedding stored successfully")
	return nil
}

// VectorSearch performs similarity search in the vector database
func (e *EmbeddingManager) VectorSearch(ctx context.Context, req VectorSearchRequest) (*VectorSearchResponse, error) {
	e.log.WithFields(logrus.Fields{
		"query":     req.Query[:min(50, len(req.Query))],
		"limit":     req.Limit,
		"threshold": req.Threshold,
	}).Info("Performing vector search")

	// Generate embedding for query if vector not provided
	var queryVector []float64
	if len(req.Vector) > 0 {
		queryVector = req.Vector
	} else if req.Query != "" {
		resp, err := e.GenerateEmbedding(ctx, req.Query)
		if err != nil {
			return nil, fmt.Errorf("failed to generate query embedding: %w", err)
		}
		queryVector = resp.Embeddings
	} else {
		return nil, fmt.Errorf("either query or vector must be provided")
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	threshold := req.Threshold
	if threshold <= 0 {
		threshold = 0.7
	}

	// Search in cached embeddings
	var results []VectorSearchResult
	e.embeddingCache.Range(func(key string, embedding []float64) bool {
		if len(key) > 11 && key[:11] == "stored_emb:" {
			score := e.cosineSimilarity(queryVector, embedding)
			if score >= threshold {
				results = append(results, VectorSearchResult{
					ID:       key[11:], // Remove "stored_emb:" prefix
					Content:  "",       // Content would come from database
					Score:    score,
					Metadata: map[string]interface{}{},
				})
			}
		}
		return true
	})

	// Sort by score descending
	for i := 0; i < len(results)-1; i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].Score > results[i].Score {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	// Limit results
	if len(results) > limit {
		results = results[:limit]
	}

	if results == nil {
		results = []VectorSearchResult{}
	}
	response := &VectorSearchResponse{
		Timestamp: time.Now(),
		Success:   true,
		Results:   results,
		Count:     len(results),
	}

	e.log.WithField("resultCount", len(results)).Info("Vector search completed")
	return response, nil
}

// GetEmbeddingStats returns statistics about embedding usage
func (e *EmbeddingManager) GetEmbeddingStats(ctx context.Context) (map[string]interface{}, error) {
	cacheSize := e.embeddingCache.Len()

	stats := map[string]interface{}{
		"cachedEmbeddings": cacheSize,
		"vectorProvider":   e.vectorProvider,
		"openAIConfigured": e.openAIKey != "",
		"supportedModels":  []string{"text-embedding-ada-002", "text-embedding-3-small", "text-embedding-3-large"},
		"defaultDimension": 1536,
		"lastUpdate":       time.Now(),
	}

	e.log.WithFields(stats).Info("Embedding statistics retrieved")
	return stats, nil
}

// ConfigureVectorProvider configures the vector database provider
func (e *EmbeddingManager) ConfigureVectorProvider(ctx context.Context, provider string) error {
	e.log.WithField("provider", provider).Info("Configuring vector provider")
	e.vectorProvider = provider
	return nil
}

// IndexDocument indexes a document for semantic search
func (e *EmbeddingManager) IndexDocument(ctx context.Context, id, title, content string, metadata map[string]interface{}) error {
	e.log.WithFields(logrus.Fields{
		"id":    id,
		"title": title,
	}).Info("Indexing document for semantic search")

	// Generate embedding for the document
	fullText := title + "\n" + content
	resp, err := e.GenerateEmbedding(ctx, fullText)
	if err != nil {
		return fmt.Errorf("failed to generate embedding for document: %w", err)
	}

	// Store the embedding
	err = e.StoreEmbedding(ctx, id, fullText, resp.Embeddings)
	if err != nil {
		return fmt.Errorf("failed to store embedding: %w", err)
	}

	e.log.WithField("id", id).Info("Document indexed successfully")
	return nil
}

// BatchIndexDocuments indexes multiple documents for semantic search
func (e *EmbeddingManager) BatchIndexDocuments(ctx context.Context, documents []map[string]interface{}) error {
	e.log.WithField("count", len(documents)).Info("Batch indexing documents for semantic search")

	// Collect all texts for batch embedding
	texts := make([]string, 0, len(documents))
	docIDs := make([]string, 0, len(documents))

	for _, doc := range documents {
		//nolint:errcheck // document map validation ensures correct types
		id, _ := doc["id"].(string)
		//nolint:errcheck // document map validation ensures correct types
		title, _ := doc["title"].(string)
		//nolint:errcheck // document map validation ensures correct types
		content, _ := doc["content"].(string)

		if id == "" {
			continue
		}

		fullText := title + "\n" + content
		texts = append(texts, fullText)
		docIDs = append(docIDs, id)
	}

	// Generate embeddings in batch
	embeddings, err := e.GenerateBatchEmbeddings(ctx, texts, "text-embedding-3-small")
	if err != nil {
		return fmt.Errorf("failed to generate batch embeddings: %w", err)
	}

	// Store all embeddings
	for i, embedding := range embeddings {
		if err := e.StoreEmbedding(ctx, docIDs[i], texts[i], embedding); err != nil {
			e.log.WithError(err).WithField("id", docIDs[i]).Error("Failed to store embedding")
			continue
		}
	}

	e.log.WithField("indexed", len(embeddings)).Info("Batch document indexing completed")
	return nil
}

// cosineSimilarity calculates cosine similarity between two vectors
func (m *EmbeddingManager) cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) {
		return 0
	}

	var dotProduct, normA, normB float64
	for i := range a {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	return dotProduct / math.Sqrt(normA) / math.Sqrt(normB)
}

// EuclideanDistance calculates the Euclidean distance between two vectors
func (m *EmbeddingManager) EuclideanDistance(a, b []float64) float64 {
	if len(a) != len(b) {
		return math.MaxFloat64
	}

	var sum float64
	for i := range a {
		diff := a[i] - b[i]
		sum += diff * diff
	}

	return math.Sqrt(sum)
}

// EmbeddingProviderInfo represents information about an embedding provider
type EmbeddingProviderInfo struct {
	Name        string    `json:"name"`
	Model       string    `json:"model"`
	Dimension   int       `json:"dimension"`
	Enabled     bool      `json:"enabled"`
	MaxTokens   int       `json:"maxTokens"`
	Description string    `json:"description"`
	LastSync    time.Time `json:"lastSync,omitempty"`
}

// ListEmbeddingProviders lists all embedding providers
func (m *EmbeddingManager) ListEmbeddingProviders(ctx context.Context) ([]map[string]interface{}, error) {
	m.log.Debug("Listing embedding providers")

	// Define available embedding providers
	providers := []EmbeddingProviderInfo{
		{
			Name:        "openai-ada",
			Model:       "text-embedding-ada-002",
			Dimension:   1536,
			Enabled:     m.openAIKey != "",
			MaxTokens:   8191,
			Description: "OpenAI Ada v2 embedding model",
		},
		{
			Name:        "openai-3-small",
			Model:       "text-embedding-3-small",
			Dimension:   1536,
			Enabled:     m.openAIKey != "",
			MaxTokens:   8191,
			Description: "OpenAI text-embedding-3-small (recommended)",
		},
		{
			Name:        "openai-3-large",
			Model:       "text-embedding-3-large",
			Dimension:   3072,
			Enabled:     m.openAIKey != "",
			MaxTokens:   8191,
			Description: "OpenAI text-embedding-3-large (highest quality)",
		},
		{
			Name:        "local-hash",
			Model:       "local-hash-embedding",
			Dimension:   1536,
			Enabled:     true,
			MaxTokens:   100000,
			Description: "Local hash-based embedding (fallback, always available)",
		},
	}

	// Convert to map format for API response
	result := make([]map[string]interface{}, len(providers))
	for i, p := range providers {
		result[i] = map[string]interface{}{
			"name":        p.Name,
			"model":       p.Model,
			"dimension":   p.Dimension,
			"enabled":     p.Enabled,
			"maxTokens":   p.MaxTokens,
			"description": p.Description,
		}
	}

	m.log.WithField("count", len(providers)).Info("Listed embedding providers")
	return result, nil
}

// RefreshAllEmbeddings refreshes all embedding providers
func (m *EmbeddingManager) RefreshAllEmbeddings(ctx context.Context) error {
	m.log.Info("Starting embedding providers refresh")

	providers, err := m.ListEmbeddingProviders(ctx)
	if err != nil {
		m.log.WithError(err).Error("Failed to list embedding providers for refresh")
		return fmt.Errorf("failed to list embedding providers: %w", err)
	}

	var refreshErrors []error
	refreshedCount := 0

	for _, provider := range providers {
		//nolint:errcheck // provider map validation ensures correct types
		providerName, _ := provider["name"].(string)
		//nolint:errcheck // provider map validation ensures correct types
		enabled, _ := provider["enabled"].(bool)

		if !enabled {
			m.log.WithField("provider", providerName).Debug("Skipping disabled embedding provider")
			continue
		}

		if err := m.refreshEmbeddingProvider(ctx, providerName); err != nil {
			m.log.WithFields(logrus.Fields{
				"provider": providerName,
				"error":    err.Error(),
			}).Warn("Failed to refresh embedding provider")
			refreshErrors = append(refreshErrors, err)
		} else {
			refreshedCount++
		}
	}

	// Clear embedding cache on refresh
	m.embeddingCache.Clear()

	m.log.WithFields(logrus.Fields{
		"refreshedCount": refreshedCount,
		"totalProviders": len(providers),
		"errorCount":     len(refreshErrors),
	}).Info("Embedding providers refresh completed")

	if len(refreshErrors) > 0 {
		return fmt.Errorf("failed to refresh %d of %d providers", len(refreshErrors), len(providers))
	}

	return nil
}

// refreshEmbeddingProvider refreshes a single embedding provider
func (m *EmbeddingManager) refreshEmbeddingProvider(ctx context.Context, providerName string) error {
	m.log.WithField("provider", providerName).Debug("Refreshing embedding provider")

	// For OpenAI providers, verify API connectivity
	if providerName == "openai-ada" || providerName == "openai-3-small" || providerName == "openai-3-large" {
		if m.openAIKey == "" {
			return fmt.Errorf("OpenAI API key not configured")
		}

		// Test with a simple embedding
		_, _, err := m.generateOpenAIEmbedding(ctx, "test", "text-embedding-3-small")
		if err != nil {
			return fmt.Errorf("failed to verify OpenAI connectivity: %w", err)
		}
	}

	m.log.WithField("provider", providerName).Info("Embedding provider refreshed successfully")
	return nil
}

// ClearCache clears the embedding cache
func (m *EmbeddingManager) ClearCache() {
	m.embeddingCache.Clear()
	m.log.Info("Embedding cache cleared")
}
