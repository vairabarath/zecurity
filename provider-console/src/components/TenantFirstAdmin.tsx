import { useState, type FormEvent } from 'react'
import { reinviteFirstAdmin } from '@/api/tenants'
import type { TenantDetail } from '@/api/types'
import { hasRole } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { ReasonField } from '@/components/ReasonField'
import type { RelayActionNotice } from '@/components/RelayDangerZone'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { reasonValid } from '@/lib/pool-format'
import { EMAIL_PATTERN, tenantCreateErrorMessage } from '@/lib/tenant-format'

const SUPER_ADMIN = ['super-admin'] as const

// Tenant detail's "First admin" section (Sprint 23 S2, D-44): shown while the
// provider-issued first-admin seat is unclaimed. An expired invitation refuses
// sign-in; a super-admin sends a new one (earlier links stop working). The
// read carries status and expiry only: no email, no token. Hiding the button
// is UX only; the controller decides.
export function TenantFirstAdminSection({ tenant, onDone }: { tenant: TenantDetail; onDone: (notice: RelayActionNotice) => void }) {
  const role = useSessionStore((s) => s.me?.role)
  const canSend = hasRole(role, SUPER_ADMIN)
  const [sending, setSending] = useState(false)
  const inv = tenant.first_admin_invitation
  if (!inv) return null
  const expired = inv.status === 'expired'
  const when = new Date(inv.expires_at).toLocaleString()

  return (
    <Card role="region" aria-labelledby="tenant-first-admin-title" className="px-5 py-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="tenant-first-admin-title" className="font-semibold">
          First admin
        </h2>
        {canSend && (
          <Button variant={expired ? 'default' : 'outline'} onClick={() => setSending(true)}>
            Send new invitation
          </Button>
        )}
      </div>
      <p className="mt-3 inline-flex flex-wrap items-center gap-2 text-sm" data-first-admin-status={inv.status}>
        {expired ? (
          <>
            <Badge variant="destructive">Invitation expired</Badge>
            <span className="text-muted-foreground">
              The first admin hasn’t signed in, and their sign-in is refused until you send a new invitation.
            </span>
          </>
        ) : (
          <>
            <Badge variant="secondary">Invitation pending</Badge>
            <span className="text-muted-foreground">The first admin hasn’t signed in yet. The invitation expires {when}.</span>
          </>
        )}
      </p>
      {canSend && sending && (
        <ReinviteDialog
          tenant={tenant}
          onCancel={() => setSending(false)}
          onDone={(n) => {
            setSending(false)
            onDone(n)
          }}
        />
      )}
    </Card>
  )
}

function ReinviteDialog({ tenant, onCancel, onDone }: { tenant: TenantDetail; onCancel: () => void; onDone: (n: RelayActionNotice) => void }) {
  const [email, setEmail] = useState('')
  const [reason, setReason] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const emailOk = email.trim() === '' || EMAIL_PATTERN.test(email.trim())
  const valid = emailOk && reasonValid(reason)

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!valid || pending) return
    setPending(true)
    setError(null)
    try {
      const res = await reinviteFirstAdmin(tenant.id, email.trim(), reason.trim())
      onDone({
        tone: 'success',
        text: `A new invitation was sent to ${res.invitation.email}; it can be used once, until ${new Date(res.invitation.expires_at).toLocaleString()}. Earlier links no longer work.`,
      })
    } catch (err) {
      setError(tenantCreateErrorMessage(err))
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !pending && onCancel()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>Send new invitation</DialogTitle>
            <DialogDescription>
              Sends <strong className="text-foreground">{tenant.name}</strong>’s first admin a new single-use invitation, valid for 7
              days. Earlier invitation links stop working at once.
            </DialogDescription>
          </DialogHeader>
          {error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          <div className="grid gap-2">
            <Label htmlFor="reinvite-email">New email (optional)</Label>
            <Input
              id="reinvite-email"
              type="email"
              value={email}
              autoComplete="off"
              aria-invalid={!emailOk}
              aria-describedby="reinvite-email-hint"
              onChange={(e) => setEmail(e.target.value)}
            />
            <p id="reinvite-email-hint" className="text-xs text-muted-foreground">
              Leave empty to send it to the same address. Another address replaces the invited admin.
            </p>
          </div>
          <ReasonField id="reinvite-reason" value={reason} onChange={setReason} />
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || pending}>
              {pending ? 'Sending…' : 'Send invitation'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
