import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { ActiveAlert, AlertChannel, AlertEvent, AlertRule } from '@/api/alerts'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
import { renderApp, sessionStorageDump, signedInAs } from '@/test/render'

// Alerts (Sprint 24 A3, F26): Active and History for both roles (relay-ops
// get relay and pool alerts only, decided by the controller); Rules and
// Channels super-admin only; every write with a reason; a webhook secret is
// shown once and never stored.

const TENANT = '6b1f5f2e-0000-4000-8000-000000000001'
const ACTIVE: ActiveAlert[] = [
  {
    rule_id: 'r1', rule_name: 'Relay offline', kind: 'relay.offline', scope: 'platform', tenant_id: null,
    target_type: 'relay', target_id: 'relay-1', target_name: 'relay-a', status: 'firing', severity: 'critical',
    summary: 'Relay relay-a is offline', pending_since: '2026-11-20T10:00:00Z', firing_since: '2026-11-20T10:05:00Z',
  },
  {
    rule_id: 'r6', rule_name: 'Connector offline', kind: 'connector.offline', scope: 'tenant', tenant_id: TENANT,
    target_type: 'connector', target_id: 'c-1', target_name: 'conn-a', status: 'pending', severity: 'warning',
    summary: 'Connector conn-a of tenant acme is offline', pending_since: '2026-11-20T10:01:00Z', firing_since: null,
  },
]
const RULE: AlertRule = {
  id: 'r2', kind: 'relay.over_capacity', scope: 'platform', name: 'Relay near or over capacity', enabled: true,
  severity: 'warning', params: { threshold_percent: 90 }, for_seconds: 600, renotify_seconds: 0,
  created_at: '2026-11-20T00:00:00Z', updated_at: '2026-11-20T00:00:00Z',
}
const CHANNEL: AlertChannel = {
  id: 'ch1', name: 'ops-hook', type: 'webhook', scope_filter: 'platform', min_severity: 'warning', enabled: true,
  url_host: 'https://hooks.example.test', recipients: null, created_at: '2026-11-20T00:00:00Z', updated_at: '2026-11-20T00:00:00Z',
}
const EVENT: AlertEvent = {
  id: 'ev1', rule_id: 'r1', kind: 'relay.offline', scope: 'platform', tenant_id: null, target_type: 'relay', target_id: 'relay-1',
  target_name: 'relay-a', transition: 'fired', status: 'firing', severity: 'critical', summary: 'Relay relay-a is offline',
  started_at: '2026-11-20T10:05:00Z', occurred_at: '2026-11-20T10:05:00Z',
}

const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')

