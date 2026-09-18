package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseModelOverrides verifies the canonical model-override JSON schema is
// decoded into config.ModelOverride entries, and that empty input yields no
// overrides while malformed input errors.
func TestParseModelOverrides(t *testing.T) {
	t.Run("valid document", func(t *testing.T) {
		overrides, err := ParseModelOverrides([]byte(`[
			{"id": "a", "category": "free", "cost_per_input_token": 0, "max_context_tokens": 4096},
			{"id": "b", "category": "premium", "cost_per_input_token": 0.001, "cost_per_output_token": 0.002}
		]`))
		require.NoError(t, err)
		require.Len(t, overrides, 2)
		assert.Equal(t, "a", overrides[0].ID)
		assert.Equal(t, "free", overrides[0].Category)
		assert.Equal(t, 4096, overrides[0].MaxContextTokens)
		assert.Equal(t, "b", overrides[1].ID)
		assert.Equal(t, 0.002, overrides[1].CostPerOutputToken)
	})

	t.Run("empty input yields no overrides", func(t *testing.T) {
		overrides, err := ParseModelOverrides(nil)
		require.NoError(t, err)
		assert.Empty(t, overrides)
	})

	t.Run("malformed input errors", func(t *testing.T) {
		_, err := ParseModelOverrides([]byte(`{"not": "an array"}`))
		require.Error(t, err)
	})
}

// TestReadModelOverridesFile verifies the boot-time file path source shares the
// same schema as the runtime section.
func TestReadModelOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	require.NoError(t, os.WriteFile(path, []byte(`[{"id": "x", "category": "economy"}]`), 0o600))

	overrides, err := ReadModelOverridesFile(path)
	require.NoError(t, err)
	require.Len(t, overrides, 1)
	assert.Equal(t, "economy", overrides[0].Category)

	_, err = ReadModelOverridesFile(filepath.Join(dir, "missing.json"))
	require.Error(t, err)
}
