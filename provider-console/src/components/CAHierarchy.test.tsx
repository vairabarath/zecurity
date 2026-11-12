import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { CANode, PKIHierarchy } from '@/api/types'
import { mockFetch, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { renderApp, signedInAs } from '@/test/render'
import { validityText } from '@/lib/format'

// Quick-win F15/F16: the CA hierarchy section of the Certificates page.

const DAY = 24 * 60 * 60_000
const iso = (ms: number) => new Date(Date.now() + ms).toISOString()
const ROOT = '11111111-0000-4000-8000-000000000001'
const INTER = '11111111-0000-4000-8000-000000000002'
const T1 = 'aaaaaaaa-0000-4000-8000-000000000001'
const T2 = 'aaaaaaaa-0000-4000-8000-000000000002'
const T3 = 'aaaaaaaa-0000-4000-8000-000000000003'

function node(over: Partial<CANode>): CANode {
  return {
    id: 'x', kind: 'workspace_ca', name: 'x', tenant_id: null, subject: 'CN=x', issuer: 'CN=Intermediate',
    serial: 'abc', not_before: iso(-DAY), not_after: iso(300 * DAY), parent_id: INTER, crl: null, ...over,
  }
}
const ws = (id: string, name: string, entries: number, over: Partial<CANode> = {}) =>
  node({ id: `ca-${id}`, name, tenant_id: id, serial: `5e${entries}`,
    crl: { url: `/ca.crl?workspace_id=${id}`, covers: ['client_devices', 'connectors'], entries, valid_for_seconds: 86400 }, ...over })

const page1: PKIHierarchy = {
  roots: [node({ id: ROOT, kind: 'root', name: 'Root CA', subject: 'CN=Zecurity Root', parent_id: null, serial: '1' })],
  intermediates: [node({ id: INTER, kind: 'intermediate', name: 'Intermediate CA', parent_id: ROOT, serial: '2',
    crl: { url: '/relay.crl', covers: ['relays'], entries: 2, valid_for_seconds: 600 } })],
  workspace_cas: [ws(T1, 'Acme', 3), ws(T2, 'Beta', 1)],
  workspace_cas_total: 3,
  next_cursor: 'c2',
}
const page2: PKIHierarchy = { ...page1, workspace_cas: [ws(T3, 'Gamma', 0)], next_cursor: null }

const EMPTY_CERTS: FakeResponse = {
  status: 200,
  body: { summary: { expired: 0, lt_24h: 0, lt_7d: 0, lt_30d: 0, ok: 0 }, items: [], next_cursor: null },
}
const pkiCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path === '/provider/pki')
const pkiRoute = (call: RecordedCall): FakeResponse => ({ status: 200, body: new URLSearchParams(call.search).get('cursor') === 'c2' ? page2 : page1 })

describe('CA hierarchy', () => {
  it('renders Root → Intermediate → workspace CAs with the CRL each one signs', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/certificates': EMPTY_CERTS, 'GET /provider/pki': pkiRoute })
    renderApp('/certificates')
    const tree = await screen.findByRole('list', { name: 'CA hierarchy' })
    expect(within(tree).getByText('Root CA')).toBeInTheDocument()
    const underRoot = within(tree).getByRole('list', { name: 'Signed by Root CA' })
    expect(within(underRoot).getByText('Intermediate CA')).toBeInTheDocument()
    const underInter = within(tree).getByRole('list', { name: 'Signed by Intermediate CA' })
    expect(within(underInter).getByText('Acme')).toBeInTheDocument()
    expect(within(underInter).getByText('Beta')).toBeInTheDocument()

    const crls = within(tree).getAllByLabelText('CRL status').map((e) => e.textContent)
    expect(crls).toContain('CRL /relay.crl · 2 entries (relays) · valid 10 minutes')
    expect(crls).toContain(`CRL /ca.crl?workspace_id=${T1} · 3 entries (client devices, connectors) · valid 24 hours`)
    expect(crls).toContain(`CRL /ca.crl?workspace_id=${T2} · 1 entry (client devices, connectors) · valid 24 hours`)
    expect(within(tree).getByText('Signs no CRL')).toBeInTheDocument() // the root
    expect(qsOf(pkiCalls(calls)[0])).toEqual({ limit: '50' })
  })

  it('a partial list says so; Show more appends the next page once', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/certificates': EMPTY_CERTS, 'GET /provider/pki': pkiRoute })
    renderApp('/certificates')
    expect(await screen.findByLabelText('Workspace CA count')).toHaveTextContent('Showing 2 of 3 workspace CAs')
    await userEvent.setup().click(screen.getByRole('button', { name: 'Show more' }))
    expect(await screen.findByText('Gamma')).toBeInTheDocument()
    expect(screen.getByLabelText('Workspace CA count')).toHaveTextContent('Showing 3 of 3 workspace CAs')
    expect(screen.queryByRole('button', { name: 'Show more' })).not.toBeInTheDocument()
    expect(pkiCalls(calls).map(qsOf)).toEqual([{ limit: '50' }, { limit: '50', cursor: 'c2' }])
    // Every CA appears exactly once.
    expect(screen.getAllByText('Acme')).toHaveLength(1)
  })

  it('a CA whose signer is unknown is listed under Unlinked CAs, not dropped', async () => {
    signedInAs(superAdminMe)
    const broken = ws(T3, 'Broken', 0, { subject: null, issuer: null, serial: null, not_before: null, parent_id: null })
    mockFetch({
      'GET /provider/certificates': EMPTY_CERTS,
      'GET /provider/pki': { status: 200, body: { ...page1, workspace_cas: [ws(T1, 'Acme', 3), broken], next_cursor: null, workspace_cas_total: 2 } },
    })
    renderApp('/certificates')
    const unlinked = await screen.findByRole('region', { name: 'Unlinked CAs' })
    expect(within(unlinked).getByText('Broken')).toBeInTheDocument()
    expect(within(unlinked).getByText('certificate could not be read')).toBeInTheDocument()
    expect(within(screen.getByRole('list', { name: 'CA hierarchy' })).queryByText('Broken')).not.toBeInTheDocument()
  })

  it('Refresh sends one request; an error offers Retry; nothing secret-looking is rendered', async () => {
    signedInAs(superAdminMe)
    let fail = false
    const { calls } = mockFetch({
      'GET /provider/certificates': EMPTY_CERTS,
      'GET /provider/pki': (c) => (fail ? { status: 500, body: { error: 'server_error' } } : pkiRoute(c)),
    })
    renderApp('/certificates')
    await screen.findByRole('list', { name: 'CA hierarchy' })
    expect(document.body.textContent?.toLowerCase()).not.toMatch(/begin|private|-----/)
    fail = true
    await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh CA hierarchy' }))
    expect(await screen.findByText(/Couldn't load the CA hierarchy/)).toBeInTheDocument()
    expect(pkiCalls(calls)).toHaveLength(2)
    fail = false
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
    await screen.findByRole('list', { name: 'CA hierarchy' })
    expect(pkiCalls(calls)).toHaveLength(3)
  })
})

describe('validityText', () => {
  it('words a CRL validity', () => {
    expect(validityText(600)).toBe('10 minutes')
    expect(validityText(86400)).toBe('24 hours')
    expect(validityText(3600)).toBe('1 hour')
    expect(validityText(90)).toBe('90 seconds')
  })
})

function qsOf(c: RecordedCall) {
  return Object.fromEntries(new URLSearchParams(c.search))
}
