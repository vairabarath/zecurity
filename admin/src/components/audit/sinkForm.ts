import {
  AuditExportSinkKind,
  AuditExportStartFrom,
  AuditStream,
  type GetAuditExportSinksQuery,
} from '@/generated/graphql'

// Form logic for the audit export sink dialog (PENDING-11 P-8), kept apart
// from the components.

export const MAX_SINKS = 3

export type Sink = GetAuditExportSinksQuery['auditExportSinks'][number]

export const STREAM_LABEL: Record<AuditStream, string> = {
  [AuditStream.Admin]: 'Admin changes',
  [AuditStream.Access]: 'Resource access',
}

export function kindLabel(k: AuditExportSinkKind): string {
  return k === AuditExportSinkKind.SplunkHec ? 'Splunk HEC' : 'Webhook'
}

export function hasCredential(s: Sink): boolean {
  return !!s.authHeaderName
}

export type FormState = {
  name: string
  kind: AuditExportSinkKind
  url: string
  admin: boolean
  access: boolean
  enabled: boolean
  startFrom: AuditExportStartFrom
  headerName: string
  headerValue: string
  clearHeader: boolean
  token: string
  index: string
  source: string
  sourcetype: string
}

export function initialForm(s?: Sink): FormState {
  return {
    name: s?.name ?? '',
    kind: s?.kind ?? AuditExportSinkKind.Webhook,
    url: s?.url ?? '',
    admin: s ? s.streams.includes(AuditStream.Admin) : true,
    access: s ? s.streams.includes(AuditStream.Access) : true,
    enabled: s?.enabled ?? true,
    startFrom: AuditExportStartFrom.AllHistory,
    headerName: s?.kind === AuditExportSinkKind.Webhook ? (s.authHeaderName ?? '') : '',
    headerValue: '',
    clearHeader: false,
    token: '',
    index: s?.splunkIndex ?? '',
    source: s?.splunkSource ?? '',
    sourcetype: s?.splunkSourcetype ?? '',
  }
}

// validateForm returns a message for a form the server would refuse, or null.
export function validateForm(f: FormState, editing?: Sink): string | null {
  if (!f.name.trim()) return 'Enter a name.'
  if (!f.url.startsWith('https://')) return 'The URL must start with https://.'
  if (!f.admin && !f.access) return 'Choose at least one stream.'
  const splunk = f.kind === AuditExportSinkKind.SplunkHec
  if (!editing) {
    if (splunk && !f.token) return 'Enter the Splunk HEC token.'
    if (!splunk && !!f.headerName !== !!f.headerValue) return 'Enter both the header name and its value, or neither.'
    return null
  }
  // The server refuses a URL change on a sink with a stored credential unless
  // the credential is entered again (it must not follow the sink elsewhere).
  const newCredential = splunk ? !!f.token : !!f.headerValue
  if (f.url !== editing.url && hasCredential(editing) && !newCredential && !f.clearHeader) {
    return splunk
      ? 'Changing the URL requires entering the Splunk HEC token again.'
      : 'Changing the URL requires entering the auth header value again (or removing the header).'
  }
  if (!splunk && f.headerValue && !f.headerName) return 'Enter the header name.'
  return null
}
