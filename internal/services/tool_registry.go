package services

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"digital.vasic.concurrency/pkg/safe"
)

// Tool represents a unified tool interface
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]interface{}
	Execute(ctx context.Context, params map[string]interface{}) (interface{}, error)
	Source() string // "mcp", "lsp", "custom", etc.
}

// ToolRegistry manages tools from various sources.
//
// Concurrent-safe by construction (CONST-029 Tier 1): `tools` and
// `customTools` are independent safe.Store instances. RegisterCustomTool
// is the only joint-write path; it writes `tools` then `customTools`
// sequentially (a reader between the two writes would see the tool in
// `tools` but not yet in `customTools` — acceptable since customTools
// is only consulted for the custom_count stat).
type ToolRegistry struct {
	tools       *safe.Store[string, Tool]
	customTools *safe.Store[string, Tool]
	mcpManager  *MCPManager
	lspClient   *LSPClient
	lastRefresh time.Time
	// externalSources holds every fetcher registered via
	// RegisterExternalToolSource, keyed by sourceName (HXC-159 T-P6.01.3).
	// RefreshTools re-invokes each of these AFTER clearing non-custom
	// tools, so an external source's tools are refreshed, not wiped —
	// closing the gap where a source registered once (e.g. the HelixSkills
	// external source, attachment-point rank 2) silently disappeared on
	// the next refresh because its tools report a Source() other than
	// "custom".
	externalSources *safe.Store[string, func() ([]Tool, error)]
}

// NewToolRegistry creates a new tool registry
func NewToolRegistry(mcpManager *MCPManager, lspClient *LSPClient) *ToolRegistry {
	return &ToolRegistry{
		tools:           safe.NewStore[string, Tool](),
		customTools:     safe.NewStore[string, Tool](),
		mcpManager:      mcpManager,
		lspClient:       lspClient,
		externalSources: safe.NewStore[string, func() ([]Tool, error)](),
	}
}

// RegisterCustomTool registers a custom tool with validation.
// Atomic check-then-insert for the primary `tools` store via Update;
// `customTools` is then populated unconditionally.
func (tr *ToolRegistry) RegisterCustomTool(tool Tool) error {
	if err := tr.validateToolMetadata(tool); err != nil {
		return fmt.Errorf("tool validation failed: %w", err)
	}

	name := tool.Name()
	var registered bool
	tr.tools.Update(name, func(existing Tool, present bool) (Tool, bool) {
		if present {
			return existing, true
		}
		registered = true
		return tool, true
	})
	if !registered {
		return fmt.Errorf("tool %s already registered", name)
	}
	tr.customTools.Put(name, tool)
	return nil
}

// validateToolMetadata validates tool metadata
func (tr *ToolRegistry) validateToolMetadata(tool Tool) error {
	if tool.Name() == "" {
		return fmt.Errorf("tool name cannot be empty")
	}

	if tool.Description() == "" {
		return fmt.Errorf("tool description cannot be empty")
	}

	params := tool.Parameters()
	if params == nil {
		return fmt.Errorf("tool parameters cannot be nil")
	}

	// Validate parameter schemas
	for paramName, paramSchema := range params {
		if err := tr.validateParameterSchema(paramName, paramSchema); err != nil {
			return err
		}
	}

	return nil
}

// validateParameterSchema validates a parameter schema
func (tr *ToolRegistry) validateParameterSchema(name string, schema interface{}) error {
	// Basic validation - can be enhanced with JSON Schema validation
	schemaMap, ok := schema.(map[string]interface{})
	if !ok {
		return fmt.Errorf("parameter %s schema must be a map", name)
	}

	if _, hasType := schemaMap["type"]; !hasType {
		return fmt.Errorf("parameter %s schema must have a type", name)
	}

	return nil
}

