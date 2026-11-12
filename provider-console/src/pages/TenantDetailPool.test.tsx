import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import type { TenantDetail } from '@/api/types'
import { TenantRelayPoolSection } from '@/components/TenantRelayPool'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import {
  BACKUP_POOL_ID,
  DEFAULT_POOL,
  DEFAULT_POOL_ID,
  EU2_POOL_ID,
  EU_PLAIN_POOL_ID,
  EU_POOL,
  EU_POOL_ID,
  FAILOVER_POOLS,
  TENANT_DEFAULT_POOL,
} from '@/test/pools'
import { renderApp, signedInAs } from '@/test/render'

// Tenant detail's Relay pool section (D-38 … D-41). Assignment is
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
    'GET /provider/relay-pools': { status: 200, body: { items: FAILOVER_POOLS } },
    [`PUT /provider/tenants/${ID}/relay-pool`]: assign,
  })
  renderApp(`/tenants/${ID}`)
  return { user: userEvent.setup(), calls }
}

async function openChange(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: 'Change pool' }))
  return screen.findByRole('dialog', { name: 'Change relay pool' })
}

const EU_TENANT_POOL = { ...EU_POOL, eligible_relays: 1, previous: null, failover: null, using_failover: false, failed_over_since: null }
const EU2_REF = { id: EU2_POOL_ID, name: 'eu-2', region: 'eu-central-1', residency: true }
const BACKUP_REF = { id: BACKUP_POOL_ID, name: 'backup', region: null, residency: false }

