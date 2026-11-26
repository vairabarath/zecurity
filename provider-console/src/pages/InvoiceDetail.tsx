import { useState } from 'react'
import { ArrowLeft, Printer, RefreshCw } from 'lucide-react'
import { Link, Navigate, useParams } from 'react-router-dom'
import { getInvoice, issueInvoice, markInvoicePaid, resendInvoice, voidInvoice } from '@/api/billing'
import type { InvoiceDetail as InvoiceDetailData } from '@/api/types'
import { ConfirmDangerDialog, MAX_DANGER_REASON } from '@/components/ConfirmDangerDialog'
import { InvoicePrint } from '@/components/InvoicePrint'
import { PageHeader } from '@/components/PageHeader'
import { ReasonField } from '@/components/ReasonField'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatDateTime } from '@/lib/format'
import {
  EMAIL_KIND_LABELS,
  EMAIL_STATUS_LABELS,
  INVOICE_STATUS_LABELS,
  INVOICE_STATUS_VARIANTS,
  invoiceErrorMessage,
  invoiceMonthLabel,
  invoiceTitle,
  todayISODate,
} from '@/lib/invoice-format'
import { useDetailQuery } from '@/lib/useDetailQuery'

// Invoice detail (Sprint 26 P4; D-66, D-68). The invoice as it will print,
// its status history and its email deliveries (counts only, never
// addresses). Actions follow the lifecycle: a draft can be issued; an issued
// invoice can be marked paid, voided (reason + typed number) or emailed
// again; paid and void invoices can be emailed again. Every write carries a
// reason except Issue, where it is optional (P3 contract).

type Action = 'issue' | 'paid' | 'void' | 'resend'

export function InvoiceDetail() {
  const { id = '' } = useParams()
  const { state, reload } = useDetailQuery(id, getInvoice)
  const [action, setAction] = useState<Action | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  if (state.status === 'session_ended') return null
  if (state.status === 'forbidden') return <Navigate to="/forbidden" replace />
  if (state.status === 'loading') return <Skeleton className="h-96 w-full" aria-label="Loading invoice" />
  if (state.status === 'not_found') {
    return (
      <Alert variant="destructive">
        <AlertDescription>
          That invoice doesn’t exist.{' '}
          <Link to="/invoices" className="underline">
            Back to invoices
          </Link>
        </AlertDescription>
      </Alert>
    )
  }
  if (state.status === 'error') {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
          <span>Couldn’t load the invoice.</span>
          <Button variant="outline" size="sm" onClick={reload}>
            Retry
          </Button>
        </AlertDescription>
      </Alert>
    )
  }
  const inv = state.data
  const done = (text: string) => {
    setAction(null)
    setNotice(text)
    reload()
  }
  return (
    <div className="flex flex-col gap-6">
      <Link to={`/invoices?month=${inv.month.slice(0, 7)}`} className="inline-flex w-fit items-center gap-1 text-[13px] text-muted-foreground hover:text-foreground">
        <ArrowLeft className="size-3.5" aria-hidden />
        Invoices for {invoiceMonthLabel(inv.month)}
      </Link>
      <PageHeader
        title={
          <span className="inline-flex flex-wrap items-center gap-3">
            <span translate="no">{invoiceTitle(inv)}</span>
            <Badge variant={INVOICE_STATUS_VARIANTS[inv.status]}>{INVOICE_STATUS_LABELS[inv.status]}</Badge>
          </span>
        }
        description={`${inv.tenant_name} · ${invoiceMonthLabel(inv.month)} · plan ${inv.plan_name} (revision ${inv.plan_revision}) · ${inv.currency}`}
        actions={
          <>
            <Button variant="outline" onClick={reload} aria-label="Refresh invoice (records an audit entry)">
              <RefreshCw aria-hidden />
              Refresh
            </Button>
            <Button variant="outline" onClick={() => window.print()}>
              <Printer aria-hidden />
              Print
            </Button>
            {inv.status === 'draft' && <Button onClick={() => setAction('issue')}>Issue</Button>}
            {inv.status === 'issued' && (
              <>
                <Button onClick={() => setAction('paid')}>Mark paid</Button>
                <Button variant="outline" onClick={() => setAction('void')}>
                  Void
                </Button>
              </>
            )}
            {inv.status !== 'draft' && (
              <Button variant="outline" onClick={() => setAction('resend')}>
                Resend email
              </Button>
            )}
          </>
        }
      />
      {notice && (
        <Alert role="status">
          <AlertDescription>{notice}</AlertDescription>
        </Alert>
      )}
      {inv.purged && (
        <Alert>
          <AlertDescription>
            <Badge variant="secondary" className="mr-2">
              Deleted
            </Badge>
            This tenant was deleted; the invoice keeps its name and slug.
          </AlertDescription>
        </Alert>
      )}
      {inv.incomplete_hours > 0 && (
        <Alert role="note" className="border-warning/40 bg-warning/10">
          <AlertDescription>
            {inv.incomplete_hours} hour{inv.incomplete_hours === 1 ? ' was' : 's were'} not fully counted this month (for example, the controller was down).
            The invoice bills what was counted, never an estimate.
          </AlertDescription>
        </Alert>
      )}
      <InvoicePrint invoice={inv} providerName={inv.provider_name} />
      <div className="grid gap-6 lg:grid-cols-2">
        <History inv={inv} />
        <Emails inv={inv} />
      </div>
      {action === 'issue' && <IssueDialog inv={inv} onCancel={() => setAction(null)} onDone={done} />}
      {action === 'void' && <VoidDialog inv={inv} onCancel={() => setAction(null)} onDone={done} />}
      {action === 'resend' && <ResendDialog inv={inv} onCancel={() => setAction(null)} onDone={done} />}
      {action === 'paid' && <MarkPaidDialog inv={inv} onCancel={() => setAction(null)} onDone={done} />}
    </div>
  )
}

