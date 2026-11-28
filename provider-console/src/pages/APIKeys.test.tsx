import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ApiKey } from '@/api/apiKeys'
import { daysUntil, expiresSoon, expiryText, presetExpiry, expiryValid } from '@/lib/apikey-format'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
import { renderApp, sessionStorageDump, signedInAs } from '@/test/render'

// Sprint 27 K2 (F35, D-76): Settings → API keys.

const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')
const DAY = 24 * 60 * 60 * 1000
const inDays = (d: number) => new Date(Date.now() + d * DAY).toISOString()

const KEY: ApiKey = {
  id: 'k1',
  name: 'ci-reader',
  role: 'auditor',
  prefix: 'zpk_1a2b3c4d',
  status: 'active',
  created_by: 'admin@provider.test',
  created_at: '2026-11-01T10:00:00Z',
  expires_at: inDays(80),
  last_used_at: null,
  last_used_ip: null,
  allowed_cidrs: [],
  revoked_at: null,
  revoked_by: null,
  revoke_reason: null,
}
const FULL_KEY = 'zpk_0f1e2d3c4b5a69788796a5b4c3d2e1f0_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG'

afterEach(() => vi.useRealTimers())

describe('API keys: access', () => {
  it('super-admin: nav entry under Governance and the page', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/api-keys': { status: 200, body: { items: [] } } })
    renderApp('/settings/api-keys')
    expect(await screen.findByText(/No API keys\. Create one for a script or integration that only needs to read\./)).toBeInTheDocument()
    expect(within(screen.getByRole('navigation', { name: 'Sections' })).getByRole('link', { name: 'API keys' })).toHaveAttribute(
      'href',
      '/settings/api-keys',
    )
  })

  it('relay-ops: no nav entry, Forbidden, zero requests', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({})
    renderApp('/settings/api-keys')
    expect(screen.getByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
    expect(within(screen.getByRole('navigation', { name: 'Sections' })).queryByRole('link', { name: 'API keys' })).not.toBeInTheDocument()
    await act(async () => {})
    expect(calls.filter((c) => c.path.includes('api-keys'))).toHaveLength(0)
  })
})

describe('API keys: list', () => {
  it('shows name, role, prefix only, created, expiry with the soon warning, last used, status', async () => {
    signedInAs(superAdminMe)
    const soon: ApiKey = { ...KEY, id: 'k2', name: 'nightly-export', role: 'billing', prefix: 'zpk_99887766', expires_at: inDays(5) }
    const revoked: ApiKey = { ...KEY, id: 'k3', name: 'old-script', status: 'revoked', revoked_at: '2026-11-20T00:00:00Z', revoked_by: 'admin@provider.test', revoke_reason: 'leaked' }
    const used: ApiKey = { ...KEY, id: 'k4', name: 'grafana', role: 'relay-ops', prefix: 'zpk_55443322', last_used_at: '2026-11-26T08:00:00Z', last_used_ip: '203.0.113.7', allowed_cidrs: ['203.0.113.0/24'] }
    mockFetch({ 'GET /provider/api-keys': { status: 200, body: { items: [KEY, soon, revoked, used] } } })
    renderApp('/settings/api-keys')
    const row = within(await screen.findByRole('row', { name: 'ci-reader' }))
    expect(row.getByText('Auditor')).toBeInTheDocument()
    expect(row.getByText('zpk_1a2b3c4d…')).toBeInTheDocument()
    expect(row.getByText('admin@provider.test')).toBeInTheDocument()
    expect(row.getByText(/expires in (79|80) days/)).toBeInTheDocument()
    expect(row.getByText('Never')).toBeInTheDocument()
    expect(row.queryByTestId('expires-soon')).not.toBeInTheDocument()

    const soonRow = within(screen.getByRole('row', { name: 'nightly-export' }))
    expect(soonRow.getByTestId('expires-soon')).toHaveTextContent(/expires in (4|5) days/)

    const revokedRow = within(screen.getByRole('row', { name: 'old-script' }))
    expect(revokedRow.getByText('Revoked')).toBeInTheDocument()
    expect(revokedRow.queryByRole('button', { name: /Revoke/ })).not.toBeInTheDocument()

    const usedRow = within(screen.getByRole('row', { name: 'grafana' }))
    expect(usedRow.getByText('203.0.113.7')).toBeInTheDocument()
    expect(usedRow.getByText('203.0.113.0/24')).toBeInTheDocument()
  })
})

