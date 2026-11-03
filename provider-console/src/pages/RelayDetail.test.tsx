import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { RelayDetail } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { RelayDangerZone } from '@/components/RelayDangerZone'
import { RelayDrainCard } from '@/components/RelayDrainCard'
import { renderApp, signedInAs } from '@/test/render'

const ID = '11111111-1111-1111-1111-111111111111'
const TENANT = '99999999-9999-9999-9999-999999999999'
const DAY = 24 * 60 * 60_000
const iso = (ms: number) => new Date(Date.now() + ms).toISOString()

function detail(over: Partial<RelayDetail> = {}): RelayDetail {
  return {
    id: ID,
    name: 'relay-a',
    status: 'active',
    version: '0.21.0',
    hostname: 'relay-a.local',
    public_addr: '203.0.113.5:9093',
    address_scope: 'public',
    capacity_label: 'medium',
    connection_count: 7,
    max_connections: 100,
    cert_serial: 'cur1',
    cert_not_after: iso(20 * DAY),
    last_heartbeat_at: iso(-60_000),
    attached_connectors: 1,
    created_at: '2026-10-01T00:00:00Z',
    draining_since: null,
    drain_state: null,
    in_relay_list: true,
    eligible_relay_count: 2,
    dns_allowlist: ['relay.example.com'],
    ip_allowlist: ['203.0.113.5'],
    certificates: [
      // Newest by issue date, but NOT current: "Current" must come from is_current only.
      { serial: 'newer-not-current', issued_at: iso(-DAY), not_after: iso(200 * DAY), revoked_at: null, revocation_reason: null, is_current: false },
      { serial: 'cur1', issued_at: iso(-10 * DAY), not_after: iso(20 * DAY), revoked_at: null, revocation_reason: null, is_current: true },
      // Expired by date but not revoked: must not be shown as revoked.
      { serial: 'old-expired', issued_at: iso(-400 * DAY), not_after: iso(-30 * DAY), revoked_at: null, revocation_reason: null, is_current: false },
      // Revoked although its date is still valid: revoked comes from revoked_at only.
      { serial: 'rev1', issued_at: iso(-20 * DAY), not_after: iso(100 * DAY), revoked_at: '2026-10-02T08:00:00Z', revocation_reason: 'superseded', is_current: false },
    ],
    certificates_truncated: false,
    attachments: [
      { connector_id: 'c-1', tenant_id: TENANT, attached_at: '2026-10-03T00:00:00Z', last_confirmed: iso(-5 * 60_000), source: 'heartbeat' },
    ],
    attachments_truncated: false,
    ...over,
  }
}

const relayCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path === `/provider/relays/${ID}`)

