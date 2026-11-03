import { describe, expect, it } from 'vitest'
import { isValidResourceHost } from './resourceHost'

// Same cases as controller/internal/resource/host_validation_test.go
// (TestNormalizeHost), so the console and the server agree.
describe('isValidResourceHost (mirrors resource.NormalizeHost)', () => {
  it.each(['10.0.0.5', ' 10.0.0.5 ', '\t192.168.1.33\n', '127.0.0.1', '172.16.0.1', '8.8.8.8'])(
    'accepts %j',
    (h) => expect(isValidResourceHost(h)).toBe(true),
  )

  it.each([
    '', '   ',
    'db.internal', 'db.internal.acme.com', 'localhost', '*.acme.com',
    '10.0.0.5:22', '10.0.0.0/24', '10.0.0', '10.0.0.256', '010.0.0.5',
    '::1', 'fd00::5', '::ffff:10.0.0.5', 'fe80::1%eth0',
    '0.0.0.0', '255.255.255.255', '224.0.0.1', '239.255.255.250',
  ])('refuses %j', (h) => expect(isValidResourceHost(h)).toBe(false))
})
