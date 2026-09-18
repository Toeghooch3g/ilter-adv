import type { FeatureFlag } from '../../lib/api'
import { featureLabel } from '../../lib/format'
import { Badge } from '../ui/badge'
import { AlertTriangle } from '../ui/icons'

interface FeatureControlCenterProps {
  features: FeatureFlag[]
}

export function FeatureControlCenter({ features }: FeatureControlCenterProps) {
  if (!features || features.length === 0) return null

  return (
    <section>
      <h2 className="text-lg font-semibold text-surface-900 mb-4">Feature Control Center</h2>
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
        {features.map((feature) => (
          <div
            key={feature.feature_key}
            className="rounded-xl border border-surface-200 bg-white p-4 shadow-card flex items-start justify-between"
          >
            <div className="flex-1 min-w-0">
              <p className="text-sm font-medium text-surface-900">{featureLabel(feature.feature_key)}</p>
              {feature.warning && <p className="text-xs text-warning mt-1">{feature.warning}</p>}
            </div>
            <div className="relative flex items-center gap-2 shrink-0">
              {feature.warning && (
                <div className="group relative">
                  <AlertTriangle size={16} className="text-warning cursor-help" />
                  <div
                    role="tooltip"
                    className="hidden group-hover:block group-focus-within:block absolute right-0 top-full mt-1 z-10 w-64 rounded-lg bg-surface-800 px-3 py-2 text-xs text-white shadow-lg"
                  >
                    {feature.warning}
                  </div>
                </div>
              )}
              <Badge variant={feature.enabled ? 'success' : 'default'}>
                {feature.enabled ? 'Enabled' : 'Disabled'}
              </Badge>
            </div>
          </div>
        ))}
      </div>
    </section>
  )
}