describe('Relay detail', () => {
  it('shows overview, allowlists, certificate history and attachments', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail() } })
    renderApp(`/relays/${ID}`)
    expect(await screen.findByRole('heading', { name: 'relay-a', level: 1 })).toBeInTheDocument()
    expect(screen.getByRole('img', { name: '7 of 100 connections used' })).toBeInTheDocument()
    expect(screen.getByText('medium')).toHaveAttribute('data-tier', 'medium')
    expect(within(screen.getByRole('list', { name: 'DNS allowlist' })).getByText('relay.example.com')).toBeInTheDocument()
    expect(within(screen.getByRole('list', { name: 'IP allowlist' })).getByText('203.0.113.5')).toBeInTheDocument()

    const att = screen.getByRole('table', { name: 'Attached connectors' })
    expect(within(att).getByText('c-1')).toBeInTheDocument()
    expect(within(att).getByText(TENANT)).toBeInTheDocument()
    expect(within(att).queryByRole('link')).not.toBeInTheDocument() // tenant id is plain text
    expect(screen.queryByText(/first 200/)).not.toBeInTheDocument()
  })

  it('"Current" comes only from is_current and revocation only from revoked_at', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail() } })
    renderApp(`/relays/${ID}`)
    await screen.findByRole('table', { name: 'Certificate history' })
    const row = (serial: string) => within(screen.getByRole('row', { name: serial }))

    expect(screen.getAllByText('Current')).toHaveLength(1)
    expect(row('cur1').getByText('Current')).toBeInTheDocument()
    expect(screen.getByRole('row', { name: 'cur1' })).toHaveAttribute('data-current', 'true')
    expect(row('newer-not-current').queryByText('Current')).not.toBeInTheDocument()

    expect(screen.getAllByText('Revoked')).toHaveLength(1)
    expect(row('rev1').getByText('Revoked')).toBeInTheDocument()
    expect(row('rev1').getByText(/superseded/)).toBeInTheDocument()
    expect(row('old-expired').queryByText('Revoked')).not.toBeInTheDocument()

    // Expiry badges use the shared bucketFor semantics.
    expect(row('old-expired').getByText('Expired')).toHaveAttribute('data-bucket', 'expired')
    expect(row('cur1').getByText('< 30 days')).toHaveAttribute('data-bucket', 'lt_30d')
    expect(row('rev1').getByText('OK')).toHaveAttribute('data-bucket', 'ok')
  })

  it('shows the 200-entry notes only when the API says a list was cut', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail({ certificates_truncated: true, attachments_truncated: true }) } })
    renderApp(`/relays/${ID}`)
    await screen.findByRole('table', { name: 'Certificate history' })
    expect(screen.getAllByText('Showing the first 200 entries.')).toHaveLength(2)
  })

  it('empty collections show their empty text', async () => {
    signedInAs(superAdminMe)
    mockFetch({
      [`GET /provider/relays/${ID}`]: { status: 200, body: detail({ dns_allowlist: [], ip_allowlist: [], certificates: [], attachments: [], cert_serial: null, cert_not_after: null }) },
    })
    renderApp(`/relays/${ID}`)
    expect(await screen.findByText('No certificates issued yet.')).toBeInTheDocument()
    expect(screen.getByText('No connectors attached.')).toBeInTheDocument()
    expect(screen.getAllByText('None')).toHaveLength(2)
  })

  it.each([
    ['unknown id (404)', 404, 'not_found'],
    ['malformed id (400)', 400, 'invalid_id'],
  ])('%s shows Relay not found with a way back', async (_n, status, error) => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relays/${ID}`]: { status, body: { error } } })
    renderApp(`/relays/${ID}`)
    expect(await screen.findByRole('heading', { name: 'Relay not found' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Back to relays/ })).toHaveAttribute('href', '/relays')
  })

  it('an error offers Retry, which sends exactly one new request', async () => {
    signedInAs(superAdminMe)
    let fail = true
    const { calls } = mockFetch({
      [`GET /provider/relays/${ID}`]: () => (fail ? { status: 500, body: { error: 'server_error' } } : { status: 200, body: detail() }),
    })
    renderApp(`/relays/${ID}`)
    expect(await screen.findByText('Couldn’t load this relay.')).toBeInTheDocument()
    await new Promise((r) => setTimeout(r, 30))
    expect(relayCalls(calls)).toHaveLength(1)
    fail = false
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
    await screen.findByRole('heading', { name: 'relay-a', level: 1 })
    expect(relayCalls(calls)).toHaveLength(2)
  })

  it('one request on open (StrictMode), none on focus; Refresh adds exactly one', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail() } })
    renderApp(`/relays/${ID}`, undefined, { strict: true })
    await screen.findByRole('heading', { name: 'relay-a', level: 1 })
    act(() => {
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await new Promise((r) => setTimeout(r, 50))
    expect(relayCalls(calls)).toHaveLength(1)
    await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh relay' }))
    await waitFor(() => expect(relayCalls(calls)).toHaveLength(2))
    await new Promise((r) => setTimeout(r, 30))
    expect(relayCalls(calls)).toHaveLength(2)
  })

  it('a role 403 shows Forbidden', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relays/${ID}`]: { status: 403, body: { error: 'forbidden' } } })
    renderApp(`/relays/${ID}`)
    expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
  })

  // Phase D (D-31): relay-ops may drain and resume, nothing else.
  it('relay-ops sees only Refresh and Drain relay, no other mutating controls; loading sends GETs only', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail() } })
    renderApp(`/relays/${ID}`)
    await screen.findByRole('heading', { name: 'relay-a', level: 1 })
    const main = screen.getByRole('main')
    expect(within(main).getAllByRole('button').map((b) => b.textContent?.trim())).toEqual(['Refresh', 'Drain relay'])
    expect(within(main).queryByText(/\b(revoke|delete|edit|create|suspend)\b/i)).not.toBeInTheDocument()
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
  })

  // Sprint 22 Phase X and D (D-30, D-31): the only mutating controls on this
  // page are Drain (both roles) and a super-admin's Revoke and Delete;
  // opening the page sends GETs only.
  it('a super-admin sees exactly Refresh, Drain relay, Revoke relay and Delete relay; loading sends GETs only', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail() } })
    renderApp(`/relays/${ID}`)
    await screen.findByRole('heading', { name: 'relay-a', level: 1 })
    const main = screen.getByRole('main')
    expect(within(main).getAllByRole('button').map((b) => b.textContent?.trim())).toEqual(['Refresh', 'Drain relay', 'Revoke relay', 'Delete relay'])
    expect(within(main).queryByText(/\b(edit|create|suspend)\b/i)).not.toBeInTheDocument()
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
  })

  // Sprint 22 Phase E (D-20): a pending relay waits for its one-time token.
  it('a pending relay says it is waiting for enrolment, with the expired-token hint', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail({ status: 'pending', certificates: [], cert_serial: null, cert_not_after: null }) } })
    renderApp(`/relays/${ID}`)
    expect(await screen.findByText(/Waiting for enrolment/)).toBeInTheDocument()
    expect(screen.getByText(/If the token has expired, delete this relay and add a new one\./)).toBeInTheDocument()
  })

  it('an active relay shows no enrolment hint', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail() } })
    renderApp(`/relays/${ID}`)
    await screen.findByRole('heading', { name: 'relay-a', level: 1 })
    expect(screen.queryByText(/Waiting for enrolment/)).not.toBeInTheDocument()
  })
})

