# HelixLLM User Manual

## Complete Guide to Using HelixLLM with HelixAgent

---

## Table of Contents

1. [Introduction](#introduction)
2. [Prerequisites](#prerequisites)
3. [Installation](#installation)
4. [Configuration](#configuration)
5. [Running HelixLLM](#running-helixllm)
6. [Testing & Verification](#testing--verification)
7. [API Usage](#api-usage)
8. [Troubleshooting](#troubleshooting)
9. [Advanced Topics](#advanced-topics)

---

## Introduction

HelixLLM is an enterprise-grade distributed LLM system that provides OpenAI-compatible APIs, local LLM inference, RAG capabilities, and multi-agent support. When integrated with HelixAgent, it becomes a first-class provider alongside Gemini, DeepSeek, Claude, and others.

### Key Features

- **OpenAI-compatible API**: Drop-in replacement for OpenAI
- **Local LLM Inference**: Run models locally with llama.cpp
- **RAG Pipeline**: Document ingestion and retrieval
- **Multi-agent Support**: ReAct agent system with tool calling
- **HTTP/3 Support**: High-performance QUIC protocol
- **Container Orchestration**: Managed via Containers module

---

## Prerequisites

### System Requirements

- **OS**: Linux (Ubuntu 20.04+ recommended)
- **CPU**: 4+ cores
- **RAM**: 8GB minimum, 16GB recommended
- **Disk**: 50GB free space
- **Container Runtime**: Podman or Docker

### Software Requirements

- **Go**: 1.25.3+ (for HelixAgent)
- **Git**: 2.30+
- **Make**: GNU Make

### Network Requirements

- Port 8443 (HelixLLM HTTPS)
- Port 5433 (PostgreSQL)
- Port 6381 (Redis)
- Port 6333-6334 (Qdrant)
- Port 9093 (Kafka)

---

## Installation

### Step 1: Clone HelixAgent with Submodules

```bash
git clone --recurse-submodules git@github.com:vasic-digital/HelixAgent.git
cd HelixAgent
```

### Step 2: Initialize HelixLLM Submodules

```bash
# If not already initialized
git submodule update --init --recursive HelixLLM

# Initialize HelixLLM's own submodules
cd HelixLLM
git submodule update --init --recursive
cd ..
```

### Step 3: Configure Environment

```bash
# Copy environment file
cp .env.example .env

# Edit configuration
nano .env
```

Add these settings:
```bash
# HelixLLM is ON by default since the local-first default (HA-F2-002) —
# this line is only needed to make the opt-in explicit or to document intent.
USE_HELIX_LLM=true

# Cloud provider auto-discovery is OFF by default — opt in only if you
# want env-credentialed cloud providers and the anonymous zen endpoint:
# HELIX_CLOUD_PROVIDERS=true

# HelixLLM Configuration
HELIX_LLM_ENDPOINT=https://localhost:8443
HELIX_LLM_TLS_SKIP_VERIFY=true
HELIX_LLM_MODE=full

# Integration Features
HELIX_LLM_USE_HELIXAGENT_MCP=true
HELIX_LLM_USE_HELIXAGENT_LSP=true
HELIX_LLM_USE_HELIXAGENT_ACP=true
HELIX_LLM_USE_HELIXAGENT_EMBEDDINGS=true
HELIX_LLM_USE_HELIXAGENT_RAG=true
HELIX_LLM_USE_HELIXAGENT_MEMORY=true
```

---

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `USE_HELIX_LLM` | `true` (local-first default, HA-F2-002) | Local HelixLLM/llama.cpp chain. Default ON; set to an explicit `false`/`0`/`no`/`off` to opt OUT |
| `HELIX_CLOUD_PROVIDERS` | `false` | Cloud provider auto-discovery (env-credentialed cloud providers + anonymous zen). Default OFF; set to `true`/`1`/`yes`/`on` to opt IN |
| `HELIX_LLM_ENDPOINT` | `https://localhost:8443` | HelixLLM API endpoint |
| `HELIX_LLM_API_KEY` | - | API key (if required) |
| `HELIX_LLM_TLS_SKIP_VERIFY` | `false` | Skip TLS verification (secure-by-default; set `true` only for local dev against self-signed certs) |
| `HELIX_LLM_MODE` | `full` | Deployment mode |
| `HELIX_LLM_DB_HOST` | `helixllm-postgres` | PostgreSQL host |
| `HELIX_LLM_REDIS_HOST` | `helixllm-redis` | Redis host |

### Deployment Modes

| Mode | Description |
|------|-------------|
| `full` | All-in-one deployment (recommended) |
| `gateway` | API gateway only |
| `brain` | LLM inference only |
| `knowledge` | RAG pipeline only |
| `agents` | Agent workers only |
| `control` | Cluster management only |

---

## Running HelixLLM

### Option 1: Automated Startup (Recommended)

HelixAgent automatically manages HelixLLM containers when `USE_HELIX_LLM=true`:

```bash
export USE_HELIX_LLM=true
./bin/helixagent
```

HelixAgent will:
1. Start HelixLLM infrastructure containers
2. Build and start HelixLLM binary (if available)
3. Register HelixLLM as a provider
4. Begin accepting requests

### Option 2: Development Mode

For development with auto-reload:

```bash
cd HelixLLM
make dev
```

**Note:** Do not start HelixLLM infrastructure containers manually with `podman-compose` or `docker-compose`. The HelixAgent binary is the sole orchestrator for all containers, including HelixLLM infrastructure. When `USE_HELIX_LLM=true`, running `./bin/helixagent` handles everything automatically.

---

## Testing & Verification

### Quick Verification

```bash
# Run challenge tests
./challenges/scripts/helixllm_integration_challenge.sh

# Expected output: 11/11 PASSED
```

### Full Test Suite

```bash
# Run LLMsVerifier comprehensive tests
./tests/helixllm/llmsverifier_test_suite.sh

# View results
ls -la reports/helixllm-verification-*/
cat reports/helixllm-verification-*/REPORT.md
```

### Manual API Testing

```bash
# Test health endpoint
curl -k https://localhost:8443/internal/health

# Test models endpoint
curl -k https://localhost:8443/v1/models

# Test chat completion
curl -k -X POST https://localhost:8443/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "helixllm-default",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

---

## API Usage

### Using HelixLLM via HelixAgent

```bash
# Chat completion through HelixAgent
curl -X POST http://localhost:7061/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer ${HELIXAGENT_API_KEY}" \
  -d '{
    "model": "helixllm",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "What can you do?"}
    ]
  }'
```

### Direct HelixLLM API

```bash
# List available models
curl -k https://localhost:8443/v1/models

# Chat completion
curl -k -X POST https://localhost:8443/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "helixllm-default",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": false
  }'

# Embeddings
curl -k -X POST https://localhost:8443/v1/embeddings \
  -H "Content-Type: application/json" \
  -d '{
    "model": "all-mpnet-base-v2",
    "input": ["Hello world"]
  }'
```

### Python SDK Example

```python
from openai import OpenAI

# Connect to HelixAgent
client = OpenAI(
    base_url="http://localhost:7061/v1",
    api_key="your-api-key"
)

# Use HelixLLM
response = client.chat.completions.create(
    model="helixllm",
    messages=[
        {"role": "user", "content": "Hello!"}
    ]
)

print(response.choices[0].message.content)
```

---

## Troubleshooting

### Issue: HelixLLM Not Starting

**Symptoms:** Containers not starting, API unavailable

**Solution:**
```bash
# Check container status (inspection only)
podman ps -a

# View logs (inspection only)
podman logs helixagent-helixllm-postgres
podman logs helixagent-helixllm-redis

# Restart by restarting HelixAgent (do not use podman-compose directly)
pkill helixagent
./bin/helixagent
```

### Issue: Provider Not Registered

**Symptoms:** "helixllm" model not available

**Solution:**
```bash
# Check provider registration
grep helixllm internal/services/provider_registry.go

# Verify compilation
go build ./internal/llm/providers/helixllm/...

# Restart HelixAgent
pkill helixagent
./bin/helixagent
```

### Issue: API Authentication Errors

**Symptoms:** 401 Unauthorized errors

**Solution:**
```bash
# Set API key
export HELIX_LLM_API_KEY="your-key"

# Or disable authentication (development only)
export HELIX_LLM_AUTH_REQUIRED=false
```

### Issue: TLS Certificate Errors

**Symptoms:** Certificate validation failed

**Solution:**
```bash
# Skip TLS verification (development)
export HELIX_LLM_TLS_SKIP_VERIFY=true

# Or generate certificates
cd HelixLLM
make certs
```

---

## Advanced Topics

### Performance Tuning

```bash
# Increase concurrent connections
export HELIX_LLM_MAX_CONCURRENT=100

# Adjust model parameters
export HELIX_LLM_TEMPERATURE=0.7
export HELIX_LLM_MAX_TOKENS=4096
```

### Monitoring

```bash
# View Prometheus metrics
curl http://localhost:9091/metrics

# Check Grafana (if enabled)
open http://localhost:3001
```

### Backup and Restore

```bash
# Backup PostgreSQL
podman exec helixagent-helixllm-postgres pg_dump -U helix helixllm > backup.sql

# Restore PostgreSQL
podman exec -i helixagent-helixllm-postgres psql -U helix helixllm < backup.sql
```

### Updating HelixLLM

```bash
# Update submodule
git submodule update --remote HelixLLM

# Rebuild
cd HelixLLM
git pull
make build

# Restart
pkill helixllm
./bin/helixllm
```

---

## Best Practices

### Production Deployment

1. **Enable TLS**: Set `HELIX_LLM_TLS_SKIP_VERIFY=false` and use valid certificates
2. **Set API Keys**: Configure authentication for all endpoints
3. **Monitor Resources**: Set up alerts for CPU, memory, and disk usage
4. **Regular Backups**: Schedule automated database backups
5. **Test Failover**: Verify high availability configuration

### Development Workflow

1. **Use Dev Mode**: `make dev` for auto-reload during development
2. **Run Tests**: Execute test suite before committing changes
3. **Check Logs**: Monitor logs for errors and warnings
4. **Version Control**: Commit changes to both main repo and submodule

---

## Support Resources

### Documentation

- Integration Guide: `docs/HELIXLLM_INTEGRATION.md`
- Testing Guide: `docs/HELIXLLM_TESTING_GUIDE.md`
- API Reference: `HelixLLM/docs/user-guide/api-reference.md`

### Getting Help

1. Check logs: `reports/helixllm-verification-*/test.log`
2. Review troubleshooting section above
3. Consult HelixLLM manual: `HelixLLM/docs/manual/architecture.md`

---

## Quick Reference Card

### Essential Commands

```bash
# Start everything (containers managed automatically)
USE_HELIX_LLM=true ./bin/helixagent

# Run tests
./tests/helixllm/llmsverifier_test_suite.sh

# Check health
curl -k https://localhost:8443/internal/health

# View logs
tail -f /tmp/helixagent-server.log

# Stop HelixAgent (containers stop with it)
pkill helixagent
```

### Key URLs

| Service | URL |
|---------|-----|
| HelixLLM API | https://localhost:8443 |
| HelixAgent API | http://localhost:7061 |
| Qdrant | http://localhost:6333 |
| Prometheus | http://localhost:9091 |

---

*Last Updated: April 6, 2026*
