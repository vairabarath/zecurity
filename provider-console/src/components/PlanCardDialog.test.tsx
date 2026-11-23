import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '@/api/client'
import { PlanCardDialog } from '@/components/PlanCardDialog'
import { FREE_LIMITS, planCard } from '@/test/plans'

const REASON = 'Pricing for Q1'

async function type(label: string | RegExp, text: string) {
  const el = screen.getByLabelText(label)
  await userEvent.clear(el)
  if (text) await userEvent.type(el, text)
}

describe('PlanCardDialog: create', () => {
  it('creates a not-billable card by default, in one call with the reason', async () => {
    const onCreate = vi.fn().mockResolvedValue(undefined)
    const onDone = vi.fn()
    render(<PlanCardDialog mode="create" currency="USD" onCreate={onCreate} onCancel={vi.fn()} onDone={onDone} />)
    expect(screen.getByText('Not billable: tenants on this card get no invoice.')).toBeInTheDocument()
    const save = screen.getByRole('button', { name: 'Create card' })
    await type('Name', 'Starter')
    expect(save).toBeDisabled() // reason missing; limits default to unlimited
    await type('Reason', REASON)
    await userEvent.click(save)
    expect(onCreate).toHaveBeenCalledWith(
      { name: 'Starter', description: '', limits: expect.objectContaining({ users: null }), price: null },
      REASON,
    )
    expect(onDone).toHaveBeenCalledWith('Plan card Starter created. Assign it to tenants from their tenant page.')
  })

  it('a billable card sends integer minor units for the fee and each billed meter', async () => {
    const onCreate = vi.fn().mockResolvedValue(undefined)
    render(<PlanCardDialog mode="create" currency="USD" onCreate={onCreate} onCancel={vi.fn()} onDone={vi.fn()} />)
    await type('Name', 'Team')
    await userEvent.click(screen.getByLabelText('Bill tenants on this card'))
    await type('Monthly fee (USD)', '19.99')
    await userEvent.click(screen.getByLabelText('Bill relay traffic to clients'))
    await type(/^Included$/, '50')
    await type('Price per GB (USD)', '0.29')
    await type('Reason', REASON)
    await userEvent.click(screen.getByRole('button', { name: 'Create card' }))
    expect(onCreate.mock.calls[0][0].price).toEqual({
      currency: 'USD',
      base_fee_minor: 1999,
      meters: { relay_bytes_to_clients: { included: 50_000_000_000, unit: 1_000_000_000, unit_price_minor: 29 } },
    })
  })

  it('a bad amount is shown on its field and blocks saving', async () => {
    render(<PlanCardDialog mode="create" currency="USD" onCreate={vi.fn()} onCancel={vi.fn()} onDone={vi.fn()} />)
    await type('Name', 'Team')
    await userEvent.click(screen.getByLabelText('Bill tenants on this card'))
    await type('Monthly fee (USD)', '19.999')
    await type('Reason', REASON)
    expect(screen.getByLabelText('Monthly fee (USD)')).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByText('Enter an amount in USD, like 20 or 19.99.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Create card' })).toBeDisabled()
  })

  it('keeps the dialog open with the error when the save fails', async () => {
    const onCreate = vi.fn().mockRejectedValue(new ApiError({ status: 409, code: 'plan_name_taken' }))
    const onDone = vi.fn()
    render(<PlanCardDialog mode="create" currency="USD" onCreate={onCreate} onCancel={vi.fn()} onDone={onDone} />)
    await type('Name', 'Team')
    await type('Reason', REASON)
    await userEvent.click(screen.getByRole('button', { name: 'Create card' }))
    expect(await screen.findByRole('alert')).toBeInTheDocument()
    expect(onDone).not.toHaveBeenCalled()
  })
})

describe('PlanCardDialog: edit', () => {
  it('says who is affected and how price changes reach invoices', () => {
    render(<PlanCardDialog mode="edit" plan={planCard()} currency="INR" onUpdate={vi.fn()} onCancel={vi.fn()} onDone={vi.fn()} />)
    expect(
      screen.getByText(/4 tenants use this card\. Limits change at once\. Price changes apply to this month’s invoices at month close/),
    ).toBeInTheDocument()
    expect(screen.getByText(/Issued invoices don’t change\./)).toBeInTheDocument()
  })

  it('sends only the price when only an included quantity changes', async () => {
    const onUpdate = vi.fn().mockResolvedValue(undefined)
    render(<PlanCardDialog mode="edit" plan={planCard()} currency="INR" onUpdate={onUpdate} onCancel={vi.fn()} onDone={vi.fn()} />)
    const save = screen.getByRole('button', { name: 'Save card' })
    await type('Reason', REASON)
    expect(save).toBeDisabled() // nothing changed yet
    // the first billed meter is relay traffic to clients (50 GB)
    const included = screen.getAllByLabelText(/^Included$/)[0]
    await userEvent.clear(included)
    await userEvent.type(included, '80')
    await userEvent.click(save)
    expect(onUpdate).toHaveBeenCalledWith(
      {
        price: {
          currency: 'INR',
          base_fee_minor: 100000,
          meters: {
            relay_bytes_to_clients: { included: 80_000_000_000, unit: 1_000_000_000, unit_price_minor: 800 },
            active_users: { included: 5, unit: 1, unit_price_minor: 30000 },
          },
        },
      },
      REASON,
    )
  })

  it('a limits-only change leaves the price out; turning billing off sends null', async () => {
    const onUpdate = vi.fn().mockResolvedValue(undefined)
    const { unmount } = render(
      <PlanCardDialog mode="edit" plan={planCard()} currency="INR" onUpdate={onUpdate} onCancel={vi.fn()} onDone={vi.fn()} />,
    )
    const users = screen.getByLabelText('Users')
    await userEvent.clear(users)
    await userEvent.type(users, '9')
    await type('Reason', REASON)
    await userEvent.click(screen.getByRole('button', { name: 'Save card' }))
    expect(onUpdate).toHaveBeenLastCalledWith({ limits: { ...FREE_LIMITS, users: 9 } }, REASON)
    unmount()

    render(<PlanCardDialog mode="edit" plan={planCard()} currency="INR" onUpdate={onUpdate} onCancel={vi.fn()} onDone={vi.fn()} />)
    await userEvent.click(screen.getByLabelText('Bill tenants on this card'))
    await type('Reason', REASON)
    await userEvent.click(screen.getByRole('button', { name: 'Save card' }))
    expect(onUpdate).toHaveBeenLastCalledWith({ price: null }, REASON)
  })
})

describe('PlanCardDialog: duplicate', () => {
  it('prefills every field from the source card and creates a new card', async () => {
    const onCreate = vi.fn().mockResolvedValue(undefined)
    const source = planCard()
    render(<PlanCardDialog mode="duplicate" plan={source} currency="INR" onCreate={onCreate} onCancel={vi.fn()} onDone={vi.fn()} />)
    expect(screen.getByLabelText('Name')).toHaveValue('Team (copy)')
    expect(screen.getByLabelText('Monthly fee (INR)')).toHaveValue('1000.00')
    await type('Name', 'Acme deal')
    await type('Reason', REASON)
    await userEvent.click(screen.getByRole('button', { name: 'Create card' }))
    expect(onCreate).toHaveBeenCalledWith(
      { name: 'Acme deal', description: source.description, limits: source.limits, price: source.price },
      REASON,
    )
  })
})
