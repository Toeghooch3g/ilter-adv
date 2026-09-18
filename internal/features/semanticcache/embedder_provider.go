package semanticcache

import (
	"context"
	"fmt"

	"github.com/ilter-ai/ilter/internal/model"
	"github.com/ilter-ai/ilter/internal/provider"
)

// ProviderEmbedder calls the chosen provider's EmbeddingProvider capability.
// The model is the part after the colon in ILTER_CACHE_EMBEDDING_MODEL.
type ProviderEmbedder struct {
	prov  provider.EmbeddingProvider
	model string
	dim   int // captured by the first successful probe; afterwards stable
}

// NewProviderEmbedder resolves providerName in the registry, requires it to
// implement provider.EmbeddingProvider, and returns a ProviderEmbedder.
func NewProviderEmbedder(reg *provider.Registry, providerName, model string) (*ProviderEmbedder, error) {
	p, err := reg.Get(providerName)
	if err != nil {
		return nil, fmt.Errorf("embedding provider %q not registered: %w", providerName, err)
	}
	ep, ok := p.(provider.EmbeddingProvider)
	if !ok {
		return nil, fmt.Errorf("provider %q does not implement EmbeddingProvider", providerName)
	}
	return &ProviderEmbedder{prov: ep, model: model}, nil
}

func (e *ProviderEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	resp, err := e.prov.Embed(ctx, &model.EmbeddingRequest{
		Model: e.model,
		Input: text,
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("embedding provider %q returned no data for model %q", e.model, e.model)
	}
	raw := resp.Data[0].Embedding // []float64
	vec := make([]float32, len(raw))
	for i, v := range raw {
		vec[i] = float32(v)
	}
	if e.dim == 0 {
		e.dim = len(vec) // first successful call captures the dim
	} else if len(vec) != e.dim {
		return nil, fmt.Errorf("embedding dim changed: was %d, now %d", e.dim, len(vec))
	}
	return vec, nil
}

func (e *ProviderEmbedder) Dim() int { return e.dim }

// SetDim seeds the dimension from a previously persisted value so a restart
// with the same model can skip the network probe.
func (e *ProviderEmbedder) SetDim(dim int) { e.dim = dim }

func (e *ProviderEmbedder) Model() string { return e.model }
