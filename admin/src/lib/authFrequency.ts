// PENDING-06 M-2: a resource policy's authentication frequency, in minutes.
// null = device-only (no authentication requirement). The server accepts
// 60 … 44640 (1 hour … 31 days).

export const AUTH_FREQUENCY_MIN = 60
export const AUTH_FREQUENCY_MAX = 44640

export const AUTH_FREQUENCY_PRESETS: number[] = [60, 240, 720, 1440, 10080, 20160, 44640]

function plural(n: number, unit: string): string {
  return `${n} ${unit}${n === 1 ? '' : 's'}`
}

/** 60 → "1 hour", 1440 → "1 day", 90 → "90 minutes". */
export function formatAuthFrequency(minutes: number): string {
  if (minutes % 1440 === 0) return plural(minutes / 1440, 'day')
  if (minutes % 60 === 0) return plural(minutes / 60, 'hour')
  return plural(minutes, 'minute')
}

/** Short summary for a policy row: "Device only", "Sign-in every 1 day", "MFA every 1 day". */
export function authRequirementSummary(minutes: number | null | undefined, requireMfa: boolean): string {
  if (minutes == null) return 'Device only'
  return `${requireMfa ? 'MFA' : 'Sign-in'} every ${formatAuthFrequency(minutes)}`
}
