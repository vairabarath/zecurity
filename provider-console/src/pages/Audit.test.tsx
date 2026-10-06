import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { StrictMode } from 'react'
import { MemoryRouter, useLocation } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { AppRoutes } from '@/App'
import type { AuditEntry, JsonValue } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { signedInAs } from '@/test/render'

const MIN = 60_000
const iso = (ms: number) => new Date(Date.now() + ms).toISOString()
const TENANT = '1f3c5a7e-1111-2222-3333-444444444444'

let seq = 0
function entry(over: Partial<AuditEntry>): AuditEntry {
  seq++
  return {
    id: `00000000-0000-0000-0000-${String(seq).padStart(12, '0')}`,
    created_at: iso(-seq * MIN),
    provider_user_id: '11111111-1111-1111-1111-111111111111',
    provider_email: 'admin@provider.test',
    action: 'provider_session.login',
    target_type: 'provider_user',
    target_id: '11111111-1111-1111-1111-111111111111',
    details: { amr: ['pwd'] },
    ip_address: '10.0.0.5',
    ...over,
  }
}

// Phase H/U actions plus a tenant read, newest first as the API returns them.
const ENTRIES: AuditEntry[] = [
  entry({ action: 'tenant.read', target_type: 'tenant', target_id: TENANT, details: { view: 'detail' }, ip_address: null }),
  entry({ action: 'provider_user.disable', provider_email: 'boss@provider.test' }),
  entry({ action: 'provider_user.enable' }),
  entry({ action: 'provider_user.role_change' }),
  entry({ action: 'provider_user.create', details: { email: 'ops@provider.test', role: 'relay-ops' } }),
  entry({ action: 'provider_user.password_change', details: { was_required: true } }),
  entry({ action: 'provider_session.login' }),
]

function LocationProbe() {
  const l = useLocation()
  return <output data-testid="location">{l.pathname + l.search}</output>
}

function renderAudit(path = '/audit', strict = false) {
  const [pathname, search = ''] = path.split('?')
  const tree = (
    <MemoryRouter initialEntries={[{ pathname, search: search ? `?${search}` : '' }]}>
      <AppRoutes />
      <LocationProbe />
    </MemoryRouter>
  )
  return render(strict ? <StrictMode>{tree}</StrictMode> : tree)
}

const auditCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path === '/provider/audit')
const qs = (c: RecordedCall) => Object.fromEntries(new URLSearchParams(c.search))
const quiet = () => new Promise((r) => setTimeout(r, 50))
const page = (items: AuditEntry[], next: string | null = null): FakeResponse => ({ status: 200, body: { items, next_cursor: next } })
const rows = () => within(screen.getByRole('table', { name: 'Audit entries' })).getAllByRole('row').slice(1)
const toRFC = (local: string) => new Date(local).toISOString().replace(/\.\d{3}Z$/, 'Z')
const location = () => screen.getByTestId('location').textContent ?? ''

describe('Audit: authorization', () => {
  it('super-admin can open /audit and has the nav entry', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/audit': page(ENTRIES) })
    renderAudit()
    await screen.findByRole('table', { name: 'Audit entries' })
    expect(within(screen.getByRole('navigation', { name: 'Sections' })).getByRole('link', { name: 'Audit' })).toHaveAttribute('href', '/audit')
  })

  it('relay-ops: no nav entry, Forbidden on direct navigation, zero requests', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({})
    renderAudit()
    expect(screen.getByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
    expect(within(screen.getByRole('navigation', { name: 'Sections' })).queryByRole('link', { name: 'Audit' })).not.toBeInTheDocument()
    await quiet()
    expect(calls).toHaveLength(0)
  })

  it('a server role 403 shows Forbidden', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/audit': { status: 403, body: { error: 'forbidden' } } })
    renderAudit()
    expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
  })
})

describe('Audit: table', () => {
  it('renders every column, newest first, in the API order', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/audit': page(ENTRIES) })
    renderAudit()
    await screen.findByRole('table', { name: 'Audit entries' })
    const r = rows()
    expect(r.map((row) => within(row).getAllByRole('cell')[2].textContent)).toEqual(ENTRIES.map((e) => e.action))
    const first = within(r[0]).getAllByRole('cell')
    expect(first[0].textContent).toBe('1 min ago')
    expect(first[0]).toHaveAttribute('title', new Date(ENTRIES[0].created_at).toLocaleString())
    expect(first[1].textContent).toBe('admin@provider.test') // the provider operator from the audit row
    expect(first[3].textContent).toBe('tenant · 1f3c5a7e…')
    expect(first[3]).toHaveAttribute('title', `tenant · ${TENANT}`)
    expect(first[4].textContent).toBe('—') // null IP
    expect(within(r[1]).getAllByRole('cell')[1].textContent).toBe('boss@provider.test')
    expect(within(r[1]).getAllByRole('cell')[4].textContent).toBe('10.0.0.5')
  })

  it('shows the Phase H/U actions', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/audit': page(ENTRIES) })
    renderAudit()
    await screen.findByRole('table', { name: 'Audit entries' })
    for (const a of ['provider_session.login', 'provider_user.password_change', 'provider_user.create', 'provider_user.role_change', 'provider_user.disable', 'provider_user.enable']) {
      expect(screen.getByText(a)).toBeInTheDocument()
    }
  })

  it('empty result shows the empty message', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/audit': page([]) })
    renderAudit()
    expect(await screen.findByText('No audit entries match these filters.')).toBeInTheDocument()
  })
})

