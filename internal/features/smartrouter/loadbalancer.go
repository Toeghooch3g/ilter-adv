package smartrouter

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/features/circuitbreaker"
	"github.com/ilter-ai/ilter/internal/platform/cooldown"
	"github.com/ilter-ai/ilter/internal/provider"
)

// Route binds a provider to a model config.
type Route struct {
	Provider provider.Provider
	Model    config.ModelConfig
}

type ProviderModelEntry struct {
	Name           string
	Active         bool
	Category       string
	CostIn         float64
	CostOut        float64
	CostCacheRead  float64
	CostCacheWrite float64
}

// LoadBalancer selects provider routes for a given model.
type LoadBalancer struct {
	mu             sync.RWMutex
	routes         map[string][]Route
	providers      map[string]provider.Provider
	inactiveModels map[string]bool
	rrCounters     map[string]int // per-model round-robin index, survives config reloads
	cache          *config.Cache
	cfg            *config.Config
}

// NewLoadBalancer creates a LoadBalancer with routes from the config.
func NewLoadBalancer(cfg *config.Config, reg *provider.Registry, cache *config.Cache) (*LoadBalancer, error) {
	config.EnrichConfig(cfg)

	lb := &LoadBalancer{
		routes:         make(map[string][]Route),
		providers:      make(map[string]provider.Provider),
		inactiveModels: make(map[string]bool),
		rrCounters:     make(map[string]int),
		cache:          cache,
		cfg:            cfg,
	}

	for _, pCfg := range cfg.Providers {
		p, err := reg.Get(pCfg.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get provider %s from registry: %w", pCfg.Name, err)
		}

		lb.providers[pCfg.Name] = p

		for _, mCfg := range pCfg.Models {
			lb.routes[mCfg.Name] = append(lb.routes[mCfg.Name], Route{
				Provider: p,
				Model:    mCfg,
			})
		}
	}

	return lb, nil
}

// GetProvider returns a provider by name from the LoadBalancer.
func (lb *LoadBalancer) GetProvider(name string) (provider.Provider, error) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	p, ok := lb.providers[name]
	if !ok {
		return nil, fmt.Errorf("provider %s not found in load balancer", name)
	}
	return p, nil
}

// filterAvailable returns routes whose circuit breaker is not open.
func filterAvailable(routes []Route) []Route {
	return slices.DeleteFunc(routes, func(r Route) bool {
		return circuitbreaker.State(r.Provider.Client().Transport) == "open"
	})
}

// NextRoute selects a provider for the given model.
func (lb *LoadBalancer) NextRoute(modelName string, preference string) (Route, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	if lb.inactiveModels[modelName] {
		return Route{}, fmt.Errorf("model %s is deactivated", modelName)
	}

	routes, ok := lb.routes[modelName]
	if !ok || len(routes) == 0 {
		return Route{}, fmt.Errorf("no providers configured for model: %s", modelName)
	}

	avail := filterAvailable(routes)
	if len(avail) == 0 {
		return Route{}, fmt.Errorf("all providers for model %s have open circuit breakers", modelName)
	}

	switch preference {
	case "round-robin":
		idx := lb.rrCounters[modelName]
		lb.rrCounters[modelName] = (idx + 1) % len(avail)
		return avail[idx], nil
	default:
		return avail[0], nil
	}
}

// SelectCandidates returns a ranked list of cooldown.Candidate for the requested model.
// Active (available) routes are prioritized; candidates in cooldown are placed at the tail.
// When FallbackConfig.ModelDowngrade is non-none, downgrade model candidates are appended
// after the primary model's candidates so FallbackExecutor can try them on exhaustion.
// resolveFallbackConfig returns the effective fallback config, preferring
// the live cache snapshot (which includes runtime_config overrides) over
// the static boot config.
func (lb *LoadBalancer) resolveFallbackConfig() config.FallbackConfig {
	fb := lb.cfg.Fallback
	if lb.cache != nil {
		if snap := lb.cache.Get(); snap != nil {
			fb = snap.Fallback()
		}
	}
	return fb
}

// buildDowngradeCandidates builds fallback candidates for every resolved
// downgrade model (skipping skipModel, if set), appending each model's
// isDowngrade=true candidates.
func (lb *LoadBalancer) buildDowngradeCandidates(ctx context.Context, downgradeModels []string, skipModel, preference string, cooldownStore cooldown.Store) []cooldown.Candidate {
	var candidates []cooldown.Candidate
	for _, dm := range downgradeModels {
		if skipModel != "" && dm == skipModel {
			continue
		}
		if dRoutes, ok := lb.routes[dm]; ok && len(dRoutes) > 0 {
			candidates = append(candidates, lb.buildCandidates(ctx, dRoutes, dm, preference, true, cooldownStore)...)
		}
	}
	return candidates
}

