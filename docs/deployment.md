# Ilter Advanced Deployment Guide

> Production deployment options for Ilter Advanced AI Gateway: single binary, Docker, Docker Compose, and Kubernetes.

---

## Prerequisites

- **Ilter Advanced binary** — Build from source (`make build`) or download from GitHub Releases (~35-40MB, unstripped-UPX plain binary; the <20MB figure below is the Docker image only)
- **Configuration** — Via `ILTER_*` environment variables (see [`configuration.md`](configuration.md))
- **Redis** (optional) — Required for the default `redis` semantic cache backend and distributed rate limiting (fails open if unavailable)
- **Ollama** (optional) — Local semantic cache embeddings as the legacy fallback (fails open if unavailable)
- **PostgreSQL** (optional) — Required for the `postgres` semantic cache backend (`ILTER_CACHE_TYPE=postgres`), with `pgvector` + optional `pgvectorscale`. Use the bundled Timescale Docker image (see below) — not Homebrew Postgres. Fails open to exact-match if unreachable.

---

## Network Ports Overview

| Port | Service | Protocol | Description |
|------|---------|----------|-------------|
| **8181** | Proxy API | HTTP/1.1, SSE | Main LLM Gateway (`/v1/chat/completions`, `/v1/messages`, `/v1/completions`, `/v1/embeddings`, `/v1/rerank`, `/v1/models`, `/admin/*`, `/mcp`, OAuth PKCE) |
| **9191** | Dashboard | HTTP/1.1, JSON | Embedded Web UI + Admin REST API (`/api/*`) |
| **9192** | Metrics | HTTP/1.1 | OpenTelemetry Prometheus scrape endpoint (`/metrics`) |

---

## Local / Development

### Quick Start

```bash
# Just run it — compiled-in defaults work out of the box
./ilter serve
```

### First-Time Setup

```bash
# Interactive wizard — configure providers, routing, feature flags
./ilter init

# Or seed demo data for dashboard development
./ilter init --demo

# Then start
./ilter serve
```

### Hot Reload

```bash
make dev
```

`make dev` runs in a dedicated terminal tab: Air (Go hot reload backend) and Vite (Astro frontend HMR) run concurrently.

---

## Docker

### Build

```bash
# Build using the root Dockerfile
docker build -t ilter .
```

### Multi-Stage Container Architecture

1. **Stage 1: Web Builder (`oven/bun:1-alpine`)** — Installs dependencies and builds Astro + React SPA into `web/dist`.
2. **Stage 2: Go Builder (`golang:1.26-alpine`)** — Compiles statically linked Go binary (`CGO_ENABLED=0`) and compresses with UPX (`upx --best --lzma`).
3. **Stage 3: Base Runtime (`scratch`)** — Minimal empty image containing only the compiled binary, CA certificates (`/etc/ssl/certs/ca-certificates.crt`), and timezone database (`/usr/share/zoneinfo`).

**Final image size: <20MB**.

### Run Container

```bash
docker run -d \
  --name ilter \
  -p 8181:8181 \
  -p 9191:9191 \
  -p 9192:9192 \
  -v $(pwd)/data:/app/data \
  ilter
```

---

## Docker Compose (Full Local Stack)

The `docker-compose.yaml` in project root spins up Ilter Advanced alongside optional services:

```bash
docker compose up -d
```

### Services

| Service | Image | Ports | Purpose | Optional? |
|---------|-------|-------|---------|-----------|
| **ilter** | Built from `./Dockerfile` | 8181, 9191, 9192 | Core Gateway, Dashboard, Metrics | Required |
| **redis** | `redis/redis-stack:latest` | 6379, 8001 | Semantic cache VSS + rate limiting | Optional (fails open) |
| **ollama** | `ollama/ollama:latest` | 11434 | Local vector embeddings (`nomic-embed-text`) | Optional (fails open) |
| **postgres** | `timescale/timescaledb-ha:pg16` | 5432 | Postgres semantic-cache backend (`pgvector` + `pgvectorscale`) | Optional (profile `pg`) |
| **ilter-pg** | Built from `./Dockerfile` | 8181, 9191, 9192 | Ilter Advanced wired to the Postgres cache backend | Optional (profile `pg`) |

### Semantic Cache Backend Selection

The semantic cache supports two storage backends, chosen by `ILTER_CACHE_TYPE`:

- **`redis`** (default): Redis Stack VSS. Requires `ILTER_REDIS_URL`. Vector dimension comes from the embedder (legacy Ollama is 768D).
- **`postgres`**: PostgreSQL with the `pgvector` (and optionally `pgvectorscale`) extension. Requires `ILTER_CACHE_PG_DSN`. Each embedding model gets its own `ilter_semantic_cache_<sanitized_model>` table, so switching models preserves cached data.

The embedding model is provider-selectable with `ILTER_CACHE_EMBEDDING_MODEL=provider:model` (e.g. `openai:text-embedding-3-small`, `voyage:voyage-3`, `ollama:nomic-embed-text`). Reranking is optional via `ILTER_CACHE_RERANK_MODEL=provider:model` + `ILTER_CACHE_RERANK_TOPK`.

