package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db"
	"github.com/ilter-ai/ilter/internal/features/budget"
	"github.com/ilter-ai/ilter/internal/features/fallback"
	"github.com/ilter-ai/ilter/internal/features/loopdetect"
	"github.com/ilter-ai/ilter/internal/features/smartrouter"
	"github.com/ilter-ai/ilter/internal/middleware"
	"github.com/ilter-ai/ilter/internal/model"
	"github.com/ilter-ai/ilter/internal/model/catalog"
	"github.com/ilter-ai/ilter/internal/platform/cooldown"
	"github.com/ilter-ai/ilter/internal/platform/reqmeta"
	"github.com/ilter-ai/ilter/internal/provider"
)

type Handler struct {
	lb               *smartrouter.LoadBalancer
	auditLogger      *middleware.AuditLoggerMiddleware
	budgetEnforcer   *budget.Enforcer
	loopDetector     *loopdetect.Detector
	cfg              *config.Config
	configCache      *config.Cache
	store            *db.SQLiteStore
	cooldownStore    cooldown.Store
	fallbackExecutor *fallback.FallbackExecutor
	chatChain        http.Handler
}

func (h *Handler) SetFallbackExecutor(fe *fallback.FallbackExecutor, store cooldown.Store) {
	h.fallbackExecutor = fe
	h.cooldownStore = store
}

// SetChatChain wires in the chat-completions middleware chain (auth, budget,
// PII, guardrails, MCP tool injection, smart routing, semantic cache, loop
// detection) so that wire-format-translating endpoints like AnthropicMessages
// and LegacyCompletions can re-enter it and get identical behavior/headers to
// a native /v1/chat/completions request.
func (h *Handler) SetChatChain(chain http.Handler) {
	h.chatChain = chain
}

// ChatChain returns the chat-completions middleware chain, for other
// consumers (e.g. the dashboard's request-replay feature) that need the same
// reference SetChatChain was given.
func (h *Handler) ChatChain() http.Handler {
	return h.chatChain
}

func NewHandler(
	lb *smartrouter.LoadBalancer,
	auditLogger *middleware.AuditLoggerMiddleware,
	budgetEnforcer *budget.Enforcer,
	loopDetector *loopdetect.Detector,
) *Handler {
	return &Handler{
		lb:             lb,
		auditLogger:    auditLogger,
		budgetEnforcer: budgetEnforcer,
		loopDetector:   loopDetector,
	}
}

func (h *Handler) SetStore(store *db.SQLiteStore) {
	h.store = store
}

// SetConfigCache sets the runtime config cache for the handler. The cache
// provides access to decrypted provider API keys and other runtime config.
// It is safe to call concurrently with requests (the cache uses atomic snapshots).
func (h *Handler) SetConfigCache(cache *config.Cache) {
	h.configCache = cache
}

func (h *Handler) SetConfig(cfg *config.Config) { h.cfg = cfg }

func (h *Handler) emitStandard() bool {
	return h.cfg != nil && h.cfg.Headers.EmitStandard
}

