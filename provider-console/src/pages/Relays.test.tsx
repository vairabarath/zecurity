import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { Page, RelaySummary } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
import { renderApp, signedInAs } from '@/test/render'

const iso = (ms: number) => new Date(Date.now() + ms).toISOString()
const MIN = 60_000
const DAY = 24 * 60 * MIN

function relay(over: Partial<RelaySummary>): RelaySummary {
  return {
    id: '11111111-1111-1111-1111-111111111111',
    name: 'relay-a',
    status: 'active',
    version: '0.21.0',
    hostname: 'relay-a.local',
    public_addr: '203.0.113.5:9093',
    address_scope: 'public',
    capacity_label: 'high',
    connection_count: 12,
    max_connections: 500,
    cert_serial: 'abc',
    cert_not_after: iso(3 * DAY),
    last_heartbeat_at: iso(-2 * MIN),
    attached_connectors: 4,
    created_at: '2026-10-01T00:00:00Z',
    ...over,
  }
}

const A = relay({})
const B = relay({ id: '22222222-2222-2222-2222-222222222222', name: 'relay-b', status: 'pending', version: null, hostname: null, public_addr: null, cert_not_after: null, last_heartbeat_at: null, attached_connectors: 0 })

const relayCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path === '/provider/relays')
const qs = (c: RecordedCall) => Object.fromEntries(new URLSearchParams(c.search))

