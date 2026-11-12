import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { PoolSelect } from '@/components/PoolSelect'
import { checkDnsSan, checkIpSan, checkList, parseList } from '@/lib/relay-install'
import { usePools } from '@/lib/usePools'

export interface NewRelay {
  name: string
  dnsAllowlist: string[]
  ipAllowlist: string[]
  /** The chosen relay pool (D-37); undefined when the pools couldn't load (the controller uses the default pool). */
  poolId?: string
}

// Collects a new relay's name and SAN allowlists (Sprint 22 Phase E). The
// checks mirror the controller's so mistakes show before the call; the
// controller validates again. The page runs the API call and passes back
// `pending`/`error`. The pool picker (D-37) loads the pools when the dialog
// opens and preselects the default pool.
export function AddRelayDialog({
  open,
  pending,
  error,
  onSubmit,
  onCancel,
}: {
  open: boolean
  pending: boolean
  error?: string
  onSubmit: (relay: NewRelay) => void
  onCancel: () => void
}) {
  // The content mounts only while open, so the form starts empty each time.
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onCancel()}>
      <DialogContent>
        <AddRelayForm pending={pending} error={error} onSubmit={onSubmit} onCancel={onCancel} />
      </DialogContent>
    </Dialog>
  )
}

function AddRelayForm({
  pending,
  error,
  onSubmit,
  onCancel,
}: {
  pending: boolean
  error?: string
  onSubmit: (relay: NewRelay) => void
  onCancel: () => void
}) {
  const [name, setName] = useState('')
  const [dns, setDns] = useState('')
  const [ips, setIps] = useState('')
  const pools = usePools()
  const [poolChoice, setPoolChoice] = useState('')
  const defaultPoolId = pools.status === 'ready' ? (pools.items.find((p) => p.is_default)?.id ?? '') : ''
  const poolId = poolChoice || defaultPoolId

  const dnsList = parseList(dns)
  const ipList = parseList(ips)
  const dnsProblem = checkList(dnsList, checkDnsSan)
  const ipProblem = checkList(ipList, checkIpSan)
  const trimmed = name.trim()
  // While the pools load, wait (it is quick); if they can't load, the relay goes to the default pool.
  const canSubmit = trimmed !== '' && !dnsProblem && !ipProblem && !pending && pools.status !== 'loading'
  const close = onCancel

  function submit(e: FormEvent) {
    e.preventDefault()
    if (canSubmit) onSubmit({ name: trimmed, dnsAllowlist: dnsList, ipAllowlist: ipList, poolId: poolId || undefined })
  }

  return (
    <form onSubmit={submit} className="grid gap-4" noValidate>
      <DialogHeader>
        <DialogTitle>Add relay</DialogTitle>
        <DialogDescription>
          Registers a pending relay and issues a one-time provisioning token, valid for 24 hours, with the
          command to install it.
        </DialogDescription>
      </DialogHeader>
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <div className="grid gap-2">
        <Label htmlFor="relay-name">Name</Label>
        <Input
          id="relay-name"
          autoComplete="off"
          maxLength={128}
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="relay-dns">DNS names</Label>
        <Input
          id="relay-dns"
          autoComplete="off"
          placeholder="relay-1.example.com"
          aria-invalid={dnsProblem !== null}
          aria-describedby="relay-dns-hint"
          value={dns}
          onChange={(e) => setDns(e.target.value)}
        />
        <p id="relay-dns-hint" className={dnsProblem ? 'text-xs text-destructive' : 'text-xs text-muted-foreground'}>
          {dnsProblem ?? 'Separate with commas or spaces. Lowercase, no wildcards.'}
        </p>
      </div>
      <div className="grid gap-2">
        <Label htmlFor="relay-ips">IP addresses</Label>
        <Input
          id="relay-ips"
          autoComplete="off"
          placeholder="203.0.113.5"
          aria-invalid={ipProblem !== null}
          aria-describedby="relay-ips-hint"
          value={ips}
          onChange={(e) => setIps(e.target.value)}
        />
        <p id="relay-ips-hint" className={ipProblem ? 'text-xs text-destructive' : 'text-xs text-muted-foreground'}>
          {ipProblem ?? 'Separate with commas or spaces.'}
        </p>
      </div>
      <p className="text-xs text-muted-foreground">
        Optional. The relay’s certificate can name only these DNS names and IP addresses; the install command
        requests all of them.
      </p>
      <div className="grid gap-2">
        <Label htmlFor="relay-pool">Relay pool</Label>
        {pools.status === 'ready' ? (
          <PoolSelect id="relay-pool" label="Relay pool" pools={pools.items} value={poolId} onChange={setPoolChoice} />
        ) : pools.status === 'loading' ? (
          <p className="text-xs text-muted-foreground" aria-busy="true">
            Loading pools…
          </p>
        ) : (
          <p className="text-xs text-muted-foreground" role="note">
            Couldn’t load the relay pools, so the relay goes to the default pool. You can move it later.
          </p>
        )}
        {pools.status === 'ready' && (
          <p className="text-xs text-muted-foreground">Only tenants that use this pool are offered the relay.</p>
        )}
      </div>
      <DialogFooter>
        <Button type="button" variant="ghost" onClick={close}>
          Cancel
        </Button>
        <Button type="submit" disabled={!canSubmit}>
          {pending ? 'Creating…' : 'Create relay'}
        </Button>
      </DialogFooter>
    </form>
  )
}
