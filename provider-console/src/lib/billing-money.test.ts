import { describe, expect, it } from 'vitest'
import {
  formatIncluded,
  formatMoney,
  formatQuantity,
  formatRate,
  moneyInput,
  parseMoney,
  parseTaxPercent,
  parseUnits,
  taxPercentInput,
  unitName,
} from '@/lib/billing-money'

// The same cases as the controller's internal/billing/money_test.go, so the
// console and the invoice email print identical amounts.

describe('formatMoney', () => {
  it.each([
    [0, 'USD', 'USD 0.00'],
    [5, 'USD', 'USD 0.05'],
    [123456, 'USD', 'USD 1,234.56'],
    [118000, 'INR', 'INR 1,180.00'],
    [1000, 'JPY', 'JPY 1,000'],
    [1234, 'KWD', 'KWD 1.234'],
    [-250, 'EUR', 'EUR -2.50'],
    [Number.MAX_SAFE_INTEGER, 'USD', 'USD 90,071,992,547,409.91'],
  ])('%d %s → %s', (minor, cur, want) => {
    expect(formatMoney(minor, cur)).toBe(want)
  })
  it('refuses an unsafe number rather than printing a rounded amount', () => {
    expect(formatMoney(2 ** 60, 'USD')).toBe('USD —')
    expect(formatMoney(1.5, 'USD')).toBe('USD —')
  })
})

describe('parseMoney (string arithmetic, never floats)', () => {
  it.each([
    ['12.34', 'USD', 1234],
    ['12.3', 'USD', 1230],
    ['12', 'USD', 1200],
    ['0.10', 'USD', 10],
    ['0.1', 'USD', 10],
    ['1,000.50', 'INR', 100050],
    [' 7 ', 'USD', 700],
    ['1000', 'JPY', 1000],
    ['1.234', 'KWD', 1234],
    ['0', 'USD', 0],
    ['19.99', 'USD', 1999], // 19.99 * 100 = 1998.9999999999998 in floats
    ['0.29', 'USD', 29], //     0.29 * 100 = 28.999999999999996 in floats
  ])('%s %s → %d', (input, cur, want) => {
    expect(parseMoney(input, cur)).toBe(want)
  })
  it.each([
    ['', 'USD'],
    ['-1', 'USD'],
    ['1.234', 'USD'], // too many decimals
    ['1.5', 'JPY'],
    ['1e3', 'USD'],
    ['abc', 'USD'],
    ['1.2.3', 'USD'],
    ['99999999999999999', 'USD'], // beyond the safe range
  ])('refuses %s %s', (input, cur) => {
    expect(parseMoney(input, cur)).toBeNull()
  })
  it('round-trips through the form text', () => {
    for (const [minor, cur] of [[1234, 'USD'], [5, 'USD'], [1000, 'JPY'], [1234, 'KWD']] as const) {
      expect(parseMoney(moneyInput(minor, cur), cur)).toBe(minor)
    }
  })
})

describe('tax rate', () => {
  it.each([
    ['18', 1800],
    ['7.25', 725],
    ['7.5', 750],
    ['0', 0],
    ['100', 10000],
  ])('%s%% → %d bp', (input, bp) => {
    expect(parseTaxPercent(input)).toBe(bp)
    expect(parseTaxPercent(taxPercentInput(bp))).toBe(bp)
  })
  it.each(['7.255', '101', '-1', '', 'x'])('refuses %s', (input) => {
    expect(parseTaxPercent(input)).toBeNull()
  })
  it.each([
    [0, '0%'],
    [1800, '18%'],
    [725, '7.25%'],
    [750, '7.5%'],
    [5, '0.05%'],
    [10000, '100%'],
  ])('formatRate(%d) = %s', (bp, want) => {
    expect(formatRate(bp)).toBe(want)
  })
})

describe('meters', () => {
  it('formats quantities like the invoice email', () => {
    expect(formatQuantity('relay_bytes_to_clients', 123_456_789_012)).toBe('123.45 GB')
    expect(formatQuantity('relay_bytes_to_connectors', 9_999_999)).toBe('0.00 GB')
    expect(formatQuantity('session_minutes', 12345)).toBe('12,345 min')
    expect(formatQuantity('active_users', 1)).toBe('1 user')
    expect(formatQuantity('active_users', 7)).toBe('7 users')
  })
  it('names units', () => {
    expect(unitName('relay_bytes_to_clients', 1e9)).toBe('GB')
    expect(unitName('relay_bytes_to_clients', 5)).toBe('5 bytes')
    expect(unitName('connector_minutes', 60)).toBe('hour')
    expect(unitName('session_minutes', 1000)).toBe('1,000 min')
    expect(unitName('active_users', 1)).toBe('user')
  })
  it('formats included quantities in the price unit', () => {
    expect(formatIncluded('relay_bytes_to_clients', 50e9, 1e9)).toBe('50 GB')
    expect(formatIncluded('connector_minutes', 6000, 60)).toBe('100 hours')
    expect(formatIncluded('connector_minutes', 60, 60)).toBe('1 hour')
    expect(formatIncluded('active_users', 5, 1)).toBe('5 users')
    expect(formatIncluded('relay_bytes_to_clients', 1_500_000_000, 1e9)).toBe('1.50 GB')
  })
  it('parses whole display units into base units', () => {
    expect(parseUnits('50', 1e9)).toBe(50e9)
    expect(parseUnits('1,000', 60)).toBe(60000)
    expect(parseUnits('1.5', 1e9)).toBeNull()
    expect(parseUnits('-1', 1)).toBeNull()
    expect(parseUnits('9999999', 1e12)).toBeNull() // beyond the safe range
  })
})
