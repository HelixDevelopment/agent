// Package config provides comprehensive MCP configuration generation for CLI agents
package config

import (
	"fmt"
	"os"
	"strings"
)

// MCPServerConfigFull represents a full MCP server configuration with all fields
type MCPServerConfigFull struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Env         map[string]string `json:"env,omitempty"` // Crush uses "env" instead of "environment"
	Enabled     bool              `json:"enabled"`
}

// MCPCategory represents a category of MCPs
type MCPCategory struct {
	Name        string
	Description string
	MCPs        []string
}

// FullMCPConfigGenerator generates comprehensive MCP configurations
type FullMCPConfigGenerator struct {
	homeDir   string
	helixHome string
	baseURL   string
	envVars   map[string]string
}

// NewFullMCPConfigGenerator creates a new full MCP config generator
func NewFullMCPConfigGenerator(baseURL string) *FullMCPConfigGenerator {
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = "/home"
	}

	helixHome := os.Getenv("HELIXAGENT_HOME")
	if helixHome == "" {
		helixHome = homeDir + "/.helixagent"
	}

	g := &FullMCPConfigGenerator{
		homeDir:   homeDir,
		helixHome: helixHome,
		baseURL:   baseURL,
		envVars:   make(map[string]string),
	}

	g.loadEnvVars()
	return g
}

