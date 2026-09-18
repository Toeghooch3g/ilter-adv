import { request } from './request'
import type { ProviderInfo } from './types'

export async function getProviders(): Promise<ProviderInfo[]> {
  return (await request<ProviderInfo[] | null>('/providers')) || []
}

export async function updateProvider(
  name: string,
  baseUrl: string,
  apiKey: string | null,
  apiKeys?: string[],
  serviceTier?: string,
): Promise<void> {
  const body: Record<string, unknown> = { name, base_url: baseUrl }
  // null = don't change the key, "" = clear the key
  if (apiKey !== null) body.api_key = apiKey
  if (apiKeys && apiKeys.length > 0) body.api_keys = apiKeys
  // undefined = don't change the tier; "" = clear it
  if (serviceTier !== undefined) body.service_tier = serviceTier
  await request('/providers', { method: 'POST', body: JSON.stringify(body) })
}

// createProvider registers a brand-new provider (defaulting to an OpenAI-
// compatible custom provider when type is omitted) and makes it live. The
// returned provider name/type confirm the creation.
export async function createProvider(args: {
  name: string
  type?: string
  baseUrl: string
  apiKey?: string
  serviceTier?: string
}): Promise<{ name: string; type: string }> {
  const body: Record<string, unknown> = { name: args.name, base_url: args.baseUrl }
  if (args.type) body.type = args.type
  if (args.apiKey) body.api_key = args.apiKey
  if (args.serviceTier) body.service_tier = args.serviceTier
  return request<{ name: string; type: string }>('/providers/create', { method: 'POST', body: JSON.stringify(body) })
}

// deleteProvider removes a provider and everything tied to it
// (runtime_config row, model overrides, discovered models). Env-seeded
// providers (key from ILTER_PROVIDER_<NAME>_API_KEY) are rejected by the
// backend with a 409.
export async function deleteProvider(name: string): Promise<void> {
  await request(`/providers/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

// ModelOverride mirrors a single entry in a provider's manual model-override
// document (the lowest-priority model source; the provider's /v1/models
// endpoint wins per-field). See config.ModelOverride in the Go backend.
export interface ModelOverride {
  id: string
  display_name?: string
  tier?: string
  cost_per_input_token?: number
  cost_per_output_token?: number
  max_context_tokens?: number
  max_output_tokens?: number
  capabilities?: string[]
}

// getModelOverrides returns a provider's stored model-override document
// (empty array when none is configured). Used both to preview the list and to
// drive the download.
export async function getModelOverrides(name: string): Promise<ModelOverride[]> {
  return (await request<ModelOverride[] | null>(`/providers/${encodeURIComponent(name)}/models-overrides`)) || []
}

// putModelOverrides replaces a provider's model-override document from a raw
// JSON string (the uploaded file's contents).
export async function putModelOverrides(name: string, json: string): Promise<void> {
  await request(`/providers/${encodeURIComponent(name)}/models-overrides`, {
    method: 'PUT',
    body: json,
  })
}