describe('Relays list', () => {
  it('renders every column; missing values show —', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A, B], next_cursor: null } satisfies Page<RelaySummary> } })
    renderApp('/relays')
    const rowA = await screen.findByRole('row', { name: 'relay-a' })
    const a = within(rowA)
    expect(a.getByText('active')).toHaveAttribute('data-status', 'active')
    expect(a.getByRole('link', { name: 'relay-a' })).toHaveAttribute('href', `/relays/${A.id}`)
    expect(a.getByText('0.21.0')).toBeInTheDocument()
    expect(a.getByText('relay-a.local')).toBeInTheDocument()
    expect(a.getByText('203.0.113.5:9093')).toBeInTheDocument()
    expect(a.getByText('high · 12 / 500')).toBeInTheDocument()
    expect(a.getByText('2 min ago')).toBeInTheDocument()
    expect(a.getByText('< 7 days')).toHaveAttribute('data-bucket', 'lt_7d') // shared bucketFor
    expect(a.getByText('4')).toBeInTheDocument()

    const b = within(screen.getByRole('row', { name: 'relay-b' }))
    expect(b.getAllByText('—').length).toBeGreaterThanOrEqual(5) // version, hostname, address, heartbeat, expiry
    expect(b.getByText('pending')).toBeInTheDocument()
  })

  it('the status filter is sent and kept in the URL', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A], next_cursor: null } } })
    renderApp('/relays')
    await screen.findByRole('row', { name: 'relay-a' })
    const user = userEvent.setup()
    await user.click(screen.getByRole('combobox', { name: 'Status filter' }))
    await user.click(await screen.findByRole('option', { name: 'inactive' }))
    await waitFor(() => expect(relayCalls(calls)).toHaveLength(2))
    expect(qs(relayCalls(calls)[1])).toEqual({ status: 'inactive' })
  })

  it('changing the status filter clears the cursor stack and starts at page 1', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/relays': (c) => {
        const p = qs(c)
        const tag = p.status ?? 'all'
        if (p.cursor && !p.cursor.startsWith(tag)) return { status: 400, body: { error: 'invalid_cursor' } } // foreign cursor
        const page = p.cursor ? 2 : 1
        return {
          status: 200,
          body: { items: [relay({ name: `${tag}-p${page}` })], next_cursor: page === 1 ? `${tag}#2` : null },
        }
      },
    })
    renderApp('/relays')
    const user = userEvent.setup()
    await screen.findByText('all-p1')
    await user.click(screen.getByRole('button', { name: /Next/ }))
    await screen.findByText('all-p2')
    expect(screen.getByText('Page 2')).toBeInTheDocument()

    await user.click(screen.getByRole('combobox', { name: 'Status filter' }))
    await user.click(await screen.findByRole('option', { name: 'active' }))
    expect(await screen.findByText('active-p1')).toBeInTheDocument()
    expect(screen.getByText('Page 1')).toBeInTheDocument()
    const last = relayCalls(calls).at(-1)!
    expect(qs(last)).toEqual({ status: 'active' }) // no cursor carried over
    expect(relayCalls(calls).every((c) => !qs(c).cursor || qs(c).cursor.startsWith(qs(c).status ?? 'all'))).toBe(true)
  })

  it('empty, error with Retry, and invalid filter with Reset', async () => {
    signedInAs(superAdminMe)
    let mode: 'empty' | 'error' = 'error'
    const { calls } = mockFetch({
      'GET /provider/relays': () => (mode === 'error' ? { status: 500, body: { error: 'server_error' } } : { status: 200, body: { items: [], next_cursor: null } }),
    })
    const view = renderApp('/relays')
    expect(await screen.findByText('Couldn’t load relays.')).toBeInTheDocument()
    expect(relayCalls(calls)).toHaveLength(1) // no automatic retry
    mode = 'empty'
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('No relays yet.')).toBeInTheDocument()
    expect(relayCalls(calls)).toHaveLength(2)
    view.unmount()

    renderApp('/relays?status=bogus')
    expect(await screen.findByText(/Invalid filter/)).toBeInTheDocument()
    expect(relayCalls(calls)).toHaveLength(2) // the invalid URL made no request
    await userEvent.setup().click(screen.getByRole('button', { name: 'Reset filters' }))
    expect(await screen.findByText('No relays yet.')).toBeInTheDocument()
    expect(qs(relayCalls(calls).at(-1)!)).toEqual({})
  })

  it('a server 400 (e.g. a stale cursor) shows Invalid filter', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/relays': { status: 400, body: { error: 'invalid_cursor' } } })
    renderApp('/relays')
    expect(await screen.findByText(/Invalid filter/)).toBeInTheDocument()
  })

  it('exactly one request on open under StrictMode; no refetch on focus or visibility', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A], next_cursor: null } } })
    renderApp('/relays', undefined, { strict: true })
    await screen.findByRole('row', { name: 'relay-a' })
    act(() => {
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await new Promise((r) => setTimeout(r, 50))
    expect(relayCalls(calls)).toHaveLength(1)
  })

  it('Refresh sends exactly one new request', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A], next_cursor: null } } })
    renderApp('/relays')
    await screen.findByRole('row', { name: 'relay-a' })
    await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh relays' }))
    await waitFor(() => expect(relayCalls(calls)).toHaveLength(2))
    await new Promise((r) => setTimeout(r, 30))
    expect(relayCalls(calls)).toHaveLength(2)
  })

  it('relay-ops can open Relays, and both roles have the nav entry', async () => {
    for (const me of [relayOpsMe, superAdminMe]) {
      signedInAs(me)
      mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A], next_cursor: null } } })
      const view = renderApp('/relays')
      await screen.findByRole('row', { name: 'relay-a' })
      expect(within(screen.getByRole('navigation', { name: 'Sections' })).getByRole('link', { name: 'Relays' })).toHaveAttribute('href', '/relays')
      view.unmount()
    }
  })

  it('a role 403 from the server shows Forbidden', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/relays': { status: 403, body: { error: 'forbidden' } } })
    renderApp('/relays')
    expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
  })

  it('is read-only: GETs only, and no mutating controls', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A], next_cursor: 'c' } } })
    renderApp('/relays')
    await screen.findByRole('row', { name: 'relay-a' })
    const main = screen.getByRole('main')
    const buttons = within(main).getAllByRole('button').map((b) => b.textContent?.trim())
    expect(buttons.sort()).toEqual(['Next', 'Previous', 'Refresh'].sort())
    expect(within(main).queryByText(/\b(revoke|delete|edit|create|suspend)\b/i)).not.toBeInTheDocument()
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
  })
})