// ── Sprint 22 Phase X: revoke and delete (D-17, D-20, D-30, D-31) ───────────

const REASON = 'host compromised'

type Route = FakeResponse | ((call: RecordedCall) => FakeResponse)

/** GET answers each detail in turn (the last one repeats); writes per routes. */
function relayRoutes(details: (RelayDetail | FakeResponse)[], writes: Record<string, Route> = {}) {
  let n = 0
  return {
    [`GET /provider/relays/${ID}`]: () => {
      const d = details[Math.min(n++, details.length - 1)]
      return 'status' in d && typeof d.status === 'number' ? (d as FakeResponse) : { status: 200, body: d }
    },
    ...writes,
  }
}

async function openAction(user: ReturnType<typeof userEvent.setup>, action: 'Revoke relay' | 'Delete relay') {
  await user.click(await screen.findByRole('button', { name: action }))
  return screen.findByRole('dialog', { name: action })
}

async function fillAndConfirm(
  user: ReturnType<typeof userEvent.setup>,
  dialog: HTMLElement,
  action: 'Revoke relay' | 'Delete relay',
  name = 'relay-a',
) {
  await user.type(within(dialog).getByLabelText('Reason'), REASON)
  await user.type(within(dialog).getByLabelText(/Type relay-a to confirm/), name)
  await user.click(within(dialog).getByRole('button', { name: action }))
}

const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')
const gets = (calls: RecordedCall[]) => relayCalls(calls).filter((c) => c.method === 'GET')

