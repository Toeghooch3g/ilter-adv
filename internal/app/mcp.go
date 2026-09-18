package app

import (
	"context"
	"log/slog"
	"strings"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/config/openapi"
	dashopenapi "github.com/ilter-ai/ilter/internal/dashboard"
	"github.com/ilter-ai/ilter/internal/features/mcp"
	"github.com/ilter-ai/ilter/internal/features/mcp/toolpricing"
	iltermiddleware "github.com/ilter-ai/ilter/internal/middleware"
	"github.com/ilter-ai/ilter/internal/model"
	"github.com/ilter-ai/ilter/internal/model/catalog"
	mcptransport "github.com/ilter-ai/ilter/internal/platform/transport/mcp"
)

// initMCP initializes the MCP gateway, OpenAPI tool provider, injection middleware, and hub.
func (a *App) initMCP() {
	cfg := a.cfg

	mcpRegistry, err := mcp.NewRegistryFromCache(a.cfgCache.Get().MCPServers(), a.store)
	if err != nil {
		slog.Error("Failed to initialize MCP registry", "error", err)
		return
	}
	a.mcpHandler.SetRegistry(mcpRegistry)
	blockedToolsFn := func() []string { return a.cfgCache.Get().MCPBlockedTools }
	mcpRegistry.SetBlockedToolsFn(blockedToolsFn)
	mcpAuthorizer := mcp.NewAuthorizer(a.store, nil, cfg.MCP.DefaultPolicy)
	mcpClients := mcp.NewClientManager(mcpRegistry)
	a.mcpExecutor = mcp.NewExecutor(mcpRegistry, mcpClients, mcpAuthorizer, a.mcpAuditLogger, a.store.DB)
	a.mcpExecutor.SetBlockedToolsFn(blockedToolsFn)

	// Tool pricing: rules live in the runtime_config "tool_pricing" section.
	// The resolver is rebuilt on every config-cache refresh (same pattern as
	// the guardrails middleware) so pricing edits apply without restart.
	var pricing *toolpricing.Resolver
	rebuildPricing := func() {
		rules, err := toolpricing.Load(context.Background(), a.store)
		if err != nil {
			slog.Warn("toolpricing: failed to load pricing rules; tool calls unbilled", "error", err)
			pricing = nil
			return
		}
		pricing = toolpricing.NewResolver(rules)
	}
	rebuildPricing()
	a.cfgCache.OnChange(func(*config.Snapshot) { rebuildPricing() })
	a.mcpExecutor.SetPricingResolver(func() *toolpricing.Resolver { return pricing })

	// Bill priced tool calls against the caller's key budget and usage_daily.
	if a.budgetMiddleware != nil {
		a.mcpExecutor.SetBudgetRecorder(func(ctx context.Context, keyID string, cost float64) {
			if err := a.budgetMiddleware.Enforcer().RecordUsage(ctx, keyID, cost); err != nil {
				slog.Warn("mcp tool budget record failed", "key_id", keyID, "cost", cost, "error", err)
			}
		})
	}
	a.mcpExecutor.SetUsageRecorder(func(ctx context.Context, keyID, serverID, toolName string, cost float64) {
		if a.store == nil {
			return
		}
		if err := a.store.RecordMCPToolUsage(ctx, keyID, serverID, toolName, cost); err != nil {
			slog.Warn("mcp tool usage record failed", "key_id", keyID, "tool", serverID+":"+toolName, "error", err)
		}
	})
	mcpGateway := mcp.NewGateway(mcpRegistry, mcpAuthorizer, a.mcpAuditLogger, a.store, &cfg.MCP, a.mcpExecutor)
	mcpGateway.SetConfigCache(a.cfgCache)

	a.mcpGatewayHandler = mcptransport.NewGatewayHandler(mcpGateway)
	a.mcpGatewayHandler.SetConfigCache(a.cfgCache)

	mcpInjector := mcp.NewInjector(mcpRegistry, mcpAuthorizer, a.store)
	mcpToolCallExecutor := mcp.NewToolCallExecutor(a.mcpExecutor)

	slog.Info(
		"MCP Gateway initialized",
		"endpoint", cfg.MCP.Endpoint,
		"servers", len(mcpRegistry.ListServers()),
		"injection", cfg.MCP.Injection.Enabled,
	)

	if cfg.MCP.HubEndpoint != "" {
		a.mcpHubHandler = mcptransport.NewHubHandler(
			mcp.NewHub(mcpRegistry, mcpAuthorizer, a.mcpExecutor, a.store, &cfg.MCP),
			mcp.NewSessionManager(),
		)
		slog.Info("MCP Hub enabled", "hub_endpoint", cfg.MCP.HubEndpoint)
	}

	// Initialize OpenAPI ToolProvider from database
	var openapiProvider *openapi.ToolProvider
	openAPISpecs, specErr := dashopenapi.LoadEnabledOpenAPISpecs(a.store)
	if specErr != nil {
		slog.Warn("Failed to load OpenAPI specs for tool injection", "error", specErr)
	} else {
		provider, initErr := openapi.NewToolProvider(openAPISpecs)
		if initErr != nil {
			slog.Error("Failed to initialize OpenAPI ToolProvider", "error", initErr)
		} else {
			provider.AdminKey = a.cfg.Auth.AdminKey

			openapiProvider = provider
			mcpGateway.SetOpenAPIProvider(openapiProvider)
			if a.openapiHandler != nil {
				a.openapiHandler.SetProvider(openapiProvider)
			}
			slog.Info("OpenAPI ToolProvider initialized", "enabled_specs", len(openAPISpecs))
		}

	}

	// Combine MCP tools and OpenAPI tools into ProviderSet
	// MCP: match tools that can be resolved by the registry (namespaced or bare without conflicts)
	mcpProv := mcp.Provider{
		Name:   "mcp",
		Prefix: "",
		Match: func(name string) bool {
			// If registry can resolve it, it's an MCP tool
			_, _, resolveErr := mcpRegistry.ResolveTool(name)
			return resolveErr == nil
		},
		Tools: func(keyID string, groupIDs []int) []model.Tool {
			if !iltermiddleware.IsEnabled(a.cfgCache, "mcp") || mcpInjector == nil {
				return nil
			}
			return mcpInjector.GetAuthorizedOpenAITools(keyID, groupIDs)
		},
		Execute: func(ctx context.Context, keyID, keyPrefix string, calls []model.ToolCall) ([]model.Message, []bool) {
			if !iltermiddleware.IsEnabled(a.cfgCache, "mcp") || mcpToolCallExecutor == nil {
				return nil, nil
			}
			return mcpToolCallExecutor.ExecuteToolCalls(ctx, keyID, keyPrefix, calls)
		},
	}

	openapiProv := mcp.Provider{
		Name:   "openapi",
		Prefix: "openapi_",
		Match: func(name string) bool {
			return strings.HasPrefix(name, "openapi_")
		},
		Tools: func(keyID string, groupIDs []int) []model.Tool {
			if !iltermiddleware.IsEnabled(a.cfgCache, "openapi") || openapiProvider == nil {
				return nil
			}
			return openapiProvider.GetAuthorizedTools(keyID, groupIDs)
		},
		Execute: func(ctx context.Context, keyID, keyPrefix string, calls []model.ToolCall) ([]model.Message, []bool) {
			if !iltermiddleware.IsEnabled(a.cfgCache, "openapi") || openapiProvider == nil {
				return nil, nil
			}
			return openapiProvider.Execute(ctx, keyID, keyPrefix, calls)
		},
	}

	providerSet, err := mcp.New(mcpProv, openapiProv)
	if err != nil {
		slog.Error("Failed to create MCP ProviderSet", "error", err)
		return
	}

	// Server-side tool injection is a per-key opt-in (api_keys
	// mcp_injection_enabled). Synthetic keys (dashboard/admin) and keys
	// without the flag never get tools injected; the flag gates both the MCP
	// and OpenAPI providers at this single choke point.
	injectFn := func(keyID string, groupIDs []int) []model.Tool {
		if !a.mcpInjectionAllowed(keyID) {
			return nil
		}
		return providerSet.Inject(keyID, groupIDs)
	}

	a.mcpInjectMiddleware = iltermiddleware.NewMCPMiddleware(
		a.cfgCache,
		injectFn,
		providerSet.Execute,
		a.piiMaskerMiddleware,
	)

	if a.guardrailsMiddleware != nil {
		a.mcpInjectMiddleware.SetGuardrailsChecker(a.guardrailsMiddleware.Checker())
	}

	a.mcpInjectMiddleware.SetSupportsToolsFn(catalog.ModelSupportsTools)
}

// mcpInjectionAllowed reports whether server-side MCP/OpenAPI tool injection
// may run for keyID. It is a per-key opt-in: empty and synthetic (admin,
// dev:*) key IDs are never allowed, and the key must exist and have
// mcp_injection_enabled set.
func (a *App) mcpInjectionAllowed(keyID string) bool {
	if keyID == "" || mcp.IsSyntheticKeyID(keyID) {
		return false
	}
	vk, err := a.store.GetAPIKey(context.Background(), keyID)
	return err == nil && vk.MCPInjectionEnabled
}
