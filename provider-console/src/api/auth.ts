import { request } from './client'
import type { LoginResponse, Me, SessionResponse } from './types'

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

export function logout(): Promise<void> {
  return request<void>('POST', '/provider/auth/logout')
}

export function getMe(): Promise<Me> {
  return request<Me>('GET', '/provider/me')
}
