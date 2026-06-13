import { useQuery } from '@apollo/client/react'
import { ScrollText } from 'lucide-react'
import { GetConnectorLogsDocument, type GetConnectorLogsQuery } from '@/generated/graphql'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, StatusPill, relativeTime } from '@/lib/console'

type LogEntry = GetConnectorLogsQuery['connectorLogs'][number]

interface ParsedLog {
  dest?: string
  proto?: string
  path?: 'direct' | 'shield_relay'
  decision: 'allowed' | 'denied'
}

function parseLog(msg: string): ParsedLog {
  const dest = msg.match(/dest=(\S+)/)?.[1]
  const proto = msg.match(/proto=(\S+)/)?.[1]
  const rawPath = msg.match(/path=(\S+)/)?.[1]
  const path = rawPath === 'direct' || rawPath === 'shield_relay' ? rawPath : undefined
  const decision = msg.includes(' deny ') ? 'denied' : 'allowed'
  return { dest, proto, path, decision }
}

function PathChip({ path }: { path?: 'direct' | 'shield_relay' }) {
  if (!path) return <span className="text-[12.5px] text-muted-foreground/60">—</span>
  return (
    <StatusPill
      label={path === 'shield_relay' ? 'Shield' : 'Direct'}
      tone={path === 'shield_relay' ? 'info' : 'muted'}
    />
  )
}

function LogRow({ entry }: { entry: LogEntry }) {
  const { dest, proto, path, decision } = parseLog(entry.message)
  return (
    <div className="admin-table-row grid-cols-[160px_140px_1fr_80px_120px_110px]">
      <span className="text-[12.5px] text-muted-foreground">{relativeTime(entry.createdAt)}</span>
      <span className="font-mono text-[12px] text-muted-foreground truncate">
        {entry.connectorId.slice(0, 8)}
      </span>
      <span className="truncate text-[13px]">{dest ?? '—'}</span>
      <span className="text-[12.5px] uppercase text-muted-foreground">{proto ?? '—'}</span>
      <PathChip path={path} />
      <StatusPill label={decision} tone={decision === 'allowed' ? 'ok' : 'danger'} />
    </div>
  )
}

export default function AccessLog() {
  const { data, loading } = useQuery(GetConnectorLogsDocument, {
    variables: { limit: 100 },
    pollInterval: 10_000,
    fetchPolicy: 'cache-and-network',
  })

  const logs = data?.connectorLogs ?? []

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div className="flex items-center gap-3">
          <div className="grid h-10 w-10 place-items-center rounded-xl bg-primary/10 text-primary">
            <ScrollText className="h-5 w-5" />
          </div>
          <div>
            <h1 className="text-[18px] font-semibold tracking-[-0.01em]">Access Log</h1>
            <p className="text-[13px] text-muted-foreground">RDE tunnel events — last 100 entries, auto-refreshed every 10s</p>
          </div>
        </div>
      </div>

      <div className="table-shell">
        <div className="table-scroll">
          <div className="grid min-w-[780px] grid-cols-[160px_140px_1fr_80px_120px_110px] gap-4 table-head">
            <span>Time</span>
            <span>Connector</span>
            <span>Destination</span>
            <span>Proto</span>
            <span>Path</span>
            <span>Decision</span>
          </div>

          {loading && logs.length === 0 && (
            <div className="flex flex-col gap-2 p-3">
              {Array.from({ length: 5 }).map((_, i) => (
                <Skeleton key={i} className="h-12 rounded-2xl" />
              ))}
            </div>
          )}

          {!loading && logs.length === 0 && (
            <EmptyState
              icon={<ScrollText className="h-6 w-6" />}
              title="No tunnel events yet."
              description="Events appear here once devices connect via zecurity up."
            />
          )}

          {logs.map((entry) => (
            <LogRow key={entry.id} entry={entry} />
          ))}
        </div>
      </div>
    </div>
  )
}
