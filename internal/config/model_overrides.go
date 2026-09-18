package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// ParseModelOverrides decodes a JSON model-override document (an array of
// ModelOverride entries) into the corresponding slice. This is the canonical
// parser for both the runtime_config "model_overrides" section value and the
// on-disk overrides file, so the two share one schema.
func ParseModelOverrides(data []byte) ([]ModelOverride, error) {
	var overrides []ModelOverride
	if len(data) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(data, &overrides); err != nil {
		return nil, fmt.Errorf("parse model overrides: %w", err)
	}
	return overrides, nil
}

// ReadModelOverridesFile reads and parses a JSON model-override document from
// disk. Missing files yield an error; empty files yield no overrides.
func ReadModelOverridesFile(path string) ([]ModelOverride, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read model overrides file: %w", err)
	}
	return ParseModelOverrides(data)
}
