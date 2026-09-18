package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v7"
	"github.com/sony/gobreaker/v2"

	"github.com/ilter-ai/ilter/internal/features/mcp/toolpricing"
)

// Executor resolves the server for a tool call, delegates to the appropriate
// TransportClient, and applies retry/backoff + circuit circuitbreaker.
type Executor struct {
	registry        *Registry
	clients         *ClientManager
	authorizer      *Authorizer
	auditLog        *AuditLogger
	db              *sql.DB
	toolConfigCache sync.Map // toolName → *ToolConfig
	rateLimits      sync.Map // toolName → *rateLimitWindow

	// blockedToolsFn returns the current blocked-tool list (same closure as
	// Registry.SetBlockedToolsFn). Nil disables execution-time blocking.
	blockedToolsFn func() []string

	// pricingFn returns the current tool-pricing resolver; nil disables cost
	// accounting (un-priced tools always cost 0).
	pricingFn func() *toolpricing.Resolver
	// budgetRecorder bills the computed cost against the caller's key budget
	// (budget.Enforcer.RecordUsage); nil disables budget recording.
	budgetRecorder func(ctx context.Context, keyID string, cost float64)
	// usageRecorder records one usage_daily row per priced call (provider
	// "mcp"); nil disables usage_daily recording.
	usageRecorder func(ctx context.Context, keyID, serverID, toolName string, cost float64)
}

// SetPricingResolver installs the closure returning the current pricing
// resolver (rebuilt on config-cache changes).
func (ex *Executor) SetPricingResolver(fn func() *toolpricing.Resolver) {
	ex.pricingFn = fn
}

// SetBudgetRecorder installs the budget recording callback.
func (ex *Executor) SetBudgetRecorder(fn func(ctx context.Context, keyID string, cost float64)) {
	ex.budgetRecorder = fn
}

// SetUsageRecorder installs the usage_daily recording callback.
func (ex *Executor) SetUsageRecorder(fn func(ctx context.Context, keyID, serverID, toolName string, cost float64)) {
	ex.usageRecorder = fn
}

// computeCost resolves the priced cost of a successful tool call (0 when no
// rule matches or pricing is disabled). A per-tool dashboard cost (registry
// toolCosts) takes precedence over the tool_pricing runtime rules for the
// exact (server, tool); both fall back to 0 for unpriced tools.
func (ex *Executor) computeCost(serverID, toolName string, args json.RawMessage) float64 {
	if cost := ex.registry.ToolCost(serverID, toolName); cost > 0 {
		return cost
	}
	if ex.pricingFn == nil {
		return 0
	}
	r := ex.pricingFn()
	if r == nil {
		return 0
	}
	return r.CostFor(serverID, toolName, args)
}

// SetBlockedToolsFn installs the closure the executor consults before every
// tool call (defense in depth behind the registry's ListTools filter, so a
// client that guesses a blocked tool's name is still rejected).
func (ex *Executor) SetBlockedToolsFn(fn func() []string) {
	ex.blockedToolsFn = fn
}

// isBlocked reports whether the server's tool matches the blocked-tools list.
// Entries match a tool's server-prefixed exposed name
// (ExposedToolName(server.Config.Name, server.ID, toolName)) exactly; bare
// names and legacy "server__tool" forms no longer match.
func (ex *Executor) isBlocked(server *ServerInfo, toolName string) bool {
	if ex.blockedToolsFn == nil {
		return false
	}
	blocked := ex.blockedToolsFn()
	if len(blocked) == 0 {
		return false
	}
	return slices.Contains(blocked, ExposedToolName(server.Config.Name, server.ID, toolName))
}

func NewExecutor(registry *Registry, clients *ClientManager, authorizer *Authorizer, auditLog *AuditLogger, db *sql.DB) *Executor {
	return &Executor{
		registry:   registry,
		clients:    clients,
		authorizer: authorizer,
		auditLog:   auditLog,
		db:         db,
	}
}

