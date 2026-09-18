package dashboard

// This file re-exports types from subpackages for test compilation.
// Tests in this package reference types without qualifiers.

import (
	"github.com/ilter-ai/ilter/internal/dashboard/features"
	"github.com/ilter-ai/ilter/internal/dashboard/models"
	"github.com/ilter-ai/ilter/internal/dashboard/providers"
	"github.com/ilter-ai/ilter/internal/dashboard/smartrouter"
	"github.com/ilter-ai/ilter/internal/dashboard/stats"
)

// StatsResponse and CircuitBreakerSummaryResponse re-export the stats
// package's response types for test compilation.
type (
	StatsResponse                 = stats.Response
	CircuitBreakerSummaryResponse = stats.CircuitBreakerSummaryResponse
)

// FeatureItem re-exports the features package's item type for test
// compilation.
type FeatureItem = features.FeatureItem

// ModelResponseItem and related request types re-export the models
// package's types for test compilation.
type (
	ModelResponseItem          = models.ModelResponseItem
	ToggleModelRequest         = models.ToggleModelRequest
	UpdateModelCategoryRequest = models.UpdateModelCategoryRequest
)

// ProviderSummary re-exports the providers package's summary type for test
// compilation.
type ProviderSummary = providers.ProviderSummary

// OptimizeRequest and related smart router types re-export the smartrouter
// package's types for test compilation.
type (
	OptimizeRequest            = smartrouter.OptimizeRequest
	OptimizeResponse           = smartrouter.OptimizeResponse
	SmartRouterStatsResponse   = smartrouter.StatsResponse
	SmartRouterHistoryResponse = smartrouter.HistoryResponse
)

// PIIExportItem and related PII types alias this package's own PII item
// types for test compilation.
type (
	PIIExportItem = ExportItem
	PIIEventItem  = EventItem
	PIIStats      = Stats
)

// GuardrailViolationsResponse is a page of guardrail event items.
type GuardrailViolationsResponse = Page[GuardrailEventItem]
