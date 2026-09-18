package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ilter-ai/ilter/internal/db"

	"github.com/ilter-ai/ilter/internal/model"
)

// Injector converts MCP ToolDefinitions into OpenAI function-calling tools
// and resolves which tools a given API key is authorized to use.
type Injector struct {
	registry   *Registry
	authorizer *Authorizer
	store      *db.SQLiteStore
}

func NewInjector(registry *Registry, authorizer *Authorizer, store *db.SQLiteStore) *Injector {
	return &Injector{
		registry:   registry,
		authorizer: authorizer,
		store:      store,
	}
}

// GetAuthorizedOpenAITools returns the list of MCP tools the caller is
// allowed to use, converted to OpenAI function-calling format (model.Tool).
// keyID may be "" (admin / anonymous).  groupIDs is optional; pass nil to skip.
func (inj *Injector) GetAuthorizedOpenAITools(keyID string, groupIDs []int) []model.Tool {
	allTools := inj.registry.ListTools()
	if len(allTools) == 0 {
		return nil
	}

	keyPrefix := inj.resolveKeyPrefix(keyID)

	authorized := inj.authorizer.GetAuthorizedToolsForServers(keyPrefix, groupIDs, keyID, allTools)
	if len(authorized) == 0 {
		return nil
	}
	authSet := make(map[string]bool, len(authorized))
	for _, ti := range authorized {
		authSet[serverToolKey(ti.ServerID, ti.Tool.Name)] = true
	}

	out := buildAuthorizedOpenAITools(allTools, authSet)

	if len(out) > 0 && MCPToolsInjected != nil {
		MCPToolsInjected.Add(context.Background(), int64(len(out)))
	}

	return out
}

// serverToolKey is the per-server map key for an authorized tool. It is only
// compared against the corresponding key built in buildAuthorizedOpenAITools,
// never parsed, so the separator never needs to round-trip through a tool
// name. NUL cannot appear in tool names or server IDs (both are validated),
// which keeps the key collision-free.
func serverToolKey(serverID, toolName string) string {
	return serverID + "\x00" + toolName
}

// buildAuthorizedOpenAITools converts each tool in allTools whose
// (serverID, tool name) pair is in authSet to model.Tool. Keying the set
// per-server lets a server-qualified deny (e.g. anginxbrowser_search) hide
// a tool on only the denying server while the same bare name stays exposed
// on another server.
//
// Every injected tool name is the server-prefixed exposed form
// (ExposedToolName: "{server_name}-{tool_name}"), matching the names shown
// by the gateway's and hub's tools/list — so the LLM's tool_calls round-trip
// through Registry.ResolveTool, which resolves only prefixed names.
func buildAuthorizedOpenAITools(allTools []ToolInfo, authSet map[string]bool) []model.Tool {
	out := make([]model.Tool, 0, len(allTools))
	for _, ti := range allTools {
		if !authSet[serverToolKey(ti.ServerID, ti.Tool.Name)] {
			continue
		}
		t := convertTool(ti.Tool)
		t.Function.Name = ExposedToolName(ti.ServerName, ti.ServerID, t.Function.Name)
		out = append(out, t)
	}
	return out
}

func IsSyntheticKeyID(keyID string) bool {
	return keyID == "admin" || strings.HasPrefix(keyID, "dev:")
}

// resolveKeyPrefix queries the database for the key prefix of the given
// API key ID.  Returns "" on error, for admin/dev keys, or empty keyID.
func (inj *Injector) resolveKeyPrefix(keyID string) string {
	if keyID == "" || inj.store == nil || IsSyntheticKeyID(keyID) {
		return ""
	}
	prefix, err := ExtractKeyInfo(context.Background(), keyID, inj.store)
	if err != nil {
		mcpLog.Debug("failed to resolve key prefix", "key_id", keyID, "error", err)
		return ""
	}
	return prefix
}

func convertTool(td ToolDefinition) model.Tool {
	params := make(map[string]any)
	if len(td.InputSchema) > 0 {
		if err := json.Unmarshal(td.InputSchema, &params); err != nil {
			mcpLog.Warn("failed to parse tool input schema",
				"tool", td.Name, "error", err)
			// Fall back to an empty schema so the LLM still sees the tool.
			params = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
	} else {
		// Default to an empty object schema when none is provided.
		params = map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}

	// Ensure the top-level "type" field is set (it is required by OpenAI).
	if _, ok := params["type"]; !ok {
		params["type"] = "object"
	}

	return model.Tool{
		Type: "function",
		Function: model.ToolFunction{
			Name:        td.Name,
			Description: td.Description,
			Parameters:  params,
		},
	}
}
