import { useEffect, useState, type FormEvent } from 'react'
import { ApiError } from '@/api/client'
import { listPlans } from '@/api/reads'
import { createTenant } from '@/api/tenants'
import type { CreateTenantResponse, PlanSummary } from '@/api/types'
import { ReasonField } from '@/components/ReasonField'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { reasonValid } from '@/lib/pool-format'
import { EMAIL_PATTERN, SLUG_PATTERN, slugFromName, tenantCreateErrorMessage } from '@/lib/tenant-format'

// Create tenant (Sprint 23 S2, D-44). Super-admin only; the controller decides
// (decide()), hiding the button is UX only. The workspace is created through
// the same bootstrap as self-signup; its first admin gets a single-use
// invitation by email (7 days). The console never sees the invitation token.

const DEFAULT_PLAN = 'default'

type PlansState = { status: 'loading' } | { status: 'ready'; items: PlanSummary[] } | { status: 'error' }

export function CreateTenantDialog({ onCancel, onDone }: { onCancel: () => void; onDone: (created: CreateTenantResponse) => void }) {
  const [plans, setPlans] = useState<PlansState>({ status: 'loading' })
  const [name, setName] = useState('')
  const [slug, setSlug] = useState('')
  const [slugEdited, setSlugEdited] = useState(false)
  const [planId, setPlanId] = useState(DEFAULT_PLAN)
  const [email, setEmail] = useState('')
  const [reason, setReason] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let live = true
    listPlans().then(
      (res) => live && setPlans({ status: 'ready', items: res.items }),
      (err: unknown) => live && !(err instanceof ApiError && err.sessionEnded) && setPlans({ status: 'error' }),
    )
    return () => {
      live = false
    }
  }, [])

  const effectiveSlug = slugEdited ? slug : slugFromName(name)
  const defaultPlan = plans.status === 'ready' ? plans.items.find((p) => p.is_default) : undefined
  const nameOk = name.trim().length > 0 && name.trim().length <= 100
  const slugOk = SLUG_PATTERN.test(effectiveSlug)
  const emailOk = EMAIL_PATTERN.test(email.trim())
  const valid = nameOk && slugOk && emailOk && reasonValid(reason)

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!valid || pending) return
    setPending(true)
    setError(null)
    try {
      const created = await createTenant(effectiveSlug, name.trim(), planId === DEFAULT_PLAN ? null : planId, email.trim(), reason.trim())
      onDone(created)
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
            <DialogTitle>Create tenant</DialogTitle>
            <DialogDescription>
              Creates the workspace the same way as self-signup: its trust domain, workspace CA, default policy and plan. The
              first admin gets a single-use invitation by email, valid for 7 days. They accept it by signing in with that email.
            </DialogDescription>
          </DialogHeader>
          {error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          <div className="grid gap-2">
            <Label htmlFor="tenant-name">Name</Label>
            <Input id="tenant-name" value={name} autoComplete="off" onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="tenant-slug">Slug</Label>
            <Input
              id="tenant-slug"
              value={effectiveSlug}
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              aria-invalid={effectiveSlug !== '' && !slugOk}
              aria-describedby="tenant-slug-hint"
              onChange={(e) => {
                setSlugEdited(true)
                setSlug(e.target.value)
              }}
            />
            <p id="tenant-slug-hint" className="text-xs text-muted-foreground">
              Trust domain{' '}
              <span className="font-mono text-foreground" data-trust-domain>
                ws-{effectiveSlug || '…'}.zecurity.in
              </span>
              . The slug can’t be changed later.
            </p>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="tenant-plan">Plan</Label>
            <Select value={planId} onValueChange={setPlanId} disabled={plans.status !== 'ready'}>
              <SelectTrigger id="tenant-plan" aria-label="Plan">
                <SelectValue placeholder={plans.status === 'loading' ? 'Loading plans…' : 'Choose a plan'} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={DEFAULT_PLAN}>Default plan{defaultPlan ? ` (${defaultPlan.name})` : ''}</SelectItem>
                {plans.status === 'ready' &&
                  plans.items.map((p) => (
                    <SelectItem key={p.id} value={p.id}>
                      {p.name}
                    </SelectItem>
                  ))}
              </SelectContent>
            </Select>
            {plans.status === 'error' && <p className="text-xs text-destructive">Couldn’t load plans. The default plan will be used.</p>}
          </div>
          <div className="grid gap-2">
            <Label htmlFor="tenant-admin-email">First admin’s email</Label>
            <Input
              id="tenant-admin-email"
              type="email"
              value={email}
              autoComplete="off"
              aria-invalid={email !== '' && !emailOk}
              aria-describedby="tenant-admin-email-hint"
              onChange={(e) => setEmail(e.target.value)}
            />
            <p id="tenant-admin-email-hint" className="text-xs text-muted-foreground">
              Must not already belong to another Zecurity workspace: that account would sign in to its own workspace instead.
            </p>
          </div>
          <ReasonField id="tenant-create-reason" value={reason} onChange={setReason} />
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || pending}>
              {pending ? 'Creating…' : 'Create tenant'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
