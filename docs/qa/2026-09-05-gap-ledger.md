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


---

## Remediation record — HA-F2-002 (added 2026-09-06)

Everything above this line is the 2026-09-05 read-only snapshot of the PRE-fix
state and is left unedited. This section records what actually landed.

### Correction to commit `7ad2b508`

`7ad2b508` ("local-first serving defaults — local chain ON, cloud opt-in")
delivered the `USE_HELIX_LLM` flip correctly, but its message and the docs it
shipped claimed `HELIX_CLOUD_PROVIDERS` covered "every env-credentialed cloud
provider". **That claim was false**, and is recorded here rather than quietly
dropped (§11.4 — an overstated closure is a PASS-bluff at the documentation
layer on top of the functional gap).

What `7ad2b508` actually gated: **one** site —
`NewProviderRegistry`'s `enableAutoDiscovery := CloudProvidersOptedIn()`
(`internal/services/provider_registry.go:331`), which transitively covered
`initAutoDiscovery` and the synthesized anonymous-`zen` default.

What it did NOT gate, both live in the standard deployment:

1. `LoadRegistryConfigFromAppConfig` (`provider_registry.go`, called from
   `router.go:263`) set `Enabled: <KEY> != ""` for
   deepseek/claude/gemini/qwen/openrouter, so `createProviderFromConfig`'s
   `cfg.Enabled && cfg.APIKey != ""` branch built a live cloud client from an
   ambient key. The chat handler's `ListProvidersOrderedByScore` fallback and
   `debate_service`'s `GetProvider(...)` calls could then route to it.
2. `verifier.discoverProviders` (`internal/verifier/startup.go`), reached at
   every boot from `cmd/helixagent/main.go` `runStartupVerification`, called
   `DiscoverModels(...)` per env-credentialed provider and then sent real
   verification prompts via `createProviderForVerification`.
   `grep HELIX_CLOUD_PROVIDERS internal/verifier/` returned zero hits.

The shipped `docker-compose.yml:159-169` forwards `CLAUDE_API_KEY`,
`DEEPSEEK_API_KEY`, `GEMINI_API_KEY` and `QWEN_API_KEY` from the operator's
`.env` into the container, so this was the normal path, not an edge case.

Why the existing RED test did not catch it: `TestProviderRegistry_LocalFirstDefaults`
calls `clearProviderEnvVarsForTest(t)` first, so it never observed a SET key
still enabling a provider, and it asserted internal flags rather than the
absence of a cloud route.

### What landed (2026-09-06)

- One shared predicate in the leaf package `internal/localfirst`
  (`CloudProvidersOptedIn`, `HelixLLMEnabled`). `internal/services` delegates to
  it; `internal/verifier` imports it directly, retiring the duplicated
  `getEnvBoolVerifier("USE_HELIX_LLM", true)` mirror that existed because
  services imports verifier.
- Four gate sites, all the same predicate:
  1. `NewProviderRegistry` — auto-discovery + anonymous zen (unchanged, from `7ad2b508`).
  2. `LoadRegistryConfigFromAppConfig` — env-credentialed provider `Enabled`.
  3. `verifier.discoverProviders` + `discoverFreeProviders` — boot-time cloud
     discovery/verification and the anonymous zen probe. Provider-CLASS aware:
     `AuthTypeLocal` entries (`helixllm`, `ollama`) stay discoverable.
  4. `NewEmbeddingManager` — the ambient `OPENAI_API_KEY` embedding path behind
     the wired `POST /v1/protocols/execute` (`protocol_type: embedding`), which
     the original review did not name. Degrades to the existing local embedding
     fallback rather than failing.
- Deliberately NOT gated (each is already an explicit operator decision):
  a provider named by hand in the registry config; `SEARCH_EMBEDDER_TYPE=openai`;
  the separate `cmd/sanity-check` operator tool; OAuth session refresh for
  credentials the operator established with `claude login` / `qwen login`.
- Guards (§11.4.115 polarity switch, §11.4.135 standing regression guards):
  `internal/services/cloud_optin_route_red_test.go`,
  `internal/services/cloud_optin_embedding_red_test.go`,
  `internal/verifier/cloud_optin_discovery_red_test.go`.
  These observe the ROUTE — a constructed client, or a counted outbound request
  through a swapped transport / httptest endpoint — not a configuration flag.
  Both polarities are covered: cloud must be refused when unset AND allowed when
  the operator opts in.

### Honest boundary (§11.4.6)

Gating boot-time verification means an operator who wants their cloud keys
verified at startup must set `HELIX_CLOUD_PROVIDERS=true`. This is coherent
rather than a loss: with cloud not opted in, nothing can route to those
providers, so verifying them buys nothing and costs an outbound request plus a
billed prompt per provider. The opt-in restores the old behaviour exactly, and
`TestCloudGate_VerifierDiscoversCloudWithOptIn` proves it.

