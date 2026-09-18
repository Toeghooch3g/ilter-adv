# AGENTS.md — Guidance for AI coding agents working in ILTER

This file is written for AI coding agents (Claude Code, Cursor, Copilot, and similar) and human contributors alike. It tells you how this repository is structured, what the invariants are, and what must not be broken. It is a complement to — not a replacement for — [`CONTRIBUTING.md`](CONTRIBUTING.md) and the docs under [`docs/`](docs/).

---

## What ILTER is

ILTER is a self-hosted **AI gateway** in a single Go binary. It sits between your application and upstream LLM providers and adds auth, budget control, PII masking, guardrails, MCP tool injection, smart routing, loop detection, semantic caching, and observability — without changing your application's wire format.

Two runtimes coexist in this repo:

| Runtime | Language | Location |
|---------|----------|----------|
| Gateway core | Go 1.26 | `cmd/ilter`, `internal/` |
| Embedded dashboard | Astro + React (Bun) | `web/` |

The web dashboard is compiled **into** the Go binary via `embed` (`web/embed.go`). There is no separate server.

---

## Building and running

```bash
make build       # sync-version + web build + go build → ./ilter
make dev         # Air (Go hot reload) + Vite (dashboard HMR) concurrently
make test        # go test -race -count=1 ./...
make check       # build + lint (Go + web)
make fix         # gofumpt + biome format + modernize
```

Run the gateway:

```bash
./ilter serve          # compiled-in defaults, zero config
./ilter init           # interactive setup wizard
./ilter init --demo    # demo data for dashboard development
```

- Proxy API: `http://localhost:8181`
- Dashboard: `http://localhost:9191`
- Metrics: `http://localhost:9192/metrics`

`serve` **refuses to start** unless both `ILTER_ADMIN_API_KEY` and at least one provider key are set. Setting `ILTER_PROVIDER_<NAME>_API_KEY` (e.g. `ILTER_PROVIDER_OPENAI_API_KEY`) is enough to enable that provider — no `ilter init` needed.

Default/dev admin key is `test` (see `Makefile`). Do not use it in production.

---

## Repository layout

```
api/openapi.yaml        Full REST + proxy API spec (single source of truth for endpoints)
cmd/ilter/              CLI entrypoints (serve, init, models)
internal/
  app/                  App construction + wiring (run.go, init.go)
  auth/                 API-key auth (Argon2id/SHA-256 + LRU cache)
  config/               Boot + runtime configuration. ILTER_* env vars registered here
  dashboard/            Admin REST handlers (providers, keys, budget, ...)
  db/                   SQLite store, goose migrations, sqlc queries
  features/<domain>/    Transport-agnostic feature logic (pii, budget, guardrails, semanticcache, ...)
  middleware/           HTTP adapters; the 11-step request chain (see doc.go)
  model/                Model catalog + request/response model types
  provider/             Upstream provider adapters (openai, anthropic, gemini, ...)
  proxy/                OpenAI/Anthropic-compatible proxy handlers
  version/              Single version string, injected via -ldflags
web/                    Astro + React dashboard source
deploy/initdb/          Postgres bootstrap SQL (pgvector + pgvectorscale extensions)
docs/                   All user/operator documentation (see below)
```

---

## The request middleware chain — do not reorder

Every proxy request flows through one ordered chain defined in `internal/middleware/doc.go`. The order is intentional and correctness-critical:

```
auth → rate limit → budget → PII masking → guardrails
     → MCP injection → smart routing → loop detection
     → semantic cache → provider
```

PII masking **must** run before the semantic cache (so sensitive data never reaches embedding models). Auth runs first. If you believe the order should change, open an issue first — do not reorder the chain in a PR without strong justification.

### Feature / middleware boundary (strict)

Every feature lives in `internal/features/<domain>/` and has **zero knowledge of HTTP**. The HTTP adapter lives in `internal/middleware/<feature>.go`. Features export transport-agnostic methods; middleware parses HTTP and calls the feature. Do not blur this boundary.

---

## Configuration model (critical)

ILTER uses **zero-configuration**: compiled-in defaults, overridden by `ILTER_*` env vars at boot, then by hot runtime config stored in SQLite. There is **no YAML/TOML config file** (and Viper is explicitly rejected — see CONTRIBUTING).

Two layers:

