import { useState } from 'react'
import { useQuery } from '@apollo/client/react'
import { GetDiscoveredServicesDocument } from '@/generated/graphql'
import { Skeleton } from '@/components/ui/skeleton'
import { relativeTime } from '@/lib/console'
import { PromoteServiceModal } from '@/components/PromoteServiceModal'

interface DiscoveredServicesPanelProps {
  shieldId: string
}

interface PromoteTarget {
  protocol: string
  port: number
  serviceName: string
  boundIp: string
}

export function DiscoveredServicesPanel({ shieldId }: DiscoveredServicesPanelProps) {
  const { data, loading, error, refetch } = useQuery(GetDiscoveredServicesDocument, {
    variables: { shieldId },
    pollInterval: 30000,
    fetchPolicy: 'cache-and-network',
  })
  const [promote, setPromote] = useState<PromoteTarget | null>(null)

  const services = data?.getDiscoveredServices ?? []

  return (
    <div className="rounded-2xl border border-border bg-secondary/40 p-4">
      <div className="mb-3 flex items-center justify-between">
        <h4 className="text-sm font-semibold">Discovered Services</h4>
        <span className="text-xs text-muted-foreground">Auto-refreshes every 30s</span>
      </div>

      {error ? (
        <div className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
          {error.message}
        </div>
      ) : loading && services.length === 0 ? (
        <div className="space-y-2">
          {Array.from({ length: 3 }).map((_, i) => (
            <Skeleton key={i} className="h-9 rounded-lg bg-secondary" />
          ))}
        </div>
      ) : services.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border px-3 py-6 text-center text-xs text-muted-foreground">
          No services discovered yet. Shield scans every 60s.
        </div>
      ) : (
        <div className="overflow-hidden rounded-lg border border-border">
          <div className="grid grid-cols-[80px_80px_1fr_140px_120px_120px_110px] gap-3 border-b border-border bg-secondary px-3 py-2 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
            <div>Protocol</div>
            <div>Port</div>
            <div>Service</div>
            <div>Bound IP</div>
            <div>First Seen</div>
            <div>Last Seen</div>
            <div className="text-right">Action</div>
          </div>
          {services.map((s) => (
            <div
              key={`${s.protocol}-${s.port}-${s.boundIp}`}
              className="grid grid-cols-[80px_80px_1fr_140px_120px_120px_110px] items-center gap-3 border-b border-border px-3 py-2 text-sm last:border-b-0"
            >
              <div className="font-mono text-xs uppercase">{s.protocol}</div>
              <div className="font-mono text-xs">{s.port}</div>
              <div>
                <span className="inline-flex items-center rounded-full bg-secondary px-2 py-0.5 text-xs font-medium text-foreground">
                  {s.serviceName}
                </span>
              </div>
              <div className="font-mono text-xs text-muted-foreground">{s.boundIp}</div>
              <div className="text-xs text-muted-foreground">{relativeTime(s.firstSeen)}</div>
              <div className="text-xs text-muted-foreground">{relativeTime(s.lastSeen)}</div>
              <div className="text-right">
                <button
                  onClick={() =>
                    setPromote({
                      protocol: s.protocol,
                      port: s.port,
                      serviceName: s.serviceName,
                      boundIp: s.boundIp,
                    })
                  }
                  className="rounded-lg border border-border bg-background px-3 py-1 text-xs font-semibold text-primary transition hover:bg-secondary"
                >
                  Promote
                </button>
              </div>
            </div>
          ))}
        </div>
      )}

      {promote ? (
        <PromoteServiceModal
          shieldId={shieldId}
          protocol={promote.protocol}
          port={promote.port}
          serviceName={promote.serviceName}
          boundIp={promote.boundIp}
          onClose={() => {
            setPromote(null)
            refetch()
          }}
        />
      ) : null}
    </div>
  )
}
