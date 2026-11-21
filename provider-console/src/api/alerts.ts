import { request } from './client'
import { buildQuery } from './reads'
import type { Page } from './types'

// Alerts (Sprint 24 F26: A1 rules and evaluator, A2 delivery, A3 console;
// D-50, D-52, D-53, D-30 as amended). The controller decides every request
// (decide()): alert reads are super-admin, or relay-ops filtered to relay
// and pool alerts (no tenant alerts, no tenant names); rules and channels are
// super-admin only. Every write carries a reason in the body, never the URL;
// it is audited. A webhook secret is returned only by create and rotate,
// once; the console shows it in a dialog and never stores it.

export type AlertScope = 'platform' | 'tenant'
export type AlertSeverity = 'critical' | 'warning' | 'info'
export type ScopeFilter = 'all' | AlertScope
export type ChannelType = 'webhook' | 'email'

export interface ActiveAlert {
  rule_id: string
  rule_name: string
  kind: string
  scope: AlertScope
  tenant_id: string | null
  target_type: string
  target_id: string
  target_name: string
  status: 'pending' | 'firing'
  severity: AlertSeverity
  summary: string
  pending_since: string
  firing_since: string | null
}

export interface AlertEvent {
  id: string
  rule_id: string
  kind: string
  scope: AlertScope
  tenant_id: string | null
  target_type: string
  target_id: string
  target_name: string
  transition: 'fired' | 'renotified' | 'resolved'
  status: 'firing' | 'resolved'
  severity: AlertSeverity
  summary: string
  started_at: string
  occurred_at: string
}

export interface AlertRuleParams {
  threshold_percent?: number
  days?: number
  components?: string[]
  keys?: string[]
}

export interface AlertRule {
  id: string
  kind: string
  scope: AlertScope
  name: string
  enabled: boolean
  severity: AlertSeverity
  params: AlertRuleParams
  for_seconds: number
  renotify_seconds: number
  created_at: string
  updated_at: string
}

export interface AlertChannel {
  id: string
  name: string
  type: ChannelType
  scope_filter: ScopeFilter
  min_severity: AlertSeverity
  enabled: boolean
  /** Scheme and host only: the path of a webhook URL is often a credential. */
  url_host: string | null
  /** Email; null = every active super-admin. */
  recipients: string[] | null
  created_at: string
  updated_at: string
}

export interface AlertDelivery {
  channel_id: string
  channel_name: string
  attempt: number
  status: 'delivered' | 'retrying' | 'failed'
  http_status: number | null
  error: string | null
  at: string
}

export interface ChannelTestResult {
  delivered: boolean
  http_status: number | null
  error?: string
}

export interface AlertReadParams {
  scope?: '' | AlertScope
  tenant_id?: string
  status?: '' | 'pending' | 'firing'
}

export interface AlertEventParams {
  scope?: '' | AlertScope
  tenant_id?: string
  cursor?: string
  limit?: number
}

const ACTIVE_KEYS = ['scope', 'tenant_id', 'status'] as const
const EVENT_KEYS = ['scope', 'tenant_id', 'cursor', 'limit'] as const

// Reads.

export function listActiveAlerts(params?: AlertReadParams): Promise<{ items: ActiveAlert[]; truncated: boolean }> {
  return request<{ items: ActiveAlert[]; truncated: boolean }>('GET', '/provider/alerts' + buildQuery(params, ACTIVE_KEYS))
}

export function listAlertEvents(params?: AlertEventParams): Promise<Page<AlertEvent>> {
  return request<Page<AlertEvent>>('GET', '/provider/alert-events' + buildQuery(params, EVENT_KEYS))
}

export function listAlertDeliveries(eventId: string): Promise<{ items: AlertDelivery[] }> {
  return request<{ items: AlertDelivery[] }>('GET', `/provider/alert-events/${encodeURIComponent(eventId)}/deliveries`)
}

export function listAlertRules(): Promise<{ items: AlertRule[] }> {
  return request<{ items: AlertRule[] }>('GET', '/provider/alert-rules')
}

export function listAlertChannels(): Promise<{ items: AlertChannel[] }> {
  return request<{ items: AlertChannel[] }>('GET', '/provider/alert-channels')
}

// Writes.

export interface RuleChange {
  name?: string
  enabled?: boolean
  severity?: AlertSeverity
  params?: AlertRuleParams
  for_seconds?: number
  renotify_seconds?: number
}

/** Disabling a rule resolves its alerts on the next evaluation. */
export function updateAlertRule(id: string, change: RuleChange, reason: string): Promise<AlertRule> {
  return request<AlertRule>('PATCH', `/provider/alert-rules/${encodeURIComponent(id)}`, { body: { ...change, reason } })
}

export interface ChannelFields {
  name?: string
  type?: ChannelType
  scope_filter?: ScopeFilter
  min_severity?: AlertSeverity
  enabled?: boolean
  url?: string
  /** null = every active super-admin. */
  recipients?: string[] | null
}

/** The secret (webhooks) is in this response only. */
export function createAlertChannel(fields: ChannelFields, reason: string): Promise<{ channel: AlertChannel; secret?: string }> {
  return request<{ channel: AlertChannel; secret?: string }>('POST', '/provider/alert-channels', { body: { ...fields, reason } })
}

export function updateAlertChannel(id: string, change: ChannelFields, reason: string): Promise<AlertChannel> {
  return request<AlertChannel>('PATCH', `/provider/alert-channels/${encodeURIComponent(id)}`, { body: { ...change, reason } })
}

export function deleteAlertChannel(id: string, reason: string): Promise<void> {
  return request<void>('DELETE', `/provider/alert-channels/${encodeURIComponent(id)}`, { body: { reason } })
}

/** Sends a test alert now; audited with the outcome. */
export function testAlertChannel(id: string, reason: string): Promise<ChannelTestResult> {
  return request<ChannelTestResult>('POST', `/provider/alert-channels/${encodeURIComponent(id)}/test`, { body: { reason } })
}

/** The new secret is in this response only; the old one stops working at once. */
export function rotateAlertChannelSecret(id: string, reason: string): Promise<{ secret: string }> {
  return request<{ secret: string }>('POST', `/provider/alert-channels/${encodeURIComponent(id)}/rotate-secret`, { body: { reason } })
}

// Incident timeline (Sprint 24 T1, F27): read-only. Alert events and provider
// audit rows (operator actions, pool failover/failback) in one time-ordered
// list. The controller adds audit rows for super-admins only; relay-ops get
// relay and pool alert rows only.

export type TimelineSource = 'alert' | 'audit'

export interface TimelineItem {
  id: string
  source: TimelineSource
  at: string
  target_type: string
  target_id: string
  target_name: string
  tenant_id: string | null
  // Alert rows.
  kind?: string
  transition?: AlertEvent['transition']
  severity?: AlertSeverity
  summary?: string
  started_at?: string
  // Audit rows.
  action?: string
  actor?: string
  reason?: string
  details?: unknown
}

/** At most one of relay_id, pool_id, tenant_id. */
export interface TimelineParams {
  relay_id?: string
  pool_id?: string
  tenant_id?: string
  source?: '' | TimelineSource
  since?: string
  until?: string
  cursor?: string
  limit?: number
}

const TIMELINE_KEYS = ['relay_id', 'pool_id', 'tenant_id', 'source', 'since', 'until', 'cursor', 'limit'] as const

export function listTimeline(params?: TimelineParams): Promise<Page<TimelineItem>> {
  return request<Page<TimelineItem>>('GET', '/provider/timeline' + buildQuery(params, TIMELINE_KEYS))
}