// resolveRequestedModel resolves the model to use for this request.
// Priority: middleware-selected model (from SmartRouterMiddleware) > explicit model in the
// request body. Returns ok=false and writes an error response
// when no model can be resolved.
//
// A request model of the form "provider/model" pins the request to exactly
// that provider when the prefix names a configured provider serving the
// model; the caller filters SelectCandidates to it. Any other prefixed name
// has its prefix stripped as before (garbage prefixes must not break
// requests). The returned pinnedProvider is non-empty only for a valid pin.
func (h *Handler) resolveRequestedModel(w http.ResponseWriter, r *http.Request, req *model.ChatCompletionRequest, meta *reqmeta.RequestLoggingMetadata) (selectedModel string, complexityScore float64, pinnedProvider string, ok bool) {
	// 1. Check if SmartRouterMiddleware already selected a model
	if midModel, ok := r.Context().Value(middleware.StrategyKey).(string); ok && midModel != "" {
		selectedModel = midModel
		if meta != nil {
			meta.WithLock(func() {
				complexityScore = meta.ComplexityScore
			})
		}
	} else if req.Model != "" {
		// 2. Use explicit model from request body
		selectedModel = req.Model
		complexityScore = smartrouter.ScoreComplexity(r.Context(), req.Messages)
		if meta != nil {
			meta.SetComplexityScore(complexityScore)
		}
	} else {
		model.WriteJSONError(w, http.StatusBadRequest, model.ErrTypeInvalidRequest, "no model selected in request")
		return "", 0, "", false
	}

	// Provider pinning: "provider/model" resolves to that provider's route
	// for the model. Smart-router-selected models are already bare names, so
	// pinning only ever fires for an explicit request-body model. A nil lb
	// (unit-test handlers without routing) behaves as the old strip-only path.
	if h.lb != nil {
		if before, after, hasSlash := strings.Cut(selectedModel, "/"); hasSlash && after != "" && h.lb.HasProviderModel(before, after) {
			pinnedProvider = before
			selectedModel = after
		}
	}
	if pinnedProvider == "" {
		if canonical := catalog.CanonicalModelID(selectedModel); canonical != selectedModel {
			slog.Debug("model: stripped provider prefix", "before", selectedModel, "after", canonical)
			selectedModel = canonical
		}
	}

	return selectedModel, complexityScore, pinnedProvider, true
}

func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	meta := reqmeta.GetRequestMetadata(r.Context())

	defer func() { _ = r.Body.Close() }()
	var req model.ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.recordErrorAudit(r, nil, "", http.StatusBadRequest, err, start, nil)
		model.WriteJSONError(w, http.StatusBadRequest, model.ErrTypeInvalidRequest, fmt.Sprintf("invalid json body: %v", err))
		return
	}

	keyID := reqmeta.GetKeyID(r.Context())
	if meta != nil {
		meta.SetKeyID(keyID)
	}

	selectedModel, complexityScore, pinnedProvider, ok := h.resolveRequestedModel(w, r, &req, meta)
	if !ok {
		h.recordErrorAudit(r, nil, "", http.StatusBadRequest, fmt.Errorf("no model selected in request"), start, req.Messages)
		return
	}
	req.Model = selectedModel

	preference, _ := r.Context().Value(middleware.PreferenceKey).(string)
	candidates, err := h.lb.SelectCandidates(r.Context(), req.Model, preference, h.cooldownStore)
	if err != nil {
		h.recordErrorAudit(r, nil, selectedModel, http.StatusNotFound, err, start, req.Messages)
		model.WriteJSONError(w, http.StatusNotFound, model.ErrTypeModelNotFound, err.Error())
		return
	}

	// A "provider/model" request pins dispatch to exactly that provider:
	// drop every candidate that isn't the pinned one before dispatch.
	if pinnedProvider != "" {
		candidates = slices.DeleteFunc(candidates, func(c cooldown.Candidate) bool {
			return c.Provider != pinnedProvider
		})
		if len(candidates) == 0 {
			err := fmt.Errorf("no providers configured for model: %s (pinned provider %q)", selectedModel, pinnedProvider)
			h.recordErrorAudit(r, nil, selectedModel, http.StatusNotFound, err, start, req.Messages)
			model.WriteJSONError(w, http.StatusNotFound, model.ErrTypeModelNotFound, err.Error())
			return
		}
	}

	routes, _ := h.lb.GetRoutes(req.Model)
	var firstRoute smartrouter.Route
	if len(routes) > 0 {
		firstRoute = routes[0]
	}

	// Cost estimate response headers (computed after routes are known)
	costEstimate, altCost, savingsPotential := computeCostEstimates(req.Messages, firstRoute.Model, selectedModel, req.MaxTokens)
	setPreRequest(w, preRequest{
		ComplexityScore:  complexityScore,
		SelectedModel:    selectedModel,
		CostEstimate:     costEstimate,
		AlternativeCost:  altCost,
		SavingsPotential: savingsPotential,
	}, h.emitStandard())

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var resp *http.Response
	var p provider.Provider
	var finalCandidate cooldown.Candidate
	var dispatched bool

	if h.fallbackExecutor != nil {
		resp, p, finalCandidate, dispatched = h.dispatchWithFallback(ctx, w, r, &req, candidates, start)
	} else {
		resp, p, finalCandidate, dispatched = h.dispatchSingleCandidate(ctx, w, r, &req, candidates, start)
	}
	if !dispatched {
		return
	}

	// Both downstream handlers (streaming and non-streaming) close resp.Body
	// themselves; this defer guards the transient period so the linter can
	// verify the body is always released. Double-close is a no-op for net/http.
	defer func() { _ = resp.Body.Close() }()

	finalRoute := smartrouter.Route{
		Provider: p,
		Model: config.ModelConfig{
			Name: finalCandidate.Model,
		},
	}
	// Recover the full ModelConfig (pricing fields) for the provider/model the
	// request was actually dispatched to. SelectCandidates yields only names;
	// without this the cost calc / pricing headers would read zero prices.
	if r, ok := h.lb.GetRouteByProvider(finalCandidate.Model, p.Name()); ok {
		finalRoute.Model = r.Model
	}

	if req.Stream {
		h.handleStreaming(ctx, cancel, w, resp, p, &finalRoute, req.Model, start, r, &req)
		return
	}

	h.writeNonStreamingResponse(ctx, w, r, resp, &finalRoute, &req, start)
}

