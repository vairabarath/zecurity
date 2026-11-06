import { useCallback, useMemo, useState } from 'react'
import { RefreshCw } from 'lucide-react'
import { getPki } from '@/api/reads'
import type { CANode, CRLStatus, PKIHierarchy } from '@/api/types'
import { ExpiryBadge } from '@/components/ExpiryBadge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { validityText } from '@/lib/format'
import { useDetailQuery } from '@/lib/useDetailQuery'

// CA hierarchy and CRL status (quick-win F15/F16, cert.read: super-admin).
//
// Renders GET /provider/pki as returned: Root → Intermediate → workspace CAs,
// linked by parent_id (the server resolves it from the key identifiers), and
// the CRL each CA signs. Public certificate data only.
//
// One request when the page opens and again only on Refresh (useDetailQuery:
// no polling, no focus refetch). Workspace CAs come one page at a time:
// "Show more" appends the next page, and the count is always shown against
// the total, so a partial list never looks complete. A CA whose parent is
// not among the loaded CAs (an unparsable certificate, an unresolved issuer)
// is listed under "Unlinked CAs" rather than dropped.

const PAGE = 50

const fetchFirst = () => getPki({ limit: PAGE })

const COVERS: Record<string, string> = { relays: 'relays', client_devices: 'client devices', connectors: 'connectors' }

function CRLLine({ crl }: { crl: CRLStatus | null }) {
  if (!crl) return <p className="text-xs text-faint">Signs no CRL</p>
  return (
    <p className="text-xs text-muted-foreground" aria-label="CRL status">
      CRL <span className="font-mono">{crl.url}</span> · {crl.entries.toLocaleString()} {crl.entries === 1 ? 'entry' : 'entries'} (
      {crl.covers.map((c) => COVERS[c] ?? c).join(', ')}) · valid {validityText(crl.valid_for_seconds)}
    </p>
  )
}

function NodeRow({ n }: { n: CANode }) {
  return (
    <div className="flex flex-col gap-0.5 py-2" data-ca-id={n.id}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">{n.name}</span>
        <ExpiryBadge notAfter={n.not_after} />
        {n.serial ? (
          <span className="font-mono text-xs text-muted-foreground">serial {n.serial}</span>
        ) : (
          <span className="text-xs text-warning-text">certificate could not be read</span>
        )}
      </div>
      {n.subject && <p className="truncate font-mono text-xs text-faint" title={n.subject}>{n.subject}</p>}
      <CRLLine crl={n.crl} />
    </div>
  )
}

interface Loaded {
  data: PKIHierarchy
  /** Workspace CAs from "Show more" pages, after data.workspace_cas. */
  more: CANode[]
  next: string | null
}

