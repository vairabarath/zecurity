import { useState } from 'react'
import { useMutation } from '@apollo/client/react'
import { GetUsersDocument, Role, SetMemberRoleDocument } from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { MAX_ROLE_REASON, ROLE_INFO, ROLE_ORDER } from '@/lib/capabilities'
import { cn } from '@/lib/utils'

export type ManageRoleTarget = { id: string; email: string; role: string }

// ManageRoleDialog — PENDING-19 §7. One role per person (R-1), set by hand
// (R-3, also for directory-managed users: the role is Zecurity-owned), with a
// required reason that goes to the audit log (R-6). The server refuses your
// own account and removing the last active Admin; its message is shown as-is.
export function ManageRoleDialog({
  target,
  onClose,
}: {
  target: ManageRoleTarget | null
  onClose: () => void
}) {
  return (
    <Dialog open={target !== null} onOpenChange={(o) => { if (!o) onClose() }}>
      <DialogContent className="max-w-lg">
        {/* Keyed so each opening starts from the user's current role. */}
        {target ? <ManageRoleForm key={target.id} target={target} onClose={onClose} /> : null}
      </DialogContent>
    </Dialog>
  )
}

function ManageRoleForm({ target, onClose }: { target: ManageRoleTarget; onClose: () => void }) {
  const current = target.role.toUpperCase() as Role
  const [role, setRole] = useState<Role>(current)
  const [reason, setReason] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [setMemberRole, { loading }] = useMutation(SetMemberRoleDocument, {
    refetchQueries: [{ query: GetUsersDocument }],
    awaitRefetchQueries: true,
  })

  const trimmed = reason.trim()
  const tooLong = trimmed.length > MAX_ROLE_REASON
  const unchanged = role === current
  const canSubmit = !loading && !unchanged && trimmed.length > 0 && !tooLong

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!canSubmit) return
    setError(null)
    try {
      await setMemberRole({ variables: { userId: target.id, role, reason: trimmed } })
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not change the role.')
    }
  }

  return (
    <form onSubmit={handleSubmit} className="space-y-4">
      <DialogHeader>
        <DialogTitle>Manage role</DialogTitle>
        <DialogDescription>
          Choose the console role for <span className="font-semibold text-foreground">{target.email}</span>.
          It applies on their next request; their Zecurity client keeps working.
        </DialogDescription>
      </DialogHeader>

      <fieldset className="space-y-2" aria-label="Role">
        {ROLE_ORDER.map((r) => {
          const info = ROLE_INFO[r]
          const id = `role-${r}`
          return (
            <label
              key={r}
              htmlFor={id}
              className={cn(
                'flex cursor-pointer items-start gap-3 rounded-xl border px-3 py-2.5 transition',
                role === r ? 'border-primary bg-primary/8' : 'border-border hover:bg-secondary',
              )}
            >
              <input
                id={id}
                type="radio"
                name="role"
                value={r}
                checked={role === r}
                onChange={() => setRole(r)}
                className="mt-1 accent-[var(--primary)]"
              />
              <span className="min-w-0">
                <span className="block text-sm font-semibold">
                  {info.label}
                  {r === current ? <span className="ml-2 text-xs font-normal text-muted-foreground">current</span> : null}
                </span>
                <span className="block text-xs text-muted-foreground">{info.description}</span>
              </span>
            </label>
          )
        })}
      </fieldset>

      <div className="space-y-1.5">
        <Label htmlFor="role-reason">Reason (required, kept in the audit log)</Label>
        <textarea
          id="role-reason"
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          rows={2}
          className="flex w-full rounded-lg border border-border bg-secondary px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-primary/30"
          placeholder="e.g. joined the network team"
        />
        {tooLong ? (
          <p className="text-xs text-destructive">At most {MAX_ROLE_REASON} characters.</p>
        ) : null}
      </div>

      {role === Role.Member && !unchanged ? (
        <p className="text-xs text-muted-foreground">
          Member has no console access: {target.email} will lose the console on their next request.
        </p>
      ) : null}

      {error ? (
        <p role="alert" className="text-sm text-destructive">{error}</p>
      ) : null}

      <DialogFooter>
        <Button type="button" variant="outline" onClick={onClose} disabled={loading}>
          Cancel
        </Button>
        <Button type="submit" disabled={!canSubmit}>
          {loading ? 'Saving…' : 'Save role'}
        </Button>
      </DialogFooter>
    </form>
  )
}
