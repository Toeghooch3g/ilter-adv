package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"

	"github.com/ilter-ai/ilter/internal/model"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/middleware"
)

type topCachedQuery struct {
	QueryPreview string  `json:"query_preview"`
	Model        string  `json:"model"`
	HitCount     int     `json:"hit_count"`
	LastAccessed string  `json:"last_accessed"`
	AvgLatency   float64 `json:"avg_latency"`
}

type cacheHourlyPoint struct {
	Time   string `json:"time"`
	Hits   int    `json:"hits"`
	Misses int    `json:"misses"`
}

type semanticCacheSummaryResponse struct {
	CacheHits24h        int                `json:"cache_hits_24h"`
	CacheMisses24h      int                `json:"cache_misses_24h"`
	HitRatePct          float64            `json:"hit_rate_pct"`
	CacheSizeEntries    int                `json:"cache_size_entries"`
	CacheSizeMB         float64            `json:"cache_size_mb"`
	AvgLatencySavedMs   float64            `json:"avg_latency_saved_ms"`
	RedisConnected      bool               `json:"redis_connected"`
	RedisError          string             `json:"redis_error,omitempty"`
	Mode                string             `json:"mode"`
	SimilarityThreshold float64            `json:"similarity_threshold"`
	TTLSeconds          int                `json:"ttl_seconds"`
	TopQueries          []topCachedQuery   `json:"top_queries"`
	HourlyData          []cacheHourlyPoint `json:"hourly_data"`
}