describe('Alerts: active and history', () => {
  it('super-admin sees platform and tenant alerts, with state and a tenant link; the scope filter is sent', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/alerts': { status: 200, body: { items: ACTIVE, truncated: false } } })
    renderApp('/alerts')
    const relay = within(await screen.findByRole('row', { name: 'Relay relay-a is offline' }))
    expect(relay.getByText('Firing')).toBeInTheDocument()
    expect(relay.getByText('Critical')).toBeInTheDocument()
    const conn = within(screen.getByRole('row', { name: 'Connector conn-a of tenant acme is offline' }))
    expect(conn.getByText('Pending')).toBeInTheDocument()
    expect(conn.getByRole('link', { name: 'tenant' })).toHaveAttribute('href', `/tenants/${TENANT}`)
    await userEvent.setup().click(within(screen.getByRole('group', { name: 'Scope' })).getByRole('button', { name: 'Tenant' }))
    await waitFor(() => expect(calls.some((c) => c.path === '/provider/alerts' && c.search === 'scope=tenant')).toBe(true))
    expect(writes(calls)).toEqual([])
  })

  it('relay-ops: only Active and History, no scope filter, no rule or channel request even by URL', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({ 'GET /provider/alerts': { status: 200, body: { items: [ACTIVE[0]], truncated: false } } })
    renderApp('/alerts?tab=rules')
    await screen.findByRole('row', { name: 'Relay relay-a is offline' })
    const view = within(screen.getByRole('group', { name: 'View' }))
    expect(view.getAllByRole('button').map((b) => b.textContent)).toEqual(['Active', 'History'])
    expect(screen.queryByRole('group', { name: 'Scope' })).not.toBeInTheDocument()
    expect(calls.every((c) => c.path === '/provider/alerts' && c.search === '')).toBe(true)
  })

  it('shows the truncation note', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/alerts': { status: 200, body: { items: ACTIVE, truncated: true } } })
    renderApp('/alerts')
    expect(await screen.findByText(/more are open/)).toBeInTheDocument()
  })

  it('history pages by cursor and opens an event’s deliveries', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/alert-events': (c) =>
        c.search.includes('cursor=c2')
          ? { status: 200, body: { items: [{ ...EVENT, id: 'ev0', summary: 'Older event' }], next_cursor: null } }
          : { status: 200, body: { items: [EVENT], next_cursor: 'c2' } },
      'GET /provider/alert-events/ev1/deliveries': {
        status: 200,
        body: { items: [{ channel_id: 'ch1', channel_name: 'ops-hook', attempt: 1, status: 'retrying', http_status: 500, error: 'receiver answered HTTP 500', at: '2026-11-20T10:05:01Z' }] },
      },
    })
    renderApp('/alerts?tab=history')
    const user = userEvent.setup()
    const row = within(await screen.findByRole('row', { name: 'Relay relay-a is offline' }))
    await user.click(row.getByRole('button', { name: 'Deliveries of Relay relay-a is offline' }))
    expect(await screen.findByText('receiver answered HTTP 500')).toBeInTheDocument()
    await user.keyboard('{Escape}')
    await user.click(screen.getByRole('button', { name: 'Older' }))
    expect(await screen.findByRole('row', { name: 'Older event' })).toBeInTheDocument()
    expect(calls.some((c) => c.path === '/provider/alert-events' && c.search.includes('cursor=c2'))).toBe(true)
  })
})

describe('Alerts: rules', () => {
  it('edits a rule with a reason; the change and the reason are in the body', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/alert-rules': { status: 200, body: { items: [RULE] } },
      'PATCH /provider/alert-rules/r2': { status: 200, body: { ...RULE, enabled: false } },
    })
    renderApp('/alerts?tab=rules')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Edit Relay near or over capacity' }))
    const dialog = within(await screen.findByRole('dialog'))
    const save = dialog.getByRole('button', { name: 'Save rule' })
    expect(save).toBeDisabled()
    await user.click(dialog.getByRole('checkbox', { name: 'Enabled' }))
    const threshold = dialog.getByLabelText('Threshold (% of link capacity)')
    await user.clear(threshold)
    await user.type(threshold, '80')
    expect(save).toBeDisabled() // no reason yet
    await user.type(dialog.getByLabelText('Reason'), 'too noisy on the lab')
    await user.click(save)
    await waitFor(() => expect(writes(calls)).toHaveLength(1))
    expect(writes(calls)[0].body).toEqual({ enabled: false, params: { threshold_percent: 80 }, reason: 'too noisy on the lab' })
    expect(await screen.findByText('Rule Relay near or over capacity saved.')).toBeInTheDocument()
  })
})

