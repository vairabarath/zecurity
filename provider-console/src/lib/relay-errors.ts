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
