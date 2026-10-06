import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { ApiError } from '@/api/client'

// Shared state for the paginated read pages (relays, tenants, audit,
// certificates). NOT for tenant detail: that fetch writes an audit row and
// is owned by its page.
//
// Filters live in the URL query string (allowlisted keys only), so reload,
// back/forward and shared links restore them. The cursor stack lives in
// memory and belongs to one filter set: when the filters change (by the UI
// or by back/forward) the stack is dropped and paging restarts at page 1, so
// a cursor issued under other filters is never sent.
//
// Requests happen only when the filters or page change, or on an explicit
// retry(). There is no polling, no refetch on focus/visibility, no automatic
// retry, and StrictMode's double effect run does not send a second request.

export type Filters = Record<string, string>

export type PagedState<R> =
  | { status: 'loading' }
  | { status: 'ready'; data: R }
  | { status: 'invalid' } // a filter value the page or the server rejected (400)
  | { status: 'forbidden' } // a role 403 (the session is still valid)
  | { status: 'error'; error: unknown }
  | { status: 'session_ended' } // the client already ended the session; guards show Login

export interface PagedQueryOptions<R> {
  /** The only URL keys read or written. */
  filterKeys: readonly string[]
  /** Page-level validation of URL values; false → 'invalid' with no request. */
  validate?: (filters: Filters) => boolean
  /** Fetch one page. `cursor` is undefined for page 1. */
  fetchPage: (filters: Filters, cursor: string | undefined) => Promise<R>
}

export interface PagedQuery<R> {
  filters: Filters
  state: PagedState<R>
  /** 1-based page number within the current filter set. */
  page: number
  hasPrev: boolean
  hasNext: boolean
  next: () => void
  prev: () => void
  /** Replace the filters (pushes a history entry, restarts at page 1). */
  setFilters: (filters: Filters) => void
  /** Clear every filter. */
  resetFilters: () => void
  /** Explicit user action: re-request the current page once. */
  retry: () => void
}

/** Allowlisted, non-empty filters in a canonical (sorted) order. */
export function readFilters(sp: URLSearchParams, keys: readonly string[]): Filters {
  const out: Filters = {}
  for (const k of [...keys].sort()) {
    const v = sp.get(k)
    if (v !== null && v !== '') out[k] = v
  }
  return out
}

function filtersKey(f: Filters): string {
  return JSON.stringify(f)
}

type Result<R> = { key: string; state: PagedState<R> }

export function usePagedQuery<R extends { next_cursor: string | null }>(opts: PagedQueryOptions<R>): PagedQuery<R> {
  const { filterKeys, validate, fetchPage } = opts
  const [searchParams, setSearchParams] = useSearchParams()
  const filters = useMemo(() => readFilters(searchParams, filterKeys), [searchParams, filterKeys])
  const fkey = filtersKey(filters)
  const valid = validate ? validate(filters) : true

  // The cursor stack is tagged with the filter set it belongs to; under any
  // other filters it reads as page 1. stack[i] is the cursor that fetched
  // page i+1 (page 1 has none).
  const [paging, setPaging] = useState<{ fkey: string; stack: (string | undefined)[] }>({ fkey, stack: [undefined] })
  // Any filter change (UI, back/forward, edited URL) discards the old stack
  // for good, so returning to an earlier filter set starts at page 1 again.
  if (paging.fkey !== fkey) setPaging({ fkey, stack: [undefined] })
  const stack = useMemo(() => (paging.fkey === fkey ? paging.stack : [undefined]), [paging, fkey])
  const cursor = stack[stack.length - 1]

  const [nonce, setNonce] = useState(0)
  const reqKey = `${fkey}|${stack.length}|${cursor ?? ''}|${nonce}`
  const [result, setResult] = useState<Result<R> | undefined>(undefined)

  // Guards against StrictMode's second effect run and stale responses:
  // a request is sent once per reqKey, and only the latest reqKey's
  // response is applied.
  const sentRef = useRef<string | undefined>(undefined)
  const latestRef = useRef(reqKey)
  const fetchRef = useRef(fetchPage)
  // Layout effects run before the fetch effect below, so it always sees the
  // current key and fetcher.
  useLayoutEffect(() => {
    latestRef.current = reqKey
    fetchRef.current = fetchPage
  })

  useEffect(() => {
    if (!valid || sentRef.current === reqKey) return
    sentRef.current = reqKey
    const key = reqKey
    fetchRef.current(filters, cursor).then(
      (data) => {
        if (latestRef.current === key) setResult({ key, state: { status: 'ready', data } })
      },
      (err: unknown) => {
        if (latestRef.current !== key) return
        let state: PagedState<R>
        if (err instanceof ApiError && err.sessionEnded) state = { status: 'session_ended' }
        else if (err instanceof ApiError && err.status === 400) state = { status: 'invalid' }
        else if (err instanceof ApiError && err.status === 403) state = { status: 'forbidden' }
        else state = { status: 'error', error: err }
        setResult({ key, state })
      },
    )
    // filters and cursor are fully described by reqKey.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reqKey, valid])

  let state: PagedState<R>
  if (!valid) state = { status: 'invalid' }
  else if (result && result.key === reqKey) state = result.state
  else state = { status: 'loading' }

  const nextCursor = state.status === 'ready' ? state.data.next_cursor : null

  const next = useCallback(() => {
    if (!nextCursor) return
    setPaging({ fkey, stack: [...stack, nextCursor] })
  }, [fkey, stack, nextCursor])

  const prev = useCallback(() => {
    if (stack.length <= 1) return
    setPaging({ fkey, stack: stack.slice(0, -1) })
  }, [fkey, stack])

  const setFilters = useCallback(
    (f: Filters) => {
      const sp = new URLSearchParams()
      for (const [k, v] of Object.entries(readFilters(new URLSearchParams(f), filterKeys))) sp.set(k, v)
      setSearchParams(sp) // push: back/forward walk through filter sets
    },
    [filterKeys, setSearchParams],
  )

  const resetFilters = useCallback(() => setSearchParams(new URLSearchParams()), [setSearchParams])
  const retry = useCallback(() => setNonce((n) => n + 1), [])

  return {
    filters,
    state,
    page: stack.length,
    hasPrev: stack.length > 1,
    hasNext: nextCursor !== null,
    next,
    prev,
    setFilters,
    resetFilters,
    retry,
  }
}
