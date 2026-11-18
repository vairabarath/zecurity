import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import type { TenantDetail } from '@/api/types'
import { TenantPlanSection } from '@/components/TenantPlan'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
import { LEGACY_PLAN_ID, PLANS, TENANT_FREE_PLAN } from '@/test/plans'
import { TENANT_DEFAULT_POOL } from '@/test/pools'
import { renderApp, signedInAs } from '@/test/render'

// Tenant detail's Plan section (Sprint 23 B1, D-43): the plan and its six
// limits; Change plan (super-admin, reason) loads the plan list when opened.

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
    plan: TENANT_FREE_PLAN,
    suspension: null,
    ...over,
  }
}

const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')

describe('Tenant detail: Plan', () => {
  it('shows the plan and its six limits', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/tenants/${ID}`]: { status: 200, body: tenant() } })
    renderApp(`/tenants/${ID}`)
    const section = within(await screen.findByRole('region', { name: 'Plan' }))
    expect(section.getByText('Free')).toBeInTheDocument()
    expect(section.getByText('Default')).toBeInTheDocument()
    const limits = within(section.getByLabelText('Plan limits'))
    expect(limits.getByText('Users').nextElementSibling).toHaveTextContent('5')
    expect(limits.getByText('Resources').nextElementSibling).toHaveTextContent('50')
    expect(limits.getByText('Client devices').nextElementSibling).toHaveTextContent('25')
  })

  it('an unlimited plan reads "Unlimited"', async () => {
    signedInAs(superAdminMe)
    mockFetch({
      [`GET /provider/tenants/${ID}`]: { status: 200, body: tenant({ plan: { id: LEGACY_PLAN_ID, name: 'Legacy', limits: PLANS[1].limits, is_default: false } }) },
    })
    renderApp(`/tenants/${ID}`)
    const section = within(await screen.findByRole('region', { name: 'Plan' }))
    expect(within(section.getByLabelText('Plan limits')).getByText('Users').nextElementSibling).toHaveTextContent('Unlimited')
  })

  it('Change plan: pick a plan, give a reason, sends plan_id and reason', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      [`GET /provider/tenants/${ID}`]: { status: 200, body: tenant() },
      'GET /provider/plans': { status: 200, body: { items: PLANS } },
      [`PUT /provider/tenants/${ID}/plan`]: { status: 204 },
    })
    renderApp(`/tenants/${ID}`)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Change plan' }))
    const dialog = await screen.findByRole('dialog', { name: 'Change plan' })
    const save = within(dialog).getByRole('button', { name: 'Change plan' })
    expect(save).toBeDisabled() // same plan, no reason
    await user.click(await within(dialog).findByRole('combobox', { name: 'Plan' }))
    await user.click(await screen.findByRole('option', { name: 'Legacy' }))
    expect(within(dialog).getByText(/Users Unlimited/)).toBeInTheDocument()
    expect(save).toBeDisabled() // reason missing
    await user.type(within(dialog).getByLabelText('Reason'), ' internal tenant ')
    await user.click(save)
    expect(await screen.findByText(/Acme is now on the Legacy plan/)).toBeInTheDocument()
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'PUT', path: `/provider/tenants/${ID}/plan`, body: { plan_id: LEGACY_PLAN_ID, reason: 'internal tenant' } }),
    ])
  })

  it('relay-ops get no Change plan control (the section itself; the page is super-admin only)', () => {
    signedInAs(relayOpsMe)
    render(
      <MemoryRouter>
        <TenantPlanSection tenant={tenant()} onDone={vi.fn()} />
      </MemoryRouter>,
    )
    expect(screen.queryByRole('button', { name: 'Change plan' })).not.toBeInTheDocument()
  })
})
