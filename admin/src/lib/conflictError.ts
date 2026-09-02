// Error classification for the SCIM provisioning-conflicts queue (FE-4, ADR-025
// §4.1/§9). The backend's ErrorPresenter (controller/graph/resolvers/presenter.go)
// surfaces client-actionable SCIM errors with a structured extensions.code; the
// frontend branches on that code, never on the message text.
//
// Status set is a WHITELIST: FORBIDDEN(403) / BAD_REQUEST(400) / NOT_FOUND(404) /
// CONFLICT(409). Everything else — a 5xx, a zero-status SCIMError, a 401, or an
// apperr.UserError (which carries NO code) — is collapsed to INTERNAL.
//
// CRITICAL BOUNDARY: a missing/unrecognized code is ALWAYS INTERNAL, never a
// denial. apperr.UserError surfaces verbatim with no code, and a missing code
// must never be inferred as a permission problem. This is the exact trap that
// bit FE-1 (updateScimConfig refuses via apperr.UserError with no code, so a
// code-branch there would be wrong). INTERNAL only governs how we branch — the
// message is always preserved and shown.

export type ConflictErrorCode =
  | 'FORBIDDEN'
  | 'BAD_REQUEST'
  | 'NOT_FOUND'
  | 'CONFLICT'
  | 'INTERNAL'

export type ConflictError = {
  code: ConflictErrorCode
  message: string
}

const KNOWN_CODES: ReadonlySet<string> = new Set([
  'FORBIDDEN',
  'BAD_REQUEST',
  'NOT_FOUND',
  'CONFLICT',
])

// Extracts the first GraphQL error's extensions.code, if it is a recognized
// client-safe code. Returns undefined for anything else (masked/unknown).
function firstExtensionCode(err: unknown): string | undefined {
  const gqlErrors = (
    err as { graphQLErrors?: Array<{ extensions?: Record<string, unknown> }> }
  )?.graphQLErrors
  const code = gqlErrors?.[0]?.extensions?.['code']
  return typeof code === 'string' ? code : undefined
}

function extractMessage(err: unknown): string {
  if (err && typeof err === 'object' && 'message' in err) {
    const msg = (err as { message?: unknown }).message
    if (typeof msg === 'string' && msg.trim()) return msg
  }
  return 'The request failed.'
}

export function asConflictError(err: unknown): ConflictError {
  const message = extractMessage(err)
  const raw = firstExtensionCode(err)
  const code: ConflictErrorCode =
    raw && KNOWN_CODES.has(raw) ? (raw as ConflictErrorCode) : 'INTERNAL'
  return { code, message }
}

// Human-readable guidance for a failed transition. Keeps the branching rules in
// one place so the row component stays declarative. The server message is always
// safe to show verbatim for the coded paths.
export function conflictGuidance(error: ConflictError): {
  title: string
  body: string
} {
  switch (error.code) {
    case 'FORBIDDEN':
      // Only Accept can return FORBIDDEN (the single 403 in AcceptLink's
      // break_glass permission check). Surface the server message and point the
      // admin at the missing permission.
      return {
        title: 'Permission required',
        body: `Accepting a directory link requires the identity.mapping.break_glass permission — ADMIN role is not sufficient. ${error.message}`,
      }
    case 'NOT_FOUND':
    case 'CONFLICT':
      // The conflict moved under us (resolved/removed elsewhere). Refresh the
      // queue rather than retrying the now-stale transition.
      return {
        title: 'Conflict changed',
        body: `This conflict was modified elsewhere. Refresh the queue and try again. (${error.message})`,
      }
    default:
      // INTERNAL (5xx / zero-status / 401 / apperr.UserError with no code).
      // Treat as an unexpected failure — surface and stop; never infer a
      // permission problem from it.
      return {
        title: 'Action failed',
        body: error.message,
      }
  }
}
