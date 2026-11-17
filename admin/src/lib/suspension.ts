import { ServerError } from '@apollo/client/errors'

// Sprint 23 S1 (D-45): a workspace suspended by the provider. The controller
// refuses it in three shapes, and the console answers all three the same way:
// end the local session and show the suspended message on the login page
// (never a generic error).
//   - sign-in: the OAuth callback redirects to /login?error=workspace_suspended;
//   - refresh: POST /auth/refresh answers 401 {"error":"workspace suspended"};
//   - any console request with a token issued before the suspension: HTTP 403
//     with extensions.code TENANT_SUSPENDED (Apollo v4 surfaces it as a
//     ServerError and never parses the body, so it is read from bodyText).

export const TENANT_SUSPENDED = 'TENANT_SUSPENDED'
/** The refresh refusal message (auth.ErrMsgWorkspaceSuspended). */
export const REFRESH_SUSPENDED = 'workspace suspended'
/** The login page reason (CALLBACK_ERRORS key) for a suspended workspace. */
export const SUSPENDED_REASON = 'workspace_suspended'
export const SUSPENDED_LOGIN_PATH = `/login?error=${SUSPENDED_REASON}`

/** Whether an Apollo error is the provider-suspension refusal. */
export function isTenantSuspendedError(error: unknown): boolean {
  if (!ServerError.is(error) || error.statusCode !== 403) return false
  try {
    const body = JSON.parse(error.bodyText) as { errors?: { extensions?: { code?: unknown } }[] }
    return body.errors?.some((e) => e.extensions?.code === TENANT_SUSPENDED) ?? false
  } catch {
    return false
  }
}

/**
 * Whether a refused /auth/refresh response says the workspace is suspended.
 * Reads a clone, so the caller can still read the body.
 */
export async function isSuspendedRefresh(resp: Response): Promise<boolean> {
  if (resp.status !== 401) return false
  try {
    const body = (await resp.clone().json()) as { error?: unknown }
    return body.error === REFRESH_SUSPENDED
  } catch {
    return false
  }
}