// selectDowngradeOnlyCandidates handles SelectCandidates when modelName has
// no registered routes at all: it tries the configured downgrade fallback
// before giving up.
func (lb *LoadBalancer) selectDowngradeOnlyCandidates(ctx context.Context, modelName string, fb config.FallbackConfig, preference string, cooldownStore cooldown.Store) ([]cooldown.Candidate, error) {
	if !fb.Enabled || fb.ModelDowngrade == "" || fb.ModelDowngrade == "none" {
		return nil, fmt.Errorf("no providers configured for model: %s", modelName)
	}
	downgradeModels := lb.resolveDowngradeModels(modelName, fb)
	candidates := lb.buildDowngradeCandidates(ctx, downgradeModels, "", preference, cooldownStore)
	if len(candidates) > 0 {
		return candidates, nil
	}
	return nil, fmt.Errorf("no providers configured for model: %s", modelName)
}

func (lb *LoadBalancer) SelectCandidates(ctx context.Context, modelName string, preference string, cooldownStore cooldown.Store) ([]cooldown.Candidate, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	if lb.inactiveModels[modelName] {
		return nil, fmt.Errorf("model %s is deactivated", modelName)
	}

	routes, ok := lb.routes[modelName]
	hasPrimaryRoutes := ok && len(routes) > 0

	fb := lb.resolveFallbackConfig()

	// Model not in registry — try downgrade fallback before erroring.
	// Uses the same dashboard-configured algorithm (cheapest/specific/none).
	if !hasPrimaryRoutes {
		return lb.selectDowngradeOnlyCandidates(ctx, modelName, fb, preference, cooldownStore)
	}

	candidates := lb.buildCandidates(ctx, routes, modelName, preference, false, cooldownStore)

	// Append ModelDowngrade candidates when the fallback config requests it.
	if fb.Enabled && fb.ModelDowngrade != "" && fb.ModelDowngrade != "none" {
		downgradeModels := lb.resolveDowngradeModels(modelName, fb)
		candidates = append(candidates, lb.buildDowngradeCandidates(ctx, downgradeModels, modelName, preference, cooldownStore)...)
	}

	return candidates, nil
}

// candidatesForRoute builds one cooldown.Candidate per API key configured
// for r's provider (or a single default-keyed candidate when it exposes
// none), splitting them into ready vs. in-cooldown buckets.
func candidatesForRoute(ctx context.Context, r Route, isDowngrade bool, cooldownStore cooldown.Store) (ready, inCooldown []cooldown.Candidate) {
	var keys []string
	if kp, ok := r.Provider.(interface{ APIKeys() []string }); ok {
		keys = kp.APIKeys()
	}
	if len(keys) == 0 {
		keys = []string{""}
	}

	for i, k := range keys {
		keyID := "default"
		if len(keys) > 1 {
			keyID = fmt.Sprintf("key_%d", i+1) // 1-indexed, only when multi-key
		}
		cand := cooldown.Candidate{
			Provider:    r.Provider.Name(),
			Model:       r.Model.Name,
			APIKey:      strings.TrimSpace(k),
			KeyID:       keyID,
			IsDowngrade: isDowngrade,
		}
		if cooldownStore != nil && cooldownStore.InCooldown(ctx, cand) {
			inCooldown = append(inCooldown, cand)
		} else {
			ready = append(ready, cand)
		}
	}
	return ready, inCooldown
}

// buildCandidates creates candidate entries from routes, optionally marking them as IsDowngrade.
func (lb *LoadBalancer) buildCandidates(ctx context.Context, routes []Route, modelName, preference string, isDowngrade bool, cooldownStore cooldown.Store) []cooldown.Candidate {
	avail := filterAvailable(routes)
	if len(avail) == 0 {
		avail = routes
	}

	if preference == "round-robin" && len(avail) > 1 {
		idx := lb.rrCounters[modelName]
		lb.rrCounters[modelName] = (idx + 1) % len(avail)
		avail = append(avail[idx:], avail[:idx]...)
	}

	var candidates []cooldown.Candidate
	var inCooldown []cooldown.Candidate

	for _, r := range avail {
		ready, cd := candidatesForRoute(ctx, r, isDowngrade, cooldownStore)
		candidates = append(candidates, ready...)
		inCooldown = append(inCooldown, cd...)
	}

	candidates = append(candidates, inCooldown...)
	return candidates
}

