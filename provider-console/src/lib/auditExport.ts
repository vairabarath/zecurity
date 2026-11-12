import { queryAudit, type AuditQueryParams } from '@/api/reads'
import type { AuditEntry, Page } from '@/api/types'

// Provider audit export (quick-win F38, lite): client-side, from the same
// GET /provider/audit the page reads, so the same authorization (audit.view,
// super-admin) and the same fields apply; nothing is added or unredacted.
//
// - Filters and order: the applied filters, newest first, as the page shows.
// - Pagination: follows the server's keyset cursor (created_at, id), so pages
//   neither repeat nor skip rows, even while new entries arrive (they are
//   newer than the first page). Ids are still checked: a repeat stops the
//   export with an error rather than writing a duplicate.
// - Limit: at most EXPORT_MAX rows; when more match, the result says so.
// - CSV cells that a spreadsheet would run as a formula are neutralised.

export const EXPORT_MAX = 10_000
const PAGE_SIZE = 200 // the API's maximum page

export type AuditFilters = Omit<AuditQueryParams, 'limit' | 'cursor'>

export interface AuditExport {
  rows: AuditEntry[]
  /** More rows matched than EXPORT_MAX. */
  truncated: boolean
}

type Fetch = (params: AuditQueryParams) => Promise<Page<AuditEntry>>

export async function collectAudit(
  filters: AuditFilters,
  opts: { max?: number; fetch?: Fetch; onProgress?: (n: number) => void } = {},
): Promise<AuditExport> {
  const max = opts.max ?? EXPORT_MAX
  const getPage = opts.fetch ?? queryAudit
  const rows: AuditEntry[] = []
  const seen = new Set<string>()
  let cursor: string | undefined
  for (;;) {
    const page = await getPage({ ...filters, limit: Math.min(PAGE_SIZE, max - rows.length + 1), cursor })
    for (const e of page.items) {
      if (seen.has(e.id)) throw new Error(`audit export: entry ${e.id} returned twice`)
      seen.add(e.id)
      if (rows.length === max) return { rows, truncated: true }
      rows.push(e)
    }
    opts.onProgress?.(rows.length)
    if (!page.next_cursor) return { rows, truncated: false }
    if (rows.length === max) {
      // Exactly max rows so far; one more page tells whether more exist.
      const peek = await getPage({ ...filters, limit: 1, cursor: page.next_cursor })
      return { rows, truncated: peek.items.length > 0 }
    }
    cursor = page.next_cursor
  }
}

const COLUMNS = [
  'created_at',
  'provider_email',
  'action',
  'target_type',
  'target_id',
  'ip_address',
  'details',
  'id',
  'provider_user_id',
] as const

/**
 * One CSV cell. A value a spreadsheet would treat as a formula (starting,
 * after any leading whitespace, with = + - @, or with a tab or carriage
 * return) is prefixed with an apostrophe so it stays text.
 */
export function csvCell(value: unknown): string {
  let s = value === null || value === undefined ? '' : typeof value === 'string' ? value : JSON.stringify(value)
  if (/^[\t\r]/.test(s) || /^\s*[=+\-@]/.test(s)) s = `'${s}`
  return /[",\r\n]/.test(s) || s !== s.trim() ? `"${s.replace(/"/g, '""')}"` : s
}

export function toCSV(rows: AuditEntry[]): string {
  const lines = [COLUMNS.join(',')]
  for (const r of rows) lines.push(COLUMNS.map((c) => csvCell(r[c])).join(','))
  return lines.join('\r\n') + '\r\n'
}

export function toJSON(rows: AuditEntry[]): string {
  return JSON.stringify(rows, null, 2) + '\n'
}

/** Offer content as a file download (nothing is stored). */
export function download(filename: string, type: string, content: string) {
  const url = URL.createObjectURL(new Blob([content], { type }))
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.rel = 'noopener'
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}
