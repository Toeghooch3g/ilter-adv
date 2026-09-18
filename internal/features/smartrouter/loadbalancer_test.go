package smartrouter

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/model"
	"github.com/ilter-ai/ilter/internal/model/catalog"
	"github.com/ilter-ai/ilter/internal/provider"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubProvider is a minimal provider.Provider that only exposes identity and
// a canned discovery result. Route loading only needs Name()/Type(), but the
// Provider interface requires the rest, so each method is stubbed.
type stubProvider struct {
	name        string
	ptype       string
	models      []catalog.ModelInfo
	discoverErr error
}

func (p *stubProvider) Name() string { return p.name }
func (p *stubProvider) Type() string { return p.ptype }
func (p *stubProvider) TransformRequest(context.Context, *model.ChatCompletionRequest) (*http.Request, error) {
	return nil, errors.New("not implemented")
}

func (p *stubProvider) TransformResponse(context.Context, *http.Response) (*model.ChatCompletionResponse, error) {
	return nil, errors.New("not implemented")
}
func (p *stubProvider) Client() *http.Client              { return &http.Client{} }
func (p *stubProvider) HealthCheck(context.Context) error { return nil }
func (p *stubProvider) DiscoverModels(context.Context) ([]catalog.ModelInfo, error) {
	if p.discoverErr != nil {
		return nil, p.discoverErr
	}
	return p.models, nil
}

// TestLoadRoutesFromDB_KeysByProviderName verifies that route rows from the
// provider_models store are looked up by provider *name* (the unique instance),
// not by provider *type*. Two custom providers sharing a type ("openai") must
// each get only their own models loaded into routes; otherwise one provider's
// models would appear on the other's routes (the collision this feature fixes).
func TestLoadRoutesFromDB_KeysByProviderName(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "custom-a", Type: "openai"},
			{Name: "custom-b", Type: "openai"},
		},
	}

	reg := provider.NewRegistry()
	reg.Register(&stubProvider{name: "custom-a", ptype: "openai"})
	reg.Register(&stubProvider{name: "custom-b", ptype: "openai"})

	lb, err := NewLoadBalancer(cfg, reg, nil)
	require.NoError(t, err)

	// The store returns models keyed by provider name: each instance exposes
	// one distinct model so we can detect any cross-wiring.
	err = lb.LoadRoutesFromDB(func(providerName string) ([]ProviderModelEntry, error) {
		switch providerName {
		case "custom-a":
			return []ProviderModelEntry{{Name: "model-a", Active: true}}, nil
		case "custom-b":
			return []ProviderModelEntry{{Name: "model-b", Active: true}}, nil
		default:
			return nil, nil
		}
	})
	require.NoError(t, err)

	// model-a must route only to custom-a, model-b only to custom-b.
	routesA, err := lb.GetRoutes("model-a")
	require.NoError(t, err)
	require.Len(t, routesA, 1)
	assert.Equal(t, "custom-a", routesA[0].Provider.Name())

	routesB, err := lb.GetRoutes("model-b")
	require.NoError(t, err)
	require.Len(t, routesB, 1)
	assert.Equal(t, "custom-b", routesB[0].Provider.Name())

	// Cross-checks: each model must not be wired to the other provider.
	for _, r := range routesA {
		assert.NotEqual(t, "custom-b", r.Provider.Name())
	}
	for _, r := range routesB {
		assert.NotEqual(t, "custom-a", r.Provider.Name())
	}
}

// TestRebuildProviders_UsesDBRows_WhenDiscoveryFails verifies that a runtime
// route rebuild (syncModelsToDB → RebuildProviders) keeps routes populated
// from persisted provider_models rows even when every provider's live
// /v1/models discovery fails. Before the fix RebuildProviders rebuilt only
// from DiscoverModels and silently skipped providers on error, wiping all
// routes and 404ing every chat request until a later successful reload.
func TestRebuildProviders_UsesDBRows_WhenDiscoveryFails(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "deepinfra", Type: "openai"},
		},
	}

	reg := provider.NewRegistry()
	reg.Register(&stubProvider{name: "deepinfra", ptype: "openai", discoverErr: errors.New("upstream down")})

	lb, err := NewLoadBalancer(cfg, reg, nil)
	require.NoError(t, err)

	// DB rows carry pricing; discovery failure must not erase them.
	loadFromDB := func(providerName string) ([]ProviderModelEntry, error) {
		if providerName == "deepinfra" {
			return []ProviderModelEntry{
				{Name: "DeepSeek-V4.1-Flash", Active: true, CostIn: 0.000002, CostOut: 0.000002},
			}, nil
		}
		return nil, nil
	}

	require.NoError(t, lb.RebuildProviders(reg, loadFromDB))

	route, err := lb.NextRoute("DeepSeek-V4.1-Flash", "")
	require.NoError(t, err, "route must survive discovery failure")
	assert.Equal(t, "deepinfra", route.Provider.Name())

	got, ok := lb.GetRouteByProvider("DeepSeek-V4.1-Flash", "deepinfra")
	require.True(t, ok, "GetRouteByProvider must resolve the DB-priced route")
	assert.Equal(t, 0.000002, got.Model.CostPerInputToken, "pricing must survive the rebuild")
	assert.Equal(t, 0.000002, got.Model.CostPerOutputToken)
}

// TestRebuildProviders_UsesDiscovery_WhenDBEmpty verifies the fallback path:
// a provider with no persisted rows is still rebuilt from live discovery, so
// a fresh boot with discovery succeeding keeps working.
func TestRebuildProviders_UsesDiscovery_WhenDBEmpty(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "ollama", Type: "ollama"},
		},
	}

	reg := provider.NewRegistry()
	reg.Register(&stubProvider{name: "ollama", ptype: "ollama", models: []catalog.ModelInfo{{ID: "llama3"}}})

	lb, err := NewLoadBalancer(cfg, reg, nil)
	require.NoError(t, err)

	require.NoError(t, lb.RebuildProviders(reg, func(string) ([]ProviderModelEntry, error) {
		return nil, nil // no DB rows at all
	}))

	route, err := lb.NextRoute("llama3", "")
	require.NoError(t, err)
	assert.Equal(t, "ollama", route.Provider.Name())
}
