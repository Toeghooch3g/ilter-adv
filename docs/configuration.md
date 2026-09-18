# Ilter Advanced Configuration Guide

> Ilter Advanced uses a **zero-configuration** approach. Compiled-in defaults work out of the box. Override any setting via `ILTER_*` environment variables. No YAML file is required.

---

## Quick Start

```bash
# Works immediately with compiled-in defaults:
./ilter serve

# Interactive setup wizard (providers, routing, feature flags):
./ilter init

# Deterministic demo data for development:
./ilter init --demo
```

---

## Configuration Architecture

Ilter Advanced separates operational boot configuration from dynamic gateway business logic using two distinct layers:

| Layer | Source | Scope & Lifecycle | Examples |
|-------|--------|------------------|----------|
| **Boot Config** | Environment variables (`ILTER_*`) + compiled-in defaults | Loaded once at startup. Requires process restart to change. | Server ports, SQLite file path, log level, metrics listen address, Redis URL |
| **Runtime Config** | SQLite `runtime_config` table | Dynamic state. Updates take effect **immediately** without process restart. | Upstream provider credentials, MCP servers, OpenAPI specs, guardrails, routing strategies, feature flags |

---

## Environment Variables (Boot Config)

All environment variables use the `ILTER_` prefix:

| Variable | Default | Description |
|----------|---------|-------------|
| `ILTER_SERVER_PORT` | `8181` | Main Proxy HTTP listen port |
| `ILTER_DASHBOARD_PORT` | `9191` | Embedded Web Dashboard UI port |
| `ILTER_METRICS_LISTEN_ADDR` | `:9192` | OpenTelemetry Prometheus `/metrics` listen address |
| `ILTER_STORAGE_PATH` | `"./data/ilter.db"` | SQLite database file path |
| `ILTER_ADMIN_API_KEY` | (auto-generated) | Admin API break-glass key. Also derives AES-256-GCM key for provider secret encryption at rest |
| `ILTER_LOG_LEVEL` | `"info"` | Log level: `debug`, `info`, `warn`, `error` |
| `ILTER_REDIS_URL` | `""` (none) | Redis connection URL shared by rate limiter and semantic cache |
| `ILTER_CACHE_TYPE` | `""` (`redis`) | Semantic cache backend: `redis` (default), `postgres`, or `disabled` |
| `ILTER_CACHE_PG_DSN` | `""` (none) | Postgres DSN for the semantic cache backend (e.g. `postgres://user:pass@host:5432/db?sslmode=require`) |
| `ILTER_CACHE_PG_REQUIRE_VECTORSCALE` | `true` | Require the `pgvectorscale` extension; set `false` to fall back to plain `pgvector` |
| `ILTER_CACHE_EMBEDDING_MODEL` | `""` (legacy Ollama) | `provider:model` for cache embeddings (e.g. `openai:text-embedding-3-small`); empty falls back to the legacy Ollama embedder |
| `ILTER_CACHE_RERANK_MODEL` | `""` (none) | `provider:model` for the cache reranker (e.g. `cohere:rerank-english-v3.0`); empty disables rerank |
| `ILTER_CACHE_RERANK_TOPK` | `10` | Number of KNN candidates pulled before rerank |

### Semantic Cache Backends

The semantic cache stores embeddings + responses and serves semantic hits on top of a SHA-256 exact-match key. Three modes are supported, selected by `ILTER_CACHE_TYPE`:

- **`redis`** (default): Redis Stack VSS. Uses `ILTER_REDIS_URL`. Embedding dimension comes from the embedder (legacy Ollama is 768D).
- **`postgres`**: PostgreSQL with the `pgvector` (and optionally `pgvectorscale`) extension. Requires `ILTER_CACHE_PG_DSN`. Each embedding model gets its own `ilter_semantic_cache_<sanitized_model>` table so switching models preserves old cached data.
- **`disabled`**: disables the cache entirely. Requires no Redis, no Postgres DSN, and no embedding provider — the middleware short-circuits before reading or embedding any request body, so every request misses cache. Use this when you run Ilter Advanced without any cache backend or embedding credentials.