Not claimed: that no cloud path remains anywhere in the tree. The enumeration
covered `internal/` and `cmd/` non-test sources; `cmd/sanity-check` and
`SEARCH_EMBEDDER_TYPE=openai` are known, deliberately-ungated, separately-opted-in
paths, and `internal/embeddings/models/registry.go` carries an ungated OpenAI
default that is currently unreachable (the RAG pipeline it belongs to is wired
with `Pipeline: nil` at `router.go:1235`) — it will need this same gate if that
wiring is ever completed.

---

## Round-8 review remediation — HA-F2-002 (added 2026-09-06)

Append-only, like the section above: nothing earlier in this file is edited.
The two items below SUPERSEDE the corresponding statements in the previous
section, which are left in place as the record of what was believed then.

### The fifth gate site: OAuth session refresh (was "deliberately NOT gated")

The previous section listed, under "Deliberately NOT gated", *"OAuth session
refresh for credentials the operator established with `claude login` /
`qwen login`."* That classification was too generous and is now **withdrawn**;
the path is gated.

What was actually there (`internal/router/router.go:428-441`, pre-fix):
`authadapter.GetOAuthCredentialPaths()`
(`internal/adapters/auth/integration.go:383-399`) `os.Stat`s
`~/.claude/.credentials.json` and `~/.qwen/oauth_creds.json`. **File presence
alone** — no env var, no config, no operator statement of intent — was enough
to construct an `OAuthCredentialManager` and `Start()` a 5-minute ticker whose
`RefreshAll` POSTs `grant_type=refresh_token` to
`https://api.anthropic.com/oauth/token` and
`https://dashscope.aliyuncs.com/api/token`. The only guard was
`!standaloneMode`, i.e. it ran in exactly the production configuration.

Why the earlier reasoning fails: the `claude login` that wrote that file
authorised **Claude Code**, not HelixAgent. An ambient credential file on disk
is not the operator asking *this* service to reach a third party — and that is
precisely the distinction the other four sites are gated on. Being latent
today (the generic reader unmarshals a flat
`{access_token, refresh_token, expires_at}` while the real Claude file nests
under `claudeAiOauth` and the Qwen file uses a millisecond `expiry_date`, so
`NeedsRefresh` never fires against the real files) is a schema accident, not a
guarantee; it arms itself the day either upstream schema changes.

Landed:

- `internal/router/oauth_credentials.go` — `newOAuthCredentialManager`, gated
  on the same `localfirst.CloudProvidersOptedIn()` predicate as the other four.
  `SetupRouterWithContext` now calls it instead of inlining the block.
- `internal/adapters/auth/integration.go:408` — the identical ungated block in
  `InitializeAuthIntegration` is gated too. That function has **no production
  caller** today (only `auth_middleware_test.go:514`), so this closes the shape
  rather than a live route; left in place per §11.4.124 rather than deleted.
- Guard: `internal/router/cloud_optin_oauth_red_test.go`, matching the sibling
  guards' shape — it counts outbound requests through a swapped
  `http.DefaultTransport` (the refresher's client has a nil `Transport`), with
  the seeded credential files carrying the FLAT schema and a past expiry so the
  refresh genuinely fires. Both polarities plus the standalone-mode boundary.

Honest boundary (§11.4.6): the operator loses nothing. With cloud not opted in
there is no route a refreshed token could serve, and `HELIX_CLOUD_PROVIDERS=true`
restores the refresh loop unchanged — `TestCloudGate_OAuthRefreshStartsWithOptIn`
proves it.

### Correction: why the `embeddings/models/registry.go` landmine is unreachable

The previous section justified the ungated `OPENAI_API_KEY` read at
`internal/embeddings/models/registry.go:159` as unreachable because
"the RAG pipeline it belongs to is wired with `Pipeline: nil` at
`router.go:1235`". That is true but **not load-bearing** — it argues from one
consumer's configuration rather than from reachability, and would stop holding
the moment anyone constructed a pipeline anywhere.

The firmer reason, both halves verified in this session:

1. `NewEmbeddingModelRegistry` (`internal/embeddings/models/registry.go:116`)
   is the **sole caller** of the `loadDefaultConfigs()` that performs the
   ungated read (`registry.go:132` → `:157-190`), and it has **zero non-test
   callers module-wide**. Every call site is a `_test.go` file
   (`internal/handlers/rag_handler_test.go:72`,
   `internal/rag/pipeline_test.go:53`,
   `internal/rag/pipeline_extended_test.go:376`,
   `internal/embeddings/models/registry{,_extended}_test.go`,
   `tests/unit/rag/pipeline_test.go:73,236,244,259,283`).
2. `RAGHandlerConfig.EmbeddingRegistry` (`internal/handlers/rag_handler.go:23`)
   is likewise never set outside tests — the router call site
   (`router.go:1237-1240`) passes only `Pipeline` and `Logger`.

So no production code path constructs the registry at all; the `Pipeline: nil`
wiring is a second, weaker line of defence behind that. The gate is still owed
if the registry is ever constructed from production code — which is what makes
fact 1 the one worth stating, since it is the fact that would change.
