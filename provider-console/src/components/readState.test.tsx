import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import type { PagedState } from '@/lib/usePagedQuery'
import { DataState } from './DataState'
import { ExpiryBadge } from './ExpiryBadge'
import { Pager } from './Pager'
import { StatusBadge } from './StatusBadge'

type Data = { items: string[] }

function renderState(state: PagedState<Data>) {
  const onRetry = vi.fn()
  const onReset = vi.fn()
  render(
    <MemoryRouter initialEntries={['/list']}>
      <Routes>
        <Route path="/forbidden" element={<p>forbidden page</p>} />
        <Route
          path="/list"
          element={
            <DataState
              state={state}
              what="relays"
              emptyMessage="No relays yet."
              isEmpty={(d) => d.items.length === 0}
              onRetry={onRetry}
              onReset={onReset}
            >
              {(d) => <p>rows: {d.items.join(',')}</p>}
            </DataState>
          }
        />
      </Routes>
    </MemoryRouter>,
  )
  return { onRetry, onReset }
}

describe('DataState', () => {
  it('loading', () => {
    renderState({ status: 'loading' })
    expect(screen.getByLabelText('Loading relays')).toHaveAttribute('aria-busy', 'true')
  })

  it('ready with data renders the children', () => {
    renderState({ status: 'ready', data: { items: ['a', 'b'] } })
    expect(screen.getByText('rows: a,b')).toBeInTheDocument()
  })

  it('ready but empty shows the empty message', () => {
    renderState({ status: 'ready', data: { items: [] } })
    expect(screen.getByText('No relays yet.')).toBeInTheDocument()
  })

  it('error offers an explicit Retry and nothing else', async () => {
    const { onRetry, onReset } = renderState({ status: 'error', error: new Error('x') })
    expect(screen.getByText('Couldn’t load relays.')).toBeInTheDocument()
    expect(onRetry).not.toHaveBeenCalled()
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
    expect(onRetry).toHaveBeenCalledTimes(1)
    expect(onReset).not.toHaveBeenCalled()
  })

  it('invalid offers Reset filters', async () => {
    const { onReset } = renderState({ status: 'invalid' })
    expect(screen.getByText(/Invalid filter/)).toBeInTheDocument()
    await userEvent.setup().click(screen.getByRole('button', { name: 'Reset filters' }))
    expect(onReset).toHaveBeenCalledTimes(1)
  })

  it('a role 403 goes to the Forbidden page', () => {
    renderState({ status: 'forbidden' })
    expect(screen.getByText('forbidden page')).toBeInTheDocument()
  })

  it('a session-ending response renders nothing (the guards show Login)', () => {
    renderState({ status: 'session_ended' })
    expect(screen.queryByText(/relays|rows/)).not.toBeInTheDocument()
  })
})

describe('StatusBadge', () => {
  it.each([
    ['active', 'bg-secure'],
    ['revoked', 'bg-destructive'],
    ['inactive', 'bg-warning'],
    ['deleted', 'bg-muted'],
    ['pending', 'border'],
    ['some-new-status', 'border'],
  ])('%s', (status, cls) => {
    render(<StatusBadge status={status} />)
    const el = screen.getByText(status)
    expect(el).toHaveAttribute('data-status', status)
    expect(el.className).toContain(cls)
  })
})

describe('ExpiryBadge', () => {
  const now = new Date('2026-10-06T12:00:00Z')
  it('computes the bucket from the date when none is given', () => {
    render(<ExpiryBadge notAfter="2026-10-08T12:00:00Z" now={now} />)
    expect(screen.getByText('< 7 days')).toHaveAttribute('data-bucket', 'lt_7d')
  })

  it("uses the server's bucket when given", () => {
    render(<ExpiryBadge notAfter="2026-10-08T12:00:00Z" bucket="ok" now={now} />)
    expect(screen.getByText('OK')).toHaveAttribute('data-bucket', 'ok')
  })

  it('shows — for a missing or invalid date', () => {
    const { container } = render(
      <>
        <ExpiryBadge notAfter={null} />
        <ExpiryBadge notAfter="nope" />
      </>,
    )
    expect(container.textContent).toBe('——')
  })
})

describe('Pager', () => {
  it('renders nothing for a single page', () => {
    const { container } = render(<Pager page={1} hasPrev={false} hasNext={false} onPrev={vi.fn()} onNext={vi.fn()} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('enables only the possible directions and reports the page', async () => {
    const onNext = vi.fn()
    const onPrev = vi.fn()
    render(<Pager page={2} hasPrev hasNext={false} onPrev={onPrev} onNext={onNext} />)
    expect(screen.getByText('Page 2')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Next/ })).toBeDisabled()
    await userEvent.setup().click(screen.getByRole('button', { name: /Previous/ }))
    expect(onPrev).toHaveBeenCalledTimes(1)
    expect(onNext).not.toHaveBeenCalled()
  })
})
