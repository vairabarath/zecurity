import { useCallback, useEffect, useState } from 'react'
import { KeyRound, Plus, ShieldCheck } from 'lucide-react'
import { Navigate } from 'react-router-dom'
import { ApiError } from '@/api/client'
import { listApiKeys, type ApiKey } from '@/api/apiKeys'
import { ROLE_LABELS } from '@/auth/roles'
import { CreateApiKeyDialog, KeyShownOnceDialog, RevokeApiKeyDialog } from '@/components/APIKeyDialogs'
import { PageHeader } from '@/components/PageHeader'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { toast } from '@/components/ui/use-toast'
import { apiKeyErrorMessage, expiresSoon, expiryText } from '@/lib/apikey-format'
import { formatDate, formatDateTime } from '@/lib/format'

// Settings → API keys (Sprint 27 K2, F35; D-76). Super-admin only: the route
// is wrapped in RequireRole and the controller authorizes every call. A new
// key is shown once (KeyShownOnceDialog) and never stored; the list shows
// only each key's 12-character prefix.

type ListState =
  | { status: 'loading' }
  | { status: 'ready'; keys: ApiKey[] }
  | { status: 'error'; message: string }
  | { status: 'forbidden' }

type Action = { kind: 'create' } | { kind: 'revoke'; key: ApiKey } | { kind: 'shown'; name: string; key: string }

export function APIKeys() {
  const [list, setList] = useState<ListState>({ status: 'loading' })
  const [action, setAction] = useState<Action | null>(null)

  const reload = useCallback(async () => {
    try {
      setList({ status: 'ready', keys: (await listApiKeys()).items })
    } catch (err) {
      if (err instanceof ApiError && err.sessionEnded) return
      if (err instanceof ApiError && err.status === 403) {
        setList({ status: 'forbidden' })
        return
      }
      setList({ status: 'error', message: apiKeyErrorMessage(err) })
    }
  }, [])

  useEffect(() => {
    // Initial load; reload() only sets state once the request settles.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void reload()
  }, [reload])

  if (list.status === 'forbidden') return <Navigate to="/forbidden" replace />

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="API keys"
        description="Read-only keys for scripts and integrations. Each key has one role and expires within a year. Super-admins only."
        actions={
          <Button onClick={() => setAction({ kind: 'create' })}>
            <Plus aria-hidden />
            Create key
          </Button>
        }
      />

      {list.status === 'loading' && <Skeleton className="h-40 w-full" aria-label="Loading API keys" />}

      {list.status === 'error' && (
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
            <span>Couldn’t load API keys. {list.message}</span>
            <Button variant="outline" size="sm" onClick={() => void reload()}>
              Retry
            </Button>
          </AlertDescription>
        </Alert>
      )}

      {list.status === 'ready' && list.keys.length === 0 && (
        <Card className="flex items-center gap-3 px-5 py-6 text-sm text-muted-foreground">
          <KeyRound className="size-5 shrink-0" aria-hidden />
          No API keys. Create one for a script or integration that only needs to read.
        </Card>
      )}

      {list.status === 'ready' && list.keys.length > 0 && <KeysTable keys={list.keys} onRevoke={(key) => setAction({ kind: 'revoke', key })} />}

      <div className="flex gap-3 rounded-xl border border-border bg-sidebar px-4 py-3.5 text-[13px] leading-relaxed text-subtle-foreground">
        <ShieldCheck className="mt-0.5 size-[18px] shrink-0 text-[#c2b8ff]" strokeWidth={1.8} aria-hidden />
        <p>
          A key can only read, never write, and can’t sign in or manage operators or keys. The full key is shown once when it is created. A warning
          alert fires 14 days before a key expires. Creating and revoking keys is audited.
        </p>
      </div>

      {action?.kind === 'create' && (
        <CreateApiKeyDialog
          onCancel={() => setAction(null)}
          onCreated={({ api_key, key }) => {
            setAction({ kind: 'shown', name: api_key.name, key })
            void reload()
          }}
        />
      )}
      {action?.kind === 'shown' && <KeyShownOnceDialog name={action.name} apiKey={action.key} onClose={() => setAction(null)} />}
      {action?.kind === 'revoke' && (
        <RevokeApiKeyDialog
          apiKey={action.key}
          onCancel={() => setAction(null)}
          onRevoked={() => {
            const name = action.key.name
            setAction(null)
            toast({ title: `API key ${name} revoked.` })
            void reload()
          }}
        />
      )}
    </div>
  )
}

function StatusBadge({ k }: { k: ApiKey }) {
  if (k.status === 'revoked') return <Badge variant="outline">Revoked</Badge>
  if (k.status === 'expired') return <Badge variant="outline">Expired</Badge>
  return <Badge>Active</Badge>
}

function KeysTable({ keys, onRevoke }: { keys: ApiKey[]; onRevoke: (k: ApiKey) => void }) {
  return (
    <Card className="overflow-hidden p-0">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Role</TableHead>
            <TableHead>Key</TableHead>
            <TableHead>Created</TableHead>
            <TableHead>Expires</TableHead>
            <TableHead>Last used</TableHead>
            <TableHead>Status</TableHead>
            <TableHead className="text-right">
              <span className="sr-only">Actions</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {keys.map((k) => {
            const soon = expiresSoon(k.status, k.expires_at)
            return (
              <TableRow key={k.id} aria-label={k.name}>
                <TableCell className="font-medium">
                  {k.name}
                  {k.allowed_cidrs.length > 0 && (
                    <div className="font-mono text-[11px] text-muted-foreground" title="Allowed source addresses">
                      {k.allowed_cidrs.join(', ')}
                    </div>
                  )}
                </TableCell>
                <TableCell>{ROLE_LABELS[k.role] ?? k.role}</TableCell>
                <TableCell className="font-mono text-xs">{k.prefix}…</TableCell>
                <TableCell className="text-xs">
                  {formatDate(k.created_at)}
                  <div className="text-muted-foreground">{k.created_by}</div>
                </TableCell>
                <TableCell className="text-xs">
                  {formatDate(k.expires_at)}
                  {k.status === 'active' && (
                    <div className={soon ? 'font-medium text-warning-text' : 'text-muted-foreground'} data-testid={soon ? 'expires-soon' : undefined}>
                      {expiryText(k.expires_at)}
                    </div>
                  )}
                </TableCell>
                <TableCell className="text-xs">
                  {k.last_used_at ? formatDateTime(k.last_used_at) : 'Never'}
                  {k.last_used_ip && <div className="font-mono text-muted-foreground">{k.last_used_ip}</div>}
                </TableCell>
                <TableCell>
                  <StatusBadge k={k} />
                  {k.status === 'revoked' && k.revoked_by && <div className="mt-1 text-[11px] text-muted-foreground">by {k.revoked_by}</div>}
                </TableCell>
                <TableCell className="text-right">
                  {k.status !== 'revoked' && (
                    <Button variant="outline" size="sm" onClick={() => onRevoke(k)} aria-label={`Revoke ${k.name}`}>
                      Revoke
                    </Button>
                  )}
                </TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </Card>
  )
}
