import { ApiError } from '@/api/client'
import { ErrorCode } from '@/api/types'

// Readable messages for relay-pool failures (D-37 … D-41). The server decides;
// these only explain its answer. Same shape as relay-errors.ts: `field` puts
// the message on that form field; `refresh` means the target changed or is
// gone, so the dialog closes and the page reloads with the message as a
// notice; otherwise the message stays in the dialog.

export interface PoolFailure {
  message: string
  field?: 'name' | 'region' | 'reason' | 'failover'
  refresh?: boolean
}

const NETWORK = 'Can’t reach the controller. Check your connection; nothing was changed by this attempt.'

/** Region codes as the controller accepts them (D-37): lowercase, hyphen-separated, at most 32 characters. */
export const REGION_PATTERN = /^[a-z][a-z0-9]*(-[a-z0-9]+)*$/
export const MAX_REGION = 32
/** Pool names: trimmed, 1–64 characters, no control characters (relay ValidPoolName). */
export const MAX_POOL_NAME = 64

/** A problem with a region code, or null when it is valid (or empty: no region). */
export function regionProblem(region: string): string | null {
  if (region === '') return null
  if (region.length > MAX_REGION || !REGION_PATTERN.test(region)) {
    return 'Use a lowercase code such as eu-central-1: letters, digits and single hyphens, starting with a letter, at most 32 characters.'
  }
  return null
}

export type PoolEdit = 'create' | 'rename' | 'placement' | 'delete'

/** Pool create, rename, region/residency and delete. */
export function poolEditFailure(err: unknown, edit: PoolEdit, poolName = 'This pool'): PoolFailure {
  const fallback = { message: 'The change didn’t go through. Try again.' }
  if (!(err instanceof ApiError)) return fallback
  switch (err.code) {
    case ErrorCode.invalidName:
      return { field: 'name', message: 'Enter a name of 1 to 64 characters, without control characters.' }
    case ErrorCode.nameTaken:
      return { field: 'name', message: 'Another pool already has this name.' }
    case ErrorCode.invalidRegion:
      return { field: 'region', message: 'The controller rejected this region code. Use a lowercase code such as eu-central-1.' }
    case ErrorCode.residencyNeedsRegion:
      return { field: 'region', message: 'Residency needs a region. Enter a region code or turn residency off.' }
    case ErrorCode.poolInUse:
      return {
        message: `${poolName} is used by tenants (as their primary or failover pool), so its region and residency can’t change. Create a new pool with the region and residency you want and move the tenants to it instead.`,
      }
    case ErrorCode.poolNotEmpty:
      return {
        message: `${poolName} still has relays or tenants (including tenants using it as their failover pool). Move its relays to another pool and reassign those tenants, then delete it.`,
      }
    case ErrorCode.defaultPool:
      return { refresh: true, message: 'The default pool can’t be renamed, changed or deleted.' }
    case ErrorCode.forbidden:
      return {
        message:
          edit === 'placement' || edit === 'create'
            ? 'Only super-admins can set a pool’s region or residency.'
            : 'Your role can’t change relay pools.',
      }
    case ErrorCode.notFound:
      return { refresh: true, message: `${poolName} no longer exists, possibly removed by another operator.` }
    case ErrorCode.invalidRequest:
      return { message: 'The controller rejected this request as malformed. Reload the page and try again.' }
    case ErrorCode.networkError:
      return { message: NETWORK }
    default:
      return fallback
  }
}

/** POST /provider/relays/{id}/pool. */
export function moveRelayFailure(err: unknown, relayName: string): PoolFailure {
  const fallback = { message: 'The move didn’t go through. Try again.' }
  if (!(err instanceof ApiError)) return fallback
  switch (err.code) {
    case ErrorCode.lastEligibleRelay:
      return {
        message: `${relayName} is the last eligible relay of its pool, and tenants use that pool (as their primary or failover pool), so it can’t leave it, even when those tenants have a failover pool. Add, move or resume another relay in that pool first, or reassign those tenants.`,
      }
    case ErrorCode.reasonRequired:
      return { field: 'reason', message: 'Enter a reason. It is recorded in the provider audit log.' }
    case ErrorCode.reasonTooLong:
      return { field: 'reason', message: 'Keep the reason to 500 characters or fewer.' }
    case ErrorCode.poolNotFound:
      return { message: 'That pool no longer exists. Close this dialog and open it again to pick another.' }
    case ErrorCode.forbidden:
      return { message: 'Your role can’t move relays between pools.' }
    case ErrorCode.alreadyRevoked:
      return { refresh: true, message: `${relayName} has been revoked, possibly by another operator. The page is up to date.` }
    case ErrorCode.alreadyDeleted:
      return { refresh: true, message: `${relayName} has been deleted, possibly by another operator. The page is up to date.` }
    case ErrorCode.notFound:
      return { refresh: true, message: `${relayName} no longer exists.` }
    case ErrorCode.networkError:
      return { message: NETWORK }
    default:
      return fallback
  }
}

/** PUT /provider/tenants/{id}/relay-pool (super-admin). */
export function assignPoolFailure(err: unknown, tenantName: string): PoolFailure {
  const fallback = { message: 'The pool change didn’t go through. Try again.' }
  if (!(err instanceof ApiError)) return fallback
  switch (err.code) {
    case ErrorCode.poolHasNoEligibleRelay:
      return {
        message: 'A chosen pool (primary or failover) has no eligible relay, so it couldn’t carry this tenant’s relay traffic. Add or resume a relay in it first, or choose another pool.',
      }
    case ErrorCode.failoverSameAsPrimary:
      return { field: 'failover', message: 'The failover pool must be a different pool from the primary.' }
    case ErrorCode.failoverRegionMismatch:
      return { field: 'failover', message: 'The failover pool must be in exactly the same region as the primary. Cross-region failover isn’t allowed.' }
    case ErrorCode.failoverNeedsResidency:
      return { field: 'failover', message: 'The primary is a residency pool, so the failover pool must be a residency pool in the same region.' }
    case ErrorCode.reasonRequired:
      return { field: 'reason', message: 'Enter a reason. It is recorded in the provider audit log.' }
    case ErrorCode.reasonTooLong:
      return { field: 'reason', message: 'Keep the reason to 500 characters or fewer.' }
    case ErrorCode.poolNotFound:
      return { message: 'A chosen pool (primary or failover) no longer exists. Close this dialog and open it again to pick another.' }
    case ErrorCode.invalidMigration:
      return { message: 'Choose Graceful or Immediate.' }
    case ErrorCode.forbidden:
      return { message: 'Only super-admins can change a tenant’s relay pool.' }
    case ErrorCode.notFound:
      return { refresh: true, message: `${tenantName} no longer exists.` }
    case ErrorCode.networkError:
      return { message: NETWORK }
    default:
      return fallback
  }
}
