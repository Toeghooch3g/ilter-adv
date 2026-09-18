package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ilter-ai/ilter/internal/db/dbtest"
)

// TestMCPInjectionAllowed covers the per-key gate on server-side MCP/OpenAPI
// tool injection: empty/synthetic key IDs are never allowed, the key must
// exist and be enabled for injection, and the flag defaults to off.
func TestMCPInjectionAllowed(t *testing.T) {
	store := dbtest.NewFile(t)
	a := &App{store: store}

	assert.False(t, a.mcpInjectionAllowed(""), "empty key ID must not allow injection")
	assert.False(t, a.mcpInjectionAllowed("admin"), "synthetic admin key must not allow injection")
	assert.False(t, a.mcpInjectionAllowed("dev:local"), "synthetic dev key must not allow injection")
	assert.False(t, a.mcpInjectionAllowed("no-such-key"), "unknown key must not allow injection")

	// New keys default to injection off.
	key, _, err := store.CreateAPIKey(context.Background(), "mcp-gate", nil, nil, 0, 0, 0, 0, nil, nil, nil)
	require.NoError(t, err)
	assert.False(t, a.mcpInjectionAllowed(key.ID), "new key must default to injection off")

	// Opt in, mirroring the dashboard caller: mutate the preloaded existing
	// struct and pass it through (booleans are written verbatim).
	existing, err := store.GetAPIKey(context.Background(), key.ID)
	require.NoError(t, err)
	existing.MCPInjectionEnabled = true
	require.NoError(t, store.UpdateAPIKey(context.Background(), key.ID, *existing, false, false))
	assert.True(t, a.mcpInjectionAllowed(key.ID), "opted-in key must allow injection")

	// Opt back out.
	existing, err = store.GetAPIKey(context.Background(), key.ID)
	require.NoError(t, err)
	existing.MCPInjectionEnabled = false
	require.NoError(t, store.UpdateAPIKey(context.Background(), key.ID, *existing, false, false))
	assert.False(t, a.mcpInjectionAllowed(key.ID), "opted-out key must not allow injection")
}
