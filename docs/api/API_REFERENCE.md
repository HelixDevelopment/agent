# HelixAgent API Reference

Complete API documentation for HelixAgent and LLMsVerifier.

## Table of Contents

1. [HelixAgent REST API](#helixagent-rest-api)
2. [AI Debate Ensemble API](#ai-debate-ensemble-api)
3. [Protocol APIs](#protocol-apis)
4. [Background Tasks API](#background-tasks-api)
5. [CLI Agent Configuration API](#cli-agent-configuration-api)
6. [Authentication API](#authentication-api)
7. [Provider Management API](#provider-management-api)
8. [Sessions API](#sessions-api)
9. [Features API](#features-api)
10. [Model Metadata API](#model-metadata-api)
11. [RAG API](#rag-retrieval-augmented-generation-api)
12. [Embeddings API](#embeddings-api-extended)
13. [MCP API](#mcp-api-extended)
14. [LSP API](#lsp-api-extended)
15. [Protocol Management API](#protocol-management-api)
16. [Monitoring Endpoints](#monitoring-endpoints)
17. [Debates Team API](#debates-team-api)
18. [Ensemble API](#ensemble-api)
19. [Completion API](#completion-api)
20. [Agentic Workflows API](#agentic-workflows-api)
21. [Planning API](#planning-api)
22. [LLMOps API](#llmops-api)
23. [Benchmark API](#benchmark-api)
24. [Discovery API](#discovery-api)
25. [Scoring API](#scoring-api)
26. [Verification API](#verification-api)
27. [Health Monitoring API](#health-monitoring-api)
28. [Cognee API](#cognee-api)
29. [Vision API](#vision-api)
30. [Search API](#search-api)
31. [Templates API](#templates-api)
32. [Checkpoints API](#checkpoints-api)
33. [Browser Automation API](#browser-automation-api)
34. [Skills API](#skills-api)
35. [GraphQL API](#graphql-api)
36. [QA API](#qa-api)
37. [Startup and Infrastructure](#startup-and-infrastructure)
38. [LLMsVerifier Capability Detection API](#llmsverifier-capability-detection-api)

---

## HelixAgent REST API

Base URL: `http://localhost:7061`

### Authentication

Most endpoints require authentication via Bearer token:

```bash
Authorization: Bearer YOUR_API_KEY
```

### OpenAI-Compatible Endpoints

#### POST /v1/chat/completions

Create a chat completion using the AI Debate Ensemble.

**Request:**
```json
{
  "model": "helixagent-debate",
  "messages": [
    {"role": "system", "content": "You are a helpful assistant."},
    {"role": "user", "content": "Explain quantum computing."}
  ],
  "stream": true,
  "temperature": 0.7,
  "max_tokens": 4096,
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "Glob",
        "description": "Find files matching a pattern",
        "parameters": {
          "type": "object",
          "properties": {"pattern": {"type": "string"}},
          "required": ["pattern"]
        }
      }
    }
  ]
}
```

**Response (Streaming):**
```
data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"delta":{"content":"Quantum"},"index":0}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"delta":{"content":" computing"},"index":0}]}

data: [DONE]
```

**Response (Non-Streaming):**
```json
{
  "id": "chatcmpl-xxx",
  "object": "chat.completion",
  "created": 1705555200,
  "model": "helixagent-debate",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Quantum computing is..."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 25,
    "completion_tokens": 150,
    "total_tokens": 175
  }
}
```

##### HelixAgent request extensions

These fields are HelixAgent additions to the OpenAI chat-completions request
body. They are optional; omitting them preserves the documented default
behaviour.

| Field | Type | Default | Effect |
|---|---|---|---|
| `passthrough` | boolean | `false` | Opts the request OUT of the multi-round AI Debate and routes it verbatim to the provider chain. |
| `force_provider` | string | `""` | Sends the request to the named provider, on both the streaming and non-streaming provider chain. Unknown or failing names fall back to the normal chain — never a hard failure. |
| `ensemble_config` | object | `null` | Debate/ensemble tuning. See [Ensemble API](#ensemble-api). |

###### `passthrough`

```json
{
  "model": "helixagent-debate",
  "messages": [{"role": "user", "content": "Reply with exactly: OK"}],
  "passthrough": true
}
```

**What it does.** With `passthrough: true`, your messages are forwarded to the
provider chain **unmodified** and you get the provider's literal answer, instead
of a debated synthesis from the ensemble. Use it when you want the model to obey
a precise instruction rather than deliberate about it.

**Default.** `false`. The three debate/ensemble model aliases
(`helixagent-debate`, `helixagent-ensemble`, `helix-debate`, and their
`helixagent/`-qualified forms) keep debating unless you explicitly opt out.
`"passthrough": false` and a missing/`null` field are identical to the default.

**Type.** Strictly boolean. A string `"true"` or a number `1` is rejected with
HTTP 400 — the flag fails closed rather than guessing.

**Interaction with `stream`.** Honoured identically for `stream: true` and
`stream: false`. A streaming pass-through emits SSE chunks straight from the
provider.

**Interaction with `model`.** Only meaningful for the debate/ensemble aliases.
`helixagent-llm` already routes to the provider chain, so the flag is a no-op
there.

**What the response looks like — the `model` field reports WHO ANSWERED.** A
pass-through response reports **the provider and model that actually served the
request**, not the alias you asked for, in the form `<provider>/<model>`:

```json
{
  "model": "helixllm/qwen2.5-coder-7b",
  "system_fingerprint": "fp_helixllm_v1",
  "choices": [ ... ]
}
```

The provider half is the provider the chain actually invoked (`helixllm`, or the
cloud provider that answered, or the one you named in `force_provider`). The
model half is the model id **that provider itself reported** for the response —
not a name looked up in our configuration.

This holds on **both** halves: the non-streaming response body and every SSE
chunk of a streaming response carry the same value, including the
tool-call-bearing streaming path.

**Why it is not the model you requested.** The debate/ensemble aliases
(`helixagent-debate`, `helixagent-ensemble`, `helix-debate`) name a *route*, not
a model — no model is called `helixagent-debate`. Echoing the alias back told
you nothing about which of several possible providers answered. Reporting the
resolved identity instead is also what the OpenAI API itself does: request
`gpt-4o` and the response comes back as `gpt-4o-2024-08-06`.

> **Accepted cost — read this if you compare model strings.** A client that
> asserts `response.model == request.model` **will now see a mismatch** on the
> pass-through route, and on the other routes that reach the provider chain
> (`helixagent-llm`, and the multi-turn / tools bypasses). This is intended, not
> a defect: the mismatch is the information. If your client pins or validates
> model names, compare against the model you requested — which you already have
> — and treat the response `model` as a *report of what served you*, useful for
> logging, cost attribution, and debugging which route answered.

**When a provider reports no model id.** Some providers return a completion
without naming a model. In that case the response reports the provider it
genuinely came from and marks the unknown half explicitly:

```json
{ "model": "deepseek/<unreported>" }
```

The angle brackets are not valid in any provider's model identifier, so this can
never be mistaken for a real model name. HelixAgent deliberately does **not**
invent an id, and deliberately does **not** fall back to the alias you
requested — a silent fallback would put a plausible-looking wrong answer in the
field precisely in the cases nobody inspects.

**When the flag does NOT apply.** Two request shapes are handled before it is
consulted, in both streaming and non-streaming modes:

- **Tool-result turns** (the last message is a `tool` result) are synthesised by
  the tool-result handler. This breaks the
  debate → `tool_calls` → results → debate loop, and takes precedence over
  `passthrough`.
- Requests that **already** bypass the debate by other means — multi-turn
  conversations (more than one `user` message) and requests carrying a `tools`
  array — are unaffected; the flag makes that pre-existing behaviour explicit
  and requestable rather than incidental.

**Behavioural differences from the debate route.** These are deliberate; they
follow from "verbatim to the provider":

- **Skills are not injected.** Skill matching rewrites the message set, which is
  precisely what `passthrough` opts out of.
- **`force_provider` is honoured** and takes precedence over the chain's normal
  ordering.
- **Errors.** When every provider in the chain fails you get
  `503 no_provider_available`, the provider chain's long-standing error contract
  (shared with the `helixagent-llm` route), rather than the ensemble's
  categorised error.
- **Timeouts.** The provider chain applies no per-provider deadline, so a hung
  provider is bounded only by your own client timeout. This is a pre-existing
  property of the chain shared by every route that uses it, not something
  `passthrough` introduces; set a client-side timeout accordingly.

#### GET /v1/models

List available models.

**Response:**
```json
{
  "object": "list",
  "data": [
    {
      "id": "helixagent-debate",
      "object": "model",
      "created": 1705555200,
      "owned_by": "helixagent",
      "capabilities": {
        "vision": true,
        "streaming": true,
        "function_calling": true,
        "embeddings": true,
        "mcp": true,
        "acp": true,
        "lsp": true
      }
    }
  ]
}
```

#### POST /v1/embeddings

Generate embeddings for text.

**Request:**
```json
{
  "model": "helixagent-debate",
  "input": "The quick brown fox jumps over the lazy dog."
}
```

**Response:**
```json
{
  "object": "list",
  "data": [
    {
      "object": "embedding",
      "embedding": [0.0023, -0.0012, ...],
      "index": 0
    }
  ],
  "model": "helixagent-debate",
  "usage": {
    "prompt_tokens": 10,
    "total_tokens": 10
  }
}
```

---

## AI Debate Ensemble API

### POST /v1/debates

Create a new AI debate.

**Request:**
```json
{
  "topic": "Should AI systems be open source?",
  "participants": [
    {"role": "analyst", "provider": "anthropic", "model": "claude-3-opus"},
    {"role": "proposer", "provider": "openai", "model": "gpt-4"},
    {"role": "critic", "provider": "deepseek", "model": "deepseek-chat"},
    {"role": "synthesizer", "provider": "gemini", "model": "gemini-pro"},
    {"role": "mediator", "provider": "qwen", "model": "qwen-max"}
  ],
  "rounds": 3,
  "dialogue_style": "theater"
}
```

**Response:**
```json
{
  "id": "debate-abc123",
  "status": "created",
  "topic": "Should AI systems be open source?",
  "participants": [...],
  "created_at": "2025-01-14T10:30:00Z"
}
```

### GET /v1/debates/:id

Get debate details and status.

**Response:**
```json
{
  "id": "debate-abc123",
  "status": "completed",
  "topic": "Should AI systems be open source?",
  "rounds": [
    {
      "number": 1,
      "responses": [
        {"role": "analyst", "content": "Let me analyze..."},
        {"role": "proposer", "content": "I propose..."},
        {"role": "critic", "content": "I challenge..."},
        {"role": "synthesizer", "content": "Combining perspectives..."},
        {"role": "mediator", "content": "After weighing..."}
      ]
    }
  ],
  "consensus": "The debate concluded with...",
  "completed_at": "2025-01-14T10:35:00Z"
}
```

### GET /v1/debates/:id/status

Get debate execution status (for async debates).

**Response:**
```json
{
  "id": "debate-abc123",
  "status": "running",
  "current_round": 2,
  "total_rounds": 3,
  "progress": 66.7
}
```

### GET /v1/debates

List all debates.

**Response:**
```json
{
  "debates": [
    {"id": "debate-abc123", "topic": "...", "status": "completed"},
    {"id": "debate-def456", "topic": "...", "status": "running"}
  ],
  "total": 2
}
```

### GET /v1/debates/:id/results

Get final debate results.

**Response:**
```json
{
  "id": "debate-abc123",
  "consensus": "The debate concluded that...",
  "confidence": 0.92,
  "voting_method": "condorcet",
  "participants_agreed": 4,
  "participants_total": 5
}
```

### POST /v1/debates/:id/approve

Approve a debate at an approval gate (when approval gates are enabled).

**Response:**
```json
{
  "id": "debate-abc123",
  "gate": "phase_3",
  "approved": true,
  "approved_at": "2026-04-06T10:35:00Z"
}
```

### POST /v1/debates/:id/reject

Reject a debate at an approval gate.

### GET /v1/debates/:id/gates

Get approval gate status for a debate.

**Response:**
```json
{
  "debate_id": "debate-abc123",
  "gates": [
    {"phase": "proposal", "status": "approved", "approved_at": "2026-04-06T10:31:00Z"},
    {"phase": "critique", "status": "pending"}
  ]
}
```

### GET /v1/debates/:id/audit

Get full audit trail for a debate (provenance tracking).

**Response:**
```json
{
  "debate_id": "debate-abc123",
  "events": [
    {"type": "session_started", "timestamp": "2026-04-06T10:30:00Z"},
    {"type": "phase_completed", "phase": "dehallucination", "timestamp": "2026-04-06T10:31:00Z"},
    {"type": "phase_completed", "phase": "proposal", "timestamp": "2026-04-06T10:32:00Z"},
    {"type": "vote_cast", "participant": "claude", "timestamp": "2026-04-06T10:34:00Z"},
    {"type": "consensus_reached", "timestamp": "2026-04-06T10:35:00Z"}
  ],
  "total_events": 14
}
```

### DELETE /v1/debates/:id

Delete a debate.

**Response:**
```json
{
  "id": "debate-abc123",
  "deleted": true
}
```

---

## Protocol APIs

All protocol endpoints support two access modes:
1. **SSE mode** (GET) -- establishes a Server-Sent Events connection for real-time streaming
2. **Message mode** (POST) -- sends a single JSON-RPC or protocol-specific message

These SSE/message pairs are registered at the `/v1` level for each protocol.

### MCP (Model Context Protocol)

#### GET /v1/mcp

SSE endpoint for MCP connection (StreamableHTTP).

**Response (SSE):**
```
event: endpoint
data: {"uri": "http://localhost:7061/v1/mcp"}

event: heartbeat
data: {"timestamp": "2026-04-06T10:30:00Z"}
```

#### POST /v1/mcp

Send MCP message (StreamableHTTP POST mode).

**Request:**
```json
{
  "jsonrpc": "2.0",
  "method": "tools/list",
  "id": 1
}
```

**Response:**
```json
{
  "jsonrpc": "2.0",
  "result": {
    "tools": [
      {"name": "Bash", "description": "Execute shell commands"},
      {"name": "Read", "description": "Read file contents"},
      {"name": "Write", "description": "Write file contents"}
    ]
  },
  "id": 1
}
```

### ACP (Agent Communication Protocol)

#### GET /v1/acp

SSE endpoint for ACP connection.

#### POST /v1/acp

Send ACP message.

**Request:**
```json
{
  "type": "request",
  "agent_id": "agent-123",
  "action": "execute_task",
  "payload": {
    "task": "analyze_code",
    "target": "/path/to/file.go"
  }
}
```

### LSP (Language Server Protocol)

#### GET /v1/lsp

SSE endpoint for LSP connection.

#### POST /v1/lsp

Send LSP request.

**Request:**
```json
{
  "jsonrpc": "2.0",
  "method": "textDocument/definition",
  "params": {
    "textDocument": {"uri": "file:///path/to/file.go"},
    "position": {"line": 10, "character": 5}
  },
  "id": 1
}
```

### Embeddings

#### GET /v1/embeddings

SSE endpoint for embeddings connection.

#### POST /v1/embeddings

Send embeddings request via protocol message.

### Vision

#### GET /v1/vision

SSE endpoint for vision connection.

#### POST /v1/vision

Analyze images via protocol message.

**Request:**
```json
{
  "model": "helixagent-debate",
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "Describe this image"},
        {"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}
      ]
    }
  ]
}
```

### Cognee

#### GET /v1/cognee

SSE endpoint for Cognee knowledge graph connection.

#### POST /v1/cognee

Send Cognee request via protocol message.

### RAG

#### GET /v1/rag

SSE endpoint for RAG connection.

#### POST /v1/rag

Send RAG request via protocol message.

### Cognee

#### POST /v1/cognee/memory

Add content to knowledge graph memory.

**Request:**
```json
{
  "content": "Important information about our project architecture...",
  "metadata": {
    "source": "architecture.md",
    "tags": ["architecture", "design"]
  }
}
```

#### POST /v1/cognee/search

Search knowledge graph.

**Request:**
```json
{
  "query": "What is the project architecture?",
  "limit": 10
}
```

See [Cognee API](#cognee-api) below for full endpoint reference.

---

## Background Tasks API

### POST /v1/tasks

Create a background task.

**Request:**
```json
{
  "type": "command",
  "command": "npm run build",
  "working_dir": "/path/to/project",
  "priority": "high",
  "endless": false
}
```

**Response:**
```json
{
  "id": "task-xyz789",
  "status": "pending",
  "created_at": "2025-01-14T10:30:00Z"
}
```

### GET /v1/tasks/:id/status

Get task status.

**Response:**
```json
{
  "id": "task-xyz789",
  "status": "running",
  "progress": 45.5,
  "started_at": "2025-01-14T10:30:05Z",
  "resources": {
    "cpu_percent": 25.3,
    "memory_mb": 512,
    "io_read_bytes": 1048576,
    "io_write_bytes": 524288
  }
}
```

### GET /v1/tasks/:id/events

SSE stream for task events.

**Response (SSE):**
```
event: progress
data: {"progress": 50.0, "message": "Compiling..."}

event: output
data: {"stream": "stdout", "content": "Building module 5/10..."}

event: complete
data: {"status": "completed", "exit_code": 0}
```

### POST /v1/tasks/:id/cancel

Cancel a running task.

**Response:**
```json
{
  "id": "task-xyz789",
  "status": "cancelled"
}
```

### GET /v1/tasks

List all tasks.

### GET /v1/tasks/queue/stats

Get task queue statistics.

### GET /v1/tasks/events

Poll for task events (SSE long-poll).

### GET /v1/tasks/:id

Get task details.

### GET /v1/tasks/:id/logs

Get task logs.

### GET /v1/tasks/:id/resources

Get task resource usage (CPU, memory, I/O).

### POST /v1/tasks/:id/pause

Pause a running task.

### POST /v1/tasks/:id/resume

Resume a paused task.

### DELETE /v1/tasks/:id

Delete a task.

### POST /v1/webhooks

Register a webhook for task events.

**Request:**
```json
{
  "url": "https://example.com/webhook",
  "events": ["task.completed", "task.failed"],
  "secret": "webhook-secret-123"
}
```

### GET /v1/webhooks

List registered webhooks.

### DELETE /v1/webhooks/:id

Delete a webhook.

### GET /v1/tasks/:id/analyze

Analyze task for stuck detection.

**Response:**
```json
{
  "id": "task-xyz789",
  "stuck_analysis": {
    "is_stuck": false,
    "checks": {
      "heartbeat": {"passed": true, "last_heartbeat": "2025-01-14T10:35:00Z"},
      "cpu_freeze": {"passed": true, "cpu_usage": 25.3},
      "memory_leak": {"passed": true, "memory_growth_rate": 0.01},
      "io_starvation": {"passed": true, "io_activity": true}
    }
  }
}
```

---

## CLI Agent Configuration API

### GET /v1/agents

List all supported CLI agents.

**Response:**
```json
{
  "agents": [
    {
      "name": "opencode",
      "language": "Go",
      "config_format": "json",
      "streaming": true,
      "mcp_support": true,
      "provider_count": 15
    },
    {
      "name": "claudecode",
      "language": "TypeScript",
      "config_format": "json",
      "streaming": true,
      "mcp_support": true,
      "provider_count": 1
    }
  ],
  "total": 18
}
```

### GET /v1/agents/:name

Get specific agent details.

**Response:**
```json
{
  "name": "kilocode",
  "language": "TypeScript",
  "config_format": "json",
  "config_path": "~/.kilocode/settings.json",
  "streaming": {
    "supported": true,
    "types": ["async_generator"],
    "chunk_types": ["text", "reasoning", "tool_call"]
  },
  "network": {
    "http_versions": ["http/1.1", "http/2"],
    "http3_supported": false,
    "proxy_supported": true
  },
  "compression": {
    "supported": false
  },
  "caching": {
    "supported": true,
    "types": ["prompt_caching"]
  },
  "protocols": ["openai", "anthropic", "mcp"],
  "provider_count": 43,
  "tool_count": 28,
  "extended": {
    "plan_act_modes": true,
    "checkpointing": true,
    "auto_approval": true
  }
}
```

### GET /v1/agents/protocol/:protocol

Get agents supporting a specific protocol.

**Response:**
```json
{
  "protocol": "mcp",
  "agents": ["opencode", "claudecode", "amazonq", "helixcode"]
}
```

### GET /v1/agents/tool/:tool

Get agents supporting a specific tool.

**Response:**
```json
{
  "tool": "Git",
  "agents": ["opencode", "claudecode", "kilocode", "aider", "plandex"]
}
```

---

## Authentication API

### POST /v1/auth/register

Register a new user account.

**Request:**
```json
{
  "username": "user@example.com",
  "password": "securePassword123",
  "name": "John Doe"
}
```

**Response:**
```json
{
  "id": "user-abc123",
  "username": "user@example.com",
  "name": "John Doe",
  "created_at": "2025-01-14T10:30:00Z"
}
```

### POST /v1/auth/login

Authenticate and receive tokens.

**Request:**
```json
{
  "username": "user@example.com",
  "password": "securePassword123"
}
```

**Response:**
```json
{
  "access_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
  "refresh_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
  "token_type": "Bearer",
  "expires_in": 3600
}
```

### POST /v1/auth/refresh

Refresh access token using refresh token.

**Request:**
```json
{
  "refresh_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
}
```

**Response:**
```json
{
  "access_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
  "token_type": "Bearer",
  "expires_in": 3600
}
```

### POST /v1/auth/logout

Invalidate current session.

**Response:**
```json
{
  "message": "Successfully logged out"
}
```

### GET /v1/auth/me

Get current user information.

**Response:**
```json
{
  "id": "user-abc123",
  "username": "user@example.com",
  "name": "John Doe",
  "roles": ["user"],
  "created_at": "2025-01-14T10:30:00Z"
}
```

---

## Provider Management API

### GET /v1/providers

List all configured LLM providers.

**Response:**
```json
{
  "providers": [
    {
      "id": "claude",
      "name": "Claude (Anthropic)",
      "type": "oauth",
      "status": "verified",
      "score": 9.2,
      "models": ["claude-3-opus", "claude-3-sonnet", "claude-3-haiku"]
    },
    {
      "id": "deepseek",
      "name": "DeepSeek",
      "type": "api_key",
      "status": "verified",
      "score": 8.8,
      "models": ["deepseek-chat", "deepseek-coder"]
    }
  ],
  "total": 10
}
```

### GET /v1/providers/verification

Get verification status of all providers.

**Response:**
```json
{
  "verified": 8,
  "failed": 1,
  "pending": 1,
  "providers": [
    {
      "id": "claude",
      "verified": true,
      "score": 9.2,
      "last_verified": "2025-01-14T10:00:00Z"
    }
  ]
}
```

### POST /v1/providers/verify

Trigger verification of all providers.

**Response:**
```json
{
  "status": "verification_started",
  "providers_queued": 10
}
```

### GET /v1/providers/discovery

Get provider discovery summary.

**Response:**
```json
{
  "api_key_providers": ["deepseek", "gemini", "mistral"],
  "oauth_providers": ["claude", "qwen"],
  "free_providers": ["zen"],
  "discovered_at": "2025-01-14T10:00:00Z"
}
```

### POST /v1/providers/discover

Discover and verify available providers.

**Response:**
```json
{
  "discovered": 10,
  "verified": 8,
  "failed": 2,
  "providers": [...]
}
```

### POST /v1/providers/rediscover

Re-discover providers (clears cache and rediscovers from scratch).

**Response:**
```json
{
  "rediscovered": 10,
  "new_providers": 1,
  "removed_providers": 0,
  "providers": [...]
}
```

### GET /v1/providers/best

Get best providers ranked by verification score.

**Query Parameters:**
- `limit` (optional): Number of providers to return (default: 5)
- `capability` (optional): Filter by capability (e.g., "vision", "streaming")

**Response:**
```json
{
  "providers": [
    {"id": "claude", "score": 9.2, "rank": 1},
    {"id": "deepseek", "score": 8.8, "rank": 2},
    {"id": "gemini", "score": 8.5, "rank": 3}
  ]
}
```

### POST /v1/providers

Add a new provider configuration.

**Request:**
```json
{
  "id": "custom-provider",
  "name": "Custom Provider",
  "type": "api_key",
  "api_key": "sk-xxx",
  "base_url": "https://api.custom-provider.com/v1",
  "models": ["custom-model-1", "custom-model-2"]
}
```

### GET /v1/providers/:id

Get specific provider details.

### PUT /v1/providers/:id

Update provider configuration.

### DELETE /v1/providers/:id

Remove a provider.

### GET /v1/providers/:id/verification

Get verification status for a specific provider.

### POST /v1/providers/:id/verify

Trigger verification for a specific provider.

### GET /v1/providers/:id/health

Check provider health status.

**Response:**
```json
{
  "provider": "claude",
  "healthy": true,
  "circuit_breaker": {
    "state": "closed",
    "failure_count": 0,
    "last_failure": null
  }
}
```

---

## Sessions API

### POST /v1/sessions

Create a new conversation session.

**Request:**
```json
{
  "model": "helixagent-debate",
  "system_prompt": "You are a helpful assistant.",
  "metadata": {
    "project": "code-review"
  }
}
```

**Response:**
```json
{
  "id": "session-xyz789",
  "model": "helixagent-debate",
  "created_at": "2025-01-14T10:30:00Z",
  "expires_at": "2025-01-14T11:30:00Z"
}
```

### GET /v1/sessions/:id

Get session details.

### DELETE /v1/sessions/:id

Terminate a session.

### GET /v1/sessions

List all active sessions.

---

## Features API

### GET /v1/features

Get all enabled features.

**Response:**
```json
{
  "features": {
    "ai_debate": true,
    "multi_pass_validation": true,
    "mcp_integration": true,
    "lsp_integration": true,
    "acp_integration": true,
    "rag_enabled": true,
    "embeddings_enabled": true
  }
}
```

### GET /v1/features/available

Get all available features with their status.

**Response:**
```json
{
  "features": [
    {
      "name": "ai_debate",
      "enabled": true,
      "description": "Multi-LLM debate system"
    },
    {
      "name": "rag",
      "enabled": true,
      "description": "Retrieval-Augmented Generation"
    }
  ]
}
```

### GET /v1/features/agents

Get features available for CLI agents.

---

## Model Metadata API

### GET /v1/models/metadata

List all models with metadata.

**Response:**
```json
{
  "models": [
    {
      "id": "claude-3-opus",
      "provider": "anthropic",
      "context_window": 200000,
      "capabilities": ["vision", "function_calling", "streaming"],
      "pricing": {
        "input": 0.015,
        "output": 0.075
      }
    }
  ]
}
```

### GET /v1/models/metadata/:id

Get specific model metadata.

### GET /v1/models/metadata/:id/benchmarks

Get model benchmark results.

**Response:**
```json
{
  "model": "claude-3-opus",
  "benchmarks": {
    "mmlu": 0.867,
    "humaneval": 0.842,
    "gsm8k": 0.956,
    "hellaswag": 0.952
  }
}
```

### GET /v1/models/metadata/compare

Compare multiple models.

**Query Parameters:**
- `models`: Comma-separated list of model IDs

**Response:**
```json
{
  "comparison": [
    {"model": "claude-3-opus", "score": 9.2, "context": 200000},
    {"model": "gpt-4", "score": 8.9, "context": 128000}
  ]
}
```

### GET /v1/models/metadata/capability/:capability

Get models with a specific capability.

---

## RAG (Retrieval-Augmented Generation) API

### GET /v1/rag/health

Check RAG system health.

**Response:**
```json
{
  "healthy": true,
  "vector_db": "qdrant",
  "vector_db_status": "connected",
  "document_count": 15420,
  "index_status": "ready"
}
```

### GET /v1/rag/stats

Get RAG system statistics.

**Response:**
```json
{
  "documents_indexed": 15420,
  "total_chunks": 89543,
  "avg_chunk_size": 512,
  "index_size_mb": 256,
  "queries_last_hour": 1234
}
```

### POST /v1/rag/documents

Ingest a document into the RAG system.

**Request:**
```json
{
  "content": "Document content to index...",
  "metadata": {
    "source": "architecture.md",
    "type": "documentation",
    "tags": ["architecture", "design"]
  },
  "chunk_strategy": "semantic"
}
```

**Response:**
```json
{
  "document_id": "doc-abc123",
  "chunks_created": 15,
  "indexed_at": "2025-01-14T10:30:00Z"
}
```

### POST /v1/rag/documents/batch

Batch ingest multiple documents.

**Request:**
```json
{
  "documents": [
    {"content": "...", "metadata": {...}},
    {"content": "...", "metadata": {...}}
  ]
}
```

### DELETE /v1/rag/documents/:id

Delete a document from the index.

### POST /v1/rag/search

Search documents using vector similarity.

**Request:**
```json
{
  "query": "How does the authentication system work?",
  "limit": 10,
  "threshold": 0.7,
  "filters": {
    "type": "documentation"
  }
}
```

**Response:**
```json
{
  "results": [
    {
      "document_id": "doc-abc123",
      "chunk_id": "chunk-1",
      "content": "The authentication system uses JWT tokens...",
      "score": 0.92,
      "metadata": {"source": "auth.md"}
    }
  ],
  "total": 5
}
```

### POST /v1/rag/search/hybrid

Hybrid search using both dense and sparse retrieval.

**Request:**
```json
{
  "query": "authentication JWT tokens",
  "dense_weight": 0.7,
  "sparse_weight": 0.3,
  "limit": 10
}
```

### POST /v1/rag/search/expanded

Search with query expansion (HyDE).

**Request:**
```json
{
  "query": "How to debug memory leaks?",
  "expansion_model": "claude-3-haiku",
  "limit": 10
}
```

### POST /v1/rag/rerank

Rerank search results using cross-encoder.

**Request:**
```json
{
  "query": "authentication system",
  "documents": [
    {"id": "doc-1", "content": "..."},
    {"id": "doc-2", "content": "..."}
  ]
}
```

### POST /v1/rag/compress

Compress context for LLM consumption.

**Request:**
```json
{
  "query": "Explain the architecture",
  "documents": ["doc-1", "doc-2", "doc-3"],
  "max_tokens": 4000
}
```

### POST /v1/rag/expand

Expand a query for better retrieval.

### POST /v1/rag/chunk

Chunk a document manually.

**Request:**
```json
{
  "content": "Long document text...",
  "strategy": "semantic",
  "chunk_size": 512,
  "overlap": 50
}
```

---

## Embeddings API (Extended)

### POST /v1/embeddings/generate

Generate embeddings for text.

**Request:**
```json
{
  "input": ["Text to embed", "Another text"],
  "model": "bge-m3"
}
```

**Response:**
```json
{
  "embeddings": [
    {"index": 0, "embedding": [0.023, -0.012, ...]},
    {"index": 1, "embedding": [0.015, -0.008, ...]}
  ],
  "model": "bge-m3",
  "dimensions": 1024
}
```

### POST /v1/embeddings/search

Vector similarity search.

**Request:**
```json
{
  "query_embedding": [0.023, -0.012, ...],
  "collection": "documents",
  "limit": 10,
  "threshold": 0.7
}
```

### POST /v1/embeddings/index

Index content with embeddings.

### POST /v1/embeddings/batch-index

Batch index multiple items.

### GET /v1/embeddings/stats

Get embedding system statistics.

### GET /v1/embeddings/providers

List available embedding providers.

**Response:**
```json
{
  "providers": [
    {"id": "openai", "models": ["text-embedding-3-small", "text-embedding-3-large"]},
    {"id": "bge", "models": ["bge-m3", "bge-large-en"]},
    {"id": "nomic", "models": ["nomic-embed-text-v1.5"]}
  ]
}
```

---

## MCP API (Extended)

### GET /v1/mcp/capabilities

Get MCP server capabilities.

**Response:**
```json
{
  "capabilities": {
    "tools": true,
    "prompts": true,
    "resources": true,
    "sampling": false
  },
  "server_info": {
    "name": "HelixAgent MCP Server",
    "version": "1.0.0"
  }
}
```

### GET /v1/mcp/tools

List all available MCP tools.

**Response:**
```json
{
  "tools": [
    {
      "name": "Bash",
      "description": "Execute shell commands",
      "parameters": {...}
    },
    {
      "name": "Read",
      "description": "Read file contents",
      "parameters": {...}
    }
  ],
  "total": 21
}
```

### POST /v1/mcp/tools/call

Execute an MCP tool.

**Request:**
```json
{
  "name": "Read",
  "arguments": {
    "file_path": "/path/to/file.go"
  }
}
```

### GET /v1/mcp/prompts

List available prompts.

### GET /v1/mcp/resources

List available resources.

### GET /v1/mcp/tools/search

Search for tools by keyword.

**Query Parameters:**
- `q`: Search query
- `category`: Filter by category

### GET /v1/mcp/tools/suggestions

Get tool suggestions based on context.

### GET /v1/mcp/adapters/search

Search for MCP adapters.

### GET /v1/mcp/categories

Get all tool categories.

### GET /v1/mcp/stats

Get MCP usage statistics.

---

## LSP API (Extended)

### GET /v1/lsp/servers

List available LSP servers.

**Response:**
```json
{
  "servers": [
    {"language": "go", "name": "gopls", "status": "running"},
    {"language": "typescript", "name": "typescript-language-server", "status": "running"},
    {"language": "python", "name": "pylsp", "status": "stopped"}
  ]
}
```

### POST /v1/lsp/execute

Execute an LSP request.

**Request:**
```json
{
  "language": "go",
  "method": "textDocument/definition",
  "params": {
    "textDocument": {"uri": "file:///path/to/file.go"},
    "position": {"line": 10, "character": 5}
  }
}
```

### POST /v1/lsp/sync

Synchronize LSP servers with workspace.

### GET /v1/lsp/stats

Get LSP usage statistics.

---

## Protocol Management API

### POST /v1/protocols/execute

Execute a unified protocol request.

**Request:**
```json
{
  "protocol": "mcp",
  "method": "tools/call",
  "params": {...}
}
```

### GET /v1/protocols/servers

List all protocol servers.

### GET /v1/protocols/metrics

Get protocol usage metrics.

### POST /v1/protocols/refresh

Refresh all protocol connections.

### POST /v1/protocols/configure

Configure protocol settings.

---

## Monitoring Endpoints

### GET /health

Basic health check.

**Response:**
```json
{
  "status": "healthy"
}
```

### GET /v1/health

Detailed health check.

**Response:**
```json
{
  "status": "healthy",
  "version": "1.0.0",
  "uptime": "24h30m15s",
  "components": {
    "database": "healthy",
    "redis": "healthy",
    "qdrant": "healthy"
  }
}
```

### GET /metrics

Prometheus metrics endpoint.

### GET /v1/monitoring/status

Get overall monitoring status combining all components.

**Response:**
```json
{
  "healthy": true,
  "circuit_breakers": {
    "healthy": true,
    "total": 10,
    "open": 0,
    "providers": {...}
  },
  "oauth_tokens": {
    "healthy": true,
    "tokens": {...}
  },
  "provider_health": {
    "healthy": true,
    "providers": {...}
  },
  "fallback_chain": {
    "validated": true,
    "valid": true
  }
}
```

### GET /v1/monitoring/circuit-breakers

Get status of all circuit breakers.

**Response:**
```json
{
  "healthy": true,
  "total": 10,
  "open": 0,
  "half_open": 1,
  "closed": 9,
  "providers": {
    "claude": {"state": "closed", "failures": 0, "successes": 150},
    "deepseek": {"state": "closed", "failures": 2, "successes": 98}
  }
}
```

### POST /v1/monitoring/circuit-breakers/:provider/reset

Reset a specific provider's circuit breaker.

**Response:**
```json
{
  "message": "Circuit breaker reset for provider: claude"
}
```

### POST /v1/monitoring/circuit-breakers/reset-all

Reset all circuit breakers.

**Response:**
```json
{
  "message": "All circuit breakers reset",
  "count": 10
}
```

### GET /v1/monitoring/oauth-tokens

Get OAuth token status for all providers.

**Response:**
```json
{
  "healthy": true,
  "tokens": {
    "claude": {
      "valid": true,
      "expires_at": "2025-01-15T10:30:00Z",
      "expires_in": "23h45m"
    },
    "qwen": {
      "valid": true,
      "expires_at": "2025-01-14T18:00:00Z",
      "expires_in": "7h30m"
    }
  }
}
```

### POST /v1/monitoring/oauth-tokens/:provider/refresh

Refresh OAuth token for a specific provider.

**Response:**
```json
{
  "message": "OAuth token refreshed for provider: claude",
  "expires_at": "2025-01-15T10:30:00Z"
}
```

### GET /v1/monitoring/provider-health

Get health status of all providers.

**Response:**
```json
{
  "healthy": true,
  "total_providers": 10,
  "healthy_providers": 9,
  "unhealthy_providers": 1,
  "providers": {
    "claude": {"healthy": true, "latency_ms": 245, "last_check": "2025-01-14T10:30:00Z"},
    "deepseek": {"healthy": true, "latency_ms": 180, "last_check": "2025-01-14T10:30:00Z"}
  }
}
```

### POST /v1/monitoring/provider-health/check

Force health check for all providers.

**Response:**
```json
{
  "message": "Health check initiated for all providers",
  "providers_checked": 10
}
```

### POST /v1/monitoring/provider-health/:provider/check

Force health check for a specific provider.

**Response:**
```json
{
  "provider": "claude",
  "healthy": true,
  "latency_ms": 245,
  "checked_at": "2025-01-14T10:35:00Z"
}
```

### GET /v1/monitoring/fallback-chain

Get fallback chain status.

**Response:**
```json
{
  "validated": true,
  "valid": true,
  "chain": [
    {"position": 1, "provider": "claude", "score": 9.2},
    {"position": 2, "provider": "deepseek", "score": 8.8},
    {"position": 3, "provider": "gemini", "score": 8.5}
  ],
  "last_validated": "2025-01-14T10:00:00Z"
}
```

### POST /v1/monitoring/fallback-chain/validate

Validate the current fallback chain configuration.

**Response:**
```json
{
  "valid": true,
  "message": "Fallback chain validated successfully",
  "warnings": []
}
```

### GET /v1/monitoring/concurrency

Get concurrency monitoring status.

**Response:**
```json
{
  "active_requests": 15,
  "max_concurrent": 100,
  "utilization_pct": 15.0,
  "per_provider": {
    "claude": {"active": 5, "max": 20},
    "deepseek": {"active": 3, "max": 20}
  }
}
```

### GET /v1/monitoring/concurrency/alerts

Get concurrency alert statistics.

### GET /v1/monitoring/concurrency/alerts/dead-letter

Get dead-letter concurrency alerts.

### POST /v1/monitoring/concurrency/alerts/dead-letter/:key/retry

Retry a dead-letter alert.

### GET /v1/monitoring/concurrency/alerts/retry-queue

Get alerts in the retry queue.

### POST /v1/monitoring/concurrency/alerts/retry-queue/:key/cancel

Cancel a retry attempt.

---

## Debates Team API

### GET /v1/debates/team

Get current AI Debate team configuration.

**Response:**
```json
{
  "team": [
    {
      "position": "analyst",
      "primary": {"provider": "claude", "model": "claude-3-opus", "score": 9.2},
      "fallbacks": [
        {"provider": "deepseek", "model": "deepseek-chat", "score": 8.8}
      ]
    },
    {
      "position": "proposer",
      "primary": {"provider": "gemini", "model": "gemini-pro", "score": 8.5},
      "fallbacks": [...]
    }
  ],
  "total_llms": 15,
  "verified_at": "2025-01-14T10:00:00Z"
}
```

---

## Ensemble API

### POST /v1/ensemble/completions

Run an ensemble completion (forces multi-provider ensemble mode).

**Request:**
```json
{
  "model": "helixagent-debate",
  "messages": [
    {"role": "user", "content": "Compare REST vs GraphQL architectures."}
  ],
  "ensemble_config": {
    "strategy": "confidence_weighted",
    "min_providers": 2,
    "confidence_threshold": 0.8,
    "fallback_to_best": true,
    "timeout": 30
  }
}
```

**Response:**
```json
{
  "id": "ensemble-20250406120000",
  "object": "ensemble.completion",
  "created": 1743940800,
  "model": "claude",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "REST and GraphQL differ in several key ways..."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 12,
    "completion_tokens": 150,
    "total_tokens": 162
  },
  "ensemble": {
    "voting_method": "confidence_weighted",
    "responses_count": 3,
    "scores": {"claude": 9.2, "deepseek": 8.8, "gemini": 8.5},
    "selected_provider": "claude",
    "selection_score": 9.2
  }
}
```

### POST /v1/ensemble/sessions

Create an ensemble session.

**Request:**
```json
{
  "strategy": "confidence_weighted",
  "participants": {
    "primary": {"provider": "claude", "model": "claude-3-opus"},
    "critiques": [{"provider": "deepseek", "model": "deepseek-chat"}],
    "verifiers": [{"provider": "gemini", "model": "gemini-pro"}]
  }
}
```

### GET /v1/ensemble/sessions

List ensemble sessions.

### GET /v1/ensemble/sessions/:id

Get session details.

### POST /v1/ensemble/sessions/:id/execute

Execute a session.

### POST /v1/ensemble/sessions/:id/cancel

Cancel a running session.

### POST /v1/ensemble/teams

Create a team of LLM providers for ensemble operations.

**Request:**
```json
{
  "name": "code-review-team",
  "agents": [
    {"provider": "claude", "model": "claude-3-opus", "role": "reviewer"},
    {"provider": "deepseek", "model": "deepseek-coder", "role": "analyzer"}
  ]
}
```

### GET /v1/ensemble/teams

List all teams.

### GET /v1/ensemble/teams/:id

Get team details.

### PUT /v1/ensemble/teams/:id

Update a team.

### DELETE /v1/ensemble/teams/:id

Delete a team.

### POST /v1/ensemble/teams/:id/agents

Add an agent to a team.

### DELETE /v1/ensemble/teams/:id/agents/:agentId

Remove an agent from a team.

### POST /v1/ensemble/teams/:id/execute

Execute a task with the team.

---

## Completion API

Skills-enhanced completion endpoints with intent-based routing.

### POST /v1/completion

Run a single completion.

**Request:**
```json
{
  "prompt": "Explain the observer pattern in Go.",
  "model": "helixagent-debate",
  "temperature": 0.7,
  "max_tokens": 2048
}
```

**Response:**
```json
{
  "id": "compl-abc123",
  "object": "text_completion",
  "created": 1743940800,
  "model": "helixagent-debate",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "The observer pattern..."},
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 200,
    "total_tokens": 210
  }
}
```

### POST /v1/completion/stream

Run a streaming completion.

### POST /v1/completion/chat

Run a chat-style completion with message history.

**Request:**
```json
{
  "messages": [
    {"role": "system", "content": "You are a Go expert."},
    {"role": "user", "content": "How do I use channels?"}
  ],
  "model": "helixagent-debate",
  "stream": false
}
```

### POST /v1/completion/chat/stream

Run a streaming chat completion.

### GET /v1/completion/models

List models available for completion.

---

## Agentic Workflows API

Graph-based agentic workflow orchestration.

### POST /v1/agentic/workflows

Create and execute a workflow.

**Request:**
```json
{
  "name": "code-review-pipeline",
  "description": "Automated code review workflow",
  "nodes": [
    {"id": "analyze", "name": "Code Analysis", "type": "llm"},
    {"id": "review", "name": "Review", "type": "llm"},
    {"id": "report", "name": "Generate Report", "type": "llm"}
  ],
  "edges": [
    {"from": "analyze", "to": "review"},
    {"from": "review", "to": "report"}
  ],
  "entry_point": "analyze",
  "end_nodes": ["report"],
  "config": {
    "max_iterations": 10,
    "timeout_seconds": 300,
    "enable_checkpoints": true,
    "enable_self_correction": true,
    "max_retries": 3
  },
  "input": {
    "query": "Review this Go function for best practices",
    "context": {"file_path": "/path/to/file.go"}
  }
}
```

**Response:**
```json
{
  "id": "wf-abc123",
  "name": "code-review-pipeline",
  "status": "completed",
  "nodes_count": 3,
  "edges_count": 2,
  "entry_point": "analyze",
  "end_nodes": ["report"],
  "history": [
    {"node_id": "analyze", "status": "completed", "duration_ms": 1200},
    {"node_id": "review", "status": "completed", "duration_ms": 950},
    {"node_id": "report", "status": "completed", "duration_ms": 800}
  ]
}
```

### GET /v1/agentic/workflows/:id

Get workflow status and results.

---

## Planning API

AI planning algorithms: Hierarchical Planning (HiPlan), Monte Carlo Tree Search (MCTS), and Tree of Thoughts (ToT).

### POST /v1/planning/hiplan

Execute hierarchical planning.

**Request:**
```json
{
  "goal": "Refactor the authentication system to support OAuth 2.0",
  "config": {
    "max_milestones": 5,
    "max_steps_per_milestone": 10,
    "enable_parallel_milestones": true,
    "max_parallel_milestones": 3,
    "enable_adaptive_planning": true,
    "timeout_seconds": 600
  }
}
```

**Response:**
```json
{
  "plan_id": "plan-abc123",
  "goal": "Refactor the authentication system to support OAuth 2.0",
  "state": "completed",
  "progress": 100.0,
  "milestones": [
    {
      "id": "m1",
      "name": "Design OAuth flow",
      "state": "completed",
      "priority": 1,
      "progress": 100.0,
      "steps_count": 4
    },
    {
      "id": "m2",
      "name": "Implement token management",
      "state": "completed",
      "priority": 2,
      "progress": 100.0,
      "steps_count": 6
    }
  ],
  "duration_ms": 5200,
  "created_at": "2026-04-06T10:30:00Z"
}
```

### POST /v1/planning/mcts

Run Monte Carlo Tree Search planning.

**Request:**
```json
{
  "goal": "Find optimal database migration strategy",
  "config": {
    "max_iterations": 1000,
    "exploration_weight": 1.414,
    "max_depth": 10,
    "timeout_seconds": 120
  }
}
```

**Response:**
```json
{
  "search_id": "mcts-abc123",
  "goal": "Find optimal database migration strategy",
  "state": "completed",
  "best_path": ["analyze_schema", "create_migration", "test_rollback", "apply"],
  "best_score": 0.92,
  "iterations_run": 500,
  "nodes_explored": 1250,
  "duration_ms": 3400,
  "created_at": "2026-04-06T10:30:00Z"
}
```

### POST /v1/planning/tot

Run Tree of Thoughts reasoning.

**Request:**
```json
{
  "goal": "Design a scalable caching strategy",
  "config": {
    "branching_factor": 3,
    "max_depth": 5,
    "evaluation_strategy": "vote",
    "timeout_seconds": 180
  }
}
```

**Response:**
```json
{
  "tree_id": "tot-abc123",
  "goal": "Design a scalable caching strategy",
  "state": "completed",
  "best_thought_path": [
    {"depth": 0, "thought": "Consider cache layers..."},
    {"depth": 1, "thought": "L1: in-memory, L2: Redis..."},
    {"depth": 2, "thought": "TTL policies per data type..."}
  ],
  "best_score": 0.88,
  "thoughts_generated": 45,
  "thoughts_evaluated": 45,
  "duration_ms": 4100,
  "created_at": "2026-04-06T10:30:00Z"
}
```

### POST /v1/planning/plan-mode/enter

Enter plan mode (from Claude Code integration).

### POST /v1/planning/plan-mode/:id/exit

Exit plan mode for a session.

### GET /v1/planning/plan-mode/:id/status

Get plan mode status.

### POST /v1/planning/plan-mode/:id/verify

Verify a plan before execution.

### POST /v1/planning/plan-mode/:id/execute

Start plan execution.

### PUT /v1/planning/plan-mode/:id/tasks/:taskId

Update a specific task within a plan.

---

## LLMOps API

LLM operations management: A/B experiments, continuous evaluation, and prompt versioning.

### POST /v1/llmops/experiments

Create an A/B experiment.

**Request:**
```json
{
  "name": "prompt-optimization-v2",
  "description": "Compare original vs optimized prompt",
  "variants": [
    {"name": "control", "model": "claude-3-opus", "prompt_template": "..."},
    {"name": "optimized", "model": "claude-3-opus", "prompt_template": "..."}
  ],
  "traffic_split": {"control": 0.5, "optimized": 0.5},
  "metrics": ["latency", "quality_score", "token_usage"],
  "target_metric": "quality_score"
}
```

**Response:**
```json
{
  "id": "exp-abc123",
  "name": "prompt-optimization-v2",
  "status": "running",
  "variants": [...],
  "traffic_split": {"control": 0.5, "optimized": 0.5},
  "created_at": "2026-04-06T10:30:00Z"
}
```

### GET /v1/llmops/experiments

List all experiments.

**Response:**
```json
{
  "experiments": [
    {"id": "exp-abc123", "name": "prompt-optimization-v2", "status": "running"},
    {"id": "exp-def456", "name": "model-comparison", "status": "completed", "winner": "variant_b"}
  ],
  "total": 2
}
```

### GET /v1/llmops/experiments/:id

Get experiment details with results.

### POST /v1/llmops/evaluate

Run a continuous evaluation.

**Request:**
```json
{
  "name": "weekly-quality-check",
  "dataset": "golden-test-set",
  "prompt_name": "code-review-v2",
  "prompt_version": "1.3.0",
  "model_name": "claude-3-opus",
  "metrics": ["accuracy", "relevance", "coherence"]
}
```

**Response:**
```json
{
  "id": "eval-abc123",
  "name": "weekly-quality-check",
  "status": "completed",
  "dataset": "golden-test-set",
  "results": {
    "accuracy": 0.94,
    "relevance": 0.91,
    "coherence": 0.96
  },
  "created_at": "2026-04-06T10:30:00Z"
}
```

### GET /v1/llmops/prompts

List prompt versions.

### POST /v1/llmops/prompts

Create a new prompt version.

**Request:**
```json
{
  "name": "code-review",
  "version": "1.4.0",
  "template": "Review the following code for {{language}} best practices:\n\n{{code}}",
  "variables": ["language", "code"],
  "metadata": {"author": "team-ai", "change_log": "Added language-specific rules"}
}
```

---

## Benchmark API

LLM benchmarking: SWE-bench, HumanEval, MMLU, and custom benchmarks.

### POST /v1/benchmark/run

Start a benchmark suite.

**Request:**
```json
{
  "name": "provider-comparison-q1",
  "benchmark_type": "humaneval",
  "provider_name": "claude",
  "model_name": "claude-3-opus",
  "config": {
    "sample_size": 100,
    "timeout_per_task": 60,
    "parallel_workers": 4
  }
}
```

**Response:**
```json
{
  "id": "bench-abc123",
  "name": "provider-comparison-q1",
  "benchmark_type": "humaneval",
  "provider_name": "claude",
  "model_name": "claude-3-opus",
  "status": "running",
  "created_at": "2026-04-06T10:30:00Z"
}
```

### GET /v1/benchmark/results

List benchmark results.

**Response:**
```json
{
  "runs": [
    {
      "id": "bench-abc123",
      "name": "provider-comparison-q1",
      "benchmark_type": "humaneval",
      "provider_name": "claude",
      "status": "completed",
      "summary": {
        "pass_rate": 0.842,
        "avg_latency_ms": 2500,
        "total_tokens": 150000
      }
    }
  ],
  "total": 1
}
```

### GET /v1/benchmark/results/:id

Get specific benchmark result with detailed metrics.

---

## Discovery API

Dynamic model discovery with 3-tier fallback (Provider API, models.dev, hardcoded).

### GET /v1/discovery/models

Get all discovered models across providers.

**Response:**
```json
{
  "models": [
    {
      "model_id": "claude-3-opus",
      "model_name": "Claude 3 Opus",
      "provider": "anthropic",
      "verified": true,
      "code_visible": true,
      "overall_score": 9.2,
      "discovered_at": "2026-04-06T10:00:00Z",
      "capabilities": ["vision", "function_calling", "streaming"]
    }
  ],
  "total": 150
}
```

### GET /v1/discovery/models/selected

Get models selected for the debate team.

### GET /v1/discovery/stats

Get discovery statistics.

**Response:**
```json
{
  "total_models_discovered": 150,
  "providers_queried": 43,
  "tier1_hits": 120,
  "tier2_hits": 25,
  "tier3_fallbacks": 5,
  "last_discovery": "2026-04-06T10:00:00Z"
}
```

### POST /v1/discovery/trigger

Trigger a fresh model discovery cycle.

### GET /v1/discovery/ensemble

Get models available for ensemble operations.

### GET /v1/discovery/debate-model

Get the best model for debate based on current scores.

---

## Scoring API

Provider and model scoring with weighted 5-component pipeline.

### GET /v1/scoring/model/:model_id

Get score for a specific model.

**Response:**
```json
{
  "model_id": "claude-3-opus",
  "model_name": "Claude 3 Opus",
  "overall_score": 9.2,
  "score_suffix": "/10",
  "components": {
    "speed_score": 8.5,
    "cost_score": 7.8,
    "efficiency_score": 9.0,
    "capability_score": 9.8,
    "recency_score": 9.5
  },
  "calculated_at": "2026-04-06T10:00:00Z"
}
```

### POST /v1/scoring/batch

Batch calculate scores for multiple models.

### GET /v1/scoring/top

Get top-scored models.

**Query Parameters:**
- `limit` (optional): Number of models (default: 10)
- `provider` (optional): Filter by provider

### GET /v1/scoring/range

Get models within a score range.

**Query Parameters:**
- `min`: Minimum score
- `max`: Maximum score

### GET /v1/scoring/weights

Get current scoring weights.

**Response:**
```json
{
  "response_speed": 0.25,
  "cost_effectiveness": 0.25,
  "model_efficiency": 0.20,
  "capability": 0.20,
  "recency": 0.10
}
```

### PUT /v1/scoring/weights

Update scoring weights.

### GET /v1/scoring/model/:model_id/detail

Get detailed model name with score information.

### POST /v1/scoring/cache/invalidate

Invalidate the scoring cache.

### POST /v1/scoring/compare

Compare scores between models.

**Request:**
```json
{
  "models": ["claude-3-opus", "gpt-4", "gemini-pro"]
}
```

---

## Verification API

Model verification with 8-test pipeline.

### POST /v1/verification/model

Verify a single model.

**Request:**
```json
{
  "model_id": "claude-3-opus",
  "provider": "anthropic"
}
```

### POST /v1/verification/batch

Batch verify multiple models.

### GET /v1/verification/status

Get overall verification status.

**Response:**
```json
{
  "total_models": 150,
  "verified": 120,
  "failed": 15,
  "pending": 15,
  "last_run": "2026-04-06T10:00:00Z"
}
```

### GET /v1/verification/models

Get all verified models.

### POST /v1/verification/model/:model_id/reverify

Re-verify a specific model.

### GET /v1/verification/tests

Get available verification tests.

### GET /v1/verification/health

Get verification system health.

### POST /v1/verification/code-visibility

Test code visibility for a model.

---

## Health Monitoring API

Extended health monitoring for providers, circuit breakers, and latency tracking.

### GET /v1/health/providers

Get health status of all providers.

**Response:**
```json
{
  "providers": [
    {
      "provider_id": "claude",
      "healthy": true,
      "latency_ms": 245,
      "last_check": "2026-04-06T10:30:00Z"
    }
  ],
  "total": 10,
  "healthy": 9
}
```

### GET /v1/health/providers/healthy

Get only healthy providers.

### GET /v1/health/providers/fastest

Get the fastest responding provider.

### GET /v1/health/provider/:provider_id

Get health for a specific provider.

### GET /v1/health/provider/:provider_id/latency

Get latency history for a provider.

### GET /v1/health/provider/:provider_id/available

Check if a provider is available.

### GET /v1/health/circuit-breakers

Get circuit breaker status for all providers.

### POST /v1/health/provider/:provider_id/success

Record a successful request for a provider.

### POST /v1/health/provider/:provider_id/failure

Record a failed request for a provider.

### POST /v1/health/provider

Register a new provider for health monitoring.

### DELETE /v1/health/provider/:provider_id

Remove a provider from health monitoring.

### GET /v1/health/status

Get overall health service status.

---

## Cognee API

Comprehensive knowledge graph integration with memory, cognify, insights, and dataset management.

### GET /v1/cognee/health

Check Cognee service health.

**Response:**
```json
{
  "status": "healthy",
  "service": "cognee",
  "version": "1.0.0",
  "timestamp": 1743940800
}
```

### GET /v1/cognee/stats

Get Cognee usage statistics.

### GET /v1/cognee/config

Get current Cognee configuration.

### POST /v1/cognee/start

Ensure Cognee service is running.

### POST /v1/cognee/memory

Add content to Cognee memory.

**Request:**
```json
{
  "content": "Important information about our project architecture...",
  "metadata": {
    "source": "architecture.md",
    "tags": ["architecture", "design"]
  }
}
```

### POST /v1/cognee/search

Search Cognee knowledge graph.

**Request:**
```json
{
  "query": "What is the project architecture?",
  "limit": 10
}
```

### POST /v1/cognee/cognify

Process content through Cognee's cognification pipeline.

### POST /v1/cognee/insights

Get insights from the knowledge graph.

### POST /v1/cognee/graph/complete

Get graph completion suggestions.

### GET /v1/cognee/visualize

Visualize the knowledge graph.

### POST /v1/cognee/code

Process code through Cognee's code intelligence.

### POST /v1/cognee/datasets

Create a new dataset.

### GET /v1/cognee/datasets

List all datasets.

### DELETE /v1/cognee/datasets/:name

Delete a dataset.

### POST /v1/cognee/feedback

Provide feedback to improve knowledge quality.

---

## Vision API

Image analysis with multiple capabilities: OCR, object detection, captioning, classification.

### GET /v1/vision/health

Check vision service health.

**Response:**
```json
{
  "status": "healthy",
  "service": "vision",
  "version": "1.0.0",
  "capabilities": ["analyze", "ocr", "detect", "caption", "describe", "classify"]
}
```

### GET /v1/vision/capabilities

List all vision capabilities.

### GET /v1/vision/:capability/status

Get status of a specific capability.

### POST /v1/vision/analyze

General image analysis.

**Request:**
```json
{
  "image": "data:image/png;base64,...",
  "prompt": "Describe what you see in this image"
}
```

**Response:**
```json
{
  "analysis": "The image shows a flowchart diagram...",
  "confidence": 0.95,
  "provider": "claude"
}
```

### POST /v1/vision/ocr

Extract text from images.

### POST /v1/vision/detect

Detect objects in images.

### POST /v1/vision/caption

Generate captions for images.

### POST /v1/vision/describe

Generate detailed descriptions.

### POST /v1/vision/classify

Classify image contents.

### POST /v1/vision/:capability

Generic capability endpoint (routes by capability field).

---

## Search API

Vector-based semantic code search with ChromaDB/Qdrant backends.

### POST /v1/search/semantic

Perform semantic code search.

**Request:**
```json
{
  "query": "How does the circuit breaker pattern work?",
  "language": "go",
  "file_pattern": "*.go",
  "top_k": 10,
  "min_score": 0.7,
  "filters": {
    "path_prefix": "internal/"
  }
}
```

**Response:**
```json
{
  "results": [
    {
      "file_path": "internal/services/circuit_breaker.go",
      "content": "func (cb *CircuitBreaker) Execute(...",
      "score": 0.94,
      "line_start": 45,
      "line_end": 78
    }
  ],
  "total_found": 5,
  "query_time_ms": 120
}
```

### POST /v1/search/index

Trigger code indexing.

**Response:**
```json
{
  "files_indexed": 1250,
  "chunks_created": 8500,
  "duration_ms": 15000,
  "errors": []
}
```

---

## Templates API

Reusable prompt templates with Git integration.

### GET /v1/templates

List all templates.

**Response:**
```json
{
  "templates": [
    {
      "id": "code-review",
      "name": "Code Review",
      "description": "Standard code review template",
      "variables": ["language", "file_path"]
    }
  ]
}
```

### GET /v1/templates/:id

Get a specific template.

### POST /v1/templates/apply

Apply a template with variable substitution.

**Request:**
```json
{
  "template_id": "code-review",
  "variables": {
    "language": "Go",
    "file_path": "internal/handlers/router.go"
  }
}
```

---

## Checkpoints API

Workspace snapshots with Git state capture for safe experimentation.

### GET /v1/checkpoints

List all checkpoints.

**Response:**
```json
{
  "checkpoints": [
    {
      "id": "cp-abc123",
      "name": "before-refactor",
      "description": "State before auth refactoring",
      "created_at": "2026-04-06T10:30:00Z",
      "git_ref": "abc1234",
      "git_branch": "feat/auth-refactor",
      "tags": ["safe-point"],
      "file_count": 45
    }
  ]
}
```

### POST /v1/checkpoints

Create a new checkpoint.

**Request:**
```json
{
  "name": "before-refactor",
  "description": "State before auth refactoring",
  "tags": ["safe-point"]
}
```

**Response:**
```json
{
  "id": "cp-abc123",
  "name": "before-refactor",
  "created_at": "2026-04-06T10:30:00Z",
  "git_ref": "abc1234",
  "git_branch": "feat/auth-refactor",
  "file_count": 45
}
```

### POST /v1/checkpoints/:id/restore

Restore workspace to a checkpoint.

### DELETE /v1/checkpoints/:id

Delete a checkpoint.

---

## Browser Automation API

Playwright-based web automation for testing and scraping.

### POST /v1/browser/navigate

Navigate to a URL.

**Request:**
```json
{
  "url": "https://example.com",
  "wait_for": "networkidle",
  "timeout": 30
}
```

**Response:**
```json
{
  "success": true,
  "url": "https://example.com",
  "title": "Example Domain"
}
```

### POST /v1/browser/click

Click an element.

**Request:**
```json
{
  "selector": "button#submit",
  "button": "left"
}
```

### POST /v1/browser/type

Type text into an element.

**Request:**
```json
{
  "selector": "input#search",
  "text": "HelixAgent API",
  "clear": true
}
```

### POST /v1/browser/screenshot

Take a screenshot.

**Request:**
```json
{
  "selector": "#main-content",
  "full_page": false
}
```

### POST /v1/browser/extract

Extract content from the page.

### POST /v1/browser/evaluate

Evaluate JavaScript in the browser context.

---

## Skills API

Skill registry for enhanced LLM completions.

### GET /v1/skills

List all available skills.

**Response:**
```json
{
  "skills": [
    {
      "name": "code-review",
      "description": "Automated code review with best practices",
      "category": "development",
      "tags": ["code", "review", "quality"],
      "version": "1.0.0"
    }
  ],
  "total": 25
}
```

### GET /v1/skills/categories

List skill categories.

**Response:**
```json
{
  "categories": ["development", "testing", "documentation", "security", "devops"]
}
```

### GET /v1/skills/:category

Get skills in a specific category.

### POST /v1/skills/match

Match skills to a query.

**Request:**
```json
{
  "query": "I need to review this code for security issues",
  "limit": 5
}
```

**Response:**
```json
{
  "matches": [
    {"name": "security-review", "relevance": 0.95},
    {"name": "code-review", "relevance": 0.82}
  ]
}
```

---

## GraphQL API

Feature-flagged GraphQL endpoint (requires `GRAPHQL_ENABLED=true`).

### POST /v1/graphql

Execute a GraphQL query.

**Request:**
```json
{
  "query": "{ providers { name status score models { id name } } }",
  "variables": {}
}
```

**Response:**
```json
{
  "data": {
    "providers": [
      {
        "name": "claude",
        "status": "verified",
        "score": 9.2,
        "models": [
          {"id": "claude-3-opus", "name": "Claude 3 Opus"}
        ]
      }
    ]
  }
}
```

---

## Startup and Infrastructure

### GET /v1/startup/verification

Get startup verification results.

**Response:**
```json
{
  "status": "completed",
  "providers_verified": 10,
  "providers_failed": 2,
  "debate_team_ready": true,
  "duration_ms": 45000,
  "timestamp": "2026-04-06T10:00:00Z"
}
```

### GET /v1/bigdata/components

Get BigData component status (when BigData features are enabled).

**Response:**
```json
{
  "components": {
    "neo4j": {"enabled": true, "status": "connected"},
    "clickhouse": {"enabled": true, "status": "connected"},
    "kafka": {"enabled": false, "status": "disabled"}
  }
}
```

### GET /v1/debates/orchestrator/status

Get NEW debate orchestrator framework status.

**Response:**
```json
{
  "framework": "new_debate_orchestrator",
  "documentation_compliance": "docs/requests/debate requirements",
  "target_agents": 15,
  "statistics": {
    "agent_count": 12,
    "sessions_completed": 45,
    "avg_duration_ms": 8500
  }
}
```

### Debug Endpoints (ENABLE_PPROF=true)

When `ENABLE_PPROF=true` is set, the following debug endpoints are available:

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/debug/pprof/` | pprof index |
| `GET` | `/debug/pprof/cmdline` | Command line arguments |
| `GET` | `/debug/pprof/profile` | CPU profile |
| `GET` | `/debug/pprof/symbol` | Symbol lookup |
| `GET` | `/debug/pprof/trace` | Execution trace |
| `GET` | `/debug/pprof/goroutine` | Goroutine profile |
| `GET` | `/debug/pprof/heap` | Heap profile |
| `GET` | `/debug/pprof/threadcreate` | Thread creation profile |
| `GET` | `/debug/pprof/block` | Block profile |
| `GET` | `/debug/pprof/mutex` | Mutex contention profile |

---

## LLMsVerifier Capability Detection API

### Go Package API

```go
import "llm-verifier/capabilities"

// Create detector
detector := capabilities.NewDetector()

// Detect provider capabilities dynamically
caps, err := detector.DetectProviderCapabilities(ctx, "openai", apiKey)

// Query specific capabilities
sseType := capabilities.StreamingTypeSSE
query := &capabilities.CapabilityQuery{
    Provider:         "openai",
    RequireStreaming: &sseType,
    RequireVision:    true,
}
result, err := detector.Query(ctx, query)

// Get full capability matrix
matrix := detector.GetCapabilityMatrix()
sseProviders := matrix.ByStreaming[capabilities.StreamingTypeSSE]

// Generate CLI agent configuration
generator := capabilities.NewConfigGenerator("localhost", 7061)
config, err := generator.GenerateForAgent("opencode", nil)
```

### Capability Types

#### StreamingType
```go
StreamingTypeSSE           // Server-Sent Events
StreamingTypeWebSocket     // WebSocket
StreamingTypeAsyncGen      // AsyncGenerator/yield
StreamingTypeJSONL         // JSON Lines streaming
StreamingTypeMpscStream    // Rust MPSC channel
StreamingTypeEventStream   // AWS EventStream
StreamingTypeStdout        // Standard output
StreamingTypeNone          // No streaming
```

#### CompressionType
```go
CompressionGzip     // Gzip compression
CompressionBrotli   // Brotli compression
CompressionSemantic // Semantic context compression
CompressionChat     // Chat history compression
```

#### CachingType
```go
CachingAnthropic    // Anthropic cache_control
CachingDashScope    // DashScope X-DashScope-CacheControl
CachingPrompt       // Generic prompt caching
CachingSemantic     // Semantic similarity caching
CachingLLMOps       // LangChain/SQLite cache
```

#### ProtocolType
```go
ProtocolMCP         // Model Context Protocol
ProtocolACP         // Agent Communication Protocol
ProtocolLSP         // Language Server Protocol
ProtocolGRPC        // gRPC
ProtocolOpenAI      // OpenAI-compatible API
ProtocolAnthropic   // Anthropic API
ProtocolOllama      // Ollama local API
```

### Key Functions

```go
// Get provider capabilities
caps := capabilities.GetProviderBaseCapabilities("openai")

// Get CLI agent capabilities
agentCaps := capabilities.GetCLIAgentCapabilities("kilocode")

// List all providers
providers := capabilities.GetAllProviders()

// List all CLI agents
agents := capabilities.GetAllCLIAgents()

// Find providers with specific capability
streamingProviders := capabilities.GetProvidersWithCapability("streaming", nil)
oauthProviders := capabilities.GetProvidersWithCapability("oauth", nil)

// Find CLI agents with specific capability
mcpAgents := capabilities.GetCLIAgentsWithCapability("mcp")
checkpointAgents := capabilities.GetCLIAgentsWithCapability("checkpointing")
```

---

## QA API

The HelixQA subsystem exposes six endpoints under `/v1/qa` for autonomous QA
pipeline management, findings lifecycle tracking, platform discovery, and
project knowledge extraction.

Full documentation: **[QA_API_REFERENCE.md](QA_API_REFERENCE.md)**

### Quick Reference

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/qa/sessions` | Start an autonomous QA session across one or more platforms |
| `GET` | `/v1/qa/findings` | List findings, optionally filtered by status (`open`, `fixed`, `verified`) |
| `GET` | `/v1/qa/findings/:id` | Get a specific finding by ID (e.g. `HELIX-001`) |
| `PUT` | `/v1/qa/findings/:id` | Update a finding's lifecycle status |
| `GET` | `/v1/qa/platforms` | List supported QA platforms (`android`, `web`, `desktop`, `cli`, `api`, …) |
| `POST` | `/v1/qa/discover` | Discover project documentation, constraints, and credentials |

### Example: Start a Web QA Session

```bash
curl -s -X POST http://localhost:7061/v1/qa/sessions \
  -H "Authorization: Bearer $HELIXAGENT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "project_root": "/home/user/myapp",
    "platforms": ["web"],
    "web_url": "http://localhost:3000",
    "output_dir": "/tmp/qa-run-01"
  }' | jq .
```

**Response:**
```json
{
  "status": "completed",
  "session_id": "a3f1b2c4-dead-beef-cafe-000000000001",
  "duration": 184000000000,
  "tests_planned": 42,
  "tests_run": 38,
  "issues_found": 5,
  "tickets_created": 5,
  "coverage_pct": 74.3
}
```

### Example: List Open Findings

```bash
curl -s "http://localhost:7061/v1/qa/findings?status=open" \
  -H "Authorization: Bearer $HELIXAGENT_API_KEY" | jq .
```

See [QA_API_REFERENCE.md](QA_API_REFERENCE.md) for complete request/response
schemas, all error codes, and curl examples for every endpoint.

---

## Error Responses

All endpoints return standard error responses:

```json
{
  "error": {
    "code": "invalid_request",
    "message": "The request body is malformed.",
    "details": {
      "field": "messages",
      "issue": "required field missing"
    }
  }
}
```

### Error Codes

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `invalid_request` | 400 | Malformed request |
| `authentication_error` | 401 | Invalid or missing API key |
| `permission_denied` | 403 | Insufficient permissions |
| `not_found` | 404 | Resource not found |
| `rate_limited` | 429 | Too many requests |
| `internal_error` | 500 | Server error |
| `service_unavailable` | 503 | Service temporarily unavailable |

---

## Rate Limits

| Endpoint | Limit | Window |
|----------|-------|--------|
| `/v1/chat/completions` | 60 | 1 minute |
| `/v1/debates` | 10 | 1 minute |
| `/v1/embeddings` | 100 | 1 minute |
| `/v1/tasks` | 30 | 1 minute |

Rate limit headers:
```
X-RateLimit-Limit: 60
X-RateLimit-Remaining: 45
X-RateLimit-Reset: 1705555260
```

---

## WebSocket Endpoints

### WS /v1/ws/tasks/:id

Real-time task updates via WebSocket.

**Messages:**
```json
// Progress update
{"type": "progress", "data": {"progress": 50.0, "message": "Building..."}}

// Output
{"type": "output", "data": {"stream": "stdout", "content": "Compiling module..."}}

// Complete
{"type": "complete", "data": {"status": "completed", "exit_code": 0}}

// Error
{"type": "error", "data": {"code": "task_failed", "message": "Build failed"}}
```

---

## SDK Examples

### Python

```python
import requests

response = requests.post(
    "http://localhost:7061/v1/chat/completions",
    headers={"Authorization": "Bearer YOUR_API_KEY"},
    json={
        "model": "helixagent-debate",
        "messages": [{"role": "user", "content": "Hello!"}],
        "stream": False
    }
)
print(response.json()["choices"][0]["message"]["content"])
```

### TypeScript

```typescript
const response = await fetch("http://localhost:7061/v1/chat/completions", {
  method: "POST",
  headers: {
    "Authorization": "Bearer YOUR_API_KEY",
    "Content-Type": "application/json"
  },
  body: JSON.stringify({
    model: "helixagent-debate",
    messages: [{ role: "user", content: "Hello!" }],
    stream: false
  })
});
const data = await response.json();
console.log(data.choices[0].message.content);
```

### Go

```go
import "dev.helix.agent/client"

client := client.New("http://localhost:7061", "YOUR_API_KEY")
response, err := client.ChatCompletion(ctx, &client.ChatRequest{
    Model: "helixagent-debate",
    Messages: []client.Message{
        {Role: "user", Content: "Hello!"},
    },
})
fmt.Println(response.Choices[0].Message.Content)
```

---

## Related Documentation

- [Capability Detection](../../LLMsVerifier/docs/CAPABILITY_DETECTION.md) - Full capability detection documentation
- [CLI Agent Registry](../../CLAUDE.md#cli-agent-registry) - Detailed CLI agent information
- [AI Debate Team](../../CLAUDE.md#ai-debate-team-composition) - Debate team configuration
- [Background Execution](../architecture/README.md) - Background task system
- [Challenge Scripts](./challenges/scripts/) - Validation challenges
