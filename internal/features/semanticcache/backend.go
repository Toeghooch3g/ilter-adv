package semanticcache

import (
	"context"
	"time"
)

// CacheBackend is the storage-agnostic interface the SemanticCache delegates
// to. Implementations: RedisBackend (Redis Stack VSS) and PostgresBackend
// (pgvector/pgvectorscale).
type CacheBackend interface {
	// Get returns the closest cached response. threshold is the maximum
	// allowed cosine distance (1 - similarity) when k == 1. When k > 1 the
	// caller asks for the top-k nearest candidates regardless of threshold.
	Get(ctx context.Context, embedding []float32, exactKey string, threshold float64, k int) ([]Hit, error)
	Set(ctx context.Context, embedding []float32, exactKey string, response string, ttl time.Duration) error
	Mode() string // "semantic" | "exact" | "disabled" | "pgvector" | "pgvectorscale"
	Close() error
}

// Hit is a single cache result returned by CacheBackend.Get.
type Hit struct {
	ID       string  // exact_key of the cached response
	Response string  // cached response body
	Score    float64 // cosine distance (0..2) for vector search, 0 for exact match
}

// Reranker re-orders a list of candidate responses by relevance to the query.
// Implementations must be safe to call concurrently; the cache middleware
// invokes it from request goroutines.
type Reranker interface {
	// Rerank returns the candidates re-ordered best-first by relevance to
	// query. The returned slice must contain the same documents as candidates,
	// permuted; implementations may attach a relevance score via Reranked.Text.
	Rerank(ctx context.Context, query string, candidates []Reranked) ([]Reranked, error)
	Model() string
}

// Reranked is a rerank input/output unit: a cached response (Text) whose
// relevance to the query the reranker scores.
type Reranked struct {
	ID      string  // exact_key from the cache
	Text    string  // the cached response body
	Score   float64 // relevance score assigned by the reranker (0..1)
	Present bool    // true when Score is meaningful (the reranker ran)
}

// HitToReranked converts vector hits into rerank candidates, dropping the
// raw distance in favor of a zeroed relevance score.
func HitToReranked(hits []Hit) []Reranked {
	out := make([]Reranked, len(hits))
	for i, h := range hits {
		out[i] = Reranked{ID: h.ID, Text: h.Response}
	}
	return out
}
