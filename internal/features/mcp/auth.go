package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db"
)

type Grant struct {
	ID          string
	SubjectType string // 'key' | 'user' | 'group'
	SubjectID   string
	ServerID    string
	Tools       string // '*' or JSON array
	Effect      string // 'allow' | 'deny'
}

type Authorizer struct {
	store         *db.SQLiteStore
	rules         []config.MCPAccessRule
	defaultPolicy string // "allow" or "deny"
}

func NewAuthorizer(store *db.SQLiteStore, rules []config.MCPAccessRule, defaultPolicy string) *Authorizer {
	if defaultPolicy != "allow" {
		defaultPolicy = "deny"
	}
	return &Authorizer{
		store:         store,
		rules:         rules,
		defaultPolicy: defaultPolicy,
	}
}

type AuthResult struct {
	Allowed     bool
	Tool        string
	MatchedRule string
}

// CheckAccess denies-overrides: DB grants → config rules → default policy.
// serverID is the resolved server ID (may be "" for server-agnostic checks).
// toolName is the bare tool name (no server prefix).
func (a *Authorizer) CheckAccess(keyPrefix string, groupIDs []int, keyID string, serverID string, toolName string) *AuthResult {
	if a.store != nil {
		if result := a.checkGrantAccess(keyPrefix, groupIDs, keyID, serverID, toolName); result != nil {
			return result
		}
	}

	if result := a.checkConfigRuleAccess(keyPrefix, groupIDs, keyID, serverID, toolName); result != nil {
		return result
	}

	allowed := a.resolveDefaultPolicy() == "allow"
	return &AuthResult{
		Allowed: allowed,
		Tool:    toolName,
	}
}

// checkGrantAccess evaluates DB grants for the request, returning a
// non-nil AuthResult when a deny or allow grant matched (deny-overrides).
func (a *Authorizer) checkGrantAccess(keyPrefix string, groupIDs []int, keyID string, serverID, toolName string) *AuthResult {
	denied, allowed := a.resolveGrants(keyPrefix, groupIDs, keyID, serverID, toolName)
	if denied {
		return &AuthResult{
			Allowed:     false,
			Tool:        toolName,
			MatchedRule: fmt.Sprintf("grant:deny:%s", toolName),
		}
	}
	if allowed {
		return &AuthResult{
			Allowed:     true,
			Tool:        toolName,
			MatchedRule: fmt.Sprintf("grant:allow:%s", toolName),
		}
	}
	return nil
}

// checkConfigRuleAccess evaluates the static config.MCPAccessRule list,
// returning a non-nil AuthResult when a deny or allow rule matched
// (deny-overrides across all matching rules).
func (a *Authorizer) checkConfigRuleAccess(keyPrefix string, groupIDs []int, keyID string, serverID, toolName string) *AuthResult {
	configDenied, configAllowed, configMatched := evaluateConfigRules(a.rules, keyPrefix, groupIDs, keyID, serverID, toolName)
	if configDenied {
		return &AuthResult{
			Allowed:     false,
			Tool:        toolName,
			MatchedRule: configMatched,
		}
	}
	if configAllowed {
		return &AuthResult{
			Allowed:     true,
			Tool:        toolName,
			MatchedRule: configMatched,
		}
	}
	return nil
}

// evaluateConfigRules scans rules for those whose subject matches the
// caller, and among those, whether any pattern denies or allows toolName on
// serverID (deny-overrides). matched names the pattern of the last denying
// rule, or else the first allowing rule.
func evaluateConfigRules(rules []config.MCPAccessRule, keyPrefix string, groupIDs []int, keyID, serverID, toolName string) (denied, allowed bool, matched string) {
	for _, rule := range rules {
		if !matchRuleSubject(rule, keyPrefix, groupIDs, keyID) {
			continue
		}
		ruleEffect := rule.Effect
		if ruleEffect == "" {
			ruleEffect = "allow"
		}
		for _, pattern := range rule.Tools {
			if !configToolMatches(pattern, serverID, toolName) {
				continue
			}
			if ruleEffect == "deny" {
				denied = true
				matched = pattern
			} else if !allowed {
				allowed = true
				matched = pattern
			}
		}
	}
	return denied, allowed, matched
}

// resolveGrants scans subject hierarchy (key→user→group). Deny-overrides: deny trumps all.
func (a *Authorizer) resolveGrants(keyPrefix string, groupIDs []int, keyID string, serverID, toolName string) (bool, bool) {
	if serverID == "" {
		return false, false
	}

	candidates := a.subjectCandidates(keyPrefix, groupIDs, keyID)
	hasDeny := false
	hasAllow := false

	for _, c := range candidates {
		denied, allowed := a.matchGrants(c.typ, c.id, serverID, toolName)
		if denied {
			hasDeny = true
		}
		if allowed {
			hasAllow = true
		}
	}

	return hasDeny, hasAllow
}