// dispatchWithFallback runs req through h.fallbackExecutor across
// candidates, writing an error response (and recording an error audit
// entry) if every candidate fails. Returns ok=false when it already wrote
// the failure response and the caller should return immediately.
func (h *Handler) dispatchWithFallback(ctx context.Context, w http.ResponseWriter, r *http.Request, req *model.ChatCompletionRequest, candidates []cooldown.Candidate, start time.Time) (resp *http.Response, p provider.Provider, finalCandidate cooldown.Candidate, ok bool) {
	res, execErr := h.fallbackExecutor.Execute(ctx, candidates, func(c context.Context, cand cooldown.Candidate, pvd provider.Provider) (int, http.Header, error) {
		callCtx := c
		if cand.APIKey != "" {
			callCtx = provider.WithSelectedAPIKey(c, cand.APIKey)
		}
		// ModelDowngrade: cand.Model may differ from req.Model when falling
		// back to an alternative model. Use cand.Model so the upstream
		// receives the actual model the candidate serves.
		req.Model = cand.Model
		providerReq, errTransform := pvd.TransformRequest(callCtx, req)
		if errTransform != nil {
			return http.StatusBadRequest, nil, errTransform
		}
		httpResp, errDo := pvd.Client().Do(providerReq)
		if errDo != nil {
			return 0, nil, errDo
		}
		if httpResp.StatusCode >= 400 {
			headers := httpResp.Header
			bodyBytes, _ := io.ReadAll(httpResp.Body)
			_ = httpResp.Body.Close()
			cleanMsg := sanitizeProviderErrorMessage(string(bodyBytes))
			return httpResp.StatusCode, headers, fmt.Errorf("provider %s status %d: %s", pvd.Name(), httpResp.StatusCode, cleanMsg)
		}
		resp = httpResp
		p = pvd
		finalCandidate = cand
		return httpResp.StatusCode, httpResp.Header, nil
	})
	if execErr == nil {
		return resp, p, finalCandidate, true
	}

	statusCode := http.StatusServiceUnavailable
	if res != nil && res.StatusCode > 0 {
		statusCode = res.StatusCode
	}
	errType := model.ErrTypeAllProvidersFail
	if statusCode == http.StatusTooManyRequests {
		errType = model.ErrTypeInsufficientQuota
	}
	cleanMsg := sanitizeProviderErrorMessage(execErr.Error())
	slog.Error("all providers failed", "model", req.Model, "error", execErr)
	h.recordErrorAudit(r, nil, req.Model, statusCode, execErr, start, req.Messages)
	model.WriteJSONError(w, statusCode, errType, cleanMsg)
	return nil, nil, cooldown.Candidate{}, false
}

