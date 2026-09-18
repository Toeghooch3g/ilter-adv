package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sony/gobreaker/v2"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db"
	"github.com/ilter-ai/ilter/internal/features/circuitbreaker"
	"github.com/ilter-ai/ilter/internal/features/loopdetect"
	"github.com/ilter-ai/ilter/internal/features/pii"
	"github.com/ilter-ai/ilter/internal/features/semanticcache"
	"github.com/ilter-ai/ilter/internal/features/smartrouter"
	iltermiddleware "github.com/ilter-ai/ilter/internal/middleware"
	"github.com/ilter-ai/ilter/internal/platform/logging"
	"github.com/ilter-ai/ilter/internal/provider"
)

type redisLogger struct{}

func (redisLogger) Printf(ctx context.Context, format string, v ...any) {
	slog.DebugContext(ctx, fmt.Sprintf(format, v...), "component", "redis")
}

func (a *App) initStore() error {
	cfg := a.cfg
	if cfg.Storage.Type == "sqlite" && cfg.Storage.SqlitePath != "" && !filepath.IsAbs(cfg.Storage.SqlitePath) {
		if abs, err := filepath.Abs(cfg.Storage.SqlitePath); err == nil {
			cfg.Storage.SqlitePath = abs
		}
	}
	if cfg.Storage.Type == "sqlite" && cfg.Storage.SqlitePath != ":memory:" {
		if _, err := os.Stat(cfg.Storage.SqlitePath); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("failed to access database at %q: %w", cfg.Storage.SqlitePath, err)
			}
			// No DB yet. That's fine if the operator bootstrapped via env
			// (admin break-glass key + at least one provider key) — the
			// store below creates the file and runs migrations on its own.
			// Otherwise there's no way to authenticate or route requests,
			// so fail fast instead of serving a useless empty gateway.
			if !config.AdminKeyEnv.WasSet() || !config.AnyProviderKeyEnvSet() {
				return fmt.Errorf(
					"database not found at %q: run 'ilter init' first, or set %s and at least one provider key (e.g. %s) to boot from env",
					cfg.Storage.SqlitePath, "ILTER_ADMIN_API_KEY", config.ProviderKeyEnv("opencode_go"),
				)
			}
		}
	}
	store, err := db.NewSQLiteStore(cfg.Storage)
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	db.InitConfigResolvers(store)

	cfg.Providers = loadProvidersFromDB(store)
	config.EnrichConfig(cfg)
	config.ResolveProviderKeys(cfg, slog.Default())

	if err := pii.LoadPatternsFromDB(store.DB); err != nil {
		slog.Warn("failed to load PII patterns from DB", "error", err)
	}

	a.store = store
	return nil
}

func loadProvidersFromDB(store *db.SQLiteStore) []config.ProviderConfig {
	providerEntries, err := store.GetBySection(context.Background(), "provider")
	if err != nil {
		slog.Warn("no providers in runtime_config (run 'ilter init' first)", "error", err)
		return nil
	}
	overrideEntries, errOver := store.GetBySection(context.Background(), "model_overrides")
	if errOver != nil {
		slog.Warn("failed to read model_overrides from runtime_config", "error", errOver)
	}
	// Delegate to the shared parser so the boot provider set stays identical to
	// the config-cache snapshot (which the runtime registry hot-reloads from).
	return config.ProviderConfigsFromSections(providerEntries, overrideEntries)
}

func (a *App) setupLogging() {
	cfg := a.cfg
	level := slog.LevelInfo
	switch cfg.Logging.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.Logging.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = logging.NewPrettyHandler(os.Stdout, *opts)
	}
	slog.SetDefault(slog.New(handler))
	redis.SetLogger(redisLogger{})

	slog.Info("starting proxy", "addr", fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port), "dashboard_port", cfg.Dashboard.Port)
}

