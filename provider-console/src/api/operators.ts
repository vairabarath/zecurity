import type { ProviderRole } from '@/auth/roles'
import { request } from './client'
import type { ChangeRoleResponse, CreateOperatorResponse, OperatorView, ResetPasswordResponse } from './types'

// Operator management calls (Phase U; super-admin only, the controller
// decides). These five mutations plus the auth calls are the console's only
// writes (api-surface.test.ts). Temporary passwords come back in the response
// body and are handed straight to the caller; nothing here keeps them.

function userPath(id: string): string {
  return `/provider/users/${encodeURIComponent(id)}`
}

export function listOperators(): Promise<OperatorView[]> {
  return request<OperatorView[]>('GET', '/provider/users')
}

export function createOperator(email: string, role: ProviderRole): Promise<CreateOperatorResponse> {
  return request<CreateOperatorResponse>('POST', '/provider/users', { body: { email, role } })
}

export function changeOperatorRole(id: string, role: ProviderRole): Promise<ChangeRoleResponse> {
  return request<ChangeRoleResponse>('PATCH', userPath(id), { body: { role } })
}

export function disableOperator(id: string): Promise<void> {
  return request<void>('POST', `${userPath(id)}/disable`)
}

/** Clears the operator's password (has_password becomes false); a reset is a separate call. */
export function enableOperator(id: string): Promise<void> {
  return request<void>('POST', `${userPath(id)}/enable`)
}

export function resetOperatorPassword(id: string): Promise<ResetPasswordResponse> {
  return request<ResetPasswordResponse>('POST', `${userPath(id)}/reset-password`)
}
