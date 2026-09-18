import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { api } from '../../lib/api'
import { qk } from '../../lib/query'
import { EmptyState } from './empty-state'
import { QueryProvider } from './query-provider'

interface FeatureGateProps {
  featureKey: string
  children: ReactNode
}

/**
 * FeatureGate renders its children only when the named feature is enabled
 * (feature:chat / feature:jobs). While the features list is loading it shows
 * a subtle placeholder; when disabled it renders a friendly empty state so
 * the /chat and /jobs views reflect the runtime toggle.
 */
export function FeatureGate({ featureKey, children }: FeatureGateProps) {
  return (
    <QueryProvider>
      <FeatureGateContent featureKey={featureKey}>{children}</FeatureGateContent>
    </QueryProvider>
  )
}

function FeatureGateContent({ featureKey, children }: FeatureGateProps) {
  const { data: flags, isLoading } = useQuery({
    queryKey: qk.features,
    queryFn: () => api.features.getFeatures(),
  })

  if (isLoading) {
    return <div className="h-64 animate-pulse rounded-xl bg-surface-100" />
  }

  const enabled = (flags ?? []).find((f) => f.feature_key === featureKey)?.enabled ?? true
  if (!enabled) {
    return <EmptyState title="Feature disabled" description="This feature has been turned off by an administrator." />
  }
  return <>{children}</>
}