describe('API keys: create', () => {
  it('sends the right body; the key is shown once, cleared on close and never stored', async () => {
    signedInAs(superAdminMe)
    let listed: ApiKey[] = []
    const { calls } = mockFetch({
      'GET /provider/api-keys': () => ({ status: 200, body: { items: listed } }),
      'POST /provider/api-keys': () => {
        listed = [KEY]
        return { status: 201, body: { api_key: KEY, key: FULL_KEY } }
      },
    })
    renderApp('/settings/api-keys')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Create key' }))
    const dialog = within(await screen.findByRole('dialog'))
    await user.type(dialog.getByLabelText('Name'), 'ci-reader')
    await user.selectOptions(dialog.getByLabelText('Role'), 'support')
    expect(dialog.getByTestId('key-role-help')).toHaveTextContent('Tenants and their agents')
    await user.click(dialog.getByRole('button', { name: '30 days' }))
    await user.type(dialog.getByLabelText(/Allowed source addresses/), '203.0.113.0/24, 2001:db8::/48')
    const submit = dialog.getByRole('button', { name: 'Create key' })
    expect(submit).toBeDisabled() // no reason yet
    await user.type(dialog.getByLabelText('Reason'), 'nightly export job')
    await user.click(submit)

    await waitFor(() => expect(writes(calls)).toHaveLength(1))
    const body = writes(calls)[0].body as Record<string, unknown>
    expect(body).toMatchObject({
      name: 'ci-reader',
      role: 'support',
      allowed_cidrs: ['203.0.113.0/24', '2001:db8::/48'],
      reason: 'nightly export job',
    })
    const days = (Date.parse(body.expires_at as string) - Date.now()) / DAY
    expect(days).toBeGreaterThan(29.9)
    expect(days).toBeLessThan(30.1)
    expect(body.expires_at as string).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/)

    expect(await screen.findByLabelText('API key')).toHaveTextContent(FULL_KEY)
    expect(screen.getByText(/You won’t see this key again/)).toBeInTheDocument()
    expect(sessionStorageDump()).not.toContain(FULL_KEY)
    expect(JSON.stringify({ ...localStorage })).not.toContain(FULL_KEY)
    await user.click(screen.getByRole('button', { name: 'I’ve saved it' }))
    await waitFor(() => expect(screen.queryByText(FULL_KEY)).not.toBeInTheDocument())
    expect(await screen.findByRole('row', { name: 'ci-reader' })).toBeInTheDocument()
    expect(document.body.textContent).not.toContain(FULL_KEY)
  })

  it('omits allowed_cidrs when empty, defaults to 90 days, and blocks a bad CIDR', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/api-keys': { status: 200, body: { items: [] } },
      'POST /provider/api-keys': { status: 201, body: { api_key: KEY, key: FULL_KEY } },
    })
    renderApp('/settings/api-keys')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Create key' }))
    const dialog = within(await screen.findByRole('dialog'))
    await user.type(dialog.getByLabelText('Name'), 'k')
    await user.type(dialog.getByLabelText('Reason'), 'r')
    await user.type(dialog.getByLabelText(/Allowed source addresses/), 'not-a-cidr')
    expect(dialog.getByText('Not a CIDR: not-a-cidr')).toBeInTheDocument()
    expect(dialog.getByRole('button', { name: 'Create key' })).toBeDisabled()
    await user.clear(dialog.getByLabelText(/Allowed source addresses/))
    expect(dialog.getByRole('button', { name: '90 days' })).toHaveAttribute('aria-pressed', 'true')
    await user.click(dialog.getByRole('button', { name: 'Create key' }))
    await waitFor(() => expect(writes(calls)).toHaveLength(1))
    const body = writes(calls)[0].body as Record<string, unknown>
    expect(body).not.toHaveProperty('allowed_cidrs')
    expect(body.role).toBe('auditor')
    expect(Math.round((Date.parse(body.expires_at as string) - Date.now()) / DAY)).toBe(90)
  })

  it('shows the server error (name taken) and keeps the dialog open', async () => {
    signedInAs(superAdminMe)
    mockFetch({
      'GET /provider/api-keys': { status: 200, body: { items: [] } },
      'POST /provider/api-keys': { status: 409, body: { error: 'name_taken' } },
    })
    renderApp('/settings/api-keys')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Create key' }))
    const dialog = within(await screen.findByRole('dialog'))
    await user.type(dialog.getByLabelText('Name'), 'dup')
    await user.type(dialog.getByLabelText('Reason'), 'r')
    await user.click(dialog.getByRole('button', { name: 'Create key' }))
    expect(await dialog.findByRole('alert')).toHaveTextContent('A key with this name already exists')
    expect(screen.queryByLabelText('API key')).not.toBeInTheDocument()
  })
})

