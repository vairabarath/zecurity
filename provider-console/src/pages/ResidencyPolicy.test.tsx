import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { RelayDetail, RelayPoolDetail, RelaySummary } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe } from '@/test/fetch'
import { DEFAULT_POOL, EU_POOL, EU_POOL_ID, POOLS, RELAY_POLICY } from '@/test/pools'
import { renderApp, signedInAs } from '@/test/render'

// D-40: residency is shown as enforced only when the pool read says so, and
// each relay in a residency pool shows its own workspace-policy state.

function relay(over: Partial<RelaySummary> = {}): RelaySummary {
  return {
    id: '11111111-1111-1111-1111-111111111111',
    name: 'relay-eu-a',
    status: 'active',
    version: '0.23.0',
    hostname: null,
    public_addr: null,
    address_scope: null,
    capacity_label: 'high',
    connection_count: 0,
    max_connections: 100,
    cert_serial: null,
    cert_not_after: null,
    last_heartbeat_at: null,
    attached_connectors: 0,
    created_at: '2026-10-01T00:00:00Z',
    draining_since: null,
    drain_state: null,
    bandwidth: null,
    pool: EU_POOL,
    policy: RELAY_POLICY,
    ...over,
  }
}

const ENFORCING = relay()
const APPLYING = relay({ id: '22222222-2222-2222-2222-222222222222', name: 'relay-eu-b', policy: { supported: true, applied_version: 2, current: false } })
const OLD = relay({ id: '33333333-3333-3333-3333-333333333333', name: 'relay-eu-c', policy: { supported: false, applied_version: null, current: false } })
const PLAIN = relay({ id: '44444444-4444-4444-4444-444444444444', name: 'relay-plain', pool: DEFAULT_POOL })
const fleetOk = { status: 200, body: { items: [ENFORCING, APPLYING, OLD, PLAIN], next_cursor: null } }

describe('Residency badge (pool reads)', () => {
  it('enforced, not enforced and non-residency pools read differently', async () => {
    signedInAs(relayOpsMe)
    const notEnforced = { ...POOLS[1], id: 'eeeeeeee-0000-0000-0000-0000000000e9', name: 'eu-slow', residency_enforced: false }
    mockFetch({ 'GET /provider/relay-pools': { status: 200, body: { items: [POOLS[0], POOLS[1], notEnforced] } } })
    renderApp('/relay-pools')

    const enforced = within(await screen.findByRole('row', { name: 'eu-1' })).getByText(/Residency \(enforced\)/)
    expect(enforced).toHaveAttribute('data-residency', 'enforced')
    expect(enforced).toHaveAttribute('title', expect.stringMatching(/sessions end at the relay’s next heartbeat \(30 seconds at most by default\)/))

    const slow = within(screen.getByRole('row', { name: 'eu-slow' })).getByText(/Residency \(not enforced\)/)
    expect(slow).toHaveAttribute('data-residency', 'not-enforced')
    expect(slow).toHaveAttribute('title', expect.stringMatching(/doesn’t support the workspace policy or hasn’t applied the current version/))
    expect(slow).not.toHaveTextContent(/\(enforced\)/)

    const def = within(screen.getByRole('row', { name: 'default' }))
    expect(def.queryByText(/Residency/)).not.toBeInTheDocument()
  })

  it('residency_enforced is never shown for a non-residency pool, whatever the flag says', async () => {
    signedInAs(relayOpsMe)
    mockFetch({ 'GET /provider/relay-pools': { status: 200, body: { items: [{ ...POOLS[0], residency_enforced: true }] } } })
    renderApp('/relay-pools')
    const def = within(await screen.findByRole('row', { name: 'default' }))
    expect(def.queryByText(/enforced/)).not.toBeInTheDocument()
  })
})