describe('Relay detail: danger zone visibility', () => {
  it.each([
    ['active', ['Revoke relay', 'Delete relay']],
    ['inactive', ['Revoke relay', 'Delete relay']],
    ['pending', ['Revoke relay', 'Delete relay']],
    ['revoked', ['Delete relay']],
  ] as const)('super-admin, %s relay: %j', async (status, actions) => {
    signedInAs(superAdminMe)
    mockFetch(relayRoutes([detail({ status })]))
    renderApp(`/relays/${ID}`)
    const zone = await screen.findByRole('region', { name: 'Danger zone' })
    expect(within(zone).getAllByRole('button').map((b) => b.textContent?.trim())).toEqual([...actions])
  })

  it('super-admin, deleted relay: no danger zone', async () => {
    signedInAs(superAdminMe)
    mockFetch(relayRoutes([detail({ status: 'deleted' })]))
    renderApp(`/relays/${ID}`)
    await screen.findByRole('heading', { name: 'relay-a', level: 1 })
    expect(screen.queryByRole('region', { name: 'Danger zone' })).not.toBeInTheDocument()
  })

  it('relay-ops: the page loads, with no danger zone', async () => {
    signedInAs(relayOpsMe)
    mockFetch(relayRoutes([detail()]))
    renderApp(`/relays/${ID}`)
    await screen.findByRole('heading', { name: 'relay-a', level: 1 })
    expect(screen.queryByRole('region', { name: 'Danger zone' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Revoke relay|Delete relay/ })).not.toBeInTheDocument()
  })

  // Unknown roles can't open relay pages at all (route guard); the danger zone
  // also checks the role itself, so it stays hidden even without the guard.
  it.each([
    ['an unknown role', 'auditor'],
    ['an empty role', ''],
    ['a near-miss role', 'Super-Admin'],
  ])('%s: Forbidden page, and the danger zone itself refuses to render', async (_, role) => {
    signedInAs({ ...superAdminMe, role })
    mockFetch(relayRoutes([detail()]))
    renderApp(`/relays/${ID}`)
    expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Revoke relay|Delete relay/ })).not.toBeInTheDocument()

    cleanup()
    render(<RelayDangerZone relay={detail()} onDone={() => {}} />)
    expect(screen.queryByRole('region', { name: 'Danger zone' })).not.toBeInTheDocument()
  })

  it('the danger zone component renders for a super-admin (positive control for the direct render)', () => {
    signedInAs(superAdminMe)
    render(<RelayDangerZone relay={detail()} onDone={() => {}} />)
    expect(screen.getByRole('region', { name: 'Danger zone' })).toBeInTheDocument()
  })
})

describe('Relay detail: revoke and delete requests', () => {
  it('revoke: one POST to /revoke with the reason and name in the body, then a refresh shows it revoked', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch(
      relayRoutes([detail(), detail({ status: 'revoked' })], { [`POST /provider/relays/${ID}/revoke`]: { status: 204 } }),
    )
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    await fillAndConfirm(user, await openAction(user, 'Revoke relay'), 'Revoke relay')

    expect(await screen.findByText(/relay-a is revoked/)).toBeInTheDocument()
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'POST', path: `/provider/relays/${ID}/revoke`, search: '', body: { reason: REASON, confirm_name: 'relay-a' } }),
    ])
    expect(gets(calls)).toHaveLength(2)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    const zone = screen.getByRole('region', { name: 'Danger zone' })
    expect(within(zone).getAllByRole('button').map((b) => b.textContent?.trim())).toEqual(['Delete relay'])
  })

  it('delete: one DELETE with the reason and name in the body, then a refresh shows it deleted', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch(
      relayRoutes([detail({ status: 'pending' }), detail({ status: 'deleted' })], { [`DELETE /provider/relays/${ID}`]: { status: 204 } }),
    )
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    await fillAndConfirm(user, await openAction(user, 'Delete relay'), 'Delete relay')

    expect(await screen.findByText(/relay-a is deleted/)).toBeInTheDocument()
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'DELETE', path: `/provider/relays/${ID}`, search: '', body: { reason: REASON, confirm_name: 'relay-a' } }),
    ])
    expect(screen.queryByRole('region', { name: 'Danger zone' })).not.toBeInTheDocument()
  })

  it('a revoked relay can still be deleted', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch(
      relayRoutes([detail({ status: 'revoked' }), detail({ status: 'deleted' })], { [`DELETE /provider/relays/${ID}`]: { status: 204 } }),
    )
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    await fillAndConfirm(user, await openAction(user, 'Delete relay'), 'Delete relay')
    expect(await screen.findByText(/relay-a is deleted/)).toBeInTheDocument()
    expect(writes(calls).map((c) => c.method)).toEqual(['DELETE'])
  })

  it('a double click sends exactly one request', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch(
      relayRoutes([detail(), detail({ status: 'revoked' })], { [`POST /provider/relays/${ID}/revoke`]: { status: 204 } }),
    )
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    const dialog = await openAction(user, 'Revoke relay')
    await user.type(within(dialog).getByLabelText('Reason'), REASON)
    await user.type(within(dialog).getByLabelText(/Type relay-a to confirm/), 'relay-a')
    await user.dblClick(within(dialog).getByRole('button', { name: 'Revoke relay' }))
    await screen.findByText(/relay-a is revoked/)
    expect(writes(calls)).toHaveLength(1)
  })

  it('cancelling sends nothing', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch(relayRoutes([detail()]))
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    const dialog = await openAction(user, 'Delete relay')
    await user.type(within(dialog).getByLabelText('Reason'), REASON)
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(writes(calls)).toHaveLength(0)
  })
})

