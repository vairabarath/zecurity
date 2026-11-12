import { useState } from 'react'
import { useQuery } from '@apollo/client/react'
import { ChevronLeft, ChevronRight, Clock3, History } from 'lucide-react'
import { GetAuditLogsDocument, type GetAuditLogsQuery } from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, ErrorState, relativeTime } from '@/lib/console'

// PENDING-11 S-9: the workspace's admin audit stream, read-only (CONSOLE_READ).
// Each row is hash-chained; the same rows are what SIEM export sinks receive.

type Entry = GetAuditLogsQuery['auditLogs']['entries'][number]

export const AUDIT_PAGE_SIZE = 50

function isFailure(action: string): boolean {
  return action.endsWith('_failed') || action.endsWith('_denied')
}

function DetailsCell({ details }: { details?: string | null }) {
  const [open, setOpen] = useState(false)
  if (!details) return <span className="text-[12.5px] text-muted-foreground/60">—</span>
  let pretty = details
  try {
    pretty = JSON.stringify(JSON.parse(details), null, 2)
  } catch {
    // Show the raw text.
  }
  return (
    <div className="min-w-0">
      <button
        type="button"
        className="max-w-full truncate text-left font-mono text-[12px] text-muted-foreground hover:text-foreground"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
      >
        {open ? 'Hide details' : details}
      </button>
      {open ? (
        <pre className="mt-2 max-h-64 overflow-auto rounded-lg border border-border bg-secondary p-3 font-mono text-[11.5px] text-foreground">
          {pretty}
        </pre>
      ) : null}
    </div>
  )
}

export default function AuditLog() {
  // Page cursors: `before` of each page we've moved past, newest first.
  const [cursors, setCursors] = useState<number[]>([])
  const [filter, setFilter] = useState('')
  const [appliedFilter, setAppliedFilter] = useState('')
  const before = cursors.length ? cursors[cursors.length - 1] : undefined

  const { data, loading, error, refetch } = useQuery(GetAuditLogsDocument, {
    variables: { before, limit: AUDIT_PAGE_SIZE, actionPrefix: appliedFilter || undefined },
    fetchPolicy: 'cache-and-network',
  })

  const entries: Entry[] = data?.auditLogs.entries ?? []
  const nextBefore = data?.auditLogs.nextBefore ?? null

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Audit Log</h2>
          <p className="page-subtitle">
            Every administrative change and sign-in in this workspace, newest first. Rows are
            hash-chained and can be exported to a SIEM (Settings → Audit export).
          </p>
        </div>
        <form
          className="flex items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault()
            setCursors([])
            setAppliedFilter(filter.trim())
          }}
        >
          <Input
            aria-label="Filter by action prefix"
            placeholder="Action prefix, e.g. auth."
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            className="w-56"
          />
          <Button type="submit" variant="outline">
            Filter
          </Button>
        </form>
      </div>

      <div className="table-shell">
        <div className="table-scroll">
          <div className="table-head grid min-w-[1080px] items-center grid-cols-[80px_150px_220px_220px_200px_1fr] gap-4 px-5 py-4">
            {['Seq', 'Time', 'Action', 'Actor', 'Target', 'Details'].map((label) => (
              <div key={label} className="table-head-label">
                {label}
              </div>
            ))}
          </div>

          {error && !data ? (
            <ErrorState
              title="Could not load the audit log"
              description="The request failed. Try again."
              action={
                <Button variant="outline" onClick={() => refetch()}>
                  Retry
                </Button>
              }
            />
          ) : loading && !data ? (
            <div className="min-w-[1080px] space-y-3 p-5">
              {Array.from({ length: 5 }).map((_, i) => (
                <Skeleton key={i} className="h-12 rounded-2xl bg-secondary" />
              ))}
            </div>
          ) : entries.length === 0 ? (
            <EmptyState
              icon={<History className="h-6 w-6" />}
              title="No audit events"
              description={appliedFilter ? `Nothing matches "${appliedFilter}".` : 'Changes appear here as they happen.'}
            />
          ) : (
            <div className="min-w-[1080px]">
              {entries.map((e) => (
                <div
                  key={e.id}
                  data-testid="audit-row"
                  className="admin-table-row grid items-start grid-cols-[80px_150px_220px_220px_200px_1fr] gap-4 px-5 py-3"
                >
                  <div className="font-mono text-[12.5px] text-muted-foreground" title={`row hash ${e.rowHash}`}>
                    {e.seq}
                  </div>
                  <div className="font-mono text-[12.5px] text-muted-foreground/80" title={new Date(e.createdAt).toLocaleString()}>
                    <span className="inline-flex items-center gap-1.5">
                      <Clock3 className="h-3.5 w-3.5 opacity-60" />
                      {relativeTime(e.createdAt)}
                    </span>
                  </div>
                  <div className="min-w-0">
                    <span
                      className={
                        'inline-flex max-w-full truncate rounded-md border px-2 py-0.5 font-mono text-[11.5px] ' +
                        (isFailure(e.action)
                          ? 'border-destructive/30 bg-destructive/10 text-destructive'
                          : 'border-border bg-secondary text-foreground')
                      }
                    >
                      {e.action}
                    </span>
                  </div>
                  <div className="min-w-0 truncate text-[13px]" title={e.actorUserId ?? 'system'}>
                    {e.actorEmail}
                  </div>
                  <div className="min-w-0 truncate font-mono text-[12px] text-muted-foreground" title={`${e.targetType} ${e.targetId}`}>
                    {e.targetType}
                    {e.targetId ? ` · ${e.targetId}` : ''}
                  </div>
                  <DetailsCell details={e.details} />
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      <div className="flex items-center justify-end gap-2">
        <Button
          variant="outline"
          disabled={cursors.length === 0}
          onClick={() => setCursors((c) => c.slice(0, -1))}
        >
          <ChevronLeft className="h-4 w-4" /> Back to newer
        </Button>
        <Button
          variant="outline"
          disabled={nextBefore === null}
          onClick={() => nextBefore !== null && setCursors((c) => [...c, nextBefore])}
        >
          Older <ChevronRight className="h-4 w-4" />
        </Button>
      </div>
    </div>
  )
}
