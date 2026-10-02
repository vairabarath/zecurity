import { Badge } from '@/components/ui/badge'

const LABELS: Record<string, string> = {
  'super-admin': 'Super-admin',
  'relay-ops': 'Relay ops',
}

/** A provider role as a badge; an unknown role is shown as-is. */
export function RoleBadge({ role }: { role: string }) {
  return (
    <Badge variant="outline" className="border-primary/40 text-primary">
      {LABELS[role] ?? role}
    </Badge>
  )
}