describe('Tenant detail: relay pool section', () => {
  it('shows the pool (linked), region, residency, eligible relays and "No failover pool"', async () => {
    setup()
    const section = within(await screen.findByRole('region', { name: 'Relay pool' }))
    expect(section.getByRole('link', { name: 'default' })).toHaveAttribute('href', `/relay-pools/${DEFAULT_POOL_ID}`)
    expect(section.getByText('Eligible relays').nextElementSibling).toHaveTextContent('2')
    expect(within(section.getByRole('group', { name: 'Failover pool' })).getByText(/^No failover pool\./)).toBeInTheDocument()
    expect(section.queryByText(/Migrating from/)).not.toBeInTheDocument()
    expect(section.queryByText(/Using failover pool/)).not.toBeInTheDocument()
    expect(section.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('a residency pool shows "Residency" with no enforcement claim (the tenant read has no enforcement flag)', async () => {
    setup(tenant({ relay_pool: EU_TENANT_POOL }))
    const section = within(await screen.findByRole('region', { name: 'Relay pool' }))
    expect(section.getByText('eu-central-1')).toBeInTheDocument()
    const badge = section.getAllByText(/^Residency/).find((el) => el.hasAttribute('data-residency'))!
    expect(badge).toHaveTextContent(/^Residency\./) // label "Residency" + screen-reader note
    expect(badge).toHaveAttribute('data-residency', 'unknown')
    expect(badge).toHaveAttribute('title', expect.stringMatching(/shown on the pool’s page/))
    expect(section.queryByText(/\(enforced\)|\(not enforced\)/)).not.toBeInTheDocument()
  })

  it('shows the failover pool with its region and eligible relays, and warns when it has none', async () => {
    setup(tenant({ relay_pool: { ...EU_TENANT_POOL, failover: { ...EU2_REF, eligible_relays: 0 } } }))
    const fo = within(await screen.findByRole('group', { name: 'Failover pool' }))
    expect(fo.getByRole('link', { name: 'eu-2' })).toHaveAttribute('href', `/relay-pools/${EU2_POOL_ID}`)
    expect(fo.getByText('eu-central-1')).toBeInTheDocument()
    expect(fo.getByText('Failover eligible relays').nextElementSibling).toHaveTextContent('0')
    expect(fo.getByText(/eu-2 has no eligible relay, so it can’t take over if eu-1 fails/)).toBeInTheDocument()
  })

  it('says clearly when the tenant is using its failover pool', async () => {
    const since = '2026-11-12T10:00:00Z'
    setup(tenant({ relay_pool: { ...EU_TENANT_POOL, eligible_relays: 0, failover: { ...EU2_REF, eligible_relays: 1 }, using_failover: true, failed_over_since: since } }))
    const status = await screen.findByText(/Using failover pool/)
    expect(status).toHaveTextContent(`Using failover pool eu-2 since ${new Date(since).toLocaleString()}: the primary pool eu-1 has no eligible relay`)
    expect(within(status).getByRole('link', { name: 'eu-2' })).toBeInTheDocument()
    // While on failover, the "relay path is down" alert doesn't apply.
    expect(within(screen.getByRole('region', { name: 'Relay pool' })).queryByRole('alert')).not.toBeInTheDocument()
  })

  it('shows a graceful migration in progress', async () => {
    const until = '2026-11-12T10:05:00Z'
    setup(tenant({ relay_pool: { ...EU_TENANT_POOL, previous: { ...DEFAULT_POOL, until } } }))
    const line = await screen.findByText(/Migrating from/)
    expect(line).toHaveTextContent(`Migrating from default until ${new Date(until).toLocaleString()}`)
    expect(within(line).getByRole('link', { name: 'default' })).toHaveAttribute('href', `/relay-pools/${DEFAULT_POOL_ID}`)
  })

  it('warns when the pool has no eligible relay and there is no failover pool', async () => {
    setup(tenant({ relay_pool: { ...TENANT_DEFAULT_POOL, eligible_relays: 0 } }))
    const section = within(await screen.findByRole('region', { name: 'Relay pool' }))
    expect(section.getByRole('alert')).toHaveTextContent(/its relay path is down .* and it has no failover pool/)
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

type User = ReturnType<typeof userEvent.setup>

async function choose(user: User, dialog: HTMLElement, field: 'Primary pool' | 'Failover pool', option: RegExp) {
  await user.click(await within(dialog).findByRole('combobox', { name: field }))
  await user.click(await screen.findByRole('option', { name: option }))
}

async function optionsOf(user: User, dialog: HTMLElement, field: 'Primary pool' | 'Failover pool') {
  await user.click(await within(dialog).findByRole('combobox', { name: field }))
  const opts = (await screen.findAllByRole('option')).map((o) => ({ text: o.textContent ?? '', disabled: o.getAttribute('aria-disabled') === 'true' }))
  await user.keyboard('{Escape}')
  return opts
}

describe('Tenant detail: Change pool', () => {
  it('failover only (same primary): offers only pools that pass the D-39 rules; sends the whole assignment; no migration choice', async () => {
    const { user, calls } = setup()
    const dialog = await openChange(user)
    const save = within(dialog).getByRole('button', { name: 'Save pools' })
    expect(within(dialog).getByRole('combobox', { name: 'Primary pool' })).toHaveTextContent(/^default/)
    expect(within(dialog).getByRole('combobox', { name: 'Failover pool' })).toHaveTextContent('None')
    expect(within(dialog).getByText(/switches back once the primary has been healthy for 5 minutes by default/)).toBeInTheDocument()
    expect(within(dialog).getByText(/never cross-region/)).toBeInTheDocument()
    expect(within(dialog).queryByRole('group', { name: 'Migration' })).not.toBeInTheDocument()

    const opts = await optionsOf(user, dialog, 'Failover pool')
    const find = (re: RegExp) => opts.find((o) => re.test(o.text))!
    expect(find(/^None/).disabled).toBe(false)
    expect(find(/^default/)).toEqual({ text: expect.stringMatching(/\(the primary pool\)$/), disabled: true })
    expect(find(/^eu-1/)).toEqual({ text: expect.stringMatching(/\(another region\)$/), disabled: true })
    expect(find(/^spare/)).toEqual({ text: expect.stringMatching(/\(no eligible relay\)$/), disabled: true })
    expect(find(/^backup/).disabled).toBe(false)

    expect(save).toBeDisabled() // nothing changed
    await choose(user, dialog, 'Failover pool', /^backup/)
    expect(save).toBeDisabled() // no reason
    await user.type(within(dialog).getByLabelText('Reason'), ' add failover ')
    await user.click(save)
    expect(await screen.findByRole('status')).toHaveTextContent('Acme keeps default with failover pool backup.')
    expect(writes(calls)).toEqual([
      expect.objectContaining({
        method: 'PUT',
        path: `/provider/tenants/${ID}/relay-pool`,
        search: '',
        body: { pool_id: DEFAULT_POOL_ID, failover_pool_id: BACKUP_POOL_ID, reason: 'add failover', migration: 'graceful' },
      }),
    ])
    expect(detailCalls(calls)).toHaveLength(2) // exactly one audited reload
  })

  it('a residency primary offers only residency failover pools in its region; the current failover is preselected', async () => {
    const { user } = setup(tenant({ relay_pool: { ...EU_TENANT_POOL, failover: { ...EU2_REF, eligible_relays: 1 } } }))
    const dialog = await openChange(user)
    expect(within(dialog).getByRole('combobox', { name: 'Failover pool' })).toHaveTextContent(/^eu-2/)
    const opts = await optionsOf(user, dialog, 'Failover pool')
    expect(opts.find((o) => o.text.startsWith('eu-plain'))).toEqual({ text: expect.stringMatching(/\(not a residency pool\)$/), disabled: true })
    expect(opts.find((o) => o.text.startsWith('backup'))).toEqual({ text: expect.stringMatching(/\(another region\)$/), disabled: true })
    expect(opts.find((o) => o.text.startsWith('eu-2'))!.disabled).toBe(false)
  })

  it('choosing None removes the failover pool (failover_pool_id: null)', async () => {
    const { user, calls } = setup(tenant({ relay_pool: { ...EU_TENANT_POOL, failover: { ...EU2_REF, eligible_relays: 1 } } }))
    const dialog = await openChange(user)
    await choose(user, dialog, 'Failover pool', /^None/)
    await user.type(within(dialog).getByLabelText('Reason'), 'r')
    await user.click(within(dialog).getByRole('button', { name: 'Save pools' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Acme keeps eu-1 with no failover pool.')
    expect(writes(calls)[0].body).toEqual({ pool_id: EU_POOL_ID, failover_pool_id: null, reason: 'r', migration: 'graceful' })
  })

  it('changing only the primary still sends the current failover pool', async () => {
    const { user, calls } = setup(tenant({ relay_pool: { ...EU_TENANT_POOL, failover: { ...EU2_REF, eligible_relays: 1 } } }))
    const dialog = await openChange(user)
    await choose(user, dialog, 'Primary pool', /^eu-plain/)
    expect(within(dialog).getByRole('radio', { name: /Graceful/ })).toBeChecked()
    await user.type(within(dialog).getByLabelText('Reason'), 'r')
    await user.click(within(dialog).getByRole('button', { name: 'Save pools' }))
    expect(await screen.findByRole('status')).toHaveTextContent(/Acme now uses eu-plain with failover pool eu-2\. eu-1 stays authorized/)
    expect(writes(calls)[0].body).toEqual({ pool_id: EU_PLAIN_POOL_ID, failover_pool_id: EU2_POOL_ID, reason: 'r', migration: 'graceful' })
  })

  it('a new primary that no longer fits the failover pool blocks Save until the failover is fixed', async () => {
    const { user, calls } = setup(tenant({ relay_pool: { ...TENANT_DEFAULT_POOL, failover: { ...BACKUP_REF, eligible_relays: 2 } } }))
    const dialog = await openChange(user)
    const primaryOpts = await optionsOf(user, dialog, 'Primary pool')
    expect(primaryOpts.find((o) => o.text.startsWith('spare'))).toEqual({ text: expect.stringMatching(/\(no eligible relay\)$/), disabled: true })
    await choose(user, dialog, 'Primary pool', /^eu-1/)
    await user.type(within(dialog).getByLabelText('Reason'), 'r')
    const save = within(dialog).getByRole('button', { name: 'Save pools' })
    expect(within(dialog).getByRole('alert')).toHaveTextContent('This failover pool doesn’t fit the primary: another region.')
    expect(save).toBeDisabled()
    await choose(user, dialog, 'Failover pool', /^eu-2/)
    expect(save).toBeEnabled()
    await user.click(within(dialog).getByRole('radio', { name: /Immediate/ }))
    expect(within(dialog).getByText(/loses default at once/)).toBeInTheDocument()
    await user.click(save)
    await screen.findByRole('status')
    expect(writes(calls)[0].body).toEqual({ pool_id: EU_POOL_ID, failover_pool_id: EU2_POOL_ID, reason: 'r', migration: 'immediate' })
  })

  it.each([
    ['failover_region_mismatch', 400, /exactly the same region as the primary/, true],
    ['failover_needs_residency', 400, /must be a residency pool in the same region/, true],
    ['failover_same_as_primary', 400, /different pool from the primary/, true],
    ['pool_has_no_eligible_relay', 409, /A chosen pool \(primary or failover\) has no eligible relay/, false],
    ['pool_not_found', 400, /A chosen pool \(primary or failover\) no longer exists/, false],
    ['forbidden', 403, /Only super-admins can change a tenant’s relay pool/, false],
  ])('%s is explained in the dialog and nothing reloads', async (code, status, message, onField) => {
    const { user, calls } = setup(tenant(), { status, body: { error: code } })
    const dialog = await openChange(user)
    await choose(user, dialog, 'Failover pool', /^backup/)
    await user.type(within(dialog).getByLabelText('Reason'), 'r')
    await user.click(within(dialog).getByRole('button', { name: 'Save pools' }))
    const alert = await within(dialog).findByRole('alert')
    expect(alert).toHaveTextContent(message)
    // Failover rule errors replace the failover field's help text; others are a general alert.
    expect(within(dialog).queryByText(/Controller-driven/) === null).toBe(onField)
    expect(detailCalls(calls)).toHaveLength(1)
  })

  it('warns on a cross-region move of a residency tenant', async () => {
    const { user } = setup(tenant({ relay_pool: EU_TENANT_POOL }))
    const dialog = await openChange(user)
    await choose(user, dialog, 'Primary pool', /^default/)
    expect(within(dialog).getByText(/moves the tenant’s relay traffic to another region/)).toBeInTheDocument()
  })

  it('opening the dialog reads the pool list, not the audited tenant detail', async () => {
    const { user, calls } = setup()
    const dialog = await openChange(user)
    await within(dialog).findByRole('combobox', { name: 'Primary pool' })
    expect(detailCalls(calls)).toHaveLength(1)
    expect(calls.filter((c) => c.path === '/provider/relay-pools')).toHaveLength(1)
  })
})
