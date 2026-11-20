import { ApiError } from '@/api/client'
import type { AlertSeverity } from '@/api/alerts'

// Labels and messages for the Alerts page (Sprint 24 A3, F26).

export const KIND_LABELS: Record<string, string> = {
  'relay.offline': 'Relay offline',
  'relay.over_capacity': 'Relay near or over capacity',
  'pool.no_eligible_relay': 'Relay pool without a relay',
  'controlplane.degraded': 'Control plane degraded',
  'certificate.expiring': 'Platform certificate expiring',
  'connector.offline': 'Connector offline',
  'shield.offline': 'Shield offline',
  'tenant.no_relay_path': 'Tenant without a relay path',
  'tenant.over_quota': 'Tenant over quota',
}

export function kindLabel(kind: string): string {
  return KIND_LABELS[kind] ?? kind
}

export const SEVERITY_LABELS: Record<AlertSeverity, string> = { critical: 'Critical', warning: 'Warning', info: 'Info' }

export function severityVariant(s: string): 'destructive' | 'warning' | 'info' {
  return s === 'critical' ? 'destructive' : s === 'warning' ? 'warning' : 'info'
}

export const SCOPE_FILTER_LABELS: Record<string, string> = {
  all: 'All alerts',
  platform: 'Platform alerts only',
  tenant: 'Tenant alerts only',
}

/** 0 → "at once"; 90 → "1 min 30 s"; 3600 → "1 h". */
export function formatSeconds(s: number): string {
  if (s <= 0) return 'at once'
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  return [h && `${h} h`, m && `${m} min`, sec && `${sec} s`].filter(Boolean).join(' ')
}

const ALERT_ERRORS: Record<string, string> = {
  reason_required: 'Enter a reason.',
  reason_too_long: 'Keep the reason to 500 characters or fewer.',
  invalid_params: 'Those parameters aren’t valid for this rule.',
  invalid_rule: 'Check the rule’s values: severity, delays and name.',
  rule_name_taken: 'Another rule has that name.',
  invalid_channel: 'Check the channel’s values (name, filters, recipients).',
  invalid_url: 'The webhook URL was refused: use https and a public host name, without credentials.',
  channel_name_taken: 'Another channel has that name.',
  not_found: 'It no longer exists. Refresh the page.',
  forbidden: 'Your role can’t do that.',
}

export function alertErrorMessage(err: unknown): string {
  if (err instanceof ApiError) return ALERT_ERRORS[err.code] ?? 'Something went wrong. Try again.'
  return 'Couldn’t reach the controller. Try again.'
}

/** Whole seconds from a text field, or null when it isn't one in range. */
export function parseSeconds(v: string, min: number, max: number, zeroOk: boolean): number | null {
  const t = v.trim()
  if (!/^\d{1,6}$/.test(t)) return null
  const n = Number(t)
  if (n === 0) return zeroOk ? 0 : null
  return n >= min && n <= max ? n : null
}

const EMAIL_RE = /^[^\s@<>()",;]+@[^\s@<>()",;]+\.[^\s@<>()",;]+$/

/** Comma- or line-separated emails; null when any is invalid, [] when empty. */
export function parseRecipients(v: string): string[] | null {
  const list = v
    .split(/[\s,;]+/)
    .map((s) => s.trim())
    .filter(Boolean)
  if (list.length > 50) return null
  return list.every((e) => EMAIL_RE.test(e)) ? list : null
}
