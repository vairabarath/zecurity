import { useCallback, useEffect, useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import { KeyRound } from 'lucide-react'
import { toast } from 'sonner'
import { GetConsoleSecurityDocument, SetConsoleRequireMfaDocument } from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { Capability, useCan } from '@/lib/capabilities'
import { relativeTime } from '@/lib/console'
import { getMfaStatus, isMfaRequired, type MfaStatus } from '@/lib/mfa'
import { toastMutationError } from '@/lib/mutationErrors'
import { requestMfa } from '@/store/mfaPrompt'

// Settings → Security (PENDING-06 M-6).
//   - Your authenticator: set it up or verify a code (gives this session the
//     MFA mark, which turning the requirement on and break-glass both need).
//   - Require MFA for the admin console: IDENTITY_ADMIN. The server refuses
//     to turn it on unless this session has passed MFA, so the workspace
//     can't lock itself out; the console's MFA prompt answers that refusal.
export function ConsoleSecurityCard() {
  const canEdit = useCan(Capability.IdentityAdmin)
  const [status, setStatus] = useState<MfaStatus | null>(null)
  const [statusError, setStatusError] = useState(false)

  const loadStatus = useCallback(() => {
    getMfaStatus().then(
      (s) => {
        setStatus(s)
        setStatusError(false)
      },
      () => setStatusError(true),
    )
  }, [])
  useEffect(loadStatus, [loadStatus])

  const { data, loading } = useQuery(GetConsoleSecurityDocument, { fetchPolicy: 'cache-and-network' })
  const [setRequireMfa, { loading: saving }] = useMutation(SetConsoleRequireMfaDocument, {
    refetchQueries: [{ query: GetConsoleSecurityDocument }],
  })

  async function verifyNow() {
    if (await requestMfa('self')) {
      toast.success(status?.enrolled ? 'Code verified' : 'Authenticator set up')
      loadStatus()
    }
  }

  async function toggle(required: boolean) {
    try {
      await setRequireMfa({ variables: { required } })
      toast.success(required ? 'The admin console now requires MFA' : 'The admin console no longer requires MFA')
      loadStatus()
    } catch (err) {
      // MFA_REQUIRED was already answered by the prompt; closing it is not an error.
      if (!isMfaRequired(err)) toastMutationError(err)
    }
  }

  const required = data?.consoleSecurity.requireMfa ?? false

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm font-medium">
          <KeyRound className="h-4 w-4" />
          Security
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-6 text-sm">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="space-y-1">
            <div className="font-medium">Your authenticator app</div>
            {statusError ? (
              <p className="text-muted-foreground">Could not read your MFA status.</p>
            ) : !status ? (
              <Skeleton className="h-4 w-56" />
            ) : status.enrolled ? (
              <p className="text-muted-foreground">
                Set up.{' '}
                {status.sessionMfaAt
                  ? `This session passed MFA ${relativeTime(status.sessionMfaAt)}.`
                  : 'This session has not passed MFA.'}
              </p>
            ) : (
              <p className="text-muted-foreground">
                Not set up. Resources that require MFA, and break-glass actions, will ask you to set one up.
              </p>
            )}
          </div>
          {status && (
            <Button variant="outline" size="sm" onClick={() => void verifyNow()}>
              {status.enrolled ? 'Verify a code' : 'Set up'}
            </Button>
          )}
        </div>

        <div className="flex flex-wrap items-start justify-between gap-4 border-t border-border pt-5">
          <div className="max-w-xl space-y-1">
            <div className="font-medium">Require MFA for the admin console</div>
            <p className="text-muted-foreground">
              Everyone who signs in to this console must also enter a code from an authenticator app. Users
              without one set it up at their next visit. Members using only the client are asked too when
              they open the install page.
            </p>
            {!canEdit && <p className="text-xs text-muted-foreground">Only an Admin can change this.</p>}
          </div>
          {loading && !data ? (
            <Skeleton className="h-6 w-11 rounded-full" />
          ) : (
            <Switch
              checked={required}
              disabled={!canEdit || saving}
              onCheckedChange={(next) => void toggle(next)}
              aria-label="Require MFA for the admin console"
            />
          )}
        </div>
      </CardContent>
    </Card>
  )
}
