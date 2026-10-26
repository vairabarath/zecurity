import { request } from './client'
import type { LoginResponse, Me, SessionResponse, TOTPEnrollResponse } from './types'

// Provider auth calls (Phase H). Passwords are passed straight into the
// request body and kept nowhere else.

export function login(email: string, password: string): Promise<LoginResponse> {
  return request<LoginResponse>('POST', '/provider/auth/login', {
    body: { email, password },
    auth: false,
    credentialCheck: true,
  })
}

export function changePassword(currentPassword: string, newPassword: string): Promise<SessionResponse> {
  return request<SessionResponse>('POST', '/provider/auth/password', {
    body: { current_password: currentPassword, new_password: newPassword },
    credentialCheck: true,
  })
}

// TOTP steps (Sprint 22 Phase T). They need the MFA-stage token the login or
// password change returned; a wrong code is a form error (401 invalid_code).

export function verifyTotp(code: string): Promise<SessionResponse> {
  return request<SessionResponse>('POST', '/provider/auth/totp/verify', { body: { code }, credentialCheck: true })
}

/** Returns a new setup secret, exactly once. The caller keeps it in memory only. */
export function enrollTotp(): Promise<TOTPEnrollResponse> {
  return request<TOTPEnrollResponse>('POST', '/provider/auth/totp/enroll')
}

export function confirmTotp(code: string): Promise<SessionResponse> {
  return request<SessionResponse>('POST', '/provider/auth/totp/enroll/confirm', { body: { code }, credentialCheck: true })
}

export function logout(): Promise<void> {
  return request<void>('POST', '/provider/auth/logout')
}

export function getMe(): Promise<Me> {
  return request<Me>('GET', '/provider/me')
}
