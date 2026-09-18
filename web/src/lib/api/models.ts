import { adaptModelProvider } from './adapters'
import { request } from './request'
import type { ModelProvider } from './types'

interface GoModelResponseItem {
  name: string
  provider: string
  type: string
  owned_by: string
  active: boolean
  configured: boolean
  display_name?: string
  category?: string
  cost_per_input_token?: number
  cost_per_output_token?: number
}

export async function getModelProviders(): Promise<ModelProvider[]> {
  const items = await request<GoModelResponseItem[]>('/models')
  return items.map(adaptModelProvider)
}

export async function toggleModel(provider: string, name: string, active: boolean): Promise<void> {
  await request('/models/toggle', {
    method: 'POST',
    body: JSON.stringify({ provider, name, active }),
  })
}

export async function updateModelCategory(name: string, category: string): Promise<void> {
  await request('/models/category', {
    method: 'POST',
    body: JSON.stringify({ name, category }),
  })
}

export async function getCategories(): Promise<string[]> {
  const res = await request<{ categories: string[] }>('/models/categories')
  return res.categories ?? []
}

export async function createCategory(name: string): Promise<void> {
  await request('/models/categories', {
    method: 'POST',
    body: JSON.stringify({ name }),
  })
}

export async function deleteCategory(name: string): Promise<void> {
  await request(`/models/categories/${encodeURIComponent(name)}`, { method: 'DELETE' })
}