type subject struct {
	typ string
	id  string
}

func (a *Authorizer) subjectCandidates(keyPrefix string, groupIDs []int, keyID string) []subject {
	candidates := []subject{}
	if keyID != "" {
		candidates = append(candidates, subject{"key", keyID})
	}
	if after, ok := strings.CutPrefix(keyPrefix, "user:"); ok {
		candidates = append(candidates, subject{"user", after})
	}
	if after, ok := strings.CutPrefix(keyPrefix, "group:"); ok {
		candidates = append(candidates, subject{"group", after})
	}
	for _, gid := range groupIDs {
		candidates = append(candidates, subject{"group", fmt.Sprintf("%d", gid)})
	}
	return candidates
}

func (a *Authorizer) matchGrants(subjectType, subjectID, serverID, toolName string) (bool, bool) {
	rows, err := a.store.DB.Query(
		`SELECT tools, effect FROM mcp_grant WHERE subject_type = ? AND (subject_id = ? OR subject_id = '*') AND (server_id = ? OR server_id = '*') AND enabled = 1`,
		subjectType, subjectID, serverID,
	)
	if err != nil {
		return false, false
	}
	defer func() { _ = rows.Close() }()

	hasDeny := false
	hasAllow := false

	for rows.Next() {
		var tools, effect string
		if err := rows.Scan(&tools, &effect); err != nil {
			continue
		}
		if !toolMatches(tools, toolName) {
			continue
		}
		if effect == "deny" {
			hasDeny = true
		} else {
			hasAllow = true
		}
	}
	if err := rows.Err(); err != nil {
		slog.Warn("error iterating mcp_grant rows", "error", err)
	}

	return hasDeny, hasAllow
}

func matchRuleSubject(rule config.MCPAccessRule, keyPrefix string, groupIDs []int, keyID string) bool {
	if rule.KeyPrefix != "" {
		if keyPrefix == "" || !strings.HasPrefix(keyPrefix, rule.KeyPrefix) {
			return false
		}
	}
	if rule.GroupID != nil {
		if len(groupIDs) == 0 {
			return false
		}
		match := slices.Contains(groupIDs, *rule.GroupID)
		if !match {
			return false
		}
	}
	if rule.KeyID != "" {
		if keyID == "" || keyID != rule.KeyID {
			return false
		}
	}
	return true
}

func (a *Authorizer) resolveDefaultPolicy() string {
	if a.store == nil {
		return a.defaultPolicy
	}
	var value string
	err := a.store.DB.QueryRow(
		`SELECT value FROM runtime_config WHERE section = 'mcp' AND key = 'default_policy' LIMIT 1`,
	).Scan(&value)
	if err != nil {
		return a.defaultPolicy
	}
	if value == "true" {
		return "allow"
	}
	return "deny"
}

func (a *Authorizer) GetAuthorizedTools(keyPrefix string, groupIDs []int, keyID string, allTools []string) []string {
	var authorized []string
	for _, tool := range allTools {
		if result := a.CheckAccess(keyPrefix, groupIDs, keyID, "", tool); result.Allowed {
			authorized = append(authorized, tool)
		}
	}
	return authorized
}

// configToolMatches checks whether a config rule pattern matches the given
// serverID and toolName. A bare pattern ("tool-a") matches on any server.
func configToolMatches(pattern, serverID, toolName string) bool {
	if pattern == "*" || pattern == "*/*" {
		return true
	}

	var serverPat string
	toolPat := pattern
	if strings.Contains(pattern, "/") {
		parts := strings.SplitN(pattern, "/", 2)
		serverPat, toolPat = parts[0], parts[1]
	} else {
		serverPat = "*"
	}

	match, err := path.Match(serverPat, serverID)
	if err != nil || !match {
		return false
	}

	return toolMatches(toolPat, toolName)
}

func toolMatches(tools, toolName string) bool {
	if tools == "*" || tools == "*/*" {
		return true
	}

	toolName = strings.TrimSpace(toolName)

	if strings.HasPrefix(tools, "[") {
		t := strings.Trim(tools, "[]")
		for tname := range strings.SplitSeq(t, ",") {
			tname = strings.Trim(tname, "\" ")
			if tname == toolName {
				return true
			}
		}
		return false
	}

	matched, err := path.Match(tools, toolName)
	if err != nil {
		return false
	}
	return matched
}

func ExtractKeyInfo(ctx context.Context, keyID string, store *db.SQLiteStore) (keyPrefix string, err error) {
	if keyID == "" {
		return "", nil
	}
	vk, err := store.GetAPIKey(ctx, keyID)
	if err != nil {
		return "", fmt.Errorf("get virtual key: %w", err)
	}
	if vk.UserID != nil {
		return fmt.Sprintf("user:%d", *vk.UserID), nil
	}
	if vk.GroupID != nil {
		return fmt.Sprintf("group:%d", *vk.GroupID), nil
	}
	return "", nil
}
