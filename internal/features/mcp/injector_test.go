package mcp

import (
	"encoding/json"
	"testing"

	"github.com/ilter-ai/ilter/internal/config"
)

func TestConvertTool_WithSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`)
	td := ToolDefinition{
		Name:        "my-tool",
		Description: "Does something",
		InputSchema: schema,
	}
	tool := convertTool(td)
	if tool.Type != "function" {
		t.Fatalf("expected function type, got %s", tool.Type)
	}
	if tool.Function.Name != "my-tool" {
		t.Fatalf("expected my-tool, got %s", tool.Function.Name)
	}
	params := tool.Function.Parameters
	if params["type"] != "object" {
		t.Fatalf("expected type=object, got %v", params["type"])
	}
	props, ok := params["properties"].(map[string]any)
	if !ok || props["name"] == nil {
		t.Fatal("expected properties with name field")
	}
}

func TestConvertTool_EmptySchema(t *testing.T) {
	td := ToolDefinition{Name: "empty-schema"}
	tool := convertTool(td)
	params := tool.Function.Parameters
	if params["type"] != "object" {
		t.Fatalf("expected type=object, got %v", params["type"])
	}
}

func TestConvertTool_InvalidSchema(t *testing.T) {
	// Invalid JSON schema → falls back to empty object schema.
	td := ToolDefinition{
		Name:        "bad-schema",
		InputSchema: json.RawMessage(`{invalid}`),
	}
	tool := convertTool(td)
	params := tool.Function.Parameters
	if params["type"] != "object" {
		t.Fatalf("expected fallback type=object, got %v", params["type"])
	}
}

func TestConvertTool_MissingType(t *testing.T) {
	// Schema missing "type" field → should be injected.
	schema := json.RawMessage(`{"properties":{"x":{}}}`)
	td := ToolDefinition{Name: "no-type", InputSchema: schema}
	tool := convertTool(td)
	params := tool.Function.Parameters
	if params["type"] != "object" {
		t.Fatalf("expected injected type=object, got %v", params["type"])
	}
}

func TestGetAuthorizedOpenAITools_Empty(t *testing.T) {
	reg := &Registry{servers: map[string]*ServerInfo{}}
	auth := NewAuthorizer(nil, nil, "deny")
	inj := NewInjector(reg, auth, nil)

	tools := inj.GetAuthorizedOpenAITools("", nil)
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools, got %d", len(tools))
	}
}

func TestGetAuthorizedOpenAITools_AllAuthorized(t *testing.T) {
	reg := &Registry{
		servers: map[string]*ServerInfo{
			"s1": {
				ID: "s1", Config: config.MCPServerConfig{ID: "s1"},
				Tools: []ToolDefinition{
					{Name: "alpha", Description: "Alpha tool"},
					{Name: "beta", Description: "Beta tool"},
				},
			},
		},
	}
	// Wildcard rule authorizes everything.
	auth := NewAuthorizer(nil, []config.MCPAccessRule{
		{Tools: []string{"*/*"}},
	}, "deny")
	inj := NewInjector(reg, auth, nil)

	tools := inj.GetAuthorizedOpenAITools("", nil)
	if len(tools) != 2 {
		t.Fatalf("expected 2 authorized tools, got %d", len(tools))
	}
	// Every injected name is server-prefixed (empty server name falls back
	// to the server ID).
	if tools[0].Function.Name != "s1-alpha" {
		t.Fatalf("expected first tool 's1-alpha', got %s", tools[0].Function.Name)
	}
	if tools[1].Function.Name != "s1-beta" {
		t.Fatalf("expected second tool 's1-beta', got %s", tools[1].Function.Name)
	}
}

func TestGetAuthorizedOpenAITools_PrefixedNames(t *testing.T) {
	reg := &Registry{
		servers: map[string]*ServerInfo{
			"s1": {
				ID: "s1", Config: config.MCPServerConfig{ID: "s1"},
				Tools: []ToolDefinition{
					{Name: "fetch", Description: "Fetch from s1"},
					{Name: "unique-s1", Description: "Only on s1"},
				},
			},
			"s2": {
				ID: "s2", Config: config.MCPServerConfig{ID: "s2"},
				Tools: []ToolDefinition{
					{Name: "fetch", Description: "Fetch from s2"},
				},
			},
		},
	}
	auth := NewAuthorizer(nil, []config.MCPAccessRule{
		{Tools: []string{"*/*"}},
	}, "deny")
	inj := NewInjector(reg, auth, nil)

	tools := inj.GetAuthorizedOpenAITools("", nil)
	if len(tools) != 3 {
		t.Fatalf("expected 3 authorized tools, got %d", len(tools))
	}
	names := make(map[string]bool, len(tools))
	for _, tl := range tools {
		names[tl.Function.Name] = true
	}
	// Every injected name is server-prefixed (empty server name falls back
	// to the server ID), including "fetch" offered by both servers.
	if !names["s1-fetch"] || !names["s2-fetch"] {
		t.Fatalf("expected prefixed s1-fetch/s2-fetch, got %v", names)
	}
	// "unique-s1" is also prefixed.
	if !names["s1-unique-s1"] {
		t.Fatalf("expected prefixed 's1-unique-s1', got %v", names)
	}
}

func TestGetAuthorizedOpenAITools_PartialAuth(t *testing.T) {
	reg := &Registry{
		servers: map[string]*ServerInfo{
			"s1": {
				ID: "s1", Config: config.MCPServerConfig{ID: "s1"},
				Tools: []ToolDefinition{
					{Name: "public", Description: "Public"},
					{Name: "secret", Description: "Secret"},
				},
			},
		},
	}
	// Only authorize "public".
	auth := NewAuthorizer(nil, []config.MCPAccessRule{
		{Tools: []string{"public"}},
	}, "deny")
	inj := NewInjector(reg, auth, nil)

	tools := inj.GetAuthorizedOpenAITools("", nil)
	if len(tools) != 1 {
		t.Fatalf("expected 1 authorized tool, got %d", len(tools))
	}
	// Every injected name is server-prefixed.
	if tools[0].Function.Name != "s1-public" {
		t.Fatalf("expected 's1-public', got %s", tools[0].Function.Name)
	}
}

func TestResolveKeyPrefix_Admin(t *testing.T) {
	inj := NewInjector(nil, nil, nil)
	prefix := inj.resolveKeyPrefix("")
	if prefix != "" {
		t.Fatalf("expected empty prefix for admin, got %s", prefix)
	}
}

func TestResolveKeyPrefix_NilStore(t *testing.T) {
	inj := NewInjector(nil, nil, nil)
	prefix := inj.resolveKeyPrefix("42")
	if prefix != "" {
		t.Fatalf("expected empty prefix when store is nil, got %s", prefix)
	}
}
