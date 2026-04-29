import { useEffect, useRef, useState } from 'react'
import { useParams } from 'react-router-dom'
import { useMutation } from '@apollo/client/react'
import { InitiateAuthDocument } from '@/generated/graphql'

interface InvitationInfo {
  id: string
  email: string
  status: string
  workspace_name: string
  expires_at: string
}

export default function InviteAccept() {
  const { token } = useParams<{ token: string }>()
  const [invitation, setInvitation] = useState<InvitationInfo | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const fetched = useRef(false)

  const [initiateAuth] = useMutation(InitiateAuthDocument)

  useEffect(() => {
    if (fetched.current || !token) return
    fetched.current = true

    fetch(`/api/invitations/${token}`)
      .then(r => r.ok ? r.json() : Promise.reject(r.status))
      .then((data: InvitationInfo) => setInvitation(data))
      .catch(() => setError('Invitation not found or has expired.'))
  }, [token])

  async function handleSignIn() {
    if (!invitation || !token) return
    setLoading(true)
    try {
      sessionStorage.setItem('ztna_invite_token', token)
      const result = await initiateAuth({
        variables: { provider: 'google', workspaceName: invitation.workspace_name },
      })
      const { redirectUrl, state } = result.data!.initiateAuth
      sessionStorage.setItem('ztna_oauth_state', state)
      window.location.href = redirectUrl
    } catch {
      sessionStorage.removeItem('ztna_invite_token')
      setError('Failed to start sign-in. Please try again.')
      setLoading(false)
    }
  }

  if (error) {
    return (
      <div className="flex min-h-screen flex-col items-center justify-center gap-4 px-4">
        <div className="text-4xl">🔒</div>
        <h1 className="text-xl font-semibold">Invalid Invitation</h1>
        <p className="text-sm text-muted-foreground">{error}</p>
      </div>
    )
  }

  if (!invitation) {
    return (
      <div className="flex min-h-screen items-center justify-center">
        <p className="text-sm text-muted-foreground">Loading invitation...</p>
      </div>
    )
  }

  return (
    <div className="flex min-h-screen flex-col items-center justify-center gap-6 px-4">
      <div className="grid h-14 w-14 place-items-center rounded-2xl bg-[linear-gradient(135deg,oklch(0.86_0.095_175)_0%,oklch(0.70_0.10_175)_100%)] text-[oklch(0.22_0.02_200)] shadow-[0_6px_16px_oklch(0.86_0.095_175/0.22)]">
        <span className="text-2xl font-bold">Z</span>
      </div>

      <div className="text-center">
        <h1 className="text-2xl font-semibold tracking-tight">You've been invited</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Join <span className="font-medium text-foreground">{invitation.workspace_name}</span> on Zecurity
        </p>
      </div>

      <div className="w-full max-w-sm rounded-2xl border border-border bg-card p-5 text-sm">
        <div className="flex items-center justify-between py-1.5">
          <span className="text-muted-foreground">Invited email</span>
          <span className="font-medium">{invitation.email}</span>
        </div>
        <div className="flex items-center justify-between py-1.5">
          <span className="text-muted-foreground">Workspace</span>
          <span className="font-medium">{invitation.workspace_name}</span>
        </div>
        <div className="flex items-center justify-between py-1.5">
          <span className="text-muted-foreground">Expires</span>
          <span className="font-medium">{new Date(invitation.expires_at).toLocaleDateString()}</span>
        </div>
      </div>

      <button
        onClick={handleSignIn}
        disabled={loading}
        className="flex w-full max-w-sm items-center justify-center gap-2 rounded-xl bg-primary px-6 py-3 text-sm font-semibold text-primary-foreground shadow transition hover:opacity-90 disabled:opacity-50"
      >
        {loading ? 'Redirecting...' : 'Sign in with Google to accept'}
      </button>

      <p className="text-xs text-muted-foreground">
        You must sign in with the Google account associated with{' '}
        <span className="font-medium">{invitation.email}</span>
      </p>
    </div>
  )
}