describe('Audit: details', () => {
  it('collapsed by default; toggles open and closed with zero requests; never in the URL', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/audit': page(ENTRIES) })
    renderAudit()
    await screen.findByRole('table', { name: 'Audit entries' })
    expect(screen.queryByLabelText('Details JSON')).not.toBeInTheDocument()
    const user = userEvent.setup()
    const row = within(rows()[4])
    await user.click(row.getByRole('button', { name: 'Show details' }))
    expect(row.getByLabelText('Details JSON').textContent).toBe(JSON.stringify(ENTRIES[4].details, null, 2))
    expect(row.getByRole('button', { name: 'Hide details' })).toHaveAttribute('aria-expanded', 'true')
    await user.click(row.getByRole('button', { name: 'Hide details' }))
    expect(row.queryByLabelText('Details JSON')).not.toBeInTheDocument()
    await quiet()
    expect(calls).toHaveLength(1) // every endpoint counted: toggling sends nothing at all
    expect(location()).toBe('/audit')
  })

  it('renders details as plain text: script-like content is not interpreted', async () => {
    signedInAs(superAdminMe)
    const evil = '<script>window.__pwned=1</script><img src=x onerror="window.__pwned=2">'
    mockFetch({ 'GET /provider/audit': page([entry({ details: { note: evil } })]) })
    renderAudit()
    await screen.findByRole('table', { name: 'Audit entries' })
    await userEvent.setup().click(screen.getByRole('button', { name: 'Show details' }))
    const pre = screen.getByLabelText('Details JSON')
    expect(pre.textContent).toContain(evil.replace(/"/g, '\\"'))
    expect(pre.querySelector('script, img')).toBeNull()
    expect(document.querySelector('main script, main img')).toBeNull()
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined()
  })

  it.each<[string, JsonValue]>([
    ['null', null],
    ['true', true],
    ['false', false],
    ['number', 42.5],
    ['string', 'plain <b>text</b>'],
    ['array', [1, 'two', null]],
    ['object', { a: { b: [true] } }],
  ])('details value %s renders as its JSON text', async (_n, value) => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/audit': page([entry({ details: value })]) })
    renderAudit()
    await screen.findByRole('table', { name: 'Audit entries' })
    await userEvent.setup().click(screen.getByRole('button', { name: 'Show details' }))
    const pre = screen.getByLabelText('Details JSON')
    expect(pre.textContent).toBe(JSON.stringify(value, null, 2))
    expect(pre.children).toHaveLength(0) // text only, no elements
  })
})

