import { useState, type FormEvent } from 'react'
import { Check, Copy } from 'lucide-react'
import { API_KEY_ROLES, createApiKey, revokeApiKey, type ApiKey, type ApiKeyRole } from '@/api/apiKeys'
import { ConfirmDangerDialog } from '@/components/ConfirmDangerDialog'
import { ReasonField } from '@/components/ReasonField'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { ROLE_LABELS } from '@/auth/roles'
import {
  API_KEY_ROLE_HELP,
  EXPIRY_PRESETS,
  apiKeyErrorMessage,
  cidrLooksValid,
  dateExpiry,
  expiryValid,
  parseCidrs,
  presetExpiry,
} from '@/lib/apikey-format'
import { reasonValid } from '@/lib/pool-format'
import { useWrite } from '@/lib/useWrite'

// API key dialogs (Sprint 27 K2, D-76). Create → the full key is shown once
// (KeyShownOnceDialog) and lives only in that dialog's props: closing it drops
// the key, and it is never written to session or local storage. Revoke needs a
// reason and the key's name typed exactly (D-17 style); the server checks both.

const FIELD_CLASS =
  'flex h-9 w-full rounded-lg border border-input bg-field px-3 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/30'

type Expiry = { kind: 'preset'; days: number } | { kind: 'date'; date: string }

