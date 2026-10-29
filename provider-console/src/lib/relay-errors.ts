import { ApiError } from '@/api/client'
import { ErrorCode } from '@/api/types'

// Readable messages for relay mutation failures (Sprint 22 Phase A codes).
// The server decides; these only explain its answer.
export function relayErrorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) return 'The action failed. Try again.'
  switch (err.code) {
    case ErrorCode.invalidName:
      return 'Enter a name of 1 to 128 characters, without control characters.'
    case ErrorCode.invalidDnsSan:
      return `A DNS name was rejected${err.detail ? `: ${err.detail}` : '.'}`
    case ErrorCode.invalidIpSan:
      return `An IP address was rejected${err.detail ? `: ${err.detail}` : '.'}`
    case ErrorCode.forbidden:
      return 'Your role can’t do this.'
    case ErrorCode.networkError:
      return 'Can’t reach the controller. Check your connection.'
    default:
      return 'The action failed. Try again.'
  }
}

export type RelayAction = 'revoke' | 'delete'

/**
 * What a failed revoke/delete means for the console (Phase X). `field` puts
 * the message on that dialog field; `refresh` means the relay changed (or is
 * gone) under us, so the dialog closes, the page reloads and `message` is
 * shown as a notice; otherwise the message stays in the dialog.
 */
export interface RelayActionFailure {
  message: string
  field?: 'reason' | 'confirm'
  refresh?: boolean
}

export function relayActionFailure(err: unknown, action: RelayAction, relayName: string): RelayActionFailure {
  const verb = action === 'revoke' ? 'revoke' : 'delete'
  if (!(err instanceof ApiError)) return { message: `The ${verb} didn’t go through. Try again.` }
  switch (err.code) {
    case ErrorCode.confirmationMismatch:
      return { field: 'confirm', message: 'That doesn’t match the relay’s name exactly (case matters).' }
    case ErrorCode.reasonRequired:
      return { field: 'reason', message: 'Enter a reason. It is recorded in the provider audit log.' }
    case ErrorCode.reasonTooLong:
      return { field: 'reason', message: 'Keep the reason to 500 characters or fewer.' }
    case ErrorCode.forbidden:
      return { message: 'Only super-admins can revoke or delete relays.' }
    case ErrorCode.alreadyRevoked:
      return { refresh: true, message: `${relayName} was already revoked, possibly by another operator. The page is up to date.` }
    case ErrorCode.alreadyDeleted:
      return { refresh: true, message: `${relayName} was already deleted, possibly by another operator. The page is up to date.` }
    case ErrorCode.notFound:
      return { refresh: true, message: `${relayName} no longer exists.` }
    case ErrorCode.networkError:
      return { message: 'Can’t reach the controller. Check your connection; nothing was changed by this attempt.' }
    default:
      return { message: `The ${verb} didn’t go through. Try again.` }
  }
}

export type DrainAction = 'drain' | 'resume'

/** What a failed drain/resume means for the console (Phase D, D-32); same shape as relayActionFailure. */
export function drainActionFailure(err: unknown, action: DrainAction, relayName: string): RelayActionFailure {
  const fallback = { message: `The ${action} didn’t go through. Try again.` }
  if (!(err instanceof ApiError)) return fallback
  switch (err.code) {
    case ErrorCode.lastEligibleRelay:
      return {
        message: `${relayName} is the only relay connectors can use right now, so it can’t be drained. Add or resume another relay first.`,
      }
    case ErrorCode.reasonTooLong:
      return { field: 'reason', message: 'Keep the reason to 500 characters or fewer.' }
    case ErrorCode.forbidden:
      return { message: 'Your role can’t drain or resume relays.' }
    case ErrorCode.invalidState:
      return { refresh: true, message: `${relayName} hasn’t enrolled yet, so there is nothing to drain.` }
    case ErrorCode.alreadyRevoked:
      return { refresh: true, message: `${relayName} has been revoked, possibly by another operator. The page is up to date.` }
    case ErrorCode.alreadyDeleted:
      return { refresh: true, message: `${relayName} has been deleted, possibly by another operator. The page is up to date.` }
    case ErrorCode.notFound:
      return { refresh: true, message: `${relayName} no longer exists.` }
    case ErrorCode.networkError:
      return { message: 'Can’t reach the controller. Check your connection; nothing was changed by this attempt.' }
    default:
      return fallback
  }
}
