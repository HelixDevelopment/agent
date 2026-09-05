# Gap Ledger — helix_agent — spec 002 (local adaptive serving) — Task F2

- Date: 2026-09-05
- Scope: `submodules/helix_agent` — FR-019 state, provider bluff evidence, default-provider reachability.
- Method: read-only extraction. Every `file:line` below was opened and read in this session. Anything not directly observed is marked `UNCONFIRMED:`.
- Related spec: `/home/milosvasic/Projects/helix_code/specs/002-adaptive-local-model-serving/spec.md` FR-019 (line 272).

## Step 1 — FR-019 state (Models host/avail/withheld)

The FR-019 machinery EXISTS and is real, concentrated in `internal/catalog`:

- Declaration: `internal/catalog/option.go:28-44` — three-valued `Availability` (`""` unreported / `"serving"` / `"withheld"`) with `Usable()` true only for `serving`; closed `WithheldReason` set at `option.go:65-106` (5 values); `HelixLLMOption` at `option.go:116-132` (ID vs ModelIdentity split).
- Wire decode: `internal/catalog/helixllm_source.go:31-75` — `OptionsFromModels` reads the serving layer's GET /v1models additions (`model_identity`, `host`, `availability`, `withheld_reason` — declared in `internal/adapters/helixllm/types.go:150-170`); unknown availability → unreported (never usable), unknown reason discarded.
- Catalog projection: `internal/catalog/option.go:187-218` (`helixLLMEntries`) binds `Enabled = Availability.Usable()`; `internal/catalog/catalog.go:83-90` carries `availability`/`withheld_reason` on `Entry`.
- Serving: `GET /v1/catalog` at `internal/catalog/handler.go:22-31`, routed at `internal/router/router.go:852` (protected group).
- Wiring: `internal/router/router.go:1677-1690` (`newCatalogOptions`) → `router.go:1731-1764` (`newHelixLLMCatalogSource`) — the ONLY production `catalog.New` call site (`router.go:850`). Gated on `USE_HELIX_LLM == "true"` (`router.go:1678`). Listing bounded: 3 s timeout / 30 s TTL (`router.go:1703,1709`).

Gaps recorded below (HA-F2-001, HA-F2-005).

## Step 2 — Provider bluff evidence

The brief's anchor — `internal/provider/openai_compatible.go` ~line 2925, "nonce echo" — is **UNCONFIRMED in its claimed form**:

- No `internal/provider/` directory exists in helix_agent; the real file is `internal/handlers/openai_compatible.go` (8210 lines).
- The string `nonce` does not appear anywhere in that file (nor in helix_agent `internal/` at all — only `runOnce`/`shutdownOnce` substring false-positives).
- Line ~2925 (in `convertSingleResponseToOpenAI`, func at `:2858-2940`) is an HONEST token-split report: it emits the provider's real per-direction counts and its comment documents a previous `TokensUsed/2` synthesis that was already fixed.

The nearest REAL bluff in the same facade is the hardcoded `/v1/models` list (HA-F2-001 below).

## Step 3 — Default-provider check

How helix_agent chooses its LLM endpoint today:

- Provider registry auto-discovery is ON by default: `internal/services/provider_registry.go:325-331` (`NewProviderRegistry` → `enableAutoDiscovery = true` unless `cfg.DisableAutoDiscovery`).
- Auto-discovery registers EVERY cloud provider whose API key sits in the environment (`provider_registry.go:420-480`, `initAutoDiscovery` → `DiscoverProviderCredentials` → `RegisterProvider`).
- The `zen` (OpenCode) provider is enabled by default under auto-discovery with NO credential: `provider_registry.go:850-863` (`Enabled: r.autoDiscovery`), lazily materializing an anonymous client to a public endpoint via `zen.NewZenProviderAnonymous` (`provider_registry.go:1742-1748`). Comment at `:833-849` confirms this is a deliberate implicit cloud provider acquisition.
- The local HelixLLM/llama.cpp path is OFF by default everywhere it matters: `USE_HELIX_LLM == "true"` required at `provider_registry.go:788`, `router.go:1678`, `internal/handlers/openai_compatible.go:2685,2751`.
- Endpoint resolution when enabled: `internal/llm/providers/helixllm/provider.go:102-140` (`resolveEndpoint`: explicit → `HELIX_LLM_LOCAL_OPENAI_ENDPOINT` → `HELIX_LLM_ENDPOINT` → `HELIX_LLM_HOST/PORT` → default `https://localhost:8443`, `:30`).

