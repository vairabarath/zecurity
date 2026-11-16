import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { GuidedTour } from './GuidedTour'
import { featureTours, pageTourSeen, resetPageTours } from '@/lib/guidedTours'
import { useGuidedTour } from '@/store/guidedTour'

function Where() {
  return <div data-testid="path">{useLocation().pathname}</div>
}

// Stand-ins for the real pages: just the elements the tours point at.
function SettingsStub() {
  return (
    <div data-tour="security-card">
      <div data-tour="mfa-authenticator">auth</div>
      <div data-tour="mfa-console">console</div>
    </div>
  )
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/settings" element={<SettingsStub />} />
        <Route path="/policies" element={<div data-tour="policies-list">policies</div>} />
        <Route path="/plain" element={<div>no tour here</div>} />
      </Routes>
      <Where />
      <GuidedTour />
    </MemoryRouter>,
  )
}

describe('GuidedTour', () => {
  beforeEach(() => {
    localStorage.clear()
    act(() => useGuidedTour.getState().stop())
  })
  afterEach(() => act(() => useGuidedTour.getState().stop()))

  it('runs a page tour on the first visit only, with Next, Back and Done', async () => {
    renderAt('/settings')
    expect(await screen.findByRole('dialog')).toHaveTextContent('Security')
    expect(screen.getByText('1 of 3')).toBeInTheDocument()
    expect(screen.getByTestId('tour-spotlight')).toBeInTheDocument()
    expect(pageTourSeen('/settings')).toBe(true)

    fireEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByText('Your authenticator app')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Back' }))
    expect(await screen.findByText('1 of 3')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Next' }))
    await screen.findByText('2 of 3')
    fireEvent.click(screen.getByRole('button', { name: 'Next' }))
    await screen.findByText('3 of 3')
    expect(screen.queryByRole('button', { name: 'Skip' })).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Done' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('does not run again once seen, and Skip or Esc ends it', async () => {
    const first = renderAt('/settings')
    await screen.findByRole('dialog')
    fireEvent.click(screen.getByRole('button', { name: 'Skip' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    first.unmount()

    renderAt('/settings')
    await new Promise((r) => setTimeout(r, 300))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()

    resetPageTours()
    act(() => useGuidedTour.getState().start(featureTours.mfa))
    await screen.findByRole('dialog')
    fireEvent.keyDown(window, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('pages without a tour show nothing', async () => {
    renderAt('/plain')
    await new Promise((r) => setTimeout(r, 300))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('a feature tour moves across pages', async () => {
    localStorage.setItem('zecurity.tour.seen.v1:/settings', '1')
    localStorage.setItem('zecurity.tour.seen.v1:/policies', '1')
    renderAt('/plain')
    act(() => useGuidedTour.getState().start(featureTours.mfa))
    await waitFor(() => expect(screen.getByTestId('path')).toHaveTextContent('/settings'))
    await screen.findByText(`1 of ${featureTours.mfa.length}`)
    for (let i = 0; i < 3; i++) fireEvent.click(await screen.findByRole('button', { name: 'Next' }))
    await waitFor(() => expect(screen.getByTestId('path')).toHaveTextContent('/policies'))
    expect(await screen.findByText('Resource policies')).toBeInTheDocument()
  })
})
