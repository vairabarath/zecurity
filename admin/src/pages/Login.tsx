import { useState, type FormEvent } from 'react'
import { useLazyQuery, useMutation } from '@apollo/client/react'
import { Link, useSearchParams } from 'react-router-dom'
import {
  InitiateAuthDocument,
  LookupWorkspaceDocument,
  LookupWorkspacesByEmailDocument,
  LookupIdpConnectionsDocument,
} from '@/generated/graphql'
import type {
  LookupWorkspacesByEmailQuery,
  LookupIdpConnectionsQuery,
} from '@/generated/graphql'
import {
  AuthCard,
  AuthInput,
  AuthShell,
  BrandBlock,
  Field,
  FooterLink,
  LockLabel,
  MailLabel,
  ModeTabs,
  PrimaryButton,
} from '@/components/auth/AuthLayout'

type AuthMode = 'endpoint' | 'email'
type IdpConnection = LookupIdpConnectionsQuery['lookupIdpConnections'][number]
type ResolvedWorkspace = { name: string; slug: string }

// The workspace's own BYO connections. Anything not explicitly "bootstrap" is
// treated as enterprise so an unrecognised tier degrades to the primary path
// rather than silently disappearing from the login page.
const isEnterprise = (connection: IdpConnection) => connection.tier !== 'bootstrap'

// The OAuth callback cannot render anything itself — it only redirects to
// /login?error=<reason> (internal/auth/callback.go: fail()). Until this map
// existed the reason was silently dropped and the user was returned to a blank
// form, which is the worst possible outcome for the case that matters most:
// a proven sign-in refused because the workspace never granted them access.
//
// Unknown reasons fall back to a generic message rather than echoing the raw
// code, so a crafted ?error= cannot paint arbitrary text onto the login page.
const CALLBACK_ERRORS: Record<string, string> = {
  not_invited:
    'Your identity provider signed you in, but this network has not granted you access. Ask an administrator to invite you.',
  authentication_failed: 'Sign-in with your identity provider failed. Please try again.',
  missing_params: 'The sign-in response was incomplete. Please start again.',
  state_expired: 'This sign-in attempt expired. Please start again.',
  state_invalid: 'The sign-in response could not be verified. Please start again.',
  session_failed: 'Signed in, but the session could not be loaded. Please try again.',
  token_issue_failed: 'Signed in, but no session could be issued. Please try again.',
  refresh_issue_failed: 'Signed in, but the session could not be persisted. Please try again.',
  server_error: 'The server could not complete sign-in. Please try again shortly.',
}

const callbackErrorMessage = (reason: string | null): string | null => {
  if (!reason) return null
  return CALLBACK_ERRORS[reason] ?? 'Sign-in could not be completed. Please try again.'
}

// Raised when the public IdP-discovery query itself fails. It exists to keep an
// API/network failure from being read as "this workspace has no IdP configured":
// only an EMPTY SUCCESSFUL response may fall back to Google.
class IdpDiscoveryError extends Error {}

