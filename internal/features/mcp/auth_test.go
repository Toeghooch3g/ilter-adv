package mcp

import (
	"slices"
	"testing"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db"
	"github.com/ilter-ai/ilter/internal/model"
)

func TestMatchToolPatternWildcard(t *testing.T) {
	if !toolMatches("*/*", "anything") {
		t.Error("*/* should match anything")
	}
	if !toolMatches("*/*", "read_file") {
		t.Error("*/* should match any tool name")
	}
	if !toolMatches("*/*", "") {
		t.Error("*/* should match empty string")
	}
}

func TestMatchToolPatternExact(t *testing.T) {
	if !toolMatches("read_file", "read_file") {
		t.Error("exact match should succeed")
	}
	if toolMatches("read_file", "write_file") {
		t.Error("different names should not match")
	}
}

func TestMatchToolPatternGlob(t *testing.T) {
	if !toolMatches("read_*", "read_file") {
		t.Error("read_* should match read_file")
	}
	if !toolMatches("read_*", "read_data") {
		t.Error("read_* should match read_data")
	}
	if toolMatches("read_*", "write_file") {
		t.Error("read_* should not match write_file")
	}
}

func TestMatchToolPatternInvalid(t *testing.T) {
	if toolMatches("[invalid", "anything") {
		t.Error("invalid pattern should return false")
	}
}

func TestAuthorizerCheckAccessNoRules(t *testing.T) {
	a := NewAuthorizer(nil, nil, "deny")
	result := a.CheckAccess("", nil, "", "", "any_tool")
	if result.Allowed {
		t.Error("expected access denied with no rules")
	}
	if result.Tool != "any_tool" {
		t.Errorf("expected tool 'any_tool', got %q", result.Tool)
	}
}

func TestAuthorizerCheckAccessEmptyRules(t *testing.T) {
	a := NewAuthorizer(nil, []config.MCPAccessRule{}, "deny")
	result := a.CheckAccess("", nil, "", "", "any_tool")
	if result.Allowed {
		t.Error("expected access denied with empty rules")
	}
}

func TestAuthorizerCheckAccessWildcardRule(t *testing.T) {
	a := NewAuthorizer(nil, []config.MCPAccessRule{
		{Tools: []string{"*/*"}},
	}, "deny")
	result := a.CheckAccess("", nil, "", "", "any_tool")
	if !result.Allowed {
		t.Error("expected allowed with wildcard rule")
	}
	if result.Tool != "any_tool" {
		t.Errorf("expected tool 'any_tool', got %q", result.Tool)
	}
	if result.MatchedRule != "*/*" {
		t.Errorf("expected matched rule '*/*', got %q", result.MatchedRule)
	}
}

func TestAuthorizerCheckAccessExactRule(t *testing.T) {
	a := NewAuthorizer(nil, []config.MCPAccessRule{
		{Tools: []string{"read_file", "write_file"}},
	}, "deny")
	if r := a.CheckAccess("", nil, "", "", "read_file"); !r.Allowed {
		t.Error("expected allowed for read_file")
	}
	if r := a.CheckAccess("", nil, "", "", "write_file"); !r.Allowed {
		t.Error("expected allowed for write_file")
	}
	if r := a.CheckAccess("", nil, "", "", "delete_file"); r.Allowed {
		t.Error("expected denied for delete_file")
	}
}

func TestAuthorizerCheckAccessGlobRule(t *testing.T) {
	a := NewAuthorizer(nil, []config.MCPAccessRule{
		{Tools: []string{"read_*"}},
	}, "deny")
	if r := a.CheckAccess("", nil, "", "", "read_file"); !r.Allowed {
		t.Error("expected allowed for read_file with read_*")
	}
	if r := a.CheckAccess("", nil, "", "", "read_data"); !r.Allowed {
		t.Error("expected allowed for read_data with read_*")
	}
	if r := a.CheckAccess("", nil, "", "", "write_file"); r.Allowed {
		t.Error("expected denied for write_file with read_*")
	}
}