describe('Relay detail: revoke and delete errors', () => {
  async function failWith(action: 'Revoke relay' | 'Delete relay', res: FakeResponse, after: (RelayDetail | FakeResponse)[] = []) {
    signedInAs(superAdminMe)
    const key = action === 'Revoke relay' ? `POST /provider/relays/${ID}/revoke` : `DELETE /provider/relays/${ID}`
    const { calls } = mockFetch(relayRoutes([detail(), ...after], { [key]: res }))
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    const dialog = await openAction(user, action)
    await fillAndConfirm(user, dialog, action)
    return { calls, dialog }
  }

  it('confirmation_mismatch: shown on the name field, dialog stays open, no refresh', async () => {
    const { calls, dialog } = await failWith('Revoke relay', { status: 400, body: { error: 'confirmation_mismatch' } })
    const name = within(dialog).getByLabelText(/Type relay-a to confirm/)
    await waitFor(() => expect(name).toHaveAttribute('aria-invalid', 'true'))
    expect(name).toHaveAccessibleDescription(/doesn’t match the relay’s name/)
    expect(gets(calls)).toHaveLength(1)
  })

  it.each([
    ['reason_required', /Enter a reason/],
    ['reason_too_long', /500 characters or fewer/],
  ])('%s: shown on the reason field', async (code, message) => {
    const { dialog } = await failWith('Delete relay', { status: 400, body: { error: code } })
    const reason = within(dialog).getByLabelText('Reason')
    await waitFor(() => expect(reason).toHaveAccessibleDescription(message))
    expect(reason).toHaveAttribute('aria-invalid', 'true')
  })

  it('forbidden: an error inside the dialog, no Forbidden page', async () => {
    const { dialog } = await failWith('Revoke relay', { status: 403, body: { error: 'forbidden' } })
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/Only super-admins can revoke or delete relays/)
    expect(screen.queryByRole('heading', { name: /don’t have access/ })).not.toBeInTheDocument()
    expect(screen.getByRole('dialog', { name: 'Revoke relay' })).toBeInTheDocument()
  })

  it('already_revoked: refreshes and explains', async () => {
    const { calls } = await failWith('Revoke relay', { status: 409, body: { error: 'already_revoked' } }, [detail({ status: 'revoked' })])
    expect(await screen.findByText(/already revoked/)).toBeInTheDocument()
    expect(gets(calls)).toHaveLength(2)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.queryByText(/relay-a is revoked\./)).not.toBeInTheDocument() // not reported as our success
  })

  it('already_deleted: refreshes and explains', async () => {
    const { calls } = await failWith('Delete relay', { status: 409, body: { error: 'already_deleted' } }, [detail({ status: 'deleted' })])
    expect(await screen.findByText(/already deleted/)).toBeInTheDocument()
    expect(gets(calls)).toHaveLength(2)
    expect(screen.queryByRole('region', { name: 'Danger zone' })).not.toBeInTheDocument()
  })

  it('not_found: refreshes; the page says the relay is gone', async () => {
    const { calls } = await failWith('Delete relay', { status: 404, body: { error: 'not_found' } }, [{ status: 404, body: { error: 'not_found' } }])
    expect(await screen.findByRole('heading', { name: 'Relay not found' })).toBeInTheDocument()
    expect(gets(calls)).toHaveLength(2)
  })

  it('an unknown error: a generic message in the dialog, nothing reported as done', async () => {
    const { dialog, calls } = await failWith('Revoke relay', { status: 500, body: { error: 'server_error' } })
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/didn’t go through/)
    expect(screen.queryByText(/relay-a is revoked/)).not.toBeInTheDocument()
    expect(gets(calls)).toHaveLength(1)
  })
})

