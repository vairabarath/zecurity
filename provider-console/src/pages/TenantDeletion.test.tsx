import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import type { TenantDetail } from '@/api/types'
import { TenantDeletionSection } from '@/components/TenantDeletion'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
import { FREE_QUOTAS, TENANT_FREE_PLAN } from '@/test/plans'
import { TENANT_DEFAULT_POOL } from '@/test/pools'
import { renderApp, signedInAs } from '@/test/render'

// Tenant detail's Deletion section (Sprint 23 S4, D-46, D-17 extended):
// super-admin only; Delete only for a suspended tenant, with a reason and the
// slug typed back; cancel and the legal hold need a reason; status only.

const ID = 'aaaaaaaa-1111-2222-3333-444444444444'
const SUSPENSION = { suspended_at: '2026-11-17T10:00:00Z', by: 'ops@provider.test', reason: 'non-payment' }

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
    quotas: FREE_QUOTAS,
    suspension: null,
    deletion: null,
    first_admin_invitation: null,
    ...over,
  }
}

const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')
const SCHEDULED = {
  scheduled_at: '2026-11-18T10:00:00Z',
  delete_after: '2026-12-18T10:00:00Z',
  by: 'admin@provider.test',
  reason: 'contract ended',
  legal_hold: false,
  legal_hold_reason: null,
  purge_step: null,
}

function setup(body: TenantDetail, routes: Record<string, { status: number; body?: unknown }> = {}) {
  signedInAs(superAdminMe)
  const { calls } = mockFetch({ [`GET /provider/tenants/${ID}`]: { status: 200, body }, ...routes })
  renderApp(`/tenants/${ID}`)
  return { user: userEvent.setup(), calls }
}

describe('Tenant detail: deletion', () => {
  it('an active tenant can\u2019t be deleted: the button is disabled with the reason', async () => {
    setup(tenant())
    const section = within(await screen.findByRole('region', { name: /^Deletion/ }))
    expect(section.getByText(/Only a suspended tenant can be deleted/)).toBeInTheDocument()
    expect(section.getByRole('button', { name: 'Delete tenant' })).toBeDisabled()
  })

  it('a suspended tenant: Delete needs a reason and the slug; one POST with both in the body', async () => {
    const { user, calls } = setup(tenant({ suspension: SUSPENSION }), {
      [`POST /provider/tenants/${ID}/delete`]: { status: 200, body: { delete_after: '2026-12-18T10:00:00Z' } },
    })
    const section = within(await screen.findByRole('region', { name: /^Deletion/ }))
    await user.click(section.getByRole('button', { name: 'Delete tenant' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'Delete tenant' }))
    const confirm = dialog.getByRole('button', { name: 'Schedule deletion' })
    await user.type(dialog.getByLabelText('Reason'), ' contract ended ')
    expect(confirm).toBeDisabled()
    await user.type(dialog.getByLabelText(/to confirm/), 'acme')
    await user.click(confirm)
    expect(await screen.findByText(/will be deleted on/)).toBeInTheDocument()
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'POST', path: `/provider/tenants/${ID}/delete`, search: '', body: { reason: 'contract ended', confirm_slug: 'acme' } }),
    ])
  })

  it('a scheduled deletion shows the date, cancel and legal hold; cancel sends the reason', async () => {
    const { user, calls } = setup(tenant({ suspension: SUSPENSION, deletion: SCHEDULED }), {
      [`POST /provider/tenants/${ID}/delete/cancel`]: { status: 204 },
    })
    const section = within(await screen.findByRole('region', { name: /^Deletion/ }))
    expect(section.getByText('Scheduled')).toBeInTheDocument()
    expect(section.getByText('contract ended')).toBeInTheDocument()
    expect(section.queryByRole('button', { name: 'Delete tenant' })).not.toBeInTheDocument()
    await user.click(section.getByRole('button', { name: 'Cancel deletion' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'Cancel deletion' }))
    await user.type(dialog.getByLabelText('Reason'), 'renewed')
    await user.click(dialog.getByRole('button', { name: 'Cancel deletion' }))
    expect(await screen.findByText(/deletion is cancelled/)).toBeInTheDocument()
    expect(writes(calls)[0]).toEqual(expect.objectContaining({ path: `/provider/tenants/${ID}/delete/cancel`, body: { reason: 'renewed' } }))
  })

  it('a started purge shows its step and offers no cancel; the legal hold is a PUT with on=true', async () => {
    const { user, calls } = setup(tenant({ suspension: SUSPENSION, deletion: { ...SCHEDULED, purge_step: 'exported' } }), {
      [`PUT /provider/tenants/${ID}/legal-hold`]: { status: 204 },
    })
    const section = within(await screen.findByRole('region', { name: /^Deletion/ }))
    expect(section.getByText(/Logs exported and verified/)).toBeInTheDocument()
    expect(section.queryByRole('button', { name: 'Cancel deletion' })).not.toBeInTheDocument()
    await user.click(section.getByRole('button', { name: 'Place legal hold' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'Place legal hold' }))
    await user.type(dialog.getByLabelText('Reason'), 'litigation')
    await user.click(dialog.getByRole('button', { name: 'Place legal hold' }))
    expect(await screen.findByText(/is on legal hold/)).toBeInTheDocument()
    expect(writes(calls)[0]).toEqual(expect.objectContaining({ method: 'PUT', path: `/provider/tenants/${ID}/legal-hold`, body: { on: true, reason: 'litigation' } }))
  })

  it('relay-ops get no deletion controls', () => {
    signedInAs(relayOpsMe)
    render(
      <MemoryRouter>
        <TenantDeletionSection tenant={tenant({ suspension: SUSPENSION, deletion: SCHEDULED })} onDone={vi.fn()} />
      </MemoryRouter>,
    )
    expect(screen.queryAllByRole('button')).toHaveLength(0)
  })
})
