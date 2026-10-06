import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { StrictMode } from 'react'
import { MemoryRouter, useLocation, useNavigate } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '@/api/client'
import { readFilters, usePagedQuery, type Filters } from './usePagedQuery'

interface FakePage {
  items: string[]
  next_cursor: string | null
}

type Fetch = (filters: Filters, cursor: string | undefined) => Promise<FakePage>

const KEYS = ['status', 'q'] as const

// A three-page dataset per filter set: cursors are tagged with the filters
// that produced them, so a cursor reused across filter sets is detectable.
function dataset(): Fetch {
  return vi.fn(async (filters: Filters, cursor?: string) => {
    const tag = JSON.stringify(filters)
    const page = cursor ? Number(cursor.split('#')[1]) : 1
    if (cursor && !cursor.startsWith(tag)) throw new Error(`cursor ${cursor} reused under ${tag}`)
    return { items: [`${tag} page ${page}`], next_cursor: page < 3 ? `${tag}#${page + 1}` : null }
  })
}

function Harness({ fetchPage, validate }: { fetchPage: Fetch; validate?: (f: Filters) => boolean }) {
  const q = usePagedQuery<FakePage>({ filterKeys: KEYS, fetchPage, validate })
  const location = useLocation()
  const navigate = useNavigate()
  return (
    <div>
      <p data-testid="status">{q.state.status}</p>
      <p data-testid="items">{q.state.status === 'ready' ? q.state.data.items.join(',') : ''}</p>
      <p data-testid="page">{q.page}</p>
      <p data-testid="search">{location.search}</p>
      <p data-testid="filters">{JSON.stringify(q.filters)}</p>
      <button onClick={q.next} disabled={!q.hasNext}>next</button>
      <button onClick={q.prev} disabled={!q.hasPrev}>prev</button>
      <button onClick={() => q.setFilters({ status: 'inactive' })}>filter inactive</button>
      <button onClick={() => q.setFilters({ status: 'active', q: '', token: 'tok-secret' })}>filter active</button>
      <button onClick={q.resetFilters}>reset</button>
      <button onClick={q.retry}>retry</button>
      <button onClick={() => navigate(-1)}>back</button>
      <button onClick={() => navigate(1)}>forward</button>
    </div>
  )
}

function renderHarness(fetchPage: Fetch, path = '/x', opts: { strict?: boolean; validate?: (f: Filters) => boolean } = {}) {
  const tree = (
    <MemoryRouter initialEntries={[path]}>
      <Harness fetchPage={fetchPage} validate={opts.validate} />
    </MemoryRouter>
  )
  return render(opts.strict ? <StrictMode>{tree}</StrictMode> : tree)
}

const text = (id: string) => screen.getByTestId(id).textContent
const calls = (f: Fetch) => (f as ReturnType<typeof vi.fn>).mock.calls as [Filters, string | undefined][]

describe('readFilters', () => {
  it('keeps only allowlisted, non-empty keys in a canonical order', () => {
    const sp = new URLSearchParams('token=abc&q=x&status=&password=p&status2=y')
    expect(readFilters(sp, KEYS)).toEqual({ q: 'x' })
  })
})

