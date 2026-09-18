import { useState } from 'react'
import { toast } from 'sonner'
import { api } from '../../../lib/api'
import { logger } from '../../../lib/logger'
import { Button } from '../../ui/button'
import { Globe, Key, Plus, X } from '../../ui/icons'

interface AddProviderModalProps {
  onClose: () => void
  onCreated: (name: string) => void
}

// providerTypes lists the selectable provider kinds for creation. "openai" is
// the generic OpenAI-compatible type — the one to choose for a custom endpoint
// (any base URL). The others map to built-in defaults.
const providerTypes: { value: string; label: string }[] = [
  { value: 'openai', label: 'Custom (OpenAI-compatible)' },
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'deepseek', label: 'DeepSeek' },
  { value: 'deepinfra', label: 'DeepInfra' },
  { value: 'gemini', label: 'Gemini' },
  { value: 'openrouter', label: 'OpenRouter' },
  { value: 'ollama', label: 'Ollama (local)' },
  { value: 'qwen', label: 'Qwen' },
]

// AddProviderModal registers a brand-new provider. It captures the identity
// (name + type), base URL, and API key; the provider becomes routable
// immediately via the runtime reload. Model metadata can be added afterwards
// through the Models JSON download/upload controls.
export function AddProviderModal({ onClose, onCreated }: AddProviderModalProps) {
  const [name, setName] = useState('')
  const [type, setType] = useState('openai')
  const [baseUrl, setBaseUrl] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [serviceTier, setServiceTier] = useState('')
  const [saving, setSaving] = useState(false)

  const handleSave = async () => {
    if (!name.trim() || !baseUrl.trim()) {
      toast.error('Missing fields', { description: 'Provider name and base URL are required.' })
      return
    }
    setSaving(true)
    try {
      const created = await api.providers.createProvider({
        name: name.trim(),
        type,
        baseUrl: baseUrl.trim(),
        apiKey: apiKey || undefined,
        serviceTier: serviceTier || undefined,
      })
      toast.success('Provider added', { description: `${created.name} is now configured.` })
      onCreated(created.name)
    } catch (err) {
      logger.error('Failed to create provider:', err)
      toast.error('Add failed', { description: 'Could not create the provider.' })
    } finally {
      setSaving(false)
    }
  }

  const inputCls =
    'w-full rounded-lg border border-surface-300 px-3 py-2 text-sm text-surface-900 placeholder:text-surface-400 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500'

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm p-4"
      onClick={onClose}
    >
      <div
        className="w-full max-w-lg overflow-hidden rounded-2xl bg-white shadow-2xl border border-surface-200 flex flex-col max-h-[90vh]"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-6 py-4 border-b border-surface-100 bg-surface-50/50 flex items-center justify-between">
          <div className="flex items-center gap-3">
            <span className="inline-flex items-center justify-center h-9 w-9 rounded-lg bg-brand-50 text-brand-700">
              <Plus size={18} />
            </span>
            <div>
              <h3 className="text-base font-semibold text-surface-900">Add Provider</h3>
              <p className="text-xs text-surface-500">Register an upstream LLM provider</p>
            </div>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="rounded-lg p-1.5 text-surface-400 hover:bg-surface-200 hover:text-surface-600 transition-colors"
          >
            <X size={18} />
          </button>
        </div>

        <div className="p-6 overflow-y-auto space-y-4">
          <div>
            <label htmlFor="new-provider-name" className="block text-xs font-medium text-surface-700 mb-1">
              Name
            </label>
            <input
              id="new-provider-name"
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="my-custom-provider"
              className={inputCls}
            />
          </div>

          <div>
            <label htmlFor="new-provider-type" className="block text-xs font-medium text-surface-700 mb-1">
              Type
            </label>
            <select id="new-provider-type" value={type} onChange={(e) => setType(e.target.value)} className={inputCls}>
              {providerTypes.map((t) => (
                <option key={t.value} value={t.value}>
                  {t.label}
                </option>
              ))}
            </select>
            <p className="mt-1 text-[11px] text-surface-500">
              Choose “Custom (OpenAI-compatible)” for a self-hosted or third-party endpoint.
            </p>
          </div>

          <div>
            <label
              htmlFor="new-provider-base-url"
              className="flex items-center gap-1 text-xs font-medium text-surface-700 mb-1"
            >
              <Globe size={12} /> Base URL
            </label>
            <input
              id="new-provider-base-url"
              type="text"
              value={baseUrl}
              onChange={(e) => setBaseUrl(e.target.value)}
              placeholder="https://api.example.com/v1"
              className={inputCls}
            />
          </div>

          <div>
            <label
              htmlFor="new-provider-api-key"
              className="flex items-center gap-1 text-xs font-medium text-surface-700 mb-1"
            >
              <Key size={12} /> API Key (optional)
            </label>
            <input
              id="new-provider-api-key"
              type="password"
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
              placeholder="sk-..."
              className={`${inputCls} font-mono`}
            />
            <p className="mt-1 text-[11px] text-surface-500">
              Leave blank for providers with public or no-auth endpoints.
            </p>
          </div>

          <div>
            <label htmlFor="new-provider-service-tier" className="block text-xs font-medium text-surface-700 mb-1">
              Service Tier (optional)
            </label>
            <select
              id="new-provider-service-tier"
              value={serviceTier}
              onChange={(e) => setServiceTier(e.target.value)}
              className={inputCls}
            >
              <option value="">(none — client decides)</option>
              <option value="default">default</option>
              <option value="priority">priority</option>
              <option value="flex">flex</option>
            </select>
            <p className="mt-1 text-[11px] text-surface-500">
              Provider-level default service tier for OpenAI-compatible providers.
            </p>
          </div>
        </div>

        <div className="px-6 py-4 border-t border-surface-100 flex items-center justify-end gap-2">
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={handleSave} disabled={saving}>
            {saving ? 'Adding...' : 'Add Provider'}
          </Button>
        </div>
      </div>
    </div>
  )
}
