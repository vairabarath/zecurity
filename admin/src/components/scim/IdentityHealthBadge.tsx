import { StatusPill, relativeTime } from '@/lib/console'

type HealthTone = 'ok' | 'warn' | 'danger' | 'muted' | 'info'

// Backend-derived state (DirectoryService.IdentityHealth, M1-9b). Pure
// presentation only — never recompute thresholds client-side.
const HEALTH_TONE: Record<string, HealthTone> = {
  Healthy: 'ok',
  Delayed: 'warn',
  Disconnected: 'danger',
  Disabled: 'muted',
}

export type IdentityHealthBadgeProps = {
  identityHealth: string
  lastSyncAt?: string | null
  scimEnabled: boolean
}

// Per FE-2 spec: only show the badge for SCIM-capable connections (gate on
// scimEnabled — NOT scimEnabledAllowed, which is not on the connection type).
export function IdentityHealthBadge({
  identityHealth,
  lastSyncAt,
  scimEnabled,
}: IdentityHealthBadgeProps) {
  if (!scimEnabled) return null

  // identityHealth is String! but the resolver only assigns it inside a guard
  // (scimEnabled && ScimStore && DirectoryService, and only when IdentityHealth
  // returns no error), so it can legitimately arrive empty. Show "Unknown"
  // rather than an empty pill: this is the failure path, which is exactly when
  // an operator needs a legible signal. Hiding the badge instead would make a
  // SCIM-enabled connection look like it has no SCIM at all.
  const label = identityHealth || 'Unknown'
  const tone = HEALTH_TONE[identityHealth] ?? 'muted'

  return (
    <span className="inline-flex items-center gap-2">
      <StatusPill label={label} tone={tone} />
      <span className="shrink-0 text-xs text-muted-foreground">
        {lastSyncAt ? `last synced ${relativeTime(lastSyncAt)}` : 'never synced'}
      </span>
    </span>
  )
}
