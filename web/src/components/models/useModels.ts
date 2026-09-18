import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'
import { api } from '../../lib/api'
import { logger } from '../../lib/logger'
import { qk, queryClient } from '../../lib/query'
import { useApiMutation } from '../../lib/useApiMutation'
import { useExport } from '../ui/useExport'

export interface Model {
  id: string
  name: string
  provider: string
  model: string
  is_active: boolean
  category: string
  cost_per_1m_in: number
  cost_per_1m_out: number
  cost_per_1m_cached_in: number
  cost_per_1m_cache_write: number
}

export const categories = ['free', 'economy', 'standard', 'premium'] as const
export type ModelCategory = (typeof categories)[number]

/** Formats cost values (dollars per 1M tokens) concisely.
 *  - $0 → "0"
 *  - $0.0000014 → "0.000001" (strips trailing zeros)
 *  - $0.001 → "0.001"
 *  - $0.14 → "0.14"
 */
export function formatCost(cost: number): string {
  if (cost === 0) return '0'
  const s = cost.toFixed(10).replace(/\.?0+$/, '')
  if (s.length > 8) {
    return Number(cost)
      .toFixed(6)
      .replace(/\.?0+$/, '')
  }
  return s
}

export function useModels() {
  const { exportCsv } = useExport()

  const {
    data: models = [],
    isLoading,
    error,
    refetch,
  } = useQuery({
    queryKey: qk.models,
    queryFn: () =>
      api.models.getModelProviders().then((items) =>
        items.map((item) => ({
          id: item.id,
          name: item.name,
          provider: item.provider,
          model: item.model,
          is_active: item.is_active,
          category: (item.category || 'standard') as Model['category'],
          cost_per_1m_in: item.cost_per_1m_in || 0,
          cost_per_1m_out: item.cost_per_1m_out || 0,
          cost_per_1m_cached_in: item.cost_per_1m_cached_in || 0,
          cost_per_1m_cache_write: item.cost_per_1m_cache_write || 0,
        })),
      ),
  })

  // User-manageable category list (defaults + user-added). Falls back to the
  // out-of-box defaults if the API call fails so the view stays usable.
  const staticCategories = [...categories] as string[]
  const { data: categoryList = staticCategories } = useQuery({
    queryKey: qk.categories,
    queryFn: () => api.models.getCategories(),
  })
  const effectiveCategories = categoryList.length > 0 ? categoryList : staticCategories

  const [search, setSearch] = useState('')
  const [categoryFilter, setCategoryFilter] = useState<string | null>(null)
  const [showAddModal, setShowAddModal] = useState(false)
  const [configModel, setConfigModel] = useState<Model | null>(null)
  const [configForm, setConfigForm] = useState({
    name: '',
    provider: '',
    model: '',
    category: 'standard' as Model['category'],
    cost_in: 0,
    cost_out: 0,
  })

  const filtered = models.filter((m) => {
    if (categoryFilter && m.category !== categoryFilter) return false
    const q = search.toLowerCase()
    return m.name.toLowerCase().includes(q) || m.provider.toLowerCase().includes(q) || m.model.toLowerCase().includes(q)
  })

  const toggleModelMutation = useApiMutation(
    ({ provider, name, active }: { provider: string; name: string; active: boolean }) =>
      api.models.toggleModel(provider, name, active),
    { invalidate: [qk.models] },
  )

  const toggleModel = async (id: string) => {
    const model = models.find((m) => m.id === id)
    if (!model) return
    const newActive = !model.is_active
    queryClient.setQueryData(qk.models, (old: Model[] | undefined) =>
      (old || []).map((m) => (m.id === id ? { ...m, is_active: newActive } : m)),
    )
    try {
      // Key by the bare model id + provider (model.model/model.provider), not
      // the prettified display name (model.name): the backend updates the
      // provider_models row by (provider, model) composite key.
      await toggleModelMutation.mutateAsync({ provider: model.provider, name: model.model, active: newActive })
      toast.success(newActive ? 'Model enabled' : 'Model disabled', {
        description: `${model.name} is now ${newActive ? 'active' : 'disabled'}.`,
      })
    } catch (e) {
      logger.error('Failed to toggle model', { name: model.name, active: newActive, error: e })
      queryClient.setQueryData(qk.models, (old: Model[] | undefined) =>
        (old || []).map((m) => (m.id === id ? { ...m, is_active: !newActive } : m)),
      )
      toast.error('Toggle failed', { description: `Could not update ${model.name}.` })
    }
  }

  const updateCategory = useApiMutation(
    ({ name, category }: { name: string; category: string }) => api.models.updateModelCategory(name, category),
    { invalidate: [qk.models] },
  )

  const addCategoryMutation = useApiMutation((name: string) => api.models.createCategory(name), {
    invalidate: [qk.categories],
  })

  const removeCategoryMutation = useApiMutation((name: string) => api.models.deleteCategory(name), {
    invalidate: [qk.categories],
  })

  const handleSaveConfig = async () => {
    if (!configModel) return
    try {
      // Persist the active toggle from the Config modal first (the modal only
      // flips local state; the backend updates provider_models by
      // (provider, model) composite key), then the category.
      await api.models.toggleModel(configModel.provider, configModel.model, configModel.is_active)
      // Key by the bare model id (configModel.model), not the prettified
      // display name (configModel.name): the backend updates provider_models
      // by bare model id.
      await updateCategory.mutateAsync({ name: configModel.model, category: configModel.category })
      toast.success('Model updated', { description: `${configModel.name} category set to ${configModel.category}.` })
      setConfigModel(null)
    } catch (e) {
      logger.error('Failed to update model category', {
        name: configModel.model,
        category: configModel.category,
        error: e,
      })
      toast.error('Update failed', { description: `Could not update ${configModel.name}.` })
    }
  }

  const addModel = useApiMutation(
    (data: { name: string; provider: string; model: string; category: string; cost_in: number; cost_out: number }) =>
      api.models.updateModelCategory(data.name, data.category),
    { invalidate: [qk.models] },
  )

  const handleAddProvider = () => {
    const name = configForm.name
    const newModel: Model = {
      id: `m${Date.now()}`,
      name: configForm.name,
      provider: configForm.provider,
      model: configForm.model,
      is_active: true,
      category: configForm.category,
      cost_per_1m_in: configForm.cost_in,
      cost_per_1m_out: configForm.cost_out,
      cost_per_1m_cached_in: 0,
      cost_per_1m_cache_write: 0,
    }
    queryClient.setQueryData(qk.models, (old: Model[] | undefined) => [...(old || []), newModel])
    setShowAddModal(false)
    setConfigForm({ name: '', provider: '', model: '', category: 'standard', cost_in: 0, cost_out: 0 })
    toast.success('Model added', { description: `"${name}" has been added to the registry.` })
    addModel.mutate({
      name: configForm.name,
      provider: configForm.provider,
      model: configForm.model,
      category: configForm.category,
      cost_in: configForm.cost_in,
      cost_out: configForm.cost_out,
    })
  }

  const exportModels = () => {
    exportCsv(
      filtered.map((m) => ({
        Name: m.name,
        Provider: m.provider,
        Model: m.model,
        Status: m.is_active ? 'Active' : 'Disabled',
        Category: m.category,
        'Cost/1M In': m.cost_per_1m_in,
        'Cost/1M Out': m.cost_per_1m_out,
      })),
      [
        { key: 'Name' as const, header: 'Name' },
        { key: 'Provider' as const, header: 'Provider' },
        { key: 'Model' as const, header: 'Model' },
        { key: 'Status' as const, header: 'Status' },
        { key: 'Category' as const, header: 'Category' },
        { key: 'Cost/1M In' as const, header: 'Cost/1M In' },
        { key: 'Cost/1M Out' as const, header: 'Cost/1M Out' },
      ],
      'models.csv',
    )
  }

  return {
    models,
    filtered,
    isLoading,
    error,
    refetch,
    search,
    setSearch,
    categoryFilter,
    setCategoryFilter,
    showAddModal,
    setShowAddModal,
    configModel,
    setConfigModel,
    configForm,
    setConfigForm,
    toggleModel,
    updateCategory,
    handleSaveConfig,
    handleAddProvider,
    exportModels,
    formatCost,
    categories: effectiveCategories,
    addCategory: addCategoryMutation.mutateAsync,
    removeCategory: removeCategoryMutation.mutateAsync,
  }
}
