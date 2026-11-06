import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { DocumentNode } from 'graphql'
import {
  AuditExportSinkKind,
  AuditExportStartFrom,
  AuditStream,
  CreateAuditExportSinkDocument,
  GetAuditExportSinksDocument,
  UpdateAuditExportSinkDocument,
} from '@/generated/graphql'
import { AuditExportSection } from './AuditExportSection'
import { validateForm, type Sink } from './sinkForm'

// PENDING-11 P-8 / S2: the Settings → Audit export section.

const NOW = '2026-11-06T10:00:00Z'
const calls = new Map<DocumentNode, unknown[]>()
let sinks: Sink[] = []

vi.mock('@apollo/client/react', () => ({
  useQuery: (doc: DocumentNode) => ({
    data: doc === GetAuditExportSinksDocument ? { auditExportSinks: sinks } : undefined,
    loading: false,
    error: undefined,
    refetch: vi.fn(),
  }),
  useMutation: (doc: DocumentNode) => [
    vi.fn(async (opts: unknown) => {
      calls.set(doc, [...(calls.get(doc) ?? []), opts])
      if (doc === CreateAuditExportSinkDocument) {
        return { data: { createAuditExportSink: { signingSecret: 'zsk_once', sink: sinks[0] } } }
      }
      return { data: {} }
    }),
    { loading: false },
  ],
}))

function sink(over: Partial<Sink> = {}): Sink {
  return {
    __typename: 'AuditExportSink',
    id: 'sink-1',
    name: 'siem',
    kind: AuditExportSinkKind.Webhook,
    url: 'https://siem.example.com/x',
    streams: [AuditStream.Admin, AuditStream.Access],
    enabled: true,
    authHeaderName: 'Authorization',
    splunkIndex: null,
    splunkSource: null,
    splunkSourcetype: null,
    s3Bucket: null,
    s3Region: null,
    s3Prefix: null,
    s3Endpoint: null,
    s3AccessKeyId: null,
    createdAt: NOW,
    updatedAt: NOW,
    status: [
      {
        __typename: 'AuditExportStreamStatus',
        stream: AuditStream.Admin,
        cursorSeq: 10,
        headSeq: 12,
        lag: 2,
        lastSuccessAt: NOW,
        lastError: 'destination answered HTTP 503',
        failingSince: NOW,
        oldestPendingAt: NOW,
        lagging: false,
      },
    ],
    ...over,
  }
}

beforeEach(() => {
  calls.clear()
  sinks = []
})

