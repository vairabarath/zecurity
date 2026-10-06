import { Badge, type BadgeProps } from '@/components/ui/badge'

type Variant = NonNullable<BadgeProps['variant']>

// Status values the read APIs return (relays, tenants, connectors, shields,
// remote networks). The badge only colours the API's own value; it never
// derives a status.
const VARIANTS: Record<string, Variant> = {
  active: 'success',
  pending: 'outline',
  provisioning: 'outline',
  inactive: 'warning',
  disconnected: 'warning',
  suspended: 'warning',
  revoked: 'destructive',
  deleted: 'secondary',
}

/** A lifecycle status as a badge; an unknown value is shown as-is, neutral. */
export function StatusBadge({ status }: { status: string }) {
  return (
    <Badge variant={VARIANTS[status] ?? 'outline'} className="whitespace-nowrap capitalize" data-status={status}>
      {status}
    </Badge>
  )
}
