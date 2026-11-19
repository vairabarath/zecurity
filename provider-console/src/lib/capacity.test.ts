import { describe, expect, it } from 'vitest'
import { formatLinkSpeed, formatUtilization, linkSpeedInput, MAX_LINK_CAPACITY_BPS, parseLinkSpeed } from './capacity'

// Links are configured in bits per second; the API stores bytes per second
// (Sprint 23 B2, D-42). 1 Gbps = 125,000,000 B/s.
describe('link capacity units', () => {
  it('parses Mbps and Gbps into bytes per second', () => {
    expect(parseLinkSpeed('1', 'Gbps')).toBe(125_000_000)
    expect(parseLinkSpeed('100', 'Mbps')).toBe(12_500_000)
    expect(parseLinkSpeed('2.5', 'Gbps')).toBe(312_500_000)
    expect(parseLinkSpeed(' 10 ', 'Gbps')).toBe(1_250_000_000)
  })

  it('refuses zero, negatives, junk and anything above 400 Gbps', () => {
    for (const v of ['0', '-1', '', 'abc', '1e3', '1,000', '401']) {
      expect(parseLinkSpeed(v, 'Gbps'), v).toBeNull()
    }
    expect(parseLinkSpeed('400', 'Gbps')).toBe(MAX_LINK_CAPACITY_BPS)
  })

  it('formats bytes per second as a link speed in bits', () => {
    expect(formatLinkSpeed(125_000_000)).toBe('1 Gbps')
    expect(formatLinkSpeed(12_500_000)).toBe('100 Mbps')
    expect(formatLinkSpeed(312_500_000)).toBe('2.5 Gbps')
  })

  it('prefills the form in whole Gbps when exact, else Mbps', () => {
    expect(linkSpeedInput(null)).toEqual({ value: '', unit: 'Gbps' })
    expect(linkSpeedInput(125_000_000)).toEqual({ value: '1', unit: 'Gbps' })
    expect(linkSpeedInput(12_500_000)).toEqual({ value: '100', unit: 'Mbps' })
  })

  it('shows utilization as a whole percentage', () => {
    expect(formatUtilization(0.88)).toBe('88 %')
    expect(formatUtilization(1.2)).toBe('120 %')
  })
})
