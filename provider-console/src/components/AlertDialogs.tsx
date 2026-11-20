import { useState, type FormEvent, type ReactNode } from 'react'
import { Check, Copy } from 'lucide-react'
import {
  createAlertChannel,
  deleteAlertChannel,
  rotateAlertChannelSecret,
  testAlertChannel,
  updateAlertChannel,
  updateAlertRule,
  type AlertChannel,
  type AlertRule,
  type AlertSeverity,
  type ChannelFields,
  type ChannelTestResult,
  type ChannelType,
  type RuleChange,
  type ScopeFilter,
} from '@/api/alerts'
import { ReasonField } from '@/components/ReasonField'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  alertErrorMessage,
  formatSeconds,
  kindLabel,
  parseRecipients,
  parseSeconds,
  SCOPE_FILTER_LABELS,
  SEVERITY_LABELS,
} from '@/lib/alert-format'
import { reasonValid } from '@/lib/pool-format'

// Alert rule and channel dialogs (Sprint 24 A3, F26; D-52, D-53).
// Super-admin only; the controller decides. Every write needs a reason
// (audited). A webhook secret is shown once (SecretDialog) and never stored.

const SELECT_CLASS =
  'flex h-9 w-full rounded-lg border border-input bg-field px-3 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/30'

function FormDialog({
  title,
  description,
  children,
  error,
  pending,
  valid,
  submitLabel,
  pendingLabel,
  destructive,
  onSubmit,
  onCancel,
}: {
  title: string
  description: ReactNode
  children?: ReactNode
  error: string | null
  pending: boolean
  valid: boolean
  submitLabel: string
  pendingLabel: string
  destructive?: boolean
  onSubmit: () => void
  onCancel: () => void
}) {
  function submit(e: FormEvent) {
    e.preventDefault()
    if (valid && !pending) onSubmit()
  }
  return (
    <Dialog open onOpenChange={(o) => !o && !pending && onCancel()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            <DialogDescription>{description}</DialogDescription>
          </DialogHeader>
          {error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          {children}
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" variant={destructive ? 'destructive' : 'default'} disabled={!valid || pending}>
              {pending ? pendingLabel : submitLabel}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function useWrite() {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  async function run(fn: () => Promise<void>) {
    setPending(true)
    setError(null)
    try {
      await fn()
    } catch (err) {
      setError(alertErrorMessage(err))
    } finally {
      setPending(false)
    }
  }
  return { pending, error, run }
}

function Field({ id, label, hint, children }: { id: string; label: string; hint?: string; children: ReactNode }) {
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      {children}
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  )
}

function SeveritySelect({ id, value, onChange }: { id: string; value: AlertSeverity; onChange: (v: AlertSeverity) => void }) {
  return (
    <select id={id} className={SELECT_CLASS} value={value} onChange={(e) => onChange(e.target.value as AlertSeverity)}>
      {(['critical', 'warning', 'info'] as const).map((s) => (
        <option key={s} value={s}>
          {SEVERITY_LABELS[s]}
        </option>
      ))}
    </select>
  )
}

// Rules.

export function EditRuleDialog({ rule, onCancel, onDone }: { rule: AlertRule; onCancel: () => void; onDone: (text: string) => void }) {
  const [enabled, setEnabled] = useState(rule.enabled)
  const [severity, setSeverity] = useState<AlertSeverity>(rule.severity)
  const [forS, setForS] = useState(String(rule.for_seconds))
  const [renotify, setRenotify] = useState(String(rule.renotify_seconds))
  const [threshold, setThreshold] = useState(String(rule.params.threshold_percent ?? ''))
  const [days, setDays] = useState(String(rule.params.days ?? ''))
  const [reason, setReason] = useState('')
  const { pending, error, run } = useWrite()

  const forV = parseSeconds(forS, 1, 86400, true)
  const renotifyV = parseSeconds(renotify, 300, 604800, true)
  const thresholdV = rule.kind === 'relay.over_capacity' ? parseSeconds(threshold, 1, 1000, false) : null
  const daysV = rule.kind === 'certificate.expiring' ? parseSeconds(days, 1, 365, false) : null
  const paramsOk =
    (rule.kind !== 'relay.over_capacity' || thresholdV !== null) && (rule.kind !== 'certificate.expiring' || daysV !== null)

  const change: RuleChange = {}
  if (enabled !== rule.enabled) change.enabled = enabled
  if (severity !== rule.severity) change.severity = severity
  if (forV !== null && forV !== rule.for_seconds) change.for_seconds = forV
  if (renotifyV !== null && renotifyV !== rule.renotify_seconds) change.renotify_seconds = renotifyV
  if (thresholdV !== null && thresholdV !== rule.params.threshold_percent) change.params = { threshold_percent: thresholdV }
  if (daysV !== null && daysV !== rule.params.days) change.params = { days: daysV }
  const valid = forV !== null && renotifyV !== null && paramsOk && Object.keys(change).length > 0 && reasonValid(reason)

  return (
    <FormDialog
      title="Edit alert rule"
      description={
        <>
          <strong className="text-foreground">{rule.name}</strong> ({kindLabel(rule.kind)}, {rule.scope} alerts). Disabling it
          resolves its open alerts on the next evaluation.
        </>
      }
      error={error}
      pending={pending}
      valid={valid}
      submitLabel="Save rule"
      pendingLabel="Saving…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(async () => {
          await updateAlertRule(rule.id, change, reason.trim())
          onDone(`Rule ${rule.name} saved.`)
        })
      }
    >
      <label className="inline-flex items-center gap-2 text-sm">
        <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
        Enabled
      </label>
      <Field id="rule-severity" label="Severity">
        <SeveritySelect id="rule-severity" value={severity} onChange={setSeverity} />
      </Field>
      {rule.kind === 'relay.over_capacity' && (
        <Field id="rule-threshold" label="Threshold (% of link capacity)" hint="Fires at or above this share of the relay’s configured link capacity.">
          <Input id="rule-threshold" inputMode="numeric" value={threshold} aria-invalid={thresholdV === null} onChange={(e) => setThreshold(e.target.value)} />
        </Field>
      )}
      {rule.kind === 'certificate.expiring' && (
        <Field id="rule-days" label="Days before expiry" hint="Platform CAs within this many days. Relay and controller certificates: within 24 hours.">
          <Input id="rule-days" inputMode="numeric" value={days} aria-invalid={daysV === null} onChange={(e) => setDays(e.target.value)} />
        </Field>
      )}
      <Field id="rule-for" label="Fire after (seconds)" hint={forV !== null ? `The condition must hold ${formatSeconds(forV)}. 0 to 86400.` : '0 to 86400.'}>
        <Input id="rule-for" inputMode="numeric" value={forS} aria-invalid={forV === null} onChange={(e) => setForS(e.target.value)} />
      </Field>
      <Field id="rule-renotify" label="Notify again while firing (seconds)" hint="0 = never; otherwise 300 to 604800.">
        <Input id="rule-renotify" inputMode="numeric" value={renotify} aria-invalid={renotifyV === null} onChange={(e) => setRenotify(e.target.value)} />
      </Field>
      <ReasonField id="rule-reason" value={reason} onChange={setReason} />
    </FormDialog>
  )
}

