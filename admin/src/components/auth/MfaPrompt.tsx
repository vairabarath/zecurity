import { useEffect, useRef, useState, type FormEvent } from 'react'
import { QRCodeSVG } from 'qrcode.react'
import { KeyRound, Loader2 } from 'lucide-react'
import { apolloClient } from '@/apollo/client'
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
import {
  MfaApiError,
  getMfaStatus,
  groupKey,
  mfaErrorMessage,
  startMfaEnrollment,
  verifyMfaCode,
  type MfaEnrollment,
} from '@/lib/mfa'
import { useAuthStore } from '@/store/auth'
import { useMfaPrompt, type MfaPromptReason } from '@/store/mfaPrompt'

// The console's MFA prompt (PENDING-06 M-6, M-7). Mounted once, above the
// routes, so it also works while the route guard is still probing `me`.
//
// It asks for a code from the user's authenticator app; a user without one
// sets it up here (QR code and setup key, confirmed by the first code). The
// setup secret lives in this component's state only: never in storage, the
// URL or a log, and it is dropped when the prompt closes.

type Step = 'loading' | 'verify' | 'enroll' | 'error'

const COPY: Record<MfaPromptReason, { title: string; description: string }> = {
  console: {
    title: 'Two-factor authentication required',
    description: 'This workspace requires two-factor authentication to use the admin console.',
  },
  'step-up': {
    title: 'Confirm it’s you',
    description: 'This action needs a second factor verified in the last 15 minutes.',
  },
  self: {
    title: 'Verify your authenticator',
    description: 'Enter a code from your authenticator app.',
  },
}

export function MfaPrompt() {
  const { open, reason, finish } = useMfaPrompt()

  async function signOut() {
    finish(false)
    await apolloClient.clearStore()
    useAuthStore.getState().clearAuth()
    window.location.href = '/login'
  }

  const copy = COPY[reason]
  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) finish(false) }}>
      <DialogContent aria-describedby="mfa-prompt-description" className="max-w-md">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <KeyRound className="h-4 w-4" />
            {copy.title}
          </DialogTitle>
          <DialogDescription id="mfa-prompt-description">{copy.description}</DialogDescription>
        </DialogHeader>
        {/* Mounted only while open: no code, secret or error survives a close. */}
        {open && (
          <PromptBody
            onVerified={() => finish(true)}
            onCancel={() => finish(false)}
            onSignOut={reason === 'console' ? () => void signOut() : undefined}
          />
        )}
      </DialogContent>
    </Dialog>
  )
}

function PromptBody({
  onVerified,
  onCancel,
  onSignOut,
}: {
  onVerified: () => void
  onCancel: () => void
  onSignOut?: () => void
}) {
  const [step, setStep] = useState<Step>('loading')
  const [enrollment, setEnrollment] = useState<MfaEnrollment>()
  const [loadError, setLoadError] = useState<string>()
  // One enrolment request per prompt: a second one would replace the secret
  // the first showed (StrictMode runs effects twice).
  const enrollStarted = useRef(false)

  function beginEnrollment() {
    if (enrollStarted.current) return
    enrollStarted.current = true
    setStep('enroll')
    startMfaEnrollment().then(setEnrollment, (err: unknown) => {
      if (err instanceof MfaApiError && err.code === 'ALREADY_ENROLLED') {
        setStep('verify')
        return
      }
      setLoadError(err instanceof MfaApiError && err.code === 'UNAUTHORIZED'
        ? 'Your session has ended. Sign in again.'
        : 'Could not start the setup. Try again shortly.')
      setStep('error')
    })
  }

  const statusStarted = useRef(false)
  useEffect(() => {
    if (statusStarted.current) return
    statusStarted.current = true
    getMfaStatus().then(
      (s) => (s.enrolled ? setStep('verify') : beginEnrollment()),
      (err: unknown) => {
        setLoadError(mfaErrorMessage(err))
        setStep('error')
      },
    )
  }, [])

  const leave = onSignOut ? (
    <Button type="button" variant="outline" onClick={onSignOut}>Sign out</Button>
  ) : (
    <Button type="button" variant="outline" onClick={onCancel}>Cancel</Button>
  )

  if (step === 'loading' || (step === 'enroll' && !enrollment)) {
    return (
      <div className="flex items-center gap-2 py-6 text-sm text-muted-foreground" aria-busy="true">
        <Loader2 className="h-4 w-4 animate-spin" />
        {step === 'enroll' ? 'Preparing setup…' : 'Loading…'}
      </div>
    )
  }

  if (step === 'error') {
    return (
      <div className="space-y-4">
        <Alert variant="destructive">
          <AlertDescription>{loadError}</AlertDescription>
        </Alert>
        <DialogFooter>{leave}</DialogFooter>
      </div>
    )
  }

  return (
    <div className="space-y-5">
      {step === 'enroll' && enrollment && (
        <>
          <ol className="list-decimal space-y-1 pl-5 text-sm text-muted-foreground">
            <li>Scan the QR code with your authenticator app, or enter the setup key.</li>
            <li>Enter the 6-digit code it shows to confirm.</li>
          </ol>
          <div className="flex justify-center rounded-xl bg-white p-4">
            <QRCodeSVG value={enrollment.uri} size={168} role="img" aria-label="QR code for your authenticator app" />
          </div>
          <div className="space-y-1.5">
            <span className="text-xs font-medium text-muted-foreground">Setup key</span>
            <code
              aria-label="Setup key"
              className="block select-all break-all rounded-lg border border-border bg-secondary/40 px-3 py-2 font-mono text-sm tracking-wide"
            >
              {groupKey(enrollment.secret)}
            </code>
          </div>
        </>
      )}
      <CodeForm
        submitLabel={step === 'enroll' ? 'Confirm and continue' : 'Verify'}
        leave={leave}
        onVerified={onVerified}
        onNotEnrolled={beginEnrollment}
      />
    </div>
  )
}

function CodeForm({
  submitLabel,
  leave,
  onVerified,
  onNotEnrolled,
}: {
  submitLabel: string
  leave: React.ReactNode
  onVerified: () => void
  onNotEnrolled: () => void
}) {
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
      await verifyMfaCode(entered)
      onVerified()
    } catch (err) {
      if (err instanceof MfaApiError && err.code === 'NOT_ENROLLED') {
        onNotEnrolled()
        return
      }
      setError(mfaErrorMessage(err))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <form onSubmit={submit} className="space-y-4" noValidate>
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <div className="space-y-1.5">
        <Label htmlFor="mfa-code">Authentication code</Label>
        <Input
          id="mfa-code"
          name="mfa-code"
          inputMode="numeric"
          autoComplete="one-time-code"
          maxLength={6}
          autoFocus
          className="h-11 text-center font-mono text-lg tracking-[0.4em]"
          value={code}
          onChange={(e) => setCode(e.target.value.replace(/\D/g, '').slice(0, 6))}
        />
      </div>
      <DialogFooter>
        {leave}
        <Button type="submit" disabled={submitting || code.length !== 6}>
          {submitting ? 'Checking…' : submitLabel}
        </Button>
      </DialogFooter>
    </form>
  )
}
