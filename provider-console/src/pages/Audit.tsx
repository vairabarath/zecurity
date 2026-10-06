import { useState, type FormEvent } from 'react'
import { RefreshCw } from 'lucide-react'
import { queryAudit } from '@/api/reads'
import type { AuditEntry, JsonValue, Page } from '@/api/types'
import { DataState } from '@/components/DataState'
import { Pager } from '@/components/Pager'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatRelative } from '@/lib/expiry'
import { formatDateTime } from '@/lib/format'
import { usePagedQuery, type Filters } from '@/lib/usePagedQuery'

// Provider audit (Phase P, audit.view: super-admin only). Read-only;
// viewing it is not itself audited (the server guarantees that).
//
// Filters are the six API fields, kept in the URL. Text fields apply only on
// Apply / Enter (typing sends nothing). The time window is the API's
// half-open [since, until), sent as RFC 3339; since > until is invalid and
// sends nothing. Audit details stay on the page: rendered as plain text,
// toggled locally, never put in the URL.

const AUDIT_FILTER_KEYS = ['action', 'target_type', 'target_id', 'provider_email', 'since', 'until'] as const

const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/

function validTime(v: string | undefined): boolean {
  return v === undefined || (RFC3339.test(v) && !Number.isNaN(Date.parse(v)))
}

function validAuditFilters(f: Filters): boolean {
  if (!validTime(f.since) || !validTime(f.until)) return false
  return !(f.since && f.until && Date.parse(f.since) > Date.parse(f.until))
}

function fetchAudit(f: Filters, cursor: string | undefined): Promise<Page<AuditEntry>> {
  return queryAudit({ ...f, cursor })
}

/** datetime-local value (viewer's local time) → RFC 3339 UTC, whole seconds. */
function localToRFC3339(local: string): string {
  if (!local) return ''
  const t = new Date(local)
  if (Number.isNaN(t.getTime())) return ''
  return t.toISOString().replace(/\.\d{3}Z$/, 'Z')
}

/** RFC 3339 → datetime-local value in the viewer's local time ("" if invalid). */
function rfc3339ToLocal(v: string | undefined): string {
  if (!v || !validTime(v)) return ''
  const d = new Date(v)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

function shortId(id: string): string {
  return id.length > 12 ? `${id.slice(0, 8)}…` : id
}

export function Audit() {
  const q = usePagedQuery<Page<AuditEntry>>({ filterKeys: AUDIT_FILTER_KEYS, validate: validAuditFilters, fetchPage: fetchAudit })

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="font-display text-2xl font-semibold tracking-tight">Audit</h1>
          <p className="text-sm text-muted-foreground">Actions taken by provider operators, newest first.</p>
        </div>
        <Button variant="outline" onClick={q.retry} aria-label="Refresh audit">
          <RefreshCw aria-hidden />
          Refresh
        </Button>
      </div>

      {/* Re-seeded from the URL whenever the applied filters change (Apply, Clear, back/forward). */}
      <AuditFilterForm key={JSON.stringify(q.filters)} applied={q.filters} onApply={q.setFilters} onClear={q.resetFilters} />

      <DataState
        state={q.state}
        what="audit entries"
        emptyMessage="No audit entries match these filters."
        isEmpty={(d) => d.items.length === 0}
        onRetry={q.retry}
        onReset={q.resetFilters}
      >
        {(page) => (
          <>
            <AuditTable entries={page.items} />
            <Pager page={q.page} hasPrev={q.hasPrev} hasNext={q.hasNext} onPrev={q.prev} onNext={q.next} />
          </>
        )}
      </DataState>
    </div>
  )
}

