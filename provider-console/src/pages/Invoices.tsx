import { useState } from 'react'
import { ChevronLeft, ChevronRight, Download, RefreshCw, RotateCcw } from 'lucide-react'
import { Link, useSearchParams } from 'react-router-dom'
import { getInvoice, listInvoices, regenerateInvoices } from '@/api/billing'
import type { Invoice, InvoiceListItem, InvoicePage, InvoiceStatus } from '@/api/types'
import { can } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { ConfirmDangerDialog } from '@/components/ConfirmDangerDialog'
import { FilterChips } from '@/components/FilterChips'
import { PageHeader } from '@/components/PageHeader'
import { Pager } from '@/components/Pager'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { download } from '@/lib/auditExport'
import { formatMoney } from '@/lib/billing-money'
import { toInvoiceLinesCSV, toInvoicesCSV, toInvoicesJSON } from '@/lib/invoiceExport'
import {
  EMAIL_STATUS_LABELS,
  INVOICE_STATUSES,
  INVOICE_STATUS_LABELS,
  INVOICE_STATUS_VARIANTS,
  invoiceErrorMessage,
  invoiceTitle,
} from '@/lib/invoice-format'
import { addMonths, monthKey, monthLabel, monthStart } from '@/lib/usage-format'
import { useDetailQuery } from '@/lib/useDetailQuery'

// Invoices (Sprint 26 P4; D-66, D-68, D-70). Super-admin only
// (invoice.read / invoice.write; relay-ops get 403). Every list request is
// audited (invoice.fleet.read), so the page reads once per month, filter or
// page and on Refresh; never by polling.
//
// URL: ?month=YYYY-MM (default: last month: a month is invoiced after it
// closes) &status=draft|issued|paid|void.

const STATUS_CHIPS = [{ value: '', label: 'All' }, ...INVOICE_STATUSES.map((s) => ({ value: s, label: INVOICE_STATUS_LABELS[s] }))]
const PAGE_SIZE = 50
/** Export reads each invoice's detail for its lines (one audited read each), so it is capped. */
export const EXPORT_MAX = 500

export function Invoices() {
  const [sp, setSp] = useSearchParams()
  const [now] = useState(() => new Date())
  const current = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1))
  const parsed = monthStart(sp.get('month') ?? '')
  const month = parsed && parsed.getTime() <= current.getTime() ? parsed : addMonths(current, -1)
  const rawStatus = sp.get('status') ?? ''
  const status = (INVOICE_STATUSES as readonly string[]).includes(rawStatus) ? (rawStatus as InvoiceStatus) : undefined
  const [round, setRound] = useState(0)
  const [notice, setNotice] = useState<string | null>(null)

  const set = (key: string, value: string | null) => {
    const p = new URLSearchParams(sp)
    if (value) p.set(key, value)
    else p.delete(key)
    setSp(p, { replace: true })
  }
  const mk = monthKey(month)
  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Invoices"
        description="One invoice per tenant per UTC month, made after the month closes from the tenant’s month-start plan card and its counted usage. Review drafts, then issue them; an issued invoice never changes."
        actions={
          <Button
            variant="outline"
            onClick={() => {
              setNotice(null)
              setRound((n) => n + 1)
            }}
            aria-label="Refresh invoices (records an audit entry)"
          >
            <RefreshCw aria-hidden />
            Refresh
          </Button>
        }
      />
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" aria-label="Previous month" onClick={() => set('month', monthKey(addMonths(month, -1)))}>
            <ChevronLeft aria-hidden />
          </Button>
          <h2 className="text-sm font-medium" aria-live="polite">
            {monthLabel(month)}
          </h2>
          <Button
            variant="outline"
            size="sm"
            aria-label="Next month"
            disabled={month.getTime() >= current.getTime()}
            onClick={() => set('month', monthKey(addMonths(month, 1)))}
          >
            <ChevronRight aria-hidden />
          </Button>
        </div>
        <ExportButtons month={mk} />
      </div>
      <FilterChips label="Status" options={STATUS_CHIPS} value={status ?? ''} onChange={(v) => set('status', v || null)} />
      {notice && (
        <Alert role="status">
          <AlertDescription>{notice}</AlertDescription>
        </Alert>
      )}
      {/* keyed: a new month, filter or Refresh starts at the first page. */}
      <InvoiceTable
        key={`${mk}|${status ?? ''}|${round}`}
        month={mk}
        status={status}
        round={round}
        onRegenerated={(text) => {
          setNotice(text)
          setRound((n) => n + 1)
        }}
      />
    </div>
  )
}