describe('Audit: filters', () => {
  async function ready() {
    signedInAs(superAdminMe)
    const fetched = mockFetch({ 'GET /provider/audit': page(ENTRIES) })
    renderAudit()
    await screen.findByRole('table', { name: 'Audit entries' })
    return fetched
  }

  it('typing sends zero requests; Apply sends exactly one with each text filter', async () => {
    const { calls } = await ready()
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Action'), 'tenant.read')
    await user.type(screen.getByLabelText('Target type'), 'tenant')
    await user.type(screen.getByLabelText('Target ID'), TENANT)
    await user.type(screen.getByLabelText('Operator email'), 'Boss@Provider.Test')
    await quiet()
    expect(auditCalls(calls)).toHaveLength(1)
    await user.click(screen.getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(auditCalls(calls)).toHaveLength(2))
    // Sent exactly as typed: no case normalization (the server matches emails case-insensitively).
    expect(qs(auditCalls(calls)[1])).toEqual({ action: 'tenant.read', target_type: 'tenant', target_id: TENANT, provider_email: 'Boss@Provider.Test' })
    expect(location()).toBe(`/audit?action=tenant.read&provider_email=Boss%40Provider.Test&target_id=${TENANT}&target_type=tenant`)
    await quiet()
    expect(auditCalls(calls)).toHaveLength(2)
  })

  it.each([
    ['Action', 'action', 'provider_user.create'],
    ['Target type', 'target_type', 'relay'],
    ['Target ID', 'target_id', 'abc-123'],
    ['Operator email', 'provider_email', 'ops@provider.test'],
  ])('%s → %s only', async (label, key, value) => {
    const { calls } = await ready()
    const user = userEvent.setup()
    await user.type(screen.getByLabelText(label), value)
    await user.click(screen.getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(auditCalls(calls)).toHaveLength(2))
    expect(qs(auditCalls(calls)[1])).toEqual({ [key]: value })
  })

  it('Enter in the form sends exactly one request', async () => {
    const { calls } = await ready()
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Action'), 'tenant.read{Enter}')
    await waitFor(() => expect(auditCalls(calls)).toHaveLength(2))
    await quiet()
    expect(auditCalls(calls)).toHaveLength(2)
    expect(qs(auditCalls(calls)[1])).toEqual({ action: 'tenant.read' })
  })

  it('From/Until are sent as RFC 3339 and keep the half-open window as chosen', async () => {
    const { calls } = await ready()
    fireEvent.change(screen.getByLabelText('From (inclusive)'), { target: { value: '2026-10-01T09:30' } })
    fireEvent.change(screen.getByLabelText('Until (exclusive)'), { target: { value: '2026-10-02T18:45' } })
    await userEvent.setup().click(screen.getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(auditCalls(calls)).toHaveLength(2))
    const sent = qs(auditCalls(calls)[1])
    expect(sent).toEqual({ since: toRFC('2026-10-01T09:30'), until: toRFC('2026-10-02T18:45') })
    expect(sent.since).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:00Z$/)
  })

  it('an equal From and Until is allowed (an empty half-open window), not invalid', async () => {
    const { calls } = await ready()
    fireEvent.change(screen.getByLabelText('From (inclusive)'), { target: { value: '2026-10-01T09:30' } })
    fireEvent.change(screen.getByLabelText('Until (exclusive)'), { target: { value: '2026-10-01T09:30' } })
    await userEvent.setup().click(screen.getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(auditCalls(calls)).toHaveLength(2))
    expect(screen.queryByText(/Invalid filter/)).not.toBeInTheDocument()
  })

  it('From after Until is Invalid filter with zero requests; Reset recovers', async () => {
    const { calls } = await ready()
    const user = userEvent.setup()
    fireEvent.change(screen.getByLabelText('From (inclusive)'), { target: { value: '2026-10-03T00:00' } })
    fireEvent.change(screen.getByLabelText('Until (exclusive)'), { target: { value: '2026-10-01T00:00' } })
    await user.click(screen.getByRole('button', { name: 'Apply' }))
    expect(await screen.findByText(/Invalid filter/)).toBeInTheDocument()
    await quiet()
    expect(auditCalls(calls)).toHaveLength(1) // nothing sent for the inverted range
    await user.click(screen.getByRole('button', { name: 'Reset filters' }))
    await waitFor(() => expect(auditCalls(calls)).toHaveLength(2))
    expect(qs(auditCalls(calls)[1])).toEqual({})
  })

  it.each([
    ['inverted range', 'since=2026-10-03T00:00:00Z&until=2026-10-01T00:00:00Z'],
    ['malformed since', 'since=yesterday'],
    ['malformed until', 'until=2026-13-45T99:00:00Z'],
    ['date without time', 'since=2026-10-01'],
  ])('a hand-edited URL with %s is Invalid filter with zero requests', async (_n, query) => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/audit': page(ENTRIES) })
    renderAudit(`/audit?${query}`)
    expect(await screen.findByText(/Invalid filter/)).toBeInTheDocument()
    await quiet()
    expect(auditCalls(calls)).toHaveLength(0)
  })

  it('a URL with valid filters loads them; unknown URL keys are ignored', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/audit': page(ENTRIES) })
    renderAudit('/audit?action=tenant.read&since=2026-10-01T00:00:00Z&token=tok-secret&details=x')
    await screen.findByRole('table', { name: 'Audit entries' })
    expect(qs(auditCalls(calls)[0])).toEqual({ action: 'tenant.read', since: '2026-10-01T00:00:00Z' })
    expect(screen.getByLabelText('Action')).toHaveValue('tenant.read')
  })

  it('Clear resets every filter and pagination', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/audit': (c) => page(ENTRIES, qs(c).cursor ? null : 'c1') })
    renderAudit('/audit?action=tenant.read')
    const user = userEvent.setup()
    await screen.findByRole('table', { name: 'Audit entries' })
    await user.click(screen.getByRole('button', { name: /Next/ }))
    await screen.findByText('Page 2')
    await user.click(screen.getByRole('button', { name: 'Clear' }))
    await waitFor(() => expect(qs(auditCalls(calls).at(-1)!)).toEqual({}))
    expect(location()).toBe('/audit')
    expect(screen.getByLabelText('Action')).toHaveValue('')
    expect(screen.queryByText('Page 2')).not.toBeInTheDocument()
  })
})

