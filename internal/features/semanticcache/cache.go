package semanticcache

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ilter-ai/ilter/internal/config"
)

type CacheMode string

const (
	CacheModeSemantic CacheMode = "semantic" // VSS with embedder
	CacheModeExact    CacheMode = "exact"    // SHA256 exact match
	CacheModeDisabled CacheMode = "disabled" // cache not available
)

// SemanticCache is the cache facade. GetFull/SetFull operate on whichever
// CacheBackend was selected at construction (Redis or Postgres), calling the
// embedder and optional reranker as configured.
type SemanticCache struct {
	cfg      config.CacheConfig
	embedder Embedder
	reranker Reranker
	backend  CacheBackend
}

// New selects the storage backend from cfg.Type and returns a SemanticCache.
// signature: New(cfg, embedder, reranker, redisClient, pgDSN).
func New(cfg config.CacheConfig, embedder Embedder, reranker Reranker, redisClient *redis.Client, pgDSN string) *SemanticCache {
	sc := &SemanticCache{
		cfg:      cfg,
		embedder: embedder,
		reranker: reranker,
	}
	if !cfg.Enabled {
		return sc
	}

	switch cfg.Type {
	case "disabled":
		// Explicitly disabled: no backend, no external DB, no embedder probe.
		// Mode() returns CacheModeDisabled and GetFull/SetFull no-op.
		return sc
	case "postgres":
		b, err := NewPostgresBackend(context.Background(), cfg, embedder)
		if err != nil {
			slog.Error("postgres semantic cache backend init failed; cache disabled", "error", err)
			return sc
		}
		sc.backend = b
	default: // "" or "redis"
		if redisClient != nil {
			b := NewRedisBackend(redisClient, embedder)
			b.maxEntries = cfg.MaxEntries
			sc.backend = b
			if embedder != nil {
				b.initIndex(embedder.Dim())
			}
		}
	}
	return sc
}

func (c *SemanticCache) Mode() CacheMode {
	if !c.cfg.Enabled || c.backend == nil {
		return CacheModeDisabled
	}
	if c.embedder != nil && c.backend.Mode() != "exact" {
		return CacheModeSemantic
	}
	return CacheModeExact
}

// GetFull returns the cached response for embedText, embedding it first and
// (optionally) re-ranking the top-K vector candidates. Falls back to exact
// match within the backend when no semantic hit qualifies.
func (c *SemanticCache) GetFull(ctx context.Context, embedText string, exactKey string) (response string, score float64, found bool) {
	if c.backend == nil {
		return "", 0, false
	}

	var emb []float32
	if c.embedder != nil {
		var err error
		emb, err = c.embedder.Embed(ctx, embedText)
		if err != nil {
			slog.Warn("Embedding failed, falling back to exact match", "error", err)
			emb = nil
		}
	}

	threshold := c.cfg.SimilarityThreshold
	if threshold == 0 {
		threshold = 0.70
	}

	// Rerank path: pull top-K regardless of threshold, rerank, and accept the
	// best candidate only if its rerank relevance clears the threshold.
	if c.reranker != nil && emb != nil {
		k := c.cfg.RerankTopK
		if k <= 0 {
			k = 10
		}
		hits, err := c.backend.Get(ctx, emb, exactKey, 0, k)
		if err == nil && len(hits) > 0 {
			reranked, rerr := c.reranker.Rerank(ctx, embedText, HitToReranked(hits))
			if rerr == nil && len(reranked) > 0 && reranked[0].Present && reranked[0].Score >= threshold {
				slog.Debug("semantic cache hit (rerank)", "score", reranked[0].Score, "threshold", threshold)
				return reranked[0].Text, reranked[0].Score, true
			}
			if rerr != nil {
				slog.Warn("Rerank failed, falling back to KNN top-1", "error", rerr)
			}
		}
	}

	hits, err := c.backend.Get(ctx, emb, exactKey, threshold, 1)
	if err == nil && len(hits) > 0 {
		slog.Debug("semantic cache hit", "score", hits[0].Score, "threshold", threshold)
		return hits[0].Response, hits[0].Score, true
	}
	return "", 0, false
}

// SetFull embeds embedText (when an embedder is present) and stores the
// response in the backend under exactKey. The exact-match entry is always
// stored; the vector entry only when an embedder produced an embedding.
func (c *SemanticCache) SetFull(ctx context.Context, embedText string, exactKey string, response string) error {
	if c.backend == nil {
		return nil
	}

	var emb []float32
	if c.embedder != nil {
		var err error
		emb, err = c.embedder.Embed(ctx, embedText)
		if err != nil {
			slog.Warn("Semantic cache skip: embedding failed", "error", err)
			emb = nil
		}
	}

	ttl := c.cfg.TTL
	if ttl <= 0 {
		ttl = 1 * time.Hour
	}
	return c.backend.Set(ctx, emb, exactKey, response, ttl)
}