Conclusion: **cloud paths ARE reachable by default** (env-credentialed providers + anonymous zen), while the local serving path requires an explicit opt-in. Spec 002's "all local LLM execution" posture is not the default (HA-F2-002).

## Ledger

| id | severity | file:line | defect | fix_direction |
|---|---|---|---|---|
| HA-F2-001 | high | `internal/handlers/openai_compatible.go:2289-2348` | `UnifiedHandler.Models` (and duplicate `ModelsPublic` at `:2351-2353`, which no route registers) returns a hardcoded list of 5 pseudo-models (`helixagent-debate`, `helixagent-llm`, `helixagent-ensemble`, `helix-debate`, `helix-llm`) with full `allow_*` permissions and NO availability/withheld/host field of any kind. This is the `/v1/models` facade that CLI agents "pre-validate against" (comment at `:2300-2301`) — it presents every pseudo-model as usable whether or not anything is serving it, which is exactly what FR-019 forbids. The truthful `/v1/catalog` (Step 1) is a different endpoint; the facade never consults it. | Make `/v1/models` serve from the same catalog service (or at minimum annotate each entry with the catalog's `availability`), so a stopped/absent model is not presented as usable; delete or wire `ModelsPublic` deliberately. |
| HA-F2-002 | high | `internal/services/provider_registry.go:325-331,420-480,850-863` + `provider_registry.go:788` | Cloud endpoints are reachable by default: auto-discovery (default ON) registers any env-credentialed cloud provider, and the credential-less `zen` provider talks to a public endpoint anonymously by default — while the local llama.cpp/HelixLLM path stays OFF unless `USE_HELIX_LLM=true` (also `router.go:1678`, `handlers/openai_compatible.go:2685,2751`). Prompts can leave the host with zero operator action; spec 002's local-first posture is inverted. | For spec 002 deployments: gate auto-discovery and the anonymous zen provider behind the same local-first switch (or a `DISABLE_CLOUD_PROVIDERS`-class default-deny), keep llama.cpp/Colibri as the default chain, and require an explicit operator opt-in for any cloud route. |
| HA-F2-003 | medium | `internal/adapters/helixllm/adapter.go:49-56` | The adapter's own default contradicts the system's: `cfg.Enabled = getEnvBool("USE_HELIX_LLM", true)` defaults the adapter to ENABLED, while every system-level gate uses strict `== "true"` (default OFF: `provider_registry.go:788`, `router.go:1678`). Any future caller that constructs the adapter with a zero `Config` silently gets an enabled adapter. Today the only production construction passes `Enabled: true` explicitly behind the gate (`router.go:1736-1737`), so this is latent, not live. | Align the adapter default with the system (`getEnvBool("USE_HELIX_LLM", false)`) or remove the field's defaulting entirely and require callers to state intent. |
| HA-F2-004 | medium | `internal/llm/providers/helixllm/provider.go:443-469` (with `:65,192-194`) | `GetCapabilities` hardcodes `SupportedModels: []string{p.model}` where `p.model` defaults to the non-evidenced id `helixllm-default` (`:65`), plus broad hardcoded capability flags (`SupportsReasoning: true`, `SupportsCodeAnalysis: true`, …) that nothing on the serving layer confirmed. The registry also synthesizes the same id (`provider_registry.go:789-794`). The catalog treats `SupportedModels` only as a display hint (`catalog.go:100-104`), but the verifier/registry surfaces still carry a fabricated model id + capability set (CONST-036/CONST-040 class). | Populate `SupportedModels` from a live GET /v1/models (serving-only entries) and derive capability flags from verifier metadata instead of struct literals. |
| HA-F2-005 | low | `internal/catalog/option.go:187-218` + `internal/router/router.go:1677-1690` | FR-019 availability is produced and served ONLY on `GET /v1/catalog`; no Go consumer in the repo reads `availability`/`withheld_reason` (grep: only catalog package + its tests). The `helixllm/<host>/<model>[:<variant>]` ModelIdentity (scheme confirmed in helix_llm `internal/naming/identity.go:6,121`) is carried verbatim as a label and never parsed by any Go consumer; downstream handling of the `[:<variant>]` suffix is UNCONFIRMED (no consumer exists in-repo to verify). | Wire at least one first-party consumer (e.g. FR-017 provider-config generation or the TUI model picker) to filter on `availability == "serving"`, and add a contract test that a withheld option never lands in a generated consumer config. |
| HA-F2-006 | info (UNCONFIRMED) | claimed `internal/provider/openai_compatible.go:~2925` | Claimed "nonce echo" bluff anchor cannot be confirmed: the path does not exist (real file `internal/handlers/openai_compatible.go`), no `nonce` token exists anywhere in the file or in helix_agent `internal/`, and line ~2925 is an honest provider-reported token-split. Nearest real facade bluff is HA-F2-001. | Re-source the anchor (which repo/file/line produced the "nonce echo" claim); if it referred to the hardcoded `/v1/models` list, track HA-F2-001 instead. |

## JSON

```json
[
  {
    "id": "HA-F2-001",
    "repo": "helix_agent",
    "severity": "high",
    "file": "internal/handlers/openai_compatible.go:2289-2348",
    "defect": "UnifiedHandler.Models returns a hardcoded list of 5 pseudo-models with full allow_* permissions and no availability/withheld/host field; the OpenAI-compatible /v1/models facade that CLI agents pre-validate against presents every pseudo-model as usable whether or not anything is serving it (FR-019 violation). Duplicate ModelsPublic at :2351-2353 is registered on no route.",
    "fix_direction": "Serve /v1/models from the catalog service (or annotate each entry with catalog availability) so a stopped/absent model is never presented as usable; delete or deliberately wire ModelsPublic.",
    "test_plan": "RED: start with no provider serving and assert /v1/models does not mark pseudo-models available (or lists them availability=withheld); GREEN: with the catalog sourced from a live listing, assert served models report serving and stopped ones do not appear as usable. Paired mutation: reintroduce the hardcoded unconditional list -> test FAILs."
  },
  {
    "id": "HA-F2-002",
    "repo": "helix_agent",
    "severity": "high",
    "file": "internal/services/provider_registry.go:325-331,420-480,850-863,788",
    "defect": "Cloud endpoints are reachable by default: auto-discovery (default ON) registers any env-credentialed cloud provider and the credential-less zen provider (Enabled: r.autoDiscovery, provider_registry.go:855) talks to a public endpoint anonymously via NewZenProviderAnonymous (:1742-1748), while the local llama.cpp/HelixLLM path requires USE_HELIX_LLM=true (default OFF). Spec 002's all-local posture is inverted; prompts can leave the host with zero operator action.",
    "fix_direction": "For spec 002 deployments gate auto-discovery and the anonymous zen provider behind a local-first / default-deny-cloud switch; make llama.cpp+Colibri the default chain with explicit operator opt-in for any cloud route.",
    "test_plan": "RED: with no USE_HELIX_LLM and no cloud opt-in, assert a chat request routes to a local llama.cpp endpoint (or fails closed) and never to a public/cloud endpoint; assert zen is not registered. GREEN: same assert with the local chain up. Paired mutation: flip the local-first gate off -> cloud route reachable -> test FAILs."
  },
  {
    "id": "HA-F2-003",
    "repo": "helix_agent",
    "severity": "medium",
    "file": "internal/adapters/helixllm/adapter.go:49-56",
    "defect": "Adapter's own defaulting sets cfg.Enabled = getEnvBool(\"USE_HELIX_LLM\", true), contradicting every system-level gate that requires USE_HELIX_LLM == \"true\" (default OFF: provider_registry.go:788, router.go:1678). Latent divergence: any future zero-Config construction silently enables the adapter.",
    "fix_direction": "Change the adapter default to false (or drop defaulting and require callers to state Enabled explicitly), matching the system-wide gate.",
    "test_plan": "Unit: construct adapter with zero Config and USE_HELIX_LLM unset -> Enabled must be false. Integration: catalog source construction without the gate must yield no HelixLLM options. Paired mutation: revert default to true -> test FAILs."
  },
  {
    "id": "HA-F2-004",
    "repo": "helix_agent",
    "severity": "medium",
    "file": "internal/llm/providers/helixllm/provider.go:443-469",
    "defect": "GetCapabilities hardcodes SupportedModels to [p.model] where p.model defaults to the non-evidenced id \"helixllm-default\" (:65,:192-194), plus broad hardcoded capability flags (SupportsReasoning/SupportsCodeAnalysis/... true) no serving layer confirmed; registry default config synthesizes the same id (provider_registry.go:789-794). Catalog treats SupportedModels as display hint only, but the fabricated id+capabilities still surface via registry/verifier (CONST-036/040 class).",
    "fix_direction": "Populate SupportedModels from live GET /v1/models (serving-only) and source capability flags from verifier metadata instead of struct literals.",
    "test_plan": "RED: with HelixLLM serving 0 models, assert GetCapabilities reports no supported models; with N serving models, assert exactly those N. GREEN: assert flags match verifier-reported capabilities. Paired mutation: restore the hardcoded literal list -> test FAILs."
  },
  {
    "id": "HA-F2-005",
    "repo": "helix_agent",
    "severity": "low",
    "file": "internal/catalog/option.go:187-218",
    "defect": "FR-019 availability/withheld_reason is produced and served only on GET /v1/catalog; no Go consumer in the repo reads those fields (only catalog package + tests). The helixllm/<host>/<model>[:<variant>] ModelIdentity is carried verbatim as a label, never parsed by any Go consumer; downstream [:<variant>] handling UNCONFIRMED (no in-repo consumer exists).",
    "fix_direction": "Wire a first-party consumer (FR-017 provider-config generation or TUI model picker) to filter on availability == \"serving\"; add a contract test that withheld options never reach a generated consumer config.",
    "test_plan": "Integration: catalog containing a withheld option -> generated consumer config must exclude it and (where the format allows) record the withheld_reason. Paired mutation: strip the availability filter in the generator -> withheld model leaks into config -> test FAILs."
  },
  {
    "id": "HA-F2-006",
    "repo": "helix_agent",
    "severity": "info",
    "file": "UNCONFIRMED: claimed internal/provider/openai_compatible.go:~2925 (does not exist; real file internal/handlers/openai_compatible.go, line ~2925 is an honest token-split report)",
    "defect": "Claimed 'nonce echo' bluff anchor cannot be confirmed: no internal/provider directory, no 'nonce' token anywhere in helix_agent internal/, and the cited region is honest provider-reported token accounting. Nearest real facade bluff is HA-F2-001.",
    "fix_direction": "Re-source the anchor; if it meant the hardcoded /v1/models list, track HA-F2-001.",
    "test_plan": "N/A — anchor unconfirmed; no test authored against an unverified location (§11.4.6 no-guessing)."
  }
]
```
