import { describe, expect, it } from 'vitest'
import type { AuditQueryParams } from '@/api/reads'
import type { AuditEntry, Page } from '@/api/types'
import { collectAudit, csvCell, toCSV, toJSON } from './auditExport'

// Quick-win F38: client-side audit export.

function entry(i: number, over: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: `id-${String(i).padStart(5, '0')}`,
    created_at: new Date(Date.UTC(2026, 10, 1) - i * 1000).toISOString(),
    provider_user_id: 'u1',
    provider_email: 'boss@provider.test',
    action: 'relay.drain',
    target_type: 'relay',
    target_id: `r${i}`,
    details: { reason: 'maintenance' },
    ip_address: '203.0.113.9',
    ...over,
  }
}

/** A fake GET /provider/audit over `total` rows, newest first, keyset-paged by index. */
function fakeAudit(total: number) {
  const calls: AuditQueryParams[] = []
  const fetch = async (p: AuditQueryParams): Promise<Page<AuditEntry>> => {
    calls.push(p)
    const start = p.cursor ? Number(p.cursor) : 0
    const limit = Math.min(p.limit ?? 50, 200)
    const items = Array.from({ length: Math.max(0, Math.min(limit, total - start)) }, (_, k) => entry(start + k))
    const next = start + items.length < total ? String(start + items.length) : null
    return { items, next_cursor: next }
  }
  return { fetch, calls }
}

describe('collectAudit', () => {
  it('pages through every row once, in order, keeping the filters', async () => {
    const { fetch, calls } = fakeAudit(450)
    const r = await collectAudit({ action: 'relay.drain', since: '2026-10-01T00:00:00Z' }, { fetch })
    expect(r.truncated).toBe(false)
    expect(r.rows.map((e) => e.id)).toEqual(Array.from({ length: 450 }, (_, i) => entry(i).id))
    expect(calls.map((c) => c.cursor)).toEqual([undefined, '200', '400'])
    for (const c of calls) {
      expect(c.action).toBe('relay.drain')
      expect(c.since).toBe('2026-10-01T00:00:00Z')
      expect(c.limit).toBeLessThanOrEqual(200)
    }
  })

  it('stops at the limit and says so when more rows match', async () => {
    const { fetch } = fakeAudit(1000)
    const r = await collectAudit({}, { fetch, max: 300 })
    expect(r.rows).toHaveLength(300)
    expect(r.truncated).toBe(true)
  })

  it('exactly the limit is not reported as truncated', async () => {
    for (const total of [300, 400]) {
      const { fetch } = fakeAudit(total)
      const r = await collectAudit({}, { fetch, max: 400 })
      expect(r.rows).toHaveLength(total)
      expect(r.truncated).toBe(false)
    }
  })

  it('a page boundary at the limit peeks once to decide', async () => {
    const { fetch, calls } = fakeAudit(401)
    const r = await collectAudit({}, { fetch, max: 400 })
    expect(r.rows).toHaveLength(400)
    expect(r.truncated).toBe(true)
    expect(calls.length).toBeLessThanOrEqual(4)
  })

  it('a repeated entry fails the export instead of writing a duplicate', async () => {
    const fetch = async (p: AuditQueryParams): Promise<Page<AuditEntry>> =>
      p.cursor ? { items: [entry(1)], next_cursor: null } : { items: [entry(0), entry(1)], next_cursor: 'x' }
    await expect(collectAudit({}, { fetch })).rejects.toThrow(/returned twice/)
  })
})

describe('CSV', () => {
  it('neutralises formula cells, including after leading whitespace', () => {
    for (const bad of ['=HYPERLINK("x")', '+1', '-1+1', '@SUM(A1)', ' =1', '\t=1', '\r=1', '\tplain']) {
      expect(csvCell(bad).replace(/^"/, '').startsWith("'")).toBe(true)
    }
    expect(csvCell('relay.drain')).toBe('relay.drain')
    expect(csvCell('a,b')).toBe('"a,b"')
    expect(csvCell('say "hi"')).toBe('"say ""hi"""')
    expect(csvCell(null)).toBe('')
  })

  it('writes one header and one line per row; details as JSON text', () => {
    const csv = toCSV([entry(0, { details: { reason: '=cmd|calc' }, provider_email: '@attacker' })])
    const [header, line] = csv.split('\r\n')
    expect(header).toBe('created_at,provider_email,action,target_type,target_id,ip_address,details,id,provider_user_id')
    expect(line).toContain("'@attacker")
    expect(line).toContain('"{""reason"":""=cmd|calc""}"') // JSON starts with "{", not a formula
  })
})

describe('JSON', () => {
  it('round-trips the rows unchanged', () => {
    const rows = [entry(0), entry(1)]
    expect(JSON.parse(toJSON(rows))).toEqual(rows)
  })
})
