import { useState } from 'react'
import { Link } from 'react-router-dom'
import { listInvoices } from '@/api/billing'
import { Pager } from '@/components/Pager'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Card } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatMoney } from '@/lib/billing-money'
import { INVOICE_STATUS_LABELS, INVOICE_STATUS_VARIANTS, invoiceMonthLabel, invoiceTitle } from '@/lib/invoice-format'
import { useDetailQuery } from '@/lib/useDetailQuery'

// A tenant's invoices (Sprint 26 P4, D-66), newest month first, each linking
// to its detail. Super-admin only (invoice.read); read once per open and
// page (the list read is audited).

const PAGE_SIZE = 12

export function TenantInvoices({ tenantId }: { tenantId: string }) {
  const [cursors, setCursors] = useState<string[]>([''])
  const cursor = cursors[cursors.length - 1]
  const { state } = useDetailQuery(`tenant-invoices|${tenantId}|${cursor}`, () =>
    listInvoices({ tenant_id: tenantId, cursor: cursor || undefined, limit: PAGE_SIZE }),
  )
  let body
  if (state.status === 'loading') body = <Skeleton className="mx-5 mb-5 h-24" aria-label="Loading invoices" />
  else if (state.status === 'session_ended') return null
  else if (state.status !== 'ready') {
    body = (
      <Alert variant="destructive" className="mx-5 mb-5 w-auto">
        <AlertDescription>{state.status === 'forbidden' ? 'Invoices are available to super-admins only.' : 'Couldn’t load invoices.'}</AlertDescription>
      </Alert>
    )
  } else if (state.data.items.length === 0) {
    body = <p className="px-5 pb-5 text-sm text-muted-foreground">No invoices yet. A month is invoiced after it closes.</p>
  } else {
    const p = state.data
    body = (
      <>
        <Table aria-label="Tenant invoices">
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>Month</TableHead>
              <TableHead>Invoice</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="text-right">Total</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {p.items.map((i) => (
              <TableRow key={i.id}>
                <TableCell className="whitespace-nowrap">{invoiceMonthLabel(i.month)}</TableCell>
                <TableCell>
                  <Link to={`/invoices/${encodeURIComponent(i.id)}`} className="text-primary hover:underline" translate="no">
                    {invoiceTitle(i)}
                  </Link>
                </TableCell>
                <TableCell>
                  <Badge variant={INVOICE_STATUS_VARIANTS[i.status]}>{INVOICE_STATUS_LABELS[i.status]}</Badge>
                </TableCell>
                <TableCell className="text-right font-mono text-xs tabular-nums">{formatMoney(i.total_minor, i.currency)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        <Pager
          page={cursors.length}
          hasPrev={cursors.length > 1}
          hasNext={!!p.next_cursor}
          onPrev={() => setCursors((c) => c.slice(0, -1))}
          onNext={() => p.next_cursor && setCursors((c) => [...c, p.next_cursor!])}
        />
      </>
    )
  }
  return (
    <Card aria-labelledby="tenant-invoices-title">
      <h2 id="tenant-invoices-title" className="px-5 py-4 font-semibold">
        Invoices
      </h2>
      {body}
    </Card>
  )
}