// A paid date is a calendar date (no time): shown in UTC so it never moves a
// day or gains a time of day in the viewer's zone. Its note is the payment
// reference the operator entered.
const paidDateFormat = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeZone: 'UTC' })
function formatPaidDate(at: string): string {
  const d = new Date(at)
  return Number.isNaN(d.getTime()) ? at : paidDateFormat.format(d)
}

function History({ inv }: { inv: InvoiceDetailData }) {
  return (
    <Card className="p-5">
      <h2 className="font-display text-base font-semibold">Status history</h2>
      {inv.history.length === 0 ? (
        <p className="mt-2 text-sm text-muted-foreground">No changes yet.</p>
      ) : (
        <ol className="mt-3 flex flex-col gap-2 text-sm" aria-label="Status history">
          {inv.history.map((h, n) => (
            <li key={`${h.status}-${h.at}-${n}`} className="flex flex-col">
              <span className="inline-flex flex-wrap items-center gap-2">
                <Badge variant={INVOICE_STATUS_VARIANTS[h.status]}>{INVOICE_STATUS_LABELS[h.status]}</Badge>
                <span className="text-[13px] text-muted-foreground">
                  {h.status === 'paid' ? formatPaidDate(h.at) : formatDateTime(h.at)} · <span className="break-all">{h.actor}</span>
                </span>
              </span>
              {h.reason && (
                <span className="break-words text-[13px] text-muted-foreground">
                  {h.status === 'paid' ? 'Reference' : 'Reason'}: {h.reason}
                </span>
              )}
            </li>
          ))}
        </ol>
      )}
    </Card>
  )
}

