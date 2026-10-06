import { useCallback, useState, type FormEvent } from 'react'
import { RefreshCw } from 'lucide-react'
import { Link } from 'react-router-dom'
import { getCertificates } from '@/api/reads'
import {
  CERTIFICATE_KINDS,
  EXPIRY_BUCKETS,
  type CertificateEntry,
  type CertificateExpiryResult,
  type CertificateKind,
  type CertificateSummary,
} from '@/api/types'
import { DataState } from '@/components/DataState'
import { ExpiryBadge } from '@/components/ExpiryBadge'
import { Pager } from '@/components/Pager'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { BUCKET_VARIANTS } from '@/lib/expiry'
import { formatDateTime } from '@/lib/format'
import { usePagedQuery, type Filters } from '@/lib/usePagedQuery'

// Certificates (Phase P, cert.read: super-admin only). Read-only.
//
// Renders GET /provider/certificates as returned:
//   - the summary tiles are the API's summary (over everything matching
//     kind/tenant, ignoring `within`); nothing is recounted here;
//   - rows keep the API order (soonest expiry first) and its per-row
//     bucket; the API reports no issue date, current or revocation state
//     (revoked certificates are excluded server-side), so none is shown
//     or inferred;
//   - client devices have no entity_name (privacy), shown as "—".
// Relay rows link to relay detail; tenant ids link to tenant detail on an
// explicit click only (tenant detail is an audited read): nothing here
// prefetches or looks up a tenant.

const FILTER_KEYS = ['within', 'kind', 'tenant_id'] as const
const DEFAULT = 'default' // Select sentinel: no `within` sent, the server default (720h) applies
const ANY = 'any'
const MAX_WITHIN_SECONDS = 8760 * 3600

/** Listing windows offered in the UI (all within the server's 8760h maximum). */
const WITHIN_OPTIONS = [
  { value: '24h', label: 'Next 24 hours' },
  { value: '168h', label: 'Next 7 days' },
  { value: DEFAULT, label: 'Next 30 days (default)' },
  { value: '2160h', label: 'Next 90 days' },
  { value: '8760h', label: 'Next 365 days (maximum)' },
] as const

const UUID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/

/** Seconds in a Go-style duration made of h/m/s parts, or null if malformed. */
function durationSeconds(v: string): number | null {
  const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$/.exec(v)
  if (!m || v === '') return null
  return Number(m[1] ?? 0) * 3600 + Number(m[2] ?? 0) * 60 + Number(m[3] ?? 0)
}

function validFilters(f: Filters): boolean {
  if (f.within !== undefined) {
    const s = durationSeconds(f.within)
    if (s === null || s <= 0 || s > MAX_WITHIN_SECONDS) return false
  }
  if (f.kind !== undefined && !(CERTIFICATE_KINDS as readonly string[]).includes(f.kind)) return false
  if (f.tenant_id !== undefined && !UUID.test(f.tenant_id)) return false
  return true
}

function fetchCertificates(f: Filters, cursor: string | undefined): Promise<CertificateExpiryResult> {
  return getCertificates({ within: f.within, kind: f.kind as CertificateKind | undefined, tenant_id: f.tenant_id, cursor })
}

const TILE_LABELS: Record<keyof CertificateSummary, string> = {
  expired: 'Expired',
  lt_24h: '< 24 hours',
  lt_7d: '< 7 days',
  lt_30d: '< 30 days',
  ok: 'OK',
}

