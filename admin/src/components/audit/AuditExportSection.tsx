import { useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import {
  AlertTriangle,
  Check,
  Copy,
  Pencil,
  Plus,
  RefreshCw,
  Send,
  Trash2,
  Webhook,
} from 'lucide-react'
import { toast } from 'sonner'
import {
  AuditExportSinkKind,
  AuditExportStartFrom,
  AuditStream,
  CreateAuditExportSinkDocument,
  DeleteAuditExportSinkDocument,
  GetAuditExportSinksDocument,
  RotateAuditExportSinkSecretDocument,
  TestAuditExportSinkDocument,
  UpdateAuditExportSinkDocument,
  type CreateAuditExportSinkInput,
  type UpdateAuditExportSinkInput,
} from '@/generated/graphql'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { EmptyState, ErrorState, StatusPill, relativeTime } from '@/lib/console'
import { mutationErrorMessage, toastMutationError } from '@/lib/mutationErrors'
import {
  MAX_SINKS,
  STREAM_LABEL,
  initialForm,
  kindLabel,
  validateForm,
  type FormState,
  type Sink,
} from './sinkForm'

// PENDING-11 P-8: SIEM export sinks (AUDIT_ADMIN: Admin + Security). Secrets
// are write-only: the signing secret is shown once after create/rotate (local
// state only, never the Apollo cache), and the auth header value / Splunk
// token are never read back.

// ── one-time secret ─────────────────────────────────────────────────────────

function SecretDialog({ secret, onClose }: { secret: string | null; onClose: () => void }) {
  const [copied, setCopied] = useState(false)
  async function copy() {
    if (!secret) return
    try {
      await navigator.clipboard.writeText(secret)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 2000)
    } catch {
      // The value is selectable in the box.
    }
  }
  return (
    <Dialog open={!!secret} onOpenChange={(o) => { if (!o) { setCopied(false); onClose() } }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Signing secret</DialogTitle>
        </DialogHeader>
        <div className="space-y-3 pt-1">
          <Alert variant="destructive">
            <AlertTriangle className="h-4 w-4" />
            <AlertDescription>
              Copy this secret now; it will not be shown again. Your receiver uses it to check the
              X-Zecurity-Signature header (HMAC-SHA256 of "&lt;t&gt;.&lt;body&gt;").
            </AlertDescription>
          </Alert>
          <div className="flex items-center gap-2">
            <code
              data-testid="audit-sink-secret"
              className="flex min-h-10 flex-1 items-center overflow-x-auto rounded-lg border border-border bg-secondary px-3 py-2 font-mono text-xs break-all"
            >
              {secret}
            </code>
            <Button type="button" variant="outline" onClick={copy} aria-label="Copy secret">
              {copied ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
            </Button>
          </div>
        </div>
        <DialogFooter>
          <Button onClick={() => { setCopied(false); onClose() }}>I have copied it</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// ── create / edit ───────────────────────────────────────────────────────────

function SinkDialog({
  open,
  sink,
  onClose,
  onCreated,
}: {
  open: boolean
  sink?: Sink
  onClose: () => void
  onCreated: (secret: string) => void
}) {
  const [f, setF] = useState<FormState>(() => initialForm(sink))
  const [error, setError] = useState<string | null>(null)
  const editing = !!sink
  const splunk = f.kind === AuditExportSinkKind.SplunkHec

  const [create, { loading: creating }] = useMutation(CreateAuditExportSinkDocument, {
    fetchPolicy: 'no-cache',
    refetchQueries: [GetAuditExportSinksDocument],
  })
  const [update, { loading: updating }] = useMutation(UpdateAuditExportSinkDocument, {
    refetchQueries: [GetAuditExportSinksDocument],
  })

  const set = <K extends keyof FormState>(k: K, v: FormState[K]) => setF((p) => ({ ...p, [k]: v }))
  const streams = [f.admin && AuditStream.Admin, f.access && AuditStream.Access].filter(Boolean) as AuditStream[]
  const splunkInput = () => ({
    token: f.token || undefined,
    index: f.index || undefined,
    source: f.source || undefined,
    sourcetype: f.sourcetype || undefined,
  })

  async function submit() {
    const msg = validateForm(f, sink)
    if (msg) {
      setError(msg)
      return
    }
    setError(null)
    try {
      if (!sink) {
        const input: CreateAuditExportSinkInput = {
          name: f.name.trim(),
          kind: f.kind,
          url: f.url,
          streams,
          enabled: f.enabled,
          startFrom: f.startFrom,
        }
        if (splunk) input.splunk = splunkInput()
        else if (f.headerName) input.authHeader = { name: f.headerName, value: f.headerValue }
        const res = await create({ variables: { input } })
        const secret = res.data?.createAuditExportSink.signingSecret
        onClose()
        if (secret) onCreated(secret)
        return
      }
      const input: UpdateAuditExportSinkInput = {}
      if (f.name.trim() !== sink.name) input.name = f.name.trim()
      if (f.url !== sink.url) input.url = f.url
      const before = [...sink.streams].sort().join()
      if ([...streams].sort().join() !== before) input.streams = streams
      if (splunk) {
        const changed =
          !!f.token ||
          f.index !== (sink.splunkIndex ?? '') ||
          f.source !== (sink.splunkSource ?? '') ||
          f.sourcetype !== (sink.splunkSourcetype ?? '')
        if (changed) input.splunk = splunkInput()
      } else if (f.clearHeader) {
        input.clearAuthHeader = true
      } else if (f.headerValue) {
        input.authHeader = { name: f.headerName, value: f.headerValue }
      }
      if (Object.keys(input).length > 0) {
        await update({ variables: { id: sink.id, input } })
      }
      onClose()
    } catch (err) {
      setError(mutationErrorMessage(err))
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) onClose() }}>
      <DialogContent className="max-h-[90vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{editing ? `Edit ${sink.name}` : 'Add audit export sink'}</DialogTitle>
        </DialogHeader>
        <form
          className="space-y-4 pt-1"
          onSubmit={(e) => {
            e.preventDefault()
            void submit()
          }}
        >
          <div className="space-y-1.5">
            <Label htmlFor="sink-name">Name</Label>
            <Input id="sink-name" value={f.name} maxLength={64} onChange={(e) => set('name', e.target.value)} />
          </div>
          {!editing ? (
            <div className="space-y-1.5">
              <Label>Destination type</Label>
              <div className="flex gap-2">
                {[AuditExportSinkKind.Webhook, AuditExportSinkKind.SplunkHec].map((k) => (
                  <Button
                    key={k}
                    type="button"
                    variant={f.kind === k ? 'default' : 'outline'}
                    onClick={() => set('kind', k)}
                    aria-pressed={f.kind === k}
                  >
                    {kindLabel(k)}
                  </Button>
                ))}
              </div>
            </div>
          ) : null}
          <div className="space-y-1.5">
            <Label htmlFor="sink-url">URL</Label>
            <Input
              id="sink-url"
              value={f.url}
              placeholder={splunk ? 'https://splunk.example.com:8088/services/collector/event' : 'https://siem.example.com/zecurity'}
              onChange={(e) => set('url', e.target.value)}
            />
            <p className="text-xs text-muted-foreground">
              HTTPS only; private and loopback addresses are refused. Put credentials in the{' '}
              {splunk ? 'token' : 'auth header'}, not the URL.
            </p>
          </div>
          <fieldset className="space-y-1.5">
            <legend className="text-sm font-medium">Streams</legend>
            {(
              [
                ['admin', AuditStream.Admin],
                ['access', AuditStream.Access],
              ] as const
            ).map(([key, s]) => (
              <label key={key} className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={f[key]}
                  onChange={(e) => set(key, e.target.checked)}
                />
                {STREAM_LABEL[s]}
              </label>
            ))}
          </fieldset>
          {!editing ? (
            <fieldset className="space-y-1.5">
              <legend className="text-sm font-medium">Start from</legend>
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="radio"
                  name="start-from"
                  checked={f.startFrom === AuditExportStartFrom.AllHistory}
                  onChange={() => set('startFrom', AuditExportStartFrom.AllHistory)}
                />
                All retained history
              </label>
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="radio"
                  name="start-from"
                  checked={f.startFrom === AuditExportStartFrom.Now}
                  onChange={() => set('startFrom', AuditExportStartFrom.Now)}
                />
                Only new events
              </label>
            </fieldset>
          ) : null}

          {splunk ? (
            <div className="space-y-3">
              <div className="space-y-1.5">
                <Label htmlFor="sink-token">HEC token</Label>
                <Input
                  id="sink-token"
                  type="password"
                  autoComplete="off"
                  value={f.token}
                  placeholder={editing ? 'Leave blank to keep the current token' : ''}
                  onChange={(e) => set('token', e.target.value)}
                />
              </div>
              <div className="grid grid-cols-3 gap-2">
                <div className="space-y-1.5">
                  <Label htmlFor="sink-index">Index</Label>
                  <Input id="sink-index" value={f.index} onChange={(e) => set('index', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="sink-source">Source</Label>
                  <Input id="sink-source" value={f.source} placeholder="zecurity" onChange={(e) => set('source', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="sink-sourcetype">Sourcetype</Label>
                  <Input id="sink-sourcetype" value={f.sourcetype} placeholder="zecurity:audit:v1" onChange={(e) => set('sourcetype', e.target.value)} />
                </div>
              </div>
            </div>
          ) : (
            <div className="space-y-3">
              <div className="grid grid-cols-2 gap-2">
                <div className="space-y-1.5">
                  <Label htmlFor="sink-header-name">Auth header (optional)</Label>
                  <Input
                    id="sink-header-name"
                    value={f.headerName}
                    placeholder="Authorization"
                    disabled={f.clearHeader}
                    onChange={(e) => set('headerName', e.target.value)}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="sink-header-value">Header value</Label>
                  <Input
                    id="sink-header-value"
                    type="password"
                    autoComplete="off"
                    value={f.headerValue}
                    disabled={f.clearHeader}
                    placeholder={editing && sink.authHeaderName ? 'Leave blank to keep' : ''}
                    onChange={(e) => set('headerValue', e.target.value)}
                  />
                </div>
              </div>
              {editing && sink.authHeaderName ? (
                <label className="flex items-center gap-2 text-sm">
                  <input type="checkbox" checked={f.clearHeader} onChange={(e) => set('clearHeader', e.target.checked)} />
                  Remove the auth header
                </label>
              ) : null}
            </div>
          )}

          {!editing ? (
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={f.enabled} onCheckedChange={(v) => set('enabled', v)} aria-label="Enabled" />
              Start delivering immediately
            </label>
          ) : null}

          {error ? (
            <Alert variant="destructive">
              <AlertTriangle className="h-4 w-4" />
              <AlertDescription data-testid="sink-form-error">{error}</AlertDescription>
            </Alert>
          ) : null}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={creating || updating}>
              {editing ? 'Save' : 'Add sink'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// ── list ────────────────────────────────────────────────────────────────────

function StreamStatus({ s }: { s: Sink['status'][number] }) {
  const failing = !!s.failingSince
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
      <span className="font-medium text-foreground">{STREAM_LABEL[s.stream]}</span>
      <StatusPill label={failing ? 'failing' : s.lag > 0 ? 'catching up' : 'up to date'} tone={failing ? 'danger' : s.lag > 0 ? 'warn' : 'ok'} />
      <span>
        {s.lag} pending · seq {s.cursorSeq}/{s.headSeq}
      </span>
      <span>last delivered {s.lastSuccessAt ? relativeTime(s.lastSuccessAt) : 'never'}</span>
      {s.lastError ? <span className="text-destructive">{s.lastError}</span> : null}
    </div>
  )
}

function SinkRow({
  sink,
  onEdit,
  onSecret,
}: {
  sink: Sink
  onEdit: () => void
  onSecret: (secret: string) => void
}) {
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [setEnabled, { loading: toggling }] = useMutation(UpdateAuditExportSinkDocument, {
    onError: toastMutationError,
  })
  const [rotate, { loading: rotating }] = useMutation(RotateAuditExportSinkSecretDocument, {
    fetchPolicy: 'no-cache',
    refetchQueries: [GetAuditExportSinksDocument],
    onError: toastMutationError,
  })
  const [remove, { loading: deleting }] = useMutation(DeleteAuditExportSinkDocument, {
    refetchQueries: [GetAuditExportSinksDocument],
    onError: toastMutationError,
  })
  const [test, { loading: testing }] = useMutation(TestAuditExportSinkDocument, {
    fetchPolicy: 'no-cache',
    onError: toastMutationError,
  })
  const lagging = sink.status.some((s) => s.lagging)

  return (
    <div className="space-y-3 rounded-xl border border-border p-4" data-testid="audit-sink">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <span className="font-semibold">{sink.name}</span>
            <StatusPill label={kindLabel(sink.kind)} tone="info" />
            {!sink.enabled ? <StatusPill label="paused" tone="muted" /> : null}
          </div>
          <div className="truncate font-mono text-xs text-muted-foreground">{sink.url}</div>
          {sink.authHeaderName ? (
            <div className="text-xs text-muted-foreground">
              {sink.kind === AuditExportSinkKind.SplunkHec ? 'HEC token set' : `Auth header: ${sink.authHeaderName}`}
            </div>
          ) : null}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Switch
            checked={sink.enabled}
            disabled={toggling}
            aria-label={`Deliver to ${sink.name}`}
            onCheckedChange={(v) => void setEnabled({ variables: { id: sink.id, input: { enabled: v } } })}
          />
          <Button
            variant="outline"
            size="sm"
            disabled={testing}
            onClick={async () => {
              const res = await test({ variables: { id: sink.id } })
              const r = res.data?.testAuditExportSink
              if (r?.ok) toast.success('Test event delivered.')
              else if (r) toast.error('Test delivery failed', { description: r.error ?? undefined })
            }}
          >
            <Send className="h-4 w-4" /> Test
          </Button>
          <Button variant="outline" size="sm" onClick={onEdit}>
            <Pencil className="h-4 w-4" /> Edit
          </Button>
          <Button
            variant="outline"
            size="sm"
            disabled={rotating}
            onClick={async () => {
              const res = await rotate({ variables: { id: sink.id } })
              const secret = res.data?.rotateAuditExportSinkSecret.signingSecret
              if (secret) onSecret(secret)
            }}
          >
            <RefreshCw className="h-4 w-4" /> Rotate secret
          </Button>
          <Button variant="outline" size="sm" onClick={() => setConfirmDelete(true)} aria-label={`Delete ${sink.name}`}>
            <Trash2 className="h-4 w-4" />
          </Button>
        </div>
      </div>
      {lagging ? (
        <Alert variant="destructive">
          <AlertTriangle className="h-4 w-4" />
          <AlertDescription>
            This sink is more than 7 days behind. Access events are deleted after 90 days whether
            or not they were exported; fix the destination before they are lost.
          </AlertDescription>
        </Alert>
      ) : null}
      <div className="space-y-1.5">
        {sink.status.map((s) => (
          <StreamStatus key={s.stream} s={s} />
        ))}
      </div>

      <Dialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {sink.name}?</DialogTitle>
          </DialogHeader>
          <p className="text-sm text-muted-foreground">
            Delivery stops and the sink's position in each stream is lost. Adding it again starts
            from the beginning of retained history.
          </p>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmDelete(false)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={deleting}
              onClick={async () => {
                await remove({ variables: { id: sink.id } })
                setConfirmDelete(false)
              }}
            >
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

export function AuditExportSection() {
  const { data, loading, error, refetch } = useQuery(GetAuditExportSinksDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 15000,
  })
  const [dialog, setDialog] = useState<{ sink?: Sink } | null>(null)
  const [secret, setSecret] = useState<string | null>(null)
  const sinks = data?.auditExportSinks ?? []

  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-4 space-y-0">
        <div>
          <CardTitle className="text-sm font-medium">Audit export</CardTitle>
          <CardDescription>
            Send the workspace's hash-chained audit log (admin changes) and resource access events
            to a SIEM. Delivery is at least once; deduplicate on workspace, stream and seq.
          </CardDescription>
        </div>
        <Button size="sm" disabled={sinks.length >= MAX_SINKS} onClick={() => setDialog({})}>
          <Plus className="h-4 w-4" /> Add sink
        </Button>
      </CardHeader>
      <CardContent className="space-y-3">
        {error && !data ? (
          <ErrorState
            title="Could not load export sinks"
            description="The request failed."
            action={<Button variant="outline" onClick={() => refetch()}>Retry</Button>}
          />
        ) : loading && !data ? (
          <Skeleton className="h-20 rounded-xl" />
        ) : sinks.length === 0 ? (
          <EmptyState
            icon={<Webhook className="h-6 w-6" />}
            title="No export sinks"
            description="Add a webhook or Splunk HEC destination (up to 3)."
          />
        ) : (
          sinks.map((s) => (
            <SinkRow key={s.id} sink={s} onEdit={() => setDialog({ sink: s })} onSecret={setSecret} />
          ))
        )}
      </CardContent>
      {dialog ? (
        <SinkDialog
          key={dialog.sink?.id ?? 'new'}
          open
          sink={dialog.sink}
          onClose={() => setDialog(null)}
          onCreated={setSecret}
        />
      ) : null}
      <SecretDialog secret={secret} onClose={() => setSecret(null)} />
    </Card>
  )
}
