import { describe, expect, it } from 'vitest'
import { ShieldStatus } from '@/generated/graphql'
import { hostShield, protectTargetShield, type ShieldLike } from './protectShield'

const HOST = '192.168.1.41'
const sh = (id: string, status: ShieldStatus, lanIp: string | null = HOST): ShieldLike => ({
  id,
  name: `shield-${id}`,
  status,
  lanIp,
})

describe('protectTargetShield (mirrors MarkProtecting)', () => {
  it('Fix 10: a resource created before its shield (nothing bound) is protectable', () => {
    const active = sh('a', ShieldStatus.Active)
    expect(protectTargetShield(HOST, null, [active])).toBe(active)
  })

  it('prefers the bound shield when it is active', () => {
    const bound = sh('b', ShieldStatus.Active)
    const other = sh('o', ShieldStatus.Active)
    expect(protectTargetShield(HOST, bound, [other, bound])).toBe(bound)
  })

  it('falls back to another active shield on the host when the bound one is not active', () => {
    const bound = sh('b', ShieldStatus.Revoked)
    const fresh = sh('n', ShieldStatus.Active)
    expect(protectTargetShield(HOST, bound, [bound, fresh])).toBe(fresh)
  })

  it('needs an ACTIVE shield whose LAN IP is the resource host', () => {
    expect(protectTargetShield(HOST, null, [])).toBeNull()
    expect(protectTargetShield(HOST, null, [sh('d', ShieldStatus.Disconnected)])).toBeNull()
    expect(protectTargetShield(HOST, null, [sh('x', ShieldStatus.Active, '192.168.1.99')])).toBeNull()
    expect(protectTargetShield(HOST, null, [sh('n', ShieldStatus.Active, null)])).toBeNull()
  })
})

describe('hostShield (what to describe when nothing is protectable)', () => {
  it('is the bound shield, else a shield on the host, else null', () => {
    const bound = sh('b', ShieldStatus.Disconnected, '10.0.0.1')
    const onHost = sh('h', ShieldStatus.Disconnected)
    expect(hostShield(HOST, bound, [onHost])).toBe(bound)
    expect(hostShield(HOST, null, [onHost])).toBe(onHost)
    expect(hostShield(HOST, null, [sh('x', ShieldStatus.Active, '10.0.0.9')])).toBeNull()
  })
})