// resolveDowngradeModels returns models to use as fallback candidates.
func (lb *LoadBalancer) resolveDowngradeModels(primaryModel string, fb config.FallbackConfig) []string {
	if fb.ModelDowngrade == "" || fb.ModelDowngrade == "none" {
		return nil
	}

	switch fb.ModelDowngrade {
	case "cheapest":
		return lb.findCheapestDowngrade(primaryModel, fb.AllowedModels)
	default:
		if len(fb.AllowedModels) > 0 {
			for _, m := range fb.AllowedModels {
				if m == fb.ModelDowngrade {
					return []string{m}
				}
			}
			return nil
		}
		if fb.ModelDowngrade == primaryModel {
			return nil
		}
		return []string{fb.ModelDowngrade}
	}
}

// findCheapestDowngrade finds all allowed models at the cheapest per-token cost
// that aren't the primary model. All equal-cost models are returned (e.g., all
// free models with cost=0) so FallbackExecutor can iterate through them — if
// one is in cooldown or fails, the next is tried instead of stopping at one.
// Among ties, order follows the dashboard-configured AllowedModels priority
// list (first added = tried first), not an arbitrary/alphabetical order —
// FallbackExecutor stops at the first candidate that succeeds, so list order
// decides the winner.
// candidateDowngradeModels lists every registered model other than
// primaryModel, restricted to allowedModels when that list is non-empty.
func candidateDowngradeModels(routes map[string][]Route, primaryModel string, allowedModels []string) []string {
	var candidates []string
	for name := range routes {
		if name == primaryModel {
			continue
		}
		if len(allowedModels) > 0 && !slices.Contains(allowedModels, name) {
			continue
		}
		candidates = append(candidates, name)
	}
	return candidates
}

// modelCost returns a model's per-token cost (input+output), or ok=false if
// it has no registered route.
func modelCost(routes map[string][]Route, name string) (cost float64, ok bool) {
	rs := routes[name]
	if len(rs) == 0 {
		return 0, false
	}
	return rs[0].Model.CostPerInputToken + rs[0].Model.CostPerOutputToken, true
}

// cheapestModelsAmong finds the minimum cost among candidates and returns
// the set of candidate names tied at that minimum.
func cheapestModelsAmong(routes map[string][]Route, candidates []string) map[string]bool {
	minCost := math.MaxFloat64
	for _, name := range candidates {
		if cost, ok := modelCost(routes, name); ok && cost < minCost {
			minCost = cost
		}
	}

	inCheapest := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		if cost, ok := modelCost(routes, name); ok && cost == minCost {
			inCheapest[name] = true
		}
	}
	return inCheapest
}