func (a *App) initRedis() *circuitbreaker.RedisBreaker {
	cfg := a.cfg
	redisURL := ""
	switch {
	case cfg.RateLimit.Enabled && cfg.RateLimit.RedisURL != "":
		redisURL = cfg.RateLimit.RedisURL
	case cfg.Cache.Enabled && cfg.Cache.RedisURL != "":
		redisURL = cfg.Cache.RedisURL
	case cfg.Budget.Enabled && cfg.RateLimit.RedisURL != "":
		redisURL = cfg.RateLimit.RedisURL
	}

	if redisURL == "" {
		return nil
	}

	redisOpts, errParse := redis.ParseURL(redisURL)
	if errParse != nil {
		slog.Warn("Failed to parse Redis URL, features will degrade gracefully", "error", errParse)
		return nil
	}

	client := redis.NewClient(redisOpts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if errPing := client.Ping(ctx).Err(); errPing != nil {
		slog.Warn("Failed to connect to Redis, features will degrade gracefully", "error", errPing)
		return nil
	}
	return circuitbreaker.NewRedisBreaker(client, 200*time.Millisecond, gobreaker.Settings{})
}

func (a *App) initMiddleware(rg, cacheGuard *circuitbreaker.RedisBreaker) {
	cfg := a.cfg
	store := a.store

	a.authMiddleware = iltermiddleware.NewAuthMiddleware(cfg.Auth, store).WithMCPEndpoint(cfg.MCP.Endpoint)

	rlMw, err := iltermiddleware.NewRateLimitMiddleware(&cfg.RateLimit, rg, a.cfgCache)
	if err != nil {
		slog.Error("Failed to initialize rate limiter middleware", "error", err)
	}
	a.rateLimitMiddleware = rlMw

	a.budgetMiddleware = iltermiddleware.NewBudgetMiddleware(cfg.Budget, rg, store, a.cfgCache)

	embedder, reranker := a.resolveCacheEmbedding(cfg.Cache)
	a.semanticCacheMiddleware = iltermiddleware.NewSemanticCacheMiddleware(cfg.Cache, cacheGuard, a.cfgCache, embedder, reranker)

	a.auditLoggerMiddleware = iltermiddleware.NewAuditLoggerMiddleware(store)
}

// resolveCacheEmbedding resolves the semantic-cache embedder (probing/persisting
// its dimension) and the optional reranker from cfg. Any resolution failure is
// logged and degrades the cache to exact-only / top-1 mode rather than failing
// the boot.
func (a *App) resolveCacheEmbedding(cfg config.CacheConfig) (semanticcache.Embedder, semanticcache.Reranker) {
	if cfg.Type == "disabled" {
		// Fully disabled caching: no embedder, no dimension probe, and no
		// requirement for Redis, Postgres, or any embedding provider. The
		// middleware short-circuits on the disabled backend, so no embedding
		// is ever attempted.
		return nil, nil
	}
	embedder, err := semanticcache.ResolveEmbedder(a.reg, cfg)
	if err != nil {
		slog.Error("semantic cache: failed to resolve embedder; cache disabled", "error", err)
		return nil, nil
	}
	if embedder != nil {
		a.ensureCacheDim(cfg, embedder)
	}

	var reranker semanticcache.Reranker
	if cfg.RerankModel != "" {
		providerName, model, ok := strings.Cut(cfg.RerankModel, ":")
		if !ok || providerName == "" || model == "" {
			slog.Error("semantic cache: ILTER_CACHE_RERANK_MODEL must be provider:model; rerank disabled", "value", cfg.RerankModel)
		} else {
			r, err := semanticcache.NewProviderReranker(a.reg, providerName, model)
			if err != nil {
				slog.Error("semantic cache: failed to resolve reranker; top-1 mode", "error", err)
			} else {
				reranker = r
			}
		}
	}
	return embedder, reranker
}

// ensureCacheDim probes the embedder's dimension (or reuses a persisted value)
// and stores it in runtime_config so restarts with the same model skip the probe.
func (a *App) ensureCacheDim(cfg config.CacheConfig, embedder semanticcache.Embedder) {
	const section = "semantic_cache"
	const dimKey = "embedding_dim"
	const modelKey = "embedding_model"

	// Reuse a persisted dim only when the model that produced it matches.
	storedModel, _ := a.store.GetRuntimeConfigEntry(context.Background(), section, modelKey)
	if storedModel != nil && storedModel.Value == cfg.EmbeddingModel {
		if stored, err := a.store.GetRuntimeConfigEntry(context.Background(), section, dimKey); err == nil && stored.Value != "" {
			if dim, err := strconv.Atoi(stored.Value); err == nil && dim > 0 {
				if pe, ok := embedder.(*semanticcache.ProviderEmbedder); ok {
					pe.SetDim(dim)
					slog.Info("semantic cache: embedding_dim from runtime_config (skipped probe)", "dim", dim, "model", cfg.EmbeddingModel)
					return
				}
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	dim, err := semanticcache.ProbeDim(ctx, embedder)
	if err != nil {
		slog.Error("semantic cache: dim probe failed; cache will operate in exact/top-1 mode", "error", err, "model", cfg.EmbeddingModel)
		return
	}
	if err := a.store.UpsertRuntimeConfig(context.Background(), section, dimKey, fmt.Sprintf("%d", dim), "boot"); err != nil {
		slog.Warn("semantic cache: failed to persist embedding_dim", "error", err)
	} else {
		_ = a.store.UpsertRuntimeConfig(context.Background(), section, modelKey, cfg.EmbeddingModel, "boot")
	}
	slog.Info("semantic cache: probed embedding_dim", "dim", dim, "model", cfg.EmbeddingModel)
}

func initLoadBalancer(cfg *config.Config, reg *provider.Registry, store *db.SQLiteStore, cfgCache *config.Cache) (*smartrouter.LoadBalancer, *loopdetect.Detector, error) {
	lb, err := smartrouter.NewLoadBalancer(cfg, reg, cfgCache)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize load balancer: %w", err)
	}
	if errRoutes := lb.LoadRoutesFromDB(providerModelEntries(store)); errRoutes != nil {
		return nil, nil, fmt.Errorf("failed to load routes from DB: %w", errRoutes)
	}
	slog.Info("routes loaded")
	if inactiveModels, errDb := store.GetInactiveModels(context.Background()); errDb == nil {
		lb.SetInactiveModels(inactiveModels)
	}
	loopSettings := config.LoopSettingsWithDefaults(cfg.CostGuard.LoopSettings)
	if overrides, errDb := store.GetBySection(context.Background(), "loop_settings"); errDb == nil {
		loopSettings = config.ApplyLoopSettingsOverrides(loopSettings, overrides)
	}
	cfg.CostGuard.LoopSettings = loopSettings
	loopDetector := loopdetect.NewDetector(loopSettings)
	return lb, loopDetector, nil
}

// providerModelEntries returns a loader of provider_models rows mapped to
// smartrouter.ProviderModelEntry, shared by boot route loading
// (initLoadBalancer) and the runtime route rebuild (syncModelsToDB →
// RebuildProviders) so both build routes from the same persisted source
// with full pricing fields.
func providerModelEntries(store *db.SQLiteStore) func(provider string) ([]smartrouter.ProviderModelEntry, error) {
	return func(providerName string) ([]smartrouter.ProviderModelEntry, error) {
		models, errQ := store.GetProviderModels(context.Background(), providerName)
		if errQ != nil {
			return nil, errQ
		}
		entries := make([]smartrouter.ProviderModelEntry, len(models))
		for i, m := range models {
			entries[i] = smartrouter.ProviderModelEntry{
				Name:           m.Model,
				Active:         m.Active,
				Category:       m.Category,
				CostIn:         m.CostIn,
				CostOut:        m.CostOut,
				CostCacheRead:  m.CostCacheRead,
				CostCacheWrite: m.CostCacheWrite,
			}
		}
		return entries, nil
	}
}
