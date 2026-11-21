import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { TimelineItem } from '@/api/alerts'
import { mockFetch, relayOpsMe, superAdminMe } from '@/test/fetch'
import { renderApp, signedInAs } from '@/test/render'

// Incident timeline (Sprint 24 T1, F27): read-only; alert events and audit
// rows in order; focus by relay, pool or tenant through the URL; relay-ops
// get relay and pool rows only (D-55), no tenant links and no tenant focus.

const RELAY = '6b1f5f2e-0000-4000-8000-0000000000a1'
const POOL = '6b1f5f2e-0000-4000-8000-0000000000b1'
const TENANT = '6b1f5f2e-0000-4000-8000-0000000000c1'

const ITEMS: TimelineItem[] = [
  {
    id: 'au2', source: 'audit', at: '2026-11-20T10:06:00Z', target_type: 'tenant', target_id: TENANT, target_name: 'acme',
    tenant_id: TENANT, action: 'tenant.suspend', actor: 'admin@provider.test', reason: 'unpaid',
  },
  {
    id: 'ev3', source: 'alert', at: '2026-11-20T10:04:00Z', target_type: 'connector', target_id: 'c-1', target_name: 'acme-conn',
    tenant_id: TENANT, kind: 'connector.offline', transition: 'fired', severity: 'warning', summary: 'Connector acme-conn is offline',
  },
  {
    id: 'au1', source: 'audit', at: '2026-11-20T10:02:00Z', target_type: 'relay_pool', target_id: POOL, target_name: 'pool-east',
    tenant_id: null, action: 'relay_pool.failover', actor: 'system:relay-pool-failover',
  },
  {
    id: 'ev1', source: 'alert', at: '2026-11-20T10:01:00Z', target_type: 'relay', target_id: RELAY, target_name: 'relay-1',
    tenant_id: null, kind: 'relay.offline', transition: 'fired', severity: 'critical', summary: 'Relay relay-1 is offline',
  },
  {
    id: 'au0', source: 'audit', at: '2026-11-20T10:00:00Z', target_type: 'relay', target_id: RELAY, target_name: 'relay-1',
    tenant_id: null, action: 'relay.drain', actor: 'admin@provider.test', reason: 'kernel upgrade',
  },
]

const page = (items: TimelineItem[], next: string | null = null) => ({ status: 200, body: { items, next_cursor: next } })

describe('Timeline', () => {
  it('super-admin: alerts and operator actions in order, with reasons and links; read-only', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/timeline': page(ITEMS) })
    renderApp('/timeline')
    const rows = await screen.findAllByRole('row', { name: /./ })
    expect(rows.slice(1).map((r) => r.getAttribute('aria-label'))).toEqual([
      'Tenant suspended', 'Connector acme-conn is offline', 'Pool failed over', 'Relay relay-1 is offline', 'Relay drained',
    ])
    const drain = within(screen.getByRole('row', { name: 'Relay drained' }))
    expect(drain.getByText('Operator')).toBeInTheDocument()
    expect(drain.getByText(/Reason: kernel upgrade/)).toBeInTheDocument()
    expect(drain.getByRole('link', { name: 'relay-1' })).toHaveAttribute('href', `/relays/${RELAY}`)
    const failover = within(screen.getByRole('row', { name: 'Pool failed over' }))
    expect(failover.getByText('System')).toBeInTheDocument()
    expect(failover.getByRole('link', { name: 'pool-east' })).toHaveAttribute('href', `/relay-pools/${POOL}`)
    const conn = within(screen.getByRole('row', { name: 'Connector acme-conn is offline' }))
    expect(conn.getByRole('link', { name: 'tenant' })).toHaveAttribute('href', `/tenants/${TENANT}`)
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
    expect(calls.map((c) => c.search)).toEqual(['limit=50'])
  })

  it('focus on a relay from a row, then clear it; the source filter is sent', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/timeline': page(ITEMS) })
    renderApp('/timeline')
    const user = userEvent.setup()
    const drain = within(await screen.findByRole('row', { name: 'Relay drained' }))
    await user.click(drain.getByRole('button', { name: 'Show only relay relay-1' }))
    await waitFor(() => expect(calls.at(-1)?.search).toBe(`relay_id=${RELAY}&limit=50`))
    expect(screen.getByRole('status')).toHaveTextContent('Relay:relay-1')
    await user.click(screen.getByRole('button', { name: 'Clear focus' }))
    await waitFor(() => expect(calls.at(-1)?.search).toBe('limit=50'))
    await user.click(within(screen.getByRole('group', { name: 'Show' })).getByRole('button', { name: 'Operator actions' }))
    await waitFor(() => expect(calls.at(-1)?.search).toBe('source=audit&limit=50'))
  })

  it('focus from the URL (pool, tenant); an invalid id is ignored', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/timeline': page([]) })
    renderApp(`/timeline?pool_id=${POOL}&focus_name=pool-east`)
    expect(await screen.findByText('Nothing recorded for this filter.')).toBeInTheDocument()
    expect(calls[0].search).toBe(`pool_id=${POOL}&limit=50`)
  })

  it('ignores a malformed focus id', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/timeline': page([]) })
    renderApp('/timeline?relay_id=relay-1')
    expect(await screen.findByText('Nothing recorded yet.')).toBeInTheDocument()
    expect(calls[0].search).toBe('limit=50')
  })

  it('pages by cursor', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/timeline': (c) => (c.search.includes('cursor=c2') ? page([ITEMS[4]]) : page(ITEMS.slice(0, 4), 'c2')),
    })
    renderApp('/timeline')
    const user = userEvent.setup()
    await screen.findByRole('row', { name: 'Pool failed over' })
    await user.click(screen.getByRole('button', { name: /Next/ }))
    expect(await screen.findByRole('row', { name: 'Relay drained' })).toBeInTheDocument()
    expect(calls.at(-1)?.search).toBe('cursor=c2&limit=50')
  })

  it('relay-ops (D-55): relay and pool operator actions with actor and reason; no tenant focus or links, even by URL', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({ 'GET /provider/timeline': page([ITEMS[3], ITEMS[4]]) })
    renderApp(`/timeline?tenant_id=${TENANT}&source=audit`)
    const drain = within(await screen.findByRole('row', { name: 'Relay drained' }))
    expect(drain.getByText(/admin@provider.test · Reason: kernel upgrade/)).toBeInTheDocument()
    expect(drain.getByRole('button', { name: 'Show only relay relay-1' })).toBeInTheDocument()
    expect(within(screen.getByRole('group', { name: 'Show' })).getByRole('button', { name: 'Operator actions' })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'tenant' })).not.toBeInTheDocument()
    // The tenant focus is dropped; the source filter is sent (the controller limits it).
    expect(calls.map((c) => c.search)).toEqual(['source=audit&limit=50'])
  })

  it('the Timeline section is in the nav for both roles', async () => {
    for (const me of [superAdminMe, relayOpsMe]) {
      signedInAs(me)
      mockFetch({ 'GET /provider/timeline': page([]) })
      const { unmount } = renderApp('/timeline')
      expect(await screen.findByRole('link', { name: /Timeline/ })).toHaveAttribute('href', '/timeline')
      unmount()
    }
  })
})
