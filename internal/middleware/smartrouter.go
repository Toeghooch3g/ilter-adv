package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/features/smartrouter"
	"github.com/ilter-ai/ilter/internal/model"

	"github.com/ilter-ai/ilter/internal/platform/reqmeta"
)

type strategyContextKey struct{}

var StrategyKey strategyContextKey

type preferenceContextKey struct{}

var PreferenceKey preferenceContextKey

type SmartRouterMiddleware struct {
	configCache *config.Cache
	smartRouter atomic.Pointer[smartrouter.SmartRouter]
}

func NewSmartRouterMiddleware(configCache *config.Cache, smartRouter *smartrouter.SmartRouter) *SmartRouterMiddleware {
	m := &SmartRouterMiddleware{configCache: configCache}
	m.smartRouter.Store(smartRouter)
	return m
}

// UpdateSmartRouter atomically swaps the SmartRouter instance on config change.
func (m *SmartRouterMiddleware) UpdateSmartRouter(sr *smartrouter.SmartRouter) {
	m.smartRouter.Store(sr)
}

// resolveRoutingConfig returns the active routing config, and false if
// smart routing shouldn't run for this request (no config snapshot, or the
// feature is disabled).
func (m *SmartRouterMiddleware) resolveRoutingConfig() (rc config.RoutingConfig, enabled bool) {
	snap := m.configCache.Get()
	if snap == nil {
		return config.RoutingConfig{}, false
	}
	rc = snap.RoutingConfig()
	return rc, rc.Enabled
}

// routeViaSmartRouter selects a model via the smart router, sets the
// StrategyKey/PreferenceKey context values, and records the decision in
// request metadata. Returns ctx with those values attached.
func (m *SmartRouterMiddleware) routeViaSmartRouter(ctx context.Context, req *model.ChatCompletionRequest, rc config.RoutingConfig) context.Context {
	sr := m.smartRouter.Load()
	selectedModel, score, err := sr.RouteRequest(ctx, req)
	if err != nil || selectedModel == "" {
		return context.WithValue(ctx, StrategyKey, "")
	}

	preference := rc.ProviderPreference
	if preference == "" {
		preference = "cheapest"
	}
	newCtx := context.WithValue(ctx, StrategyKey, selectedModel)
	newCtx = context.WithValue(newCtx, PreferenceKey, preference)
	if meta := reqmeta.GetRequestMetadata(ctx); meta != nil {
		meta.SetSmartRouted(true, selectedModel)
		meta.SetComplexityScore(score)
	}
	return newCtx
}

func (m *SmartRouterMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, enabled := m.resolveRoutingConfig()
		if !enabled {
			next.ServeHTTP(w, r)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		var req model.ChatCompletionRequest
		if err = json.Unmarshal(body, &req); err != nil {
			next.ServeHTTP(w, r)
			return
		}

		if req.Model != "" {
			ctx := context.WithValue(r.Context(), StrategyKey, req.Model)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		ctx := m.routeViaSmartRouter(r.Context(), &req, rc)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
