import { useQuery } from '@tanstack/react-query'
import { useMemo } from 'react'
import { api } from '../../lib/api'
import { qk } from '../../lib/query'

export interface ProviderStatus {
  name: string
  status: 'online' | 'offline' | 'degraded'
  activeModels: number
  totalModels: number
}

const REFETCH_INTERVAL = 30_000

export function useOverview() {
  const { data: stats, isLoading: statsLoading } = useQuery({
    queryKey: qk.dashboardStats,
    queryFn: api.dashboard.getDashboardStats,
    refetchInterval: REFETCH_INTERVAL,
  })

  const { data: providerInfos, isLoading: providersLoading } = useQuery({
    queryKey: qk.providers,
    queryFn: api.providers.getProviders,
    refetchInterval: REFETCH_INTERVAL,
  })

  const { data: costSummary } = useQuery({
    queryKey: qk.costSummary('7d'),
    queryFn: () => api.costs.getCostSummary('7d'),
  })

  const { data: features } = useQuery({
    queryKey: qk.features,
    queryFn: api.features.getFeatures,
  })

  // Real per-provider health, derived from live circuit-breaker state on the
  // backend (see providers.HandleProviders) — not merely "is a model toggled on".
  const providers: ProviderStatus[] = useMemo(() => {
    if (!providerInfos) return []
    return providerInfos.map((p) => ({
      name: p.name,
      status: (p.status as ProviderStatus['status']) || 'offline',
      activeModels: p.active_models,
      totalModels: p.total_models,
    }))
  }, [providerInfos])

  return {
    stats: stats ?? null,
    statsLoading,
    providers,
    providersLoading,
    costSummary: costSummary ?? null,
    features: features ?? [],
    loading: statsLoading,
  }
}
