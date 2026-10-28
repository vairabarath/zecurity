import { useState, type FormEvent } from 'react'
import { Clock, ScrollText, ShieldCheck } from 'lucide-react'
import { ApiError } from '@/api/client'
import { ErrorCode } from '@/api/types'
import { signIn } from '@/auth/flows'
import { useSessionStore, type EndReason } from '@/auth/session'
import { ProviderBrand } from '@/components/ProviderBrand'
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
  mfa_required: 'Sign in again and confirm with your authenticator app.',
}

// What the controller enforces, stated plainly for the sign-in page.
const ASSURANCES = [
  { icon: ShieldCheck, title: 'Rate-limited, non-revealing sign-in', body: 'Wrong guesses are slowed down and never reveal which accounts exist.' },
  { icon: Clock, title: '15-minute sessions that end on change', body: 'Sessions aren’t extended silently. A role change, disable or password reset signs the operator out at once.' },
  { icon: ScrollText, title: 'Every tenant view is audited', body: 'Opening a tenant’s detail writes a tenant.read entry to the provider audit log.' },
]

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
    <main className="flex min-h-screen flex-wrap">
      <section
        aria-label="About the Provider Console"
        className="flex flex-[1_1_520px] flex-col justify-between gap-12 border-[#1a202b] bg-sidebar px-6 py-10 sm:px-14 sm:py-12 lg:border-r"
      >
        <ProviderBrand size="lg" />
        <div className="flex max-w-[520px] flex-col gap-7">
          <p className="font-display text-[34px] font-semibold leading-[1.08] tracking-[-0.03em] sm:text-[44px]">
            Run the platform that keeps every tenant connected.
          </p>
          <p className="text-[17px] leading-relaxed text-subtle-foreground">
            Watch the relay fleet, every tenant’s footprint and the certificates that hold it together, from one place.
          </p>
          <ul className="flex flex-col gap-4">
            {ASSURANCES.map(({ icon: Icon, title, body }) => (
              <li key={title} className="flex items-start gap-3.5">
                <span className="inline-flex size-[34px] shrink-0 items-center justify-center rounded-[10px] bg-primary-soft text-[#c2b8ff]">
                  <Icon className="size-[18px]" strokeWidth={1.8} aria-hidden />
                </span>
                <span className="flex flex-col gap-0.5">
                  <span className="font-medium">{title}</span>
                  <span className="text-sm text-muted-foreground">{body}</span>
                </span>
              </li>
            ))}
          </ul>
        </div>
        <p className="text-[13px] text-faint">Relay changes are limited to relay operators and super-admins; operator management to super-admins.</p>
      </section>

      <section aria-label="Sign in form" className="flex flex-[1_1_440px] items-center justify-center px-6 py-12">
        {/* method=post keeps credentials out of the URL even if scripts fail. */}
        <form method="post" onSubmit={onSubmit} className="flex w-full max-w-[400px] flex-col gap-5" noValidate>
          <div className="flex flex-col gap-2">
            <h1 className="font-display text-[26px] font-semibold tracking-[-0.02em]">Sign in</h1>
            <p className="text-[15px] text-subtle-foreground">Use your provider operator account.</p>
          </div>
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
            <div className="flex items-baseline justify-between gap-3">
              <Label htmlFor="password">Password</Label>
              <span className="text-[13px] text-muted-foreground">Forgot it? Ask a super-admin.</span>
            </div>
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
          <Button type="submit" className="h-11 text-[15px]" disabled={submitting || email.trim() === '' || password === ''}>
            {submitting ? 'Signing in…' : 'Sign in'}
          </Button>
          <p className="text-center text-[13px] text-faint">Sign-in pauses for 15 minutes after 5 failed attempts on an account, or 20 from one address.</p>
        </form>
      </section>
    </main>
  )
}
