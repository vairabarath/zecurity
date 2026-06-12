import { useMemo } from 'react'
import { useQuery } from '@apollo/client/react'
import { Clock3, ScrollText } from 'lucide-react'
import {
  GetConnectorLogsDocument,
  type GetConnectorLogsQuery,
} from '@/generated/graphql'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, StatusPill, relativeTime } from '@/lib/console'
import { parseConnectorLog } from '@/lib/parseConnectorLog'

type LogEntry = GetConnectorLogsQuery['connectorLogs'][number]

function shortId(id: string): string {
  return id.length > 10 ? id.slice(0, 8) : id
}

function PathChip({ path }: { path?: 'direct' | 'shield_relay' }) {
  if (!path) return <span className="text-[12.5px] text-muted-foreground/60">—</span>
  const label = path === 'shield_relay' ? 'Shield' : 'Direct'
  return <StatusPill label={label} tone={path === 'shield_relay' ? 'info' : 'muted'} />
}

function DecisionChip({ decision }: { decision?: 'allowed' | 'denied' }) {
  if (!decision) return <span className="text-[12.5px] text-muted-foreground/60">—</span>
  return <StatusPill label={decision} tone={decision === 'allowed' ? 'ok' : 'danger'} />
}

export default function AccessLog() {
  const { data, loading } = useQuery(GetConnectorLogsDocument, {
    variables: { limit: 100 },
    fetchPolicy: 'cache-and-network',
    pollInterval: 10000,
  })

  const logs = useMemo<LogEntry[]>(() => data?.connectorLogs ?? [], [data])

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Access Log</h2>
          <p className="page-subtitle">Tunnel attempts across all connectors. Refreshes every 10s.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="status-pill border-border bg-secondary text-muted-foreground">
            <span className="font-bold text-foreground">{logs.length}</span> events
          </span>
        </div>
      </div>

      <div className="table-shell">
        <div className="table-scroll">
          <div className="table-head grid min-w-[1080px] items-center grid-cols-[160px_140px_1fr_120px_120px] gap-4 px-5 py-4">
            {['Time', 'Connector', 'Resource', 'Path', 'Decision'].map((label) => (
              <div key={label} className="table-head-label">{label}</div>
            ))}
          </div>

          {loading && !data ? (
            <div className="min-w-[1080px] p-5 space-y-3">
              {Array.from({ length: 5 }).map((_, index) => (
                <Skeleton key={index} className="h-14 rounded-2xl bg-secondary" />
              ))}
            </div>
          ) : logs.length === 0 ? (
            <EmptyState
              icon={<ScrollText className="h-6 w-6" />}
              title="No tunnel events yet"
              description="Events appear here once a client connects through a Connector."
            />
          ) : (
            <div className="min-w-[1080px]">
              {logs.map((entry) => {
                const parsed = parseConnectorLog(entry.message)
                const resourceLabel = parsed.destination
                  ? parsed.port
                    ? `${parsed.destination}:${parsed.port}`
                    : parsed.destination
                  : entry.message
                return (
                  <div key={entry.id} className="admin-table-row grid items-center grid-cols-[160px_140px_1fr_120px_120px] gap-4 px-5 py-4">
                    <div className="font-mono text-[12.5px] text-muted-foreground/80" title={new Date(entry.createdAt).toLocaleString()}>
                      <span className="inline-flex items-center gap-1.5">
                        <Clock3 className="h-3.5 w-3.5 opacity-60" />
                        {relativeTime(entry.createdAt)}
                      </span>
                    </div>

                    <div>
                      <span className="inline-flex items-center rounded-md border border-border bg-secondary px-2 py-0.5 font-mono text-[11px] text-muted-foreground">
                        {shortId(entry.connectorId)}
                      </span>
                    </div>

                    <div className="min-w-0">
                      <div className="truncate font-mono text-[13px] text-foreground">{resourceLabel}</div>
                      {parsed.protocol ? (
                        <div className="truncate text-[10.5px] uppercase tracking-wide text-muted-foreground/60">{parsed.protocol}</div>
                      ) : null}
                    </div>

                    <div><PathChip path={parsed.path} /></div>
                    <div><DecisionChip decision={parsed.decision} /></div>
                  </div>
                )
              })}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
