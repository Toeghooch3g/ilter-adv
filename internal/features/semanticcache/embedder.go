package semanticcache

import (
	"context"
	"fmt"
	"strings"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/provider"
)

// Embedder produces a vector for a text input. Dim() returns the vector
// length; it MUST be stable across calls for the lifetime of the process so
// the cache DDL is correct.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dim() int
	Model() string // human-readable id, used in cache DDL comment + logs
}

// ProbeDim issues a single embedding of a known short string and returns the
// returned vector length. Used at startup when the chosen embedder cannot
// declare its dim in advance.
func ProbeDim(ctx context.Context, e Embedder) (int, error) {
	v, err := e.Embed(ctx, "ilter-cache-dim-probe")
	if err != nil {
		return 0, fmt.Errorf("probe dim: %w", err)
	}
	return len(v), nil
}

// ResolveEmbedder returns the embedder selected by cfg.EmbeddingModel, or
// (when empty) the legacy Ollama embedder built from cfg.OllamaURL. A nil
// embedder is returned when both are unset; the cache then operates in
// exact-only mode. Resolution rules (in order):
//
//  1. cfg.EmbeddingModel == "" and cfg.OllamaURL == "" → (nil, nil).
//  2. cfg.EmbeddingModel == "" and cfg.OllamaURL != "" → legacy
//     OllamaEmbedder (dim 768, model "nomic-embed-text").
//  3. cfg.EmbeddingModel == "provider:model" → a ProviderEmbedder that calls
//     the listed provider's EmbeddingProvider capability.
func ResolveEmbedder(reg *provider.Registry, cfg config.CacheConfig) (Embedder, error) {
	if cfg.EmbeddingModel == "" {
		if cfg.OllamaURL == "" {
			return nil, nil
		}
		return NewOllamaEmbedder(cfg.OllamaURL), nil
	}

	providerName, model, ok := strings.Cut(cfg.EmbeddingModel, ":")
	if !ok || providerName == "" || model == "" {
		return nil, fmt.Errorf("ILTER_CACHE_EMBEDDING_MODEL must be in the form provider:model (e.g. openai:text-embedding-3-small); got %q", cfg.EmbeddingModel)
	}

	pe, err := NewProviderEmbedder(reg, providerName, model)
	if err != nil {
		return nil, err
	}
	return pe, nil
}
