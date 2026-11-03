import { useState } from 'react'
import { cn } from '@/lib/utils'
import DeviceProfiles from '@/pages/DeviceProfiles'

type Tab = 'device-profiles'

const TAB_LABELS: Record<Tab, string> = {
  'device-profiles': 'Device Policies',
}

export default function Policies() {
  const [tab, setTab] = useState<Tab>('device-profiles')

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

      {tab === 'device-profiles' && <DeviceProfiles />}
    </div>
  )
}
