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
  if (k === AuditExportSinkKind.SplunkHec) return 'Splunk HEC'
  if (k === AuditExportSinkKind.S3) return 'Amazon S3'
  return 'Webhook'
}

export function hasCredential(s: Sink): boolean {
  return !!s.authHeaderName || s.kind === AuditExportSinkKind.S3
}

// Where an S3 sink writes, for display: s3://bucket/prefix.
export function s3Location(s: Pick<Sink, 's3Bucket' | 's3Prefix'>): string {
  return `s3://${s.s3Bucket ?? ''}${s.s3Prefix ? `/${s.s3Prefix}` : ''}/`
}

// Mirrors the server's S3 rules (PENDING-11 §9 W-3).
const S3_BUCKET = /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/
const S3_REGION = /^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$/
const S3_PREFIX = /^[A-Za-z0-9/_.-]{0,256}$/
const S3_KEY_ID = /^[A-Za-z0-9]{3,128}$/

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
  bucket: string
  region: string
  prefix: string
  endpoint: string
  accessKeyId: string
  secretAccessKey: string
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
    bucket: s?.s3Bucket ?? '',
    region: s?.s3Region ?? '',
    prefix: s?.s3Prefix ?? '',
    endpoint: s?.s3Endpoint ?? '',
    accessKeyId: s?.s3AccessKeyId ?? '',
    secretAccessKey: '',
  }
}

// validateForm returns a message for a form the server would refuse, or null.
export function validateForm(f: FormState, editing?: Sink): string | null {
  if (!f.name.trim()) return 'Enter a name.'
  if (f.kind === AuditExportSinkKind.S3) return validateS3(f, editing)
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

// s3Changed reports whether an edit moves the sink to another destination or
// key pair, which the server accepts only with the secret entered again.
export function s3Changed(f: FormState, s: Sink): boolean {
  return (
    f.bucket.trim() !== (s.s3Bucket ?? '') ||
    f.region.trim() !== (s.s3Region ?? '') ||
    f.endpoint.trim() !== (s.s3Endpoint ?? '') ||
    f.accessKeyId.trim() !== (s.s3AccessKeyId ?? '')
  )
}

function validateS3(f: FormState, editing?: Sink): string | null {
  if (!f.admin && !f.access) return 'Choose at least one stream.'
  const bucket = f.bucket.trim()
  if (!S3_BUCKET.test(bucket) || bucket.includes('..')) return 'Enter a valid bucket name (3-63 characters: a-z 0-9 . -).'
  if (!S3_REGION.test(f.region.trim())) return 'Enter the bucket region, for example eu-west-1.'
  const prefix = f.prefix.trim()
  if (
    !S3_PREFIX.test(prefix) ||
    prefix.startsWith('/') ||
    prefix.endsWith('/') ||
    prefix.includes('//') ||
    prefix.split('/').some((seg) => seg === '.' || seg === '..')
  ) {
    return 'The prefix may use A-Z a-z 0-9 / _ . - with no leading, trailing or double "/".'
  }
  const endpoint = f.endpoint.trim()
  if (endpoint && !/^https:\/\/[^/?#@\s]+\/?$/.test(endpoint)) return 'The endpoint must be https://host[:port] with no path.'
  if (!S3_KEY_ID.test(f.accessKeyId.trim())) return 'Enter the access key ID (letters and digits).'
  if (/\s/.test(f.secretAccessKey)) return 'The secret access key cannot contain spaces.'
  if (!editing) return f.secretAccessKey ? null : 'Enter the secret access key.'
  if (s3Changed(f, editing) && !f.secretAccessKey) {
    return 'Changing the bucket, region, endpoint or access key ID requires entering the secret access key again.'
  }
  return null
}
