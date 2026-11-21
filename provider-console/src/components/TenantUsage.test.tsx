import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { TenantUsage } from '@/components/TenantUsage'
import { mockFetch, superAdminMe, type RecordedCall } from '@/test/fetch'
import { signedInAs } from '@/test/render'

// Sprint 25 U3 (F33; D-59, D-60): the tenant Usage section reads the last 12
// months and the selected month by day, once each (every read is audited);
// a month change reads that month once; incomplete periods are marked and
// never shown as 0 for "not counted".

const TENANT = '33333333-3333-3333-3333-333333333333'
const m = (over: Record<string, number> = {}) => ({ relay_bytes_to_clients: 0, relay_bytes_to_connectors: 0, session_minutes: 0, connector_minutes: 0, shield_minutes: 0, ...over })
const utcMonth = (d: Date, n = 0) => new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + n, 1))

function body(c: RecordedCall) {
  const q = new URLSearchParams(c.search)
  const from = new Date(q.get('from')!)
  const tenant = { id: TENANT, name: 'Acme', slug: 'acme', plan_name: 'Free', purged: false }
  if (q.get('granularity') === 'month') {
    return {
      status: 200,
      body: {
        tenant, granularity: 'month', from: q.get('from'), to: q.get('to'),
        buckets: Array.from({ length: 12 }, (_, i) => ({
          start: utcMonth(from, i).toISOString(), complete: i < 11, collected_minutes: 1, minutes: 1,
          meters: m({ relay_bytes_to_clients: (i + 1) * 1_000_000_000 }), active_users: i,
        })),
      },
    }
  }
  return {
    status: 200,
    body: {
      tenant, granularity: 'day', from: q.get('from'), to: q.get('to'),
      buckets: [
        { start: from.toISOString(), complete: true, collected_minutes: 1440, minutes: 1440, meters: m({ relay_bytes_to_clients: 5_000_000 }) },
        { start: new Date(from.getTime() + 86_400_000).toISOString(), complete: false, collected_minutes: 600, minutes: 1440, meters: m({ relay_bytes_to_clients: 2_000_000 }) },
        { start: new Date(from.getTime() + 2 * 86_400_000).toISOString(), complete: false, collected_minutes: 0, minutes: 1440, meters: m() },
      ],
    },
  }
}

afterEach(() => vi.restoreAllMocks())

describe('Tenant usage', () => {
  it('reads 12 months and this month by day, once each; GETs only', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/usage': body })
    render(<TenantUsage tenantId={TENANT} />)
    await screen.findByRole('table', { name: 'Usage by month' })
    await waitFor(() => expect(calls).toHaveLength(2))
    const now = new Date()
    const qs = calls.map((c) => Object.fromEntries(new URLSearchParams(c.search)))
    expect(qs).toContainEqual({ tenant_id: TENANT, granularity: 'month', from: utcMonth(now, -11).toISOString(), to: utcMonth(now, 1).toISOString() })
    expect(qs).toContainEqual({ tenant_id: TENANT, granularity: 'day', from: utcMonth(now).toISOString(), to: utcMonth(now, 1).toISOString() })
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
    // Newest month first, incomplete marked; the plan is named.
    const rows = within(screen.getByRole('table', { name: 'Usage by month' })).getAllByRole('row').slice(1)
    expect(within(rows[0]).getByText('Incomplete')).toBeInTheDocument()
    expect(within(rows[0]).getByText('12.0 GB')).toBeInTheDocument()
    expect(within(rows[1]).queryByText('Incomplete')).not.toBeInTheDocument()
    expect(screen.getByText(/Plan: Free\./)).toBeInTheDocument()
  })

  it('days: incomplete days marked, uncounted days not shown as 0', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/usage': body })
    render(<TenantUsage tenantId={TENANT} />)
    const bars = await screen.findByRole('list', { name: 'Relayed to clients by day' })
    const labels = within(bars).getAllByRole('listitem').map((li) => li.getAttribute('aria-label'))
    expect(labels[0]).toMatch(/: 5\.0 MB$/)
    expect(labels[1]).toMatch(/: 2\.0 MB \(incomplete\)$/)
    expect(labels[2]).toMatch(/: not counted$/)
    expect(labels.join()).not.toMatch(/0 B/)
  })

  it('a month change reads that month once; next month is disabled at the current month', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/usage': body })
    render(<TenantUsage tenantId={TENANT} />)
    await screen.findByRole('table', { name: 'Usage by month' })
    expect(screen.getByRole('button', { name: 'Next month' })).toBeDisabled()
    await userEvent.setup().click(screen.getByRole('button', { name: 'Previous month' }))
    await waitFor(() => expect(calls).toHaveLength(3))
    const q = new URLSearchParams(calls[2].search)
    expect(q.get('granularity')).toBe('day')
    expect(q.get('from')).toBe(utcMonth(new Date(), -1).toISOString())
  })

  it('exports the shown month as CSV with completeness per day', async () => {
    signedInAs(superAdminMe)
    const files: { name: string; text: Promise<string> }[] = []
    vi.spyOn(URL, 'createObjectURL').mockImplementation((b) => {
      files.push({ name: '', text: (b as Blob).text() })
      return 'blob:test'
    })
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      files[files.length - 1].name = this.download
    })
    const { calls } = mockFetch({ 'GET /provider/usage': body })
    render(<TenantUsage tenantId={TENANT} />)
    await screen.findByRole('list', { name: 'Relayed to clients by day' })
    const before = calls.length
    await userEvent.setup().click(screen.getByRole('button', { name: 'CSV' }))
    expect(files).toHaveLength(1)
    expect(files[0].name).toMatch(/^usage-acme-\d{4}-\d{2}\.csv$/)
    const text = await files[0].text
    expect(text.split('\r\n')[2]).toMatch(/,false,600,1440,2000000,/)
    expect(calls).toHaveLength(before) // the export re-reads nothing
  })
})
