import { useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import { AlertTriangle, Check, Copy, KeyRound, RefreshCw, Ticket, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import {
  GetScimTokensDocument,
  MintScimTokenDocument,
  RevokeScimTokenDocument,
  RotateScimTokenDocument,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { EmptyState, ErrorState, StatusPill, relativeTime } from '@/lib/console'

type ScimToken = {
  id: string
  label?: string | null
  createdAt: string
  lastUsedAt?: string | null
  expiresAt?: string | null
  revokedAt?: string | null
}

function errorMessage(err: unknown): string {
  if (err && typeof err === 'object' && 'message' in err) {
    const msg = (err as { message?: unknown }).message
    if (typeof msg === 'string' && msg.trim()) return msg
  }
  return 'The request failed.'
}

// Shows a freshly minted/rotated token exactly once.
//
// The plaintext lives ONLY in this dialog's local state and is dropped when the
// dialog closes. It is never written to the Apollo cache, a store, or storage —
// the server cannot show it again.
function PlaintextDialog({
  plaintext,
  rotated,
  onClose,
}: {
  plaintext: string | null
  rotated: boolean
  onClose: () => void
}) {
  const [copied, setCopied] = useState(false)

  async function handleCopy() {
    if (!plaintext) return
    try {
      await navigator.clipboard.writeText(plaintext)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 2000)
    } catch {
      // Clipboard unavailable; the value is selectable in the box.
    }
  }

  function handleClose() {
    setCopied(false)
    onClose()
  }

  return (
    <Dialog open={!!plaintext} onOpenChange={(next) => { if (!next) handleClose() }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{rotated ? 'Token rotated' : 'Token created'}</DialogTitle>
        </DialogHeader>
        <div className="space-y-3 pt-1">
          <Alert variant="destructive">
            <AlertTriangle className="h-4 w-4" />
            <AlertDescription>
              Copy this token now — it will not be shown again. Closing this dialog discards it
              permanently.
            </AlertDescription>
          </Alert>
          <div className="flex items-center gap-2">
            <code
              data-testid="scim-token-plaintext"
              className="flex min-h-10 flex-1 items-center overflow-x-auto rounded-lg border border-border bg-secondary px-3 py-2 font-mono text-xs break-all text-foreground"
            >
              {plaintext}
            </code>
            <Button type="button" variant="outline" onClick={handleCopy} aria-label="Copy token">
              {copied ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
            </Button>
          </div>
          {rotated ? (
            <p className="text-xs text-muted-foreground">
              Update the token in your identity provider now. The previous token no longer works.
            </p>
          ) : null}
        </div>
        <DialogFooter>
          <Button onClick={handleClose}>I have copied it</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function MintDialog({
  open,
  title,
  confirmLabel,
  warning,
  loading,
  onClose,
  onSubmit,
}: {
  open: boolean
  title: string
  confirmLabel: string
  warning?: string
  loading: boolean
  onClose: () => void
  onSubmit: (label: string) => void
}) {
  const [label, setLabel] = useState('')

  function handleOpenChange(next: boolean) {
    if (!next) {
      setLabel('')
      onClose()
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            onSubmit(label.trim())
          }}
          className="space-y-4 pt-1"
        >
          {warning ? (
            <Alert variant="destructive">
              <AlertTriangle className="h-4 w-4" />
              <AlertDescription>{warning}</AlertDescription>
            </Alert>
          ) : null}
          <div className="space-y-1.5">
            <Label htmlFor="token-label">
              Label <span className="text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="token-label"
              placeholder="e.g. Okta production"
              value={label}
              onChange={(e) => setLabel(e.target.value)}
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => handleOpenChange(false)} disabled={loading}>
              Cancel
            </Button>
            <Button type="submit" disabled={loading}>
              {loading ? 'Working…' : confirmLabel}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function tokenState(token: ScimToken): { label: string; tone: 'ok' | 'warn' | 'danger' | 'muted' } {
  if (token.revokedAt) return { label: 'revoked', tone: 'danger' }
  if (token.expiresAt && new Date(token.expiresAt).getTime() <= Date.now()) {
    return { label: 'expired', tone: 'warn' }
  }
  return { label: 'active', tone: 'ok' }
}

export function ScimTokenPanel({ connectionId }: { connectionId: string }) {
  const [showMint, setShowMint] = useState(false)
  const [showRotate, setShowRotate] = useState(false)
  const [revokeTarget, setRevokeTarget] = useState<ScimToken | null>(null)
  const [plaintext, setPlaintext] = useState<string | null>(null)
  const [rotated, setRotated] = useState(false)

  const { data, loading, error, refetch } = useQuery(GetScimTokensDocument, {
    variables: { connectionId },
    fetchPolicy: 'cache-and-network',
  })
  const tokens: ScimToken[] = (data?.scimTokens ?? []) as ScimToken[]

  // Mint and rotate return the token plaintext, which ADR-025 §7 shows exactly
  // once. The component already keeps it in local state and drops it when the
  // dialog closes, but by default Apollo also writes mutation results into the
  // normalized cache under ROOT_MUTATION — a path outside this component's
  // control. 'no-cache' keeps the secret out of the cache entirely.
  //
  // Safe here because neither result is read from the cache: the token list is
  // refreshed by the explicit refetch() after each call.
  const [mintToken, { loading: minting }] = useMutation(MintScimTokenDocument, {
    fetchPolicy: 'no-cache',
  })
  const [rotateToken, { loading: rotating }] = useMutation(RotateScimTokenDocument, {
    fetchPolicy: 'no-cache',
  })
  // revoke returns only a Boolean — no secret, so normal cache behaviour is fine.
  const [revokeToken, { loading: revoking }] = useMutation(RevokeScimTokenDocument)

  async function handleMint(label: string) {
    try {
      const res = await mintToken({
        variables: { connectionId, label: label || null, expiresAt: null },
      })
      setShowMint(false)
      setRotated(false)
      setPlaintext(res.data?.mintScimToken.plaintext ?? null)
      void refetch()
    } catch (err) {
      toast.error(errorMessage(err))
    }
  }

  async function handleRotate(label: string) {
    try {
      const res = await rotateToken({
        variables: { connectionId, label: label || null, expiresAt: null },
      })
      setShowRotate(false)
      setRotated(true)
      setPlaintext(res.data?.rotateScimToken.plaintext ?? null)
      void refetch()
    } catch (err) {
      toast.error(errorMessage(err))
    }
  }

  async function handleRevoke() {
    if (!revokeTarget) return
    try {
      await revokeToken({ variables: { connectionId, tokenId: revokeTarget.id } })
      setRevokeTarget(null)
      toast.success('Token revoked.')
      void refetch()
    } catch (err) {
      toast.error(errorMessage(err))
    }
  }

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="space-y-1.5">
            <CardTitle className="flex items-center gap-2">
              <Ticket className="h-4 w-4 text-muted-foreground" />
              SCIM tokens
            </CardTitle>
            <CardDescription>
              The bearer token your identity provider presents. It is what scopes every SCIM request
              to this connection.
            </CardDescription>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button variant="outline" onClick={() => setShowRotate(true)} disabled={tokens.length === 0}>
              <RefreshCw className="mr-2 h-4 w-4" />
              Rotate
            </Button>
            <Button onClick={() => setShowMint(true)}>
              <KeyRound className="mr-2 h-4 w-4" />
              Mint token
            </Button>
          </div>
        </div>
      </CardHeader>

      <CardContent className="p-0">
        {loading && tokens.length === 0 ? (
          <div className="space-y-2 p-5">
            <Skeleton className="h-12 w-full" />
            <Skeleton className="h-12 w-full" />
          </div>
        ) : error ? (
          <ErrorState
            title="Could not load tokens"
            description={error.message}
            action={<Button variant="outline" onClick={() => void refetch()}>Retry</Button>}
          />
        ) : tokens.length === 0 ? (
          <EmptyState
            icon={<KeyRound className="h-6 w-6" />}
            title="No SCIM tokens"
            description="Mint a token and paste it into your identity provider together with the SCIM base URL."
            action={<Button onClick={() => setShowMint(true)}>Mint token</Button>}
          />
        ) : (
          <div className="divide-y divide-border/40">
            {tokens.map((token) => {
              const state = tokenState(token)
              return (
                <div key={token.id} className="flex flex-wrap items-center gap-3 px-5 py-3.5">
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-sm font-medium">
                      {token.label || 'Unlabelled token'}
                    </div>
                    <div className="mt-0.5 text-xs text-muted-foreground">
                      Created {relativeTime(token.createdAt)} · Last used{' '}
                      {relativeTime(token.lastUsedAt)}
                      {token.expiresAt ? ` · Expires ${relativeTime(token.expiresAt)}` : ''}
                    </div>
                  </div>
                  <StatusPill label={state.label} tone={state.tone} />
                  {token.revokedAt ? null : (
                    <Button
                      variant="outline"
                      onClick={() => setRevokeTarget(token)}
                      aria-label="Revoke token"
                    >
                      <Trash2 className="h-4 w-4" />
                    </Button>
                  )}
                </div>
              )
            })}
          </div>
        )}
      </CardContent>

      <MintDialog
        open={showMint}
        title="Mint SCIM token"
        confirmLabel="Mint token"
        loading={minting}
        onClose={() => setShowMint(false)}
        onSubmit={(label) => void handleMint(label)}
      />

      <MintDialog
        open={showRotate}
        title="Rotate SCIM token"
        confirmLabel="Rotate token"
        warning="Rotating issues a new token and invalidates the current one. Provisioning breaks until you update the token in your identity provider."
        loading={rotating}
        onClose={() => setShowRotate(false)}
        onSubmit={(label) => void handleRotate(label)}
      />

      <PlaintextDialog
        plaintext={plaintext}
        rotated={rotated}
        onClose={() => setPlaintext(null)}
      />

      <Dialog open={!!revokeTarget} onOpenChange={(next) => { if (!next) setRevokeTarget(null) }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke token</DialogTitle>
          </DialogHeader>
          <p className="text-sm text-muted-foreground">
            Revoking{' '}
            <span className="font-semibold text-foreground">
              {revokeTarget?.label || 'this token'}
            </span>{' '}
            stops all SCIM provisioning that uses it, immediately and permanently.
          </p>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRevokeTarget(null)} disabled={revoking}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={() => void handleRevoke()} disabled={revoking}>
              {revoking ? 'Revoking…' : 'Revoke'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  )
}