describe('usePagedQuery', () => {
  it('loads page 1 from the URL filters, ignoring other URL keys', async () => {
    const f = dataset()
    renderHarness(f, '/x?status=active&token=abc&details=x')
    expect(text('status')).toBe('loading')
    expect(await screen.findByText('{"status":"active"} page 1')).toBeInTheDocument()
    expect(calls(f)).toEqual([[{ status: 'active' }, undefined]])
  })

  it('Next walks the server cursors; Previous uses the client stack', async () => {
    const user = userEvent.setup()
    const f = dataset()
    renderHarness(f)
    await screen.findByText('{} page 1')
    await user.click(screen.getByText('next'))
    await screen.findByText('{} page 2')
    await user.click(screen.getByText('next'))
    await screen.findByText('{} page 3')
    expect(text('page')).toBe('3')
    expect(screen.getByText('next')).toBeDisabled() // last page: no server cursor
    await user.click(screen.getByText('prev'))
    await screen.findByText('{} page 2')
    await user.click(screen.getByText('prev'))
    await screen.findByText('{} page 1')
    expect(screen.getByText('prev')).toBeDisabled()
    expect(calls(f).map(([, c]) => c ?? null)).toEqual([null, '{}#2', '{}#3', '{}#2', null])
  })

  it('a filter change clears the cursor stack and restarts at page 1', async () => {
    const user = userEvent.setup()
    const f = dataset()
    renderHarness(f)
    await screen.findByText('{} page 1')
    await user.click(screen.getByText('next'))
    await screen.findByText('{} page 2')
    await user.click(screen.getByText('filter inactive'))
    expect(await screen.findByText('{"status":"inactive"} page 1')).toBeInTheDocument()
    expect(text('page')).toBe('1')
    expect(text('search')).toBe('?status=inactive')
    expect(calls(f).at(-1)).toEqual([{ status: 'inactive' }, undefined]) // no foreign cursor sent
  })

  it('setFilters writes only allowlisted, non-empty keys to the URL', async () => {
    const user = userEvent.setup()
    const f = dataset()
    renderHarness(f)
    await screen.findByText('{} page 1')
    await user.click(screen.getByText('filter active'))
    await screen.findByText('{"status":"active"} page 1')
    expect(text('search')).toBe('?status=active')
    expect(text('search')).not.toContain('tok-secret')
  })

  it('back/forward restore the URL filters and restart paging for that filter set', async () => {
    const user = userEvent.setup()
    const f = dataset()
    renderHarness(f, '/x?status=active')
    await screen.findByText('{"status":"active"} page 1')
    await user.click(screen.getByText('next'))
    await screen.findByText('{"status":"active"} page 2')
    await user.click(screen.getByText('filter inactive'))
    await screen.findByText('{"status":"inactive"} page 1')
    await user.click(screen.getByText('next'))
    await screen.findByText('{"status":"inactive"} page 2')

    await user.click(screen.getByText('back'))
    expect(await screen.findByText('{"status":"active"} page 1')).toBeInTheDocument()
    expect(text('search')).toBe('?status=active')
    expect(text('page')).toBe('1')

    await user.click(screen.getByText('forward'))
    expect(await screen.findByText('{"status":"inactive"} page 1')).toBeInTheDocument()
    expect(text('page')).toBe('1')
    // Every request used either no cursor or one issued under its own filters
    // (the fake fetch throws otherwise).
    expect(calls(f).every(([fl, c]) => !c || c.startsWith(JSON.stringify(fl)))).toBe(true)
  })

  it('invalid URL values make no request; reset clears them and loads', async () => {
    const user = userEvent.setup()
    const f = dataset()
    renderHarness(f, '/x?status=bogus', { validate: (fl) => !fl.status || ['active', 'inactive'].includes(fl.status) })
    expect(text('status')).toBe('invalid')
    expect(calls(f)).toHaveLength(0)
    await user.click(screen.getByText('reset'))
    await screen.findByText('{} page 1')
    expect(text('search')).toBe('')
    expect(calls(f)).toHaveLength(1)
  })

  it.each([
    ['a 400', new ApiError({ status: 400, code: 'invalid_cursor' }), 'invalid'],
    ['a role 403', new ApiError({ status: 403, code: 'forbidden' }), 'forbidden'],
    ['a 500', new ApiError({ status: 500, code: 'server_error' }), 'error'],
    ['a network failure', new ApiError({ status: 0, code: 'network_error' }), 'error'],
    ['a session-ending 401', new ApiError({ status: 401, code: 'provider session revoked', sessionEnded: true }), 'session_ended'],
  ])('%s maps to %s', async (_n, err, want) => {
    const f = vi.fn(async () => {
      throw err
    }) as unknown as Fetch
    renderHarness(f)
    await vi.waitFor(() => expect(text('status')).toBe(want))
  })

  it('Retry is the only way to re-request after an error, and sends exactly one request', async () => {
    const user = userEvent.setup()
    let fail = true
    const f = vi.fn(async () => {
      if (fail) throw new ApiError({ status: 500, code: 'server_error' })
      return { items: ['ok'], next_cursor: null }
    }) as unknown as Fetch
    renderHarness(f)
    await vi.waitFor(() => expect(text('status')).toBe('error'))
    await new Promise((r) => setTimeout(r, 50))
    expect(calls(f)).toHaveLength(1) // no automatic retry
    fail = false
    await user.click(screen.getByText('retry'))
    await screen.findByText('ok')
    expect(calls(f)).toHaveLength(2)
  })

  it('StrictMode does not send a second request', async () => {
    const f = dataset()
    renderHarness(f, '/x', { strict: true })
    await screen.findByText('{} page 1')
    expect(calls(f)).toHaveLength(1)
  })

  it('never refetches on focus, visibility or reconnect', async () => {
    const f = dataset()
    renderHarness(f)
    await screen.findByText('{} page 1')
    act(() => {
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
      window.dispatchEvent(new Event('online'))
    })
    await new Promise((r) => setTimeout(r, 50))
    expect(calls(f)).toHaveLength(1)
  })

  it('a slow response for old filters never overwrites the current page', async () => {
    const user = userEvent.setup()
    let releaseOld!: () => void
    const f = vi.fn(async (filters: Filters) => {
      if (!filters.status) {
        await new Promise<void>((r) => (releaseOld = r))
        return { items: ['OLD'], next_cursor: null }
      }
      return { items: ['NEW'], next_cursor: null }
    }) as unknown as Fetch
    renderHarness(f)
    await user.click(screen.getByText('filter inactive'))
    await screen.findByText('NEW')
    await act(async () => releaseOld())
    expect(screen.getByTestId('items').textContent).toBe('NEW')
  })
})
