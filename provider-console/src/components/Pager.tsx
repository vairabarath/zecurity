import { ChevronLeft, ChevronRight } from 'lucide-react'
import { Button } from '@/components/ui/button'

/** Previous / Next for cursor paging. Previous walks the client's cursor stack. */
export function Pager({
  page,
  hasPrev,
  hasNext,
  onPrev,
  onNext,
}: {
  page: number
  hasPrev: boolean
  hasNext: boolean
  onPrev: () => void
  onNext: () => void
}) {
  if (!hasPrev && !hasNext) return null
  return (
    <nav aria-label="Pagination" className="flex items-center justify-end gap-2 pt-3">
      <Button variant="outline" size="sm" onClick={onPrev} disabled={!hasPrev}>
        <ChevronLeft aria-hidden />
        Previous
      </Button>
      <span className="text-xs text-muted-foreground" aria-live="polite">
        Page {page}
      </span>
      <Button variant="outline" size="sm" onClick={onNext} disabled={!hasNext}>
        Next
        <ChevronRight aria-hidden />
      </Button>
    </nav>
  )
}
