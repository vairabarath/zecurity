import { print } from 'graphql'
import { toast } from 'sonner'
import { CombinedGraphQLErrors } from '@apollo/client/errors'
import { MeDocument, type MeQuery } from '@/generated/graphql'
import { useAuthStore } from '@/store/auth'

// The @requires directive refuses a call with this message prefix when the
// caller's LIVE role lacks the capability (PENDING-19 §5).
const REFUSAL_PREFIX = 'forbidden: requires capability'

export function isCapabilityRefusal(error: unknown): boolean {
  return (
    CombinedGraphQLErrors.is(error) &&
    error.errors.some((e) => typeof e.message === 'string' && e.message.startsWith(REFUSAL_PREFIX))
  )
}

let inFlight: Promise<void> | null = null

// handleCapabilityRefusal tells the user the server refused the action and
// re-reads `me`, so the console's controls follow the user's current role at
// once. The console reads capabilities once per load, so without this a user
// demoted while a page is open keeps controls the server will refuse.
//
// It calls /graphql with fetch rather than the Apollo client to stay out of the
// link chain that invoked it. A Member (no CONSOLE_READ) is then redirected to
// /install by the console route guard.
export function handleCapabilityRefusal(): Promise<void> {
  toast.error("You don't have permission for this action", {
    id: 'capability-refused',
    description: 'Your role may have changed. The console now shows what your current role allows.',
  })
  if (inFlight) return inFlight
  inFlight = (async () => {
    try {
      const token = useAuthStore.getState().accessToken
      const resp = await fetch('/graphql', {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
          'Content-Type': 'application/json',
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
        },
        body: JSON.stringify({ query: print(MeDocument) }),
      })
      if (!resp.ok) return
      const body = (await resp.json()) as { data?: MeQuery }
      if (body.data?.me) useAuthStore.getState().setUser(body.data.me)
    } catch {
      // Best effort: the server refused the call either way; the next load
      // re-reads the role.
    } finally {
      inFlight = null
    }
  })()
  return inFlight
}
