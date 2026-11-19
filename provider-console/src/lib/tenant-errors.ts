import { ApiError } from '@/api/client'
import { ErrorCode } from '@/api/types'

export type TenantLifecycleAction = 'suspend' | 'resume' | 'delete' | 'cancel the deletion' | 'legal hold change'

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
      return { message: 'Only super-admins can suspend, resume or delete tenants.' }
    case ErrorCode.deleteScheduled:
      return action === 'resume'
        ? { message: `${tenantName} is scheduled for deletion. Cancel the deletion first.` }
        : { refresh: true, message: `${tenantName}’s deletion is already scheduled. The page is up to date.` }
    case ErrorCode.deleteNotScheduled:
      return { refresh: true, message: `${tenantName} has no scheduled deletion (another operator may have cancelled it). The page is up to date.` }
    case ErrorCode.purgeStarted:
      return { refresh: true, message: `${tenantName}’s purge has started and can’t be cancelled any more. Place a legal hold to pause it.` }
    case ErrorCode.legalHoldUnchanged:
      return { refresh: true, message: `${tenantName}’s legal hold was already in that state. The page is up to date.` }
    case ErrorCode.alreadySuspended:
      return { refresh: true, message: `${tenantName} was already suspended, possibly by another operator. The page is up to date.` }
    case ErrorCode.notSuspended:
      if (action === 'delete') return { refresh: true, message: `Only a suspended tenant can be deleted. ${tenantName} isn’t suspended: suspend it first.` }
      return { refresh: true, message: `${tenantName} isn’t suspended (another operator may have resumed it). The page is up to date.` }
    case ErrorCode.notFound:
      return { refresh: true, message: `${tenantName} no longer exists.` }
    case ErrorCode.networkError:
      return { message: 'Can’t reach the controller. Check your connection; nothing was changed by this attempt.' }
    default:
      return fallback
  }
}
