package jobs

import (
	"fmt"
	"strings"
)

// DefaultMaxVarLength is the maximum length of a single variable value.
const DefaultMaxVarLength = 65536

// SanitizeVarValue validates and sanitizes a single variable value.
func SanitizeVarValue(key, value string, maxLen int) (string, error) {
	if strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("variable %q contains null byte", key)
	}
	if len(value) > maxLen {
		return "", fmt.Errorf("variable %q exceeds max length (%d > %d)", key, len(value), maxLen)
	}
	return value, nil
}

// resolveTypedVariable resolves a {"type": ..., ...} variable source.
func resolveTypedVariable(name, typ string, m map[string]any, maxVarLength int) (any, error) {
	switch typ {
	case "static":
		vStr, _ := m["value"].(string)
		return SanitizeVarValue(name, vStr, maxVarLength)
	default:
		return nil, fmt.Errorf("unknown variable source type %q for variable %q", typ, name)
	}
}

// resolveVariable resolves a single VariablesConfig entry — a typed
// {"type": "static", "value": ...} object, or a bare string — to its
// sanitized value.
func resolveVariable(name string, val any, maxVarLength int) (any, error) {
	if m, ok := val.(map[string]any); ok {
		if typ, _ := m["type"].(string); typ != "" {
			return resolveTypedVariable(name, typ, m, maxVarLength)
		}
	}
	vStr, ok := val.(string)
	if !ok {
		return nil, fmt.Errorf("variable %q: expected string, got %T", name, val)
	}
	return SanitizeVarValue(name, vStr, maxVarLength)
}

// ResolveVariables resolves a VariablesConfig map and returns the
// resolved variable map. Each value is sanitized before being returned.
func ResolveVariables(varsConfig VariablesConfig, maxVarLength int) (map[string]any, error) {
	if maxVarLength <= 0 {
		maxVarLength = DefaultMaxVarLength
	}
	if len(varsConfig) == 0 {
		return map[string]any{}, nil
	}

	result := make(map[string]any, len(varsConfig))
	for name, val := range varsConfig {
		resolved, err := resolveVariable(name, val, maxVarLength)
		if err != nil {
			return nil, err
		}
		result[name] = resolved
	}
	return result, nil
}