// orderCheapestModels renders the cheapest-model set as a deterministic
// list: following allowedModels' priority order when given, else
// alphabetical.
func orderCheapestModels(inCheapest map[string]bool, allowedModels []string) []string {
	if len(allowedModels) > 0 {
		var cheapest []string
		for _, name := range allowedModels {
			if inCheapest[name] {
				cheapest = append(cheapest, name)
			}
		}
		return cheapest
	}
	names := make([]string, 0, len(inCheapest))
	for name := range inCheapest {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (lb *LoadBalancer) findCheapestDowngrade(primaryModel string, allowedModels []string) []string {
	candidates := candidateDowngradeModels(lb.routes, primaryModel, allowedModels)
	if len(candidates) == 0 {
		return nil
	}
	inCheapest := cheapestModelsAmong(lb.routes, candidates)
	return orderCheapestModels(inCheapest, allowedModels)
}

// RebuildProviders clears all routes and reloads them from the persisted
// provider_models rows (keyed by provider name, full pricing included),
// falling back to live discovery only for providers with no DB rows. This
// keeps routes stable when a provider's /v1/models discovery fails — a
// transient upstream error can no longer wipe every route and 404 chat.
func (lb *LoadBalancer) RebuildProviders(reg *provider.Registry, loadFromDB func(provider string) ([]ProviderModelEntry, error)) error {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.routes = make(map[string][]Route)
	lb.providers = make(map[string]provider.Provider)

	for _, p := range reg.List() {
		lb.providers[p.Name()] = p
	}

	if loadFromDB != nil {
		if err := lb.loadRoutesFromDBLocked(loadFromDB); err != nil {
			return fmt.Errorf("rebuild routes from DB: %w", err)
		}
	}

	// Discovery fallback for providers that have no persisted rows (fresh
	// boot before discovery has populated provider_models). Discovery errors
	// are skipped — the DB-backed routes above are authoritative.
	for _, p := range reg.List() {
		if lb.hasAnyRouteFor(p.Name()) {
			continue
		}
		models, err := p.DiscoverModels(context.Background())
		if err != nil {
			continue
		}
		for _, m := range models {
			lb.routes[m.ID] = append(lb.routes[m.ID], Route{
				Provider: p,
				Model: config.ModelConfig{
					Name: m.ID,
				},
			})
		}
	}
	return nil
}

// hasAnyRouteFor reports whether name has at least one registered route.
func (lb *LoadBalancer) hasAnyRouteFor(name string) bool {
	for _, rs := range lb.routes {
		for _, r := range rs {
			if r.Provider != nil && r.Provider.Name() == name {
				return true
			}
		}
	}
	return false
}

// LoadRoutesFromDB adds routes from a provider model store for providers that
// have no YAML-configured models. Provider models in the DB are keyed by the
// provider's unique name (instance), not its type — several providers may
// share a type (e.g. multiple custom OpenAI-compatible endpoints), so we must
// look them up by name to avoid mixing one provider's models into another's
// routes.
func (lb *LoadBalancer) LoadRoutesFromDB(fn func(provider string) ([]ProviderModelEntry, error)) error {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.loadRoutesFromDBLocked(fn)
}

// loadRoutesFromDBLocked appends DB rows to routes; the caller must hold mu.
func (lb *LoadBalancer) loadRoutesFromDBLocked(fn func(provider string) ([]ProviderModelEntry, error)) error {
	for name, p := range lb.providers {
		models, err := fn(p.Name())
		if err != nil {
			return fmt.Errorf("load provider %s models: %w", name, err)
		}
		for _, m := range models {
			if !m.Active {
				continue
			}
			lb.routes[m.Name] = append(lb.routes[m.Name], Route{
				Provider: p,
				Model: config.ModelConfig{
					Name:                    m.Name,
					CostPerInputToken:       m.CostIn,
					CostPerOutputToken:      m.CostOut,
					CostPerCachedInputToken: m.CostCacheRead,
					CostPerCacheWriteToken:  m.CostCacheWrite,
				},
			})
		}
	}
	return nil
}

// GetRoutes returns routes for a specific model.
func (lb *LoadBalancer) GetRoutes(modelName string) ([]Route, error) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	if lb.inactiveModels[modelName] {
		return nil, fmt.Errorf("model %s is deactivated", modelName)
	}

	routes, ok := lb.routes[modelName]
	if !ok || len(routes) == 0 {
		return nil, fmt.Errorf("no providers configured for model: %s", modelName)
	}

	routesCopy := make([]Route, len(routes))
	copy(routesCopy, routes)
	return routesCopy, nil
}

// GetRouteByProvider returns the route for modelName served by providerName
// (matching by provider name), or (Route{}, false). The caller uses it to
// recover the full ModelConfig (pricing fields included) for the provider a
// request was actually dispatched to, since dispatch yields only
// provider/model names.
func (lb *LoadBalancer) GetRouteByProvider(modelName, providerName string) (Route, bool) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	for _, r := range lb.routes[modelName] {
		if r.Provider != nil && r.Provider.Name() == providerName {
			return r, true
		}
	}
	return Route{}, false
}

// HasProviderModel reports whether providerName currently has a route for
// modelName. The proxy handler uses it to decide whether a "provider/model"
// request model is a valid provider pin (vs. a garbage prefix to strip).
func (lb *LoadBalancer) HasProviderModel(providerName, modelName string) bool {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	for _, r := range lb.routes[modelName] {
		if r.Provider != nil && r.Provider.Name() == providerName {
			return true
		}
	}
	return false
}

// GetAvailableModels returns model names that have at least one route.
func (lb *LoadBalancer) GetAvailableModels() []string {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	models := make([]string, 0, len(lb.routes))
	for model := range lb.routes {
		if !lb.inactiveModels[model] {
			models = append(models, model)
		}
	}
	return models
}

