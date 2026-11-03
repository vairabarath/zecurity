import { useEffect, useRef, useState, type FormEvent } from 'react'
import { QRCodeSVG } from 'qrcode.react'
import { Navigate, useNavigate } from 'react-router-dom'
import { ApiError } from '@/api/client'
import { ErrorCode, type TOTPEnrollResponse } from '@/api/types'
import { confirmTotpEnrollment, signOut, startTotpEnrollment, verifyTotp } from '@/auth/flows'
import { useSessionStore } from '@/auth/session'
import { AuthCard } from '@/components/AuthCard'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { tryAgainIn } from '@/lib/format'

// The TOTP step (Sprint 22 Phase T; D-28). Reached only with an MFA-stage
// token: "verify" (enter a code) or "enroll" (a super-admin sets TOTP up).
// The setup secret lives in this component's state only: never in
// sessionStorage, the URL or a log, and it's gone once the page unmounts.

function codeErrorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Could not check the code. Try again.'
  switch (err.code) {
    case ErrorCode.invalidCode:
      return 'That code isn’t right. Enter the current code from your authenticator app.'
    case ErrorCode.tooManyAttempts:
      return `Too many attempts. ${tryAgainIn(err.retryAfterSeconds)}`
    case ErrorCode.loginUnavailable:
      return 'Verification is unavailable right now. Try again shortly.'
    case ErrorCode.networkError:
      return 'Can’t reach the controller. Check your connection.'
    default:
      return 'Could not check the code. Try again.'
  }
}

/** "JBSWY3DP…" → "JBSW Y3DP …" for reading and typing. */
function groupKey(secret: string): string {
  return secret.replace(/(.{4})/g, '$1 ').trim()
}

export function Mfa() {
  const status = useSessionStore((s) => s.status)
  const stage = useSessionStore((s) => s.mfaStage)
  if (status === 'anonymous') return <Navigate to="/login" replace />
  if (status === 'password_change') return <Navigate to="/change-password" replace />
  if (status === 'authenticated') return <Navigate to="/" replace />
  if (status === 'loading' || !stage) {
    return (
      <div className="flex min-h-screen items-center justify-center p-6" aria-busy="true" aria-label="Loading session">
        <Skeleton className="h-24 w-full max-w-sm" />
      </div>
    )
  }
  return stage === 'enroll' ? <EnrollStep /> : <VerifyStep />
}

function CodeForm({ submitLabel, onSubmit }: { submitLabel: string; onSubmit: (code: string) => Promise<void> }) {
  const navigate = useNavigate()
  const [code, setCode] = useState('')
  const [error, setError] = useState<string>()
  const [submitting, setSubmitting] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (code.length !== 6 || submitting) return
    const entered = code
    setCode('')
    setError(undefined)
    setSubmitting(true)
    try {
      await onSubmit(entered)
      navigate('/', { replace: true })
    } catch (err) {
      if (useSessionStore.getState().status === 'loading') useSessionStore.getState().endSession('unverified')
      // A session-ending answer already moved the store; this page redirects.
      if (!(err instanceof ApiError && err.sessionEnded)) setError(codeErrorMessage(err))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-4" noValidate>
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <div className="flex flex-col gap-2">
        <Label htmlFor="totp-code">Authentication code</Label>
        <Input
          id="totp-code"
          name="totp-code"
          inputMode="numeric"
          autoComplete="one-time-code"
          maxLength={6}
          autoFocus
          className="h-11 text-center font-mono text-lg tracking-[0.4em]"
          value={code}
          onChange={(e) => setCode(e.target.value.replace(/\D/g, '').slice(0, 6))}
        />
      </div>
      <Button type="submit" className="h-11" disabled={submitting || code.length !== 6}>
        {submitting ? 'Checking…' : submitLabel}
      </Button>
      <button type="button" className="text-sm text-muted-foreground hover:text-foreground" onClick={() => void signOut()}>
        Use a different account
      </button>
    </form>
  )
}

function VerifyStep() {
  return (
    <AuthCard title="Enter your authentication code" description="Open your authenticator app and enter the 6-digit code for Zecurity Provider.">
      <CodeForm submitLabel="Verify" onSubmit={verifyTotp} />
    </AuthCard>
  )
}

function EnrollStep() {
  const [setup, setSetup] = useState<TOTPEnrollResponse>()
  const [error, setError] = useState<string>()
  // One enrolment request per visit: a second would replace the secret the
  // first one showed (StrictMode runs effects twice).
  const started = useRef(false)

  useEffect(() => {
    if (started.current) return
    started.current = true
    startTotpEnrollment().then(setSetup, (err: unknown) => {
      if (!(err instanceof ApiError && err.sessionEnded)) setError('Could not start setup. Sign in again to retry.')
    })
  }, [])

  return (
    <AuthCard
      title="Set up two-factor authentication"
      description="Super-admins must confirm each sign-in with an authenticator app (Google Authenticator, Aegis, 1Password and similar)."
    >
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      {!setup && !error && <Skeleton className="h-56 w-full" aria-label="Preparing setup" />}
      {setup && (
        <div className="flex flex-col gap-5">
          <ol className="flex list-decimal flex-col gap-1.5 pl-5 text-sm text-subtle-foreground">
            <li>Scan the QR code with your authenticator app, or enter the setup key.</li>
            <li>Enter the 6-digit code it shows to confirm.</li>
          </ol>
          <div className="flex justify-center rounded-xl bg-white p-4">
            <QRCodeSVG value={setup.otpauth_uri} size={176} role="img" aria-label="QR code for your authenticator app" />
          </div>
          <div className="flex flex-col gap-1.5">
            <span className="text-xs font-medium text-muted-foreground">Setup key</span>
            <code aria-label="Setup key" className="select-all break-all rounded-lg border border-input bg-field px-3 py-2 font-mono text-sm tracking-wide">
              {groupKey(setup.secret)}
            </code>
          </div>
          <CodeForm submitLabel="Confirm and continue" onSubmit={confirmTotpEnrollment} />
        </div>
      )}
    </AuthCard>
  )
}