describe('AuditExportSection', () => {
  it('creates a webhook sink and shows the signing secret once', async () => {
    const user = userEvent.setup()
    render(<AuditExportSection />)
    await user.click(screen.getByRole('button', { name: /Add sink/ }))
    await user.type(screen.getByLabelText('Name'), 'siem')
    await user.type(screen.getByLabelText('URL'), 'https://siem.example.com/hook')
    await user.click(screen.getByRole('button', { name: 'Add sink' }))

    const sent = calls.get(CreateAuditExportSinkDocument) as { variables: { input: unknown } }[]
    expect(sent).toHaveLength(1)
    expect(sent[0].variables.input).toEqual({
      name: 'siem',
      kind: AuditExportSinkKind.Webhook,
      url: 'https://siem.example.com/hook',
      streams: [AuditStream.Admin, AuditStream.Access],
      enabled: true,
      startFrom: AuditExportStartFrom.AllHistory,
    })
    expect(await screen.findByTestId('audit-sink-secret')).toHaveTextContent('zsk_once')
    await user.click(screen.getByRole('button', { name: 'I have copied it' }))
    expect(screen.queryByText('zsk_once')).toBeNull()
  })

  it('refuses a Splunk sink without a token before calling the server', async () => {
    const user = userEvent.setup()
    render(<AuditExportSection />)
    await user.click(screen.getByRole('button', { name: /Add sink/ }))
    await user.click(screen.getByRole('button', { name: 'Splunk HEC' }))
    await user.type(screen.getByLabelText('Name'), 'splunk')
    await user.type(screen.getByLabelText('URL'), 'https://splunk.example.com:8088/services/collector/event')
    await user.click(screen.getByRole('button', { name: 'Add sink' }))
    expect(screen.getByTestId('sink-form-error')).toHaveTextContent('HEC token')
    expect(calls.get(CreateAuditExportSinkDocument)).toBeUndefined()
  })

  it('asks for the credential again when the URL of a sink with one changes', async () => {
    sinks = [sink()]
    const user = userEvent.setup()
    render(<AuditExportSection />)
    await user.click(screen.getByRole('button', { name: /Edit/ }))
    const url = screen.getByLabelText('URL')
    await user.clear(url)
    await user.type(url, 'https://other.example.com/x')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(screen.getByTestId('sink-form-error')).toHaveTextContent('entering the auth header value again')
    expect(calls.get(UpdateAuditExportSinkDocument)).toBeUndefined()

    await user.type(screen.getByLabelText('Header value'), 'Bearer new')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    const sent = calls.get(UpdateAuditExportSinkDocument) as { variables: unknown }[]
    expect(sent[0].variables).toEqual({
      id: 'sink-1',
      input: { url: 'https://other.example.com/x', authHeader: { name: 'Authorization', value: 'Bearer new' } },
    })
  })

  it('creates an S3 sink without a URL and with the secret access key', async () => {
    const user = userEvent.setup()
    render(<AuditExportSection />)
    await user.click(screen.getByRole('button', { name: /Add sink/ }))
    await user.click(screen.getByRole('button', { name: 'Amazon S3' }))
    expect(screen.queryByLabelText('URL')).toBeNull()
    await user.type(screen.getByLabelText('Name'), 'wazuh-s3')
    await user.type(screen.getByLabelText('Bucket'), 'zecurity-audit')
    await user.type(screen.getByLabelText('Region'), 'eu-west-1')
    await user.type(screen.getByLabelText('Key prefix (optional)'), 'siem')
    await user.type(screen.getByLabelText('Access key ID'), 'AKIAIOSFODNN7EXAMPLE')
    await user.click(screen.getByRole('button', { name: 'Add sink' }))
    expect(screen.getByTestId('sink-form-error')).toHaveTextContent('secret access key')
    expect(calls.get(CreateAuditExportSinkDocument)).toBeUndefined()

    await user.type(screen.getByLabelText('Secret access key'), 'wJalrXUtnFEMI/K7MDENG')
    await user.click(screen.getByRole('button', { name: 'Add sink' }))
    const sent = calls.get(CreateAuditExportSinkDocument) as { variables: { input: unknown } }[]
    expect(sent[0].variables.input).toEqual({
      name: 'wazuh-s3',
      kind: AuditExportSinkKind.S3,
      streams: [AuditStream.Admin, AuditStream.Access],
      enabled: true,
      startFrom: AuditExportStartFrom.AllHistory,
      s3: {
        bucket: 'zecurity-audit',
        region: 'eu-west-1',
        prefix: 'siem',
        endpoint: undefined,
        accessKeyId: 'AKIAIOSFODNN7EXAMPLE',
        secretAccessKey: 'wJalrXUtnFEMI/K7MDENG',
      },
    })
  })

  it('asks for the S3 secret again when the bucket changes, not for the prefix', async () => {
    sinks = [sink({ kind: AuditExportSinkKind.S3, url: 'https://zecurity-audit.s3.eu-west-1.amazonaws.com', authHeaderName: null,
      s3Bucket: 'zecurity-audit', s3Region: 'eu-west-1', s3Prefix: 'siem', s3AccessKeyId: 'AKIAIOSFODNN7EXAMPLE' })]
    const user = userEvent.setup()
    render(<AuditExportSection />)
    expect(screen.getByText('s3://zecurity-audit/siem/')).toBeInTheDocument()
    expect(screen.getByText(/written every 5 minutes/)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /Edit/ }))
    const bucket = screen.getByLabelText('Bucket')
    await user.clear(bucket)
    await user.type(bucket, 'other-bucket')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(screen.getByTestId('sink-form-error')).toHaveTextContent('secret access key again')
    expect(calls.get(UpdateAuditExportSinkDocument)).toBeUndefined()

    await user.clear(bucket)
    await user.type(bucket, 'zecurity-audit')
    const prefix = screen.getByLabelText('Key prefix (optional)')
    await user.clear(prefix)
    await user.type(prefix, 'siem/v2')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    const sent = calls.get(UpdateAuditExportSinkDocument) as { variables: { input: { s3: unknown } } }[]
    expect(sent[0].variables.input.s3).toEqual({
      bucket: 'zecurity-audit', region: 'eu-west-1', prefix: 'siem/v2', endpoint: undefined,
      accessKeyId: 'AKIAIOSFODNN7EXAMPLE', secretAccessKey: undefined,
    })
  })

  it('shows delivery status, the last error and the lag warning, and caps at 3 sinks', () => {
    sinks = [sink({ status: [{ ...sink().status[0], lagging: true }] }), sink({ id: 's2', name: 'b' }), sink({ id: 's3', name: 'c' })]
    render(<AuditExportSection />)
    const first = screen.getAllByTestId('audit-sink')[0]
    expect(within(first).getByText('destination answered HTTP 503')).toBeInTheDocument()
    expect(within(first).getByText(/more than 7 days behind/)).toBeInTheDocument()
    expect(within(first).getByText(/2 pending/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Add sink/ })).toBeDisabled()
  })
})

