import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { StrictMode } from 'react'
import { MemoryRouter, useLocation } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { AppRoutes } from '@/App'
import type { CertificateEntry, CertificateExpiryResult, CertificateKind, CertificateSummary, ExpiryBucket } from '@/api/types'
import { bucketFor } from '@/lib/expiry'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { signedInAs } from '@/test/render'

const DAY = 24 * 60 * 60_000
const iso = (ms: number) => new Date(Date.now() + ms).toISOString()
const TENANT = '7a7a7a7a-1111-2222-3333-444444444444'
const RELAY = '5b5b5b5b-1111-2222-3333-444444444444'

function cert(kind: CertificateKind, over: Partial<CertificateEntry> = {}): CertificateEntry {
  return { kind, tenant_id: null, entity_id: `${kind}-id`, entity_name: kind, serial: 'abc123', not_after: iso(10 * DAY), bucket: 'lt_30d', ...over }
}

// Soonest expiry first, as the API orders (not_after, kind, entity_id).
const ITEMS: CertificateEntry[] = [
  cert('relay', { entity_id: RELAY, entity_name: 'relay-1', serial: '0a1b2c', not_after: iso(-DAY), bucket: 'expired' }),
  cert('controller_grpc', { entity_id: 'controller', entity_name: 'Controller gRPC', serial: '814ad5006043b094', not_after: iso(12 * 3600_000), bucket: 'lt_24h' }),
  cert('connector', { tenant_id: TENANT, entity_id: 'c1c1c1c1-1111-2222-3333-444444444444', entity_name: 'hq-1', not_after: iso(2 * DAY), bucket: 'lt_7d' }),
  cert('workspace_ca', { tenant_id: TENANT, entity_name: 'Acme', serial: '603c62c0', not_after: iso(5 * DAY), bucket: 'lt_7d' }),
  cert('client_device', { tenant_id: TENANT, entity_id: 'd1d1d1d1-1111-2222-3333-444444444444', entity_name: null, serial: 'ds1', not_after: iso(10 * DAY), bucket: 'lt_30d' }),
  cert('shield', { tenant_id: TENANT, entity_name: 'db-1', serial: null, not_after: iso(40 * DAY), bucket: 'ok' }),
  cert('intermediate', { entity_name: 'Intermediate CA', serial: 'def', not_after: iso(100 * DAY), bucket: 'ok' }),
  cert('root', { entity_name: 'Root CA', serial: 'abc', not_after: iso(300 * DAY), bucket: 'ok' }),
]
const SUMMARY: CertificateSummary = { expired: 1, lt_24h: 1, lt_7d: 2, lt_30d: 1, ok: 5 }

const result = (items: CertificateEntry[] = ITEMS, next: string | null = null, summary = SUMMARY): FakeResponse => ({
  status: 200,
  body: { summary, items, next_cursor: next } satisfies CertificateExpiryResult,
})

function LocationProbe() {
  const l = useLocation()
  return <output data-testid="location">{l.pathname + l.search}</output>
}

function renderCerts(path = '/certificates', strict = false) {
  const [pathname, search = ''] = path.split('?')
  const tree = (
    <MemoryRouter initialEntries={[{ pathname, search: search ? `?${search}` : '' }]}>
      <AppRoutes />
      <LocationProbe />
    </MemoryRouter>
  )
  return render(strict ? <StrictMode>{tree}</StrictMode> : tree)
}

const certCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path === '/provider/certificates')
const tenantDetailCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path.startsWith('/provider/tenants/'))
const qs = (c: RecordedCall) => Object.fromEntries(new URLSearchParams(c.search))
const quiet = () => new Promise((r) => setTimeout(r, 50))
const tile = (label: string) => within(screen.getByRole('listitem', { name: label }))
const rowsOf = () => within(screen.getByRole('table', { name: 'Certificates' })).getAllByRole('row').slice(1)

async function pick(label: string, option: string) {
  const user = userEvent.setup()
  await user.click(screen.getByRole('combobox', { name: label }))
  await user.click(await screen.findByRole('option', { name: option }))
}

describe('Certificates: authorization', () => {
  it('super-admin sees the nav entry and the page', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/certificates': result() })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    expect(within(screen.getByRole('navigation', { name: 'Sections' })).getByRole('link', { name: 'Certificates' })).toHaveAttribute('href', '/certificates')
  })

  it('relay-ops: no nav entry, Forbidden, zero requests', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({})
    renderCerts()
    expect(screen.getByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
    expect(within(screen.getByRole('navigation', { name: 'Sections' })).queryByRole('link', { name: 'Certificates' })).not.toBeInTheDocument()
    await quiet()
    expect(calls).toHaveLength(0)
  })

  it('a server role 403 shows Forbidden', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/certificates': { status: 403, body: { error: 'forbidden' } } })
    renderCerts()
    expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
  })
})