The embedding model is selected per-provider with `ILTER_CACHE_EMBEDDING_MODEL=provider:model`. The cache probes the provider for a real embedding at startup to learn the vector dimension, then persists it in `runtime_config` (`semantic_cache` / `embedding_dim`) so a restart with the same model skips the probe. Supported values include `openai:text-embedding-3-small` (1536D), `voyage:voyage-3` (1024D), any provider implementing the embeddings capability, and `ollama:nomic-embed-text` (768D, the legacy default when the env is empty).

Reranking is optional: set `ILTER_CACHE_RERANK_MODEL=provider:model` to run a two-stage read (KNN top-K via `ILTER_CACHE_RERANK_TOPK`, then rerank). If the reranker fails, the cache degrades to the KNN top-1 result. Empty disables rerank.

**Postgres extension requirements.** The Postgres backend requires the `vector` extension; with `ILTER_CACHE_PG_REQUIRE_VECTORSCALE=true` (default) it additionally requires `vectorscale` (and `timescaledb_toolkit` for StreamingDiskANN). Set it to `false` to fall back to a plain `pgvector` HNSW index. To run Postgres for the cache, use the bundled orchestration, which brings up the `timescale/timescaledb-ha:pg16` Docker image:

```bash
make services-up-pg          # docker compose --profile pg up -d --wait ollama postgres
```

The `timescale/timescaledb-ha:pg16` image only preloads `timescaledb` and `timescaledb_toolkit`; the `vector` and `vectorscale` extensions must be created explicitly. The compose service mounts `deploy/initdb/01-extensions.sql` into `/docker-entrypoint-initdb.d`, which runs `CREATE EXTENSION IF NOT EXISTS vector; CREATE EXTENSION IF NOT EXISTS vectorscale;` on first boot. Do **not** install Postgres via Homebrew to test this — use the Timescale Docker container.

To run the Postgres integration tests against a live backend:

```bash
make services-up-pg
make test-pg                # runs ./internal/features/semanticcache -run TestPostgresBackend
```

#### Container-only procedure (Podman, no host OS changes)

On a host where Docker Desktop is not running (or where you must **not** modify the host OS or install packages), run the Postgres integration tests entirely inside an existing Podman VM. This validates the same `TestPostgresBackend_*` suite against a real pgvectorscale Postgres without touching the host.

Prerequisites: a Podman machine already exists (the repo CI/dev box has `podman-machine-default`), the Go toolchain is on the host, and the pgx driver is wired in (the semanticcache package imports it).

1. Start the Podman VM and launch the Timescale Postgres with the extension init script mounted:

```bash
podman machine start podman-machine-default
export DOCKER_HOST='unix:///Users/aaron/.local/share/containers/podman/machine/qemu/podman.sock'

podman run -d --name ilter-pg-test \
  -e POSTGRES_USER=ilter \
  -e POSTGRES_PASSWORD=ilter \
  -e POSTGRES_DB=ilter_cache \
  -p 55432:5432 \
  -v "$PWD/deploy/initdb:/docker-entrypoint-initdb.d:ro" \
  docker.io/timescale/timescaledb-ha:pg16
```

The `deploy/initdb/01-extensions.sql` mount runs `CREATE EXTENSION IF NOT EXISTS vector; CREATE EXTENSION IF NOT EXISTS vectorscale;` on first boot — required, or the backend fails at startup with a missing `vector` extension.

2. Wait for readiness and confirm both extensions are present:

```bash
podman exec ilter-pg-test pg_isready -U ilter -d ilter_cache
podman exec ilter-pg-test psql -U ilter -d ilter_cache -tAc \
  "SELECT extname FROM pg_extension WHERE extname IN ('vector','vectorscale') ORDER BY 1;"
# expect:
#   vector
#   vectorscale
```

3. Run the integration tests from the host, pointing `ILTER_CACHE_PG_DSN` at the published port:

```bash
ILTER_TEST_POSTGRES=1 \
ILTER_CACHE_PG_DSN='postgres://ilter:ilter@127.0.0.1:55432/ilter_cache?sslmode=disable' \
  go test -race -count=1 -v ./internal/features/semanticcache/ -run TestPostgresBackend
```

Expect all four to pass: `ExactOnly`, `DynamicDim`, `SemanticHitAndMiss`, `TopK`. With `vector` + `vectorscale` installed, the backend selects the **pgvectorscale (StreamingDiskANN)** path. Confirmation the DB is genuinely exercised: the run creates `ilter_semantic_cache`, `ilter_semantic_cache_fake_dimtest`, `_fake_smh`, and `_fake_topk` tables with real rows (e.g. `_fake_topk` holds 3).

4. Tear down (leaves the host unchanged):

```bash
podman rm -f ilter-pg-test
podman machine stop podman-machine-default
```

> Port `55432` is used above only to avoid clashing with a local Postgres; the compose default DSN is `postgres://ilter:ilter@localhost:5432/ilter_cache?sslmode=disable`. The tests skip cleanly when `ILTER_TEST_POSTGRES=1` or `ILTER_CACHE_PG_DSN` is unset. This procedure was validated 2026-08-11 against `git rev-parse HEAD` = `8166355` (branch `custom_provider_support`).

### Provider API Key Overrides (Boot Time)

Provider keys can be set via environment variables. When supplied, they populate initial provider settings:

```bash
ILTER_PROVIDER_OPENAI_API_KEY=sk-... \
ILTER_PROVIDER_ANTHROPIC_API_KEY=sk-ant-... \
ILTER_PROVIDER_DEEPSEEK_API_KEY=sk-... \
ILTER_PROVIDER_DEEPINFRA_API_KEY=... \
ILTER_PROVIDER_GEMINI_API_KEY=... \
ILTER_PROVIDER_OPENROUTER_API_KEY=sk-... \
  ./ilter serve
```

Setting `ILTER_PROVIDER_<NAME>_API_KEY` is enough to auto-register that provider at boot — no `ilter init` needed. Native provider types include `deepinfra` (base URL `https://api.deepinfra.com/v1/openai`), which supports chat, streaming, embeddings, and model discovery.