describe('validateForm', () => {
  const base = {
    name: 'x', kind: AuditExportSinkKind.Webhook, url: 'https://a.example.com', admin: true, access: false,
    enabled: true, startFrom: AuditExportStartFrom.AllHistory, headerName: '', headerValue: '',
    clearHeader: false, token: '', index: '', source: '', sourcetype: '',
    bucket: '', region: '', prefix: '', endpoint: '', accessKeyId: '', secretAccessKey: '',
  }
  it('checks the basics', () => {
    expect(validateForm(base)).toBeNull()
    expect(validateForm({ ...base, url: 'http://a.example.com' })).toMatch(/https/)
    expect(validateForm({ ...base, admin: false })).toMatch(/stream/)
    expect(validateForm({ ...base, headerName: 'X-Key' })).toMatch(/both/)
  })
  it('checks S3 settings like the server', () => {
    const s3 = { ...base, kind: AuditExportSinkKind.S3, url: '', bucket: 'zecurity-audit', region: 'eu-west-1',
      accessKeyId: 'AKIAX', secretAccessKey: 'sek' }
    expect(validateForm(s3)).toBeNull()
    expect(validateForm({ ...s3, bucket: 'Bad_Bucket' })).toMatch(/bucket/)
    expect(validateForm({ ...s3, region: '' })).toMatch(/region/)
    for (const prefix of ['/a', 'a/', 'a//b', 'a/../b', 'sp ace', 'a+b']) {
      expect(validateForm({ ...s3, prefix })).toMatch(/prefix/)
    }
    expect(validateForm({ ...s3, endpoint: 'https://minio.example.com:9000' })).toBeNull()
    expect(validateForm({ ...s3, endpoint: 'https://minio.example.com/path' })).toMatch(/endpoint/)
    expect(validateForm({ ...s3, endpoint: 'http://minio.example.com' })).toMatch(/endpoint/)
    expect(validateForm({ ...s3, secretAccessKey: '' })).toMatch(/secret/)
  })
  it('lets a URL change through when the credential is removed', () => {
    expect(validateForm({ ...base, url: 'https://b.example.com', clearHeader: true }, sink())).toBeNull()
    expect(validateForm({ ...base, url: 'https://b.example.com' }, sink({ authHeaderName: null }))).toBeNull()
  })
})