// RegisterExternalToolSource registers tools from an external source.
//
// The (sourceName, toolFetcher) pair is remembered in tr.externalSources
// (HXC-159 T-P6.01.3) BEFORE the first fetch runs, so even a source whose
// very first fetch fails is retried on every subsequent RefreshTools —
// without this, RefreshTools's unconditional "clear every non-custom tool"
// step (below) would silently orphan the source's tools the moment a
// refresh ran, since a source's tools report their OWN Source() (e.g.
// "helixskills-external"), never the literal "custom" that alone survives
// a refresh.
func (tr *ToolRegistry) RegisterExternalToolSource(sourceName string, toolFetcher func() ([]Tool, error)) error {
	tr.externalSources.Put(sourceName, toolFetcher)

	tools, err := toolFetcher()
	if err != nil {
		return fmt.Errorf("failed to fetch tools from %s: %w", sourceName, err)
	}
	tr.registerFetchedTools(sourceName, tools)
	return nil
}

// registerFetchedTools validates and dedup-registers a batch of tools
// fetched from sourceName into tr.tools. Shared by
// RegisterExternalToolSource (first registration) and RefreshTools
// (re-fetch on every subsequent refresh) so both paths get the same
// validation, dedup, and logging "for free" — never a second policy path.
func (tr *ToolRegistry) registerFetchedTools(sourceName string, tools []Tool) {
	for _, tool := range tools {
		name := tool.Name()
		if err := tr.validateToolMetadata(tool); err != nil {
			log.Printf("Tool %s from %s validation failed: %v, skipping", name, sourceName, err)
			continue
		}
		if _, stored := tr.tools.PutIfAbsent(name, tool); stored {
			log.Printf("Registered tool %s from external source %s", name, sourceName)
		} else {
			log.Printf("Tool %s from %s already exists, skipping", name, sourceName)
		}
	}
}

// RefreshTools refreshes tools from all sources. Readers during refresh
// may observe a transitional state (non-custom tools briefly absent);
// callers retry on "tool not found" if that matters.
//
// HXC-159 T-P6.01.3: every source registered via RegisterExternalToolSource
// is re-fetched AFTER the clear + MCP/LSP steps below, so a refresh
// refreshes an external source's tools instead of wiping them — the
// registered source itself is never un-registered by a refresh.
func (tr *ToolRegistry) RefreshTools(ctx context.Context) error {
	// Clear non-custom tools.
	for name, tool := range tr.tools.Snapshot() {
		if tool.Source() != "custom" {
			tr.tools.Delete(name)
		}
	}

	if tr.mcpManager != nil {
		mcpTools := tr.mcpManager.ListTools()
		for _, mcpTool := range mcpTools {
			wrapper := &MCPToolWrapper{
				mcpTool:    mcpTool,
				mcpManager: tr.mcpManager,
			}
			tr.tools.Put(mcpTool.Name, wrapper)
		}
	}

	if tr.lspClient != nil { //nolint:staticcheck
		wrapper := &LSPToolWrapper{
			name:        "lsp_diagnostic",
			description: "Get diagnostics from LSP server for a given file",
			client:      tr.lspClient,
		}
		tr.tools.Put(wrapper.Name(), wrapper)
		log.Printf("Added LSP tool: %s", wrapper.Name())
	}

	for sourceName, fetcher := range tr.externalSources.Snapshot() {
		tools, fetchErr := fetcher()
		if fetchErr != nil {
			log.Printf("RefreshTools: external source %s fetch failed: %v — its tools stay absent until the next successful refresh", sourceName, fetchErr)
			continue
		}
		tr.registerFetchedTools(sourceName, tools)
	}

	tr.lastRefresh = time.Now()
	return nil
}

// GetTool returns a tool by name
func (tr *ToolRegistry) GetTool(name string) (Tool, bool) {
	return tr.tools.Get(name)
}

// ListTools returns all available tools
func (tr *ToolRegistry) ListTools() []Tool {
	return tr.tools.Values()
}

// ExecuteTool safely executes a tool with sandboxing
func (tr *ToolRegistry) ExecuteTool(ctx context.Context, name string, params map[string]interface{}) (interface{}, error) {
	tool, exists := tr.GetTool(name)
	if !exists {
		return nil, fmt.Errorf("tool %s not found", name)
	}

	// Basic parameter validation
	if err := tr.validateParameters(tool, params); err != nil {
		return nil, fmt.Errorf("parameter validation failed: %w", err)
	}

	// Execute with timeout
	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	result, err := tool.Execute(execCtx, params)
	if err != nil {
		return nil, fmt.Errorf("tool execution failed: %w", err)
	}

	return result, nil
}