| Layer | Source | Lifecycle |
|-------|--------|-----------|
| Boot config | `ILTER_*` env vars + compiled defaults | Loaded once at startup; requires restart to change |
| Runtime config | SQLite `runtime_config` table | Hot, applies immediately without restart (dashboard/`/api/*`/`ilter init`) |

### Env vars are registered in Go, not read ad hoc

Every boot env var is registered in `internal/config/env.go` via `RegisterEnv[T]`. **To add an env var, register it there** and it will be validated and surfaced by the config registry (see `internal/config/show.go`). There is no struct-flattening library. A strict validation pass rejects unknown `ILTER_*` vars — a typo is a boot error, not silent misconfiguration.

Recent semantic-cache additions (see `internal/config/env.go`):

| Env var | Default | Purpose |
|---------|---------|---------|
| `ILTER_CACHE_TYPE` | `redis` | Semantic cache backend: `redis` \| `postgres` |
| `ILTER_CACHE_PG_DSN` | (none) | Postgres DSN for the cache backend (e.g. `postgres://user:pass@host:5432/db?sslmode=require`) |
| `ILTER_CACHE_PG_REQUIRE_VECTORSCALE` | `true` | Require `pgvectorscale`; set `false` to fall back to plain `pgvector` |
| `ILTER_CACHE_EMBEDDING_MODEL` | (empty → legacy Ollama) | `provider:model` for cache embeddings (e.g. `openai:text-embedding-3-small`) |
| `ILTER_CACHE_RERANK_MODEL` | (none) | `provider:model` for the cache reranker; empty disables rerank |
| `ILTER_CACHE_RERANK_TOPK` | `10` | Number of KNN candidates pulled before rerank |

Provider keys are seeded from `ILTER_PROVIDER_<NAME>_API_KEY` (single) or `ILTER_PROVIDER_<NAME>_API_KEYS` (plural, comma/newline-separated). A provider with a key set but no DB entry is auto-registered at boot. See `internal/config/provider_keys.go`.

### Model source precedence

For each provider, the set of available models (pricing, context, tier, capabilities) resolves from three sources, ascending priority:

1. **Manual model overrides** (lowest): JSON list keyed by provider in the `model_overrides` runtime_config section. Edited only via a JSON file — dashboard **Models JSON** button or `GET`/`PUT /api/providers/{name}/models-overrides`. A boot-time file path (`ModelOverridesFile` on the provider registration) seeds it when no runtime entry exists.
2. **Provider discovery** (highest): the provider's `/v1/models` endpoint. Reported fields win per-field; unreported fields fall back to the override, then heuristics. Endpoint-only models are added; override-only models are kept.

### Providers: hot reload

Provider registrations live in the runtime `provider` section. Create/edit via Dashboard → Providers, `ilter init` (Custom/OpenAI-compatible), or `POST /api/providers/create`. When the `provider` or `model_overrides` sections change, the app rebuilds the live provider registry and load-balancer routes automatically (no restart) via the config-cache change watcher (`internal/app/providers.go`). A *custom* provider is just an `openai`-type registration with a user base URL — you can register as many as needed, each keyed by unique name.

---

## Database

- **App data**: SQLite via `modernc.org/sqlite` (pure Go, **no CGo**), WAL mode. Path: `ILTER_STORAGE_PATH` (default `./data/ilter.db`).
- **Migrations**: goose, under `internal/db/migrations/`. Add a new `.up.sql` for schema changes. Never edit an applied migration.
- **Queries**: sqlc, source in `internal/db/queries/*.sql`, generated into `internal/db/sqlc/`. Regenerate with `make sqlc-gen`.
- **Optional Redis** (rate limiting + semantic cache): fails open.
- **Optional Postgres** (semantic cache backend only, not app storage): needs `pgvector` + `pgvectorscale`. App data remains in SQLite.

### Semantic cache Postgres backend

Two storage backends, selected by `ILTER_CACHE_TYPE`:

- **`redis`** (default): Redis Stack VSS. Embedding dim from the embedder (legacy Ollama = 768D).
- **`postgres`**: pgvector (optionally pgvectorscale). Each embedding model gets its own `ilter_semantic_cache_<sanitized_model>` table, so switching models preserves old data.

