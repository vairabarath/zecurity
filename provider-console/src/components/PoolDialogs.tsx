import { useRef, useState, type FormEvent, type ReactNode } from 'react'
import { ApiError } from '@/api/client'
import { createRelayPool, deleteRelayPool, renameRelayPool, setRelayPoolPlacement } from '@/api/pools'
import type { CreateRelayPoolResponse, RelayPoolSummary } from '@/api/types'
import { RESIDENCY_NOTE } from '@/components/PoolBadges'
import type { RelayActionNotice } from '@/components/RelayDangerZone'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { MAX_POOL_NAME, MAX_REGION, poolEditFailure, regionProblem, type PoolEdit, type PoolFailure } from '@/lib/pool-errors'

// Relay-pool dialogs (D-37, D-41). Each sends exactly one request per
// confirm and reports the outcome; the page reloads its data afterwards.
// Which dialogs a role is offered mirrors decide(): create, rename and delete
// for both roles, region and residency for super-admin only. That is UX
// only; the controller authorizes every request.

/** Trimmed, 1–64 characters, no control characters (mirrors relay ValidPoolName). */
function nameProblem(name: string): string | null {
  const n = Array.from(name.trim()).length
  // eslint-disable-next-line no-control-regex
  if (n === 0 || n > MAX_POOL_NAME || /[\u0000-\u001f\u007f]/.test(name)) return 'Enter a name of 1 to 64 characters, without control characters.'
  return null
}

/** Runs one request at a time and maps its failure. */
function useOnce(edit: PoolEdit, poolName?: string) {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<PoolFailure>()
  const inFlight = useRef(false)
  async function run(fn: () => Promise<void>, onFailure: (f: PoolFailure) => void) {
    if (inFlight.current) return
    inFlight.current = true
    setPending(true)
    setError(undefined)
    try {
      await fn()
    } catch (err) {
      // An ended session is handled by the session store (sign-in page).
      if (err instanceof ApiError && err.sessionEnded) return
      const failure = poolEditFailure(err, edit, poolName)
      if (failure.refresh) onFailure(failure)
      else setError(failure)
    } finally {
      inFlight.current = false
      setPending(false)
    }
  }
  return { pending, error, run }
}

function PoolDialog({
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
  error?: PoolFailure
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
          {error && !error.field && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error.message}</AlertDescription>
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

function NameField({ value, onChange, error }: { value: string; onChange: (v: string) => void; error?: string }) {
  return (
    <div className="grid gap-2">
      <Label htmlFor="pool-name">Name</Label>
      <Input
        id="pool-name"
        autoComplete="off"
        maxLength={MAX_POOL_NAME * 2}
        value={value}
        aria-invalid={error !== undefined}
        aria-describedby={error ? 'pool-name-error' : undefined}
        onChange={(e) => onChange(e.target.value)}
      />
      {error && (
        <p id="pool-name-error" className="text-xs text-destructive">
          {error}
        </p>
      )}
    </div>
  )
}

/** Region and residency inputs (super-admin only). Residency needs a region (D-37). */
function PlacementFields({
  region,
  residency,
  onRegion,
  onResidency,
  serverError,
}: {
  region: string
  residency: boolean
  onRegion: (v: string) => void
  onResidency: (v: boolean) => void
  serverError?: string
}) {
  const problem = regionProblem(region.trim())
  const needsRegion = residency && region.trim() === ''
  const regionError = serverError ?? problem ?? (needsRegion ? 'Residency needs a region.' : undefined)
  return (
    <>
      <div className="grid gap-2">
        <Label htmlFor="pool-region">Region</Label>
        <Input
          id="pool-region"
          autoComplete="off"
          spellCheck={false}
          placeholder="eu-central-1"
          maxLength={MAX_REGION * 2}
          value={region}
          aria-invalid={regionError !== undefined}
          aria-describedby="pool-region-hint"
          onChange={(e) => onRegion(e.target.value)}
        />
        <p id="pool-region-hint" className={regionError ? 'text-xs text-destructive' : 'text-xs text-muted-foreground'}>
          {regionError ?? 'Optional. A placement and latency label, such as eu-central-1. Leave empty for no region.'}
        </p>
      </div>
      <div className="grid gap-2">
        <label className="flex items-start gap-2.5 text-sm">
          <input
            type="checkbox"
            className="mt-0.5 size-4 accent-[var(--color-primary)]"
            checked={residency}
            onChange={(e) => onResidency(e.target.checked)}
            aria-describedby="pool-residency-note"
          />
          <span>Residency pool</span>
        </label>
        <p id="pool-residency-note" className="text-xs text-muted-foreground">
          {RESIDENCY_NOTE}
        </p>
      </div>
    </>
  )
}

/** Valid placement input: a valid region (or none), and residency only with a region. */
function placementValid(region: string, residency: boolean): boolean {
  const r = region.trim()
  return regionProblem(r) === null && (!residency || r !== '')
}

export function CreatePoolDialog({
  canSetPlacement,
  onCancel,
  onCreated,
}: {
  canSetPlacement: boolean
  onCancel: () => void
  onCreated: (pool: CreateRelayPoolResponse) => void
}) {
  const [name, setName] = useState('')
  const [region, setRegion] = useState('')
  const [residency, setResidency] = useState(false)
  const { pending, error, run } = useOnce('create')
  const valid = nameProblem(name) === null && (!canSetPlacement || placementValid(region, residency))

  return (
    <PoolDialog
      title="Create pool"
      description={
        canSetPlacement
          ? 'A new, empty relay pool. Move relays into it, then assign tenants to it from their tenant page.'
          : 'A new, empty relay pool. Move relays into it; a super-admin assigns tenants to it and sets its region.'
      }
      error={error}
      pending={pending}
      valid={valid}
      submitLabel="Create pool"
      pendingLabel="Creating…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(
          async () => {
            const r = region.trim()
            const created = canSetPlacement ? await createRelayPool(name.trim(), r || null, residency) : await createRelayPool(name.trim())
            onCreated(created)
          },
          () => undefined,
        )
      }
    >
      <NameField value={name} onChange={setName} error={error?.field === 'name' ? error.message : undefined} />
      {canSetPlacement && (
        <PlacementFields
          region={region}
          residency={residency}
          onRegion={setRegion}
          onResidency={setResidency}
          serverError={error?.field === 'region' ? error.message : undefined}
        />
      )}
    </PoolDialog>
  )
}