// Channels.

function ChannelFilterFields({
  scope,
  minSeverity,
  onScope,
  onMinSeverity,
}: {
  scope: ScopeFilter
  minSeverity: AlertSeverity
  onScope: (v: ScopeFilter) => void
  onMinSeverity: (v: AlertSeverity) => void
}) {
  return (
    <>
      <Field id="channel-scope" label="Receives" hint="A channel shared with a third party should receive platform alerts only.">
        <select id="channel-scope" className={SELECT_CLASS} value={scope} onChange={(e) => onScope(e.target.value as ScopeFilter)}>
          {(['all', 'platform', 'tenant'] as const).map((s) => (
            <option key={s} value={s}>
              {SCOPE_FILTER_LABELS[s]}
            </option>
          ))}
        </select>
      </Field>
      <Field id="channel-min" label="Minimum severity">
        <SeveritySelect id="channel-min" value={minSeverity} onChange={onMinSeverity} />
      </Field>
    </>
  )
}

export function CreateChannelDialog({
  onCancel,
  onCreated,
}: {
  onCancel: () => void
  onCreated: (channel: AlertChannel, secret: string | undefined) => void
}) {
  const [name, setName] = useState('')
  const [type, setType] = useState<ChannelType>('webhook')
  const [url, setUrl] = useState('')
  const [recipients, setRecipients] = useState('')
  const [scope, setScope] = useState<ScopeFilter>('all')
  const [minSeverity, setMinSeverity] = useState<AlertSeverity>('warning')
  const [reason, setReason] = useState('')
  const { pending, error, run } = useWrite()
  const parsedRecipients = parseRecipients(recipients)
  const nameOk = name.trim().length > 0 && name.trim().length <= 100
  const targetOk = type === 'webhook' ? /^https:\/\/\S+$/.test(url.trim()) : parsedRecipients !== null
  const valid = nameOk && targetOk && reasonValid(reason)
  return (
    <FormDialog
      title="Add alert channel"
      description="Alerts are sent to every enabled channel whose filters match. A webhook receives the generic signed JSON payload; wire it into Slack, PagerDuty or similar on their side."
      error={error}
      pending={pending}
      valid={valid}
      submitLabel="Add channel"
      pendingLabel="Adding…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(async () => {
          const fields: ChannelFields = { name: name.trim(), type, scope_filter: scope, min_severity: minSeverity }
          if (type === 'webhook') fields.url = url.trim()
          else fields.recipients = parsedRecipients && parsedRecipients.length > 0 ? parsedRecipients : null
          const res = await createAlertChannel(fields, reason.trim())
          onCreated(res.channel, res.secret)
        })
      }
    >
      <Field id="channel-name" label="Name">
        <Input id="channel-name" value={name} maxLength={100} autoComplete="off" onChange={(e) => setName(e.target.value)} />
      </Field>
      <Field id="channel-type" label="Type">
        <select id="channel-type" className={SELECT_CLASS} value={type} onChange={(e) => setType(e.target.value as ChannelType)}>
          <option value="webhook">Webhook</option>
          <option value="email">Email</option>
        </select>
      </Field>
      {type === 'webhook' ? (
        <Field id="channel-url" label="Webhook URL" hint="HTTPS to a public host. Its path is kept secret: the console shows only the host.">
          <Input id="channel-url" value={url} autoComplete="off" spellCheck={false} onChange={(e) => setUrl(e.target.value)} />
        </Field>
      ) : (
        <Field id="channel-recipients" label="Recipients" hint="Email addresses, comma separated. Leave empty to send to every active super-admin.">
          <Input
            id="channel-recipients"
            value={recipients}
            autoComplete="off"
            aria-invalid={parsedRecipients === null}
            onChange={(e) => setRecipients(e.target.value)}
          />
        </Field>
      )}
      <ChannelFilterFields scope={scope} minSeverity={minSeverity} onScope={setScope} onMinSeverity={setMinSeverity} />
      <ReasonField id="channel-create-reason" value={reason} onChange={setReason} />
    </FormDialog>
  )
}

