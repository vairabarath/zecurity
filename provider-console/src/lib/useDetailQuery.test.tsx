import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { StrictMode, useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '@/api/client'
import { useDetailQuery } from './useDetailQuery'

type Fetch = (id: string) => Promise<string>

function Harness({ fetchOne, initialId = 'a' }: { fetchOne: Fetch; initialId?: string }) {
  const [id, setId] = useState(initialId)
  const { state, reload } = useDetailQuery(id, fetchOne)
  return (
    <div>
      <p data-testid="status">{state.status}</p>
      <p data-testid="data">{state.status === 'ready' ? state.data : ''}</p>
      <button onClick={() => setId('b')}>open b</button>
      <button onClick={reload}>reload</button>
    </div>
  )
}

const n = (f: Fetch) => (f as ReturnType<typeof vi.fn>).mock.calls.length

describe('useDetailQuery', () => {
  it('one request per id under StrictMode; an id change is one new request', async () => {
    const f = vi.fn(async (id: string) => `detail ${id}`) as unknown as Fetch
    render(
      <StrictMode>
        <Harness fetchOne={f} />
      </StrictMode>,
    )
    await screen.findByText('detail a')
    expect(n(f)).toBe(1)
    await userEvent.setup().click(screen.getByText('open b'))
    await screen.findByText('detail b')
    expect(n(f)).toBe(2)
  })

  it('reload is the only re-request and sends exactly one', async () => {
    const f = vi.fn(async () => 'x') as unknown as Fetch
    render(<Harness fetchOne={f} />)
    await screen.findByText('x')
    act(() => {
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
      window.dispatchEvent(new Event('online'))
    })
    await new Promise((r) => setTimeout(r, 30))
    expect(n(f)).toBe(1)
    await userEvent.setup().click(screen.getByText('reload'))
    await vi.waitFor(() => expect(n(f)).toBe(2))
  })

  it.each([
    [404, 'not_found'],
    [400, 'not_found'],
    [403, 'forbidden'],
    [500, 'error'],
  ])('HTTP %i → %s, with no automatic retry', async (status, want) => {
    const f = vi.fn(async () => {
      throw new ApiError({ status, code: 'x' })
    }) as unknown as Fetch
    render(<Harness fetchOne={f} />)
    await vi.waitFor(() => expect(screen.getByTestId('status').textContent).toBe(want))
    await new Promise((r) => setTimeout(r, 30))
    expect(n(f)).toBe(1)
  })

  it('a session-ending response maps to session_ended', async () => {
    const f = vi.fn(async () => {
      throw new ApiError({ status: 401, code: 'provider session revoked', sessionEnded: true })
    }) as unknown as Fetch
    render(<Harness fetchOne={f} />)
    await vi.waitFor(() => expect(screen.getByTestId('status').textContent).toBe('session_ended'))
  })

  it('a slow response for a previous id never replaces the current one', async () => {
    let releaseA!: () => void
    const f = vi.fn(async (id: string) => {
      if (id === 'a') await new Promise<void>((r) => (releaseA = r))
      return `detail ${id}`
    }) as unknown as Fetch
    render(<Harness fetchOne={f} />)
    await userEvent.setup().click(screen.getByText('open b'))
    await screen.findByText('detail b')
    await act(async () => releaseA())
    expect(screen.getByTestId('data').textContent).toBe('detail b')
  })
})