export function RenamePoolDialog({
  pool,
  onCancel,
  onDone,
}: {
  pool: RelayPoolSummary
  onCancel: () => void
  onDone: (notice: RelayActionNotice) => void
}) {
  const [name, setName] = useState(pool.name)
  const { pending, error, run } = useOnce('rename', pool.name)
  const valid = nameProblem(name) === null && name.trim() !== pool.name

  return (
    <PoolDialog
      title="Rename pool"
      description={
        <>
          A new name for <strong className="text-foreground">{pool.name}</strong>. Its relays and tenants don’t change.
        </>
      }
      error={error}
      pending={pending}
      valid={valid}
      submitLabel="Rename pool"
      pendingLabel="Renaming…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(
          async () => {
            await renameRelayPool(pool.id, name.trim())
            onDone({ tone: 'success', text: `Pool renamed to ${name.trim()}.` })
          },
          (f) => onDone({ tone: 'info', text: f.message }),
        )
      }
    >
      <NameField value={name} onChange={setName} error={error?.field === 'name' ? error.message : undefined} />
    </PoolDialog>
  )
}

export function PlacementDialog({
  pool,
  onCancel,
  onDone,
}: {
  pool: RelayPoolSummary
  onCancel: () => void
  onDone: (notice: RelayActionNotice) => void
}) {
  const [region, setRegion] = useState(pool.region ?? '')
  const [residency, setResidency] = useState(pool.residency)
  const { pending, error, run } = useOnce('placement', pool.name)
  const changed = (region.trim() || null) !== pool.region || residency !== pool.residency
  const valid = placementValid(region, residency) && changed

  return (
    <PoolDialog
      title="Region and residency"
      description={
        <>
          For <strong className="text-foreground">{pool.name}</strong>. These can change only while no tenant uses the
          pool; otherwise create a new pool and move the tenants to it.
        </>
      }
      error={error}
      pending={pending}
      valid={valid}
      submitLabel="Save"
      pendingLabel="Saving…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(
          async () => {
            await setRelayPoolPlacement(pool.id, region.trim() || null, residency)
            onDone({ tone: 'success', text: `${pool.name}: region and residency saved.` })
          },
          (f) => onDone({ tone: 'info', text: f.message }),
        )
      }
    >
      <PlacementFields
        region={region}
        residency={residency}
        onRegion={setRegion}
        onResidency={setResidency}
        serverError={error?.field === 'region' ? error.message : undefined}
      />
    </PoolDialog>
  )
}

export function DeletePoolDialog({
  pool,
  onCancel,
  onDeleted,
  onRefresh,
}: {
  pool: RelayPoolSummary
  onCancel: () => void
  onDeleted: () => void
  onRefresh: (notice: RelayActionNotice) => void
}) {
  const { pending, error, run } = useOnce('delete', pool.name)
  const notEmpty = pool.relays > 0 || pool.tenants > 0

  return (
    <PoolDialog
      title="Delete pool"
      description={
        <>
          <strong className="text-foreground">{pool.name}</strong> is removed. Only an empty pool can be deleted: no
          relays and no tenants.
        </>
      }
      error={error}
      pending={pending}
      valid
      destructive
      submitLabel="Delete pool"
      pendingLabel="Deleting…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(
          async () => {
            await deleteRelayPool(pool.id)
            onDeleted()
          },
          (f) => onRefresh({ tone: 'info', text: f.message }),
        )
      }
    >
      {notEmpty && (
        <Alert role="note">
          <AlertDescription>
            As of this page load it has {pool.relays} relay{pool.relays === 1 ? '' : 's'} and {pool.tenants} tenant
            {pool.tenants === 1 ? '' : 's'}, so the controller will refuse. Move them to another pool first.
          </AlertDescription>
        </Alert>
      )}
    </PoolDialog>
  )
}
