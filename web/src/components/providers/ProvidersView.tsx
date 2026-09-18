import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'
import { qk } from '../../lib/query'
import { Button } from '../ui/button'
import { Plus } from '../ui/icons'
import { QueryProvider } from '../ui/query-provider'
import { Skeleton } from '../ui/skeleton'
import { AddProviderModal } from './components/AddProviderModal'
import { ConfigModal } from './components/ConfigModal'
import { ProviderCard } from './components/ProviderCard'
import { useProviders } from './useProviders'

const ACTIVE_STATUSES = new Set(['online'])

function ProvidersViewContent() {
  const queryClient = useQueryClient()
  const [showAdd, setShowAdd] = useState(false)
  const {
    providers,
    isLoading,
    removeProvider,
    configProvider,
    configForm,
    setConfigForm,
    serviceTier,
    setServiceTier,
    serviceTierTouched,
    setServiceTierTouched,
    apiKeyTouched,
    setApiKeyTouched,
    multiKeysTouched,
    setMultiKeysTouched,
    expandedProviders,
    toggleModels,
    openConfig,
    handleSaveConfig,
    closeConfig,
  } = useProviders()

  const handleRemoveProvider = async (p: { name: string; api_key_source: string }) => {
    if (!window.confirm(`Remove provider "${p.name}"? This deletes its models and configuration.`)) return
    try {
      await removeProvider.mutateAsync(p.name)
      toast.success('Provider removed', { description: `${p.name} was deleted.` })
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'Provider could not be removed'
      toast.error('Remove failed', { description: msg })
    }
  }

  const activeProviders = providers.filter((p) => ACTIVE_STATUSES.has(p.status))
  const passiveProviders = providers.filter((p) => !ACTIVE_STATUSES.has(p.status))

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <p className="text-sm text-surface-500">Upstream providers powering the gateway.</p>
        <Button size="sm" variant="outline" onClick={() => setShowAdd(true)}>
          <Plus size={14} />
          Add Provider
        </Button>
      </div>
      {isLoading ? (
        <div className="space-y-3">
          {Array.from({ length: 3 }).map((_, i) => (
            <div key={i} className="rounded-xl border border-surface-200 bg-white p-6 shadow-card">
              <Skeleton className="h-4 w-1/3 mb-4" />
              <Skeleton className="h-8 w-1/2 mb-3" />
              <Skeleton className="h-3 w-2/3 mb-2" />
              <Skeleton className="h-3 w-1/2 mb-2" />
              <Skeleton className="h-3 w-3/4" />
            </div>
          ))}
        </div>
      ) : providers.length === 0 ? (
        <div className="rounded-xl border border-surface-200 bg-white p-6 text-center text-surface-400 text-sm">
          No providers configured
        </div>
      ) : (
        <>
          {activeProviders.length > 0 && (
            <div>
              <h3 className="text-sm font-semibold text-surface-700 uppercase tracking-wider mb-3">
                Active Providers ({activeProviders.length})
              </h3>
              <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
                {activeProviders.map((provider) => (
                  <ProviderCard
                    key={provider.name}
                    provider={provider}
                    isExpanded={expandedProviders.has(provider.name)}
                    onToggleModels={toggleModels}
                    onOpenConfig={openConfig}
                    onRemove={handleRemoveProvider}
                  />
                ))}
              </div>
            </div>
          )}

          {passiveProviders.length > 0 && (
            <div>
              <h3 className="text-sm font-semibold text-surface-500 uppercase tracking-wider mb-3">
                Passive Providers ({passiveProviders.length})
              </h3>
              <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
                {passiveProviders.map((provider) => (
                  <ProviderCard
                    key={provider.name}
                    provider={provider}
                    isExpanded={expandedProviders.has(provider.name)}
                    onToggleModels={toggleModels}
                    onOpenConfig={openConfig}
                    onRemove={handleRemoveProvider}
                  />
                ))}
              </div>
            </div>
          )}
        </>
      )}

      {configProvider && (
        <ConfigModal
          provider={configProvider}
          baseUrl={configForm.base_url}
          apiKey={configForm.api_key}
          apiKeyTouched={apiKeyTouched}
          onBaseUrlChange={(url) => setConfigForm({ ...configForm, base_url: url })}
          onApiKeyChange={(key) => {
            if (!apiKeyTouched) setApiKeyTouched(true)
            setConfigForm({ ...configForm, api_key: key })
          }}
          onMultiKeysChange={(keys) => {
            if (!multiKeysTouched) setMultiKeysTouched(true)
            setConfigForm({ ...configForm, api_keys: keys })
          }}
          serviceTier={serviceTier}
          onServiceTierChange={(tier) => {
            if (!serviceTierTouched) setServiceTierTouched(true)
            setServiceTier(tier)
          }}
          onSave={handleSaveConfig}
          onClose={closeConfig}
        />
      )}

      {showAdd && (
        <AddProviderModal
          onClose={() => setShowAdd(false)}
          onCreated={() => {
            void queryClient.invalidateQueries({ queryKey: qk.providers })
            setShowAdd(false)
          }}
        />
      )}
    </div>
  )
}

export function ProvidersView() {
  return (
    <QueryProvider>
      <ProvidersViewContent />
    </QueryProvider>
  )
}
