import { ApiError } from '@/api/client'
import { ErrorCode } from '@/api/types'

// Readable messages for operator-management failures (Phase U codes). The
// server decides; these only explain its answer.
export function operatorErrorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) return 'The action failed. Try again.'
  switch (err.code) {
    case ErrorCode.cannotModifySelf:
      return 'You can’t change your own account here. Use Change password in the account menu.'
    case ErrorCode.lastSuperAdmin:
      return 'This would leave no active super-admin. Make another operator super-admin first.'
    case ErrorCode.alreadyExists:
      return 'An operator with this email already exists.'
    case ErrorCode.existsDisabled:
      return 'This email belongs to a disabled operator. Enable that operator instead.'
    case ErrorCode.accountDisabled:
      return 'This operator is disabled. Enable them first, then reset the password.'
    case ErrorCode.invalidEmail:
      return 'Enter a valid email address.'
    case ErrorCode.invalidRole:
      return 'Choose a valid role.'
    case ErrorCode.notFound:
      return 'This operator no longer exists. The list has been refreshed.'
    case ErrorCode.serviceUnavailable:
      return 'The service is busy. Try again shortly.'
    case ErrorCode.networkError:
      return 'Can’t reach the controller. Check your connection.'
    default:
      return 'The action failed. Try again.'
  }
}