// dispatchSingleCandidate sends req to the first available candidate (used
// when no fallback executor is configured), writing an error response (and
// recording an error audit entry) on any failure. Returns ok=false when it
// already wrote the failure response and the caller should return
// immediately.
func (h *Handler) dispatchSingleCandidate(ctx context.Context, w http.ResponseWriter, r *http.Request, req *model.ChatCompletionRequest, candidates []cooldown.Candidate, start time.Time) (resp *http.Response, p provider.Provider, finalCandidate cooldown.Candidate, ok bool) {
	if len(candidates) == 0 {
		h.recordErrorAudit(r, nil, req.Model, http.StatusNotFound, fmt.Errorf("no candidates available"), start, req.Messages)
		model.WriteJSONError(w, http.StatusNotFound, model.ErrTypeModelNotFound, "no candidates available")
		return nil, nil, cooldown.Candidate{}, false
	}
	firstCand := candidates[0]
	pvd, errGet := h.lb.GetProvider(firstCand.Provider)
	if errGet != nil {
		h.recordErrorAudit(r, nil, req.Model, http.StatusNotFound, errGet, start, req.Messages)
		model.WriteJSONError(w, http.StatusNotFound, model.ErrTypeModelNotFound, errGet.Error())
		return nil, nil, cooldown.Candidate{}, false
	}
	providerReq, errTransform := pvd.TransformRequest(ctx, req)
	if errTransform != nil {
		h.recordErrorAudit(r, nil, req.Model, http.StatusBadRequest, errTransform, start, req.Messages)
		model.WriteJSONError(w, http.StatusBadRequest, model.ErrTypeInvalidRequest, errTransform.Error())
		return nil, nil, cooldown.Candidate{}, false
	}
	httpResp, errDo := pvd.Client().Do(providerReq)
	if errDo != nil {
		h.recordErrorAudit(r, nil, req.Model, http.StatusBadGateway, errDo, start, req.Messages)
		model.WriteJSONError(w, http.StatusBadGateway, model.ErrTypeAllProvidersFail, errDo.Error())
		return nil, nil, cooldown.Candidate{}, false
	}
	if httpResp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		cleanMsg := sanitizeProviderErrorMessage(string(bodyBytes))
		statusCode := httpResp.StatusCode
		errType := model.ErrTypeProviderError
		if statusCode == http.StatusTooManyRequests {
			errType = model.ErrTypeInsufficientQuota
		}
		h.recordErrorAudit(r, nil, req.Model, statusCode, fmt.Errorf("%s", cleanMsg), start, req.Messages)
		model.WriteJSONError(w, statusCode, errType, cleanMsg)
		return nil, nil, cooldown.Candidate{}, false
	}
	return httpResp, pvd, firstCand, true
}

