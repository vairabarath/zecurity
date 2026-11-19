import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import type { TenantDetail, TenantQuota } from '@/api/types'
import { TenantQuotasSection } from '@/components/TenantQuotas'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { FREE_QUOTAS, TENANT_FREE_PLAN } from '@/test/plans'
import { TENANT_DEFAULT_POOL } from '@/test/pools'
import { renderApp, signedInAs } from '@/test/render'

// Tenant detail's Quotas section (Sprint 23 S3, D-47): limit, source and
// usage per key; "Over quota" when a lowered limit is below usage; Change
// (super-admin, reason) sends one PUT and reloads once.

const ID = 'aaaaaaaa-1111-2222-3333-444444444444'

function quotas(over: Partial<Record<TenantQuota['key'], Partial<TenantQuota>>> = {}): TenantQuota[] {
  return FREE_QUOTAS.map((q) => ({ ...q, ...(over[q.key] ?? {}) }))
}

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
    plan: TENANT_FREE_PLAN,
    quotas: quotas(),
    suspension: null,
    deletion: null,
    first_admin_invitation: null,
    ...over,
  }
}

const detailCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path === `/provider/tenants/${ID}`)
const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')

function setup(body: TenantDetail, put: FakeResponse = { status: 204 }, key = 'connectors') {
  signedInAs(superAdminMe)
  const { calls } = mockFetch({
    [`GET /provider/tenants/${ID}`]: { status: 200, body },
    [`PUT /provider/tenants/${ID}/quotas/${key}`]: put,
  })
  renderApp(`/tenants/${ID}`)
  return { user: userEvent.setup(), calls }
}

describe('Tenant detail: Quotas', () => {
  it('shows used, limit and source per key, and "Over quota" only where usage is above the limit', async () => {
    setup(
      tenant({
        quotas: quotas({
          connectors: { used: 4, limit: 3, source: 'override', over: true },
          users: { used: 2 },
          shields: { limit: null, source: 'unlimited' },
        }),
      }),
    )
    const table = within(await screen.findByRole('table', { name: 'Quotas' }))
    const row = (name: string) => within(table.getByRole('row', { name: new RegExp(`^${name}`) }))
    expect(row('Connectors').getByText('4')).toBeInTheDocument()
    expect(row('Connectors').getByText('3')).toBeInTheDocument()
    expect(row('Connectors').getByText('Override')).toBeInTheDocument()
    expect(row('Connectors').getByText('Over quota')).toBeInTheDocument()
    expect(row('Users').getByText('2')).toBeInTheDocument()
    expect(row('Users').getByText('Plan')).toBeInTheDocument()
    expect(row('Users').queryByText('Over quota')).not.toBeInTheDocument()
    expect(row('Shields').getByText('Unlimited')).toBeInTheDocument()
    expect(table.getAllByText('Over quota')).toHaveLength(1)
  })

  it('sets a tenant limit with a reason: one PUT with the number in the body; reloads once', async () => {
    const { user, calls } = setup(tenant())
    await user.click(await screen.findByRole('button', { name: 'Change Connectors limit' }))
    const dialog = await screen.findByRole('dialog', { name: 'Change connectors limit' })
    const save = within(dialog).getByRole('button', { name: 'Save limit' })
    expect(save).toBeDisabled()
    await user.click(within(dialog).getByLabelText('Set a limit for this tenant'))
    await user.type(within(dialog).getByLabelText('New limit'), '3')
    expect(save).toBeDisabled() // reason required
    await user.type(within(dialog).getByLabelText('Reason'), ' pilot ')
    await user.click(save)
    await waitFor(() => expect(detailCalls(calls)).toHaveLength(2))
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'PUT', path: `/provider/tenants/${ID}/quotas/connectors`, search: '', body: { limit: 3, reason: 'pilot' } }),
    ])
    expect(await screen.findByText('Acme: the connectors limit is now 3.')).toBeInTheDocument()
  })

  it('unlimited sends limit null; back to the plan sends inherit', async () => {
    const { user, calls } = setup(tenant({ quotas: quotas({ connectors: { limit: 3, source: 'override' } }) }))
    await user.click(await screen.findByRole('button', { name: 'Change Connectors limit' }))
    let dialog = await screen.findByRole('dialog', { name: 'Change connectors limit' })
    await user.click(within(dialog).getByLabelText('Unlimited for this tenant'))
    await user.type(within(dialog).getByLabelText('Reason'), 'trial')
    await user.click(within(dialog).getByRole('button', { name: 'Save limit' }))
    await waitFor(() => expect(writes(calls)).toHaveLength(1))
    expect(writes(calls)[0].body).toEqual({ limit: null, reason: 'trial' })

    await user.click(await screen.findByRole('button', { name: 'Change Connectors limit' }))
    dialog = await screen.findByRole('dialog', { name: 'Change connectors limit' })
    await user.click(within(dialog).getByLabelText(/Use the plan’s value \(10\)/))
    await user.type(within(dialog).getByLabelText('Reason'), 'trial over')
    await user.click(within(dialog).getByRole('button', { name: 'Save limit' }))
    await waitFor(() => expect(writes(calls)).toHaveLength(2))
    expect(writes(calls)[1].body).toEqual({ inherit: true, reason: 'trial over' })
  })

  it('warns when the new limit is below usage (nothing is deleted)', async () => {
    const { user } = setup(tenant({ quotas: quotas({ connectors: { used: 5 } }) }))
    await user.click(await screen.findByRole('button', { name: 'Change Connectors limit' }))
    const dialog = await screen.findByRole('dialog', { name: 'Change connectors limit' })
    await user.click(within(dialog).getByLabelText('Set a limit for this tenant'))
    await user.type(within(dialog).getByLabelText('New limit'), '2')
    expect(within(dialog).getByRole('note')).toHaveTextContent(/Below current usage \(5\).*Nothing is deleted/)
  })

  it('a server refusal stays in the dialog', async () => {
    const { user, calls } = setup(tenant(), { status: 400, body: { error: 'invalid_limit' } })
    await user.click(await screen.findByRole('button', { name: 'Change Connectors limit' }))
    const dialog = await screen.findByRole('dialog', { name: 'Change connectors limit' })
    await user.click(within(dialog).getByLabelText('Set a limit for this tenant'))
    await user.type(within(dialog).getByLabelText('New limit'), '3')
    await user.type(within(dialog).getByLabelText('Reason'), 'x')
    await user.click(within(dialog).getByRole('button', { name: 'Save limit' }))
    expect(await within(dialog).findByText('Enter a whole number of 0 or more.')).toBeInTheDocument()
    expect(detailCalls(calls)).toHaveLength(1)
  })

  it('relay-ops get no Change controls (the section itself; the page is super-admin only)', () => {
    signedInAs(relayOpsMe)
    render(
      <MemoryRouter>
        <TenantQuotasSection tenant={tenant()} onDone={vi.fn()} />
      </MemoryRouter>,
    )
    expect(screen.getByRole('table', { name: 'Quotas' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Change/ })).not.toBeInTheDocument()
  })
})
