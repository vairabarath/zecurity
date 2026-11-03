import { ROLE_LABELS } from '@/auth/roles'
import { Badge } from '@/components/ui/badge'

/** A provider role as a badge; an unknown role is shown as-is. */
export function RoleBadge({ role }: { role: string }) {
  return (
    <Badge variant={role === 'super-admin' ? 'info' : 'secondary'} className="whitespace-nowrap">
      {ROLE_LABELS[role] ?? role}
    </Badge>
  )
}
