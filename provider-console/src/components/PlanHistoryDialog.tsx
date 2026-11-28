import { listPlanRevisions } from '@/api/billing'
import type { PlanCardSummary } from '@/api/types'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Skeleton } from '@/components/ui/skeleton'
import { formatDateTime } from '@/lib/format'
import { describeRevision } from '@/lib/plan-revisions'
import { useDetailQuery } from '@/lib/useDetailQuery'

// A plan card's history (Sprint 26 P4, D-63): every revision with who, when,
// the reason and what it changed. Read once when opened.

export function PlanHistoryDialog({ plan, onClose }: { plan: PlanCardSummary; onClose: () => void }) {
  const { state, reload } = useDetailQuery(plan.id, listPlanRevisions)
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[85vh] overflow-y-auto overscroll-contain sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>History of {plan.name}</DialogTitle>
          <DialogDescription>Every change to the card, newest first. A month is billed at the revision in force on its 1st.</DialogDescription>
        </DialogHeader>
        {state.status === 'loading' && <Skeleton className="h-24 w-full" aria-label="Loading history" />}
        {(state.status === 'error' || state.status === 'not_found' || state.status === 'forbidden') && (
          <Alert variant="destructive">
            <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
              <span>Couldn’t load the history.</span>
              <Button variant="outline" size="sm" onClick={reload}>
                Retry
              </Button>
            </AlertDescription>
          </Alert>
        )}
        {state.status === 'ready' && <RevisionList items={state.data.items} />}
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function RevisionList({ items }: { items: Awaited<ReturnType<typeof listPlanRevisions>>['items'] }) {
  if (items.length === 0) return <p className="text-sm text-muted-foreground">No revisions yet.</p>
  const byRevision = new Map(items.map((r) => [r.revision, r]))
  const sorted = [...items].sort((a, b) => b.revision - a.revision)
  return (
    <ol className="flex flex-col gap-3" aria-label="Revisions">
      {sorted.map((r) => (
        <li key={r.revision} className="rounded-md border border-border p-3 text-sm" aria-label={`Revision ${r.revision}`}>
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <span className="font-medium">Revision {r.revision}</span>
            <span className="text-[13px] text-muted-foreground">
              {formatDateTime(r.changed_at)} · <span className="break-all">{r.changed_by}</span>
            </span>
          </div>
          <ul className="mt-1.5 list-disc pl-5">
            {describeRevision(r.card, byRevision.get(r.revision - 1)?.card).map((line) => (
              <li key={line} className="break-words">
                {line}
              </li>
            ))}
          </ul>
          {r.reason && <p className="mt-1.5 break-words text-[13px] text-muted-foreground">Reason: {r.reason}</p>}
        </li>
      ))}
    </ol>
  )
}