// ── Sprint 22 Phase D: drain and resume (D-30, D-31, D-32) ─────────────────

const draining = (over: Partial<RelayDetail> = {}) =>
  detail({ draining_since: iso(-5 * 60_000), drain_state: 'migrating', in_relay_list: false, eligible_relay_count: 1, ...over })

describe('Relay detail: drain card', () => {
  it.each([
    ['active', true],
    ['inactive', true],
    ['pending', false],
    ['revoked', false],
    ['deleted', false],
  ] as const)('%s relay: drain card shown = %s, for both roles', async (status, shown) => {
    for (const me of [superAdminMe, relayOpsMe]) {
      cleanup()
      signedInAs(me)
      mockFetch(relayRoutes([detail({ status })]))
      renderApp(`/relays/${ID}`)
      await screen.findByRole('heading', { name: 'relay-a', level: 1 })
      expect(!!screen.queryByRole('region', { name: 'Drain' })).toBe(shown)
    }
  })

  it('an unknown role: the card itself refuses to render', () => {
    signedInAs({ ...superAdminMe, role: 'auditor' })
    render(<RelayDrainCard relay={detail()} onDone={() => {}} />)
    expect(screen.queryByRole('region', { name: 'Drain' })).not.toBeInTheDocument()
  })

  it('says what draining does before confirming, including reduced availability', async () => {
    signedInAs(relayOpsMe)
    mockFetch(relayRoutes([detail()]))
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Drain relay' }))
    const dialog = await screen.findByRole('dialog', { name: 'Drain relay' })
    expect(within(dialog).getByRole('note')).toHaveTextContent(/reduces relay availability/)
    // FU-14: open connections are not cut after 120 s; the console must not promise it.
    expect(dialog).toHaveTextContent(/keep running until they end/)
    expect(dialog).not.toHaveTextContent(/120 seconds/)
    expect(within(dialog).queryByLabelText(/to confirm/)).not.toBeInTheDocument() // no typed name
    expect(within(dialog).getByText(/Optional\. Recorded in the provider audit log\./)).toBeInTheDocument()
  })

  it('drain: one POST with the trimmed reason, then a refresh shows it draining', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch(relayRoutes([detail(), draining()], { [`POST /provider/relays/${ID}/drain`]: { status: 204 } }))
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Drain relay' }))
    const dialog = await screen.findByRole('dialog', { name: 'Drain relay' })
    await user.type(within(dialog).getByLabelText('Reason'), '  kernel upgrade ')
    await user.click(within(dialog).getByRole('button', { name: 'Drain relay' }))

    expect(await screen.findByText(/relay-a is draining/)).toBeInTheDocument()
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'POST', path: `/provider/relays/${ID}/drain`, search: '', body: { reason: 'kernel upgrade' } }),
    ])
    expect(gets(calls)).toHaveLength(2)
    const card = screen.getByRole('region', { name: 'Drain' })
    expect(within(card).getByText('Draining')).toBeInTheDocument()
    expect(within(card).getByRole('button', { name: 'Resume relay' })).toBeInTheDocument()
  })

  it('drain without a reason sends an empty body object; a double click sends one request', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch(relayRoutes([detail(), draining()], { [`POST /provider/relays/${ID}/drain`]: { status: 204 } }))
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Drain relay' }))
    const dialog = await screen.findByRole('dialog', { name: 'Drain relay' })
    await user.dblClick(within(dialog).getByRole('button', { name: 'Drain relay' }))
    await screen.findByText(/relay-a is draining/)
    expect(writes(calls)).toEqual([expect.objectContaining({ path: `/provider/relays/${ID}/drain`, body: {} })])
  })

  it('last_eligible_relay: explained inside the dialog, nothing refreshed', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch(
      relayRoutes([detail()], { [`POST /provider/relays/${ID}/drain`]: { status: 409, body: { error: 'last_eligible_relay' } } }),
    )
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Drain relay' }))
    const dialog = await screen.findByRole('dialog', { name: 'Drain relay' })
    await user.click(within(dialog).getByRole('button', { name: 'Drain relay' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/only relay connectors can use right now/)
    expect(gets(calls)).toHaveLength(1)
  })

  it.each([
    ['invalid_state', /hasn’t enrolled yet/],
    ['already_revoked', /has been revoked/],
    ['already_deleted', /has been deleted/],
  ])('%s: the dialog closes, the page refreshes and explains', async (code, message) => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch(relayRoutes([detail(), detail({ status: 'revoked' })], { [`POST /provider/relays/${ID}/drain`]: { status: 409, body: { error: code } } }))
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Drain relay' }))
    await user.click(within(await screen.findByRole('dialog', { name: 'Drain relay' })).getByRole('button', { name: 'Drain relay' }))
    expect(await screen.findByText(message)).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(gets(calls)).toHaveLength(2)
  })

  it('resume: one POST with an empty body, no dialog, then a refresh shows it back', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch(relayRoutes([draining(), detail()], { [`POST /provider/relays/${ID}/resume`]: { status: 204 } }))
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Resume relay' }))
    expect(await screen.findByText(/relay-a is back in the relay list/)).toBeInTheDocument()
    expect(writes(calls)).toEqual([expect.objectContaining({ method: 'POST', path: `/provider/relays/${ID}/resume`, search: '', body: {} })])
    expect(within(screen.getByRole('region', { name: 'Drain' })).getByRole('button', { name: 'Drain relay' })).toBeInTheDocument()
  })

  it('a failed resume is shown in the card', async () => {
    signedInAs(relayOpsMe)
    mockFetch(relayRoutes([draining()], { [`POST /provider/relays/${ID}/resume`]: { status: 500, body: { error: 'server_error' } } }))
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Resume relay' }))
    expect(await within(screen.getByRole('region', { name: 'Drain' })).findByRole('alert')).toHaveTextContent(/resume didn’t go through/)
  })
})