// validateParameters performs basic parameter validation
func (tr *ToolRegistry) validateParameters(tool Tool, params map[string]interface{}) error {
	// Basic validation - could be enhanced
	required := tool.Parameters()
	for key := range required {
		if _, exists := params[key]; !exists {
			return fmt.Errorf("missing required parameter: %s", key)
		}
	}
	return nil
}

// MCPToolWrapper wraps MCP tools to implement the Tool interface
type MCPToolWrapper struct {
	mcpTool    *MCPTool
	mcpManager *MCPManager
}

func (w *MCPToolWrapper) Name() string {
	return w.mcpTool.Name
}

func (w *MCPToolWrapper) Description() string {
	return w.mcpTool.Description
}

func (w *MCPToolWrapper) Parameters() map[string]interface{} {
	return w.mcpTool.InputSchema
}

func (w *MCPToolWrapper) Execute(ctx context.Context, params map[string]interface{}) (interface{}, error) {
	return w.mcpManager.CallTool(ctx, w.mcpTool.Name, params)
}

func (w *MCPToolWrapper) Source() string {
	return "mcp"
}

// LSPToolWrapper wraps LSP-based tools (code actions, etc.)
type LSPToolWrapper struct {
	name        string
	description string
	client      *LSPClient
}

func (w *LSPToolWrapper) Name() string {
	return w.name
}

func (w *LSPToolWrapper) Description() string {
	return w.description
}

func (w *LSPToolWrapper) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"file": map[string]interface{}{
			"type":        "string",
			"description": "File path to get diagnostics for",
		},
	}
}

func (w *LSPToolWrapper) Execute(ctx context.Context, params map[string]interface{}) (interface{}, error) {
	if w.client == nil {
		return nil, fmt.Errorf("LSP client not available")
	}

	filePath, ok := params["file"].(string)
	if !ok || filePath == "" {
		return nil, fmt.Errorf("missing required parameter: file")
	}

	diagnostics, err := w.client.GetDiagnostics(ctx, filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get LSP diagnostics: %w", err)
	}

	return map[string]interface{}{
		"file":        filePath,
		"diagnostics": diagnostics,
		"count":       len(diagnostics),
	}, nil
}

func (w *LSPToolWrapper) Source() string {
	return "lsp"
}

// UnifiedSearchOptions configures unified tool search
type UnifiedSearchOptions struct {
	Query      string   `json:"query"`
	Sources    []string `json:"sources,omitempty"` // "mcp", "lsp", "custom", "schema"
	Categories []string `json:"categories,omitempty"`
	MaxResults int      `json:"max_results,omitempty"`
	MinScore   float64  `json:"min_score,omitempty"`
	FuzzyMatch bool     `json:"fuzzy_match,omitempty"`
}

