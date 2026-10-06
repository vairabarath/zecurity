import { useSessionStore } from '@/auth/session'
import { ErrorCode, type ErrorBody } from './types'

// The only place in the console that calls fetch (enforced by
// src/test/static-guards.test.ts).
//
// Response classification (Sprint 21 C-a):
//   401 invalid_credentials on a credential check (login, change password)
//       → a form error; the session (if any) is untouched
//   any other 401 (the RequireProvider middleware: missing/malformed header,
//       invalid or expired token, session revoked) → the session has ended
//   403 "not a provider user" (disabled account) → the session has ended
//   403 password_change_required → only Change password is reachable
//   any other 403 (e.g. forbidden) → ApiError; the caller shows Forbidden
//
// None of this is authorization: the controller decides every request, the
// console only reacts to its answers.

/** Empty in dev (Vite proxies /provider); the controller origin in production. */
const API_BASE: string = import.meta.env.VITE_PROVIDER_API_BASE ?? ''

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly detail?: string
  readonly retryAfterSeconds?: number
  /** True when this response ended the session (it has already been cleared). */
  readonly sessionEnded: boolean

  constructor(init: {
    status: number
    code: string
    detail?: string
    retryAfterSeconds?: number
    sessionEnded?: boolean
  }) {
    super(init.code)
    this.name = 'ApiError'
    this.status = init.status
    this.code = init.code
    this.detail = init.detail
    this.retryAfterSeconds = init.retryAfterSeconds
    this.sessionEnded = init.sessionEnded ?? false
  }
}

export interface RequestOptions {
  body?: unknown
  /** Send the bearer token (default true). Login is the only call without it. */
  auth?: boolean
  /** The endpoint checks a password, so 401 invalid_credentials is a form error. */
  credentialCheck?: boolean
}

function parseRetryAfter(value: string | null): number | undefined {
  if (!value) return undefined
  const n = Number.parseInt(value, 10)
  return Number.isFinite(n) && n >= 0 ? n : undefined
}

async function readErrorBody(res: Response): Promise<ErrorBody> {
  try {
    const body: unknown = await res.json()
    return body && typeof body === 'object' ? (body as ErrorBody) : {}
  } catch {
    return {}
  }
}

export async function request<T>(
  method: 'GET' | 'POST' | 'PATCH',
  path: string,
  { body, auth = true, credentialCheck = false }: RequestOptions = {},
): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (auth) {
    const token = useSessionStore.getState().token
    if (token) headers.Authorization = `Bearer ${token}`
  }

  let res: Response
  try {
    res = await fetch(API_BASE + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store',
      credentials: 'omit',
    })
  } catch {
    throw new ApiError({ status: 0, code: ErrorCode.networkError })
  }

  if (res.ok) {
    if (res.status === 204) return undefined as T
    return (await res.json()) as T
  }

  const err = await readErrorBody(res)
  const code = err.error ?? `http_${res.status}`
  const base = {
    status: res.status,
    code,
    detail: err.detail,
    retryAfterSeconds: parseRetryAfter(res.headers.get('Retry-After')),
  }
  const session = useSessionStore.getState()

  if (res.status === 401) {
    if (credentialCheck && code === ErrorCode.invalidCredentials) {
      throw new ApiError(base)
    }
    const expired = session.expiresAt !== undefined && session.expiresAt <= Date.now()
    session.endSession(expired ? 'expired' : 'session_ended')
    throw new ApiError({ ...base, sessionEnded: true })
  }

  if (res.status === 403) {
    if (code === ErrorCode.notAProviderUser) {
      session.endSession('disabled')
      throw new ApiError({ ...base, sessionEnded: true })
    }
    if (code === ErrorCode.passwordChangeRequired) {
      session.requirePasswordChange()
    }
  }

  throw new ApiError(base)
}