function InvoiceTable({
  month,
  status,
  round,
  onRegenerated,
}: {
  month: string
  status: InvoiceStatus | undefined
  round: number
  onRegenerated: (text: string) => void
}) {
  const [cursors, setCursors] = useState<string[]>([''])
  const [regenerating, setRegenerating] = useState(false)
  // Sprint 27 O2: billing and super-admins write invoices; auditor reads only.
  const canWrite = can(useSessionStore((s) => s.me?.role), 'invoice.write')
  const cursor = cursors[cursors.length - 1]
  const { state } = useDetailQuery(`invoices|${month}|${status ?? ''}|${round}|${cursor}`, () =>
    listInvoices({ month, status, cursor: cursor || undefined, limit: PAGE_SIZE }),
  )
  if (state.status === 'loading') return <Skeleton className="h-40 w-full" aria-label="Loading invoices" />
  if (state.status === 'session_ended') return null
  if (state.status !== 'ready') {
    return (
      <Alert variant="destructive">
        <AlertDescription>
          {state.status === 'forbidden' ? 'Invoices are available to super-admins only.' : 'Couldn’t load invoices. Use Refresh to try again.'}
        </AlertDescription>
      </Alert>
    )
  }
  const p = state.data
  const drafts = p.items.filter((i) => i.status === 'draft')
  return (
    <>
      <Totals page={p} />
      <Card>
        <div className="flex flex-wrap items-center justify-between gap-3 px-4 pt-4">
          <span className="text-[13px] text-muted-foreground">
            {p.items.length === 0 ? 'No invoices for this month yet.' : `${p.items.length} invoice${p.items.length === 1 ? '' : 's'} on this page.`}
          </span>
          {canWrite && (
            <Button variant="outline" size="sm" onClick={() => setRegenerating(true)}>
              <RotateCcw aria-hidden />
              Regenerate drafts
            </Button>
          )}
        </div>
        {p.items.length > 0 && (
          <div className="overflow-x-auto">
            <Table aria-label="Invoices">
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Invoice</TableHead>
                  <TableHead>Tenant</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead className="text-right">Total</TableHead>
                  <TableHead className="text-right">Incomplete hours</TableHead>
                  <TableHead>Email</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {p.items.map((i) => (
                  <InvoiceRow key={i.id} i={i} />
                ))}
              </TableBody>
            </Table>
          </div>
        )}
        <Pager
          page={cursors.length}
          hasPrev={cursors.length > 1}
          hasNext={!!p.next_cursor}
          onPrev={() => setCursors((c) => c.slice(0, -1))}
          onNext={() => p.next_cursor && setCursors((c) => [...c, p.next_cursor!])}
        />
      </Card>
      {regenerating && (
        <RegenerateDialog month={month} drafts={drafts} more={!!p.next_cursor || cursors.length > 1} onCancel={() => setRegenerating(false)} onDone={onRegenerated} />
      )}
    </>
  )
}

function Totals({ page }: { page: InvoicePage }) {
  const rows = INVOICE_STATUSES.filter((s) => page.totals[s]?.count)
  if (rows.length === 0) return null
  return (
    <dl className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4" aria-label="Totals per status">
      {rows.map((s) => {
        const t = page.totals[s]!
        return (
          <Card key={s} className="flex flex-col gap-1 p-4">
            <dt className="flex items-center gap-2 text-[13px] text-muted-foreground">
              <Badge variant={INVOICE_STATUS_VARIANTS[s]}>{INVOICE_STATUS_LABELS[s]}</Badge>
              {t.count.toLocaleString()} invoice{t.count === 1 ? '' : 's'}
            </dt>
            <dd className="font-display text-xl font-semibold tabular-nums">{formatMoney(t.total_minor, page.currency)}</dd>
            <dd className="text-[12.5px] text-muted-foreground tabular-nums">
              Subtotal {formatMoney(t.subtotal_minor, page.currency)} · tax {formatMoney(t.tax_minor, page.currency)}
            </dd>
          </Card>
        )
      })}
    </dl>
  )
}

function InvoiceRow({ i }: { i: InvoiceListItem }) {
  return (
    <TableRow aria-label={`${invoiceTitle(i)} ${i.tenant_name}`}>
      <TableCell className="whitespace-nowrap">
        <Link to={`/invoices/${encodeURIComponent(i.id)}`} className="font-medium text-primary hover:underline" translate="no">
          {invoiceTitle(i)}
        </Link>
      </TableCell>
      <TableCell>
        <span className="inline-flex min-w-0 flex-wrap items-center gap-2">
          {i.purged ? (
            <span className="break-words">{i.tenant_name}</span>
          ) : (
            <Link to={`/tenants/${encodeURIComponent(i.tenant_id)}`} className="break-words text-primary hover:underline">
              {i.tenant_name}
            </Link>
          )}
          {i.purged && <Badge variant="secondary">Deleted</Badge>}
        </span>
      </TableCell>
      <TableCell>
        <Badge variant={INVOICE_STATUS_VARIANTS[i.status]}>{INVOICE_STATUS_LABELS[i.status]}</Badge>
      </TableCell>
      <TableCell className="text-right font-mono text-xs tabular-nums">{formatMoney(i.total_minor, i.currency)}</TableCell>
      <TableCell className="text-right font-mono text-xs tabular-nums">
        {i.incomplete_hours > 0 ? <span className="text-warning-text">{i.incomplete_hours}</span> : '0'}
      </TableCell>
      <TableCell className="text-[13px]">
        {i.last_email_status ? (
          <span className={i.last_email_status === 'failed' || i.last_email_status === 'not_delivered' ? 'text-critical-text' : undefined}>
            {EMAIL_STATUS_LABELS[i.last_email_status]}
          </span>
        ) : (
          <span className="text-muted-foreground">—</span>
        )}
      </TableCell>
    </TableRow>
  )
}