// UnifiedSearchResult represents a unified search result
type UnifiedSearchResult struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Source      string                 `json:"source"`
	Category    string                 `json:"category,omitempty"`
	Score       float64                `json:"score"`
	MatchType   string                 `json:"match_type"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// Search performs unified search across all tool sources
func (tr *ToolRegistry) Search(opts UnifiedSearchOptions) []UnifiedSearchResult {
	if opts.MaxResults <= 0 {
		opts.MaxResults = 50
	}
	if opts.MinScore <= 0 {
		opts.MinScore = 0.1
	}

	var results []UnifiedSearchResult
	query := strings.ToLower(opts.Query)

	// Determine which sources to search
	searchAll := len(opts.Sources) == 0
	sourceMap := make(map[string]bool)
	for _, s := range opts.Sources {
		sourceMap[strings.ToLower(s)] = true
	}

	// Search registered tools
	for _, tool := range tr.tools.Snapshot() {
		source := strings.ToLower(tool.Source())
		if !searchAll && !sourceMap[source] {
			continue
		}

		score, matchType := tr.calculateToolSearchScore(tool, query, opts.FuzzyMatch)
		if score >= opts.MinScore {
			results = append(results, UnifiedSearchResult{
				Name:        tool.Name(),
				Description: tool.Description(),
				Source:      tool.Source(),
				Score:       score,
				MatchType:   matchType,
				Parameters:  tool.Parameters(),
			})
		}
	}

	// Sort by score descending
	tr.sortSearchResults(results)

	// Limit results
	if len(results) > opts.MaxResults {
		results = results[:opts.MaxResults]
	}

	return results
}

// calculateToolSearchScore calculates relevance score for a tool
func (tr *ToolRegistry) calculateToolSearchScore(tool Tool, query string, fuzzy bool) (float64, string) {
	if query == "" {
		return 1.0, "all"
	}

	var maxScore float64
	var matchType string

	name := strings.ToLower(tool.Name())
	desc := strings.ToLower(tool.Description())

	// Exact name match
	if name == query {
		return 1.0, "name"
	}

	// Name contains query
	if strings.Contains(name, query) {
		score := 0.9 * (float64(len(query)) / float64(len(name)))
		if score > maxScore {
			maxScore = score
			matchType = "name"
		}
	}

	// Description match
	if strings.Contains(desc, query) {
		words := strings.Fields(query)
		matchedWords := 0
		for _, word := range words {
			if strings.Contains(desc, word) {
				matchedWords++
			}
		}
		score := 0.7 * (float64(matchedWords) / float64(len(words)))
		if score > maxScore {
			maxScore = score
			matchType = "description"
		}
	}

	// Parameter name match
	for paramName := range tool.Parameters() {
		if strings.Contains(strings.ToLower(paramName), query) {
			score := 0.5
			if score > maxScore {
				maxScore = score
				matchType = "parameter"
			}
		}
	}

	// Fuzzy match as fallback
	if fuzzy && maxScore < 0.3 {
		fuzzyScore := tr.fuzzyMatch(name, query)
		if fuzzyScore > maxScore {
			maxScore = fuzzyScore
			matchType = "fuzzy"
		}
	}

	return maxScore, matchType
}

// fuzzyMatch calculates a fuzzy match score
func (tr *ToolRegistry) fuzzyMatch(s1, s2 string) float64 {
	if len(s1) == 0 || len(s2) == 0 {
		return 0
	}

	shorter, longer := s1, s2
	if len(s1) > len(s2) {
		shorter, longer = s2, s1
	}

	matches := 0
	for _, c := range shorter {
		if strings.ContainsRune(longer, c) {
			matches++
		}
	}

	return 0.5 * (float64(matches) / float64(len(longer)))
}

// sortSearchResults sorts results by score descending
func (tr *ToolRegistry) sortSearchResults(results []UnifiedSearchResult) {
	for i := 0; i < len(results)-1; i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].Score > results[i].Score {
				results[i], results[j] = results[j], results[i]
			}
		}
	}
}

// GetToolSuggestions returns suggestions based on partial input
func (tr *ToolRegistry) GetToolSuggestions(prefix string, maxSuggestions int) []Tool {
	if maxSuggestions <= 0 {
		maxSuggestions = 10
	}

	prefixLower := strings.ToLower(prefix)
	var suggestions []Tool

	for _, tool := range tr.tools.Snapshot() {
		if strings.HasPrefix(strings.ToLower(tool.Name()), prefixLower) {
			suggestions = append(suggestions, tool)
			if len(suggestions) >= maxSuggestions {
				break
			}
		}
	}

	return suggestions
}

// GetToolsBySource returns tools from a specific source
func (tr *ToolRegistry) GetToolsBySource(source string) []Tool {
	var tools []Tool
	sourceLower := strings.ToLower(source)

	for _, tool := range tr.tools.Snapshot() {
		if strings.ToLower(tool.Source()) == sourceLower {
			tools = append(tools, tool)
		}
	}

	return tools
}

// GetToolStats returns statistics about registered tools
func (tr *ToolRegistry) GetToolStats() map[string]interface{} {
	snap := tr.tools.Snapshot()
	sourceCounts := make(map[string]int)
	for _, tool := range snap {
		sourceCounts[tool.Source()]++
	}

	return map[string]interface{}{
		"total_tools":  len(snap),
		"by_source":    sourceCounts,
		"last_refresh": tr.lastRefresh,
		"custom_count": tr.customTools.Len(),
	}
}
