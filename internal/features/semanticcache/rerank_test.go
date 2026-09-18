package semanticcache

import (
	"context"
	"testing"
	"time"

	"github.com/ilter-ai/ilter/internal/config"
)

// fakeBackend returns canned hits; used to exercise GetFull rerank logic.
type fakeBackend struct {
	hits []Hit
	err  error
}

func (f *fakeBackend) Get(context.Context, []float32, string, float64, int) ([]Hit, error) {
	return f.hits, f.err
}

func (f *fakeBackend) Set(context.Context, []float32, string, string, time.Duration) error {
	return nil
}
func (f *fakeBackend) Mode() string { return "semantic" }
func (f *fakeBackend) Close() error { return nil }

// fakeReranker is a deterministic Reranker.
type fakeReranker struct {
	rerank func(ctx context.Context, query string, candidates []Reranked) ([]Reranked, error)
}

func (r *fakeReranker) Rerank(ctx context.Context, query string, candidates []Reranked) ([]Reranked, error) {
	if r.rerank == nil {
		return candidates, nil
	}
	return r.rerank(ctx, query, candidates)
}
func (r *fakeReranker) Model() string { return "fake:rerank" }

func newRerankCache(cfg config.CacheConfig, backend CacheBackend, reranker Reranker) *SemanticCache {
	if cfg.SimilarityThreshold == 0 {
		cfg.SimilarityThreshold = 0.5
	}
	if cfg.RerankTopK == 0 {
		cfg.RerankTopK = 10
	}
	return &SemanticCache{
		cfg:      cfg,
		embedder: &fakeEmbedder{dim: 4},
		reranker: reranker,
		backend:  backend,
	}
}

func TestReranker_TopOneSelected(t *testing.T) {
	backend := &fakeBackend{hits: []Hit{
		{ID: "a", Response: "response-a", Score: 0.1},
		{ID: "b", Response: "response-b", Score: 0.2},
		{ID: "c", Response: "response-c", Score: 0.3},
	}}
	reranker := &fakeReranker{
		rerank: func(_ context.Context, _ string, candidates []Reranked) ([]Reranked, error) {
			// Re-order so "b" wins.
			var b Reranked
			var rest []Reranked
			for _, c := range candidates {
				if c.ID == "b" {
					b = c
				} else {
					rest = append(rest, c)
				}
			}
			b.Score = 0.95
			b.Present = true
			out := append([]Reranked{b}, rest...)
			for i := range out {
				out[i].Present = true
				if out[i].Score == 0 {
					out[i].Score = 0.5
				}
			}
			return out, nil
		},
	}
	sc := newRerankCache(config.CacheConfig{SimilarityThreshold: 0.8}, backend, reranker)

	resp, score, found := sc.GetFull(context.Background(), "query", "exactkey")
	if !found {
		t.Fatal("expected found=true")
	}
	if resp != "response-b" {
		t.Errorf("resp = %q, want response-b", resp)
	}
	if score != 0.95 {
		t.Errorf("score = %v, want 0.95 (rerank score, not cosine distance)", score)
	}
}

func TestReranker_EmptyCandidates(t *testing.T) {
	backend := &fakeBackend{hits: nil}
	reranker := &fakeReranker{}
	sc := newRerankCache(config.CacheConfig{}, backend, reranker)

	resp, _, found := sc.GetFull(context.Background(), "query", "exactkey")
	if found {
		t.Errorf("expected found=false, got found with resp %q", resp)
	}
	if resp != "" {
		t.Errorf("expected empty resp, got %q", resp)
	}
}

func TestReranker_RerankErrorFallsBackToTop1(t *testing.T) {
	backend := &fakeBackend{hits: []Hit{{ID: "a", Response: "top1", Score: 0.1}}}
	reranker := &fakeReranker{
		rerank: func(context.Context, string, []Reranked) ([]Reranked, error) {
			return nil, errBoom
		},
	}
	sc := newRerankCache(config.CacheConfig{SimilarityThreshold: 0.8}, backend, reranker)

	resp, _, found := sc.GetFull(context.Background(), "query", "exactkey")
	if !found {
		t.Fatal("expected found=true via KNN top-1 fallback")
	}
	if resp != "top1" {
		t.Errorf("resp = %q, want top1", resp)
	}
}

var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "rerank boom" }