The embedder is resolved per-provider via `ILTER_CACHE_EMBEDDING_MODEL=provider:model`. The cache probes the real provider for an embedding at startup to learn the vector dim, then persists it in `runtime_config` (`semantic_cache` → `embedding_dim`). Degradation is graceful throughout: embedder failure → exact-only (SHA-256); reranker failure → KNN top-1.

**To test the Postgres backend, use the Timescale Docker image — not Homebrew Postgres.** The `timescale/timescaledb-ha:pg16` image preloads only `timescaledb` + `timescaledb_toolkit`; `vector` and `vectorscale` must be created explicitly. The compose service mounts `deploy/initdb/01-extensions.sql`.

```bash
make services-up-pg    # docker compose --profile pg up -d --wait ollama postgres
make test-pg           # ./internal/features/semanticcache -run TestPostgresBackend
```

The Postgres integration tests are gated on **both** `ILTER_TEST_POSTGRES=1` and `ILTER_CACHE_PG_DSN` set; they skip cleanly otherwise.

The pgvector HNSW fallback (`ILTER_CACHE_PG_REQUIRE_VECTORSCALE=false`) is **not** integration-tested (only the pgvectorscale path is). Low risk but unreviewed in CI.

---

## Code conventions (non-negotiable)

- **No `panic`.** Always return `error`: `fmt.Errorf("context: %w", err)`. Early-return; avoid `else` after `if err != nil`.
- **No CGo.** The binary must cross-compile (linux/amd64, linux/arm64, darwin). `modernc.org/sqlite`, not `mattn/go-sqlite3`.
- **Constructor injection**: `NewHandler(db, cfg)`. No singletons, no package-level mutable state, no `init()` side effects.
- **`log/slog`** for structured logging — not `fmt.Println`/`log.Printf`.
- `make fix` runs gofumpt + biome + modernize. Run `make check` before committing.
- **Version**: single source of truth is the repo-root `VERSION` file. `make build`/`make dev` sync it into `web/package.json` and inject it via `-ldflags`. Bump only `VERSION`; nothing else hardcodes ilter's version.

---

## Testing

```bash
make test        # unit + integration, -race -count=1 ./...
make check-go    # build + golangci-lint
make check-web   # build dashboard + astro check + biome lint
make test-pg     # Postgres semantic-cache integration (needs services-up-pg + Docker)
make test-e2e    # Playwright dashboard tests — attaches to a `make dev` instance
```

Rules:
- Every new feature needs at least one test.
- Handler tests: `httptest.NewRecorder()` + `chi.NewRouter()`.
- Provider tests: `roundTripFunc` mock transport (see `internal/provider/mock.go`).
- When you change middleware chain order, update `internal/middleware/doc.go`.
- When you change the DB schema, add a migration file.

---

## Documentation map

| Doc | What it covers |
|-----|----------------|
| [`docs/architecture.md`](docs/architecture.md) | Full request flow, service topology, MCP negotiation, provider table |
| [`docs/configuration.md`](docs/configuration.md) | All `ILTER_*` env vars, runtime config sections, model source precedence |
| [`docs/deployment.md`](docs/deployment.md) | Binary / Docker / Docker Compose / Kubernetes, Postgres cache backend, monitoring |
| [`docs/agent-setup.md`](docs/agent-setup.md) | Step-by-step setup written for an AI coding agent |
| [`docs/faq.md`](docs/faq.md) | Design rationale and FAQ |
| [`docs/comparison.md`](docs/comparison.md) | Gateway comparison |
| [`api/openapi.yaml`](api/openapi.yaml) | Every endpoint + schema — read before implementing anything beyond the basic chat-completions swap |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | Contribution process, settled architecture decisions |

---

## Frequently needed operations

**Add a new provider adapter** — see `CONTRIBUTING.md` "Adding a New Provider": create `internal/provider/<name>.go` implementing `provider.Provider`, add `<name>_test.go`, register in `internal/provider/factory.go`, add a model-config entry, document in `docs/architecture.md`.

**Bring up the full local stack** — `docker compose up -d` (ilter + redis + ollama). For the Postgres cache backend: `docker compose --profile pg up -d`.

**Where are provider API keys applied at boot?** — `internal/config/provider_keys.go:ResolveProviderKeys`. Setting `ILTER_PROVIDER_<NAME>_API_KEY` (or `..._API_KEYS`) auto-registers the provider and overrides its DB-stored key.
