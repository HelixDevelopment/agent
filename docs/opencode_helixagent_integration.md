# OpenCode (Zen) Integration with HelixAgent

**Revision:** 1  
**Last modified:** 2026-07-24T00:00:00Z

## Overview

HelixAgent integrates OpenCode's CLI agents through the Zen provider type.
Zen wraps OpenCode's Go-based CLI agent runtime, enabling anonymous free model
usage via built-in fallback chains and paid model support with API keys.

## Architecture

```
HelixAgent (provider_registry.go)
  → Zen provider (providers/zen/)
    → OpenCode CLI (zen_cli.go)
    → OpenCode HTTP API (zen_http.go)
```

The Zen provider supports two transport modes:
- **CLI mode** — invokes OpenCode's native Go CLI directly
- **HTTP mode** — communicates with an OpenCode server over HTTP

## Configuration

### YAML Configuration

```yaml
providers:
  zen:
    type: zen
    enabled: true
    models:
      - id: "big-pickle"
        name: "Big Pickle (Free)"
        enabled: true
        weight: 1.0
      - id: "glm-5-free"
        name: "GLM-5 Free"
        enabled: true
        weight: 0.9
      - id: "kimi-k2"
        name: "Kimi K2 (Paid)"
        enabled: true
        weight: 1.1
```

### JSON Configuration

```json
{
  "providers": {
    "zen": {
      "type": "zen",
      "enabled": true,
      "models": [
        {"id": "big-pickle", "name": "Big Pickle (Free)", "enabled": true, "weight": 1.0},
        {"id": "glm-5-free", "name": "GLM-5 Free", "enabled": true, "weight": 0.9},
        {"id": "kimi-k2", "name": "Kimi K2 (Paid)", "enabled": true, "weight": 1.1}
      ]
    }
  }
}
```

## Adding Free Anonymous Models

Free anonymous models do not require an API key. Register them by name and
weight in the `models` array:

```yaml
providers:
  zen:
    type: zen
    enabled: true
    models:
      - id: "big-pickle"
        name: "Big Pickle (Free)"
        enabled: true
        weight: 1.0          # Higher weight = preferred in fallback chain
      - id: "glm-5-free"
        name: "GLM-5 Free"
        enabled: true
        weight: 0.9
```

The Zen provider will attempt models in descending weight order. If the
highest-weight model is unavailable, it falls back to the next model.

## Adding Paid Models with API Key

Paid models require an API key set via environment variable:

```bash
export ZEN_API_KEY="your-api-key-here"
```

Then add the paid model to the configuration:

```yaml
providers:
  zen:
    type: zen
    enabled: true
    api_key: "${ZEN_API_KEY}"    # resolved at runtime
    base_url: "https://api.zen.example.com/v1"
    models:
      - id: "kimi-k2"
        name: "Kimi K2 (Paid)"
        enabled: true
        weight: 1.1
```

## Environment Variables

| Variable          | Description                          | Required          |
|-------------------|--------------------------------------|-------------------|
| `ZEN_API_KEY`     | API key for paid Zen models          | For paid models   |
| `ZEN_BASE_URL`    | Base URL for Zen HTTP API            | No (has default)  |
| `USE_HELIX_LLM`        | Local HelixLLM/llama.cpp chain. **Default ON** — set to an explicit `false`/`0`/`no`/`off` to opt OUT | No |
| `HELIX_CLOUD_PROVIDERS` | Cloud provider auto-discovery (every env-credentialed cloud provider + anonymous zen). **Default OFF** — set to `true`/`1`/`yes`/`on` to opt IN | No |

## Generic Provider (OpenAI-Compatible)

For any OpenAI-compatible API that does not have a dedicated HelixAgent provider
implementation, use the `generic` provider type:

```yaml
providers:
  custom_llm:
    type: generic
    enabled: true
    api_key: "${CUSTOM_API_KEY}"
    base_url: "https://api.example.com/v1/chat/completions"
    models:
      - id: "custom-model-v1"
        name: "Custom Model v1"
        enabled: true
        weight: 1.0
```

The generic provider supports:
- OpenAI-compatible chat completions (`POST /v1/chat/completions`)
- Streaming responses (SSE)
- Health checks
- Capability introspection

## Default Registration

The Zen provider is registered by default in `provider_registry.go` with three
models: Big Pickle (free), GLM-5 Free, and Kimi K2 (paid). It is enabled by
default — no additional configuration is required for free model usage.

Custom Zen configurations in the provider registry YAML/JSON will override the
defaults.
