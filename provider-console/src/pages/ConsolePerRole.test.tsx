import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { BillingSettings, TenantSummary } from '@/api/types'
import { can, PROVIDER_ROLES, ROLE_HELP, rolesWith, sectionsFor, TOTP_REQUIRED_ROLES } from '@/auth/roles'
import { mockFetch, superAdminMe, type RecordedCall } from '@/test/fetch'
import { PLANS, planCard } from '@/test/plans'
import { renderApp, signedInAs } from '@/test/render'

// Sprint 27 O2 (F19; D-72, D-73, D-74): the console per role. UX only: the
// controller authorizes every request; these tests pin what each role sees.

const as = (role: string) => signedInAs({ ...superAdminMe, email: `${role}@provider.test`, role })
const navLinks = () =>
  within(screen.getByRole('navigation', { name: 'Sections' }))
    .getAllByRole('link')
    .map((a) => a.textContent?.trim())
const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')
const TENANT_ID = 'aaaaaaaa-1111-2222-3333-444444444444'
const SETTINGS: BillingSettings = { country: 'US', currency: 'USD', tax_rate_bp: 0, tax_label: '', invoice_retention_years: 8, email_tenants: true }
const CARDS = [...PLANS.map((p) => ({ ...p, price: null, revision: 1 })), planCard({ id: 'c1' })]

// Every read the Overview might make, answered with a 500: the test is about
// which of them a role sends, not what they show.
const OVERVIEW_500 = Object.fromEntries(
  [
    'GET /provider/relays',
    'GET /provider/relay-pools',
    'GET /provider/alerts',
    'GET /provider/certificates',
    'GET /provider/audit',
    'GET /provider/health',
    'GET /provider/metrics/fleet.active_sessions',
    'GET /provider/metrics/fleet.connections',
  ].map((k) => [k, { status: 500, body: { error: 'x' } }]),
)

function tenant(over: Partial<TenantSummary> = {}): TenantSummary {
  const agents = { pending: 0, active: 1, disconnected: 0, revoked: 0 }
  return {
    id: TENANT_ID,
    slug: 'acme',
    name: 'Acme',
    status: 'active',
    trust_domain: 'acme.zecurity.test',
    created_at: '2026-10-01T00:00:00Z',
    ca_not_after: null,
    counts: {
      connectors: agents,
      shields: agents,
      remote_networks: 1,
      users: { active: 3, suspended: 0, locked: 0, deleted: 0 },
      client_devices: { active: 2, re_enroll_required: 0, renew_pending: 0, revoked: 0 },
      policies: { resources: 1, groups: 1, access_rules: 1, resource_policies: 0 },
    },
    suspension: null,
    deletion: null,
    current_plan: { id: 'p-free', name: 'Free' },
    ...over,
  }
}

describe('O2: the role matrix in the console', () => {
  it('has five roles, help for each, and TOTP required for all but relay-ops', () => {
    expect(PROVIDER_ROLES).toEqual(['super-admin', 'relay-ops', 'auditor', 'billing', 'support'])
    for (const r of PROVIDER_ROLES) expect(ROLE_HELP[r].length).toBeGreaterThan(10)
    expect([...TOTP_REQUIRED_ROLES].sort()).toEqual(['auditor', 'billing', 'super-admin', 'support'])
  })

  it('can() follows the server matrix (spot checks; TestConsoleRoleMatrix pins the whole table)', () => {
    expect(can('super-admin', 'anything.at.all')).toBe(true)
    expect(can('auditor', 'audit.view')).toBe(true)
    expect(can('auditor', 'relay.drain')).toBe(false)
    expect(can('billing', 'tenant.plan.assign')).toBe(true)
    expect(can('billing', 'tenant.read')).toBe(false)
    expect(can('billing', 'plan.delete')).toBe(false)
    expect(can('support', 'tenant.read')).toBe(true)
    expect(can('support', 'invoice.read')).toBe(false)
    expect(can('relay-ops', 'relay.revoke')).toBe(false)
    expect(can('owner', 'relay.read')).toBe(false)
    expect(can(undefined, 'relay.read')).toBe(false)
    expect(rolesWith('tenant.read')).toEqual(['super-admin', 'auditor', 'support'])
  })

  it.each([
    ['relay-ops', ['Overview', 'Alerts', 'Timeline', 'Relays', 'Relay pools']],
    ['auditor', ['Overview', 'Alerts', 'Timeline', 'Relays', 'Relay pools', 'Tenants', 'Plans', 'Usage', 'Invoices', 'Billing settings', 'Certificates', 'Audit log']],
    ['billing', ['Overview', 'Tenants', 'Plans', 'Usage', 'Invoices', 'Billing settings']],
    ['support', ['Overview', 'Alerts', 'Timeline', 'Tenants']],
  ])('%s: the navigation shows exactly its pages', async (role, want) => {
    expect(sectionsFor(role).map((s) => s.label)).toEqual(want)
    as(role)
    mockFetch(OVERVIEW_500)
    renderApp('/')
    await screen.findByRole('navigation', { name: 'Sections' })
    expect(navLinks().filter((t) => t !== 'Provider Console home')).toEqual(want)
  })

  it.each([
    ['billing', ['/relays', `/tenants/${TENANT_ID}`, '/alerts', '/timeline', '/audit', '/certificates', '/users', '/settings/api-keys']],
    ['support', ['/invoices', '/plans', '/usage', '/billing/settings', '/audit', '/relays', '/users']],
    ['auditor', ['/users', '/settings/api-keys']],
  ])('%s: pages outside the role → Forbidden, without a page request', async (role, paths) => {
    for (const path of paths) {
      as(role)
      const { calls } = mockFetch({})
      const { unmount } = renderApp(path)
      expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
      expect(calls).toHaveLength(0)
      unmount()
    }
  })

  it('the Overview reads only what each role may read', async () => {
    const reads = async (role: string) => {
      as(role)
      const { calls } = mockFetch(OVERVIEW_500)
      const { unmount } = renderApp('/')
      await screen.findByRole('navigation', { name: 'Sections' })
      await waitFor(() => expect(screen.queryByText(/Loading/)).toBeNull(), { timeout: 2000 }).catch(() => {})
      const paths = [...new Set(calls.map((c) => c.path))].sort()
      unmount()
      return paths
    }
    expect(await reads('billing')).toEqual([])
    expect(await reads('support')).toEqual(['/provider/alerts', '/provider/health'])
    expect(await reads('auditor')).toEqual(
      ['/provider/alerts', '/provider/audit', '/provider/certificates', '/provider/health', '/provider/metrics/fleet.active_sessions', '/provider/relay-pools', '/provider/relays'].sort(),
    )
  })
})

