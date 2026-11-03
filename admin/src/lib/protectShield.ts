import { ShieldStatus } from '@/generated/graphql'

export interface ShieldLike {
  id: string
  name: string
  status: ShieldStatus
  lanIp?: string | null
}

/**
 * The shield `protectResource` would bind, mirroring the controller's
 * `MarkProtecting` (controller/internal/resource/store.go): an ACTIVE shield in
 * the resource's remote network whose LAN IP equals the resource host,
 * preferring the shield already bound to the resource. `shields` must be the
 * shields of the resource's remote network.
 *
 * Returns null when no such shield exists. A resource created before its
 * shield has no bound shield, so gating on the bound shield alone left it
 * unprotectable (Sprint 20 Fix 10).
 */
export function protectTargetShield<T extends ShieldLike>(
  host: string,
  boundShield: T | null | undefined,
  shields: readonly T[],
): T | null {
  const candidates = shields.filter(
    (s) => s.status === ShieldStatus.Active && s.lanIp === host,
  )
  return candidates.find((s) => s.id === boundShield?.id) ?? candidates[0] ?? null
}

/**
 * The shield to describe when nothing can be protected yet: the bound shield,
 * else any shield of the remote network on the resource host (whatever its
 * status), else null (no shield on that host).
 */
export function hostShield<T extends ShieldLike>(
  host: string,
  boundShield: T | null | undefined,
  shields: readonly T[],
): T | null {
  return boundShield ?? shields.find((s) => s.lanIp === host) ?? null
}