describe('Certificates: summary', () => {
  it('five tiles show the API summary exactly (not recounted from the rows)', async () => {
    signedInAs(superAdminMe)
    // Deliberately inconsistent with the 8 rows: the tiles must show the API's numbers.
    mockFetch({ 'GET /provider/certificates': result(ITEMS, null, { expired: 7, lt_24h: 0, lt_7d: 13, lt_30d: 2, ok: 99 }) })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    expect(tile('Expired').getByText('7')).toBeInTheDocument()
    expect(tile('< 24 hours').getByText('0')).toBeInTheDocument()
    expect(tile('< 7 days').getByText('13')).toBeInTheDocument()
    expect(tile('< 30 days').getByText('2')).toBeInTheDocument()
    expect(tile('OK').getByText('99')).toBeInTheDocument()
  })

  it('changing the listing window changes the list, not the summary', async () => {
    signedInAs(superAdminMe)
    // The server: items depend on within, the summary never does.
    const { calls } = mockFetch({
      'GET /provider/certificates': (c) => (qs(c).within === '24h' ? result(ITEMS.slice(0, 2)) : result()),
    })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    expect(rowsOf()).toHaveLength(8)
    await pick('Listing window', 'Next 24 hours')
    await waitFor(() => expect(rowsOf()).toHaveLength(2))
    expect(qs(certCalls(calls).at(-1)!)).toEqual({ within: '24h' })
    expect(tile('OK').getByText('5')).toBeInTheDocument()
    expect(tile('< 7 days').getByText('2')).toBeInTheDocument()
  })
})

describe('Certificates: table', () => {
  it('renders all eight kinds in the API order with exact fields', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/certificates': result() })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    const rows = rowsOf()
    expect(rows.map((r) => within(r).getAllByRole('cell')[0].textContent)).toEqual(ITEMS.map((i) => i.kind))
    const ctl = within(screen.getByRole('row', { name: 'controller_grpc controller' }))
    expect(ctl.getByText('Controller gRPC')).toBeInTheDocument()
    expect(ctl.getByText('controller')).toBeInTheDocument()
    expect(ctl.getByText('814ad5006043b094')).toBeInTheDocument() // lowercase hex, as the API sends it
    const relay = within(screen.getByRole('row', { name: `relay ${RELAY}` }))
    expect(relay.getByRole('link', { name: 'relay-1' })).toHaveAttribute('href', `/relays/${RELAY}`)
    expect(relay.getAllByRole('cell')[2].textContent).toBe('—') // no tenant
    const shield = within(screen.getByRole('row', { name: 'shield shield-id' }))
    expect(shield.getAllByRole('cell')[3].textContent).toBe('—') // null serial
  })

  it('client devices have no name: shown as —, never an invented identity', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/certificates': result() })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    const device = within(screen.getByRole('row', { name: /^client_device / }))
    expect(device.getAllByRole('cell')[1].textContent).toBe(`—${ITEMS[4].entity_id}`)
  })

  it('renders no certificate material or secrets', async () => {
    signedInAs(superAdminMe)
    const leaky = { ...ITEMS[0], certificate_pem: '-----BEGIN CERTIFICATE-----PEMCANARY', private_key: 'KEYCANARY' }
    mockFetch({ 'GET /provider/certificates': result([leaky as CertificateEntry]) })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    expect(document.body.textContent).not.toMatch(/BEGIN|PEMCANARY|KEYCANARY|PRIVATE/i)
  })

  it('shows no Current or Revoked state: the API reports none and none is inferred', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/certificates': result() })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    const expired = within(screen.getByRole('row', { name: `relay ${RELAY}` }))
    expect(expired.getByText('Expired')).toHaveAttribute('data-bucket', 'expired') // expired, not revoked
    expect(screen.queryByText(/revoked/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/^current$/i)).not.toBeInTheDocument()
  })

  it("each row's badge is the API's bucket (authoritative), with the shared colours", async () => {
    signedInAs(superAdminMe)
    // A date 3 days out that the server bucketed as lt_24h (its clock): the server wins.
    mockFetch({ 'GET /provider/certificates': result([cert('relay', { entity_id: RELAY, not_after: iso(3 * DAY), bucket: 'lt_24h' })]) })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    const badge = within(rowsOf()[0]).getByText('< 24h')
    expect(badge).toHaveAttribute('data-bucket', 'lt_24h')
  })
})