// writeNonStreamingResponse transforms the provider's raw HTTP response
// into a chat-completion response, writes it (plus cost/usage headers) to
// w, and records the audit trail. Used when req.Stream is false.
func (h *Handler) writeNonStreamingResponse(ctx context.Context, w http.ResponseWriter, r *http.Request, resp *http.Response, finalRoute *smartrouter.Route, req *model.ChatCompletionRequest, start time.Time) {
	defer func() { _ = resp.Body.Close() }()

	chatResp, err := finalRoute.Provider.TransformResponse(ctx, resp)
	if err != nil {
		statusCode := providerErrorStatus(err)
		errType := model.ErrTypeProviderError
		if statusCode == http.StatusTooManyRequests {
			errType = model.ErrTypeInsufficientQuota
		}
		h.recordErrorAudit(r, finalRoute, req.Model, statusCode, err, start, req.Messages)
		model.WriteJSONError(w, statusCode, errType, fmt.Sprintf("failed to parse provider response: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	actualModel := finalRoute.Model.Name
	if finalRoute.Provider != nil {
		actualModel = finalRoute.Provider.Name() + "/" + finalRoute.Model.Name
	}
	w.Header().Set("X-Ilter-Model-Actual", actualModel)
	if chatResp.Usage != nil {
		actualCost := CalculateCost(finalRoute.Model, chatResp.Usage)
		chatResp.Usage.IlterCost = actualCost
		u := chatResp.Usage
		cachedTokens := 0
		if u.PromptTokensDetails != nil {
			cachedTokens = u.PromptTokensDetails.CachedTokens
		}
		inputTokens := u.PromptTokens
		if u.CacheReadIncludedInPrompt {
			inputTokens = max(u.PromptTokens-cachedTokens, 0)
		}
		setPostResponse(w, postResponse{
			Model:               finalRoute.Model.Name,
			PromptTokens:        u.PromptTokens,
			CompletionTokens:    u.CompletionTokens,
			CachedTokens:        cachedTokens,
			CacheCreationTokens: u.CacheCreationInputTokens,
			InputCost:           math.Round(float64(inputTokens)*finalRoute.Model.CostPerInputToken*1e6) / 1e6,
			CachedCost:          math.Round(float64(cachedTokens)*finalRoute.Model.CostPerCachedInputToken*1e6) / 1e6,
			CacheWriteCost:      math.Round(float64(u.CacheCreationInputTokens)*finalRoute.Model.CostPerCacheWriteToken*1e6) / 1e6,
			OutputCost:          math.Round(float64(u.CompletionTokens)*finalRoute.Model.CostPerOutputToken*1e6) / 1e6,
			ActualCost:          actualCost,
		}, h.emitStandard())
		w.Header().Set("X-Ilter-Cost", strconv.FormatFloat(math.Round(actualCost*1e6)/1e6, 'f', -1, 64))
	}
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(chatResp); err != nil {
		slog.Debug("encode response error", "error", err)
	}

	h.recordAudit(r, finalRoute, req.Model, chatResp, http.StatusOK, start, false, req.Messages)
	h.recordPostResponse(r, chatResp, finalRoute, start)
}

// requestLogEnabled reports whether request *contents* (prompt preview and
// request/response bodies) should be written to the audit log. The runtime
// feature:request_log flag (hot) overrides the boot Audit.Enabled default;
// when disabled, aggregate rows (tokens, cost, latency, status) are still
// written but content fields are blanked.
func (h *Handler) requestLogEnabled() bool {
	if h.configCache != nil && h.configCache.Get() != nil {
		return config.IsEnabled(h.configCache, "request_log")
	}
	return h.cfg != nil && h.cfg.Audit.Enabled
}

// buildAuditPromptPreview truncates the last message's text content to
// 200 chars for the audit log, if prompt logging is enabled.
func (h *Handler) buildAuditPromptPreview(messages []model.Message) string {
	if !h.requestLogEnabled() || h.cfg == nil || !h.cfg.Audit.LogPrompts || len(messages) == 0 {
		return ""
	}
	lastMsg := messages[len(messages)-1]
	contentStr, ok := lastMsg.Content.(string)
	if !ok {
		return ""
	}
	if len(contentStr) > 200 {
		return contentStr[:200]
	}
	return contentStr
}

// buildAuditRequestBody JSON-encodes messages for the audit log, if body
// logging is enabled.
func (h *Handler) buildAuditRequestBody(messages []model.Message) string {
	if !h.requestLogEnabled() || h.cfg == nil || !h.cfg.Audit.LogBodies || len(messages) == 0 {
		return ""
	}
	b, err := json.Marshal(map[string]any{"messages": messages})
	if err != nil {
		return ""
	}
	return string(b)
}

// buildAuditResponseBody JSON-encodes chatResp for the audit log, if body
// logging is enabled.
func (h *Handler) buildAuditResponseBody(chatResp *model.ChatCompletionResponse) string {
	if !h.requestLogEnabled() || h.cfg == nil || !h.cfg.Audit.LogBodies || chatResp == nil {
		return ""
	}
	b, err := json.Marshal(chatResp)
	if err != nil {
		return ""
	}
	return string(b)
}

// requestComplexityScore reads the request-scoped complexity score
// computed earlier in the pipeline, if any.
func requestComplexityScore(ctx context.Context) float64 {
	meta := reqmeta.GetRequestMetadata(ctx)
	if meta == nil {
		return 0
	}
	var score float64
	meta.WithLock(func() {
		score = meta.ComplexityScore
	})
	return score
}

func (h *Handler) recordAudit(
	r *http.Request,
	route *smartrouter.Route,
	requestedModel string,
	chatResp *model.ChatCompletionResponse,
	statusCode int,
	start time.Time,
	cacheHit bool,
	messages []model.Message,
) {
	if h.auditLogger == nil {
		return
	}

	promptTokens := 0
	completionTokens := 0
	cachedTokens := 0
	cacheCreationTokens := 0
	var usage *model.Usage
	if chatResp != nil && chatResp.Usage != nil {
		usage = chatResp.Usage
		promptTokens = usage.PromptTokens
		completionTokens = usage.CompletionTokens
		if usage.PromptTokensDetails != nil {
			cachedTokens = usage.PromptTokensDetails.CachedTokens
		}
		cacheCreationTokens = usage.CacheCreationInputTokens
	}

	cost := CalculateCost(route.Model, usage)
	latencyMs := int(time.Since(start) / time.Millisecond)

	h.auditLogger.LogAsync(middleware.AuditLogEntry{
		IPAddress:           extractClientIP(r),
		KeyID:               reqmeta.GetKeyID(r.Context()),
		Model:               requestedModel,
		Provider:            route.Provider.Name(),
		PromptTokens:        promptTokens,
		CompletionTokens:    completionTokens,
		CachedTokens:        cachedTokens,
		CacheCreationTokens: cacheCreationTokens,
		TotalCost:           cost,
		LatencyMs:           latencyMs,
		StatusCode:          statusCode,
		CacheHit:            cacheHit,
		PromptPreview:       h.buildAuditPromptPreview(messages),
		RequestBody:         h.buildAuditRequestBody(messages),
		ResponseBody:        h.buildAuditResponseBody(chatResp),
		ComplexityScore:     requestComplexityScore(r.Context()),
	})
}

// recordErrorAudit records an audit log entry for error paths in ChatCompletions.
// It handles partial information (nil route, nil messages) gracefully.
func (h *Handler) recordErrorAudit(
	r *http.Request,
	route *smartrouter.Route,
	requestedModel string,
	statusCode int,
	err error,
	start time.Time,
	messages []model.Message,
) {
	if h.auditLogger == nil {
		return
	}

	var providerName string
	if route != nil && route.Provider != nil {
		providerName = route.Provider.Name()
	}

	var responseBody string
	if h.requestLogEnabled() && h.cfg != nil && h.cfg.Audit.LogBodies && err != nil {
		responseBody = err.Error()
	}

	latencyMs := int(time.Since(start) / time.Millisecond)

	h.auditLogger.LogAsync(middleware.AuditLogEntry{
		IPAddress:        extractClientIP(r),
		KeyID:            reqmeta.GetKeyID(r.Context()),
		Model:            requestedModel,
		Provider:         providerName,
		PromptTokens:     0,
		CompletionTokens: 0,
		TotalCost:        0,
		LatencyMs:        latencyMs,
		StatusCode:       statusCode,
		CacheHit:         false,
		PromptPreview:    h.buildAuditPromptPreview(messages),
		RequestBody:      h.buildAuditRequestBody(messages),
		ResponseBody:     responseBody,
		ComplexityScore:  requestComplexityScore(r.Context()),
	})
}

// extractClientIP resolves the client IP from X-Forwarded-For, X-Real-IP, or RemoteAddr.
func extractClientIP(r *http.Request) string {
	clientIP := r.RemoteAddr
	if idx := strings.LastIndex(clientIP, ":"); idx != -1 {
		clientIP = clientIP[:idx]
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	return clientIP
}

func (h *Handler) Models(w http.ResponseWriter, _ *http.Request) {
	infos := h.lb.GetAvailableModelInfos()

	data := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		// Ids are provider-prefixed ("provider/model") so clients that list
		// models can pin a provider by echoing the id straight back into a
		// chat request. provider/owned_by/type entries stay separate fields
		// for OpenAI-compatible clients.
		data = append(data, map[string]any{
			"id":       info.Provider + "/" + info.Name,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": info.OwnedBy,
			"provider": info.Provider,
			"type":     info.Type,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	}); err != nil {
		slog.Debug("encode models error", "error", err)
	}
}
