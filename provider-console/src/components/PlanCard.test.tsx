import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { PlanCard } from '@/components/PlanCard'
import { planCard } from '@/test/plans'

describe('PlanCard', () => {
  it('shows the monthly price, each priced meter and the limits', () => {
    render(<PlanCard plan={planCard()} />)
    const card = screen.getByRole('article', { name: 'Team' })
    expect(within(card).getByTestId('plan-price')).toHaveTextContent('INR 1,000.00per month')
    const prices = within(card).getByRole('list', { name: 'Usage prices' })
    expect(prices).toHaveTextContent('Relay traffic to clients50 GB included, then INR 8.00 per GB')
    expect(prices).toHaveTextContent('Active users5 users included, then INR 300.00 per user')
    expect(within(prices).getAllByRole('listitem')).toHaveLength(2) // unpriced meters are left out
    expect(card.querySelector('[data-limit="users"]')).toHaveTextContent('5')
    expect(within(card).getByText('4 tenants · revision 3')).toBeInTheDocument()
  })

  it('says when a card is not billable', () => {
    render(<PlanCard plan={planCard({ price: null, is_default: true, tenants: 1 })} />)
    expect(screen.getByTestId('plan-price')).toHaveTextContent('Not billable. Tenants on this plan get no invoice.')
    expect(screen.queryByRole('list', { name: 'Usage prices' })).toBeNull()
    expect(screen.getByText('Default')).toBeInTheDocument()
    expect(screen.getByText('1 tenant · revision 3')).toBeInTheDocument()
  })

  it('a base fee only card lists no usage prices', () => {
    render(<PlanCard plan={planCard({ price: { currency: 'USD', base_fee_minor: 2000, meters: {} } })} />)
    expect(screen.getByTestId('plan-price')).toHaveTextContent('USD 20.00per month')
    expect(screen.queryByRole('list', { name: 'Usage prices' })).toBeNull()
  })

  it('shows only the actions it is given, each named for the card', async () => {
    const onEdit = vi.fn()
    const onDuplicate = vi.fn()
    const { rerender } = render(<PlanCard plan={planCard()} />)
    expect(screen.queryByRole('button')).toBeNull()
    rerender(<PlanCard plan={planCard()} onEdit={onEdit} onDuplicate={onDuplicate} />)
    await userEvent.click(screen.getByRole('button', { name: 'Edit Team' }))
    await userEvent.click(screen.getByRole('button', { name: 'Duplicate Team' }))
    expect(onEdit).toHaveBeenCalledOnce()
    expect(onDuplicate).toHaveBeenCalledOnce()
    expect(screen.queryByRole('button', { name: 'History of Team' })).toBeNull()
  })
})