describe('Certificates: no client/server bucket drift', () => {
  // The controller's bucketFor boundaries (providerquery/certificates.go),
  // checked against the console's shared helper at the exact edges.
  const now = new Date('2026-10-06T12:00:00Z')
  const H = 3600_000
  it.each<[string, number, ExpiryBucket]>([
    ['expired (not_after == now)', 0, 'expired'],
    ['just under 24h', 24 * H - 1, 'lt_24h'],
    ['exactly 24h', 24 * H, 'lt_24h'],
    ['just over 24h', 24 * H + 1, 'lt_7d'],
    ['exactly 7d', 7 * DAY, 'lt_7d'],
    ['exactly 30d', 30 * DAY, 'lt_30d'],
    ['just over 30d', 30 * DAY + 1, 'ok'],
  ])('%s → %s', (_n, offset, serverBucket) => {
    expect(bucketFor(new Date(now.getTime() + offset), now)).toBe(serverBucket)
  })
})

describe('Certificates: filters', () => {
  it('default window sends no within (server default 720h); each window value is sent exactly', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/certificates': result() })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    expect(qs(certCalls(calls)[0])).toEqual({})
    expect(screen.getByRole('combobox', { name: 'Listing window' })).toHaveTextContent('Next 30 days (default)')
    for (const [label, value] of [['Next 7 days', '168h'], ['Next 90 days', '2160h'], ['Next 365 days (maximum)', '8760h']]) {
      await pick('Listing window', label)
      await waitFor(() => expect(qs(certCalls(calls).at(-1)!)).toEqual({ within: value }))
    }
    await pick('Listing window', 'Next 30 days (default)')
    await waitFor(() => expect(qs(certCalls(calls).at(-1)!)).toEqual({}))
  })

  it.each(['9000h', '8761h', '0h', '-5h', '30d', 'soon', '1h30x'])('a URL window of %s is Invalid filter with zero requests', async (w) => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/certificates': result() })
    renderCerts(`/certificates?within=${encodeURIComponent(w)}`)
    expect(await screen.findByText(/Invalid filter/)).toBeInTheDocument()
    await quiet()
    expect(certCalls(calls)).toHaveLength(0)
  })

  it('the maximum 8760h and other valid durations from a URL are sent exactly', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/certificates': result() })
    renderCerts('/certificates?within=8760h')
    await screen.findByRole('table', { name: 'Certificates' })
    expect(qs(certCalls(calls)[0])).toEqual({ within: '8760h' })
  })

  it.each([
    'client_device',
    'connector',
    'controller_grpc',
    'intermediate',
    'relay',
    'root',
    'shield',
    'workspace_ca',
  ])('kind %s can be selected and is sent as is', async (k) => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/certificates': result() })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    await pick('Kind filter', k)
    await waitFor(() => expect(qs(certCalls(calls).at(-1)!)).toEqual({ kind: k }))
  })

  it('an unknown kind or malformed tenant in the URL is Invalid filter with zero requests', async () => {
    for (const q of ['kind=ssh', 'tenant_id=acme']) {
      signedInAs(superAdminMe)
      const { calls } = mockFetch({ 'GET /provider/certificates': result() })
      const view = renderCerts(`/certificates?${q}`)
      expect(await screen.findByText(/Invalid filter/)).toBeInTheDocument()
      await quiet()
      expect(certCalls(calls)).toHaveLength(0)
      view.unmount()
    }
  })

  it('tenant filter: typing sends nothing; Apply sends tenant_id once; no tenant lookup happens', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/certificates': result() })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Tenant ID'), TENANT)
    await quiet()
    expect(certCalls(calls)).toHaveLength(1)
    await user.click(within(screen.getByRole('form', { name: 'Tenant filter' })).getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(certCalls(calls)).toHaveLength(2))
    expect(qs(certCalls(calls)[1])).toEqual({ tenant_id: TENANT })
    expect(tenantDetailCalls(calls)).toHaveLength(0)
  })
})

describe('Certificates: tenant links (audited detail)', () => {
  it('rendering, hovering and focusing tenant links send no tenant-detail request; a click sends one', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/certificates': result(),
      [`GET /provider/tenants/${TENANT}`]: {
        status: 200,
        body: { id: TENANT, slug: 'acme', name: 'Acme', status: 'active', trust_domain: 'acme.test', created_at: '2026-10-01T00:00:00Z', ca_not_after: null, counts: { connectors: { pending: 0, active: 0, disconnected: 0, revoked: 0 }, shields: { pending: 0, active: 0, disconnected: 0, revoked: 0 }, remote_networks: 0, users: { active: 0, suspended: 0, locked: 0, deleted: 0 }, client_devices: { active: 0, re_enroll_required: 0, renew_pending: 0, revoked: 0 } }, remote_networks: [], remote_networks_truncated: false, connectors: [], connectors_truncated: false, shields: [], shields_truncated: false },
      },
    })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    const user = userEvent.setup()
    const links = screen.getAllByRole('link', { name: TENANT })
    expect(links).toHaveLength(4) // connector, workspace CA, device, shield rows
    for (const l of links) {
      await user.hover(l)
      l.focus()
    }
    await quiet()
    expect(tenantDetailCalls(calls)).toHaveLength(0)
    await user.click(links[0])
    await screen.findByRole('heading', { name: 'Acme', level: 1 })
    expect(tenantDetailCalls(calls)).toHaveLength(1)
  })
})