#### Postgres backend (`docker compose --profile pg up -d`)

```bash
docker compose --profile pg up -d     # brings up ollama, postgres, and ilter-pg
```

The compose stack includes two extra services behind the `pg` profile:

- **`postgres`** — `timescale/timescaledb-ha:pg16` with `POSTGRES_USER=ilter`, `POSTGRES_PASSWORD=ilter`, `POSTGRES_DB=ilter_cache`. It mounts `deploy/initdb/` into `/docker-entrypoint-initdb.d`, which runs `CREATE EXTENSION IF NOT EXISTS vector; CREATE EXTENSION IF NOT EXISTS vectorscale;` on first boot. Internet access is required on first boot to download the Timescale image; run offline is not supported.
- **`ilter-pg`** — Ilter Advanced wired to the Postgres cache backend:
  ```yaml
  environment:
    ILTER_CACHE_TYPE=postgres
    ILTER_CACHE_PG_DSN=postgres://ilter:ilter@postgres:5432/ilter_cache?sslmode=disable
    ILTER_CACHE_EMBEDDING_MODEL=openai:text-embedding-3-small
  ```
  Set `ILTER_PROVIDER_OPENAI_API_KEY` (or another provider's key) in your shell before `docker compose --profile pg up -d` so the gateway has a valid embedding provider.

> **Important:** the `timescale/timescaledb-ha` image preloads only `timescaledb` and `timescaledb_toolkit`. It does **not** auto-create `pgvector`/`pgvectorscale`; they are created by `deploy/initdb/01-extensions.sql`. If you skip that mount, the Postgres cache backend fails at startup because `vector` is missing. Do **not** use Homebrew Postgres — use the Timescale Docker container.

---

## Providers & Model Selection in Production

### Registering providers

Providers can be added through any of three equivalent entry points. All target the same runtime `provider` section and take effect **immediately** (hot reload — no restart):

1. **Web Dashboard → Providers → Add Provider** — name, type, base URL, optional API key.
2. **`ilter init`** — choose **Custom (OpenAI-compatible)** to register a provider with a custom base URL (stored as an `openai`-type provider).
3. **Admin API** — `POST /api/providers/create`:
   ```json
   POST /api/providers/create
   {
     "name": "my-self-hosted",
     "type": "openai",
     "base_url": "https://llm.example.com/v1",
     "api_key": "sk-..."
   }
   ```

A *custom* provider is simply an `openai`-type registration with a user-supplied `base_url` — it reuses the full OpenAI provider stack (chat, streaming, embeddings, rerank, model discovery) against your endpoint. You can register as many as you like; each is keyed by unique name, so two custom providers never collide even when they share a type and expose the same model IDs.

**Boot-time env seeding.** Setting `ILTER_PROVIDER_<NAME>_API_KEY` (or the plural `ILTER_PROVIDER_<NAME>_API_KEYS`, comma/newline-separated) is enough to enable that provider at boot — no `ilter init` needed. Supported names (matching the built-in provider types) include `OPENAI`, `ANTHROPIC`, `GEMINI`, `DEEPSEEK`, `OPENROUTER`, `OLLAMA`, `QWEN`, `OPENCODE_GO`, `OPENCODE_ZEN`, and `MOCK`. See [`configuration.md`](configuration.md) for the full provider env-var table.

### How model selection works

For each provider, the available models (pricing, context limits, tier, capabilities) resolve from three sources, in ascending priority:

1. **Manual model overrides** (lowest priority): a JSON list `{id, display_name, tier, cost_per_input_token, cost_per_output_token, max_context_tokens, max_output_tokens, capabilities}` keyed by provider in the `model_overrides` runtime section. Edit only through a JSON file: Dashboard → provider → **Models JSON** download/upload, or `GET`/`PUT /api/providers/{name}/models-overrides`. A file path on the provider registration (`ModelOverridesFile`) can seed the list at boot when no runtime entry exists — useful for keeping custom provider metadata in version control.
2. **Provider discovery** (highest priority): the provider's `/v1/models` endpoint. For each model ID, any detail the endpoint reports wins per-field over the override; unreported details fall back to the override, then to naming-convention heuristics. Endpoint-only models are added; override-only models are kept.

### Semantic cache embedding & rerank providers

The semantic cache needs an embedding provider, and may use a reranker for two-stage retrieval. Both are selected per-provider as `provider:model` strings:

| Env var | Example | Behavior |
|---------|---------|----------|
| `ILTER_CACHE_EMBEDDING_MODEL` | `openai:text-embedding-3-small` | Embeddings for cache vectors. Empty falls back to the legacy Ollama embedder (768D). |
| `ILTER_CACHE_RERANK_MODEL` | `cohere:rerank-english-v3.0` | Optional reranker. Empty disables rerank (KNN top-1). |

The cache probes the chosen provider for a real embedding at startup to learn the vector dimension, persists it in `runtime_config` (`semantic_cache` → `embedding_dim`), and reuses it on restart when the model is unchanged (skipping the probe). On failure it degrades gracefully — embedder failure → SHA-256 exact-match only; reranker failure → KNN top-1 — rather than failing boot.

When you switch `ILTER_CACHE_EMBEDDING_MODEL`, Postgres allocates a fresh per-model table (preserving the old model's cache); Redis uses the new dimension for new writes. See [`configuration.md`](configuration.md#semantic-cache-backends) for the full backend guide.



## Kubernetes Deployment

Since Ilter Advanced is a single stateless binary with embedded assets, deployment to Kubernetes is straightforward using standard Kubernetes manifests.

### Kubernetes Deployment & Service Manifest (`ilter-k8s.yaml`)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ilter-gateway
  labels:
    app: ilter
spec:
  replicas: 2
  selector:
    matchLabels:
      app: ilter
  template:
    metadata:
      labels:
        app: ilter
    spec:
      containers:
      - name: ilter
        image: ilter:latest
        imagePullPolicy: IfNotPresent
        env:
        - name: ILTER_SERVER_PORT
          value: "8181"
        - name: ILTER_DASHBOARD_PORT
          value: "9191"
        - name: ILTER_METRICS_LISTEN_ADDR
          value: ":9192"
        - name: ILTER_STORAGE_PATH
          value: "/app/data/ilter.db"
        ports:
        - containerPort: 8181
          name: proxy
        - containerPort: 9191
          name: dashboard
        - containerPort: 9192
          name: metrics
        livenessProbe:
          httpGet:
            path: /admin/health
            port: 8181
          initialDelaySeconds: 5
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /admin/health
            port: 8181
          initialDelaySeconds: 2
          periodSeconds: 5
        volumeMounts:
        - name: data-volume
          mountPath: /app/data
      volumes:
      - name: data-volume
        emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: ilter-service
spec:
  type: ClusterIP
  selector:
    app: ilter
  ports:
  - port: 8181
    targetPort: 8181
    name: proxy
  - port: 9191
    targetPort: 9191
    name: dashboard
  - port: 9192
    targetPort: 9192
    name: metrics
```

Deploy with:

```bash
kubectl apply -f ilter-k8s.yaml
```

---

## Monitoring & Prometheus Configuration

Ilter Advanced exposes OpenTelemetry metrics in Prometheus format at `http://<host>:9192/metrics`.

### Prometheus Scrape Configuration

Add the following job to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: 'ilter'
    scrape_interval: 15s
    static_configs:
      - targets: ['ilter-service:9192']
```

Key operational metrics exposed:
- `ilter_requests_total` — Request counters by provider, model, and status
- `ilter_request_duration_seconds` — Latency distribution histograms
- `ilter_token_usage_total` — Input and output tokens consumed
- `ilter_guardrail_violations_total` — Violations triggered by type and severity
- `ilter_mcp_tool_calls_total` — Tool call counts and latency

---

## Troubleshooting & Operations

### Debugging Empty `scratch` Base Containers

Because the final runtime container is built on an empty `scratch` image, shell utilities like `sh`, `bash`, `curl`, or `ls` are not present inside the container.

- **Kubernetes**: Use [Ephemeral Debug Containers](https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers/):
  ```bash
  kubectl debug -it pod/ilter-gateway-xxxx --image=busybox --target=ilter
  ```
- **Docker**: Inspect logs or copy binary/data to host:
  ```bash
  docker cp ilter:/app/data/ilter.db ./ilter.db
  ```

### Fail-Open Degradation

Ilter Advanced is designed for high availability. If an optional backend becomes unreachable:
- **Redis offline**: Rate limiting and semantic caching degrade gracefully (fail open). Requests pass through to upstream providers without caching or distributed rate enforcement.
- **Ollama offline**: Semantic cache falls back to exact SHA256 prompt hash matching. Proxy requests continue normally.
- **Postgres offline** (Postgres cache backend): Semantic cache falls back to exact SHA256 prompt hash matching. Proxy requests continue normally. The gateway needs `pgvector` (`CREATE EXTENSION vector`) present at startup — if the `vectorscale` extension is missing, set `ILTER_CACHE_PG_REQUIRE_VECTORSCALE=false` to use a plain `pgvector` HNSW index.

### Semantic Cache Troubleshooting

- **Embedding provider key invalid / 401**: the cache probes the embedding provider at startup and on failure logs it and degrades to exact-match — it does not block boot. Verify the key set by `ILTER_CACHE_EMBEDDING_MODEL`'s `provider:` prefix.
- **Dimension mismatch after switching models**: each embedding model gets its own Postgres table (`ilter_semantic_cache_<sanitized_model>`) and its own persisted `embedding_dim` in `runtime_config`; old data is preserved, not corrupted. To reset, delete the table and the `semantic_cache` runtime_config entry.
- **`vector` extension missing**: the Postgres backend fails at startup. Confirm the `deploy/initdb/01-extensions.sql` mount ran on first boot, or run `CREATE EXTENSION IF NOT EXISTS vector;` manually. Do not install Postgres via Homebrew — use the Timescale container.
