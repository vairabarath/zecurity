import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import type { TenantDetail } from '@/api/types'
import { TenantRelayPoolSection } from '@/components/TenantRelayPool'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { DEFAULT_POOL, DEFAULT_POOL_ID, EU_POOL, EU_POOL_ID, POOLS, TENANT_DEFAULT_POOL } from '@/test/pools'
import { renderApp, signedInAs } from '@/test/render'

// Tenant detail's Relay pool section (D-38, D-40, D-41). Assignment is
// super-admin only; every successful tenant GET is an audited read, so the
// pool list for the dialog comes from the (unaudited) pool list read.

const ID = 'aaaaaaaa-1111-2222-3333-444444444444'

function tenant(over: Partial<TenantDetail> = {}): TenantDetail {
  return {
    id: ID,
    slug: 'acme',
    name: 'Acme',
    status: 'active',
    trust_domain: 'acme.zecurity.test',
    created_at: '2026-10-01T00:00:00Z',
    ca_not_after: null,
    counts: {
      connectors: { pending: 0, active: 1, disconnected: 0, revoked: 0 },
      shields: { pending: 0, active: 0, disconnected: 0, revoked: 0 },
      remote_networks: 0,
      users: { active: 0, suspended: 0, locked: 0, deleted: 0 },
      client_devices: { active: 0, re_enroll_required: 0, renew_pending: 0, revoked: 0 },
      policies: { resources: 0, groups: 0, access_rules: 0, resource_policies: 0 },
    },
    remote_networks: [],
    remote_networks_truncated: false,
    connectors: [],
    connectors_truncated: false,
    shields: [],
    shields_truncated: false,
    relay_pool: TENANT_DEFAULT_POOL,
    ...over,
  }
}

const detailCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path === `/provider/tenants/${ID}`)
const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')

function setup(body: TenantDetail = tenant(), assign: FakeResponse = { status: 204 }) {
  signedInAs(superAdminMe)
  const { calls } = mockFetch({
    [`GET /provider/tenants/${ID}`]: { status: 200, body },
    'GET /provider/relay-pools': { status: 200, body: { items: POOLS } },
    [`PUT /provider/tenants/${ID}/relay-pool`]: assign,
  })
  renderApp(`/tenants/${ID}`)
  return { user: userEvent.setup(), calls }
}

async function openChange(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: 'Change pool' }))
  return screen.findByRole('dialog', { name: 'Change relay pool' })
}