describe('Relay policy indicator', () => {
  it('the Relays list shows Enforcing / Applying… / Not supported for residency pools and — elsewhere', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/relays': fleetOk })
    renderApp('/relays')
    const cell = (name: string) => within(screen.getByRole('row', { name }))
    await screen.findByRole('row', { name: 'relay-eu-a' })
    expect(cell('relay-eu-a').getByText('Enforcing')).toHaveAttribute('data-policy', 'enforcing')
    const applying = cell('relay-eu-b').getByText('Applying…')
    expect(applying).toHaveAttribute('title', expect.stringMatching(/next heartbeat/))
    expect(cell('relay-eu-c').getByText('Not supported')).toHaveAttribute('title', expect.stringMatching(/older relay build/))
    expect(cell('relay-plain').queryByText(/Enforcing|Applying|Not supported/)).not.toBeInTheDocument()
    const headers = within(screen.getByRole('table', { name: 'Relays' })).getAllByRole('columnheader').map((h) => h.textContent)
    expect(headers).toContain('Policy')
  })

  function detail(r: RelaySummary): RelayDetail {
    return {
      ...r,
      dns_allowlist: [],
      ip_allowlist: [],
      certificates: [],
      certificates_truncated: false,
      attachments: [],
      attachments_truncated: false,
      in_relay_list: true,
      eligible_relay_count: 2,
    }
  }

  it('relay detail shows the policy state and applied version for a residency pool', async () => {
    signedInAs(relayOpsMe)
    mockFetch({ [`GET /provider/relays/${APPLYING.id}`]: { status: 200, body: detail(APPLYING) } })
    renderApp(`/relays/${APPLYING.id}`)
    const card = within(await screen.findByRole('region', { name: 'Relay pool' }))
    const fact = card.getByText('Workspace policy').nextElementSibling as HTMLElement
    expect(fact).toHaveTextContent('Applying…')
    expect(fact).toHaveTextContent('applied version 2')
    // The relay read has no enforcement flag for the pool: no claim either way.
    expect(card.queryByText(/\(enforced\)|\(not enforced\)/)).not.toBeInTheDocument()
  })

  it('relay detail shows no policy row for a non-residency pool', async () => {
    signedInAs(relayOpsMe)
    mockFetch({ [`GET /provider/relays/${PLAIN.id}`]: { status: 200, body: detail(PLAIN) } })
    renderApp(`/relays/${PLAIN.id}`)
    const card = within(await screen.findByRole('region', { name: 'Relay pool' }))
    expect(card.queryByText('Workspace policy')).not.toBeInTheDocument()
  })
})

describe('Pool detail: policy per relay', () => {
  const member = (r: RelaySummary) => ({ id: r.id, name: r.name, status: r.status, eligible: true, policy: r.policy })
  function poolDetail(over: Partial<RelayPoolDetail> = {}): RelayPoolDetail {
    return {
      ...POOLS[1],
      residency_enforced: false,
      relay_list: [ENFORCING, APPLYING, OLD].map(member),
      relay_list_truncated: false,
      tenant_list: null,
      tenant_list_truncated: false,
      ...over,
    }
  }

  it('not enforced: names the relays that block enforcement and shows each relay’s policy, from the pool read only', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({ [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 200, body: poolDetail() } })
    renderApp(`/relay-pools/${EU_POOL_ID}`)
    await screen.findByRole('heading', { name: 'eu-1', level: 1 })
    expect(screen.getAllByText(/Residency \(not enforced\)/).length).toBeGreaterThan(0)
    const note = screen.getByText(/Blocking enforcement:/)
    expect(note).toHaveTextContent('relay-eu-b (applying the current policy), relay-eu-c (relay build doesn’t support the policy)')
    expect(note).not.toHaveTextContent('relay-eu-a')
    const table = within(screen.getByRole('table', { name: 'Pool relays' }))
    expect(within(table.getByRole('row', { name: 'relay-eu-a' })).getByText('Enforcing')).toBeInTheDocument()
    expect(within(table.getByRole('row', { name: 'relay-eu-b' })).getByText('Applying…')).toBeInTheDocument()
    expect(within(table.getByRole('row', { name: 'relay-eu-c' })).getByText('Not supported')).toBeInTheDocument()
    expect(screen.getByText('Policy version').nextElementSibling).toHaveTextContent('3')
    expect(calls.map((c) => c.path)).toEqual([`/provider/relay-pools/${EU_POOL_ID}`])
  })

  it('not enforced with no active relay says so', async () => {
    signedInAs(relayOpsMe)
    mockFetch({ [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 200, body: poolDetail({ relay_list: [member({ ...OLD, status: 'inactive' })] }) } })
    renderApp(`/relay-pools/${EU_POOL_ID}`)
    expect(await screen.findByText(/Residency is not enforced/)).toHaveTextContent('The pool has no active relay.')
  })

  it('enforced: no warning', async () => {
    signedInAs(relayOpsMe)
    mockFetch({ [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 200, body: poolDetail({ residency_enforced: true, relay_list: [member(ENFORCING)] }) } })
    renderApp(`/relay-pools/${EU_POOL_ID}`)
    await screen.findByText('Enforcing')
    expect(screen.queryByText(/Residency is not enforced/)).not.toBeInTheDocument()
    expect(screen.getAllByText(/Residency \(enforced\)/).length).toBeGreaterThan(0)
  })

  it('a non-residency pool shows no policy column', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({
      [`GET /provider/relay-pools/${POOLS[0].id}`]: { status: 200, body: { ...poolDetail(), ...POOLS[0], relay_list: [member(PLAIN)] } },
    })
    renderApp(`/relay-pools/${POOLS[0].id}`)
    await screen.findByRole('heading', { name: 'default', level: 1 })
    expect(within(screen.getByRole('table', { name: 'Pool relays' })).queryByText('Policy')).not.toBeInTheDocument()
    expect(calls).toHaveLength(1)
  })
})
