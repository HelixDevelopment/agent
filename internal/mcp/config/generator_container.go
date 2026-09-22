// Package config provides containerized MCP configuration generation for CLI agents
// This generator uses Docker containers instead of npx for all MCP servers,
// eliminating npm/npx dependencies completely.
package config

import (
	"fmt"
	"os"
	"strings"
)

// ContainerMCPServerConfig represents a containerized MCP server configuration
type ContainerMCPServerConfig struct {
	Type        string            `json:"type"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Env         map[string]string `json:"env,omitempty"` // Crush uses "env" instead of "environment"
	Enabled     bool              `json:"enabled"`
	Port        int               `json:"port"`
	Category    string            `json:"category"`
}

// MCPContainerPort defines the port allocation for each MCP server
type MCPContainerPort struct {
	Name     string
	Port     int
	Category string
}

// Port allocation scheme for containerized MCP servers
// Organized by category for easy management and no conflicts
var MCPContainerPorts = []MCPContainerPort{
	// TIER 1: Core Official MCP Servers (8200-8209)
	{"fetch", 8200, "core"},
	{"git", 8201, "core"},
	{"time", 8202, "core"},
	{"filesystem", 8203, "core"},
	{"memory", 8204, "core"},
	{"everything", 8205, "core"},
	{"sequential-thinking", 8206, "core"},
	{"sqlite", 8207, "core"},
	{"puppeteer", 8208, "core"},
	{"postgres", 8209, "core"},

	// TIER 2: Database MCP Servers (8210-8214)
	{"mongodb", 8210, "database"},
	{"redis", 8211, "database"},
	{"mysql", 8212, "database"},
	{"elasticsearch", 8213, "database"},
	{"supabase", 8214, "database"},

	// TIER 3: Vector Database MCP Servers (8215-8218)
	{"qdrant", 8215, "vector"},
	{"chroma", 8216, "vector"},
	{"pinecone", 8217, "vector"},
	{"weaviate", 8218, "vector"},

	// TIER 4: DevOps & Infrastructure (8220-8233)
	{"github", 8220, "devops"},
	{"gitlab", 8221, "devops"},
	{"sentry", 8222, "devops"},
	{"kubernetes", 8223, "devops"},
	{"docker", 8224, "devops"},
	{"ansible", 8225, "devops"},
	{"aws", 8226, "devops"},
	{"gcp", 8227, "devops"},
	{"heroku", 8228, "devops"},
	{"cloudflare", 8229, "devops"},
	{"vercel", 8230, "devops"},
	{"workers", 8231, "devops"},
	{"jetbrains", 8232, "devops"},
	{"k8s-alt", 8233, "devops"},

	// TIER 5: Browser & Web Automation (8234-8237)
	{"playwright", 8234, "browser"},
	{"browserbase", 8235, "browser"},
	{"firecrawl", 8236, "browser"},
	{"crawl4ai", 8237, "browser"},

	// TIER 6: Communication (8238-8240)
	{"slack", 8238, "communication"},
	{"discord", 8239, "communication"},
	{"telegram", 8240, "communication"},

	// TIER 7: Productivity & Project Management (8250-8259)
	{"notion", 8250, "productivity"},
	{"linear", 8251, "productivity"},
	{"jira", 8252, "productivity"},
	{"asana", 8253, "productivity"},
	{"trello", 8254, "productivity"},
	{"todoist", 8255, "productivity"},
	{"monday", 8256, "productivity"},
	{"airtable", 8257, "productivity"},
	{"obsidian", 8258, "productivity"},
	{"atlassian", 8259, "productivity"},

	// TIER 8: Search & AI (8260-8269)
	{"brave-search", 8260, "search"},
	{"exa", 8261, "search"},
	{"tavily", 8262, "search"},
	{"perplexity", 8263, "search"},
	{"kagi", 8264, "search"},
	{"omnisearch", 8265, "search"},
	{"context7", 8266, "search"},
	{"llamaindex", 8267, "search"},
	{"langchain", 8268, "search"},
	{"openai", 8269, "search"},

	// TIER 9: Google Services (8270-8274)
	{"google-drive", 8270, "google"},
	{"google-calendar", 8271, "google"},
	{"google-maps", 8272, "google"},
	{"youtube", 8273, "google"},
	{"gmail", 8274, "google"},

	// TIER 10: Monitoring & Observability (8275-8277)
	{"datadog", 8275, "monitoring"},
	{"grafana", 8276, "monitoring"},
	{"prometheus", 8277, "monitoring"},

	// TIER 11: Finance & Business (8278-8280)
	{"stripe", 8278, "finance"},
	{"hubspot", 8279, "finance"},
	{"zendesk", 8280, "finance"},

	// TIER 12: Design (8281-8282)
	{"figma", 8281, "design"},
	{"miro", 8282, "design"},

	// TIER 13: Growth blocks (8283-8290) — added 2026-09-03 alongside the
	// catalogue restore.
	//
	// Every pre-existing band above was allocated EXACTLY full (core 10/10,
	// devops 14/14, search 10/10, design 1/1, ...), so there was no free slot
	// anywhere for a newly restored integration. TestCompareWithNPXGenerator
	// requires every MCP the NPX generator emits to have a container
	// counterpart, so these needed real port allocations rather than being
	// left unlisted (resolveContainerEndpoints silently skips unallocated
	// names, which would have produced a remote entry with no URL).
	//
	// Each growth block is a contiguous per-category extension into previously
	// unallocated space, with headroom for the next addition. The band table
	// in generator_container_test.go is extended to match — the categories
	// stay bounded, they are just no longer bounded by a single range.
	{"duckduckgo", 8283, "search"}, // search growth block 8283-8284
	{"circleci", 8285, "devops"},   // devops growth block 8285-8286
	{"gcs", 8287, "cloud"},         // cloud block 8287-8288
	{"replicate", 8289, "ai"},      // ai block 8289-8290

	// TIER 14: HelixSkills (8291) — HXC-159 T-P6.02. Containerized
	// equivalent of the "local" go-run entry generator_full.go's
	// GenerateAllMCPs adds; TestCompareWithNPXGenerator requires every
	// name present there to also exist here.
	{"helixskills", 8291, "knowledge"},
}

// ContainerMCPConfigGenerator generates containerized MCP configurations
type ContainerMCPConfigGenerator struct {
	homeDir   string
	helixHome string
	baseURL   string
	mcpHost   string
	envVars   map[string]string
	portMap   map[string]MCPContainerPort
}

// NewContainerMCPConfigGenerator creates a new container MCP config generator
func NewContainerMCPConfigGenerator(baseURL string) *ContainerMCPConfigGenerator {
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = "/home"
	}

	helixHome := os.Getenv("HELIXAGENT_HOME")
	if helixHome == "" {
		helixHome = homeDir + "/.helixagent"
	}

	// MCP container host - can be overridden for remote deployments
	mcpHost := os.Getenv("MCP_CONTAINER_HOST")
	if mcpHost == "" {
		mcpHost = "localhost"
	}

	g := &ContainerMCPConfigGenerator{
		homeDir:   homeDir,
		helixHome: helixHome,
		baseURL:   baseURL,
		mcpHost:   mcpHost,
		envVars:   make(map[string]string),
		portMap:   make(map[string]MCPContainerPort),
	}

	// Build port map for quick lookups
	for _, p := range MCPContainerPorts {
		g.portMap[p.Name] = p
	}

	g.loadEnvVars()
	return g
}

// loadEnvVars loads environment variables from .env files and environment
func (g *ContainerMCPConfigGenerator) loadEnvVars() {
	// Load from multiple .env files
	envFiles := []string{".env", ".env.local", ".env.mcp", ".env.mcp.generated"}
	for _, file := range envFiles {
		if data, err := os.ReadFile(file); err == nil {
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					key := strings.TrimSpace(parts[0])
					value := strings.Trim(strings.TrimSpace(parts[1]), "\"'")
					g.envVars[key] = value
				}
			}
		}
	}

	// Also load from environment (overrides files)
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 {
			g.envVars[parts[0]] = parts[1]
		}
	}
}

// hasEnvVar checks if an environment variable is set and non-empty
func (g *ContainerMCPConfigGenerator) hasEnvVar(name string) bool {
	val, ok := g.envVars[name]
	return ok && val != ""
}

// hasAnyEnvVar checks if any of the given environment variables is set
func (g *ContainerMCPConfigGenerator) hasAnyEnvVar(names ...string) bool {
	for _, name := range names {
		if g.hasEnvVar(name) {
			return true
		}
	}
	return false
}

// hasAllEnvVars checks if all of the given environment variables are set
func (g *ContainerMCPConfigGenerator) hasAllEnvVars(names ...string) bool {
	for _, name := range names {
		if !g.hasEnvVar(name) {
			return false
		}
	}
	return true
}

// getMCPURL returns the container URL for an MCP server
func (g *ContainerMCPConfigGenerator) getMCPURL(name string) string {
	if port, ok := g.portMap[name]; ok {
		return fmt.Sprintf("http://%s:%d/sse", g.mcpHost, port.Port)
	}
	return ""
}

// GenerateContainerMCPs generates ALL MCP configurations using containers
// ZERO npx commands - all MCPs use containerized remote endpoints
func (g *ContainerMCPConfigGenerator) GenerateContainerMCPs() map[string]ContainerMCPServerConfig {
	mcps := make(map[string]ContainerMCPServerConfig)

	// ==========================================================================
	// CATEGORY 1: HelixAgent Core (Always enabled - remote endpoint)
	// ==========================================================================
	mcps["helixagent"] = ContainerMCPServerConfig{
		Type:     "remote",
		URL:      g.baseURL + "/mcp/sse",
		Enabled:  true,
		Port:     0,
		Category: "helixagent",
	}

	// ==========================================================================
	// CATEGORY 2: Core MCP Servers (Always available - no API keys needed)
	// ==========================================================================
	mcps["fetch"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true,
		Category: "core",
	}
	mcps["git"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true,
		Category: "core",
	}
	mcps["time"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true,
		Category: "core",
	}
	mcps["filesystem"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true,
		Category: "core",
	}
	mcps["memory"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true,
		Category: "core",
	}
	mcps["everything"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true,
		Category: "core",
	}
	mcps["sequential-thinking"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true,
		Category: "core",
	}
	mcps["sqlite"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true,
		Category: "core",
	}
	mcps["puppeteer"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true,
		Category: "core",
	}
	mcps["postgres"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAnyEnvVar("POSTGRES_URL", "POSTGRES_HOST"),
		Category: "core",
	}

	// ==========================================================================
	// CATEGORY 3: Database MCP Servers (Enabled if backend available)
	// ==========================================================================
	mcps["mongodb"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAnyEnvVar("MONGODB_URI", "MONGODB_HOST"),
		Category: "database",
	}
	mcps["redis"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAnyEnvVar("REDIS_URL", "REDIS_HOST"),
		Category: "database",
	}
	mcps["mysql"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAnyEnvVar("MYSQL_URL", "MYSQL_HOST"),
		Category: "database",
	}
	mcps["elasticsearch"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAnyEnvVar("ELASTICSEARCH_URL", "ELASTICSEARCH_HOST"),
		Category: "database",
	}
	mcps["supabase"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAllEnvVars("SUPABASE_URL", "SUPABASE_KEY"),
		Category: "database",
	}

	// ==========================================================================
	// CATEGORY 4: Vector Database MCP Servers
	// ==========================================================================
	mcps["qdrant"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAnyEnvVar("QDRANT_URL", "QDRANT_HOST"),
		Category: "vector",
	}
	mcps["chroma"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAnyEnvVar("CHROMA_URL", "CHROMA_HOST"),
		Category: "vector",
	}
	mcps["pinecone"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("PINECONE_API_KEY"),
		Category: "vector",
	}
	mcps["weaviate"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAnyEnvVar("WEAVIATE_URL", "WEAVIATE_HOST"),
		Category: "vector",
	}

	// ==========================================================================
	// CATEGORY 5: DevOps & Infrastructure
	// ==========================================================================
	mcps["github"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("GITHUB_TOKEN"),
		Category: "devops",
	}
	mcps["gitlab"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("GITLAB_TOKEN"),
		Category: "devops",
	}
	mcps["sentry"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAllEnvVars("SENTRY_AUTH_TOKEN", "SENTRY_ORG"),
		Category: "devops",
	}
	mcps["kubernetes"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("KUBECONFIG"),
		Category: "devops",
	}
	mcps["docker"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true, // Always available - uses local docker socket
		Category: "devops",
	}
	mcps["ansible"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true, // Always available
		Category: "devops",
	}
	mcps["aws"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAllEnvVars("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"),
		Category: "devops",
	}
	mcps["gcp"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("GOOGLE_APPLICATION_CREDENTIALS"),
		Category: "devops",
	}
	mcps["heroku"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("HEROKU_API_KEY"),
		Category: "devops",
	}
	mcps["cloudflare"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("CLOUDFLARE_API_TOKEN"),
		Category: "devops",
	}
	mcps["vercel"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("VERCEL_TOKEN"),
		Category: "devops",
	}
	mcps["workers"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("CLOUDFLARE_API_TOKEN"),
		Category: "devops",
	}
	mcps["jetbrains"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true, // Works locally
		Category: "devops",
	}

	// ==========================================================================
	// CATEGORY 6: Browser & Web Automation
	// ==========================================================================
	mcps["playwright"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true, // Always available
		Category: "browser",
	}
	mcps["browserbase"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("BROWSERBASE_API_KEY"),
		Category: "browser",
	}
	mcps["firecrawl"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("FIRECRAWL_API_KEY"),
		Category: "browser",
	}
	mcps["crawl4ai"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true, // Works locally
		Category: "browser",
	}

	// ==========================================================================
	// CATEGORY 7: Communication
	// ==========================================================================
	mcps["slack"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAllEnvVars("SLACK_BOT_TOKEN", "SLACK_TEAM_ID"),
		Category: "communication",
	}
	mcps["discord"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("DISCORD_TOKEN"),
		Category: "communication",
	}
	mcps["telegram"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("TELEGRAM_BOT_TOKEN"),
		Category: "communication",
	}

	// ==========================================================================
	// CATEGORY 8: Productivity & Project Management
	// ==========================================================================
	mcps["notion"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("NOTION_API_KEY"),
		Category: "productivity",
	}
	mcps["linear"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("LINEAR_API_KEY"),
		Category: "productivity",
	}
	mcps["jira"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAllEnvVars("JIRA_URL", "JIRA_EMAIL", "JIRA_API_TOKEN"),
		Category: "productivity",
	}
	mcps["asana"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("ASANA_ACCESS_TOKEN"),
		Category: "productivity",
	}
	mcps["trello"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAllEnvVars("TRELLO_API_KEY", "TRELLO_API_TOKEN"),
		Category: "productivity",
	}
	mcps["todoist"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("TODOIST_API_TOKEN"),
		Category: "productivity",
	}
	mcps["monday"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("MONDAY_API_TOKEN"),
		Category: "productivity",
	}
	mcps["airtable"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("AIRTABLE_API_KEY"),
		Category: "productivity",
	}
	mcps["obsidian"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("OBSIDIAN_VAULT_PATH"),
		Category: "productivity",
	}
	mcps["atlassian"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAllEnvVars("ATLASSIAN_URL", "ATLASSIAN_EMAIL", "ATLASSIAN_API_TOKEN"),
		Category: "productivity",
	}

	// ==========================================================================
	// CATEGORY 9: Search & AI
	// ==========================================================================
	mcps["brave-search"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("BRAVE_API_KEY"),
		Category: "search",
	}
	mcps["exa"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("EXA_API_KEY"),
		Category: "search",
	}
	mcps["tavily"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("TAVILY_API_KEY"),
		Category: "search",
	}
	mcps["perplexity"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("PERPLEXITY_API_KEY"),
		Category: "search",
	}
	mcps["kagi"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("KAGI_API_KEY"),
		Category: "search",
	}
	mcps["omnisearch"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true, // Works without API key
		Category: "search",
	}
	mcps["context7"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true, // Works without API key
		Category: "search",
	}
	mcps["llamaindex"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("OPENAI_API_KEY"),
		Category: "search",
	}
	mcps["langchain"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("OPENAI_API_KEY"),
		Category: "search",
	}
	mcps["openai"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("OPENAI_API_KEY"),
		Category: "search",
	}

	// ==========================================================================
	// CATEGORY 10: Google Services
	// ==========================================================================
	mcps["google-drive"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAllEnvVars("GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"),
		Category: "google",
	}
	mcps["google-calendar"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAllEnvVars("GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"),
		Category: "google",
	}
	mcps["google-maps"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("GOOGLE_MAPS_API_KEY"),
		Category: "google",
	}
	mcps["youtube"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("YOUTUBE_API_KEY"),
		Category: "google",
	}
	mcps["gmail"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAllEnvVars("GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"),
		Category: "google",
	}

	// ==========================================================================
	// CATEGORY 11: Monitoring & Observability
	// ==========================================================================
	mcps["datadog"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAllEnvVars("DD_API_KEY", "DD_APP_KEY"),
		Category: "monitoring",
	}
	mcps["grafana"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAllEnvVars("GRAFANA_URL", "GRAFANA_TOKEN"),
		Category: "monitoring",
	}
	mcps["prometheus"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("PROMETHEUS_URL"),
		Category: "monitoring",
	}

	// ==========================================================================
	// CATEGORY 12: Finance & Business
	// ==========================================================================
	mcps["stripe"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("STRIPE_API_KEY"),
		Category: "finance",
	}
	mcps["hubspot"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("HUBSPOT_ACCESS_TOKEN"),
		Category: "finance",
	}
	mcps["zendesk"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasAllEnvVars("ZENDESK_SUBDOMAIN", "ZENDESK_EMAIL", "ZENDESK_TOKEN"),
		Category: "finance",
	}

	// ==========================================================================
	// CATEGORY 13: Design
	// ==========================================================================
	mcps["figma"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("FIGMA_API_KEY"),
		Category: "design",
	}

	// Restored 2026-09-03 — container counterparts for the integrations
	// restored in the NPX generator. Each gates on the env var its verified
	// replacement package actually reads (see the annotated NPX entries);
	// duckduckgo needs no credential, so it is unconditionally enabled.
	mcps["miro"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("MIRO_ACCESS_TOKEN"),
		Category: "design",
	}
	mcps["duckduckgo"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  true,
		Category: "search",
	}
	mcps["gcs"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("GOOGLE_CLOUD_PROJECT"),
		Category: "cloud",
	}
	mcps["circleci"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("CIRCLECI_TOKEN"),
		Category: "devops",
	}
	mcps["replicate"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("REPLICATE_API_TOKEN"),
		Category: "ai",
	}

	// HXC-159 T-P6.02: containerized HelixSkills MCP server. The "local"
	// generator (generator_full.go) shells out to `go run ./cmd/server
	// --mcp stdio` directly; the containerized equivalent instead reaches
	// it as a remote SSE endpoint on a container this project's Containers
	// Submodule (§11.4.76) is responsible for booting — this generator
	// never spawns processes itself, matching every other entry here.
	// Gated on the same HELIXSKILLS_MODULE_PATH opt-in as the local
	// generator so the two stay behaviourally aligned (both inert until an
	// operator opts in).
	mcps["helixskills"] = ContainerMCPServerConfig{
		Type:     "remote",
		Enabled:  g.hasEnvVar("HELIXSKILLS_MODULE_PATH"),
		Category: "knowledge",
	}

	g.resolveContainerEndpoints(mcps)

	return mcps
}