// ModelInfo describes one (model, provider) pair.
type ModelInfo struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Type     string `json:"type"`
	OwnedBy  string `json:"owned_by"`
	Active   bool   `json:"active"`
}

func (lb *LoadBalancer) GetAvailableModelInfos() []ModelInfo {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	infos := make([]ModelInfo, 0)
	for _, routes := range lb.routes {
		for _, r := range routes {
			if r.Provider == nil {
				slog.Warn("route has nil provider, skipping", "model", r.Model.Name)
				continue
			}
			infos = append(infos, ModelInfo{
				Name:     r.Model.Name,
				Provider: r.Provider.Name(),
				Type:     r.Provider.Type(),
				OwnedBy:  r.Provider.Name(),
				Active:   !lb.inactiveModels[r.Model.Name],
			})
		}
	}
	return infos
}

// ProviderStatus summarizes provider health and circuit breaker state.
type ProviderStatus struct {
	Name                string     `json:"name"`
	Type                string     `json:"type"`
	Status              string     `json:"status"`
	CircuitBreakerState string     `json:"circuit_breaker_state"`
	LastErrorTime       *time.Time `json:"last_error_time,omitempty"`
	LastSuccessTime     *time.Time `json:"last_success_time,omitempty"`
	TotalRequests       int64      `json:"total_requests"`
	TotalErrors         int64      `json:"total_errors"`
	SuccessRate         float64    `json:"success_rate"`
	APIKeysCount        int        `json:"api_keys_count"`
	APIKeySet           bool       `json:"api_key_set"`
	APIKeySource        string     `json:"api_key_source"`
}

// providerAPIKeyInfo looks up the configured API key count/source for a
// named provider from the boot config.
func (lb *LoadBalancer) providerAPIKeyInfo(name string) (count int, set bool, source string) {
	if lb.cfg == nil {
		return 0, false, ""
	}
	for _, p := range lb.cfg.Providers {
		if p.Name != name {
			continue
		}
		keys := p.GetAPIKeys()
		return len(keys), len(keys) > 0, p.APIKeySource
	}
	return 0, false, ""
}

// healthStatusForCircuitState maps a circuit breaker state to a
// provider-status health label.
func healthStatusForCircuitState(cbState string) string {
	switch cbState {
	case "open":
		return "offline"
	case "half-open":
		return "degraded"
	default:
		return "online"
	}
}

// buildProviderStatus assembles one provider's status row from its live
// circuit-breaker metrics and configured API key info.
func (lb *LoadBalancer) buildProviderStatus(r Route) ProviderStatus {
	name := r.Provider.Name()

	cbState := "unknown"
	var totalReqs, totalErrs int64
	var lastErr, lastSuc *time.Time

	if client := r.Provider.Client(); client != nil && client.Transport != nil {
		cbState = circuitbreaker.State(client.Transport)
		totalReqs, totalErrs, lastErr, lastSuc = circuitbreaker.Metrics(client.Transport)
	}

	successRate := 0.0
	if totalReqs > 0 {
		successRate = float64(totalReqs-totalErrs) / float64(totalReqs) * 100
	}

	apiKeysCount, apiKeySet, apiKeySource := lb.providerAPIKeyInfo(name)

	return ProviderStatus{
		Name:                name,
		Type:                r.Provider.Type(),
		Status:              healthStatusForCircuitState(cbState),
		CircuitBreakerState: cbState,
		LastErrorTime:       lastErr,
		LastSuccessTime:     lastSuc,
		TotalRequests:       totalReqs,
		TotalErrors:         totalErrs,
		SuccessRate:         successRate,
		APIKeysCount:        apiKeysCount,
		APIKeySet:           apiKeySet,
		APIKeySource:        apiKeySource,
	}
}

func (lb *LoadBalancer) GetProviderStatus() []ProviderStatus {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	seen := make(map[string]bool)
	var statuses []ProviderStatus

	for _, routes := range lb.routes {
		for _, r := range routes {
			name := r.Provider.Name()
			if seen[name] {
				continue
			}
			seen[name] = true
			statuses = append(statuses, lb.buildProviderStatus(r))
		}
	}

	return statuses
}

func (lb *LoadBalancer) SetInactiveModels(names []string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.inactiveModels = make(map[string]bool)
	for _, name := range names {
		lb.inactiveModels[name] = true
	}
}

// HasProvider checks if a provider with the given name exists.
func (lb *LoadBalancer) HasProvider(name string) bool {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	_, ok := lb.providers[name]
	return ok
}