// loadEnvVars loads environment variables from .env files and environment
func (g *FullMCPConfigGenerator) loadEnvVars() {
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
func (g *FullMCPConfigGenerator) hasEnvVar(name string) bool {
	val, ok := g.envVars[name]
	return ok && val != ""
}

// hasAnyEnvVar checks if any of the given environment variables is set
func (g *FullMCPConfigGenerator) hasAnyEnvVar(names ...string) bool {
	for _, name := range names {
		if g.hasEnvVar(name) {
			return true
		}
	}
	return false
}

// hasAllEnvVars checks if all of the given environment variables are set
func (g *FullMCPConfigGenerator) hasAllEnvVars(names ...string) bool {
	for _, name := range names {
		if !g.hasEnvVar(name) {
			return false
		}
	}
	return true
}

// expandEnvValue expands {env:VAR_NAME} placeholders with actual environment variable values
func (g *FullMCPConfigGenerator) expandEnvValue(value string) string {
	if strings.HasPrefix(value, "{env:") && strings.HasSuffix(value, "}") {
		envVar := strings.TrimSuffix(strings.TrimPrefix(value, "{env:"), "}")
		if val, ok := g.envVars[envVar]; ok && val != "" {
			return val
		}
	}
	return value
}

// expandEnvMap expands all {env:...} placeholders in an environment map
func (g *FullMCPConfigGenerator) expandEnvMap(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	expanded := make(map[string]string, len(env))
	for k, v := range env {
		expanded[k] = g.expandEnvValue(v)
	}
	return expanded
}

// GenerateAllMCPs generates ALL MCP configurations, marking which are enabled
func (g *FullMCPConfigGenerator) GenerateAllMCPs() map[string]MCPServerConfigFull {
	mcps := make(map[string]MCPServerConfigFull)

	// ==========================================================================
	// CATEGORY 1: HelixAgent Core (Always enabled)
	// ==========================================================================
	mcps["helixagent"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"node", g.helixHome + "/plugins/mcp-server/dist/index.js", "--endpoint", g.baseURL},
		Enabled: true,
	}

	// ==========================================================================
	// CATEGORY 2: Anthropic Official MCPs (No API keys, always work)
	// ==========================================================================
	mcps["filesystem"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@modelcontextprotocol/server-filesystem", g.homeDir},
		Enabled: true,
	}
	mcps["fetch"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-fetch-server"},
		Enabled: true,
	}
	mcps["memory"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@modelcontextprotocol/server-memory"},
		Enabled: true,
	}
	mcps["time"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@theo.foobar/mcp-time"},
		Enabled: true,
	}
	mcps["git"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-git"},
		Enabled: true,
	}
	mcps["sequential-thinking"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@modelcontextprotocol/server-sequential-thinking"},
		Enabled: true,
	}
	mcps["everything"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@modelcontextprotocol/server-everything"},
		Enabled: true,
	}
	mcps["sqlite"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-sqlite-npx", "/tmp/helixagent.db"},
		Enabled: true,
	}
	mcps["puppeteer"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@modelcontextprotocol/server-puppeteer"},
		Enabled: true,
	}

	// ==========================================================================
	// CATEGORY 3: Database MCPs (Local services - enabled if env vars set)
	// ==========================================================================
	mcps["postgres"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@modelcontextprotocol/server-postgres"},
		Environment: g.expandEnvMap(map[string]string{
			"POSTGRES_URL": g.getEnvOrDefault("POSTGRES_URL", fmt.Sprintf("postgresql://%s:%s@%s:%s/%s",
				g.getEnvOrDefault("POSTGRES_USER", "helixagent"),
				os.Getenv("POSTGRES_PASSWORD"),
				g.getEnvOrDefault("POSTGRES_HOST", "localhost"),
				g.getEnvOrDefault("POSTGRES_PORT", "15432"),
				g.getEnvOrDefault("POSTGRES_DB", "helixagent_db"),
			)),
		}),
		Enabled: g.hasAnyEnvVar("POSTGRES_URL", "POSTGRES_HOST"),
	}
	// Fixed 2026-09-03. `mcp-server-redis` is an npm SECURITY HOLDING package
	// (resolves, installs nothing). Real package:
	// @modelcontextprotocol/server-redis, which reads the URL from
	// process.argv[2] — the REDIS_URL env this block set was never consulted,
	// so the URL is now passed positionally.
	mcps["redis"] = MCPServerConfigFull{
		Type: "local",
		Command: []string{"npx", "-y", "@modelcontextprotocol/server-redis",
			g.getEnvOrDefault("REDIS_URL", fmt.Sprintf("redis://:%s@%s:%s",
				os.Getenv("REDIS_PASSWORD"),
				g.getEnvOrDefault("REDIS_HOST", "localhost"),
				g.getEnvOrDefault("REDIS_PORT", "16379"),
			))},
		Enabled: g.hasAnyEnvVar("REDIS_URL", "REDIS_HOST"),
	}
	mcps["mongodb"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mongodb-mcp-server"},
		Environment: g.expandEnvMap(map[string]string{
			"MONGODB_URI": g.getEnvOrDefault("MONGODB_URI", fmt.Sprintf("mongodb://%s:%s@%s:%s/%s?authSource=admin",
				g.getEnvOrDefault("MONGODB_USER", "helixagent"),
				os.Getenv("MONGODB_PASSWORD"),
				g.getEnvOrDefault("MONGODB_HOST", "localhost"),
				g.getEnvOrDefault("MONGODB_PORT", "27017"),
				g.getEnvOrDefault("MONGODB_DB", "helixagent"),
			)),
		}),
		Enabled: g.hasAnyEnvVar("MONGODB_URI", "MONGODB_HOST"),
	}
	mcps["mysql"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-mysql"},
		Environment: g.expandEnvMap(map[string]string{
			"MYSQL_URL": g.getEnvOrDefault("MYSQL_URL", fmt.Sprintf("mysql://%s:%s@%s:%s/%s",
				g.getEnvOrDefault("MYSQL_USER", "helixagent"),
				os.Getenv("MYSQL_PASSWORD"),
				g.getEnvOrDefault("MYSQL_HOST", "localhost"),
				g.getEnvOrDefault("MYSQL_PORT", "3306"),
				g.getEnvOrDefault("MYSQL_DB", "helixagent"),
			)),
		}),
		Enabled: g.hasAnyEnvVar("MYSQL_URL", "MYSQL_HOST"),
	}
	mcps["elasticsearch"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-elasticsearch"},
		Environment: g.expandEnvMap(map[string]string{
			"ELASTICSEARCH_URL": g.getEnvOrDefault("ELASTICSEARCH_URL", "http://localhost:9200"),
		}),
		Enabled: g.hasAnyEnvVar("ELASTICSEARCH_URL", "ELASTICSEARCH_HOST"),
	}

	// ==========================================================================
	// CATEGORY 4: Vector Databases (Local services)
	// ==========================================================================
	mcps["qdrant"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-qdrant"},
		Environment: g.expandEnvMap(map[string]string{
			"QDRANT_URL": g.getEnvOrDefault("QDRANT_URL", "http://localhost:6333"),
		}),
		Enabled: g.hasAnyEnvVar("QDRANT_URL", "QDRANT_HOST"),
	}
	mcps["chroma"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-chroma"},
		Environment: g.expandEnvMap(map[string]string{
			"CHROMA_URL": g.getEnvOrDefault("CHROMA_URL", "http://localhost:8000"),
		}),
		Enabled: g.hasAnyEnvVar("CHROMA_URL", "CHROMA_HOST"),
	}
	mcps["pinecone"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-pinecone"},
		Environment: g.expandEnvMap(map[string]string{
			"PINECONE_API_KEY": "{env:PINECONE_API_KEY}",
		}),
		Enabled: g.hasEnvVar("PINECONE_API_KEY"),
	}
	mcps["weaviate"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-weaviate"},
		Environment: g.expandEnvMap(map[string]string{
			"WEAVIATE_URL": g.getEnvOrDefault("WEAVIATE_URL", "http://localhost:8080"),
		}),
		Enabled: g.hasAnyEnvVar("WEAVIATE_URL", "WEAVIATE_HOST"),
	}

	// ==========================================================================
	// CATEGORY 5: DevOps & Infrastructure
	// ==========================================================================
	mcps["docker"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-docker"},
		Enabled: true, // Always enabled - uses local docker/podman
	}
	mcps["kubernetes"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-kubernetes"},
		Environment: g.expandEnvMap(map[string]string{
			"KUBECONFIG": g.getEnvOrDefault("KUBECONFIG", g.homeDir+"/.kube/config"),
		}),
		Enabled: g.hasEnvVar("KUBECONFIG"),
	}

	// ==========================================================================
	// CATEGORY 6: Development Platforms
	// ==========================================================================
	mcps["github"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@modelcontextprotocol/server-github"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"GITHUB_PERSONAL_ACCESS_TOKEN": "{env:GITHUB_TOKEN}",
		}),
		Enabled: g.hasEnvVar("GITHUB_TOKEN"),
	}
	mcps["gitlab"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@modelcontextprotocol/server-gitlab"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"GITLAB_PERSONAL_ACCESS_TOKEN": "{env:GITLAB_TOKEN}",
		}),
		Enabled: g.hasEnvVar("GITLAB_TOKEN"),
	}
	mcps["sentry"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@sentry/mcp-server"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"SENTRY_AUTH_TOKEN": "{env:SENTRY_AUTH_TOKEN}",
			"SENTRY_ORG":        "{env:SENTRY_ORG}",
		}),
		Enabled: g.hasAllEnvVars("SENTRY_AUTH_TOKEN", "SENTRY_ORG"),
	}

	// ==========================================================================
	// CATEGORY 7: Communication & Collaboration
	// ==========================================================================
	mcps["slack"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@modelcontextprotocol/server-slack"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"SLACK_BOT_TOKEN": "{env:SLACK_BOT_TOKEN}",
			"SLACK_TEAM_ID":   "{env:SLACK_TEAM_ID}",
		}),
		Enabled: g.hasAllEnvVars("SLACK_BOT_TOKEN", "SLACK_TEAM_ID"),
	}
	mcps["discord"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-discord"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"DISCORD_TOKEN": "{env:DISCORD_TOKEN}",
		}),
		Enabled: g.hasEnvVar("DISCORD_TOKEN"),
	}
	mcps["telegram"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-telegram"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"TELEGRAM_BOT_TOKEN": "{env:TELEGRAM_BOT_TOKEN}",
		}),
		Enabled: g.hasEnvVar("TELEGRAM_BOT_TOKEN"),
	}

	// ==========================================================================
	// CATEGORY 8: Productivity & Project Management
	// ==========================================================================
	mcps["notion"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@notionhq/notion-mcp-server"},
		Environment: g.expandEnvMap(map[string]string{
			"NOTION_API_KEY": "{env:NOTION_API_KEY}",
		}),
		Enabled: g.hasEnvVar("NOTION_API_KEY"),
	}
	// Fixed 2026-09-03. `mcp-server-jira` was NOT in the reported seized set —
	// the sweep of all 123 referenced package names found it independently.
	// It RESOLVES with HTTP 200 while publishing ZERO versions: an empty
	// registry shell that installs nothing. An existence-only check passed it
	// for the same reason it passed the four security-holding names.
	// Replacement: @aashari/mcp-server-atlassian-jira v3.3.0, repo
	// github.com/aashari/mcp-server-atlassian-jira -> HTTP 200, ~14799
	// downloads/week, community publisher. It reads the ATLASSIAN_* triple,
	// not the JIRA_* variables this block set.
	mcps["jira"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@aashari/mcp-server-atlassian-jira"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"ATLASSIAN_SITE_NAME":  "{env:ATLASSIAN_SITE_NAME}",
			"ATLASSIAN_USER_EMAIL": "{env:ATLASSIAN_USER_EMAIL}",
			"ATLASSIAN_API_TOKEN":  "{env:ATLASSIAN_API_TOKEN}",
		}),
		Enabled: g.hasAllEnvVars("ATLASSIAN_SITE_NAME", "ATLASSIAN_USER_EMAIL", "ATLASSIAN_API_TOKEN"),
	}
	mcps["trello"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-trello"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"TRELLO_API_KEY":   "{env:TRELLO_API_KEY}",
			"TRELLO_API_TOKEN": "{env:TRELLO_API_TOKEN}",
		}),
		Enabled: g.hasAllEnvVars("TRELLO_API_KEY", "TRELLO_API_TOKEN"),
	}
	mcps["todoist"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@doist/todoist-mcp"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"TODOIST_API_TOKEN": "{env:TODOIST_API_TOKEN}",
		}),
		Enabled: g.hasEnvVar("TODOIST_API_TOKEN"),
	}
	mcps["monday"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-monday"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"MONDAY_API_TOKEN": "{env:MONDAY_API_TOKEN}",
		}),
		Enabled: g.hasEnvVar("MONDAY_API_TOKEN"),
	}

	// ==========================================================================
	// CATEGORY 9: Search & AI
	// ==========================================================================
	mcps["brave-search"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@modelcontextprotocol/server-brave-search"},
		Environment: g.expandEnvMap(map[string]string{
			"BRAVE_API_KEY": "{env:BRAVE_API_KEY}",
		}),
		Enabled: g.hasEnvVar("BRAVE_API_KEY"),
	}
	mcps["exa"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "exa-mcp-server"},
		Environment: g.expandEnvMap(map[string]string{
			"EXA_API_KEY": "{env:EXA_API_KEY}",
		}),
		Enabled: g.hasEnvVar("EXA_API_KEY"),
	}
	mcps["tavily"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "tavily-mcp"},
		Environment: g.expandEnvMap(map[string]string{
			"TAVILY_API_KEY": "{env:TAVILY_API_KEY}",
		}),
		Enabled: g.hasEnvVar("TAVILY_API_KEY"),
	}
	mcps["perplexity"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-perplexity"},
		Environment: g.expandEnvMap(map[string]string{
			"PERPLEXITY_API_KEY": "{env:PERPLEXITY_API_KEY}",
		}),
		Enabled: g.hasEnvVar("PERPLEXITY_API_KEY"),
	}

	// ==========================================================================
	// CATEGORY 10: Cloud Providers
	// ==========================================================================
	mcps["cloudflare"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@cloudflare/mcp-server-cloudflare"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"CLOUDFLARE_API_TOKEN": "{env:CLOUDFLARE_API_TOKEN}",
		}),
		Enabled: g.hasEnvVar("CLOUDFLARE_API_TOKEN"),
	}
	mcps["vercel"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-vercel"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"VERCEL_TOKEN": "{env:VERCEL_TOKEN}",
		}),
		Enabled: g.hasEnvVar("VERCEL_TOKEN"),
	}
	mcps["heroku"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-heroku"},
		Environment: g.expandEnvMap(map[string]string{
			"HEROKU_API_KEY": "{env:HEROKU_API_KEY}",
		}),
		Enabled: g.hasEnvVar("HEROKU_API_KEY"),
	}
	mcps["aws"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-aws"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"AWS_ACCESS_KEY_ID":     "{env:AWS_ACCESS_KEY_ID}",
			"AWS_SECRET_ACCESS_KEY": "{env:AWS_SECRET_ACCESS_KEY}",
			"AWS_REGION":            g.getEnvOrDefault("AWS_REGION", "us-east-1"),
		}),
		Enabled: g.hasAllEnvVars("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"),
	}
	mcps["gcp"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-gcp"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"GOOGLE_APPLICATION_CREDENTIALS": "{env:GOOGLE_APPLICATION_CREDENTIALS}",
		}),
		Enabled: g.hasEnvVar("GOOGLE_APPLICATION_CREDENTIALS"),
	}
	// Fixed 2026-09-03. `mcp-server-supabase` is an npm SECURITY HOLDING
	// package (resolves, installs nothing). Replacement verified live:
	// @supabase/mcp-server-supabase v0.11.0, repo github.com/supabase/mcp ->
	// HTTP 200, ~102077 downloads/week, published by Supabase — VENDOR.
	//
	// Its npm README is empty, so the flags were read from the published
	// tarball's own CLI parser (dist/transports/stdio.js), which accepts
	// --access-token / --api-url / --content-api-url / --features /
	// --project-ref / --read-only and reads SUPABASE_ACCESS_TOKEN from the
	// environment. It does NOT read SUPABASE_URL or SUPABASE_KEY, which is
	// what this block was setting. --read-only is the safe default for an
	// agent-driven server.
	mcps["supabase"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@supabase/mcp-server-supabase", "--read-only"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"SUPABASE_ACCESS_TOKEN": "{env:SUPABASE_ACCESS_TOKEN}",
		}),
		Enabled: g.hasEnvVar("SUPABASE_ACCESS_TOKEN"),
	}

	// ==========================================================================
	// CATEGORY 11: Google Services
	// ==========================================================================
	mcps["google-calendar"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-google-calendar"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"GOOGLE_CLIENT_ID":     "{env:GOOGLE_CLIENT_ID}",
			"GOOGLE_CLIENT_SECRET": "{env:GOOGLE_CLIENT_SECRET}",
		}),
		Enabled: g.hasAllEnvVars("GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"),
	}
	mcps["google-maps"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@modelcontextprotocol/server-google-maps"},
		Environment: g.expandEnvMap(map[string]string{
			"GOOGLE_MAPS_API_KEY": "{env:GOOGLE_MAPS_API_KEY}",
		}),
		Enabled: g.hasEnvVar("GOOGLE_MAPS_API_KEY"),
	}
	mcps["youtube"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-youtube"},
		Environment: g.expandEnvMap(map[string]string{
			"YOUTUBE_API_KEY": "{env:YOUTUBE_API_KEY}",
		}),
		Enabled: g.hasEnvVar("YOUTUBE_API_KEY"),
	}
	mcps["gmail"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-gmail"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"GOOGLE_CLIENT_ID":     "{env:GOOGLE_CLIENT_ID}",
			"GOOGLE_CLIENT_SECRET": "{env:GOOGLE_CLIENT_SECRET}",
		}),
		Enabled: g.hasAllEnvVars("GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"),
	}

	// ==========================================================================
	// CATEGORY 12: Monitoring & Observability
	// ==========================================================================

	// ==========================================================================
	// CATEGORY 13: Finance & Business
	// ==========================================================================
	mcps["stripe"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-stripe"},
		Environment: g.expandEnvMap(map[string]string{
			"STRIPE_API_KEY": "{env:STRIPE_API_KEY}",
		}),
		Enabled: g.hasEnvVar("STRIPE_API_KEY"),
	}
	mcps["hubspot"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@hubspot/mcp-server"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"HUBSPOT_ACCESS_TOKEN": "{env:HUBSPOT_ACCESS_TOKEN}",
		}),
		Enabled: g.hasEnvVar("HUBSPOT_ACCESS_TOKEN"),
	}

	// ==========================================================================
	// CATEGORY 14: Browser & Web
	// ==========================================================================
	mcps["browserbase"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@browserbasehq/mcp"},
		Environment: g.expandEnvMap(map[string]string{
			"BROWSERBASE_API_KEY": "{env:BROWSERBASE_API_KEY}",
		}),
		Enabled: g.hasEnvVar("BROWSERBASE_API_KEY"),
	}
	mcps["playwright"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@playwright/mcp"},
		Enabled: true, // Works locally
	}

	// ==========================================================================
	// CATEGORY 15: AI & OpenAI
	// ==========================================================================
	mcps["openai"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "mcp-server-openai"},
		Environment: g.expandEnvMap(map[string]string{
			"OPENAI_API_KEY": "{env:OPENAI_API_KEY}",
		}),
		Enabled: g.hasEnvVar("OPENAI_API_KEY"),
	}

	// ==========================================================================
	// CATEGORY 16: Design
	// ==========================================================================
	mcps["figma"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "figma-developer-mcp"},
		Environment: g.expandEnvMap(map[string]string{
			"FIGMA_API_KEY": "{env:FIGMA_API_KEY}",
		}),
		Enabled: g.hasEnvVar("FIGMA_API_KEY"),
	}

	// ==========================================================================
	// CATEGORY 17: Notes & Knowledge
	// ==========================================================================
	// Restored 2026-09-03. ce9c4eb9 removed "obsidian" here because
	// `mcp-server-obsidian` 404s. Replacement verified live:
	// obsidian-mcp-server v3.5.0 (2026-08-22, 57 published versions),
	// repo github.com/cyanheads/obsidian-mcp-server -> HTTP 200,
	// ~5784 downloads/week, community publisher (cyanheads).
	//
	// NOTE (honest divergence, not an oversight): cmd/helixagent/main.go still
	// configures obsidian as `mcp-obsidian` — a different, weaker package
	// (single version, published 2024-11-29, ~1444/week, declares NO
	// repository). This generator is a separate surface from the OpenCode
	// generator that TestGeneratorAndShippedConfigAgree pins, so the two do
	// not currently have to agree; unifying them is a follow-up, deliberately
	// not folded into a restore commit.
	mcps["obsidian"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "obsidian-mcp-server"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"OBSIDIAN_API_KEY":  "{env:OBSIDIAN_API_KEY}",
			"OBSIDIAN_BASE_URL": "{env:OBSIDIAN_BASE_URL}",
		}),
		Enabled: g.hasEnvVar("OBSIDIAN_API_KEY"),
	}

	// ==========================================================================
	// CATEGORY 18: Restored integrations (2026-09-03)
	// ==========================================================================
	// Each entry below replaces a name ce9c4eb9 removed as a verified npm 404.
	// Every replacement was re-verified live against registry.npmjs.org on
	// 2026-09-03, with its declared repository URL fetched and confirmed to
	// resolve — name similarity alone is never accepted as provenance. Where a
	// package declares no repository (replicate-mcp, @modelcontextprotocol/*),
	// provenance rests on the publishing account plus a vendor-domain homepage
	// or a scope shared with a known-good control package. Env names are the
	// ones each package actually reads; several differ from the variables the
	// removed entries declared, which is exactly the class of defect that made
	// the old sqlite entry unusable.

	// @jetbrains/mcp-proxy v1.8.0, repo github.com/JetBrains/mcp-jetbrains ->
	// HTTP 200, ~1454 downloads/week, published by JetBrains — VENDOR.
	mcps["jetbrains"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@jetbrains/mcp-proxy"},
		Enabled: true, // Talks to a locally running IDE; no credential needed.
	}

	// @tacticlaunch/mcp-linear v1.4.3, repo github.com/tacticlaunch/mcp-linear
	// -> HTTP 200, ~4856 downloads/week, community publisher.
	mcps["linear"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@tacticlaunch/mcp-linear"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"LINEAR_API_TOKEN": "{env:LINEAR_API_TOKEN}",
		}),
		Enabled: g.hasEnvVar("LINEAR_API_TOKEN"),
	}

	// @roychri/mcp-server-asana v1.8.0, repo
	// github.com/roychri/mcp-server-asana -> HTTP 200, ~3169 downloads/week,
	// community publisher.
	mcps["asana"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@roychri/mcp-server-asana"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"ASANA_ACCESS_TOKEN": "{env:ASANA_ACCESS_TOKEN}",
		}),
		Enabled: g.hasEnvVar("ASANA_ACCESS_TOKEN"),
	}

	// @modelcontextprotocol/server-gdrive v2025.1.14, ~6700 downloads/week,
	// MCP-org scope sharing the control package's maintainers — VENDOR, but
	// ARCHIVED upstream. Its README's env is GDRIVE_CREDENTIALS_PATH.
	mcps["google-drive"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@modelcontextprotocol/server-gdrive"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"GDRIVE_CREDENTIALS_PATH": "{env:GDRIVE_CREDENTIALS_PATH}",
		}),
		Enabled: g.hasEnvVar("GDRIVE_CREDENTIALS_PATH"),
	}

	// @winor30/mcp-server-datadog v1.8.0, repo
	// github.com/winor30/mcp-server-datadog -> HTTP 200, ~28570
	// downloads/week, community publisher. Reads DATADOG_*, not DD_*.
	mcps["datadog"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@winor30/mcp-server-datadog"},
		Environment: g.expandEnvMap(map[string]string{
			"DATADOG_API_KEY": "{env:DATADOG_API_KEY}",
			"DATADOG_APP_KEY": "{env:DATADOG_APP_KEY}",
		}),
		Enabled: g.hasAllEnvVars("DATADOG_API_KEY", "DATADOG_APP_KEY"),
	}

	// @circleci/mcp-server-circleci v0.20.0, repo
	// github.com/CircleCI-Public/mcp-server-circleci -> HTTP 200, ~34017
	// downloads/week, published by CircleCI — VENDOR.
	mcps["circleci"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@circleci/mcp-server-circleci"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"CIRCLECI_TOKEN": "{env:CIRCLECI_TOKEN}",
		}),
		Enabled: g.hasEnvVar("CIRCLECI_TOKEN"),
	}

	// prometheus-mcp v1.1.3, repo github.com/idanfishman/prometheus-mcp ->
	// HTTP 200, ~4823 downloads/week, community publisher. The positional
	// `stdio` subcommand is REQUIRED — the package's own README config block
	// is `npx prometheus-mcp@latest stdio`; without it the binary does not
	// speak stdio MCP at all.
	mcps["prometheus"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "prometheus-mcp", "stdio"},
		Environment: g.expandEnvMap(map[string]string{
			"PROMETHEUS_URL": "{env:PROMETHEUS_URL}",
		}),
		Enabled: g.hasEnvVar("PROMETHEUS_URL"),
	}

	// replicate-mcp v0.9.0 (90 versions), ~2000 downloads/week, published by
	// Replicate (maintainer `replicatebot`, homepage
	// replicate.com/docs/reference/mcp -> HTTP 200) — VENDOR. The npm README
	// is empty; the invocation and env come from Replicate's own docs page.
	mcps["replicate"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "replicate-mcp"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"REPLICATE_API_TOKEN": "{env:REPLICATE_API_TOKEN}",
		}),
		Enabled: g.hasEnvVar("REPLICATE_API_TOKEN"),
	}

	// @k-jarzyna/mcp-miro v1.0.11, repo github.com/k-jarzyna/mcp-miro ->
	// HTTP 200, community publisher (scope matches the repo owner).
	// Adoption is low (~229 downloads/week) — the resolving repository, not
	// the download count, is what carries provenance for this one.
	mcps["miro"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@k-jarzyna/mcp-miro"},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- not a credential (map key / config label / env-var reference)
			"MIRO_ACCESS_TOKEN": "{env:MIRO_ACCESS_TOKEN}",
		}),
		Enabled: g.hasEnvVar("MIRO_ACCESS_TOKEN"),
	}

	// duckduckgo-mcp-server v0.1.2, repo
	// github.com/zhsama/duckduckgo-mcp-server -> HTTP 200, ~3445
	// downloads/week, community publisher. No credential required.
	mcps["duckduckgo"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "duckduckgo-mcp-server"},
		Enabled: true,
	}

	// @google-cloud/storage-mcp v0.6.0, repo github.com/googleapis/gcloud-mcp
	// -> HTTP 200, ~20919 downloads/week, published by Google — VENDOR.
	// Auth is Application Default Credentials; the package's own variable is
	// GOOGLE_CLOUD_PROJECT.
	mcps["gcs"] = MCPServerConfigFull{
		Type:    "local",
		Command: []string{"npx", "-y", "@google-cloud/storage-mcp"},
		Environment: g.expandEnvMap(map[string]string{
			"GOOGLE_CLOUD_PROJECT": "{env:GOOGLE_CLOUD_PROJECT}",
		}),
		Enabled: g.hasEnvVar("GOOGLE_CLOUD_PROJECT"),
	}

	// HXC-159 T-P6.02: HelixSkills MCP server. github.com/HelixDevelopment/
	// skills ships its own MCP server (13 tools, package
	// github.com/HelixDevelopment/skills/cmd/server) run via
	// `go run ./cmd/server --mcp stdio` — that binary's own documented
	// invocation contract (cmd/server/main.go's package doc comment),
	// never a Go import of the module (D-4 / T-P6.05's module-graph gate
	// enforces that mechanically). `go run ./cmd/server` is a RELATIVE
	// package path, which Go only resolves from WITHIN that module's own
	// directory tree — this MCPServerConfigFull shape has no separate
	// working-directory field (every other entry here is a globally
	// resolvable `npx` package, which needs none), so the `cd <dir> &&`
	// wrapper is the portable way to express "run this in that directory"
	// inside a plain argv Command array; confirmed necessary by measured
	// failure (`go run <abs-path>/cmd/server` from HelixAgent's own module
	// directory: "directory ... outside main module or its selected
	// dependencies") before landing this exact form.
	// HELIXSKILLS_MODULE_PATH is the config-injected checkout root
	// (CONST-051(C) dependency layout, e.g.
	// "<consuming-project-root>/submodules/skills"); disabled — present
	// but inert — until an operator sets it, so every existing deployment
	// keeps today's behaviour untouched.
	helixSkillsModulePath := g.getEnvOrDefault("HELIXSKILLS_MODULE_PATH", "")
	mcps["helixskills"] = MCPServerConfigFull{
		Type: "local",
		Command: []string{
			"sh", "-c",
			"cd " + shellSingleQuote(helixSkillsModulePath) + " && exec go run ./cmd/server --mcp stdio",
		},
		Environment: g.expandEnvMap(map[string]string{ // #nosec G101 -- env-var references, not credentials
			"HELIX_DB_HOST":     "{env:HELIXSKILLS_DB_HOST}",
			"HELIX_DB_PORT":     "{env:HELIXSKILLS_DB_PORT}",
			"HELIX_DB_NAME":     "{env:HELIXSKILLS_DB_NAME}",
			"HELIX_DB_USER":     "{env:HELIXSKILLS_DB_USER}",
			"HELIX_DB_PASSWORD": "{env:HELIXSKILLS_DB_PASSWORD}",
		}),
		Enabled: g.hasEnvVar("HELIXSKILLS_MODULE_PATH"),
	}

	return mcps
}

// GetEnabledMCPs returns only the MCPs that are enabled
func (g *FullMCPConfigGenerator) GetEnabledMCPs() map[string]MCPServerConfigFull {
	all := g.GenerateAllMCPs()
	enabled := make(map[string]MCPServerConfigFull)

	for name, cfg := range all {
		if cfg.Enabled {
			enabled[name] = cfg
		}
	}

	return enabled
}

// getEnvOrDefault returns the environment variable value or default
func (g *FullMCPConfigGenerator) getEnvOrDefault(name, defaultVal string) string {
	if val, ok := g.envVars[name]; ok && val != "" {
		return val
	}
	return defaultVal
}

// shellSingleQuote wraps s in POSIX single quotes, escaping any embedded
// single quote so the result is safe to splice into a `sh -c "..."`
// command string (used by the helixskills entry's `cd <dir> && ...`
// wrapper — see its doc comment for why a shell wrapper is required here).
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