// resolveContainerEndpoints derives the URL and the Port of every
// containerized MCP from ONE lookup in MCPContainerPorts, keyed by the name
// the server is registered under in `mcps`.
//
// WHY THIS EXISTS (2026-08-09)
//
// Both fields used to be written by hand in each of the 71 config literals:
// the URL via g.getMCPURL(name) — which reads MCPContainerPorts — and the
// Port as a bare integer. When the allocation table was migrated to the 82xx
// scheme the URLs followed automatically and the hand-written ports did not,
// so every single entry ended up contradicting itself:
//
//	fetch:  URL=http://localhost:8200/sse   Port=9101
//	github: URL=http://localhost:8220/sse   Port=9401
//
// A consumer dialling cfg.URL reached the container; one composing an address
// from cfg.Port reached a port nothing listens on. Measured pre-fix: 71 of 71
// container-backed entries disagreed (TestContainerMCPPortAgreesWithURL).
//
// Deriving both here — from the same table, keyed by the same map key — makes
// the endpoint and the port two renderings of ONE fact rather than two facts
// that have to be kept equal by hand. There is no name string to mistype: the
// key the caller registered under IS the lookup key.
//
// Names absent from MCPContainerPorts are left untouched, which is what
// "helixagent" needs: it is a remote endpoint on the HelixAgent server itself
// (URL from g.baseURL, Port 0), not one of the containerized MCP ports.
func (g *ContainerMCPConfigGenerator) resolveContainerEndpoints(mcps map[string]ContainerMCPServerConfig) {
	for name, cfg := range mcps {
		alloc, ok := g.portMap[name]
		if !ok {
			continue
		}
		cfg.Port = alloc.Port
		cfg.URL = g.getMCPURL(name)
		mcps[name] = cfg
	}
}