export function EditChannelDialog({ channel, onCancel, onDone }: { channel: AlertChannel; onCancel: () => void; onDone: (text: string) => void }) {
  const [name, setName] = useState(channel.name)
  const [enabled, setEnabled] = useState(channel.enabled)
  const [scope, setScope] = useState<ScopeFilter>(channel.scope_filter)
  const [minSeverity, setMinSeverity] = useState<AlertSeverity>(channel.min_severity)
  const [url, setUrl] = useState('')
  const [recipients, setRecipients] = useState((channel.recipients ?? []).join(', '))
  const [reason, setReason] = useState('')
  const { pending, error, run } = useWrite()
  const parsedRecipients = parseRecipients(recipients)
  const change: ChannelFields = {}
  if (name.trim() !== channel.name) change.name = name.trim()
  if (enabled !== channel.enabled) change.enabled = enabled
  if (scope !== channel.scope_filter) change.scope_filter = scope
  if (minSeverity !== channel.min_severity) change.min_severity = minSeverity
  if (channel.type === 'webhook' && url.trim() !== '') change.url = url.trim()
  if (channel.type === 'email' && parsedRecipients !== null) {
    const next = parsedRecipients.length > 0 ? parsedRecipients : null
    if (JSON.stringify(next) !== JSON.stringify(channel.recipients)) change.recipients = next
  }
  const valid =
    name.trim().length > 0 &&
    (channel.type !== 'email' || parsedRecipients !== null) &&
    (url.trim() === '' || /^https:\/\/\S+$/.test(url.trim())) &&
    Object.keys(change).length > 0 &&
    reasonValid(reason)
  return (
    <FormDialog
      title="Edit alert channel"
      description={
        <>
          <strong className="text-foreground">{channel.name}</strong> ({channel.type}). Changes apply to alerts sent from now on,
          including retries already queued.
        </>
      }
      error={error}
      pending={pending}
      valid={valid}
      submitLabel="Save channel"
      pendingLabel="Saving…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(async () => {
          await updateAlertChannel(channel.id, change, reason.trim())
          onDone(`Channel ${name.trim()} saved.`)
        })
      }
    >
      <Field id="channel-edit-name" label="Name">
        <Input id="channel-edit-name" value={name} maxLength={100} autoComplete="off" onChange={(e) => setName(e.target.value)} />
      </Field>
      <label className="inline-flex items-center gap-2 text-sm">
        <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
        Enabled
      </label>
      {channel.type === 'webhook' ? (
        <Field id="channel-edit-url" label="New webhook URL" hint={`Now: ${channel.url_host ?? 'unknown'}… Leave empty to keep it. The secret doesn’t change.`}>
          <Input id="channel-edit-url" value={url} autoComplete="off" spellCheck={false} onChange={(e) => setUrl(e.target.value)} />
        </Field>
      ) : (
        <Field id="channel-edit-recipients" label="Recipients" hint="Empty = every active super-admin.">
          <Input
            id="channel-edit-recipients"
            value={recipients}
            autoComplete="off"
            aria-invalid={parsedRecipients === null}
            onChange={(e) => setRecipients(e.target.value)}
          />
        </Field>
      )}
      <ChannelFilterFields scope={scope} minSeverity={minSeverity} onScope={setScope} onMinSeverity={setMinSeverity} />
      <ReasonField id="channel-edit-reason" value={reason} onChange={setReason} />
    </FormDialog>
  )
}