describe('Tenant detail: relay pool section', () => {
  it('shows the pool (linked), region, residency and eligible relays; no migration line', async () => {
    setup()
    const section = within(await screen.findByRole('region', { name: 'Relay pool' }))
    expect(section.getByRole('link', { name: 'default' })).toHaveAttribute('href', `/relay-pools/${DEFAULT_POOL_ID}`)
    expect(section.getByText('Eligible relays').nextElementSibling).toHaveTextContent('2')
    expect(section.queryByText(/Migrating from/)).not.toBeInTheDocument()
    expect(section.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('a residency pool reads as routing only', async () => {
    setup(tenant({ relay_pool: { ...EU_POOL, eligible_relays: 1, previous: null } }))
    const section = within(await screen.findByRole('region', { name: 'Relay pool' }))
    expect(section.getByText('eu-central-1')).toBeInTheDocument()
    expect(section.getByText(/Residency \(routing only\)/)).toHaveTextContent(/not enforced/)
  })

  it('shows a graceful migration in progress', async () => {
    const until = '2026-11-12T10:05:00Z'
    setup(tenant({ relay_pool: { ...EU_POOL, eligible_relays: 1, previous: { ...DEFAULT_POOL, until } } }))
    const line = await screen.findByText(/Migrating from/)
    expect(line).toHaveTextContent(`Migrating from default until ${new Date(until).toLocaleString()}`)
    expect(within(line).getByRole('link', { name: 'default' })).toHaveAttribute('href', `/relay-pools/${DEFAULT_POOL_ID}`)
  })

  it('warns when the pool has no eligible relay', async () => {
    setup(tenant({ relay_pool: { ...TENANT_DEFAULT_POOL, eligible_relays: 0 } }))
    const section = within(await screen.findByRole('region', { name: 'Relay pool' }))
    expect(section.getByRole('alert')).toHaveTextContent(/no eligible relay, so its relay path is down/)
    expect(section.getByText('No eligible relay')).toBeInTheDocument()
  })

  it('relay-ops get no Change pool control (the section itself; the page is super-admin only)', () => {
    signedInAs(relayOpsMe)
    render(
      <MemoryRouter>
        <TenantRelayPoolSection tenant={tenant()} onDone={vi.fn()} />
      </MemoryRouter>,
    )
    expect(screen.queryByRole('button', { name: 'Change pool' })).not.toBeInTheDocument()
  })
})

describe('Tenant detail: Change pool', () => {
  it('offers other pools only; a pool with no eligible relay can’t be chosen; reason required; graceful by default', async () => {
    const { user, calls } = setup()
    const dialog = await openChange(user)
    expect(within(dialog).getByRole('radio', { name: /Graceful/ })).toBeChecked()
    expect(within(dialog).getByText(/connect to the new pool before leaving the old one/)).toBeInTheDocument()
    expect(within(dialog).getByText(/relay availability becomes the new pool’s/)).toBeInTheDocument()
    await user.click(await within(dialog).findByRole('combobox', { name: 'New pool' }))
    const options = await screen.findAllByRole('option')
    expect(options.map((o) => o.textContent)).toEqual([expect.stringMatching(/^eu-1/), expect.stringMatching(/^spare.*\(no eligible relay\)$/)])
    expect(screen.getByRole('option', { name: /^spare/ })).toHaveAttribute('aria-disabled', 'true')
    await user.click(screen.getByRole('option', { name: /^eu-1/ }))
    const submit = within(dialog).getByRole('button', { name: 'Change pool' })
    expect(submit).toBeDisabled()
    await user.type(within(dialog).getByLabelText('Reason'), ' EU customer ')
    await user.click(submit)
    expect(await screen.findByRole('status')).toHaveTextContent(/Acme now uses eu-1\. default stays authorized for the migration window/)
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'PUT', path: `/provider/tenants/${ID}/relay-pool`, search: '', body: { pool_id: EU_POOL_ID, reason: 'EU customer', migration: 'graceful' } }),
    ])
    expect(detailCalls(calls)).toHaveLength(2) // exactly one audited reload
  })

  it('immediate is sent when chosen', async () => {
    const { user, calls } = setup()
    const dialog = await openChange(user)
    await user.click(within(dialog).getByRole('radio', { name: /Immediate/ }))
    expect(within(dialog).getByText(/loses default at once/)).toBeInTheDocument()
    await user.click(await within(dialog).findByRole('combobox', { name: 'New pool' }))
    await user.click(await screen.findByRole('option', { name: /^eu-1/ }))
    await user.type(within(dialog).getByLabelText('Reason'), 'r')
    await user.click(within(dialog).getByRole('button', { name: 'Change pool' }))
    await screen.findByRole('status')
    expect(writes(calls)[0].body).toMatchObject({ migration: 'immediate' })
  })

  it.each([
    ['pool_has_no_eligible_relay', 409, /has no eligible relay, so the tenant’s relay path would stop working/],
    ['pool_not_found', 400, /That pool no longer exists/],
    ['forbidden', 403, /Only super-admins can change a tenant’s relay pool/],
  ])('%s is explained in the dialog and nothing reloads', async (code, status, message) => {
    const { user, calls } = setup(tenant(), { status, body: { error: code } })
    const dialog = await openChange(user)
    await user.click(await within(dialog).findByRole('combobox', { name: 'New pool' }))
    await user.click(await screen.findByRole('option', { name: /^eu-1/ }))
    await user.type(within(dialog).getByLabelText('Reason'), 'r')
    await user.click(within(dialog).getByRole('button', { name: 'Change pool' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(message)
    expect(detailCalls(calls)).toHaveLength(1)
  })

  it('warns on a cross-region move of a residency tenant', async () => {
    const { user } = setup(tenant({ relay_pool: { ...EU_POOL, eligible_relays: 1, previous: null } }))
    const dialog = await openChange(user)
    await user.click(await within(dialog).findByRole('combobox', { name: 'New pool' }))
    const opts = (await screen.findAllByRole('option')).map((o) => o.textContent)
    expect(opts.some((o) => o?.startsWith('eu-1'))).toBe(false) // current pool not offered
    await user.click(screen.getByRole('option', { name: /^default/ }))
    expect(within(dialog).getByText(/moves the tenant’s relay traffic to another region/)).toBeInTheDocument()
  })

  it('opening the dialog reads the pool list, not the audited tenant detail', async () => {
    const { user, calls } = setup()
    await openChange(user)
    await screen.findByRole('combobox', { name: 'New pool' })
    expect(detailCalls(calls)).toHaveLength(1)
    expect(calls.filter((c) => c.path === '/provider/relay-pools')).toHaveLength(1)
  })
})
