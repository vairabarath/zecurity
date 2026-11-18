import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import type { TenantDetail, TenantSummary } from '@/api/types'
import { TenantSuspensionSection } from '@/components/TenantSuspension'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { FREE_QUOTAS, TENANT_FREE_PLAN } from '@/test/plans'
import { TENANT_DEFAULT_POOL } from '@/test/pools'
import { renderApp, signedInAs } from '@/test/render'

// Tenant detail's Suspension section (Sprint 23 S1, D-45, D-17 extended):
// super-admin only; Suspend needs a reason and the slug typed back, Resume a
// reason; each sends exactly one request (reason in the body, never the URL)
// and reloads the page once.

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
    first_admin_invitation: null,
    ...over,
  }
}

const detailCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path === `/provider/tenants/${ID}`)
const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')

function setup(body: TenantDetail, path: 'suspend' | 'resume', res: FakeResponse = { status: 204 }) {
  signedInAs(superAdminMe)
  const { calls } = mockFetch({
    [`GET /provider/tenants/${ID}`]: { status: 200, body },
    [`POST /provider/tenants/${ID}/${path}`]: res,
  })
  renderApp(`/tenants/${ID}`)
  return { user: userEvent.setup(), calls }
}

describe('Tenant detail: suspension', () => {
  it('an active tenant shows "Not suspended" and no badge', async () => {
    setup(tenant(), 'suspend')
    const section = within(await screen.findByRole('region', { name: 'Suspension' }))
    expect(section.getByText('Not suspended.')).toBeInTheDocument()
    expect(screen.queryByText('Suspended')).not.toBeInTheDocument()
  })

  it('a suspended tenant shows the badge, since, by and reason', async () => {
    setup(tenant({ suspension: SUSPENSION }), 'resume')
    const section = within(await screen.findByRole('region', { name: /^Suspension/ }))
    const details = within(section.getByText('ops@provider.test').closest('dl')!)
    expect(details.getByText('ops@provider.test')).toBeInTheDocument()
    expect(details.getByText('non-payment')).toBeInTheDocument()
    expect(details.getByText(new Date(SUSPENSION.suspended_at).toLocaleString())).toBeInTheDocument()
    expect(screen.getAllByText('Suspended').length).toBeGreaterThanOrEqual(2) // header and section
    expect(section.getByRole('button', { name: 'Resume tenant' })).toBeInTheDocument()
    expect(section.queryByRole('button', { name: 'Suspend tenant' })).not.toBeInTheDocument()
  })

  it('suspend needs a reason and the exact slug; sends one request with both in the body; reloads once', async () => {
    const { user, calls } = setup(tenant(), 'suspend')
    await user.click(await screen.findByRole('button', { name: 'Suspend tenant' }))
    const dialog = await screen.findByRole('dialog', { name: 'Suspend tenant' })
    const confirm = within(dialog).getByRole('button', { name: 'Suspend tenant' })
    expect(confirm).toBeDisabled()
    await user.type(within(dialog).getByLabelText('Reason'), '  non-payment  ')
    expect(confirm).toBeDisabled()
    await user.type(within(dialog).getByLabelText(/to confirm/), 'Acme') // case matters
    expect(confirm).toBeDisabled()
    await user.clear(within(dialog).getByLabelText(/to confirm/))
    await user.type(within(dialog).getByLabelText(/to confirm/), 'acme')
    expect(detailCalls(calls)).toHaveLength(1)
    await user.click(confirm)
    await waitFor(() => expect(detailCalls(calls)).toHaveLength(2))
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'POST', path: `/provider/tenants/${ID}/suspend`, search: '', body: { reason: 'non-payment', confirm_slug: 'acme' } }),
    ])
    expect(await screen.findByText(/Acme is suspended/)).toBeInTheDocument()
  })

  it('resume needs a reason; sends one request; reloads once', async () => {
    const { user, calls } = setup(tenant({ suspension: SUSPENSION }), 'resume')
    await user.click(await screen.findByRole('button', { name: 'Resume tenant' }))
    const dialog = await screen.findByRole('dialog', { name: 'Resume tenant' })
    const confirm = within(dialog).getByRole('button', { name: 'Resume tenant' })
    expect(confirm).toBeDisabled()
    await user.type(within(dialog).getByLabelText('Reason'), 'paid')
    await user.click(confirm)
    await waitFor(() => expect(detailCalls(calls)).toHaveLength(2))
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'POST', path: `/provider/tenants/${ID}/resume`, search: '', body: { reason: 'paid' } }),
    ])
    expect(await screen.findByText(/Acme is resumed/)).toBeInTheDocument()
  })

  it('a slug mismatch from the server stays in the dialog on the confirm field', async () => {
    const { user, calls } = setup(tenant(), 'suspend', { status: 400, body: { error: 'confirmation_mismatch' } })
    await user.click(await screen.findByRole('button', { name: 'Suspend tenant' }))
    const dialog = await screen.findByRole('dialog', { name: 'Suspend tenant' })
    await user.type(within(dialog).getByLabelText('Reason'), 'x')
    await user.type(within(dialog).getByLabelText(/to confirm/), 'acme')
    await user.click(within(dialog).getByRole('button', { name: 'Suspend tenant' }))
    expect(await within(dialog).findByText(/doesn’t match the tenant’s slug/)).toBeInTheDocument()
    expect(detailCalls(calls)).toHaveLength(1)
  })

  it('already suspended by someone else: closes, reloads and says so', async () => {
    const { user, calls } = setup(tenant(), 'suspend', { status: 409, body: { error: 'already_suspended' } })
    await user.click(await screen.findByRole('button', { name: 'Suspend tenant' }))
    const dialog = await screen.findByRole('dialog', { name: 'Suspend tenant' })
    await user.type(within(dialog).getByLabelText('Reason'), 'x')
    await user.type(within(dialog).getByLabelText(/to confirm/), 'acme')
    await user.click(within(dialog).getByRole('button', { name: 'Suspend tenant' }))
    await waitFor(() => expect(detailCalls(calls)).toHaveLength(2))
    expect(await screen.findByText(/was already suspended, possibly by another operator/)).toBeInTheDocument()
  })

  it('relay-ops get no Suspend or Resume control (the section itself; the page is super-admin only)', () => {
    signedInAs(relayOpsMe)
    for (const t of [tenant(), tenant({ suspension: SUSPENSION })]) {
      const { unmount } = render(
        <MemoryRouter>
          <TenantSuspensionSection tenant={t} onDone={vi.fn()} />
        </MemoryRouter>,
      )
      expect(screen.queryByRole('button', { name: /Suspend tenant|Resume tenant/ })).not.toBeInTheDocument()
      unmount()
    }
  })
})

describe('Tenants list: suspended badge', () => {
  it('shows "Suspended" next to the status of a suspended tenant only', async () => {
    signedInAs(superAdminMe)
    const summary = (over: Partial<TenantSummary>): TenantSummary => {
      const { id, slug, name, status, trust_domain, created_at, ca_not_after, counts, suspension } = tenant()
      return { id, slug, name, status, trust_domain, created_at, ca_not_after, counts, suspension, ...over }
    }
    mockFetch({
      'GET /provider/tenants': {
        status: 200,
        body: {
          items: [summary({}), summary({ id: 'bbbbbbbb-1111-2222-3333-444444444444', slug: 'beta', name: 'Beta', suspension: SUSPENSION })],
          next_cursor: null,
        },
      },
    })
    renderApp('/tenants')
    const beta = await screen.findByRole('row', { name: /Beta/ })
    expect(within(beta).getByText('Suspended')).toBeInTheDocument()
    expect(within(screen.getByRole('row', { name: /Acme/ })).queryByText('Suspended')).not.toBeInTheDocument()
  })
})
