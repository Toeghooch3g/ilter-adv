package semanticcache

import (
	"context"
	"fmt"

	"github.com/ilter-ai/ilter/internal/model"
	"github.com/ilter-ai/ilter/internal/provider"
)

// ProviderReranker re-orders cache candidates via a provider's RerankProvider
// capability. It is the rerank symmetric counterpart to ProviderEmbedder.
type ProviderReranker struct {
	prov  provider.RerankProvider
	model string
}

// NewProviderReranker resolves providerName in the registry and requires it to
// implement provider.RerankProvider.
func NewProviderReranker(reg *provider.Registry, providerName, model string) (*ProviderReranker, error) {
	p, err := reg.Get(providerName)
	if err != nil {
		return nil, fmt.Errorf("rerank provider %q not registered: %w", providerName, err)
	}
	rp, ok := p.(provider.RerankProvider)
	if !ok {
		return nil, fmt.Errorf("provider %q does not implement RerankProvider", providerName)
	}
	return &ProviderReranker{prov: rp, model: model}, nil
}

// Rerank sends the candidate response bodies as documents and returns them
// re-ordered best-first, annotated with the provider's relevance scores.
func (r *ProviderReranker) Rerank(ctx context.Context, query string, candidates []Reranked) ([]Reranked, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	docs := make([]string, len(candidates))
	for i, c := range candidates {
		docs[i] = c.Text
	}
	topN := len(candidates)
	resp, err := r.prov.Rerank(ctx, &model.RerankRequest{
		Model:           r.model,
		Query:           query,
		Documents:       docs,
		TopN:            &topN,
		ReturnDocuments: false,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Reranked, 0, len(resp.Results))
	for _, res := range resp.Results {
		if res.Index < 0 || res.Index >= len(candidates) {
			continue
		}
		c := candidates[res.Index]
		c.Score = res.RelevanceScore
		c.Present = true
		out = append(out, c)
	}
	return out, nil
}

func (r *ProviderReranker) Model() string { return r.model }
