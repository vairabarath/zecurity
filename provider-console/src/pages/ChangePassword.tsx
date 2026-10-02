import { useState, type FormEvent } from 'react'
import { Link, Navigate, useNavigate } from 'react-router-dom'
import { ApiError } from '@/api/client'
import { ErrorCode } from '@/api/types'
import { changePassword, signOut } from '@/auth/flows'
import { useSessionStore } from '@/auth/session'
import { AuthCard } from '@/components/AuthCard'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { tryAgainIn } from '@/lib/format'
import { passwordPolicyProblems } from '@/lib/password-policy'

function changeErrorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Could not change the password. Try again.'
  switch (err.code) {
    case ErrorCode.invalidCredentials:
      return 'Current password is incorrect.'
    case ErrorCode.passwordPolicy:
      return err.detail ? `New password rejected: ${err.detail.replace(/^password policy:\s*/i, '')}.` : 'New password rejected by the password policy.'
    case ErrorCode.tooManyAttempts:
      return `Too many attempts. ${tryAgainIn(err.retryAfterSeconds)}`
    case ErrorCode.loginUnavailable:
      return 'Password change is unavailable right now. Try again shortly.'
    case ErrorCode.networkError:
      return 'Can’t reach the controller. Check your connection.'
    default:
      return 'Could not change the password. Try again.'
  }
}

// Both the forced first change (password-change-only token) and a voluntary
// change from the account menu.
//
// This page does its own session check instead of sitting under RequireToken:
// a successful change briefly puts the session in "loading" while
// /provider/me reloads, and RequireToken would unmount the page mid-submit
// and lose the redirect to Home.
export function ChangePassword() {
  const status = useSessionStore((s) => s.status)
  const pwcOnly = useSessionStore((s) => s.pwcOnly)
  const email = useSessionStore((s) => s.me?.email)
  const navigate = useNavigate()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState<string>()
  const [submitting, setSubmitting] = useState(false)

  if (status === 'anonymous') return <Navigate to="/login" replace />
  if (status === 'loading' && !submitting) {
    return (
      <div className="flex min-h-screen items-center justify-center p-6" aria-busy="true" aria-label="Loading session">
        <Skeleton className="h-24 w-full max-w-sm" />
      </div>
    )
  }

  const hints = next === '' ? [] : passwordPolicyProblems(next, { email, currentPassword: current })
  const mismatch = confirm !== '' && confirm !== next
  const canSubmit = !submitting && current !== '' && next !== '' && confirm === next && hints.length === 0

  async function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (!canSubmit) return
    const [cur, nxt] = [current, next]
    // Passwords live only in this form state; drop them right away.
    setCurrent('')
    setNext('')
    setConfirm('')
    setError(undefined)
    setSubmitting(true)
    try {
      await changePassword(cur, nxt)
      navigate('/', { replace: true })
    } catch (err) {
      // The password changed but /provider/me failed for a non-session
      // reason: fail closed rather than keep an unverified session.
      if (useSessionStore.getState().status === 'loading') {
        useSessionStore.getState().endSession('unverified')
      }
      // A session-ending response already sent the store to anonymous and
      // this page redirects to Login on the next render.
      if (!(err instanceof ApiError && err.sessionEnded)) setError(changeErrorMessage(err))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <AuthCard
      title={pwcOnly ? 'Set a new password' : 'Change password'}
      description={
        pwcOnly
          ? 'You must choose a new password before you can use the Provider Console.'
          : 'Other sessions for this account will be signed out.'
      }
    >
      <form method="post" onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
        {error && (
          <Alert variant="destructive">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        {/* Lets password managers pair the new password with the account. */}
        {email && <input type="hidden" name="username" autoComplete="username" value={email} readOnly />}
        <div className="flex flex-col gap-2">
          <Label htmlFor="current-password">{pwcOnly ? 'Temporary or current password' : 'Current password'}</Label>
          <Input
            id="current-password"
            type="password"
            autoComplete="current-password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
          />
        </div>
        <div className="flex flex-col gap-2">
          <Label htmlFor="new-password">New password</Label>
          <Input
            id="new-password"
            type="password"
            autoComplete="new-password"
            aria-describedby="new-password-hints"
            value={next}
            onChange={(e) => setNext(e.target.value)}
          />
          <ul id="new-password-hints" className="text-xs text-muted-foreground">
            {hints.length === 0 ? (
              <li>12–128 characters, not your email address.</li>
            ) : (
              hints.map((h) => (
                <li key={h} className="text-warning">
                  {h}
                </li>
              ))
            )}
          </ul>
        </div>
        <div className="flex flex-col gap-2">
          <Label htmlFor="confirm-password">Confirm new password</Label>
          <Input
            id="confirm-password"
            type="password"
            autoComplete="new-password"
            aria-invalid={mismatch || undefined}
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
          />
          {mismatch && <p className="text-xs text-warning">Passwords don’t match.</p>}
        </div>
        <Button type="submit" disabled={!canSubmit}>
          {submitting ? 'Saving…' : 'Save new password'}
        </Button>
        {pwcOnly ? (
          <Button type="button" variant="ghost" onClick={() => void signOut()}>
            Sign out
          </Button>
        ) : (
          <Button type="button" variant="ghost" asChild>
            <Link to="/">Cancel</Link>
          </Button>
        )}
      </form>
    </AuthCard>
  )
}
