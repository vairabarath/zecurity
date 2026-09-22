import { useState } from 'react'
import { cn } from '@/lib/utils'
import DeviceProfiles from '@/pages/DeviceProfiles'
import ResourcePolicies from '@/pages/ResourcePolicies'

// No Sign In Policy tab: the Policies tree in the Sprint 19 plan lists one, but
// no backend for it exists anywhere in the controller — no schema, no resolver,
// no store — so a tab would be a control that cannot do anything.
type Tab = 'resource-policies' | 'device-profiles'

const TAB_LABELS: Record<Tab, string> = {
  'resource-policies': 'Resource Policies',
  'device-profiles': 'Device Profiles',
}

export default function Policies() {
  const [tab, setTab] = useState<Tab>('resource-policies')

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Policies</h2>
          <p className="page-subtitle">Manage access and posture policies.</p>
        </div>
      </div>

      <div className="flex items-center gap-1.5">
        {(Object.keys(TAB_LABELS) as Tab[]).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={cn(
              'rounded-full px-4 py-1.5 text-xs font-bold transition',
              tab === t
                ? 'border border-primary/30 bg-primary/12 text-primary'
                : 'bg-secondary text-muted-foreground hover:text-foreground',
            )}
          >
            {TAB_LABELS[t]}
          </button>
        ))}
      </div>

      {tab === 'resource-policies' && <ResourcePolicies />}
      {tab === 'device-profiles' && <DeviceProfiles />}
    </div>
  )
}