**Service tier (flex/priority).** Each provider may carry a default service tier injected into every chat request that does not set one (the client's own `service_tier` wins). Set it with `ILTER_PROVIDER_<NAME>_SERVICE_TIER` (env wins over the DB registration value) or the provider's runtime registration. Valid values: `default`, `priority`, `flex` (OpenAI- and DeepInfra-compatible; DeepInfra also accepts `priority`/`flex`/`default`). Tier-differentiated pricing is out of scope — catalog prices are the default-tier estimate.

---

## Dynamic Runtime Configuration

Runtime settings are managed in the SQLite database and updated hot in memory via an internal cache event bus (`config.Cache`). Changes take effect across all worker goroutines **without restarting the service**.

### Runtime Config Management Options

1. **Interactive CLI Setup Wizard**: `./ilter init`
2. **Web Dashboard**: Interactive UI on port `9191` (`http://localhost:9191`)
3. **Admin & REST API**: Endpoints under `/admin/*` and `/api/*` on ports `8181` and `9191`

### Catalog of Runtime Config Sections

| Section | Description | API Endpoint Path |
|---------|-------------|-------------------|
| **Providers** | Manage upstream API keys, base URLs, active models | `/api/providers` |
| **Model Overrides** | Per-provider manual model list (pricing/context/tier), lowest-priority model source | `/api/providers/{name}/models-overrides` |
| **MCP Servers** | Register stdio/SSE MCP servers, tool access grants (`mcp_grant`) | `/api/mcp/servers` |
| **MCP Blocked Tools** | Tool names hidden from every MCP surface (chat injection, gateway `tools/list`, hub `tools/list`) and rejected at call time. Each entry must be a tool's server-prefixed name as shown by `tools/list` (e.g. `kagi_search-search`); bare and legacy `server__tool` forms no longer match | `PUT /api/config/mcp/blocked_tools` with `{"value": ["kagi_search-search", ...]}` |
| **Tool Pricing** | Per-tool MCP call pricing rules billed to the caller's budget + `usage_daily` (per-call or per-page/URL units) | `PUT /api/config/tool_pricing/<rule>` with `{"value": {"server":"kagi","tool":"search","unit":"call","cost_per_unit":0.012}}` |
| **OpenAPI Specs** | Register REST APIs via OpenAPI specs for automatic MCP conversion | `/api/openapi/specs` |
| **Guardrail Rules** | Configure prompt injection, toxicity, and topic block filters | `/api/guardrails/rules` |
| **Routing Strategy** | Define heuristic thresholds, model tiers, and rule DSL | `/api/smart-loadbalancer/strategies` |
| **Feature Flags** | Hot toggle PII masking, semantic cache, rate limit, budget, and loop detection | `/api/features` |
| **Prompt Templates** | Versioned system prompts with traffic deployment splits | `/api/prompts` |
| **Jobs Engine** | Define scheduled LLM + MCP tasks with cron and webhook triggers | `/api/jobs` |

### Model Source Precedence

For every provider, the set of available models (pricing, context limits, tier, capabilities) is resolved from three sources, in ascending priority (highest wins):

1. **Manual model overrides** (lowest): a JSON list of `{id, display_name, tier, cost_per_input_token, cost_per_output_token, max_context_tokens, max_output_tokens, capabilities}` entries, keyed by provider in the `model_overrides` runtime_config section. Edited only through a JSON file: download from the dashboard **Models JSON** button or upload via **Upload** (`GET`/`PUT /api/providers/{name}/models-overrides`). A boot-time file path can seed it when no runtime entry exists (`ProviderConfig.ModelOverridesFile`).
2. **Provider discovery** (highest): the provider's `/v1/models` endpoint. For each model ID, any detail the endpoint reports wins per-field over the manual override; details it does not report fall back to the override, then to heuristics. Models reported only by the endpoint are added; models present only in the override are kept.

Heuristic defaults (naming-convention pricing/tier/context) fill any field neither source supplies.

### Managing Providers

Providers can be registered through any of three entry points, all of which target the same runtime `provider` section and take effect **immediately** (hot reload — no restart):

1. **Web Dashboard → Providers → Add Provider**: fill in a name, type, base URL, and optional API key.
2. **`ilter init` wizard**: choose **Custom (OpenAI-compatible)** to register a provider with a custom base URL (stored as an `openai`-type provider), alongside the built-in types.
3. **Admin API**: `POST /api/providers/create` (see `api/openapi.yaml` and `internal/dashboard/providers/create.go`).

#### Custom (OpenAI-compatible) providers

A *custom* provider is simply an `openai`-type registration with a user-supplied base URL — it reuses the full OpenAI provider stack (chat, streaming, embeddings, rerank, model discovery) against your endpoint. You can register as many as you like; each is a distinct instance keyed by its unique name, so two custom providers never collide even when they share a type and expose the same model IDs.

```json
POST /api/providers/create
{
  "name": "my-self-hosted",
  "type": "openai",
  "base_url": "https://llm.example.com/v1",
  "api_key": "sk-..."
}
```

#### Hot reload

Whenever the `provider` or `model_overrides` runtime sections change — provider create/edit, or a Models JSON upload — the app rebuilds the live provider registry and load-balancer routes automatically via the config-cache change watcher (`internal/app/providers.go`). Discovery is re-run (bypassing its cooldown) so new models appear without a restart.

---

## Configuration Precedence & Order

When evaluating settings, Ilter Advanced applies configuration in the following order (highest precedence wins):

1. **Request-level Headers & Context** (e.g., explicit model in request payload)
2. **Runtime Configuration** (SQLite `runtime_config` table state)
3. **Environment Variables** (`ILTER_*` env vars)
4. **Compiled-in Defaults**
