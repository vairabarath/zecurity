import { CombinedGraphQLErrors, ServerError } from '@apollo/client/errors'
import { useAuthStore } from '@/store/auth'

// PENDING-06: the console side of workspace MFA.
//
// The controller answers MFA_REQUIRED in two shapes:
//   - console-wide (M-6): /graphql and the console REST routes refuse a
//     session without MFA with HTTP 403. Apollo v4 turns every non-2xx into a
//     ServerError and never parses the body, so the code is read from bodyText.
//   - step-up (M-7 break-glass, turning console MFA on): an HTTP 200 GraphQL
//     error whose extensions.code is MFA_REQUIRED.
// Both are answered the same way: verify a code (POST /auth/mfa/verify), take
// the new access token, and send the request again.

export const MFA_REQUIRED = 'MFA_REQUIRED'

export type MfaRequiredKind = 'console' | 'step-up'

/** Which kind of MFA_REQUIRED this error is, or null if it is not one. */
export function mfaRequiredKind(error: unknown): MfaRequiredKind | null {
  if (ServerError.is(error) && error.statusCode === 403) {
    try {
      const body = JSON.parse(error.bodyText) as {
        errors?: { extensions?: { code?: unknown } }[]
      }
      if (body.errors?.some((e) => e.extensions?.code === MFA_REQUIRED)) return 'console'
    } catch {
      // Not JSON: not ours.
    }
    return null
  }
  if (CombinedGraphQLErrors.is(error) && error.errors.some((e) => e.extensions?.code === MFA_REQUIRED)) {
    return 'step-up'
  }
  return null
}

export function isMfaRequired(error: unknown): boolean {
  return mfaRequiredKind(error) !== null
}

// ── REST calls ──────────────────────────────────────────────────────────────

export type MfaErrorCode =
  | 'INVALID_CODE'
  | 'RATE_LIMITED'
  | 'NOT_ENROLLED'
  | 'ALREADY_ENROLLED'
  | 'UNAUTHORIZED'
  | 'UNAVAILABLE'
  | 'NETWORK'

export class MfaApiError extends Error {
  code: MfaErrorCode
  retryAfterSeconds?: number

  constructor(code: MfaErrorCode, message: string, retryAfterSeconds?: number) {
    super(message)
    this.name = 'MfaApiError'
    this.code = code
    this.retryAfterSeconds = retryAfterSeconds
  }
}

export interface MfaStatus {
  enrolled: boolean
  consoleRequireMfa: boolean
  sessionMfaAt: string | null
}

export interface MfaEnrollment {
  secret: string
  uri: string
}

// Refreshes the access token through the httpOnly cookie. Returns false when
// the session is gone. Unlike the Apollo link it never redirects: the prompt
// shows the error instead.
async function refresh(): Promise<boolean> {
  try {
    const resp = await fetch('/auth/refresh', {
      method: 'POST',
      credentials: 'include',
      headers: { Authorization: `Bearer ${useAuthStore.getState().accessToken ?? ''}` },
    })
    if (!resp.ok) return false
    const data = (await resp.json()) as { access_token?: string }
    if (!data.access_token) return false
    useAuthStore.getState().setAccessToken(data.access_token)
    return true
  } catch {
    return false
  }
}

async function call<T>(method: 'GET' | 'POST', path: string, body?: unknown): Promise<T> {
  const send = () =>
    fetch(path, {
      method,
      credentials: 'same-origin',
      headers: {
        ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
        Authorization: `Bearer ${useAuthStore.getState().accessToken ?? ''}`,
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    })

  const errorBody = async (r: Response) => {
    try {
      return (await r.json()) as { code?: string; error?: string }
    } catch {
      return {}
    }
  }

  let resp: Response
  let data: { code?: string; error?: string } = {}
  try {
    resp = await send()
    if (!resp.ok) data = await errorBody(resp)
    // An access token expired while the prompt was open: refresh once. A
    // wrong code is a 401 too (INVALID_CODE), and must not refresh.
    if (resp.status === 401 && data.code !== 'INVALID_CODE' && (await refresh())) {
      resp = await send()
      data = resp.ok ? {} : await errorBody(resp)
    }
  } catch {
    throw new MfaApiError('NETWORK', 'network error')
  }

  if (resp.ok) return (await resp.json()) as T

  let code: MfaErrorCode = 'UNAVAILABLE'
  const message = data.error ?? `HTTP ${resp.status}`
  if (data.code) code = data.code as MfaErrorCode
  if (resp.status === 401 && code !== 'INVALID_CODE') code = 'UNAUTHORIZED'
  const retryAfter = Number(resp.headers.get('Retry-After'))
  throw new MfaApiError(code, message, Number.isFinite(retryAfter) && retryAfter > 0 ? retryAfter : undefined)
}

export function getMfaStatus(): Promise<MfaStatus> {
  return call<MfaStatus>('GET', '/auth/mfa/status')
}

/** Starts enrolment. The secret is returned once; keep it in component state only. */
export function startMfaEnrollment(): Promise<MfaEnrollment> {
  return call<MfaEnrollment>('POST', '/auth/mfa/enroll')
}

/**
 * Verifies a code (confirming a pending enrolment on the first correct one).
 * On success the session carries the MFA mark and the returned access token,
 * which is stored here, says so.
 */
export async function verifyMfaCode(code: string): Promise<{ enrolled: boolean }> {
  const data = await call<{ accessToken: string; expiresIn: number; enrolled: boolean }>(
    'POST',
    '/auth/mfa/verify',
    { code },
  )
  useAuthStore.getState().setAccessToken(data.accessToken)
  return { enrolled: data.enrolled }
}

export function mfaErrorMessage(err: unknown): string {
  if (!(err instanceof MfaApiError)) return 'Could not check the code. Try again.'
  switch (err.code) {
    case 'INVALID_CODE':
      return 'That code is not right. Enter the current code from your authenticator app.'
    case 'RATE_LIMITED':
      return err.retryAfterSeconds
        ? `Too many wrong codes. Try again in ${Math.ceil(err.retryAfterSeconds / 60)} min.`
        : 'Too many wrong codes. Try again later.'
    case 'UNAUTHORIZED':
      return 'Your session has ended. Sign in again.'
    case 'NETWORK':
      return 'Cannot reach the controller. Check your connection.'
    default:
      return 'Verification is unavailable right now. Try again shortly.'
  }
}

/** "JBSWY3DP…" → "JBSW Y3DP …" for reading and typing. */
export function groupKey(secret: string): string {
  return secret.replace(/(.{4})/g, '$1 ').trim()
}