describe('Certificates: platform CAs', () => {
  it('root/intermediate count in the summary and the note explains why they may not be listed', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/certificates': result(ITEMS.slice(0, 6)) }) // no root/intermediate rows
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    expect(tile('OK').getByText('5')).toBeInTheDocument()
    expect(screen.getByText('Platform CAs (root, intermediate) are counted in the summary; their expiry is beyond the listing window.')).toBeInTheDocument()
    expect(screen.queryByRole('row', { name: /^root / })).not.toBeInTheDocument()
  })
})

describe('Certificates: paging', () => {
  it('Next uses the server cursor, Previous the stack; any filter change clears it', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/certificates': (c) => {
        const p = qs(c)
        const tag = `${p.within ?? ''}|${p.kind ?? ''}|${p.tenant_id ?? ''}`
        if (p.cursor && !p.cursor.startsWith(tag)) return { status: 400, body: { error: 'invalid_cursor' } }
        const n = p.cursor ? Number(p.cursor.split('#')[1]) : 1
        return result([cert('relay', { entity_id: RELAY, entity_name: `p${n}${tag}` })], n < 3 ? `${tag}#${n + 1}` : null)
      },
    })
    renderCerts()
    const user = userEvent.setup()
    const next = () => user.click(screen.getByRole('button', { name: /Next/ }))
    await screen.findByText('p1||')
    await next()
    await screen.findByText('p2||')
    expect(qs(certCalls(calls).at(-1)!).cursor).toBe('||#2')
    await next()
    await screen.findByText('p3||')
    await user.click(screen.getByRole('button', { name: /Previous/ }))
    await screen.findByText('p2||')

    // kind change → page 1, no cursor
    await pick('Kind filter', 'relay')
    await screen.findByText('p1|relay|')
    expect(qs(certCalls(calls).at(-1)!)).toEqual({ kind: 'relay' })
    await next()
    await screen.findByText('p2|relay|')
    // within change → page 1, no cursor
    await pick('Listing window', 'Next 7 days')
    await screen.findByText('p1168h|relay|')
    expect(qs(certCalls(calls).at(-1)!)).toEqual({ within: '168h', kind: 'relay' })
    await next()
    await screen.findByText('p2168h|relay|')
    // tenant change → page 1, no cursor
    await user.type(screen.getByLabelText('Tenant ID'), `${TENANT}{Enter}`)
    await screen.findByText(`p1168h|relay|${TENANT}`)
    expect(qs(certCalls(calls).at(-1)!)).toEqual({ within: '168h', kind: 'relay', tenant_id: TENANT })
    expect(screen.getByText('Page 1')).toBeInTheDocument()
  })
})

describe('Certificates: request behavior', () => {
  it('StrictMode: one request; focus/visibility/reconnect add none', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/certificates': result() })
    renderCerts('/certificates', true)
    await screen.findByRole('table', { name: 'Certificates' })
    act(() => {
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
      window.dispatchEvent(new Event('online'))
    })
    await quiet()
    expect(calls).toHaveLength(1)
  })

  it('Refresh sends exactly one request; Retry after an error exactly one; no automatic retry', async () => {
    signedInAs(superAdminMe)
    let fail = false
    const { calls } = mockFetch({ 'GET /provider/certificates': () => (fail ? { status: 500, body: { error: 'server_error' } } : result()) })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    fail = true
    await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh certificates' }))
    expect(await screen.findByText('Couldn’t load certificates.')).toBeInTheDocument()
    await quiet()
    expect(certCalls(calls)).toHaveLength(2)
    fail = false
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
    await screen.findByRole('table', { name: 'Certificates' })
    await quiet()
    expect(certCalls(calls)).toHaveLength(3)
  })

  it('empty list keeps the summary and shows the empty message', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/certificates': result([]) })
    renderCerts()
    expect(await screen.findByText('No certificates expire within this window.')).toBeInTheDocument()
    expect(tile('Expired').getByText('1')).toBeInTheDocument()
  })
})

describe('Certificates: read-only', () => {
  it('no revoke/delete/rotate/renew/edit controls; GETs only', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/certificates': result(ITEMS, 'c') })
    renderCerts()
    await screen.findByRole('table', { name: 'Certificates' })
    const main = within(screen.getByRole('main'))
    const buttons = main.getAllByRole('button').map((b) => b.textContent?.trim())
    expect(new Set(buttons)).toEqual(new Set(['Refresh', 'Apply', 'Previous', 'Next']))
    expect(main.queryByText(/\b(revoke|delete|rotate|renew|edit|reissue)\b/i)).not.toBeInTheDocument()
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
  })
})
