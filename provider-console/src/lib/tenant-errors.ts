import { ApiError } from '@/api/client'
import { ErrorCode } from '@/api/types'

export type TenantLifecycleAction = 'suspend' | 'resume'

/**
 * What a failed suspend/resume means for the console (Sprint 23 S1, D-45);
 * same shape as relayActionFailure. `field` puts the message on that dialog
 * field; `refresh` means the tenant changed under us (another operator), so
 * the dialog closes and the page reloads with `message` as a notice.
 */
export interface TenantLifecycleFailure {
  message: string
  field?: 'reason' | 'confirm'
  refresh?: boolean
}

export function tenantLifecycleFailure(err: unknown, action: TenantLifecycleAction, tenantName: string): TenantLifecycleFailure {
  const fallback = { message: `The ${action} didn’t go through. Try again.` }
  if (!(err instanceof ApiError)) return fallback
  switch (err.code) {
    case ErrorCode.confirmationMismatch:
      return { field: 'confirm', message: 'That doesn’t match the tenant’s slug exactly (case matters).' }
    case ErrorCode.reasonRequired:
      return { field: 'reason', message: 'Enter a reason. It is recorded in the provider audit log.' }
    case ErrorCode.reasonTooLong:
      return { field: 'reason', message: 'Keep the reason to 500 characters or fewer.' }
    case ErrorCode.forbidden:
      return { message: 'Only super-admins can suspend or resume tenants.' }
    case ErrorCode.alreadySuspended:
      return { refresh: true, message: `${tenantName} was already suspended, possibly by another operator. The page is up to date.` }
    case ErrorCode.notSuspended:
      return { refresh: true, message: `${tenantName} isn’t suspended (another operator may have resumed it). The page is up to date.` }
    case ErrorCode.notFound:
      return { refresh: true, message: `${tenantName} no longer exists.` }
    case ErrorCode.networkError:
      return { message: 'Can’t reach the controller. Check your connection; nothing was changed by this attempt.' }
    default:
      return fallback
  }
}
