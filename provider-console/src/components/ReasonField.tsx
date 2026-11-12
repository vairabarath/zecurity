import { Label } from '@/components/ui/label'
import { MAX_DANGER_REASON } from '@/components/ConfirmDangerDialog'
import { reasonLength } from '@/lib/pool-format'
import { cn } from '@/lib/utils'

// The required, audited reason (D-17 style) for the pool dialogs; it looks and
// counts like ConfirmDangerDialog's. The caller trims it before sending.
export function ReasonField({ id, value, onChange, error }: { id: string; value: string; onChange: (v: string) => void; error?: string }) {
  const length = reasonLength(value)
  const tooLong = length > MAX_DANGER_REASON
  const message = error ?? (tooLong ? 'Keep the reason to 500 characters or fewer.' : undefined)
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>Reason</Label>
      <textarea
        id={id}
        value={value}
        rows={3}
        autoComplete="off"
        aria-invalid={message !== undefined}
        aria-describedby={`${id}-hint`}
        onChange={(e) => onChange(e.target.value)}
        className="flex w-full rounded-lg border border-input bg-field px-3 py-2 text-sm transition-colors placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/30 aria-invalid:border-destructive"
      />
      <p id={`${id}-hint`} className="flex justify-between gap-3 text-xs">
        <span className={cn(message ? 'text-destructive' : 'text-muted-foreground')}>{message ?? 'Required. Recorded in the provider audit log.'}</span>
        <span className={cn('tabular-nums', tooLong ? 'text-destructive' : 'text-muted-foreground')}>
          {length} / {MAX_DANGER_REASON}
        </span>
      </p>
    </div>
  )
}