export function CAHierarchy() {
  const q = useDetailQuery('pki', fetchFirst)
  const [extra, setExtra] = useState<{ for: PKIHierarchy; more: CANode[]; next: string | null } | null>(null)
  const [loadingMore, setLoadingMore] = useState(false)
  const [moreError, setMoreError] = useState(false)

  const first = q.state.status === 'ready' ? q.state.data : null
  // Extra pages belong to the first page they continued; a Refresh starts over.
  const loaded: Loaded | null = useMemo(
    () =>
      first
        ? { data: first, more: extra?.for === first ? extra.more : [], next: extra?.for === first ? extra.next : first.next_cursor }
        : null,
    [first, extra],
  )

  const showMore = useCallback(async () => {
    if (!loaded?.next) return
    setLoadingMore(true)
    setMoreError(false)
    try {
      const page = await getPki({ limit: PAGE, cursor: loaded.next })
      setExtra({ for: loaded.data, more: [...loaded.more, ...page.workspace_cas], next: page.next_cursor })
    } catch {
      setMoreError(true)
    } finally {
      setLoadingMore(false)
    }
  }, [loaded])

  return (
    <Card aria-labelledby="pki-heading" className="flex flex-col">
      <div className="flex items-center justify-between border-b border-border px-5 py-4">
        <div>
          <h2 id="pki-heading" className="font-semibold">
            CA hierarchy
          </h2>
          <p className="text-xs text-muted-foreground">
            Which CA signs which, and the CRL each one signs. CRLs are signed on request: entries are what a CRL fetched now lists.
          </p>
        </div>
        <Button variant="outline" size="sm" onClick={q.reload} aria-label="Refresh CA hierarchy">
          <RefreshCw aria-hidden />
          Refresh
        </Button>
      </div>
      <div className="px-5 py-3">
        {q.state.status === 'loading' && <p className="text-sm text-muted-foreground">Loading the CA hierarchy…</p>}
        {(q.state.status === 'error' || q.state.status === 'not_found') && (
          <p className="text-sm text-destructive">
            Couldn't load the CA hierarchy.{' '}
            <Button variant="link" className="h-auto p-0" onClick={q.reload}>
              Retry
            </Button>
          </p>
        )}
        {q.state.status === 'forbidden' && <p className="text-sm text-muted-foreground">Only super-admins can see the CA hierarchy.</p>}
        {loaded && <Tree loaded={loaded} />}
        {loaded && (
          <div className="flex flex-wrap items-center gap-3 pt-3 text-xs text-muted-foreground">
            <span aria-label="Workspace CA count">
              Showing {(loaded.data.workspace_cas.length + loaded.more.length).toLocaleString()} of{' '}
              {loaded.data.workspace_cas_total.toLocaleString()} workspace CAs
            </span>
            {loaded.next && (
              <Button variant="outline" size="sm" onClick={showMore} disabled={loadingMore}>
                {loadingMore ? 'Loading…' : 'Show more'}
              </Button>
            )}
            {moreError && <span className="text-destructive">Couldn't load more workspace CAs.</span>}
          </div>
        )}
      </div>
    </Card>
  )
}

function Tree({ loaded }: { loaded: Loaded }) {
  const { roots, intermediates } = loaded.data
  const workspace = [...loaded.data.workspace_cas, ...loaded.more]
  const ids = new Set([...roots, ...intermediates].map((n) => n.id))
  const children = (id: string, list: CANode[]) => list.filter((n) => n.parent_id === id)
  // Roots are the top of the tree by kind. An intermediate or workspace CA
  // whose signer is not among the loaded platform CAs is listed separately.
  const unlinked = [
    ...intermediates.filter((n) => n.parent_id === null || !ids.has(n.parent_id)),
    ...workspace.filter((n) => n.parent_id === null || !ids.has(n.parent_id)),
  ]

  return (
    <div className="flex flex-col gap-2">
      <ul aria-label="CA hierarchy" className="flex flex-col">
        {roots.map((r) => (
          <li key={r.id}>
            <NodeRow n={r} />
            <ul aria-label={`Signed by ${r.name}`} className="ml-5 border-l border-border pl-4">
              {children(r.id, intermediates).map((i) => (
                <li key={i.id}>
                  <NodeRow n={i} />
                  <ul aria-label={`Signed by ${i.name}`} className="ml-5 border-l border-border pl-4">
                    {children(i.id, workspace).map((w) => (
                      <li key={w.id}>
                        <NodeRow n={w} />
                      </li>
                    ))}
                  </ul>
                </li>
              ))}
            </ul>
          </li>
        ))}
      </ul>
      {unlinked.length > 0 && (
        <section aria-label="Unlinked CAs" className="flex flex-col gap-1 rounded-md border border-dashed border-border px-3 py-2">
          <h3 className="text-sm font-medium">Unlinked CAs</h3>
          <p className="text-xs text-muted-foreground">Their signer could not be determined from the certificate data.</p>
          <ul>
            {unlinked.map((n) => (
              <li key={n.id}>
                <NodeRow n={n} />
              </li>
            ))}
          </ul>
        </section>
      )}
    </div>
  )
}