function AuditFilterForm({ applied, onApply, onClear }: { applied: Filters; onApply: (f: Filters) => void; onClear: () => void }) {
  const [draft, setDraft] = useState({
    action: applied.action ?? '',
    target_type: applied.target_type ?? '',
    target_id: applied.target_id ?? '',
    provider_email: applied.provider_email ?? '',
    since: rfc3339ToLocal(applied.since),
    until: rfc3339ToLocal(applied.until),
  })
  const set = (k: keyof typeof draft) => (e: { target: { value: string } }) => setDraft((d) => ({ ...d, [k]: e.target.value }))

  function submit(e: FormEvent) {
    e.preventDefault()
    // Values are sent as typed (no case changes); empty ones are dropped by
    // the URL allowlist. Times become RFC 3339 UTC.
    onApply({
      action: draft.action,
      target_type: draft.target_type,
      target_id: draft.target_id,
      provider_email: draft.provider_email,
      since: localToRFC3339(draft.since),
      until: localToRFC3339(draft.until),
    })
  }

  const field = (id: string, label: string, k: keyof typeof draft, type = 'text', placeholder?: string) => (
    <div className="grid gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <Input id={id} type={type} value={draft[k]} onChange={set(k)} placeholder={placeholder} autoComplete="off" />
    </div>
  )

  return (
    <form aria-label="Audit filters" onSubmit={submit} className="grid gap-3 rounded-lg border border-border/40 p-4 sm:grid-cols-2 lg:grid-cols-3">
      {field('audit-action', 'Action', 'action', 'text', 'e.g. tenant.read')}
      {field('audit-target-type', 'Target type', 'target_type', 'text', 'e.g. provider_user')}
      {field('audit-target-id', 'Target ID', 'target_id')}
      {field('audit-email', 'Operator email', 'provider_email')}
      {field('audit-since', 'From (inclusive)', 'since', 'datetime-local')}
      {field('audit-until', 'Until (exclusive)', 'until', 'datetime-local')}
      <div className="flex items-end gap-2 sm:col-span-2 lg:col-span-3">
        <Button type="submit">Apply</Button>
        <Button type="button" variant="ghost" onClick={onClear}>
          Clear
        </Button>
      </div>
    </form>
  )
}

function AuditTable({ entries }: { entries: AuditEntry[] }) {
  // Which rows have details open: page-local UI state only.
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set())
  const toggle = (id: string) =>
    setOpen((s) => {
      const next = new Set(s)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })

  return (
    <Table aria-label="Audit entries">
      <TableHeader>
        <TableRow>
          <TableHead>When</TableHead>
          <TableHead>Operator</TableHead>
          <TableHead>Action</TableHead>
          <TableHead>Target</TableHead>
          <TableHead>IP</TableHead>
          <TableHead>Details</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {entries.map((e) => (
          <TableRow key={e.id} aria-label={`audit ${e.id}`} className="align-top">
            <TableCell className="whitespace-nowrap" title={formatDateTime(e.created_at)}>
              {formatRelative(e.created_at)}
            </TableCell>
            <TableCell className="max-w-56 truncate" title={e.provider_email}>
              {e.provider_email}
            </TableCell>
            <TableCell className="font-mono text-xs">{e.action}</TableCell>
            <TableCell className="whitespace-nowrap text-xs" title={`${e.target_type} · ${e.target_id}`}>
              {e.target_type} · <span className="font-mono">{shortId(e.target_id)}</span>
            </TableCell>
            <TableCell className="font-mono text-xs">{e.ip_address ?? '—'}</TableCell>
            <TableCell>
              <DetailsCell details={e.details} expanded={open.has(e.id)} onToggle={() => toggle(e.id)} />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}

function DetailsCell({ details, expanded, onToggle }: { details: JsonValue; expanded: boolean; onToggle: () => void }) {
  return (
    <div className="flex flex-col items-start gap-1">
      <Button variant="link" size="sm" className="h-auto p-0" onClick={onToggle} aria-expanded={expanded}>
        {expanded ? 'Hide details' : 'Show details'}
      </Button>
      {expanded && (
        // React renders this as text: nothing in details is ever interpreted as HTML.
        <pre className="max-w-md overflow-auto whitespace-pre-wrap break-all rounded bg-muted/40 p-2 font-mono text-xs" aria-label="Details JSON">
          {JSON.stringify(details, null, 2)}
        </pre>
      )}
    </div>
  )
}