func TestAuthorizerCheckAccessKeyPrefixFilter(t *testing.T) {
	a := NewAuthorizer(nil, []config.MCPAccessRule{
		{KeyPrefix: "a1b2c3d4e5f6", Tools: []string{"read_file"}},
	}, "deny")
	if r := a.CheckAccess("a1b2c3d4e5f6", nil, "", "", "read_file"); !r.Allowed {
		t.Error("expected allowed when key prefix matches")
	}
	if r := a.CheckAccess("f6e5d4c3b2a1", nil, "", "", "read_file"); r.Allowed {
		t.Error("expected denied when key prefix does not match")
	}
}

func TestAuthorizerCheckAccessKeyPrefixPartialMatch(t *testing.T) {
	a := NewAuthorizer(nil, []config.MCPAccessRule{
		{KeyPrefix: "abc", Tools: []string{"read_file"}},
	}, "deny")
	if r := a.CheckAccess("abc123def456", nil, "", "", "read_file"); !r.Allowed {
		t.Error("expected allowed when key prefix has partial match")
	}
}

func TestAuthorizerCheckAccessGroupIDFilter(t *testing.T) {
	engGroup := new(1)
	a := NewAuthorizer(nil, []config.MCPAccessRule{
		{GroupID: engGroup, Tools: []string{"deploy"}},
	}, "deny")
	if r := a.CheckAccess("", []int{1}, "", "", "deploy"); !r.Allowed {
		t.Error("expected allowed when group ID matches")
	}
	if r := a.CheckAccess("", []int{2}, "", "", "deploy"); r.Allowed {
		t.Error("expected denied when group ID does not match")
	}
	if r := a.CheckAccess("", nil, "", "", "deploy"); r.Allowed {
		t.Error("expected denied when no group IDs but rule requires group")
	}
	if r := a.CheckAccess("", []int{}, "", "", "deploy"); r.Allowed {
		t.Error("expected denied when empty group IDs but rule requires group")
	}
}

func TestAuthorizerCheckAccessMultipleGroupIDs(t *testing.T) {
	devGroup := new(2)
	a := NewAuthorizer(nil, []config.MCPAccessRule{
		{GroupID: devGroup, Tools: []string{"deploy"}},
	}, "deny")
	// User in multiple groups, one matches
	if r := a.CheckAccess("", []int{1, 2, 3}, "", "", "deploy"); !r.Allowed {
		t.Error("expected allowed when one of multiple group IDs matches")
	}
	// User in multiple groups, none match
	if r := a.CheckAccess("", []int{3, 4, 5}, "", "", "deploy"); r.Allowed {
		t.Error("expected denied when none of multiple group IDs match")
	}
}

func TestAuthorizerCheckAccessKeyIDFilter(t *testing.T) {
	a := NewAuthorizer(nil, []config.MCPAccessRule{
		{KeyID: "42", Tools: []string{"admin_tool"}},
	}, "deny")
	if r := a.CheckAccess("", nil, "42", "", "admin_tool"); !r.Allowed {
		t.Error("expected allowed when key ID matches")
	}
	if r := a.CheckAccess("", nil, "99", "", "admin_tool"); r.Allowed {
		t.Error("expected denied when key ID does not match")
	}
	if r := a.CheckAccess("", nil, "", "", "admin_tool"); r.Allowed {
		t.Error("expected denied when key ID is empty but rule requires key ID")
	}
}

func TestAuthorizerCheckAccessMultipleRules(t *testing.T) {
	engGroup := new(1)
	a := NewAuthorizer(nil, []config.MCPAccessRule{
		{GroupID: engGroup, Tools: []string{"deploy", "rollback"}},
		{Tools: []string{"read_*"}},
	}, "deny")
	if r := a.CheckAccess("", []int{1}, "", "", "deploy"); !r.Allowed {
		t.Error("expected allowed via engineering group rule")
	}
	if r := a.CheckAccess("", nil, "", "", "read_file"); !r.Allowed {
		t.Error("expected allowed via catch-all glob rule")
	}
	if r := a.CheckAccess("", []int{2}, "", "", "deploy"); r.Allowed {
		t.Error("expected denied for deploy when other group doesn't match")
	}
	if r := a.CheckAccess("", nil, "", "", "write_file"); r.Allowed {
		t.Error("expected denied for write_file when no rule matches")
	}
}

