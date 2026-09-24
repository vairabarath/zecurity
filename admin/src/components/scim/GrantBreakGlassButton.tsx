import { useState } from 'react'
import { useMutation } from '@apollo/client/react'
import { toast } from 'sonner'
import { GrantWorkspacePermissionDocument } from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { useAuthStore } from '@/store/auth'

// Extracts a displayable message from an Apollo error without inspecting
// extensions.code — mirrors the local helper this control was lifted out of
// (ScimConfigCard.tsx). The grant mutation is ADMIN-gated server-side; any
// refusal is "the server said no" and is shown verbatim.
function errorMessage(err: unknown): string {
  if (err && typeof err === 'object' && 'message' in err) {
    const msg = (err as { message?: unknown }).message
    if (typeof msg === 'string' && msg.trim()) return msg
  }
  return 'The server refused the request.'
}

// Shared "Grant myself identity.mapping.break_glass" control (PENDING-05 §5.4).
// Owns the grant mutation and the current-user lookup so the SCIM config card
// and the provisioning-conflict dialog share one implementation and one
// success/error contract.
//
// It deliberately does NOT refetch or otherwise mutate the caller's state:
// each caller supplies onGranted, so the config card can refetch while the
// conflict dialog does nothing (a refetch there would remount the row and
// destroy the typed reason — PENDING-05 §9). Possession is an explicit row,
// never implied by role; the grant is itself audited server-side (ADR-025 §3.2).
export function GrantBreakGlassButton({
  successMessage,
  label = 'Grant myself identity.mapping.break_glass',
  onGranted,
  variant = 'outline',
  size = 'sm',
  className,
}: {
  successMessage: string
  // Idle-state copy. Defaults to the self-grant phrasing used on the FORBIDDEN
  // conflict path; the SCIM config card keeps its own pre-extraction wording.
  label?: string
  onGranted?: () => void
  variant?: 'default' | 'outline' | 'destructive'
  size?: 'default' | 'sm' | 'lg' | 'icon'
  className?: string
}) {
  const currentUser = useAuthStore((s) => s.user)
  const [grantBreakGlass, { loading: granting }] = useMutation(GrantWorkspacePermissionDocument)
  const [granted, setGranted] = useState(false)

  async function handleGrant() {
    if (!currentUser?.id) {
      toast.error('No authenticated user — cannot grant permission.')
      return
    }
    try {
      await grantBreakGlass({
        variables: {
          userId: currentUser.id,
          permission: 'identity.mapping.break_glass',
        },
      })
      setGranted(true)
      toast.success(successMessage)
      onGranted?.()
    } catch (err) {
      toast.error(errorMessage(err))
    }
  }

  return (
    <Button
      type="button"
      variant={variant}
      size={size}
      className={className}
      onClick={() => void handleGrant()}
      disabled={granting || granted}
    >
      {granted
        ? 'Break-glass permission granted'
        : granting
          ? 'Granting…'
          : label}
    </Button>
  )
}
