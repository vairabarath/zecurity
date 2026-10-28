import { describe, expect, it } from 'vitest'
import { buildInstallCommands, checkDnsSan, checkList, checkIpSan, parseList, shellQuote, type InstallParams } from './relay-install'

const P: InstallParams = {
  relayId: '0b9e7f3c-1d2a-4c55-9a77-2f1c3e4d5a6b',
  provisioningToken: 'fake-provisioning-token-for-tests-0002',
  caFingerprint: 'a'.repeat(64),
  controllerGrpcAddr: 'controller.example.com:9090',
  controllerHttpAddr: 'controller.example.com:8080',
  dnsSans: ['relay-1.example.com', 'relay-1.internal'],
  ipSans: ['203.0.113.5'],
}

describe('parseList', () => {
  it('splits on commas, whitespace and newlines and drops empties', () => {
    expect(parseList(' a.example.com, b.example.com\n\nc.example.com  ')).toEqual(['a.example.com', 'b.example.com', 'c.example.com'])
    expect(parseList('   ')).toEqual([])
  })
})

describe('checkDnsSan mirrors the server (validateDNSSANs)', () => {
  it.each(['relay.example.com', 'relay-1.internal', 'localhost'])('accepts %s', (v) => expect(checkDnsSan(v)).toBeNull())
  it.each([
    ['Relay.example.com', /lowercase/i],
    ['*.example.com', /wildcard/i],
    ['relay/1', /space|slash/i],
  ])('rejects %s', (v, re) => expect(checkDnsSan(v)).toMatch(re))
})

describe('checkIpSan mirrors the server (canonical net.IP form)', () => {
  it.each(['203.0.113.5', '10.0.0.1', '2001:db8::1', '::1'])('accepts %s', (v) => expect(checkIpSan(v)).toBeNull())
  it.each(['10.0.0.01', '256.1.1.1', '1.2.3', '2001:DB8::1', '2001:0db8::1', 'relay.example.com', '::ffff:1.2.3.4'])(
    'rejects %s',
    (v) => expect(checkIpSan(v)).not.toBeNull(),
  )
})

describe('shellQuote', () => {
  it('single-quotes and escapes embedded quotes', () => {
    expect(shellQuote('abc')).toBe(`'abc'`)
    expect(shellQuote(`a'b`)).toBe(`'a'\\''b'`)
  })
})

describe('buildInstallCommands', () => {
  it('the installer command carries every required variable, quoted', () => {
    const { installer } = buildInstallCommands(P)
    expect(installer).toContain(`CONTROLLER_ADDR='controller.example.com:9090'`)
    expect(installer).toContain(`CONTROLLER_HTTP_ADDR='controller.example.com:8080'`)
    expect(installer).toContain(`RELAY_ID='${P.relayId}'`)
    expect(installer).toContain(`RELAY_CA_FINGERPRINT='${'a'.repeat(64)}'`)
    expect(installer).toContain(`RELAY_PROVISIONING_TOKEN='${P.provisioningToken}'`)
    expect(installer).toContain(`RELAY_DNS_SANS='relay-1.example.com,relay-1.internal'`)
    expect(installer).toContain(`RELAY_IP_SANS='203.0.113.5'`)
    expect(installer).toMatch(/^sudo /)
    expect(installer).toMatch(/\.\/scripts\/relay-local-install\.sh \.\/zecurity-relay$/)
  })

  it('the direct command runs the binary with the same values', () => {
    const { direct } = buildInstallCommands(P)
    expect(direct).toContain(`RELAY_ID='${P.relayId}'`)
    expect(direct).toContain(`RELAY_PROVISIONING_TOKEN='${P.provisioningToken}'`)
    expect(direct).toContain(`RELAY_CA_FINGERPRINT='${'a'.repeat(64)}'`)
    expect(direct).toMatch(/zecurity-relay$/)
    expect(direct).not.toContain('relay-local-install.sh')
  })

  it('omits SAN variables that have no values', () => {
    const { installer, direct } = buildInstallCommands({ ...P, dnsSans: [], ipSans: [] })
    for (const cmd of [installer, direct]) {
      expect(cmd).not.toContain('RELAY_DNS_SANS')
      expect(cmd).not.toContain('RELAY_IP_SANS')
    }
  })

  it('unconfigured controller addresses become visible placeholders', () => {
    const r = buildInstallCommands({ ...P, controllerGrpcAddr: '', controllerHttpAddr: '' })
    expect(r.installer).toContain(`CONTROLLER_ADDR='<controller-host>:9090'`)
    expect(r.installer).toContain(`CONTROLLER_HTTP_ADDR='<controller-host>:8080'`)
    expect(r.placeholders).toBe(true)
    expect(buildInstallCommands(P).placeholders).toBe(false)
  })

  it('one variable per line, joined with line continuations', () => {
    const lines = buildInstallCommands(P).installer.split('\n')
    expect(lines.slice(0, -1).every((l) => l.endsWith(' \\'))).toBe(true)
  })
})

describe('checkList', () => {
  it('reports the first invalid or repeated entry', () => {
    expect(checkList(['a.example.com', 'b.example.com'], checkDnsSan)).toBeNull()
    expect(checkList(['a.example.com', 'A.example.com'], checkDnsSan)).toMatch(/^A\.example\.com: .*lowercase/i)
    expect(checkList(['10.0.0.1', '10.0.0.1'], checkIpSan)).toBe('10.0.0.1 is listed twice.')
  })
})