describe('Relay detail: drain progress comes from drain_state only', () => {
  it.each([
    ['migrating', { attached_connectors: 3 }, /3 connectors still attached/],
    ['migrating', { attached_connectors: 1 }, /1 connector still attached/],
    ['streams_open', { attached_connectors: 0, connection_count: 2 }, /2 connections still open\. They keep running until they end/],
    ['unverified', { attached_connectors: 0, connection_count: 0 }, /Not confirmed yet/],
    ['drained', { attached_connectors: 0, connection_count: 0 }, /Drained \(as of the last heartbeat\)/],
  ] as const)('%s', async (state, over, text) => {
    signedInAs(relayOpsMe)
    mockFetch(relayRoutes([draining({ drain_state: state, ...over })]))
    renderApp(`/relays/${ID}`)
    const card = await screen.findByRole('region', { name: 'Drain' })
    expect(within(card).getByText(text)).toHaveAttribute('data-drain-state', state)
  })

  it('unverified on an inactive relay says it can’t confirm, instead of asking to wait', async () => {
    signedInAs(relayOpsMe)
    mockFetch(relayRoutes([draining({ status: 'inactive', drain_state: 'unverified', attached_connectors: 0, connection_count: 0 })]))
    renderApp(`/relays/${ID}`)
    const card = await screen.findByRole('region', { name: 'Drain' })
    expect(within(card).getByText(/the relay is inactive \(no recent heartbeat\)/)).toHaveAttribute('data-drain-state', 'unverified')
    expect(within(card).queryByText(/Refresh in a minute/)).not.toBeInTheDocument()
  })

  it('zero counts alone never read as drained: the state decides', async () => {
    signedInAs(superAdminMe)
    mockFetch(relayRoutes([draining({ drain_state: 'unverified', attached_connectors: 0, connection_count: 0 })]))
    renderApp(`/relays/${ID}`)
    const card = await screen.findByRole('region', { name: 'Drain' })
    expect(within(card).queryByText(/^Drained/)).not.toBeInTheDocument()
    expect(within(card).queryByText(/can now be revoked or deleted/)).not.toBeInTheDocument()
  })

  it('drained: a super-admin is told it can now be revoked or deleted; relay-ops is not', async () => {
    signedInAs(superAdminMe)
    mockFetch(relayRoutes([draining({ drain_state: 'drained', attached_connectors: 0, connection_count: 0 })]))
    renderApp(`/relays/${ID}`)
    expect(await screen.findByText(/can now be revoked or deleted/)).toBeInTheDocument()
    cleanup()
    signedInAs(relayOpsMe)
    mockFetch(relayRoutes([draining({ drain_state: 'drained', attached_connectors: 0, connection_count: 0 })]))
    renderApp(`/relays/${ID}`)
    await screen.findByRole('region', { name: 'Drain' })
    expect(screen.queryByText(/can now be revoked or deleted/)).not.toBeInTheDocument()
  })

  it('the header shows a Draining badge next to the status', async () => {
    signedInAs(relayOpsMe)
    mockFetch(relayRoutes([draining()]))
    renderApp(`/relays/${ID}`)
    const heading = await screen.findByRole('heading', { name: 'relay-a', level: 1 })
    expect(within(heading.parentElement as HTMLElement).getByText('Draining')).toBeInTheDocument()
  })
})