export function Certificates() {
  const q = usePagedQuery<CertificateExpiryResult>({ filterKeys: FILTER_KEYS, validate: validFilters, fetchPage: fetchCertificates })
  const set = useCallback(
    (k: 'within' | 'kind' | 'tenant_id', v: string | undefined) => {
      const next: Filters = { ...q.filters }
      if (v) next[k] = v
      else delete next[k]
      q.setFilters(next)
    },
    [q],
  )

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="font-display text-2xl font-semibold tracking-tight">Certificates</h1>
          <p className="text-sm text-muted-foreground">Certificate expiry across the platform and every tenant.</p>
        </div>
        <Button variant="outline" onClick={q.retry} aria-label="Refresh certificates">
          <RefreshCw aria-hidden />
          Refresh
        </Button>
      </div>

      <div className="flex flex-wrap items-end gap-3">
        <div className="grid gap-1.5">
          <Label htmlFor="cert-within">Listing window</Label>
          <Select value={q.filters.within ?? DEFAULT} onValueChange={(v) => set('within', v === DEFAULT ? undefined : v)}>
            <SelectTrigger id="cert-within" aria-label="Listing window" className="w-56">
              <SelectValue placeholder={q.filters.within} />
            </SelectTrigger>
            <SelectContent>
              {WITHIN_OPTIONS.map((o) => (
                <SelectItem key={o.value} value={o.value}>
                  {o.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="cert-kind">Kind</Label>
          <Select value={q.filters.kind ?? ANY} onValueChange={(v) => set('kind', v === ANY ? undefined : v)}>
            <SelectTrigger id="cert-kind" aria-label="Kind filter" className="w-44">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ANY}>All kinds</SelectItem>
              {CERTIFICATE_KINDS.map((k) => (
                <SelectItem key={k} value={k}>
                  {k}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <TenantFilter key={q.filters.tenant_id ?? ''} applied={q.filters.tenant_id ?? ''} onApply={(v) => set('tenant_id', v || undefined)} />
      </div>

      <DataState
        state={q.state}
        what="certificates"
        emptyMessage="No certificates expire within this window."
        isEmpty={() => false}
        onRetry={q.retry}
        onReset={q.resetFilters}
      >
        {(res) => (
          <>
            <SummaryTiles summary={res.summary} />
            <p className="text-xs text-muted-foreground">
              Platform CAs (root, intermediate) are counted in the summary; their expiry is beyond the listing window.
            </p>
            {res.items.length === 0 ? (
              <p className="py-6 text-sm text-muted-foreground">No certificates expire within this window.</p>
            ) : (
              <CertificateTable items={res.items} />
            )}
            <Pager page={q.page} hasPrev={q.hasPrev} hasNext={q.hasNext} onPrev={q.prev} onNext={q.next} />
          </>
        )}
      </DataState>
    </div>
  )
}

/** Tenant id filter: applied on Apply / Enter only, never while typing. */
function TenantFilter({ applied, onApply }: { applied: string; onApply: (v: string) => void }) {
  const [draft, setDraft] = useState(applied)
  const submit = (e: FormEvent) => {
    e.preventDefault()
    onApply(draft.trim())
  }
  return (
    <form aria-label="Tenant filter" onSubmit={submit} className="flex items-end gap-2">
      <div className="grid gap-1.5">
        <Label htmlFor="cert-tenant">Tenant ID</Label>
        <Input id="cert-tenant" className="w-80 font-mono text-xs" value={draft} onChange={(e) => setDraft(e.target.value)} placeholder="any tenant" autoComplete="off" />
      </div>
      <Button type="submit" variant="outline">
        Apply
      </Button>
    </form>
  )
}

function SummaryTiles({ summary }: { summary: CertificateSummary }) {
  return (
    <div role="list" aria-label="Expiry summary" className="grid grid-cols-2 gap-3 sm:grid-cols-5">
      {EXPIRY_BUCKETS.map((b) => (
        <Card key={b} role="listitem" aria-label={TILE_LABELS[b]} data-bucket={b}>
          <CardContent className="flex flex-col gap-1 p-4">
            <Badge variant={BUCKET_VARIANTS[b]} className="w-fit whitespace-nowrap">
              {TILE_LABELS[b]}
            </Badge>
            <span className="font-display text-2xl font-semibold tabular-nums">{summary[b]}</span>
          </CardContent>
        </Card>
      ))}
    </div>
  )
}

function EntityCell({ c }: { c: CertificateEntry }) {
  const name = c.entity_name ?? '—' // client devices carry no name (privacy)
  const id = <span className="font-mono text-xs text-muted-foreground">{c.entity_id}</span>
  if (c.kind === 'relay') {
    return (
      <span className="flex flex-col">
        <Link to={`/relays/${encodeURIComponent(c.entity_id)}`} className="text-primary underline-offset-4 hover:underline">
          {name}
        </Link>
        {id}
      </span>
    )
  }
  return (
    <span className="flex flex-col">
      <span>{name}</span>
      {id}
    </span>
  )
}

function CertificateTable({ items }: { items: CertificateEntry[] }) {
  return (
    <Table aria-label="Certificates">
      <TableHeader>
        <TableRow>
          <TableHead>Kind</TableHead>
          <TableHead>Entity</TableHead>
          <TableHead>Tenant</TableHead>
          <TableHead>Serial</TableHead>
          <TableHead>Expires</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {items.map((c) => (
          <TableRow key={`${c.kind}:${c.entity_id}`} aria-label={`${c.kind} ${c.entity_id}`}>
            <TableCell className="font-mono text-xs">{c.kind}</TableCell>
            <TableCell>
              <EntityCell c={c} />
            </TableCell>
            <TableCell>
              {c.tenant_id ? (
                // Explicit navigation only: tenant detail is an audited read.
                <Link to={`/tenants/${encodeURIComponent(c.tenant_id)}`} className="font-mono text-xs text-primary underline-offset-4 hover:underline">
                  {c.tenant_id}
                </Link>
              ) : (
                <span className="text-muted-foreground">—</span>
              )}
            </TableCell>
            <TableCell className="font-mono text-xs">{c.serial ?? '—'}</TableCell>
            <TableCell className="whitespace-nowrap">
              <span className="inline-flex items-center gap-2">
                {formatDateTime(c.not_after)}
                <ExpiryBadge notAfter={c.not_after} bucket={c.bucket} />
              </span>
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