describe('Alerts: channels', () => {
  it('adds a webhook: the secret is shown once, cleared on close and never stored', async () => {
    signedInAs(superAdminMe)
    const secret = 'whsec_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG'
    let listed: AlertChannel[] = []
    const { calls } = mockFetch({
      'GET /provider/alert-channels': () => ({ status: 200, body: { items: listed } }),
      'POST /provider/alert-channels': () => {
        listed = [CHANNEL]
        return { status: 201, body: { channel: CHANNEL, secret } }
      },
    })
    renderApp('/alerts?tab=channels')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Add channel' }))
    const dialog = within(await screen.findByRole('dialog'))
    await user.type(dialog.getByLabelText('Name'), 'ops-hook')
    await user.type(dialog.getByLabelText('Webhook URL'), 'https://hooks.example.test/services/T0/B0/xyz')
    await user.selectOptions(dialog.getByLabelText('Receives'), 'platform')
    await user.type(dialog.getByLabelText('Reason'), 'paging')
    await user.click(dialog.getByRole('button', { name: 'Add channel' }))
    await waitFor(() => expect(writes(calls)).toHaveLength(1))
    expect(writes(calls)[0].body).toEqual({
      name: 'ops-hook', type: 'webhook', url: 'https://hooks.example.test/services/T0/B0/xyz',
      scope_filter: 'platform', min_severity: 'warning', reason: 'paging',
    })
    expect(await screen.findByLabelText('Signing secret')).toHaveTextContent(secret)
    expect(sessionStorageDump()).not.toContain(secret)
    expect(JSON.stringify({ ...localStorage })).not.toContain(secret)
    await user.click(screen.getByRole('button', { name: 'I’ve saved it' }))
    await waitFor(() => expect(screen.queryByText(secret)).not.toBeInTheDocument())
    // The list shows the host only, never the path.
    const row = within(await screen.findByRole('row', { name: 'ops-hook' }))
    expect(row.getByText('https://hooks.example.test/…')).toBeInTheDocument()
    expect(document.body.textContent).not.toContain('/services/T0/B0/xyz')
  })

  it('sends a test with a reason and reports the result', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/alert-channels': { status: 200, body: { items: [CHANNEL] } },
      'POST /provider/alert-channels/ch1/test': { status: 200, body: { delivered: false, http_status: 500, error: 'receiver answered HTTP 500' } },
    })
    renderApp('/alerts?tab=channels')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Test ops-hook' }))
    const dialog = within(await screen.findByRole('dialog'))
    await user.type(dialog.getByLabelText('Reason'), 'check')
    await user.click(dialog.getByRole('button', { name: 'Send test' }))
    expect(await screen.findByText('Test alert to ops-hook failed: receiver answered HTTP 500.')).toBeInTheDocument()
    expect(writes(calls)[0].body).toEqual({ reason: 'check' })
  })

  it('rotates a secret (shown once) and maps a refused URL to a clear message', async () => {
    signedInAs(superAdminMe)
    mockFetch({
      'GET /provider/alert-channels': { status: 200, body: { items: [CHANNEL] } },
      'POST /provider/alert-channels/ch1/rotate-secret': { status: 200, body: { secret: 'whsec_rotated' } },
      'PATCH /provider/alert-channels/ch1': { status: 400, body: { error: 'invalid_url' } },
    })
    renderApp('/alerts?tab=channels')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Rotate secret of ops-hook' }))
    let dialog = within(await screen.findByRole('dialog'))
    await user.type(dialog.getByLabelText('Reason'), 'leaked')
    await user.click(dialog.getByRole('button', { name: 'Rotate secret' }))
    expect(await screen.findByLabelText('Signing secret')).toHaveTextContent('whsec_rotated')
    await user.click(screen.getByRole('button', { name: 'I’ve saved it' }))
    await user.click(await screen.findByRole('button', { name: 'Edit ops-hook' }))
    dialog = within(await screen.findByRole('dialog'))
    await user.type(dialog.getByLabelText('New webhook URL'), 'https://10.0.0.5/hook')
    await user.type(dialog.getByLabelText('Reason'), 'move')
    await user.click(dialog.getByRole('button', { name: 'Save channel' }))
    expect(await dialog.findByText(/The webhook URL was refused/)).toBeInTheDocument()
  })
})

describe('Overview: firing alerts tile', () => {
  it('counts firing alerts and links to the Alerts page', async () => {
    signedInAs(relayOpsMe)
    mockFetch({
      'GET /provider/relays': { status: 200, body: { items: [], next_cursor: null } },
      'GET /provider/relay-pools': { status: 200, body: { items: [] } },
      'GET /provider/alerts': { status: 200, body: { items: [ACTIVE[0]], truncated: false } },
    })
    renderApp('/')
    const count = await screen.findByLabelText('1 firing alerts')
    expect(count.closest('a')).toHaveAttribute('href', '/alerts')
    expect(screen.getByText('1 critical · 0 other')).toBeInTheDocument()
  })
})
