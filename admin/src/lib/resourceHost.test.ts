import { describe, expect, it } from 'vitest'
import { isNameHost, isValidResourceHost, resourceHostKind } from './resourceHost'

// Same cases as controller/internal/resource/host_validation_test.go
// (TestNormalizeHost), so the console and the server agree.
describe('resourceHostKind (mirrors resource.NormalizeHost)', () => {
  it.each([
    ['10.0.0.5', 'ipv4'], [' 10.0.0.5 ', 'ipv4'], ['\t192.168.1.33\n', 'ipv4'], ['127.0.0.1', 'ipv4'], ['8.8.8.8', 'ipv4'],
    ['db.internal', 'name'], ['DB.Internal.Acme.COM', 'name'], ['db.internal.acme.com.', 'name'],
    [' api-1.svc.cluster.local ', 'name'], ['10-0-0-5.nip.io', 'name'], ['123.example', 'name'], ['x.y2k', 'name'],
    ['a'.repeat(63) + '.io', 'name'],
  ])('accepts %j as %s', (h, kind) => expect(resourceHostKind(h)).toBe(kind))

  it.each([
    '', '   ', '.',
    'localhost', 'fileserver',
    '*.acme.com', 'db.*.acme.com',
    'db_1.acme.com', 'db .acme.com', 'db..acme.com', '.acme.com', 'acme.com..',
    '-db.acme.com', 'db-.acme.com', 'db.acme.123', '1.2.3.4.5', '10.0.0.256', '010.0.0.5', '10.0.0',
    'a'.repeat(64) + '.io', 'a.'.repeat(127) + 'io',
    '10.0.0.5:22', '10.0.0.0/24', 'db.acme.com:5432', 'http://db.acme.com',
    '::1', 'fd00::5', '::ffff:10.0.0.5', 'fe80::1%eth0',
    '0.0.0.0', '255.255.255.255', '224.0.0.1', '239.255.255.250',
  ])('refuses %j', (h) => {
    expect(resourceHostKind(h)).toBeNull()
    expect(isValidResourceHost(h)).toBe(false)
  })

  it('isNameHost tells stored hosts apart like the server', () => {
    expect(isNameHost('10.0.0.5')).toBe(false)
    expect(isNameHost('db.internal')).toBe(true)
    expect(isNameHost('123.example')).toBe(true)
  })
})