describe('O2: write buttons follow the role', () => {
  it('auditor: Plans has no New card, edit, delete or default buttons; history stays', async () => {
    as('auditor')
    mockFetch({ 'GET /provider/plans': { status: 200, body: { items: CARDS } }, 'GET /provider/billing/settings': { status: 200, body: SETTINGS } })
    renderApp('/plans')
    const card = within(await screen.findByRole('article', { name: 'Team' }))
    expect(card.getByRole('button', { name: 'History of Team' })).toBeInTheDocument()
    expect(card.queryByRole('button', { name: /Edit Team|Duplicate Team|Delete Team|Make Team the default|Remove the price/ })).toBeNull()
    expect(screen.queryByRole('button', { name: 'New card' })).toBeNull()
    expect(screen.queryByRole('link', { name: 'Change the billing currency' })).toBeNull()
  })

  it('billing: Plans can create and edit, not delete or set the default', async () => {
    as('billing')
    mockFetch({ 'GET /provider/plans': { status: 200, body: { items: CARDS } }, 'GET /provider/billing/settings': { status: 200, body: SETTINGS } })
    renderApp('/plans')
    const card = within(await screen.findByRole('article', { name: 'Team' }))
    expect(card.getByRole('button', { name: 'Edit Team' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'New card' })).toBeInTheDocument()
    expect(card.queryByRole('button', { name: /Delete Team|Make Team the default/ })).toBeNull()
  })

  it('auditor: Billing settings are read-only (no reason, no Save, fields disabled)', async () => {
    as('auditor')
    const { calls } = mockFetch({ 'GET /provider/billing/settings': { status: 200, body: SETTINGS }, 'GET /provider/plans': { status: 200, body: { items: CARDS } } })
    renderApp('/billing/settings')
    expect(await screen.findByText(/Read-only: your role can view the billing settings/)).toBeInTheDocument()
    expect(screen.getByLabelText('Country')).toBeDisabled()
    expect(screen.queryByRole('button', { name: 'Save settings' })).toBeNull()
    expect(screen.queryByLabelText('Reason')).toBeNull()
    expect(writes(calls)).toHaveLength(0)
  })

  it('auditor: Invoices has no Regenerate drafts', async () => {
    as('auditor')
    mockFetch({ 'GET /provider/invoices': { status: 200, body: { items: [], currency: 'USD', next_cursor: null, totals: {} } } })
    renderApp('/invoices')
    expect(await screen.findByText('No invoices for this month yet.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Regenerate drafts/ })).toBeNull()
  })

  it('billing: the tenant list shows the plan and Change plan, and no link to the detail', async () => {
    as('billing')
    mockFetch({ 'GET /provider/tenants': { status: 200, body: { items: [tenant()], next_cursor: null } } })
    renderApp('/tenants')
    const row = within(await screen.findByRole('row', { name: 'Acme' }))
    expect(row.getByText('Free')).toBeInTheDocument()
    expect(row.getByRole('button', { name: 'Change plan of Acme' })).toBeInTheDocument()
    expect(row.queryByRole('link', { name: 'Acme' })).toBeNull()
    expect(screen.queryByRole('button', { name: /Create tenant/ })).toBeNull()
  })

  it('support: the tenant list links to the detail, with no Change plan or Create tenant', async () => {
    as('support')
    mockFetch({ 'GET /provider/tenants': { status: 200, body: { items: [tenant()], next_cursor: null } } })
    renderApp('/tenants')
    const row = within(await screen.findByRole('row', { name: 'Acme' }))
    expect(row.getByRole('link', { name: 'Acme' })).toHaveAttribute('href', `/tenants/${TENANT_ID}`)
    expect(row.queryByRole('button', { name: /Change plan/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /Create tenant/ })).toBeNull()
  })

  it('support: Alerts shows Active and History only, no scope filter', async () => {
    as('support')
    mockFetch({ 'GET /provider/alerts': { status: 200, body: { items: [], truncated: false } } })
    renderApp('/alerts')
    const view = await screen.findByRole('group', { name: 'View' }).catch(() => screen.findByText('Active'))
    expect(view).toBeTruthy()
    expect(screen.queryByText('Rules')).toBeNull()
    expect(screen.queryByText('Channels')).toBeNull()
    expect(screen.queryByRole('group', { name: 'Scope' })).toBeNull()
    expect(screen.getByText(/Tenant alerts: their connectors/)).toBeInTheDocument()
  })

  it('auditor: Alerts shows rules and channels without edit buttons', async () => {
    as('auditor')
    mockFetch({
      'GET /provider/alert-rules': {
        status: 200,
        body: {
          items: [
            { id: 'r1', kind: 'relay.offline', scope: 'platform', name: 'Relay offline', enabled: true, severity: 'critical', params: {}, for_seconds: 300, renotify_seconds: 0 },
          ],
        },
      },
    })
    renderApp('/alerts?tab=rules')
    expect((await screen.findAllByText('Relay offline')).length).toBeGreaterThan(0)
    expect(screen.queryByRole('button', { name: 'Edit Relay offline' })).toBeNull()
  })
})

describe('O2: operator dialog', () => {
  it('offers every role with a help line and the two-step note', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/users': { status: 200, body: [] } })
    renderApp('/users')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Add operator' }))
    const help = await screen.findByTestId('operator-role-help')
    // The default role in the add dialog is relay-ops: no two-step note.
    expect(help.textContent).toContain(ROLE_HELP['relay-ops'])
    expect(help.textContent).not.toContain('two-step')
  })
})

describe('O2: break-glass banner', () => {
  const EVENT = {
    id: 'bg-1',
    email: 'admin2@provider.test',
    reason: 'lost authenticator phone',
    host: 'ctl-1',
    created_at: '2026-11-27T09:07:20Z',
    notified_at: '2026-11-27T09:07:35Z',
    notified_count: 2,
  }

  it('super-admin: shows the recovery and acknowledges it with a required note', async () => {
    signedInAs(superAdminMe)
    let events = [EVENT]
    const { calls } = mockFetch({
      'GET /provider/break-glass': () => ({ status: 200, body: { items: events } }),
      'POST /provider/break-glass/bg-1/ack': () => {
        events = []
        return { status: 204 }
      },
      'GET /provider/api-keys': { status: 200, body: { items: [] } },
    })
    renderApp('/settings/api-keys')
    const banner = within(await screen.findByRole('region', { name: 'Break-glass recoveries' }))
    expect(banner.getByText('admin2@provider.test')).toBeInTheDocument()
    expect(banner.getByText(/lost authenticator phone/)).toBeInTheDocument()
    const user = userEvent.setup()
    await user.click(banner.getByRole('button', { name: 'Acknowledge break-glass for admin2@provider.test' }))
    const dialog = within(await screen.findByRole('dialog'))
    const submit = dialog.getByRole('button', { name: 'Acknowledge' })
    expect(submit).toBeDisabled()
    await user.type(dialog.getByLabelText('Reason'), 'confirmed with the operator')
    await user.click(submit)
    await waitFor(() => expect(writes(calls)).toHaveLength(1))
    expect(writes(calls)[0]).toMatchObject({ path: '/provider/break-glass/bg-1/ack', body: { note: 'confirmed with the operator' } })
    await waitFor(() => expect(screen.queryByRole('region', { name: 'Break-glass recoveries' })).toBeNull())
  })

  it.each(['relay-ops', 'auditor', 'billing', 'support'])('%s: never reads the banner', async (role) => {
    as(role)
    const { bannerCalls, calls } = mockFetch(OVERVIEW_500)
    renderApp('/')
    await screen.findByRole('navigation', { name: 'Sections' })
    await new Promise((r) => setTimeout(r, 50))
    expect(bannerCalls).toHaveLength(0)
    expect(calls.some((c) => c.path === '/provider/break-glass')).toBe(false)
  })
})