export default function Login() {
  const [mode, setMode] = useState<AuthMode>('endpoint')
  const [slug, setSlug] = useState('')
  const [email, setEmail] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // A reason handed back by the OAuth callback redirect. Held separately from
  // `error` (this form's own validation state) so it survives until the user
  // actually retries, and is cleared the moment they do.
  const [searchParams] = useSearchParams()
  const [callbackDismissed, setCallbackDismissed] = useState(false)
  const callbackError = callbackDismissed
    ? null
    : callbackErrorMessage(searchParams.get('error'))
  const notInvited = !callbackDismissed && searchParams.get('error') === 'not_invited'
  const [showWorkspaces, setShowWorkspaces] = useState(false)
  const [foundWorkspaces, setFoundWorkspaces] = useState<
    LookupWorkspacesByEmailQuery['lookupWorkspacesByEmail']['workspaces']
  >([])
  const [connections, setConnections] = useState<IdpConnection[]>([])
  const [selectedConnection, setSelectedConnection] = useState<IdpConnection | null>(null)
  // The platform (Google) path is kept collapsed behind an explicit disclosure.
  // See `platformConnections` below for why it is not a primary button.
  const [showOtherOptions, setShowOtherOptions] = useState(false)
  // The workspace the connection choices belong to, captured where it is
  // actually resolved (slug lookup or the email-mode workspace the user picked)
  // so initiateAuth always receives that workspace's real name.
  const [pendingWorkspace, setPendingWorkspace] = useState<ResolvedWorkspace | null>(null)

  const [lookupWorkspace] = useLazyQuery(LookupWorkspaceDocument)
  const [lookupByEmail] = useLazyQuery(LookupWorkspacesByEmailDocument)
  const [lookupIdpConnections] = useLazyQuery(LookupIdpConnectionsDocument)
  const [initiateAuth] = useMutation(InitiateAuthDocument)

  function resetIdpSelection() {
    setConnections([])
    setSelectedConnection(null)
    setPendingWorkspace(null)
    setShowOtherOptions(false)
  }

  function handleSlugChange(value: string) {
    setSlug(value.toLowerCase().replace(/[^a-z0-9-]/g, ''))
    setError(null)
    resetIdpSelection()
  }

  // Public login discovery. useLazyQuery RESOLVES (it does not throw) on a
  // GraphQL error, handing back `{ error, data: undefined }` — so both are
  // checked explicitly and turned into IdpDiscoveryError. Returning `[]` here
  // must mean "the server said this workspace has no IdP", nothing else.
  async function discoverConnections(workspaceSlug: string): Promise<IdpConnection[]> {
    let result: Awaited<ReturnType<typeof lookupIdpConnections>>
    try {
      result = await lookupIdpConnections({ variables: { workspaceSlug } })
    } catch (err) {
      // Some transports reject instead of resolving with `error`; both are a
      // discovery failure, never an empty connection list.
      throw new IdpDiscoveryError(err instanceof Error ? err.message : 'idp discovery failed')
    }
    if (result.error || !result.data) {
      throw new IdpDiscoveryError(result.error?.message ?? 'idp discovery returned no data')
    }
    return result.data.lookupIdpConnections
  }

  // The single hand-off point to an IdP: stash the CSRF state, then navigate.
  // The `window.location.href` assignment trips react-hooks' "value cannot be
  // modified" rule, which the pre-F7-5 code tripped too; a full-page OAuth
  // redirect is exactly the intent, and it is now confined to this one site.
  function redirectToIdp(payload: { redirectUrl: string; state: string }) {
    sessionStorage.setItem('ztna_oauth_state', payload.state)
    window.location.href = payload.redirectUrl
  }

  // The pre-F7-5 behavior, unchanged: no workspace-configured IdP means the
  // shared platform (Google) login path.
  async function startGoogleFallback(workspaceName: string) {
    const authResult = await initiateAuth({
      variables: { provider: 'google', workspaceName },
    })
    redirectToIdp(authResult.data!.initiateAuth)
  }

  // Shared tail of both entry points: discover, then either fall back to Google
  // or render the explicit choices.
  //
  // The fallback decision keys off the ENTERPRISE tier only. lookupIdpConnections
  // now also returns the shared platform IdP (tier "bootstrap") when the
  // workspace still allows it, so `discovered.length` is no longer the right
  // test: a workspace with no IdP of its own would otherwise render a
  // single-button "chooser" containing only Google instead of going straight
  // there, which is the pre-F7-5 behavior and the one users expect.
  async function continueWithWorkspace(workspace: ResolvedWorkspace) {
    const discovered = await discoverConnections(workspace.slug)
    if (discovered.filter(isEnterprise).length === 0) {
      await startGoogleFallback(workspace.name)
      return
    }
    setPendingWorkspace(workspace)
    setConnections(discovered)
  }

  async function handleEndpointSubmit(event: FormEvent) {
    event.preventDefault()
    if (!slug.trim()) return
    setLoading(true)
    setError(null)
    setCallbackDismissed(true)
    resetIdpSelection()
    try {
      const result = await lookupWorkspace({ variables: { slug } })
      const workspace = result.data?.lookupWorkspace
      if (!workspace?.found || !workspace.workspace) {
        setError('Network not found. Verify the endpoint and retry.')
        return
      }

      sessionStorage.setItem('ztna_workspace_slug', slug)
      await continueWithWorkspace({ name: workspace.workspace.name, slug })
    } catch (err) {
      setError(
        err instanceof IdpDiscoveryError
          ? 'Could not load the identity providers for this network. Please retry.'
          : 'Authentication failed. Check the endpoint and try again.',
      )
    } finally {
      setLoading(false)
    }
  }

  async function handleEmailSubmit(event: FormEvent) {
    event.preventDefault()
    if (!email.trim() || !email.includes('@')) return

    setLoading(true)
    setError(null)
    setCallbackDismissed(true)
    setShowWorkspaces(false)
    resetIdpSelection()
    try {
      const result = await lookupByEmail({ variables: { email: email.trim() } })
      const workspaces = result.data?.lookupWorkspacesByEmail.workspaces ?? []
      if (workspaces.length === 0) {
        setError('No workspaces are mapped to this email address.')
        return
      }
      setFoundWorkspaces(workspaces)
      setShowWorkspaces(true)
    } catch {
      setError('Workspace lookup failed. Check your connection and retry.')
    } finally {
      setLoading(false)
    }
  }

  async function startOAuth(workspaceName: string, workspaceSlug: string) {
    setLoading(true)
    setError(null)
    resetIdpSelection()
    try {
      sessionStorage.setItem('ztna_workspace_slug', workspaceSlug)
      await continueWithWorkspace({ name: workspaceName, slug: workspaceSlug })
    } catch (err) {
      setError(
        err instanceof IdpDiscoveryError
          ? 'Could not load the identity providers for this network. Please retry.'
          : 'Authentication failed. Please retry.',
      )
    } finally {
      setLoading(false)
    }
  }

  // The user picked an exact configured connection. `connection.id` is the
  // authority — initiateAuth resolves the connection by id and fails closed if
  // it is not active. A failure here surfaces the error and leaves the choices
  // on screen; it NEVER retries through Google.
  async function selectConnection(connection: IdpConnection) {
    if (!pendingWorkspace) return
    setSelectedConnection(connection)
    setLoading(true)
    setError(null)
    try {
      const authResult = await initiateAuth({
        variables: {
          provider: connection.provider,
          connectionId: connection.id,
          workspaceName: pendingWorkspace.name,
        },
      })
      redirectToIdp(authResult.data!.initiateAuth)
    } catch {
      setError(
        `Sign-in with ${connectionLabel(connection)} failed. Select a provider to try again.`,
      )
      setSelectedConnection(null)
    } finally {
      setLoading(false)
    }
  }

  // displayName is the user-visible label; an empty one falls back to the
  // provider key rather than inventing backend display metadata.
  const connectionLabel = (connection: IdpConnection) =>
    connection.displayName.trim() || connection.provider

  const enterpriseConnections = connections.filter(isEnterprise)
  // The shared platform IdP, offered only when the workspace still permits it
  // (the server drops this tier when platform login is disabled — ADR-024 §5).
  //
  // It is deliberately NOT a primary button. Login discovery happens before any
  // JWT exists, so the page cannot know whether the visitor is an admin, and
  // asking the server would mean an unauthenticated "is this person an admin?"
  // oracle. It is collapsed instead: an admin who needs the platform path can
  // reach it, while a rank-and-file member signing in through the workspace IdP
  // is not nudged into it — clicking it as a never-before-seen identity does not
  // join this workspace, it provisions a brand-new one (bootstrap.Provision).
  const platformConnections = connections.filter((c) => !isEnterprise(c))

  const connectionButton = (connection: IdpConnection) => (
    <button
      key={connection.id}
      type="button"
      disabled={loading}
      onClick={() => selectConnection(connection)}
      className="flex w-full items-center justify-between rounded-xl border border-transparent bg-card px-4 py-3 text-left transition hover:border-primary/50 hover:bg-accent disabled:opacity-60"
    >
      <div>
        <div className="text-sm font-semibold">{connectionLabel(connection)}</div>
        <div className="mt-1 text-xs text-muted-foreground">{connection.provider}</div>
      </div>
      <span className="text-xs font-semibold text-primary">Continue</span>
    </button>
  )

  // Rendered for BOTH modes: endpoint-mode discovery and the email-mode
  // workspace picker feed the same chooser.
  const idpChooser =
    connections.length > 0 ? (
      <div className="mt-4 space-y-2 rounded-2xl border border-border bg-secondary p-3">
        <div className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">
          Select identity provider
        </div>
        {selectedConnection ? (
          <div className="rounded-xl bg-card px-4 py-3">
            <div className="text-sm font-semibold">
              Signing in with {connectionLabel(selectedConnection)}
            </div>
            <div className="mt-1 text-xs text-muted-foreground">
              {pendingWorkspace?.slug}.zecurity.in
            </div>
          </div>
        ) : (
          <>
            {enterpriseConnections.map(connectionButton)}

            {platformConnections.length > 0 ? (
              showOtherOptions ? (
                <div className="space-y-2 border-t border-border pt-2">
                  <div className="text-[11px] text-muted-foreground">
                    Use this only if you cannot sign in with your organization&apos;s provider.
                  </div>
                  {platformConnections.map(connectionButton)}
                </div>
              ) : (
                <button
                  type="button"
                  disabled={loading}
                  onClick={() => setShowOtherOptions(true)}
                  className="w-full rounded-xl px-4 py-2 text-xs font-semibold text-muted-foreground transition hover:text-primary disabled:opacity-60"
                >
                  Other sign-in options
                </button>
              )
            ) : null}
          </>
        )}
      </div>
    ) : null

  return (
    <AuthShell>
      <AuthCard>
        <BrandBlock subtitle="Zero Trust Network Access" />

        {callbackError ? (
          <div
            role="alert"
            className="mb-4 rounded-2xl border border-destructive/40 bg-destructive/10 p-3"
          >
            <div className="text-[13px] font-medium text-foreground">{callbackError}</div>
            {/* The workspace has no path forward for this person — so offer the
              one they DO control: standing up their own network. */}
            {notInvited ? (
              <div className="mt-1.5 text-xs text-muted-foreground">
                Need your own network instead?{' '}
                <Link to="/signup" className="font-semibold text-primary">
                  Deploy one
                </Link>
                .
              </div>
            ) : null}
          </div>
        ) : null}

        <ModeTabs
          mode={mode}
          onChange={(nextMode) => {
            setMode(nextMode)
            setError(null)
            setShowWorkspaces(false)
            resetIdpSelection()
          }}
        />

        {mode === 'endpoint' ? (
          <form onSubmit={handleEndpointSubmit}>
            <Field
              label="Network endpoint"
              suffix=".zecurity.in"
              error={error}
              hint={!error ? 'Use your workspace slug to continue into OAuth.' : undefined}
            >
              <AuthInput
                value={slug}
                onChange={(event) => handleSlugChange(event.target.value)}
                placeholder="your-network"
                autoFocus
              />
            </Field>
            <PrimaryButton type="submit" disabled={loading || !slug.trim()}>
              <LockLabel />
              {loading ? 'Authenticating...' : 'Authenticate'}
            </PrimaryButton>
          </form>
        ) : (
          <form onSubmit={handleEmailSubmit}>
            <Field
              label="Identity lookup"
              error={error}
              hint={!error ? 'Find all workspaces associated with your email.' : undefined}
            >
              <AuthInput
                type="email"
                value={email}
                onChange={(event) => {
                  setEmail(event.target.value)
                  setError(null)
                }}
                placeholder="you@company.com"
                autoFocus
              />
            </Field>
            <PrimaryButton type="submit" disabled={loading || !email.includes('@')}>
              <MailLabel />
              {loading ? 'Searching...' : 'Find My Workspaces'}
            </PrimaryButton>

            {showWorkspaces && connections.length === 0 ? (
              <div className="mt-4 space-y-2 rounded-2xl border border-border bg-secondary p-3">
                <div className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">
                  Available Workspaces
                </div>
                {foundWorkspaces.map((workspace) => (
                  <button
                    key={workspace.id}
                    type="button"
                    disabled={loading}
                    onClick={() => startOAuth(workspace.name, workspace.slug)}
                    className="flex w-full items-center justify-between rounded-xl border border-transparent bg-card px-4 py-3 text-left transition hover:border-primary/50 hover:bg-accent disabled:opacity-60"
                  >
                    <div>
                      <div className="text-sm font-semibold">{workspace.name}</div>
                      <div className="mt-1 text-xs text-muted-foreground">
                        {workspace.slug}.zecurity.in
                      </div>
                    </div>
                    <span className="text-xs font-semibold text-primary">Continue</span>
                  </button>
                ))}
              </div>
            ) : null}
          </form>
        )}

        {idpChooser}

        <div className="mt-4 text-center text-[12.5px] text-muted-foreground">
          Don&apos;t have a network?{' '}
          <Link to="/signup" className="font-semibold text-primary">
            Deploy one
          </Link>
        </div>

        <FooterLink
          text={mode === 'endpoint' ? 'Prefer identity lookup?' : 'Know your endpoint already?'}
          cta={mode === 'endpoint' ? 'Use email instead' : 'Use endpoint instead'}
          onClick={() => {
            setMode(mode === 'endpoint' ? 'email' : 'endpoint')
            setError(null)
            setShowWorkspaces(false)
            resetIdpSelection()
          }}
        />
      </AuthCard>
    </AuthShell>
  )
}