// Phase D (P3): the last-relay warning is advisory: shown, never blocking.
describe('Relay detail: last-relay warning on revoke and delete', () => {
  it.each(['Revoke relay', 'Delete relay'] as const)('%s of the only listed relay warns, and still goes through', async (action) => {
    signedInAs(superAdminMe)
    const key = action === 'Revoke relay' ? `POST /provider/relays/${ID}/revoke` : `DELETE /provider/relays/${ID}`
    const { calls } = mockFetch(relayRoutes([detail({ in_relay_list: true, eligible_relay_count: 1 }), detail({ status: 'deleted' })], { [key]: { status: 204 } }))
    renderApp(`/relays/${ID}`)
    const user = userEvent.setup()
    const dialog = await openAction(user, action)
    expect(within(dialog).getByRole('note')).toHaveTextContent(/No eligible relays will remain/)
    await fillAndConfirm(user, dialog, action)
    await waitFor(() => expect(writes(calls)).toHaveLength(1))
  })

  it.each([
    ['another relay is listed', { in_relay_list: true, eligible_relay_count: 2 }],
    ['this relay is not listed', { in_relay_list: false, eligible_relay_count: 1 }],
  ])('no warning when %s', async (_n, over) => {
    signedInAs(superAdminMe)
    mockFetch(relayRoutes([detail(over)]))
    renderApp(`/relays/${ID}`)
    const dialog = await openAction(userEvent.setup(), 'Revoke relay')
    expect(within(dialog).queryByRole('note')).not.toBeInTheDocument()
  })
})
