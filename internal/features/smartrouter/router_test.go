package smartrouter

import (
	"context"
	"net/http"
	"testing"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/model"
	"github.com/ilter-ai/ilter/internal/model/catalog"
	"github.com/ilter-ai/ilter/internal/provider"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRouterProvider is a minimal provider.Provider for router tests.
type stubRouterProvider struct{ name string }

func (p *stubRouterProvider) Name() string { return p.name }
func (p *stubRouterProvider) Type() string { return "mock" }
func (p *stubRouterProvider) TransformRequest(context.Context, *model.ChatCompletionRequest) (*http.Request, error) {
	return nil, nil
}

func (p *stubRouterProvider) TransformResponse(context.Context, *http.Response) (*model.ChatCompletionResponse, error) {
	return nil, nil
}
func (p *stubRouterProvider) Client() *http.Client              { return &http.Client{} }
func (p *stubRouterProvider) HealthCheck(context.Context) error { return nil }
func (p *stubRouterProvider) DiscoverModels(context.Context) ([]catalog.ModelInfo, error) {
	return nil, nil
}

// TestSelectModelForTierSkipsEmbedding verifies embedding-category models are
// never selected for chat requests via complexity routing (feature 4: the
// out-of-box Embedding category must not pollute chat model selection).
func TestSelectModelForTierSkipsEmbedding(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{
			Name: "mock-provider",
			Type: "mock",
			Models: []config.ModelConfig{
				{Name: "embed-only", Weight: 1},
				{Name: "standard-chat", Weight: 1},
			},
		}},
	}
	reg := provider.NewRegistry()
	reg.Register(&stubRouterProvider{name: "mock-provider"})

	lb, err := NewLoadBalancer(cfg, reg, nil)
	require.NoError(t, err)

	// Populate catalog with an embedding-only model so the router sees it.
	catalog.ModelsMu.Lock()
	catalog.Models["embed-only"] = []catalog.ModelInfo{{ID: "embed-only", Provider: "mock-provider", Category: "embedding"}}
	catalog.Models["standard-chat"] = []catalog.ModelInfo{{ID: "standard-chat", Provider: "mock-provider", Category: "standard"}}
	catalog.ModelsMu.Unlock()
	t.Cleanup(func() {
		catalog.ModelsMu.Lock()
		delete(catalog.Models, "embed-only")
		delete(catalog.Models, "standard-chat")
		catalog.ModelsMu.Unlock()
	})

	sr := NewSmartRouter(cfg, lb)

	// With only an embedding model available, routing must fail (no chat
	// candidate), not silently select the embedding model.
	embedOnlyLB, err := NewLoadBalancer(&config.Config{
		Providers: []config.ProviderConfig{{
			Name:   "mock-provider",
			Type:   "mock",
			Models: []config.ModelConfig{{Name: "embed-only", Weight: 1}},
		}},
	}, reg, nil)
	require.NoError(t, err)
	srEmbed := NewSmartRouter(cfg, embedOnlyLB)
	_, err = srEmbed.selectModelForTier("economy")
	assert.Error(t, err, "embedding-only model must not be selectable for chat")

	// With a standard model present, the router picks it and never the
	// embedding model.
	selected, err := sr.selectModelForTier("economy")
	require.NoError(t, err)
	assert.Equal(t, "standard-chat", selected)
}
