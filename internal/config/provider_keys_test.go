package config

import (
	"testing"
)

// TestReapplyEnvKeysSeedsMissingProviders verifies that ReapplyEnvKeys
// auto-registers a known provider type from its env var when the set has no
// entry for it (the hot-reload path: env-seeded providers have no
// runtime_config row, so a DB-only snapshot must be re-seeded on reload).
func TestReapplyEnvKeysSeedsMissingProviders(t *testing.T) {
	t.Setenv("ILTER_PROVIDER_OPENAI_API_KEY", "sk-env-openai")
	t.Setenv("ILTER_PROVIDER_DEEPSEEK_API_KEY", "sk-env-deepseek")

	// DB-only set: no openai entry, but a deepseek row exists.
	in := []ProviderConfig{
		{Name: "deepseek", Type: "deepseek", BaseURL: DefaultBaseURLs["deepseek"], APIKey: "sk-db-deepseek"},
	}
	out := ReapplyEnvKeys(in)

	// Existing DB row is untouched (DB row wins) and marked db-sourced.
	var foundDeepseek bool
	for _, p := range out {
		if p.Name == "deepseek" {
			foundDeepseek = true
			if p.APIKey != "sk-db-deepseek" {
				t.Errorf("deepseek APIKey = %q, want DB value to win", p.APIKey)
			}
			if p.APIKeySource != "db" {
				t.Errorf("deepseek APIKeySource = %q, want db", p.APIKeySource)
			}
		}
	}
	if !foundDeepseek {
		t.Error("deepseek provider missing from output")
	}

	// openai is seeded from env since it has no entry.
	var foundOpenAI bool
	for _, p := range out {
		if p.Name == "openai" {
			foundOpenAI = true
			if p.APIKey != "sk-env-openai" {
				t.Errorf("openai APIKey = %q, want env-seeded", p.APIKey)
			}
			if p.APIKeySource != "ILTER_PROVIDER_OPENAI_API_KEY" {
				t.Errorf("openai APIKeySource = %q, want env var name", p.APIKeySource)
			}
			if p.BaseURL != DefaultBaseURLs["openai"] {
				t.Errorf("openai BaseURL = %q, want default", p.BaseURL)
			}
		}
	}
	if !foundOpenAI {
		t.Error("openai provider was not seeded from env")
	}
}

// TestReapplyEnvKeysDBRowWins verifies the "DB row wins" precedence: when a
// provider already has a runtime_config row (present in the set), the env var
// does NOT override its stored API key.
func TestReapplyEnvKeysDBRowWins(t *testing.T) {
	t.Setenv("ILTER_PROVIDER_OPENAI_API_KEY", "sk-env")
	in := []ProviderConfig{
		{Name: "openai", Type: "openai", BaseURL: "https://custom.example.com/v1", APIKey: "sk-db"},
	}
	out := ReapplyEnvKeys(in)
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1 (no duplicate openai registration)", len(out))
	}
	if out[0].APIKey != "sk-db" {
		t.Errorf("APIKey = %q, want DB value to win over env", out[0].APIKey)
	}
	if out[0].APIKeySource != "db" {
		t.Errorf("APIKeySource = %q, want db", out[0].APIKeySource)
	}
	if out[0].BaseURL != "https://custom.example.com/v1" {
		t.Errorf("BaseURL = %q, want existing value preserved", out[0].BaseURL)
	}
}
