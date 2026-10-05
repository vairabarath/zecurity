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
  // operator routes (Phase U). GET /provider/users answers 403 with
  // "provider action forbidden" instead; match operator 403s on the status.
  forbidden: 'forbidden',
  notFound: 'not_found',
  invalidEmail: 'invalid_email',
  invalidRole: 'invalid_role',
  cannotModifySelf: 'cannot_modify_self',
  lastSuperAdmin: 'last_super_admin',
  alreadyExists: 'already_exists',
  existsDisabled: 'exists_disabled',
  accountDisabled: 'account_disabled',
  serviceUnavailable: 'service_unavailable',
  // client-side only
  networkError: 'network_error',
} as const

/**
 * One provider operator as GET /provider/users returns it (Phase U
 * OperatorView). It never carries a password or password hash.
 */
export interface OperatorView {
  id: string
  email: string
  role: string
  disabled_at: string | null
  created_at: string
  last_login_at: string | null
  must_change_password: boolean
  /** False after enable (the old credential is cleared) until a reset. */
  has_password: boolean
}

/**
 * POST /provider/users → 201. `temporary_password` is shown once and then
 * discarded; it must never reach storage, the URL or a log.
 */
export interface CreateOperatorResponse {
  user: OperatorView
  temporary_password: string
}

/** PATCH /provider/users/{id} → 200. */
export interface ChangeRoleResponse {
  user: OperatorView
}

/** POST /provider/users/{id}/reset-password → 200. Shown once, like on create. */
export interface ResetPasswordResponse {
  temporary_password: string
}