// ExecuteToolParams carries all context needed by ExecuteTool.
type ExecuteToolParams struct {
	ToolName  string
	Arguments json.RawMessage
	APIKeyID  string
	KeyPrefix string
	ClientIP  string
}

// ExecuteTool finds the server that owns toolName, obtains a transport client,
// dispatches a tools/call JSON-RPC request, and returns the result.
func (ex *Executor) ExecuteTool(ctx context.Context, p *ExecuteToolParams) *CallToolResult {
	start := time.Now()

	// 1. Look up the tool in the catalog.
	tool, server, err := ex.registry.ResolveTool(p.ToolName)
	if err != nil {
		return errorResult(p.ToolName, err.Error())
	}

	// 1b. Blocked-tool policy (defense in depth: the registry's ListTools
	// filter hides blocked tools from every client surface; this rejects a
	// client that guesses a blocked tool's name anyway).
	if ex.isBlocked(server, tool.Name) {
		ex.logAudit(ctx, p, tool.Name, server.ID, start, 403, false, "blocked", 0)
		return errorResult(p.ToolName, "tool blocked by gateway policy")
	}

	// 1c. Admin-disabled tool policy (same defense-in-depth pattern: the
	// registry hides the tool from ListTools; reject a direct guess here).
	if ex.registry.IsToolDisabled(server.ID, tool.Name) {
		ex.logAudit(ctx, p, tool.Name, server.ID, start, 403, false, "disabled", 0)
		return errorResult(p.ToolName, "tool is disabled")
	}

	// 2. Check access (with resolved server and bare tool name).
	if blocked := ex.checkToolAccess(ctx, p, tool, server, start); blocked != nil {
		return blocked
	}

	// 3. Security checks (destructive, confirmation, rate limit).
	tc := ex.getToolConfig(p.ToolName)
	if blocked := ex.checkToolPolicy(ctx, p, tool, server, tc, start); blocked != nil {
		return blocked
	}

	// 4. Build the JSON-RPC request.
	params := &CallToolParams{
		Name:      tool.Name,
		Arguments: p.Arguments,
	}

	req := &JSONRPCRequest{
		JSONRPC: JSONRPCVersion,
		Method:  "tools/call",
		Params:  *toRawMessage(params),
	}
	id := json.RawMessage(fmt.Sprintf(`"%d"`, time.Now().UnixNano()))
	req.ID = &id

	// 5. Obtain a transport client.
	client, err := ex.clients.GetOrCreate(ctx, server)
	if err != nil {
		ex.logAudit(ctx, p, tool.Name, server.ID, start, 500, false, err.Error(), 0)
		return errorResult(p.ToolName, fmt.Sprintf("Failed to connect to server %q: %v", server.Config.Name, err))
	}

	// 6. Execute with circuit breaker + retry.
	timeout := parseDurationOrDefault(server.Config.Timeout, 30*time.Second)
	if tc != nil && tc.TimeoutMs > 0 {
		timeout = time.Duration(tc.TimeoutMs) * time.Millisecond
	}
	maxRetries := server.Config.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 1
	}
	maxRetries++ // at least one attempt

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := ex.callWithRetry(callCtx, client, req, tool.Name, server.ID, maxRetries)
	if err != nil {
		ex.logAudit(ctx, p, tool.Name, server.ID, start, 500, false, err.Error(), 0)
		return errorResult(p.ToolName, fmt.Sprintf("Tool call failed after %d attempt(s): %v", maxRetries, err))
	}

	if resp.Error != nil {
		ex.logAudit(ctx, p, tool.Name, server.ID, start, 200, false, resp.Error.Message, 0)
		return &CallToolResult{
			IsError: true,
			Content: []ToolContent{{Type: "text", Text: resp.Error.Message}},
		}
	}

	// Cost accounting runs only on the success path (status 200, success=true):
	// failed, blocked, and rate-limited calls cost nothing.
	cost := ex.computeCost(server.ID, tool.Name, p.Arguments)
	if cost > 0 {
		if ex.budgetRecorder != nil {
			ex.budgetRecorder(ctx, p.APIKeyID, cost)
		}
		if ex.usageRecorder != nil {
			ex.usageRecorder(ctx, p.APIKeyID, server.ID, tool.Name, cost)
		}
	}

	ex.logAudit(ctx, p, tool.Name, server.ID, start, 200, true, "", cost)

	return parseToolResult(resp.Result)
}

