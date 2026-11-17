import { useMutation } from '@apollo/client/react'
import { toast } from 'sonner'
import { GetUsersDocument, ResetUserMfaDocument } from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { toastMutationError } from '@/lib/mutationErrors'

export type ResetMfaTarget = { id: string; email: string }

// Reset MFA (PENDING-06 M-8): deletes another user's authenticator. Their
// sessions and devices lose the MFA mark at once, so every resource policy
// that requires MFA asks them again, and they set up a new authenticator at
// that prompt. The server refuses it for your own account.
export function ResetMfaDialog({ target, onClose }: { target: ResetMfaTarget | null; onClose: () => void }) {
  const [resetMfa, { loading }] = useMutation(ResetUserMfaDocument, {
    refetchQueries: [{ query: GetUsersDocument }],
  })

  async function confirm() {
    if (!target) return
    try {
      await resetMfa({ variables: { userId: target.id } })
      toast.success(`MFA reset for ${target.email}`)
      onClose()
    } catch (err) {
      toastMutationError(err)
    }
  }

  return (
    <Dialog open={!!target} onOpenChange={(open) => { if (!open && !loading) onClose() }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Reset MFA</DialogTitle>
          <DialogDescription>
            Remove the authenticator app of{' '}
            <span className="font-semibold text-foreground">{target?.email}</span>?
          </DialogDescription>
        </DialogHeader>
        <ul className="list-disc space-y-1 pl-5 text-sm text-muted-foreground">
          <li>Their sessions and devices lose their MFA verification now.</li>
          <li>Resources that require MFA close for them until they verify again.</li>
          <li>They set up a new authenticator at their next MFA prompt.</li>
        </ul>
        <p className="text-xs text-muted-foreground">Use this when someone lost their phone. The reset is audited.</p>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={loading}>Cancel</Button>
          <Button variant="destructive" onClick={() => void confirm()} disabled={loading}>
            {loading ? 'Resetting…' : 'Reset MFA'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
