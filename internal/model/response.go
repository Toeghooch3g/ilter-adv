package model

import (
	"encoding/json"
	"maps"
)

type ChatCompletionResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
}

type Choice struct {
	Index        int           `json:"index"`
	Message      ChoiceMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type ChoiceMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type ToolCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function ToolCallFunctionData `json:"function"`
}

type ToolCallFunctionData struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// PromptTokensDetails mirrors OpenAI's usage.prompt_tokens_details object
// (also the normalized home for cached-token counts from Anthropic/DeepSeek
// adapters, which map their cache-read fields into CachedTokens).
type PromptTokensDetails struct {
	CachedTokens    int `json:"cached_tokens"`
	AudioTokens     int `json:"audio_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// CompletionTokensDetails mirrors OpenAI's usage.completion_tokens_details
// object.
type CompletionTokensDetails struct {
	AudioTokens     int `json:"audio_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// Usage carries a request's token counts and (for Anthropic) its cache-write
// token count, plus ilter's computed cost. Every provider-supplied field that
// this struct does not model is preserved verbatim in Raw and re-emitted to
// clients on marshal, so nothing the provider reports is dropped by the
// gateway's typed re-encode.
type Usage struct {
	PromptTokens             int                      `json:"prompt_tokens"`
	CompletionTokens         int                      `json:"completion_tokens"`
	TotalTokens              int                      `json:"total_tokens"`
	PromptTokensDetails      *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails  *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
	CacheCreationInputTokens int                      `json:"cache_creation_input_tokens,omitempty"` // Anthropic cache write
	IlterCost                float64                  `json:"ilter_cost,omitempty"`
	IlterBillingKey          string                   `json:"ilter_billing_key,omitempty"`

	// Raw holds the provider's complete original usage object. It is captured
	// on unmarshal and merged back into the JSON sent to clients (typed fields
	// above are the normalized view used for cost calc / audit / stats).
	Raw map[string]any `json:"-"`

	// CacheReadIncludedInPrompt records whether the cache-read tokens counted
	// in PromptTokensDetails.CachedTokens are already a subset of PromptTokens
	// (OpenAI/DeepSeek report prompt_tokens including cached) or separate
	// (Anthropic reports input_tokens excluding cache read/write). It drives
	// CalculateCost's split; never serialized.
	CacheReadIncludedInPrompt bool `json:"-"`
}

// UnmarshalJSON decodes the typed fields and captures the full original object
// into Raw so unknown provider fields survive the round trip. It also
// normalizes the two common cache-count shapes — OpenAI's
// prompt_tokens_details.cached_tokens and DeepSeek's prompt_cache_hit_tokens —
// into PromptTokensDetails.CachedTokens for cost/audit consumers.
func (u *Usage) UnmarshalJSON(data []byte) error {
	type usageAlias Usage
	var alias usageAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*u = Usage(alias)

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err == nil {
		u.Raw = raw
		// OpenAI/DeepSeek report prompt_tokens INCLUDING the cached portion,
		// so by default cache-read is a subset of prompt tokens.
		u.CacheReadIncludedInPrompt = true
		// OpenAI: usage.prompt_tokens_details.cached_tokens
		if d, ok := raw["prompt_tokens_details"].(map[string]any); ok {
			if c, ok := d["cached_tokens"].(float64); ok && c > 0 {
				if u.PromptTokensDetails == nil {
					u.PromptTokensDetails = &PromptTokensDetails{}
				}
				u.PromptTokensDetails.CachedTokens = int(c)
			}
		}
		// DeepSeek: usage.prompt_cache_hit_tokens
		if c, ok := raw["prompt_cache_hit_tokens"].(float64); ok && c > 0 {
			if u.PromptTokensDetails == nil {
				u.PromptTokensDetails = &PromptTokensDetails{}
			}
			u.PromptTokensDetails.CachedTokens = int(c)
		}
	}
	return nil
}

// MarshalJSON re-emits the provider's original usage object verbatim (Raw)
// overlaid with ilter's normalized counts and computed cost, so clients see
// every field the provider supplied plus ilter_cost. When Raw is empty
// (manually constructed Usage, e.g. Anthropic adapter or stream rebuilds) it
// falls back to plain typed marshaling.
func (u *Usage) MarshalJSON() ([]byte, error) {
	if len(u.Raw) == 0 {
		type usageAlias Usage
		return json.Marshal(usageAlias(*u))
	}
	out := make(map[string]any, len(u.Raw)+6)
	maps.Copy(out, u.Raw)
	out["prompt_tokens"] = u.PromptTokens
	out["completion_tokens"] = u.CompletionTokens
	out["total_tokens"] = u.TotalTokens
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		if d, ok := out["prompt_tokens_details"].(map[string]any); ok {
			d["cached_tokens"] = u.PromptTokensDetails.CachedTokens
		}
	}
	if u.IlterCost != 0 {
		out["ilter_cost"] = u.IlterCost
	}
	if u.IlterBillingKey != "" {
		out["ilter_billing_key"] = u.IlterBillingKey
	}
	return json.Marshal(out)
}

type ChatCompletionChunk struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	Choices           []ChunkChoice `json:"choices"`
	SystemFingerprint string        `json:"system_fingerprint,omitempty"`
	Usage             *Usage        `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Role             string          `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []ChunkToolCall `json:"tool_calls,omitempty"`
}

type ChunkToolCall struct {
	Index    int                   `json:"index"`
	ID       string                `json:"id,omitempty"`
	Type     string                `json:"type,omitempty"`
	Function ChunkToolCallFunction `json:"function"`
}

type ChunkToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    any     `json:"code,omitempty"`
	Param   *string `json:"param,omitempty"`
}