function Emails({ inv }: { inv: InvoiceDetailData }) {
  return (
    <Card className="p-5">
      <h2 className="font-display text-base font-semibold">Email to the tenant’s admins</h2>
      {inv.emails.length === 0 ? (
        <p className="mt-2 text-sm text-muted-foreground">
          {inv.status === 'draft' ? (inv.email_tenants ? 'Emailed when issued.' : 'Email to tenants is off in the billing settings.') : 'No email was sent.'}
        </p>
      ) : (
        <Table aria-label="Email deliveries" className="mt-2">
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>Email</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="text-right">Recipients</TableHead>
              <TableHead>Sent</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {inv.emails.map((e) => (
              <TableRow key={`${e.kind}-${e.seq}`}>
                <TableCell>{EMAIL_KIND_LABELS[e.kind]}</TableCell>
                <TableCell>
                  <span className={e.status === 'failed' || e.status === 'not_delivered' ? 'text-critical-text' : undefined}>{EMAIL_STATUS_LABELS[e.status]}</span>
                  {e.last_error && <span className="block break-words text-[12px] text-muted-foreground">{e.last_error}</span>}
                </TableCell>
                <TableCell className="text-right tabular-nums">{e.recipients_count}</TableCell>
                <TableCell className="text-[13px]">{e.sent_at ? formatDateTime(e.sent_at) : '—'}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

type DialogProps = { inv: InvoiceDetailData; onCancel: () => void; onDone: (text: string) => void }

/** Runs one invoice write for a ConfirmDangerDialog. */
function useInvoiceWrite(onDone: (text: string) => void) {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const run = async (fn: () => Promise<void>, text: string) => {
    setPending(true)
    setError(null)
    try {
      await fn()
      onDone(text)
    } catch (err) {
      setError(invoiceErrorMessage(err))
      setPending(false)
    }
  }
  return { pending, error: error ? { message: error } : undefined, run }
}

function IssueDialog({ inv, onCancel, onDone }: DialogProps) {
  const w = useInvoiceWrite(onDone)
  return (
    <ConfirmDangerDialog
      title={`Issue the invoice for ${inv.tenant_name}?`}
      description={
        <>
          It gets the next invoice number and can’t be changed afterwards (a correction is a void plus a new invoice).{' '}
          {inv.email_tenants ? 'The tenant’s admins are emailed the invoice.' : 'Email to tenants is off, so nobody is emailed.'}
        </>
      }
      confirmText=""
      requireConfirmText={false}
      reasonRequired={false}
      confirmLabel="Issue invoice"
      pendingLabel="Issuing…"
      pending={w.pending}
      error={w.error}
      onCancel={onCancel}
      onConfirm={(reason) => w.run(() => issueInvoice(inv.id, reason), 'Invoice issued.')}
    />
  )
}

function VoidDialog({ inv, onCancel, onDone }: DialogProps) {
  const w = useInvoiceWrite(onDone)
  const number = inv.number ?? ''
  return (
    <ConfirmDangerDialog
      title={`Void ${number}?`}
      description={
        <>
          The invoice stays on record as void and doesn’t need paying; its number is never reused. {inv.email_tenants ? 'The tenant’s admins get a void notice with the reason.' : ''}
        </>
      }
      confirmText={number}
      confirmLabel="Void invoice"
      pendingLabel="Voiding…"
      pending={w.pending}
      error={w.error}
      onCancel={onCancel}
      onConfirm={(reason) => w.run(() => voidInvoice(inv.id, reason), `${number} voided.`)}
    />
  )
}

function ResendDialog({ inv, onCancel, onDone }: DialogProps) {
  const w = useInvoiceWrite(onDone)
  return (
    <ConfirmDangerDialog
      title={`Email ${invoiceTitle(inv)} again?`}
      description="The tenant’s active admins get the invoice email again. Use this after a failed or lost delivery."
      confirmText=""
      requireConfirmText={false}
      confirmLabel="Resend email"
      pendingLabel="Queuing…"
      pending={w.pending}
      error={w.error}
      onCancel={onCancel}
      onConfirm={(reason) => w.run(() => resendInvoice(inv.id, reason), 'Email queued.')}
    />
  )
}

const MAX_REFERENCE = 200

function MarkPaidDialog({ inv, onCancel, onDone }: DialogProps) {
  const [paidAt, setPaidAt] = useState(() => todayISODate())
  const [reference, setReference] = useState('')
  const [reason, setReason] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const today = todayISODate()
  const ref = reference.trim()
  const why = reason.trim()
  const dateOk = /^\d{4}-\d{2}-\d{2}$/.test(paidAt) && paidAt <= today
  const refOk = Array.from(ref).length <= MAX_REFERENCE
  const valid = dateOk && refOk && why !== '' && Array.from(why).length <= MAX_DANGER_REASON
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!valid) return
    setPending(true)
    setError(null)
    try {
      await markInvoicePaid(inv.id, { paid_at: paidAt, reference: ref }, why)
      onDone(`${invoiceTitle(inv)} marked paid.`)
    } catch (err) {
      setError(invoiceErrorMessage(err))
      setPending(false)
    }
  }
  return (
    <Dialog open onOpenChange={(o) => !o && !pending && onCancel()}>
      <DialogContent className="sm:max-w-md">
        <form onSubmit={submit} className="flex flex-col gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>Mark {invoiceTitle(inv)} paid</DialogTitle>
            <DialogDescription>Record the payment you received. Paid is final; nobody is emailed.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="paid-at">Paid on</Label>
            <Input id="paid-at" name="paid_at" type="date" value={paidAt} max={today} onChange={(e) => setPaidAt(e.target.value)} aria-invalid={!dateOk} />
            {!dateOk && <p className="text-[12.5px] text-critical-text">Enter the date the payment arrived (not in the future).</p>}
          </div>
          <div className="grid gap-2">
            <Label htmlFor="paid-reference">Reference</Label>
            <Input
              id="paid-reference"
              name="reference"
              autoComplete="off"
              placeholder="Bank transfer UTR 1234…"
              value={reference}
              onChange={(e) => setReference(e.target.value)}
              aria-invalid={!refOk}
            />
            {!refOk && <p className="text-[12.5px] text-critical-text">Keep the reference to {MAX_REFERENCE} characters or fewer.</p>}
          </div>
          <ReasonField id="paid-reason" value={reason} onChange={setReason} />
          {error && (
            <Alert variant="destructive" aria-live="polite">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || pending}>
              {pending ? 'Saving…' : 'Mark paid'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