// GetEnabledContainerMCPs returns only the MCPs that are enabled
func (g *ContainerMCPConfigGenerator) GetEnabledContainerMCPs() map[string]ContainerMCPServerConfig {
	all := g.GenerateContainerMCPs()
	enabled := make(map[string]ContainerMCPServerConfig)

	for name, cfg := range all {
		if cfg.Enabled {
			enabled[name] = cfg
		}
	}

	return enabled
}

// GetDisabledContainerMCPs returns MCPs that are disabled with reasons
func (g *ContainerMCPConfigGenerator) GetDisabledContainerMCPs() map[string]string {
	all := g.GenerateContainerMCPs()
	disabled := make(map[string]string)

	for name, cfg := range all {
		if !cfg.Enabled {
			reason := g.getDisableReason(name)
			disabled[name] = reason
		}
	}

	return disabled
}

// getDisableReason returns the reason why an MCP is disabled
func (g *ContainerMCPConfigGenerator) getDisableReason(name string) string {
	// Map of MCP names to their required environment variables
	requirements := map[string][]string{
		"gitlab":          {"GITLAB_TOKEN"},
		"slack":           {"SLACK_BOT_TOKEN", "SLACK_TEAM_ID"},
		"discord":         {"DISCORD_TOKEN"},
		"telegram":        {"TELEGRAM_BOT_TOKEN"},
		"notion":          {"NOTION_API_KEY"},
		"linear":          {"LINEAR_API_KEY"},
		"jira":            {"JIRA_URL", "JIRA_EMAIL", "JIRA_API_TOKEN"},
		"asana":           {"ASANA_ACCESS_TOKEN"},
		"trello":          {"TRELLO_API_KEY", "TRELLO_API_TOKEN"},
		"todoist":         {"TODOIST_API_TOKEN"},
		"monday":          {"MONDAY_API_TOKEN"},
		"brave-search":    {"BRAVE_API_KEY"},
		"exa":             {"EXA_API_KEY"},
		"tavily":          {"TAVILY_API_KEY"},
		"perplexity":      {"PERPLEXITY_API_KEY"},
		"kagi":            {"KAGI_API_KEY"},
		"cloudflare":      {"CLOUDFLARE_API_TOKEN"},
		"vercel":          {"VERCEL_TOKEN"},
		"heroku":          {"HEROKU_API_KEY"},
		"aws":             {"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"},
		"gcp":             {"GOOGLE_APPLICATION_CREDENTIALS"},
		"supabase":        {"SUPABASE_URL", "SUPABASE_KEY"},
		"google-drive":    {"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"},
		"google-calendar": {"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"},
		"google-maps":     {"GOOGLE_MAPS_API_KEY"},
		"youtube":         {"YOUTUBE_API_KEY"},
		"gmail":           {"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"},
		"datadog":         {"DD_API_KEY", "DD_APP_KEY"},
		"grafana":         {"GRAFANA_URL", "GRAFANA_TOKEN"},
		"prometheus":      {"PROMETHEUS_URL"},
		"stripe":          {"STRIPE_API_KEY"},
		"hubspot":         {"HUBSPOT_ACCESS_TOKEN"},
		"zendesk":         {"ZENDESK_SUBDOMAIN", "ZENDESK_EMAIL", "ZENDESK_TOKEN"},
		"browserbase":     {"BROWSERBASE_API_KEY"},
		"firecrawl":       {"FIRECRAWL_API_KEY"},
		"openai":          {"OPENAI_API_KEY"},
		"figma":           {"FIGMA_API_KEY"},
		"obsidian":        {"OBSIDIAN_VAULT_PATH"},
		"sentry":          {"SENTRY_AUTH_TOKEN", "SENTRY_ORG"},
		"pinecone":        {"PINECONE_API_KEY"},
		"kubernetes":      {"KUBECONFIG"},
		"postgres":        {"POSTGRES_URL", "POSTGRES_HOST"},
		"redis":           {"REDIS_URL", "REDIS_HOST"},
		"mongodb":         {"MONGODB_URI", "MONGODB_HOST"},
		"mysql":           {"MYSQL_URL", "MYSQL_HOST"},
		"elasticsearch":   {"ELASTICSEARCH_URL", "ELASTICSEARCH_HOST"},
		"qdrant":          {"QDRANT_URL", "QDRANT_HOST"},
		"chroma":          {"CHROMA_URL", "CHROMA_HOST"},
		"weaviate":        {"WEAVIATE_URL", "WEAVIATE_HOST"},
		"atlassian":       {"ATLASSIAN_URL", "ATLASSIAN_EMAIL", "ATLASSIAN_API_TOKEN"},
		"airtable":        {"AIRTABLE_API_KEY"},
		"llamaindex":      {"OPENAI_API_KEY"},
		"langchain":       {"OPENAI_API_KEY"},
		"github":          {"GITHUB_TOKEN"},
	}

	if reqs, ok := requirements[name]; ok {
		var missing []string
		for _, req := range reqs {
			if !g.hasEnvVar(req) {
				missing = append(missing, req)
			}
		}
		if len(missing) > 0 {
			return "Missing: " + strings.Join(missing, ", ")
		}
	}

	return "Unknown reason"
}

