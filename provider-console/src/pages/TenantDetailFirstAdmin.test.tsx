import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import type { TenantDetail } from '@/api/types'
import { TenantFirstAdminSection } from '@/components/TenantFirstAdmin'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
import { FREE_QUOTAS, TENANT_FREE_PLAN } from '@/test/plans'
import { TENANT_DEFAULT_POOL } from '@/test/pools'
import { renderApp, signedInAs } from '@/test/render'

// Tenant detail's First admin section (Sprint 23 S2, D-44): shown only while
// the provider-issued first-admin seat is unclaimed; Send new invitation
// (super-admin, reason, optional new email) re-issues it.

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
    quotas: FREE_QUOTAS,
    first_admin_invitation: null,
    suspension: null,
    deletion: null,
    ...over,
  }
}

const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')
const EXPIRED = { status: 'expired' as const, expires_at: '2026-11-01T00:00:00Z' }

describe('Tenant detail: First admin', () => {
  it('is hidden once the tenant has an admin (no invitation in the read)', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/tenants/${ID}`]: { status: 200, body: tenant() } })
    renderApp(`/tenants/${ID}`)
    await screen.findByRole('region', { name: 'Plan' })
    expect(screen.queryByRole('region', { name: 'First admin' })).not.toBeInTheDocument()
  })

  it('shows a pending invitation and an expired one', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/tenants/${ID}`]: { status: 200, body: tenant({ first_admin_invitation: { status: 'pending', expires_at: '2026-12-01T00:00:00Z' } }) } })
    const view = renderApp(`/tenants/${ID}`)
    expect(within(await screen.findByRole('region', { name: 'First admin' })).getByText('Invitation pending')).toBeInTheDocument()
    view.unmount()
    mockFetch({ [`GET /provider/tenants/${ID}`]: { status: 200, body: tenant({ first_admin_invitation: EXPIRED }) } })
    renderApp(`/tenants/${ID}`)
    const section = within(await screen.findByRole('region', { name: 'First admin' }))
    expect(section.getByText('Invitation expired')).toBeInTheDocument()
    expect(section.getByText(/sign-in is refused until you send a new invitation/)).toBeInTheDocument()
  })

  it('Send new invitation: same address by default, reason in the body; reloads with a notice', async () => {
    signedInAs(superAdminMe)
    let n = 0
    const { calls } = mockFetch({
      [`GET /provider/tenants/${ID}`]: () => ({
        status: 200,
        body: tenant({ first_admin_invitation: n++ === 0 ? EXPIRED : { status: 'pending', expires_at: '2026-12-01T00:00:00Z' } }),
      }),
      [`POST /provider/tenants/${ID}/first-admin-invitation`]: {
        status: 201,
        body: { tenant_id: ID, invitation: { id: 'i2', email: 'first@acme.test', expires_at: '2026-12-01T00:00:00Z' } },
      },
    })
    renderApp(`/tenants/${ID}`)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Send new invitation' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'Send new invitation' }))
    const send = dialog.getByRole('button', { name: 'Send invitation' })
    expect(send).toBeDisabled()
    await user.type(dialog.getByLabelText('New email (optional)'), 'not-an-email')
    await user.type(dialog.getByLabelText('Reason'), ' link expired ')
    expect(send).toBeDisabled()
    await user.clear(dialog.getByLabelText('New email (optional)'))
    await user.click(send)
    expect(await screen.findByText(/A new invitation was sent to first@acme.test/)).toBeInTheDocument()
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'POST', path: `/provider/tenants/${ID}/first-admin-invitation`, search: '', body: { reason: 'link expired' } }),
    ])
    expect(within(await screen.findByRole('region', { name: 'First admin' })).getByText('Invitation pending')).toBeInTheDocument()
  })

  it('a new email is sent; first_admin_exists is explained and the dialog stays open', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      [`GET /provider/tenants/${ID}`]: { status: 200, body: tenant({ first_admin_invitation: EXPIRED }) },
      [`POST /provider/tenants/${ID}/first-admin-invitation`]: { status: 409, body: { error: 'first_admin_exists' } },
    })
    renderApp(`/tenants/${ID}`)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Send new invitation' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'Send new invitation' }))
    await user.type(dialog.getByLabelText('New email (optional)'), 'right@acme.test')
    await user.type(dialog.getByLabelText('Reason'), 'typo')
    await user.click(dialog.getByRole('button', { name: 'Send invitation' }))
    expect(await dialog.findByRole('alert')).toHaveTextContent('already has an admin')
    expect(writes(calls)[0].body).toEqual({ admin_email: 'right@acme.test', reason: 'typo' })
  })

  it('relay-ops get no Send new invitation control', () => {
    signedInAs(relayOpsMe)
    render(
      <MemoryRouter>
        <TenantFirstAdminSection tenant={tenant({ first_admin_invitation: EXPIRED })} onDone={vi.fn()} />
      </MemoryRouter>,
    )
    expect(screen.queryByRole('button', { name: 'Send new invitation' })).not.toBeInTheDocument()
  })
})
