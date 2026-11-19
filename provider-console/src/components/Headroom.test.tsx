import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { RelayBandwidth, RelaySummary } from '@/api/types'
import { HeadroomCell, HeadroomDetail, RelayCapacityCard } from '@/components/Headroom'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
import { signedInAs } from '@/test/render'

// F5 headroom (Sprint 23 B2, D-42 refined 2026-11-18): the console shows the
// controller's capacity and headroom as reported; no capacity reads
// "Not configured", never zero or unlimited.

const ID = '11111111-1111-1111-1111-111111111111'
const BW: RelayBandwidth = { measured_at: '2026-11-18T10:00:00Z', window_seconds: 300, to_client_bytes_per_sec: 60_000_000, to_connector_bytes_per_sec: 50_000_000 }

type R = Pick<RelaySummary, 'id' | 'name' | 'status' | 'link_capacity_bps' | 'headroom' | 'bandwidth'>
function relay(over: Partial<R> = {}): R {
  return { id: ID, name: 'relay-a', status: 'active', bandwidth: BW, link_capacity_bps: null, headroom: null, ...over }
}
const FULLISH = {
  link_capacity_bps: 125_000_000,
  headroom: { used_bytes_per_sec: 110_000_000, headroom_bytes_per_sec: 15_000_000, utilization: 0.88, over_capacity: false },
}

describe('Headroom display', () => {
  it('no capacity reads "Not configured", never zero', () => {
    render(<HeadroomCell relay={relay()} />)
    expect(screen.getByText('Not configured')).toBeInTheDocument()
  })

  it('the cell shows what is free of the link, flagging a near-full relay', () => {
    render(<HeadroomCell relay={relay({ ...FULLISH, headroom: { ...FULLISH.headroom, utilization: 0.92 } })} />)
    const cell = screen.getByLabelText('Headroom')
    expect(cell).toHaveTextContent('15.0 MB/s free')
    expect(cell).toHaveTextContent('of 1 Gbps')
    expect(within(cell).getByText('92 %')).toBeInTheDocument()
  })

  it('over capacity is flagged', () => {
    render(<HeadroomCell relay={relay({ link_capacity_bps: 100, headroom: { used_bytes_per_sec: 120, headroom_bytes_per_sec: 0, utilization: 1.2, over_capacity: true } })} />)
    expect(screen.getByText('Over capacity')).toBeInTheDocument()
  })

  it('a capacity without a measured rate shows the link but no headroom', () => {
    render(<HeadroomCell relay={relay({ link_capacity_bps: 125_000_000, headroom: null, bandwidth: null })} />)
    expect(screen.getByText('1 Gbps link')).toBeInTheDocument()
  })

  it('the detail shows capacity, use, headroom and utilization separately, as an estimate', () => {
    render(<HeadroomDetail relay={relay(FULLISH)} />)
    expect(screen.getByText('Link capacity').nextElementSibling).toHaveTextContent('1 Gbps')
    expect(screen.getByText('In use (both directions)').nextElementSibling).toHaveTextContent('110 MB/s')
    expect(screen.getByText('Headroom').nextElementSibling).toHaveTextContent('15.0 MB/s')
    expect(screen.getByText('Utilization').nextElementSibling).toHaveTextContent('88 %')
    expect(screen.getByText(/An estimate/)).toBeInTheDocument()
  })
})

const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')

describe('Set link capacity', () => {
  it('sends bytes per second and the reason; relay-ops may do it', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({ [`PUT /provider/relays/${ID}/link-capacity`]: { status: 204 } })
    const onDone = vi.fn()
    render(<RelayCapacityCard relay={relay()} onDone={onDone} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Set link capacity' }))
    const dialog = await screen.findByRole('dialog', { name: 'Set link capacity' })
    const save = within(dialog).getByRole('button', { name: 'Save' })
    await user.type(within(dialog).getByLabelText('Link speed'), '1')
    expect(save).toBeDisabled() // reason missing
    await user.type(within(dialog).getByLabelText('Reason'), ' 1 Gbps uplink ')
    await user.click(save)
    expect(onDone).toHaveBeenCalledWith({ tone: 'success', text: 'Link capacity of relay-a set to 1 Gbps.' })
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'PUT', path: `/provider/relays/${ID}/link-capacity`, body: { bps: 125_000_000, reason: '1 Gbps uplink' } }),
    ])
  })

  it('Clear capacity sends null', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ [`PUT /provider/relays/${ID}/link-capacity`]: { status: 204 } })
    const onDone = vi.fn()
    render(<RelayCapacityCard relay={relay(FULLISH)} onDone={onDone} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Set link capacity' }))
    const dialog = await screen.findByRole('dialog', { name: 'Set link capacity' })
    expect(within(dialog).getByLabelText('Link speed')).toHaveValue('1') // prefilled, 1 Gbps
    await user.type(within(dialog).getByLabelText('Reason'), 'link unknown')
    await user.click(within(dialog).getByRole('button', { name: 'Clear capacity' }))
    expect(onDone).toHaveBeenCalledWith({ tone: 'success', text: 'Link capacity of relay-a cleared.' })
    expect(writes(calls)[0].body).toEqual({ bps: null, reason: 'link unknown' })
  })

  it('a refusal is explained in the dialog', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`PUT /provider/relays/${ID}/link-capacity`]: { status: 400, body: { error: 'invalid_capacity' } } })
    render(<RelayCapacityCard relay={relay()} onDone={vi.fn()} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Set link capacity' }))
    const dialog = await screen.findByRole('dialog', { name: 'Set link capacity' })
    await user.type(within(dialog).getByLabelText('Link speed'), '5')
    await user.type(within(dialog).getByLabelText('Reason'), 'r')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('Enter a link speed above 0 and up to 400 Gbps.')
  })

  it('no Set link capacity on a revoked relay', () => {
    signedInAs(superAdminMe)
    render(<RelayCapacityCard relay={relay({ status: 'revoked' })} onDone={vi.fn()} />)
    expect(screen.queryByRole('button', { name: 'Set link capacity' })).not.toBeInTheDocument()
  })
})