describe('Audit: pagination', () => {
  it('Next uses the returned cursor, Previous the stack; a filter change resets to page 1 with no foreign cursor', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/audit': (c) => {
        const p = qs(c)
        const tag = p.action ?? 'all'
        if (p.cursor && !p.cursor.startsWith(tag)) return { status: 400, body: { error: 'invalid_cursor' } }
        const n = p.cursor ? Number(p.cursor.split('#')[1]) : 1
        return page([entry({ action: `${tag}-p${n}` })], n < 3 ? `${tag}#${n + 1}` : null)
      },
    })
    renderAudit()
    const user = userEvent.setup()
    await screen.findByText('all-p1')
    await user.click(screen.getByRole('button', { name: /Next/ }))
    await screen.findByText('all-p2')
    expect(qs(auditCalls(calls)[1]).cursor).toBe('all#2')
    await user.click(screen.getByRole('button', { name: /Next/ }))
    await screen.findByText('all-p3')
    await user.click(screen.getByRole('button', { name: /Previous/ }))
    await screen.findByText('all-p2')
    expect(qs(auditCalls(calls)[3]).cursor).toBe('all#2')

    await user.type(screen.getByLabelText('Action'), 'x{Enter}')
    expect(await screen.findByText('x-p1')).toBeInTheDocument()
    expect(qs(auditCalls(calls).at(-1)!)).toEqual({ action: 'x' })
    expect(screen.queryByText(/Page \d/)).toHaveTextContent('Page 1')
  })
})

describe('Audit: request behavior', () => {
  it('StrictMode: exactly one request; focus/visibility/reconnect add none', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/audit': page(ENTRIES) })
    renderAudit('/audit', true)
    await screen.findByRole('table', { name: 'Audit entries' })
    act(() => {
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
      window.dispatchEvent(new Event('online'))
    })
    await quiet()
    expect(auditCalls(calls)).toHaveLength(1)
  })

  it('Refresh sends exactly one new request on the same filters', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/audit': page(ENTRIES) })
    renderAudit('/audit?action=tenant.read')
    await screen.findByRole('table', { name: 'Audit entries' })
    await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh audit' }))
    await waitFor(() => expect(auditCalls(calls)).toHaveLength(2))
    await quiet()
    expect(auditCalls(calls)).toHaveLength(2)
    expect(qs(auditCalls(calls)[1])).toEqual({ action: 'tenant.read' })
  })

  it('an error never retries by itself; Retry sends exactly one request', async () => {
    signedInAs(superAdminMe)
    let fail = true
    const { calls } = mockFetch({ 'GET /provider/audit': () => (fail ? { status: 500, body: { error: 'server_error' } } : page(ENTRIES)) })
    renderAudit()
    expect(await screen.findByText('Couldn’t load audit entries.')).toBeInTheDocument()
    await quiet()
    expect(auditCalls(calls)).toHaveLength(1)
    fail = false
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
    await screen.findByRole('table', { name: 'Audit entries' })
    expect(auditCalls(calls)).toHaveLength(2)
  })
})

describe('Audit: read-only and security', () => {
  it('has no mutating controls and sends GETs only', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/audit': page(ENTRIES, 'c') })
    renderAudit()
    await screen.findByRole('table', { name: 'Audit entries' })
    const main = within(screen.getByRole('main'))
    const buttons = main.getAllByRole('button').map((b) => b.textContent?.trim())
    expect(new Set(buttons)).toEqual(new Set(['Refresh', 'Apply', 'Clear', 'Show details', 'Previous', 'Next']))
    expect(main.queryByRole('button', { name: /\b(revoke|delete|edit|create|suspend|disable|enable)\b/i })).not.toBeInTheDocument()
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
  })

  it('no tenant identity information is rendered and nothing secret reaches the URL', async () => {
    signedInAs(superAdminMe)
    const leaky = { ...entry({}), tenant_user_email: 'u.canary@acme.example' }
    const { calls } = mockFetch({ 'GET /provider/audit': page([leaky as AuditEntry]) })
    renderAudit()
    await screen.findByRole('table', { name: 'Audit entries' })
    expect(document.body.textContent).not.toMatch(/canary|acme\.example/)
    for (const c of calls) expect(c.search + location()).not.toMatch(/tok|password|secret|details/i)
  })
})
