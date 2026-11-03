import type { ReactNode } from 'react'
import { Navigate } from 'react-router-dom'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import type { PagedState } from '@/lib/usePagedQuery'

// Loading / error / invalid / empty handling shared by the read pages, with
// C-a's error behaviour:
//   session ended → nothing here (the guards already show Login)
//   role 403      → the Forbidden page (session kept)
//   400           → "Invalid filter" with a reset action
//   other errors  → "Couldn't load…" with an explicit Retry
// Retry is always a user action; nothing here requests anything by itself.
export function DataState<R>({
  state,
  isEmpty,
  emptyMessage,
  what,
  onRetry,
  onReset,
  children,
}: {
  state: PagedState<R>
  /** Whether the loaded data has nothing to show. */
  isEmpty: (data: R) => boolean
  emptyMessage: string
  /** Noun for messages, e.g. "relays". */
  what: string
  onRetry: () => void
  onReset: () => void
  children: (data: R) => ReactNode
}) {
  switch (state.status) {
    case 'loading':
      return (
        <div aria-busy="true" aria-label={`Loading ${what}`} className="flex flex-col gap-2">
          <Skeleton className="h-8 w-full" />
          <Skeleton className="h-8 w-full" />
          <Skeleton className="h-8 w-2/3" />
        </div>
      )
    case 'session_ended':
      return null
    case 'forbidden':
      return <Navigate to="/forbidden" replace />
    case 'invalid':
      return (
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
            <span>Invalid filter. The link may be out of date.</span>
            <Button variant="outline" size="sm" onClick={onReset}>
              Reset filters
            </Button>
          </AlertDescription>
        </Alert>
      )
    case 'error':
      return (
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
            <span>Couldn’t load {what}.</span>
            <Button variant="outline" size="sm" onClick={onRetry}>
              Retry
            </Button>
          </AlertDescription>
        </Alert>
      )
    case 'ready':
      if (isEmpty(state.data)) return <p className="py-6 text-sm text-muted-foreground">{emptyMessage}</p>
      return <>{children(state.data)}</>
  }
}
