import { useState, type FormEvent } from 'react'
import { ApiError } from '@/api/client'
import { ErrorCode } from '@/api/types'
import { signIn } from '@/auth/flows'
import { useSessionStore, type EndReason } from '@/auth/session'
import { AuthCard } from '@/components/AuthCard'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { tryAgainIn } from '@/lib/format'

const END_NOTICES: Record<EndReason, string> = {
  logged_out: 'You have signed out.',
  expired: 'Your session expired. Sign in again.',
  session_ended: 'Your session has ended. Sign in again.',
  disabled: 'This account is disabled. Contact a super-admin.',
  unverified: 'Your session could not be verified. Sign in again.',
}

// Never says whether the account exists: every credential failure gets the
// same message, as the server intends.
function loginErrorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Sign-in failed. Try again.'
  switch (err.code) {
    case ErrorCode.invalidCredentials:
      return 'Email or password is incorrect.'
    case ErrorCode.tooManyAttempts:
      return `Too many attempts. ${tryAgainIn(err.retryAfterSeconds)}`
    case ErrorCode.loginUnavailable:
      return 'Sign-in is unavailable right now. Try again shortly.'
    case ErrorCode.invalidRequest:
      return 'Enter your email and password.'
    case ErrorCode.networkError:
      return 'Can’t reach the controller. Check your connection.'
    default:
      return 'Sign-in failed. Try again.'
  }
}

// Wrapped in RedirectIfAuthenticated: once signIn moves the session on, the
// guard takes the user to Change password or back where they were heading.
export function Login() {
  const endReason = useSessionStore((s) => s.endReason)
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string>()
  const [submitting, setSubmitting] = useState(false)

  async function onSubmit(e: FormEvent) {
    e.preventDefault()
    const submitted = password
    // The password lives only in this form state; drop it right away.
    setPassword('')
    setError(undefined)
    useSessionStore.getState().clearEndReason()
    setSubmitting(true)
    try {
      await signIn(email.trim(), submitted)
    } catch (err) {
      // Login succeeded but /provider/me failed for a non-session reason:
      // don't leave a half-open session behind (fail closed).
      if (useSessionStore.getState().status === 'loading') {
        useSessionStore.getState().endSession('unverified')
      }
      if (!(err instanceof ApiError && err.sessionEnded)) setError(loginErrorMessage(err))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <AuthCard title="Sign in" description="Provider operators only.">
      {/* method=post keeps credentials out of the URL even if scripts fail. */}
      <form method="post" onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
        {endReason && !error && (
          <Alert>
            <AlertDescription>{END_NOTICES[endReason]}</AlertDescription>
          </Alert>
        )}
        {error && (
          <Alert variant="destructive">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        <div className="flex flex-col gap-2">
          <Label htmlFor="email">Email</Label>
          <Input
            id="email"
            name="email"
            type="email"
            autoComplete="username"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
        </div>
        <div className="flex flex-col gap-2">
          <Label htmlFor="password">Password</Label>
          <Input
            id="password"
            name="password"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </div>
        <Button type="submit" disabled={submitting || email.trim() === '' || password === ''}>
          {submitting ? 'Signing in…' : 'Sign in'}
        </Button>
      </form>
    </AuthCard>
  )
}