// checkToolAccess applies the Authorizer's allow/deny decision for p against
// the resolved tool and server, logging an audit entry and returning a
// blocked *CallToolResult if access is denied (nil if the call may
// proceed). Extracted from ExecuteTool to keep its cognitive complexity
// down; behavior is unchanged.
func (ex *Executor) checkToolAccess(ctx context.Context, p *ExecuteToolParams, tool *ToolDefinition, server *ServerInfo, start time.Time) *CallToolResult {
	if ex.authorizer == nil {
		return nil
	}
	result := ex.authorizer.CheckAccess(p.KeyPrefix, nil, p.APIKeyID, server.ID, tool.Name)
	if !result.Allowed {
		ex.logAudit(ctx, p, tool.Name, server.ID, start, 403, false, "access denied", 0)
		return errorResult(p.ToolName, "Access denied by MCP access rules")
	}
	return nil
}

// checkToolPolicy applies the per-tool security policy (destructive,
// requires-confirmation, rate limit) carried by tc, logging an audit entry
// and returning a blocked *CallToolResult if the call is disallowed (nil
// if it may proceed). Extracted from ExecuteTool to keep its cognitive
// complexity down; behavior is unchanged.
func (ex *Executor) checkToolPolicy(ctx context.Context, p *ExecuteToolParams, tool *ToolDefinition, server *ServerInfo, tc *ToolConfig, start time.Time) *CallToolResult {
	if tc == nil {
		return nil
	}
	if tc.Destructive {
		ex.logAudit(ctx, p, tool.Name, server.ID, start, 403, false, "destructive tool blocked", 0)
		return errorResult(p.ToolName, "Tool call blocked: destructive tool not allowed")
	}
	if tc.RequiresConfirmation {
		ex.logAudit(ctx, p, tool.Name, server.ID, start, 403, false, "tool requires confirmation", 0)
		return errorResult(p.ToolName, "Tool call blocked: tool requires manual confirmation")
	}
	if ex.isRateLimited(p.ToolName, tc.RateLimitRPM) {
		ex.logAudit(ctx, p, tool.Name, server.ID, start, 403, false, "rate limit exceeded", 0)
		return errorResult(p.ToolName, "Tool call blocked: rate limit exceeded")
	}
	return nil
}

// callWithRetry executes req against client through serverID's circuit
// breaker, retrying with exponential backoff until maxRetries attempts are
// exhausted or callCtx is done. Extracted from ExecuteTool to keep its
// cognitive complexity down; behavior is unchanged, including returning the
// last attempt's error (rather than a context error) when callCtx ends the
// loop early — matching the original's goto-done fallthrough.
func (ex *Executor) callWithRetry(callCtx context.Context, client TransportClient, req *JSONRPCRequest, toolName, serverID string, maxRetries int) (*JSONRPCResponse, error) {
	cb := getBreaker(serverID)

	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 100 * time.Millisecond
	b.MaxInterval = 3 * time.Second
	// no MaxElapsedTime (v7 removed it, default 0 is correct — callCtx handles deadline)

	var resp *JSONRPCResponse
	var err error

	for attempt := range maxRetries {
		if attempt > 0 {
			d := b.NextBackOff()
			mcpLog.Debug("retrying tool call",
				"tool", toolName, "server", serverID, "attempt", attempt+1, "backoff_ms", d.Milliseconds())
			select {
			case <-callCtx.Done():
				return nil, err
			case <-time.After(d):
			}
		}

		result, cbErr := cb.Execute(func() (*JSONRPCResponse, error) {
			return client.Call(callCtx, req)
		})

		if cbErr != nil {
			err = cbErr
			continue
		}
		resp = result
		err = nil
		break
	}

	return resp, err
}

