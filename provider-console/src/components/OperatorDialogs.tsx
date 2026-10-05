import { useState, type FormEvent, type ReactNode } from 'react'
import type { OperatorView } from '@/api/types'
import { PROVIDER_ROLES, ROLE_LABELS, isProviderRole, type ProviderRole } from '@/auth/roles'
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
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'

// Dialogs for the Provider users page. Each one only collects input and
// confirms; the page runs the API call and passes back `pending`/`error`.

function ErrorAlert({ error }: { error?: string }) {
  if (!error) return null
  return (
    <Alert variant="destructive">
      <AlertDescription>{error}</AlertDescription>
    </Alert>
  )
}

function RoleSelect({
  id,
  label,
  value,
  onChange,
}: {
  id: string
  label: string
  value: ProviderRole
  onChange: (r: ProviderRole) => void
}) {
  return (
    <Select value={value} onValueChange={(v) => isProviderRole(v) && onChange(v)}>
      <SelectTrigger id={id} aria-label={label}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {PROVIDER_ROLES.map((r) => (
          <SelectItem key={r} value={r}>
            {ROLE_LABELS[r]}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

export function AddOperatorDialog({
  open,
  pending,
  error,
  onSubmit,
  onCancel,
}: {
  open: boolean
  pending: boolean
  error?: string
  onSubmit: (email: string, role: ProviderRole) => void
  onCancel: () => void
}) {
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<ProviderRole>('relay-ops')

  function close() {
    setEmail('')
    setRole('relay-ops')
    onCancel()
  }

  function submit(e: FormEvent) {
    e.preventDefault()
    if (email.trim() !== '' && !pending) onSubmit(email.trim(), role)
  }

  return (
    <Dialog open={open} onOpenChange={(o) => !o && close()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>Add operator</DialogTitle>
            <DialogDescription>
              They get a temporary password, shown once, and must change it at first sign-in.
            </DialogDescription>
          </DialogHeader>
          <ErrorAlert error={error} />
          <div className="grid gap-2">
            <Label htmlFor="operator-email">Email</Label>
            <Input
              id="operator-email"
              type="email"
              autoComplete="off"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="operator-role">Role</Label>
            <RoleSelect id="operator-role" label="Role" value={role} onChange={setRole} />
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={close}>
              Cancel
            </Button>
            <Button type="submit" disabled={pending || email.trim() === ''}>
              {pending ? 'Adding…' : 'Add operator'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

export function ChangeRoleDialog({
  operator,
  pending,
  error,
  onConfirm,
  onCancel,
}: {
  operator: OperatorView
  pending: boolean
  error?: string
  onConfirm: (role: ProviderRole) => void
  onCancel: () => void
}) {
  const current = operator.role
  const [role, setRole] = useState<ProviderRole>(isProviderRole(current) ? current : 'relay-ops')
  const unchanged = role === current

  return (
    <Dialog open onOpenChange={(o) => !o && onCancel()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Change role</DialogTitle>
          <DialogDescription>
            <span className="font-medium text-foreground">{operator.email}</span> is signed out everywhere when the
            role changes.
          </DialogDescription>
        </DialogHeader>
        <ErrorAlert error={error} />
        <dl className="grid grid-cols-[max-content_1fr] items-center gap-x-4 gap-y-3 text-sm">
          <dt className="text-muted-foreground">Current role</dt>
          <dd>{ROLE_LABELS[current] ?? current}</dd>
          <dt className="text-muted-foreground">
            <Label htmlFor="new-role">New role</Label>
          </dt>
          <dd>
            <RoleSelect id="new-role" label="New role" value={role} onChange={setRole} />
          </dd>
        </dl>
        {unchanged && <p className="text-xs text-muted-foreground">Choose a different role to change it.</p>}
        <DialogFooter>
          <Button type="button" variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
          <Button type="button" disabled={pending || unchanged} onClick={() => onConfirm(role)}>
            {pending
              ? 'Saving…'
              : unchanged
                ? 'Change role'
                : `Change to ${ROLE_LABELS[role]}`}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export function ConfirmDialog({
  title,
  description,
  confirmLabel,
  pendingLabel,
  destructive,
  pending,
  error,
  cancelLabel = 'Cancel',
  onConfirm,
  onCancel,
}: {
  title: string
  description: ReactNode
  confirmLabel: string
  pendingLabel: string
  destructive?: boolean
  pending: boolean
  error?: string
  cancelLabel?: string
  onConfirm: () => void
  onCancel: () => void
}) {
  return (
    <Dialog open onOpenChange={(o) => !o && onCancel()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{description}</DialogDescription>
        </DialogHeader>
        <ErrorAlert error={error} />
        <DialogFooter>
          <Button type="button" variant="ghost" onClick={onCancel}>
            {cancelLabel}
          </Button>
          <Button
            type="button"
            variant={destructive ? 'destructive' : 'default'}
            disabled={pending}
            onClick={onConfirm}
          >
            {pending ? pendingLabel : confirmLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