function RegenerateDialog({
  month,
  drafts,
  more,
  onCancel,
  onDone,
}: {
  month: string
  drafts: InvoiceListItem[]
  more: boolean
  onCancel: () => void
  onDone: (text: string) => void
}) {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  return (
    <ConfirmDangerDialog
      title={`Regenerate the drafts for ${monthLabel(monthStart(month)!)}?`}
      description={
        <span className="flex flex-col gap-2">
          <span>
            Drafts are rebuilt from each tenant’s month-start plan card, the current tax settings and the counted usage. Issued, paid and void invoices
            never change; tenants without an invoice get a draft.
          </span>
          {drafts.length > 0 ? (
            <span>
              Drafts that will change{more ? ' (on this page; others may change too)' : ''}:{' '}
              <span className="font-medium">{drafts.map((d) => d.tenant_name).join(', ')}</span>
            </span>
          ) : (
            <span>No drafts on this page.</span>
          )}
        </span>
      }
      confirmText={month}
      requireConfirmText={false}
      confirmLabel="Regenerate drafts"
      pendingLabel="Regenerating…"
      pending={pending}
      error={error ? { message: error } : undefined}
      onCancel={onCancel}
      onConfirm={async (reason) => {
        setPending(true)
        setError(null)
        try {
          const r = await regenerateInvoices(month, reason)
          onDone(`${r.regenerated} draft${r.regenerated === 1 ? '' : 's'} regenerated.`)
        } catch (err) {
          setError(invoiceErrorMessage(err))
          setPending(false)
        }
      }}
    />
  )
}

/** Every invoice of the month with its lines, up to EXPORT_MAX (each detail read is audited). */
async function readMonth(month: string): Promise<{ invoices: Invoice[]; truncated: boolean }> {
  const items: InvoiceListItem[] = []
  let cursor: string | undefined
  let truncated = false
  for (;;) {
    const page = await listInvoices({ month, cursor, limit: 200 })
    items.push(...page.items)
    if (items.length >= EXPORT_MAX) {
      truncated = items.length > EXPORT_MAX || !!page.next_cursor
      break
    }
    if (!page.next_cursor) break
    cursor = page.next_cursor
  }
  const invoices: Invoice[] = []
  for (const i of items.slice(0, EXPORT_MAX)) invoices.push(await getInvoice(i.id))
  return { invoices, truncated }
}

function ExportButtons({ month }: { month: string }) {
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState<string | null>(null)
  const run = async (format: 'csv' | 'lines' | 'json') => {
    setBusy(true)
    setMessage(null)
    try {
      const { invoices, truncated } = await readMonth(month)
      if (truncated) setMessage(`Exported the first ${EXPORT_MAX} invoices only.`)
      if (format === 'csv') download(`invoices-${month}.csv`, 'text/csv;charset=utf-8', toInvoicesCSV(invoices))
      else if (format === 'lines') download(`invoice-lines-${month}.csv`, 'text/csv;charset=utf-8', toInvoiceLinesCSV(invoices))
      else download(`invoices-${month}.json`, 'application/json', toInvoicesJSON(invoices))
    } catch {
      setMessage('Export failed. Try again.')
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="flex flex-wrap items-center gap-2">
      {message && (
        <span className="text-[12.5px] text-muted-foreground" role="status">
          {message}
        </span>
      )}
      {busy && (
        <span className="text-[12.5px] text-muted-foreground" role="status">
          Exporting…
        </span>
      )}
      <Button variant="outline" size="sm" disabled={busy} onClick={() => run('csv')} aria-label="Export invoices as CSV">
        <Download aria-hidden />
        CSV
      </Button>
      <Button variant="outline" size="sm" disabled={busy} onClick={() => run('lines')} aria-label="Export invoice lines as CSV">
        <Download aria-hidden />
        Lines CSV
      </Button>
      <Button variant="outline" size="sm" disabled={busy} onClick={() => run('json')} aria-label="Export invoices as JSON">
        <Download aria-hidden />
        JSON
      </Button>
    </div>
  )
}
