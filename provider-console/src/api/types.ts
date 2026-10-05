// Wire types for the provider API (controller/internal/provider). Field names
// match the JSON exactly.

/** POST /provider/auth/login → 200 */
export interface LoginResponse {
  token: string
  expires_in: number
  password_change_required: boolean
}

/** POST /provider/auth/password → 200. The previous token is dead (generation bump). */
export interface SessionResponse {
  token: string
  expires_in: number
}

/**
 * GET /provider/me → 200. `last_login_at` is stamped by the successful login
 * that started this session, so the UI labels it "Signed in at".
 * `role` comes from the database row; it is typed as string because the
 * console must cope with a role it doesn't know (it then grants nothing).
 */
export interface Me {
  user_id: string
  email: string
  role: string
  last_login_at: string | null
}

/** Every error body is `{"error": "<code>"}`; `password_policy` adds `detail`. */
export interface ErrorBody {
  error?: string
  detail?: string
}

/** Error codes the console reacts to. Anything else is shown generically. */
export const ErrorCode = {
  // auth handlers
  invalidRequest: 'invalid_request',
  invalidCredentials: 'invalid_credentials',
  tooManyAttempts: 'too_many_attempts',
  loginUnavailable: 'login_unavailable',
  passwordPolicy: 'password_policy',
  serverError: 'server_error',
  // RequireProvider middleware
  passwordChangeRequired: 'password_change_required',
  notAProviderUser: 'not a provider user',
  // operator routes (Phase U)
  forbidden: 'forbidden',
  // client-side only
  networkError: 'network_error',
} as const