func (ex *Executor) logAudit(ctx context.Context, p *ExecuteToolParams, toolName, serverID string, start time.Time, statusCode int, success bool, errMsg string, cost float64) {
	if ex.auditLog == nil {
		return
	}
	paramsStr := ""
	if p.Arguments != nil {
		if b, err := json.Marshal(p.Arguments); err == nil {
			paramsStr = string(b)
		}
	}
	ex.auditLog.LogAsync(AuditEntry{
		APIKeyID:   p.APIKeyID,
		Tool:       toolName,
		ServerID:   serverID,
		Method:     "tools/call",
		Params:     paramsStr,
		DurationMs: float64(time.Since(start).Microseconds()) / 1000.0,
		StatusCode: statusCode,
		Success:    success,
		ErrorMsg:   errMsg,
		ClientIP:   p.ClientIP,
		Cost:       cost,
	})
	if MCPToolCallsTotal != nil {
		MCPToolCallsTotal.Add(ctx, 1)
	}
}

// Circuit breaker registry
var (
	breakersMu sync.RWMutex
	breakers   = make(map[string]*gobreaker.CircuitBreaker[*JSONRPCResponse])
)

func getBreaker(serverID string) *gobreaker.CircuitBreaker[*JSONRPCResponse] {
	breakersMu.Lock()
	defer breakersMu.Unlock()
	if cb, ok := breakers[serverID]; ok {
		return cb
	}
	cb := gobreaker.NewCircuitBreaker[*JSONRPCResponse](gobreaker.Settings{
		Name:        "mcp-" + serverID,
		MaxRequests: 3,
		Interval:    10 * time.Second,
		Timeout:     30 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return counts.Requests >= 5 && failureRatio >= 0.5
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			mcpLog.Debug("circuit breaker state change",
				"name", name, "from", from, "to", to)
		},
	})
	breakers[serverID] = cb
	return cb
}

// Helpers

func toRawMessage(v any) *json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	rm := json.RawMessage(b)
	return &rm
}

func parseToolResult(raw any) *CallToolResult {
	if raw == nil {
		return &CallToolResult{
			Content: []ToolContent{{Type: "text", Text: "ok"}},
		}
	}

	// json.RawMessage is what every external MCP server sends via StdioClient/SSEClient.
	// Unmarshal it as CallToolResult first before falling back to raw text.
	if rawMsg, ok := raw.(json.RawMessage); ok && len(rawMsg) > 0 {
		var r CallToolResult
		if err := json.Unmarshal(rawMsg, &r); err == nil && len(r.Content) > 0 {
			return &r
		}
		// Not a CallToolResult object — use raw text directly (no double-marshal).
		return &CallToolResult{
			Content: []ToolContent{{Type: "text", Text: string(rawMsg)}},
		}
	}

	switch v := raw.(type) {
	case *CallToolResult:
		return v
	case map[string]any:
		b, _ := json.Marshal(v)
		var r CallToolResult
		if err := json.Unmarshal(b, &r); err == nil {
			if len(r.Content) == 0 {
				r.Content = []ToolContent{{Type: "text", Text: "ok"}}
			}
			return &r
		}
	}
	// Fallback to text representation.
	b, _ := json.Marshal(raw)
	return &CallToolResult{
		Content: []ToolContent{{Type: "text", Text: string(b)}},
	}
}

func parseDurationOrDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

func errorResult(_, msg string) *CallToolResult {
	return &CallToolResult{
		IsError: true,
		Content: []ToolContent{{Type: "text", Text: msg}},
	}
}
