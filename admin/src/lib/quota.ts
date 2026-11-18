import { CombinedGraphQLErrors, ServerError } from '@apollo/client/errors'

// Sprint 23 S3 (D-47): workspace quotas as the admin console shows them, and
// the controller's refusal at a limit (quota_exceeded {key, limit, used}):
//   - GraphQL: an error whose extensions.code is QUOTA_EXCEEDED;
//   - REST: HTTP 409 {"error":"quota_exceeded", key, limit, used, message}.

export const QUOTA_EXCEEDED = 'QUOTA_EXCEEDED'

export const QUOTA_LABELS: Record<string, string> = {
  users: 'Users',
  resources: 'Resources',
  remote_networks: 'Remote networks',
  connectors: 'Connectors',
  shields: 'Shields',
  client_devices: 'Client devices',
}

export function quotaLabel(key: string): string {
  return QUOTA_LABELS[key] ?? key
}

export interface QuotaExceeded {
  key: string
  limit: number
  used: number
}

/** The quota refusal carried by a GraphQL error, or null. */
export function quotaExceeded(error: unknown): QuotaExceeded | null {
  if (CombinedGraphQLErrors.is(error)) {
    for (const e of error.errors) {
      const ext = e.extensions as { code?: unknown; key?: unknown; limit?: unknown; used?: unknown } | undefined
      if (ext?.code === QUOTA_EXCEEDED && typeof ext.key === 'string') {
        return { key: ext.key, limit: Number(ext.limit), used: Number(ext.used) }
      }
    }
  }
  if (ServerError.is(error) && error.statusCode === 409) {
    try {
      const body = JSON.parse(error.bodyText) as { error?: unknown; key?: unknown; limit?: unknown; used?: unknown }
      if (body.error === 'quota_exceeded' && typeof body.key === 'string') {
        return { key: body.key, limit: Number(body.limit), used: Number(body.used) }
      }
    } catch {
      // Not ours.
    }
  }
  return null
}

/** The user-facing text for a refusal at a limit. */
export function quotaExceededMessage(q: QuotaExceeded): string {
  return `${quotaLabel(q.key)} limit reached (${q.used.toLocaleString()} of ${q.limit.toLocaleString()} in use). Remove some, or ask your provider to raise the limit.`
}
