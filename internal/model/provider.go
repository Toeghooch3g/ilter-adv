package model

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ProviderRegistration represents a provider configuration stored in the
// runtime database.
type ProviderRegistration struct {
	Name            string            `json:"name"`
	Provider        string            `json:"provider"`
	BaseURL         string            `json:"base_url"`
	APISecretKey    string            `json:"api_secret_key"`
	IsActive        bool              `json:"is_active"`
	Headers         map[string]string `json:"headers,omitempty"`
	CircuitBreaker  CircuitBreakerCfg `json:"circuit_breaker"`
	Timeout         time.Duration     `json:"timeout"`
	MaxRetries      int               `json:"max_retries"`
	DiscoveryPublic bool              `json:"discovery_public,omitempty"`

	// ModelOverridesFile is an optional path to a JSON model-override file
	// (same schema as the runtime "model_overrides" section). At boot it seeds
	// the provider's overrides if no runtime model_overrides entry exists yet.
	ModelOverridesFile string `json:"model_overrides_file,omitempty"`

	// ServiceTier is the provider-level default service tier injected into
	// chat requests that do not set one. Valid values: "", "default",
	// "priority", "flex" (OpenAI-compatible; "auto" may be sent by clients
	// and is passed through untouched).
	ServiceTier string `json:"service_tier,omitempty"`
}

// CircuitBreakerCfg mirrors config.CircuitBreakerConfig for standalone use
// in the model package without importing config and creating a cycle.
type CircuitBreakerCfg struct {
	MaxFailures         int           `json:"max_failures"`
	Timeout             time.Duration `json:"timeout"`
	HalfOpenMaxRequests int           `json:"half_open_max_requests"`
}

// Validate checks that required fields are set and well-formed.
func (p *ProviderRegistration) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("provider name is required")
	}
	if strings.TrimSpace(p.Provider) == "" {
		return fmt.Errorf("provider type is required")
	}
	if p.BaseURL != "" {
		u, err := url.Parse(p.BaseURL)
		if err != nil || !u.IsAbs() {
			return fmt.Errorf("invalid base_url: %q: must be an absolute URL", p.BaseURL)
		}
	}
	switch p.ServiceTier {
	case "", "default", "priority", "flex":
	default:
		return fmt.Errorf("invalid service_tier %q: must be one of default, priority, flex", p.ServiceTier)
	}
	return nil
}
