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
import { checkDnsSan, checkIpSan, checkList, parseList } from '@/lib/relay-install'

export interface NewRelay {
  name: string
  dnsAllowlist: string[]
  ipAllowlist: string[]
}

// Collects a new relay's name and SAN allowlists (Sprint 22 Phase E). The
// checks mirror the controller's so mistakes show before the call; the
// controller validates again. The page runs the API call and passes back
// `pending`/`error`.
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
  const [name, setName] = useState('')
  const [dns, setDns] = useState('')
  const [ips, setIps] = useState('')

  const dnsList = parseList(dns)
  const ipList = parseList(ips)
  const dnsProblem = checkList(dnsList, checkDnsSan)
  const ipProblem = checkList(ipList, checkIpSan)
  const trimmed = name.trim()
  const canSubmit = trimmed !== '' && !dnsProblem && !ipProblem && !pending

  function close() {
    setName('')
    setDns('')
    setIps('')
    onCancel()
  }

  function submit(e: FormEvent) {
    e.preventDefault()
    if (canSubmit) onSubmit({ name: trimmed, dnsAllowlist: dnsList, ipAllowlist: ipList })
  }

  return (
    <Dialog open={open} onOpenChange={(o) => !o && close()}>
      <DialogContent>
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
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={close}>
              Cancel
            </Button>
            <Button type="submit" disabled={!canSubmit}>
              {pending ? 'Creating…' : 'Create relay'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