func TestGetAuthorizedTools(t *testing.T) {
	a := NewAuthorizer(nil, []config.MCPAccessRule{
		{Tools: []string{"read_*", "write_file"}},
	}, "deny")
	all := []string{"read_file", "read_data", "write_file", "delete_file"}
	authorized := a.GetAuthorizedTools("", nil, "", all)

	expected := 3
	if len(authorized) != expected {
		t.Errorf("expected %d authorized tools, got %d: %v", expected, len(authorized), authorized)
	}

	forbidden := map[string]bool{"delete_file": true}
	for _, tName := range authorized {
		if forbidden[tName] {
			t.Errorf("delete_file should not be authorized")
		}
	}
}

func TestGetAuthorizedToolsNoRules(t *testing.T) {
	a := NewAuthorizer(nil, nil, "deny")
	authorized := a.GetAuthorizedTools("", nil, "", []string{"tool1", "tool2"})
	if len(authorized) != 0 {
		t.Errorf("expected 0 authorized tools with no rules, got %d", len(authorized))
	}
}

func TestGetAuthorizedToolsGroupFilter(t *testing.T) {
	engGroup := new(1)
	a := NewAuthorizer(nil, []config.MCPAccessRule{
		{GroupID: engGroup, Tools: []string{"deploy", "rollback"}},
		{Tools: []string{"read_*"}},
	}, "deny")
	all := []string{"deploy", "rollback", "read_file", "write_file"}

	// With matching group: deploy, rollback (via group rule), read_file (via catch-all glob)
	authorized := a.GetAuthorizedTools("", []int{1}, "", all)
	if len(authorized) != 3 {
		t.Errorf("expected 3 authorized tools with group match, got %d: %v", len(authorized), authorized)
	}

	// Without matching group: only the catch-all rules
	authorized = a.GetAuthorizedTools("", nil, "", all)
	if len(authorized) != 1 {
		t.Errorf("expected 1 authorized tool without group, got %d: %v", len(authorized), authorized)
	}
	if len(authorized) > 0 && authorized[0] != "read_file" {
		t.Errorf("expected only read_file, got %v", authorized)
	}
}

func TestGetAuthorizedToolsKeyPrefixAndGroup(t *testing.T) {
	devGroup := new(2)
	a := NewAuthorizer(nil, []config.MCPAccessRule{
		{KeyPrefix: "a1b2c3d4e5f6", Tools: []string{"production_tool"}},
		{GroupID: devGroup, Tools: []string{"dev_tool"}},
	}, "deny")
	all := []string{"production_tool", "dev_tool"}

	// Key prefix match, no group
	authorized := a.GetAuthorizedTools("a1b2c3d4e5f6", nil, "", all)
	if len(authorized) != 1 || authorized[0] != "production_tool" {
		t.Errorf("expected only production_tool via prefix, got %v", authorized)
	}

	// Group match, no prefix
	authorized = a.GetAuthorizedTools("", []int{2}, "", all)
	if len(authorized) != 1 || authorized[0] != "dev_tool" {
		t.Errorf("expected only dev_tool via group, got %v", authorized)
	}

	// Both match
	authorized = a.GetAuthorizedTools("a1b2c3d4e5f6", []int{2}, "", all)
	if len(authorized) != 2 {
		t.Errorf("expected 2 tools with both prefix and group match, got %d: %v", len(authorized), authorized)
	}
}

// seedServerTools returns a Registry with the given server→tools mapping,
// backed by store (so mcp_grant rows are queryable by the Authorizer).
func seedServerTools(t *testing.T, store *db.SQLiteStore, servers map[string][]ToolDefinition) *Registry {
	t.Helper()
	cfgs := make([]config.MCPServerConfig, 0, len(servers))
	for id, tools := range servers {
		cfgs = append(cfgs, config.MCPServerConfig{ID: id, Name: id, Enabled: true, Transport: "sse"})
		_ = tools
	}
	reg, err := NewRegistryFromCache(cfgs, store)
	if err != nil {
		t.Fatalf("NewRegistryFromCache: %v", err)
	}
	for id, tools := range servers {
		reg.RegisterServer(id, config.MCPServerConfig{ID: id, Name: id, Enabled: true, Transport: "sse"}, tools)
	}
	return reg
}

func oaToolNames(out []model.Tool) []string {
	names := make([]string, 0, len(out))
	for _, t := range out {
		names = append(names, t.Function.Name)
	}
	return names
}

func contains(name string, names []string) bool {
	return slices.Contains(names, name)
}