export function TestChannelDialog({
  channel,
  onCancel,
  onResult,
}: {
  channel: AlertChannel
  onCancel: () => void
  onResult: (r: ChannelTestResult) => void
}) {
  const [reason, setReason] = useState('')
  const { pending, error, run } = useWrite()
  return (
    <FormDialog
      title="Send a test alert"
      description={
        <>
          Sends a test alert to <strong className="text-foreground">{channel.name}</strong> now. Recorded in the audit log with the
          result.
        </>
      }
      error={error}
      pending={pending}
      valid={reasonValid(reason)}
      submitLabel="Send test"
      pendingLabel="Sending…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(async () => {
          onResult(await testAlertChannel(channel.id, reason.trim()))
        })
      }
    >
      <ReasonField id="channel-test-reason" value={reason} onChange={setReason} />
    </FormDialog>
  )
}

export function RotateSecretDialog({ channel, onCancel, onRotated }: { channel: AlertChannel; onCancel: () => void; onRotated: (secret: string) => void }) {
  const [reason, setReason] = useState('')
  const { pending, error, run } = useWrite()
  return (
    <FormDialog
      title="Rotate signing secret"
      description={
        <>
          Gives <strong className="text-foreground">{channel.name}</strong> a new secret. The old one stops working at once: update
          the receiver right after.
        </>
      }
      error={error}
      pending={pending}
      valid={reasonValid(reason)}
      destructive
      submitLabel="Rotate secret"
      pendingLabel="Rotating…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(async () => {
          onRotated((await rotateAlertChannelSecret(channel.id, reason.trim())).secret)
        })
      }
    >
      <ReasonField id="channel-rotate-reason" value={reason} onChange={setReason} />
    </FormDialog>
  )
}

export function DeleteChannelDialog({ channel, onCancel, onDone }: { channel: AlertChannel; onCancel: () => void; onDone: (text: string) => void }) {
  const [reason, setReason] = useState('')
  const { pending, error, run } = useWrite()
  return (
    <FormDialog
      title="Delete alert channel"
      description={
        <>
          Deletes <strong className="text-foreground">{channel.name}</strong>, its queued deliveries and its delivery history.
        </>
      }
      error={error}
      pending={pending}
      valid={reasonValid(reason)}
      destructive
      submitLabel="Delete channel"
      pendingLabel="Deleting…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(async () => {
          await deleteAlertChannel(channel.id, reason.trim())
          onDone(`Channel ${channel.name} deleted.`)
        })
      }
    >
      <ReasonField id="channel-delete-reason" value={reason} onChange={setReason} />
    </FormDialog>
  )
}

/**
 * Shows a webhook signing secret once. It lives only in this dialog's props;
 * closing the dialog drops it, and it is never stored.
 */
export function SecretDialog({ channelName, secret, onClose }: { channelName: string; secret: string; onClose: () => void }) {
  const [copied, setCopied] = useState(false)
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Signing secret for {channelName}</DialogTitle>
          <DialogDescription>
            Copy it now: it is shown only once. The receiver verifies <code>X-Zecurity-Signature</code> (HMAC-SHA256 over
            “timestamp.body”) with it and rejects timestamps older than 5 minutes.
          </DialogDescription>
        </DialogHeader>
        <div className="flex items-center gap-2">
          <code aria-label="Signing secret" className="min-w-0 flex-1 break-all rounded-lg border border-input bg-field px-3 py-2 font-mono text-xs">
            {secret}
          </code>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => {
              void navigator.clipboard?.writeText(secret).then(() => setCopied(true), () => setCopied(false))
            }}
            aria-label="Copy secret"
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