export function CreateApiKeyDialog({
  onCancel,
  onCreated,
  now = () => new Date(),
}: {
  onCancel: () => void
  onCreated: (created: { api_key: ApiKey; key: string }) => void
  now?: () => Date
}) {
  const [name, setName] = useState('')
  const [role, setRole] = useState<ApiKeyRole>('auditor')
  const [expiry, setExpiry] = useState<Expiry>({ kind: 'preset', days: 90 })
  const [cidrText, setCidrText] = useState('')
  const [reason, setReason] = useState('')
  const [error, setError] = useState<string | null>(null)
  const { pending, run } = useWrite()

  const expiresAt = expiry.kind === 'preset' ? presetExpiry(expiry.days, now()) : dateExpiry(expiry.date)
  const cidrs = parseCidrs(cidrText)
  const badCidr = cidrs.find((c) => !cidrLooksValid(c))
  const nameOk = name.trim().length > 0 && Array.from(name.trim()).length <= 64
  const valid = nameOk && expiryValid(expiresAt, now()) && !badCidr && cidrs.length <= 20 && reasonValid(reason)

  function submit(e: FormEvent) {
    e.preventDefault()
    if (!valid || pending || !expiresAt) return
    setError(null)
    void run(async () => {
      try {
        const body = { name: name.trim(), role, expires_at: expiresAt.toISOString().replace(/\.\d{3}Z$/, 'Z'), ...(cidrs.length ? { allowed_cidrs: cidrs } : {}) }
        onCreated(await createApiKey(body, reason.trim()))
      } catch (err) {
        setError(apiKeyErrorMessage(err))
      }
    })
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !pending && onCancel()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>Create API key</DialogTitle>
            <DialogDescription>
              A read-only key for a script or integration. It has one role, expires within a year, and can never write or sign in.
            </DialogDescription>
          </DialogHeader>
          {error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          <div className="grid gap-2">
            <Label htmlFor="key-name">Name</Label>
            <Input id="key-name" value={name} maxLength={64} autoComplete="off" onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="key-role">Role</Label>
            <select id="key-role" className={FIELD_CLASS} value={role} onChange={(e) => setRole(e.target.value as ApiKeyRole)}>
              {API_KEY_ROLES.map((r) => (
                <option key={r} value={r}>
                  {ROLE_LABELS[r] ?? r}
                </option>
              ))}
            </select>
            <p className="text-xs text-muted-foreground" data-testid="key-role-help">
              Can read: {API_KEY_ROLE_HELP[role]}
            </p>
          </div>
          <fieldset className="grid gap-2">
            <legend className="mb-1 text-sm font-medium">Expires</legend>
            <div className="flex flex-wrap gap-2" role="group" aria-label="Expiry presets">
              {EXPIRY_PRESETS.map((d) => (
                <Button
                  key={d}
                  type="button"
                  size="sm"
                  variant={expiry.kind === 'preset' && expiry.days === d ? 'default' : 'outline'}
                  aria-pressed={expiry.kind === 'preset' && expiry.days === d}
                  onClick={() => setExpiry({ kind: 'preset', days: d })}
                >
                  {d} days
                </Button>
              ))}
            </div>
            <Label htmlFor="key-expiry-date" className="text-xs text-muted-foreground">
              Or a date (at most 365 days away)
            </Label>
            <Input
              id="key-expiry-date"
              type="date"
              value={expiry.kind === 'date' ? expiry.date : ''}
              onChange={(e) => setExpiry(e.target.value ? { kind: 'date', date: e.target.value } : { kind: 'preset', days: 90 })}
            />
            {expiry.kind === 'date' && !expiryValid(expiresAt, now()) && (
              <p className="text-xs text-destructive">Pick a date in the future and at most 365 days away.</p>
            )}
          </fieldset>
          <div className="grid gap-2">
            <Label htmlFor="key-cidrs">Allowed source addresses (optional)</Label>
            <textarea
              id="key-cidrs"
              rows={2}
              value={cidrText}
              placeholder="203.0.113.0/24, 2001:db8::/48"
              onChange={(e) => setCidrText(e.target.value)}
              aria-invalid={badCidr !== undefined}
              className="flex w-full rounded-lg border border-input bg-field px-3 py-2 font-mono text-xs aria-invalid:border-destructive"
            />
            <p className={badCidr ? 'text-xs text-destructive' : 'text-xs text-muted-foreground'}>
              {badCidr ? `Not a CIDR: ${badCidr}` : 'CIDRs separated by commas or new lines. Empty: any address.'}
            </p>
          </div>
          <ReasonField id="key-create-reason" value={reason} onChange={setReason} />
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || pending}>
              {pending ? 'Creating…' : 'Create key'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/**
 * Shows a new API key once. It lives only in this dialog's props; closing the
 * dialog drops it, and it is never stored.
 */
export function KeyShownOnceDialog({ name, apiKey, onClose }: { name: string; apiKey: string; onClose: () => void }) {
  const [copied, setCopied] = useState(false)
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>API key {name}</DialogTitle>
          <DialogDescription>
            Copy it now. You won’t see this key again. Send it as <code>Authorization: Bearer &lt;key&gt;</code>.
          </DialogDescription>
        </DialogHeader>
        <div className="flex items-center gap-2">
          <code aria-label="API key" className="min-w-0 flex-1 break-all rounded-lg border border-input bg-field px-3 py-2 font-mono text-xs">
            {apiKey}
          </code>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => {
              void navigator.clipboard?.writeText(apiKey).then(() => setCopied(true), () => setCopied(false))
            }}
            aria-label="Copy key"
          >
            {copied ? <Check aria-hidden /> : <Copy aria-hidden />}
            {copied ? 'Copied' : 'Copy'}
          </Button>
        </div>
        <DialogFooter>
          <Button type="button" onClick={onClose}>
            I’ve saved it
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export function RevokeApiKeyDialog({ apiKey, onCancel, onRevoked }: { apiKey: ApiKey; onCancel: () => void; onRevoked: () => void }) {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()
  return (
    <ConfirmDangerDialog
      title="Revoke API key"
      description={
        <>
          Revokes <strong className="text-foreground">{apiKey.name}</strong> ({apiKey.prefix}…) at once. Scripts using it get 401 on their next
          call. This can’t be undone.
        </>
      }
      confirmText={apiKey.name}
      confirmLabel="Revoke key"
      pendingLabel="Revoking…"
      pending={pending}
      error={error ? { message: error } : undefined}
      onCancel={onCancel}
      onConfirm={(reason, typedName) => {
        setPending(true)
        setError(undefined)
        revokeApiKey(apiKey.id, reason, typedName)
          .then(() => onRevoked())
          .catch((err) => setError(apiKeyErrorMessage(err)))
          .finally(() => setPending(false))
      }}
    />
  )
}