// loadCacheHitMissStats queries audit_log for the last 24h's cache
// hit/miss counts and their average latencies.
func loadCacheHitMissStats(db *sql.DB) (cacheHits, cacheMisses int, avgHitLatency, avgMissLatency float64) {
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM audit_log
		WHERE cache_hit = 1 AND timestamp >= datetime('now', '-1 day')
	`).Scan(&cacheHits); err != nil {
		slog.Error("Failed to query cache hits", "error", err)
	}

	if err := db.QueryRow(`
		SELECT COUNT(*) FROM audit_log
		WHERE cache_hit = 0 AND timestamp >= datetime('now', '-1 day')
	`).Scan(&cacheMisses); err != nil {
		slog.Error("Failed to query cache misses", "error", err)
	}

	if err := db.QueryRow(`
		SELECT COALESCE(AVG(latency_ms), 0) FROM audit_log
		WHERE cache_hit = 1 AND timestamp >= datetime('now', '-1 day') AND latency_ms > 0
	`).Scan(&avgHitLatency); err != nil {
		slog.Warn("Failed to query average cache hit latency", "error", err)
	}

	if err := db.QueryRow(`
		SELECT COALESCE(AVG(latency_ms), 0) FROM audit_log
		WHERE cache_hit = 0 AND timestamp >= datetime('now', '-1 day') AND latency_ms > 0
	`).Scan(&avgMissLatency); err != nil {
		slog.Warn("Failed to query average cache miss latency", "error", err)
	}

	return cacheHits, cacheMisses, avgHitLatency, avgMissLatency
}

// resolveCacheSizeEntries counts the semantic-cache keys currently in
// Redis, or 0 if Redis isn't configured or the lookup fails.
func (h *Handler) resolveCacheSizeEntries(ctx context.Context) int {
	if h.cacheClient == nil {
		return 0
	}
	keys, err := h.cacheClient.Keys(ctx, "ilter:cache:*").Result()
	if err != nil {
		slog.Warn("Failed to get cache keys from Redis", "error", err)
		return 0
	}
	return len(keys)
}

// resolveCacheMode determines the semantic cache's effective mode label
// ("disabled", "enabled", or the live engine's own mode) and, when the
// feature is on but unusable, an explanatory redisError. redisConnected
// gates whether the feature can actually run — without Redis the semantic
// cache cannot function even if the flag is on.
func (h *Handler) resolveCacheMode(redisConnected bool) (cacheMode, redisError string) {
	if h.configCache == nil || !middleware.IsEnabled(h.configCache, "semantic_cache") {
		return "disabled", ""
	}
	if !redisConnected {
		if h.cfg != nil && h.cfg.Cache.RedisURL == "" {
			return "disabled", "Redis not available. Set ILTER_REDIS_URL and restart."
		}
		return "disabled", "Redis connection failed. Semantic cache cannot be enabled."
	}
	// Read the real cache engine mode (semantic/exact) from the live
	// middleware to show the user what kind of caching is active.
	if h.semanticCacheMw != nil {
		if engMode := h.semanticCacheMw.Mode(); engMode != "disabled" {
			return engMode, ""
		}
	}
	return "enabled", ""
}

// loadTopCachedQueries returns the 10 most cache-hit prompts in the last
// 24h, or an empty slice on query error.
func loadTopCachedQueries(db *sql.DB) []topCachedQuery {
	topRows, err := db.Query(`
		SELECT
			COALESCE(a.prompt_preview, '') as query_preview,
			a.model,
			COUNT(*) as hit_count,
			MAX(a.timestamp) as last_accessed,
			COALESCE(AVG(a.latency_ms), 0) as avg_latency
		FROM audit_log a
		WHERE a.cache_hit = 1
		  AND a.timestamp >= datetime('now', '-1 day')
		  AND a.prompt_preview != ''
		GROUP BY a.prompt_preview, a.model
		ORDER BY hit_count DESC
		LIMIT 10
		`)
	if err != nil {
		return []topCachedQuery{}
	}
	defer func() { _ = topRows.Close() }()

	topQueries := make([]topCachedQuery, 0, 10)
	for topRows.Next() {
		var q topCachedQuery
		if err := topRows.Scan(&q.QueryPreview, &q.Model, &q.HitCount, &q.LastAccessed, &q.AvgLatency); err == nil {
			q.AvgLatency = math.Round(q.AvgLatency*100) / 100
			topQueries = append(topQueries, q)
		}
	}
	if err := topRows.Err(); err != nil {
		slog.Warn("error iterating top cached queries", "error", err)
	}
	return topQueries
}

// loadCacheHourlyData returns hourly cache hit/miss counts for the last
// 24h, or an empty slice on query error.
func loadCacheHourlyData(db *sql.DB) []cacheHourlyPoint {
	hourRows, err := db.Query(`
		SELECT
			strftime('%Y-%m-%dT%H:00', timestamp) as bucket,
			SUM(CASE WHEN cache_hit = 1 THEN 1 ELSE 0 END) as hits,
			SUM(CASE WHEN cache_hit = 0 THEN 1 ELSE 0 END) as misses
		FROM audit_log
		WHERE timestamp >= datetime('now', '-1 day')
		GROUP BY bucket
		ORDER BY bucket ASC
		`)
	if err != nil {
		return []cacheHourlyPoint{}
	}
	defer func() { _ = hourRows.Close() }()

	hourly := make([]cacheHourlyPoint, 0, 24)
	for hourRows.Next() {
		var p cacheHourlyPoint
		if err := hourRows.Scan(&p.Time, &p.Hits, &p.Misses); err == nil {
			hourly = append(hourly, p)
		}
	}
	if err := hourRows.Err(); err != nil {
		slog.Warn("error iterating hourly cache data", "error", err)
	}
	return hourly
}

// resolveSimilarityThreshold returns the semantic cache's effective
// similarity threshold, mirroring the zero-value fallback
// semanticcache.Cache actually applies at match time
// (features/semanticcache/cache.go) — config plumbing doesn't carry the
// real default through, so this shows the value that's truly in effect.
func (h *Handler) resolveSimilarityThreshold() float64 {
	threshold := h.cfg.Cache.SimilarityThreshold
	if h.configCache != nil {
		if snap := h.configCache.Get(); snap != nil {
			threshold = snap.CacheSimilarityThreshold
		}
	}
	if threshold <= 0 {
		threshold = 0.70
	}
	return threshold
}

func (h *Handler) HandleSemanticCacheSummary(w http.ResponseWriter, r *http.Request) {
	db := h.store.DB
	resp := &semanticCacheSummaryResponse{
		TopQueries: []topCachedQuery{},
		HourlyData: []cacheHourlyPoint{},
	}

	cacheHits, cacheMisses, avgHitLatency, avgMissLatency := loadCacheHitMissStats(db)

	var hitRate float64
	total := cacheHits + cacheMisses
	if total > 0 {
		hitRate = math.Round(float64(cacheHits)/float64(total)*1000) / 10
	}

	avgLatencySaved := avgMissLatency - avgHitLatency
	if avgLatencySaved < 0 {
		avgLatencySaved = 0
	}

	cacheSizeEntries := h.resolveCacheSizeEntries(r.Context())
	cacheSizeMB := math.Round(float64(cacheSizeEntries)*15.0/1024.0*100) / 100

	redisConnected := h.cacheClient != nil
	cacheMode, redisError := h.resolveCacheMode(redisConnected)

	resp.TopQueries = loadTopCachedQueries(db)
	resp.HourlyData = loadCacheHourlyData(db)

	resp.CacheHits24h = cacheHits
	resp.CacheMisses24h = cacheMisses
	resp.HitRatePct = hitRate
	resp.CacheSizeEntries = cacheSizeEntries
	resp.CacheSizeMB = cacheSizeMB
	resp.AvgLatencySavedMs = math.Round(avgLatencySaved*100) / 100
	resp.RedisConnected = redisConnected
	resp.RedisError = redisError
	resp.Mode = cacheMode

	resp.SimilarityThreshold = h.resolveSimilarityThreshold()
	resp.TTLSeconds = int(h.cfg.Cache.TTL.Seconds())

	model.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) HandleCacheModeToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"` // "enabled" or "disabled"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Mode != "enabled" && req.Mode != "disabled" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request", "mode must be 'enabled' or 'disabled'")
		return
	}

	enabled := req.Mode == "enabled"

	// Don't allow enabling when Redis is not available — the cache
	// cannot function without it.
	if enabled && h.cacheClient == nil {
		var msg string
		if h.cfg != nil && h.cfg.Cache.RedisURL == "" {
			msg = "Redis not available. Set ILTER_REDIS_URL and restart."
		} else {
			msg = "Redis connection failed. Check Redis server and restart."
		}
		model.WriteJSONError(w, http.StatusBadRequest, "redis_unavailable", msg)
		return
	}
	// picks it up into snap.CacheEnabled (the same key middleware.IsEnabled and the
	// Feature Flags page read), and trigger config cache refresh.
	value := "false"
	if enabled {
		value = "true"
	}
	_, err := h.store.DB.Exec(
		`INSERT INTO runtime_config (section, key, value, updated_at, version)
		 VALUES ('feature', 'semantic_cache', ?, datetime('now'), 1)
		 ON CONFLICT(section, key) DO UPDATE SET value = excluded.value, version = version + 1, updated_at = datetime('now')`,
		value,
	)
	if err != nil {
		slog.Error("Failed to persist cache feature flag", "enabled", enabled, "error", err)
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to save cache feature flag")
		return
	}

	if h.configCache != nil {
		stores := &config.RuntimeStores{RuntimeConfig: h.store}
		if err := h.configCache.Refresh(r.Context(), stores); err != nil {
			slog.Warn("config cache refresh after cache toggle failed", "error", err)
		}
	}

	slog.Info("Cache toggled via feature flag", "enabled", enabled)

	model.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "mode": req.Mode})
}
