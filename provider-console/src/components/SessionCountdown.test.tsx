import { act, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { useSessionStore } from '@/auth/session'
import { SessionCountdown } from './SessionCountdown'

describe('SessionCountdown', () => {
  it('renders nothing without a session', () => {
    const { container } = render(<SessionCountdown />)
    expect(container).toBeEmptyDOMElement()
  })

  it('counts down every second and warns in the last two minutes', () => {
    vi.useFakeTimers()
    useSessionStore.getState().startSession('t', 900, false)
    render(<SessionCountdown />)
    const value = () => screen.getByLabelText('Session time remaining')
    expect(value()).toHaveTextContent('15:00')
    expect(value().parentElement).not.toHaveAttribute('data-warning')

    act(() => {
      vi.advanceTimersByTime(1000)
    })
    expect(value()).toHaveTextContent('14:59')

    act(() => {
      vi.advanceTimersByTime(780_000)
    })
    expect(value()).toHaveTextContent('1:59')
    expect(value().parentElement).toHaveAttribute('data-warning', 'true')
  })
})
