import { useEffect, useState } from 'react'
import { ApiError } from '@/api/client'
import { listRelayPools } from '@/api/reads'
import type { RelayPoolSummary } from '@/api/types'

export type PoolsState =
  | { status: 'loading' }
  | { status: 'ready'; items: RelayPoolSummary[] }
  | { status: 'error' }
  | { status: 'session_ended' }

// The pool list for a picker (Add relay, Move to pool, Change pool). Fetched
// once when the component using it mounts (the dialog content, which mounts
// only while the dialog is open); never polled. The list read is not audited.
export function usePools(): PoolsState {
  const [state, setState] = useState<PoolsState>({ status: 'loading' })
  useEffect(() => {
    let live = true
    listRelayPools().then(
      (res) => {
        if (live) setState({ status: 'ready', items: res.items })
      },
      (err: unknown) => {
        if (live) setState(err instanceof ApiError && err.sessionEnded ? { status: 'session_ended' } : { status: 'error' })
      },
    )
    return () => {
      live = false
    }
  }, [])
  return state
}
