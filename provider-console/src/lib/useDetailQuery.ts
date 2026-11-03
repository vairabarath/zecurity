import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { ApiError } from '@/api/client'

// One explicit fetch for a detail page, owned by the page.
//
// A request is sent once when the page opens (or the id changes) and again
// only when reload() is called (Refresh / Retry). There is no polling, no
// refetch on focus/visibility, no automatic retry and no prefetch, and
// StrictMode's double effect run does not send a second request. This
// matters for tenant detail, where every request writes a provider audit
// row (OQ-1); relay detail uses the same discipline.

export type DetailState<T> =
  | { status: 'loading' }
  | { status: 'ready'; data: T }
  | { status: 'not_found' } // 404, or a malformed id the server rejected (400)
  | { status: 'forbidden' } // a role 403 (the session is still valid)
  | { status: 'error'; error: unknown }
  | { status: 'session_ended' } // the client already ended the session; guards show Login

export interface DetailQuery<T> {
  state: DetailState<T>
  /** Explicit user action: one new request. */
  reload: () => void
}

export function useDetailQuery<T>(id: string, fetchOne: (id: string) => Promise<T>): DetailQuery<T> {
  const [nonce, setNonce] = useState(0)
  const reqKey = `${id}|${nonce}`
  const [result, setResult] = useState<{ key: string; state: DetailState<T> } | undefined>(undefined)

  const sentRef = useRef<string | undefined>(undefined)
  const latestRef = useRef(reqKey)
  const fetchRef = useRef(fetchOne)
  useLayoutEffect(() => {
    latestRef.current = reqKey
    fetchRef.current = fetchOne
  })

  useEffect(() => {
    if (sentRef.current === reqKey) return // StrictMode's second run
    sentRef.current = reqKey
    const key = reqKey
    fetchRef.current(id).then(
      (data) => {
        if (latestRef.current === key) setResult({ key, state: { status: 'ready', data } })
      },
      (err: unknown) => {
        if (latestRef.current !== key) return
        let state: DetailState<T>
        if (err instanceof ApiError && err.sessionEnded) state = { status: 'session_ended' }
        else if (err instanceof ApiError && (err.status === 404 || err.status === 400)) state = { status: 'not_found' }
        else if (err instanceof ApiError && err.status === 403) state = { status: 'forbidden' }
        else state = { status: 'error', error: err }
        setResult({ key, state })
      },
    )
    // id is part of reqKey.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reqKey])

  const reload = useCallback(() => setNonce((n) => n + 1), [])
  const state: DetailState<T> = result && result.key === reqKey ? result.state : { status: 'loading' }
  return { state, reload }
}
