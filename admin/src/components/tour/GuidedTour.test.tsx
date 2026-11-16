import { useState } from 'react'
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

// Task walk-throughs: steps find elements by visible text, and a `click`
// step presses its target on Next (here: opens a dialog, then highlights
// its field and leaves it open).
function UsersStub() {
  const [open, setOpen] = useState(false)
  return (
    <div>
      <button type="button" onClick={() => setOpen(true)}>Add User</button>
      {open && (
        <div role="dialog">
          <label htmlFor="invite-email">Email address</label>
          <input id="invite-email" />
          <button type="submit">Send invite</button>
        </div>
      )}
    </div>
  )
}

describe('GuidedTour: task walk-throughs', () => {
  beforeEach(() => {
    localStorage.clear()
    localStorage.setItem('zecurity.tour.seen.v1:/users', '1')
    act(() => useGuidedTour.getState().stop())
  })
  afterEach(() => act(() => useGuidedTour.getState().stop()))

  it('clicks Add User on Next, then points at the dialog field and leaves it open', async () => {
    render(
      <MemoryRouter initialEntries={['/users']}>
        <Routes>
          <Route path="/users" element={<UsersStub />} />
        </Routes>
        <GuidedTour />
      </MemoryRouter>,
    )
    act(() => useGuidedTour.getState().start(featureTours['add-user']))
    expect(await screen.findByText('Add a user')).toBeInTheDocument()
    expect(screen.queryByLabelText('Email address')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByText('Their email address')).toBeInTheDocument()
    expect(screen.getByLabelText('Email address')).toBeInTheDocument()
    // Back cannot undo the click the tour made.
    expect(screen.queryByRole('button', { name: 'Back' })).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByText('Send the invite')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Done' }))
    await waitFor(() => expect(screen.queryByText('Send the invite')).not.toBeInTheDocument())
    expect(screen.getByLabelText('Email address')).toBeInTheDocument()
  })

  it('every walk-through step names exactly one way to find its element', () => {
    for (const [name, steps] of Object.entries(featureTours)) {
      for (const s of steps) {
        const ways = [s.target, s.selector, s.button, s.field].filter(Boolean)
        expect(ways, `${name}: ${s.title}`).toHaveLength(1)
      }
    }
  })
})

describe('GuidedTour: a click step whose button is unavailable', () => {
  beforeEach(() => {
    localStorage.clear()
    localStorage.setItem('zecurity.tour.seen.v1:/users', '1')
    act(() => useGuidedTour.getState().stop())
  })
  afterEach(() => act(() => useGuidedTour.getState().stop()))

  it('says so and skips the steps inside what it would open', async () => {
    render(
      <MemoryRouter initialEntries={['/x']}>
        <Routes>
          <Route path="/x" element={<div><button disabled>Add Member</button><button>Assign Resource</button></div>} />
        </Routes>
        <GuidedTour />
      </MemoryRouter>,
    )
    act(() =>
      useGuidedTour.getState().start([
        { button: 'Add Member', click: true, title: 'Add members', body: '' },
        { field: 'User', title: 'Pick a user', body: '' },
        { button: 'Assign Resource', click: true, title: 'Assign resources', body: '' },
      ]),
    )
    expect(await screen.findByText(/turned off right now/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByText('Assign resources')).toBeInTheDocument()
    expect(screen.queryByText('Pick a user')).not.toBeInTheDocument()
  })
})