// GenerateSummary returns a summary of enabled/disabled MCPs
func (g *ContainerMCPConfigGenerator) GenerateSummary() map[string]interface{} {
	all := g.GenerateContainerMCPs()

	enabled := []string{}
	disabled := []string{}
	byCategory := make(map[string]int)

	for name, cfg := range all {
		if cfg.Enabled {
			enabled = append(enabled, name)
			byCategory[cfg.Category]++
		} else {
			disabled = append(disabled, name)
		}
	}

	return map[string]interface{}{
		"total":            len(all),
		"total_enabled":    len(enabled),
		"total_disabled":   len(disabled),
		"enabled_mcps":     enabled,
		"disabled_mcps":    disabled,
		"by_category":      byCategory,
		"container_host":   g.mcpHost,
		"npx_dependencies": 0, // ZERO npx dependencies!
	}
}

// GetPortAllocations returns the complete port allocation map
func (g *ContainerMCPConfigGenerator) GetPortAllocations() []MCPContainerPort {
	return MCPContainerPorts
}

// ValidatePortAllocations checks for port conflicts
func (g *ContainerMCPConfigGenerator) ValidatePortAllocations() error {
	usedPorts := make(map[int]string)

	for _, p := range MCPContainerPorts {
		if existing, ok := usedPorts[p.Port]; ok {
			return fmt.Errorf("port conflict: port %d used by both %s and %s", p.Port, existing, p.Name)
		}
		usedPorts[p.Port] = p.Name
	}

	return nil
}

// GetMCPsByCategory returns MCPs grouped by category
func (g *ContainerMCPConfigGenerator) GetMCPsByCategory() map[string][]ContainerMCPServerConfig {
	all := g.GenerateContainerMCPs()
	byCategory := make(map[string][]ContainerMCPServerConfig)

	for _, cfg := range all {
		byCategory[cfg.Category] = append(byCategory[cfg.Category], cfg)
	}

	return byCategory
}

// ContainsNPX checks if any MCP configuration uses npx (should always return false)
func (g *ContainerMCPConfigGenerator) ContainsNPX() bool {
	// This generator uses ONLY container URLs, no npx commands
	return false
}

// GetTotalMCPCount returns the total number of MCP servers defined
func (g *ContainerMCPConfigGenerator) GetTotalMCPCount() int {
	return len(g.GenerateContainerMCPs())
}