// TestServerQualifiedDenyAtInjection verifies that a server-qualified deny
// grant (subject '*', server 'anginxbrowser', tools '["search"]') is honored
// by the chat-request injection path: the denied server's tool is invisible,
// while the same bare tool name on an un-denied server stays exposed.
func TestServerQualifiedDenyAtInjection(t *testing.T) {
	store, err := db.NewSQLiteStore(config.StorageConfig{Type: "sqlite", SqlitePath: ":memory:"})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Scenario 1: 'search' exists only on anginxbrowser, which denies it.
	if _, err := store.DB.Exec(
		`INSERT INTO mcp_grant (id, subject_type, subject_id, server_id, tools, effect) VALUES (?, 'key', '*', 'anginxbrowser', '["search"]', 'deny')`,
		"grant-1",
	); err != nil {
		t.Fatalf("seed deny grant: %v", err)
	}

	reg1 := seedServerTools(t, store, map[string][]ToolDefinition{
		"kagi":          {{Name: "get_info", Description: "kagi info"}},
		"anginxbrowser": {{Name: "search", Description: "web search"}},
	})
	authorizer := NewAuthorizer(store, nil, "allow")
	inj := NewInjector(reg1, authorizer, store)

	out := inj.GetAuthorizedOpenAITools("key123", nil)
	names := oaToolNames(out)
	if contains("anginxbrowser-search", names) {
		t.Fatalf("anginxbrowser-search must be invisible (denied on its only server), got %v", names)
	}
	if !contains("kagi-get_info", names) {
		t.Fatalf("kagi tool kagi-get_info must be included, got %v", names)
	}

	// Scenario 2: 'search' exists on BOTH servers; only anginxbrowser denies it.
	reg2 := seedServerTools(t, store, map[string][]ToolDefinition{
		"kagi":          {{Name: "search", Description: "kagi search"}, {Name: "get_info", Description: "kagi info"}},
		"anginxbrowser": {{Name: "search", Description: "web search"}, {Name: "browse", Description: "browse"}},
	})
	inj2 := NewInjector(reg2, authorizer, store)
	out2 := inj2.GetAuthorizedOpenAITools("key123", nil)
	names2 := oaToolNames(out2)

	// Every tool is server-prefixed: only kagi's copy of search survives.
	if !contains("kagi-search", names2) {
		t.Errorf("kagi-search must be exposed (denied only on anginxbrowser), got %v", names2)
	}
	if contains("anginxbrowser-search", names2) {
		t.Errorf("anginxbrowser-search must be invisible, got %v", names2)
	}
	// anginxbrowser's other (un-denied) tool stays visible under its server name.
	if !contains("anginxbrowser-browse", names2) {
		t.Errorf("anginxbrowser-browse must be included, got %v", names2)
	}
}

// TestServerQualifiedDenyAllowsOtherServerTools is a guard that the new
// server-aware method (used at injection) resolves the same bare tool on an
// unrelated server as allowed when no grant covers it.
func TestServerQualifiedDenyAllowsOtherServerTools(t *testing.T) {
	store, err := db.NewSQLiteStore(config.StorageConfig{Type: "sqlite", SqlitePath: ":memory:"})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Deny grant scoped to kagi only — tools on other servers are untouched.
	if _, err := store.DB.Exec(
		`INSERT INTO mcp_grant (id, subject_type, subject_id, server_id, tools, effect) VALUES (?, 'key', '*', 'kagi', '["secret"]', 'deny')`,
		"grant-2",
	); err != nil {
		t.Fatalf("seed deny grant: %v", err)
	}

	reg := seedServerTools(t, store, map[string][]ToolDefinition{
		"kagi":  {{Name: "secret", Description: "kagi secret"}},
		"other": {{Name: "secret", Description: "other secret"}},
	})
	authorizer := NewAuthorizer(store, nil, "allow")
	inj := NewInjector(reg, authorizer, store)
	out := inj.GetAuthorizedOpenAITools("key123", nil)
	names := oaToolNames(out)

	if contains("kagi-secret", names) {
		t.Errorf("kagi-secret must be invisible, got %v", names)
	}
	if !contains("other-secret", names) {
		t.Errorf("other-secret must be exposed (grant only denies kagi), got %v", names)
	}
}