describe('API keys: revoke', () => {
  it('needs a reason and the typed name, then sends both', async () => {
    signedInAs(superAdminMe)
    let listed: ApiKey[] = [KEY]
    const { calls } = mockFetch({
      'GET /provider/api-keys': () => ({ status: 200, body: { items: listed } }),
      'POST /provider/api-keys/k1/revoke': () => {
        listed = [{ ...KEY, status: 'revoked', revoked_at: '2026-11-27T00:00:00Z', revoked_by: 'admin@provider.test', revoke_reason: 'rotated' }]
        return { status: 200, body: { api_key: listed[0] } }
      },
    })
    renderApp('/settings/api-keys')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Revoke ci-reader' }))
    const dialog = within(await screen.findByRole('dialog'))
    const confirm = dialog.getByRole('button', { name: 'Revoke key' })
    await user.type(dialog.getByLabelText(/Reason/), 'rotated')
    expect(confirm).toBeDisabled() // name not typed
    await user.type(dialog.getByRole('textbox', { name: /Type/ }), 'ci-reade')
    expect(confirm).toBeDisabled()
    await user.type(dialog.getByRole('textbox', { name: /Type/ }), 'r')
    await user.click(confirm)
    await waitFor(() => expect(writes(calls)).toHaveLength(1))
    expect(writes(calls)[0]).toMatchObject({ method: 'POST', path: '/provider/api-keys/k1/revoke', body: { reason: 'rotated', confirm_name: 'ci-reader' } })
    const row = within(await screen.findByRole('row', { name: 'ci-reader' }))
    expect(await row.findByText('Revoked')).toBeInTheDocument()
  })
})

describe('API keys: format helpers', () => {
  const now = new Date('2026-11-27T12:00:00Z')
  it('expiry presets stay inside the 365-day limit', () => {
    expect(expiryValid(presetExpiry(30, now), now)).toBe(true)
    expect(expiryValid(presetExpiry(90, now), now)).toBe(true)
    expect(expiryValid(presetExpiry(365, now), now)).toBe(true)
    expect(expiryValid(new Date(now.getTime() + 365 * DAY), now)).toBe(false)
    expect(expiryValid(new Date(now.getTime() - 1000), now)).toBe(false)
    expect(expiryValid(null, now)).toBe(false)
  })
  it('expires-soon warning under 14 days, active keys only', () => {
    const at = (d: number) => new Date(now.getTime() + d * DAY).toISOString()
    expect(expiresSoon('active', at(13.9), now)).toBe(true)
    expect(expiresSoon('active', at(14.1), now)).toBe(false)
    expect(expiresSoon('revoked', at(3), now)).toBe(false)
    expect(expiresSoon('active', at(-1), now)).toBe(false)
    expect(expiryText(at(0.5), now)).toBe('expires today')
    expect(expiryText(at(1.5), now)).toBe('expires in 1 day')
    expect(expiryText(at(-0.1), now)).toBe('expired')
    expect(daysUntil(at(10.5), now)).toBe(10)
  })
})
