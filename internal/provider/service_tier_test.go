package provider

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/model"
)

// decodeTransformBody runs TransformRequest against a provider and returns
// the marshaled request body as a map.
func decodeTransformBody(t *testing.T, p Provider, req *model.ChatCompletionRequest) map[string]any {
	t.Helper()
	httpReq, err := p.TransformRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	return m
}

func TestServiceTier_ProviderDefaultInjected(t *testing.T) {
	p := NewOpenAIProvider(config.ProviderConfig{
		Name: "openai", Type: "openai", BaseURL: "https://api.openai.com/v1",
		ServiceTier: "flex",
	})
	body := decodeTransformBody(t, p, &model.ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []model.Message{{Role: "user", Content: "hi"}},
	})
	if got, _ := body["service_tier"].(string); got != "flex" {
		t.Fatalf("service_tier = %q, want flex (provider default injected)", got)
	}
}

func TestServiceTier_ClientWins(t *testing.T) {
	p := NewOpenAIProvider(config.ProviderConfig{
		Name: "openai", Type: "openai", BaseURL: "https://api.openai.com/v1",
		ServiceTier: "flex",
	})
	body := decodeTransformBody(t, p, &model.ChatCompletionRequest{
		Model:       "gpt-4o",
		Messages:    []model.Message{{Role: "user", Content: "hi"}},
		ServiceTier: "priority",
	})
	if got, _ := body["service_tier"].(string); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (client wins)", got)
	}
}

func TestServiceTier_AbsentWhenProviderUnconfigured(t *testing.T) {
	p := NewOpenAIProvider(config.ProviderConfig{
		Name: "openai", Type: "openai", BaseURL: "https://api.openai.com/v1",
	})
	body := decodeTransformBody(t, p, &model.ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []model.Message{{Role: "user", Content: "hi"}},
	})
	if _, ok := body["service_tier"]; ok {
		t.Fatalf("service_tier must be absent when provider has no tier, got %v", body["service_tier"])
	}
}

func TestServiceTier_DeepInfraProvider(t *testing.T) {
	p := NewDeepInfraProvider(config.ProviderConfig{
		Name: "deepinfra", Type: "deepinfra", BaseURL: "https://api.deepinfra.com/v1/openai",
		ServiceTier: "priority",
	})
	body := decodeTransformBody(t, p, &model.ChatCompletionRequest{
		Model:    "meta-llama/Llama-4-Maverick-17B-128E-Instruct-FP8",
		Messages: []model.Message{{Role: "user", Content: "hi"}},
	})
	if got, _ := body["service_tier"].(string); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (deepinfra default injected)", got)
	}
}
