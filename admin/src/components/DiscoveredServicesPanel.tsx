import { useMemo, useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import { Check, CheckCircle, Loader2, Plus, RefreshCw, X } from 'lucide-react'
import {
  GetAllResourcesDocument,
  GetDiscoveredServicesDocument,
  PromoteDiscoveredServiceDocument,
} from '@/generated/graphql'
import { Skeleton } from '@/components/ui/skeleton'
import { relativeTime } from '@/lib/console'
import { PromoteServiceModal } from '@/components/PromoteServiceModal'

// ── Port helpers ──────────────────────────────────────────────────────────────

type PortCategory = 'web' | 'admin' | 'db' | 'cache' | 'mon' | 'app'

function classifyPort(port: number): PortCategory {
  if ([80, 443, 8080, 8443, 8888, 3000, 5173].includes(port)) return 'web'
  if ([22, 23, 3389, 5985, 5986].includes(port))              return 'admin'
  if ([5432, 3306, 27017, 1433, 5984, 9042].includes(port))   return 'db'
  if ([6379, 11211, 4222].includes(port))                     return 'cache'
  if ([9090, 9091, 9100, 3001, 9200, 9300, 5601].includes(port)) return 'mon'
  return 'app'
}

const CATEGORY_LABELS: Record<PortCategory, string> = {
  web: 'Web', admin: 'Admin', db: 'Database', cache: 'Cache', mon: 'Monitoring', app: 'Application',
}

function isStale(lastSeen: string): boolean {
  const d = new Date(lastSeen)
  return !isNaN(d.getTime()) && Date.now() - d.getTime() > 5 * 60 * 1000
}

function PortChip({ port, protocol }: { port: number; protocol: string }) {
  const isTcp = protocol.toLowerCase() === 'tcp'
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-[7px] border border-l-[3px] border-border bg-secondary py-1 pl-2 pr-2.5 font-mono ${
        isTcp ? 'border-l-primary' : 'border-l-amber-500'
      }`}
    >
      <span
        className={`text-[9.5px] font-bold uppercase tracking-wide ${
          isTcp ? 'text-primary' : 'text-amber-500'
        }`}
      >
        {protocol.toUpperCase()}
      </span>
      <span className="text-[12px] font-semibold text-foreground">{port}</span>
    </span>
  )
}

// ── Filter types ──────────────────────────────────────────────────────────────

type FilterKey = 'all' | 'unmanaged' | 'managed' | 'web' | 'db' | 'admin' | 'mon' | 'cache'

const FILTERS: Array<{ key: FilterKey; label: string }> = [
  { key: 'all',       label: 'All' },
  { key: 'unmanaged', label: 'Unmanaged' },
  { key: 'managed',   label: 'Managed' },
  { key: 'web',       label: 'Web' },
  { key: 'db',        label: 'Database' },
  { key: 'admin',     label: 'Admin' },
  { key: 'mon',       label: 'Monitoring' },
  { key: 'cache',     label: 'Cache' },
]

// ── Component ─────────────────────────────────────────────────────────────────

interface PromoteTarget {
  protocol: string
  port: number
  serviceName: string
  boundIp: string
}

export function DiscoveredServicesPanel({ shieldId }: { shieldId: string }) {
  const { data, loading, error, refetch } = useQuery(GetDiscoveredServicesDocument, {
    variables: { shieldId },
    pollInterval: 30000,
    fetchPolicy: 'cache-and-network',
  })
  const { data: resourcesData } = useQuery(GetAllResourcesDocument, {
    fetchPolicy: 'cache-and-network',
  })
  const [promoteOne] = useMutation(PromoteDiscoveredServiceDocument, {
    refetchQueries: [{ query: GetAllResourcesDocument }],
  })

  const [promote, setPromote]           = useState<PromoteTarget | null>(null)
  const [filter, setFilter]             = useState<FilterKey>('all')
  const [selected, setSelected]         = useState<Set<string>>(new Set())
  const [bulkLoading, setBulkLoading]   = useState(false)

  const services = data?.getDiscoveredServices ?? []

  const managedKeys = useMemo(() => {
    const keys = new Set<string>()
    for (const r of resourcesData?.allResources ?? []) {
      if (r.shield?.id !== shieldId) continue
      for (let p = r.portFrom; p <= r.portTo; p++) {
        keys.add(`${r.protocol}:${p}`)
      }
    }
    return keys
  }, [resourcesData, shieldId])

  const counts = useMemo(() => {
    const c: Record<FilterKey, number> = {
      all: services.length, unmanaged: 0, managed: 0,
      web: 0, db: 0, admin: 0, mon: 0, cache: 0,
    }
    for (const s of services) {
      if (managedKeys.has(`${s.protocol}:${s.port}`)) c.managed++
      else c.unmanaged++
      const cat = classifyPort(s.port)
      if (cat !== 'app') (c as Record<string, number>)[cat]++
    }
    return c
  }, [services, managedKeys])

  const visibleFilters = FILTERS.filter(
    (f) => ['all', 'unmanaged', 'managed'].includes(f.key) || counts[f.key] > 0,
  )

  const filtered = useMemo(() => {
    return services.filter((s) => {
      if (filter === 'all') return true
      if (filter === 'managed') return managedKeys.has(`${s.protocol}:${s.port}`)
      if (filter === 'unmanaged') return !managedKeys.has(`${s.protocol}:${s.port}`)
      return classifyPort(s.port) === (filter as PortCategory)
    })
  }, [services, filter, managedKeys])

  function toggleSelect(key: string) {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  }

  // Number of selected services that are not yet managed (can actually be promoted)
  const selectableSelected = [...selected].filter((k) => !managedKeys.has(k))

  async function handleBulkAdd() {
    if (selectableSelected.length === 0) return
    setBulkLoading(true)
    const targets = services.filter((s) => selectableSelected.includes(`${s.protocol}:${s.port}`))
    await Promise.allSettled(
      targets.map((s) =>
        promoteOne({ variables: { shieldId, protocol: s.protocol, port: s.port } }),
      ),
    )
    setSelected(new Set())
    setBulkLoading(false)
    void refetch()
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      {/* Filter toolbar */}
      <div className="flex flex-wrap items-center gap-3 border-b border-border px-5 py-3">
        <span className="flex shrink-0 items-center gap-2 text-sm font-bold">
          Discovered services
          <span className="rounded-full bg-secondary px-2 py-0.5 text-[11px] font-semibold text-muted-foreground">
            {services.length}
          </span>
        </span>
        <div className="flex flex-wrap gap-0.5 rounded-lg border border-border bg-secondary p-0.5">
          {visibleFilters.map(({ key, label }) => (
            <button
              key={key}
              onClick={() => setFilter(key)}
              className={`rounded-md px-2.5 py-1 text-[11.5px] font-semibold transition ${
                filter === key
                  ? 'bg-background text-foreground shadow-sm'
                  : 'text-muted-foreground hover:text-foreground'
              }`}
            >
              {label}
              {counts[key] > 0 && (
                <span className="ml-1 text-[10px] opacity-60">{counts[key]}</span>
              )}
            </button>
          ))}
        </div>
        <button
          onClick={() => void refetch()}
          className="ml-auto flex items-center gap-1.5 text-xs text-muted-foreground transition hover:text-foreground"
          title="Refresh now"
        >
          <RefreshCw className="h-3 w-3" />
          Refresh
        </button>
      </div>

      {/* Body */}
      {error ? (
        <div className="m-4 rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
          {error.message}
        </div>
      ) : loading && services.length === 0 ? (
        <div className="space-y-2 p-5">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-12 rounded-lg bg-secondary" />
          ))}
        </div>
      ) : services.length === 0 ? (
        <div className="flex flex-1 items-center justify-center py-16 text-center text-xs text-muted-foreground">
          No services discovered yet. Shield scans every 60 s.
        </div>
      ) : filtered.length === 0 ? (
        <div className="flex flex-1 items-center justify-center py-16 text-center text-xs text-muted-foreground">
          No services match this filter.
        </div>
      ) : (
        <div className="min-h-0 flex-1 overflow-y-auto">
          {/* Table head */}
          <div className="sticky top-0 z-10 grid grid-cols-[24px_80px_minmax(0,1fr)_160px_110px_130px] gap-4 border-b border-border bg-secondary/80 px-5 py-2.5 text-[10.5px] font-semibold uppercase tracking-[0.08em] text-muted-foreground backdrop-blur">
            <div />
            <div>Port</div>
            <div>Service</div>
            <div>Bound IP</div>
            <div>Last Seen</div>
            <div className="text-right">Action</div>
          </div>

          {filtered.map((s) => {
            const key     = `${s.protocol}:${s.port}`
            const managed = managedKeys.has(key)
            const checked = selected.has(key)
            const stale   = isStale(s.lastSeen)
            const cat     = classifyPort(s.port)

            return (
              <div
                key={`${s.protocol}-${s.port}-${s.boundIp}`}
                className={`grid grid-cols-[24px_80px_minmax(0,1fr)_160px_110px_130px] items-center gap-4 border-b border-border px-5 py-3 transition-colors last:border-b-0 ${
                  checked ? 'bg-primary/5' : 'hover:bg-secondary/40'
                }`}
              >
                {/* Checkbox — only for non-managed rows */}
                <div className="flex items-center">
                  {!managed ? (
                    <button
                      onClick={() => toggleSelect(key)}
                      className={`flex h-4 w-4 shrink-0 items-center justify-center rounded-[4px] border transition ${
                        checked
                          ? 'border-primary bg-primary text-primary-foreground'
                          : 'border-border bg-background hover:border-primary/50'
                      }`}
                    >
                      {checked && <Check className="h-2.5 w-2.5 stroke-[3]" />}
                    </button>
                  ) : (
                    <span className="h-4 w-4" />
                  )}
                </div>

                {/* Port chip */}
                <div>
                  <PortChip port={s.port} protocol={s.protocol} />
                </div>

                {/* Service name + category */}
                <div className="min-w-0">
                  <div className="truncate text-[13px] font-semibold leading-tight">
                    {s.serviceName || `Port ${s.port}`}
                  </div>
                  <div className="truncate text-[11px] text-muted-foreground">
                    {CATEGORY_LABELS[cat]}
                  </div>
                </div>

                {/* Bound IP */}
                <div className="truncate font-mono text-xs text-muted-foreground">{s.boundIp}</div>

                {/* Last seen with freshness dot */}
                <div
                  className={`flex items-center gap-1.5 text-xs font-medium ${
                    stale ? 'text-amber-500' : 'text-emerald-500'
                  }`}
                >
                  <span className="h-1.5 w-1.5 shrink-0 rounded-full bg-current" />
                  {relativeTime(s.lastSeen)}
                </div>

                {/* Action */}
                <div className="flex justify-end">
                  {managed ? (
                    <span className="inline-flex h-[26px] items-center gap-1.5 text-xs font-semibold text-emerald-500">
                      <CheckCircle className="h-3.5 w-3.5 shrink-0" />
                      Managed
                    </span>
                  ) : (
                    <button
                      onClick={() =>
                        setPromote({
                          protocol:    s.protocol,
                          port:        s.port,
                          serviceName: s.serviceName,
                          boundIp:     s.boundIp,
                        })
                      }
                      className="inline-flex h-[26px] items-center gap-1.5 whitespace-nowrap rounded-lg border border-border bg-background px-2.5 text-xs font-semibold text-primary transition hover:bg-secondary"
                      title="Add as resource"
                    >
                      <Plus className="h-3 w-3 shrink-0" />
                      Add Resource
                    </button>
                  )}
                </div>
              </div>
            )
          })}
        </div>
      )}

      {/* Floating bulk-action bar */}
      {selected.size > 0 && (
        <div className="fixed bottom-5 left-1/2 z-50 flex -translate-x-1/2 items-center gap-3 rounded-2xl border border-border bg-background px-4 py-2.5 shadow-2xl">
          <span className="flex items-center gap-2 text-sm font-bold">
            <span className="rounded-full bg-primary px-2.5 py-0.5 text-xs font-bold text-primary-foreground">
              {selected.size}
            </span>
            service{selected.size === 1 ? '' : 's'} selected
          </span>
          <div className="h-4 w-px bg-border" />
          <button
            onClick={handleBulkAdd}
            disabled={bulkLoading || selectableSelected.length === 0}
            className="inline-flex items-center gap-1.5 rounded-lg bg-primary px-3 py-1.5 text-xs font-bold text-primary-foreground transition hover:opacity-90 disabled:opacity-60"
          >
            {bulkLoading
              ? <Loader2 className="h-3 w-3 animate-spin" />
              : <Plus className="h-3 w-3" />}
            Add as resources
          </button>
          <div className="h-4 w-px bg-border" />
          <button
            onClick={() => setSelected(new Set())}
            className="flex h-6 w-6 items-center justify-center rounded-md text-muted-foreground transition hover:bg-secondary hover:text-foreground"
            title="Clear selection"
          >
            <X className="h-3.5 w-3.5" />
          </button>
        </div>
      )}

      {/* Single-item promote modal */}
      {promote ? (
        <PromoteServiceModal
          shieldId={shieldId}
          protocol={promote.protocol}
          port={promote.port}
          serviceName={promote.serviceName}
          boundIp={promote.boundIp}
          onClose={() => {
            setPromote(null)
            void refetch()
          }}
        />
      ) : null}
    </div>
  )
}
