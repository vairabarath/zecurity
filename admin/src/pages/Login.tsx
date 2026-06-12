import { useState, type FormEvent } from 'react'
import { useLazyQuery, useMutation } from '@apollo/client/react'
import { Link } from 'react-router-dom'
import {
  InitiateAuthDocument,
  LookupWorkspaceDocument,
  LookupWorkspacesByEmailDocument,
} from '@/generated/graphql'
import type { LookupWorkspacesByEmailQuery } from '@/generated/graphql'
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

export default function Login() {
  const [mode, setMode] = useState<AuthMode>('endpoint')
  const [slug, setSlug] = useState('')
  const [email, setEmail] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [showWorkspaces, setShowWorkspaces] = useState(false)
  const [foundWorkspaces, setFoundWorkspaces] = useState<
    LookupWorkspacesByEmailQuery['lookupWorkspacesByEmail']['workspaces']
  >([])

  const [lookupWorkspace] = useLazyQuery(LookupWorkspaceDocument)
  const [lookupByEmail] = useLazyQuery(LookupWorkspacesByEmailDocument)
  const [initiateAuth] = useMutation(InitiateAuthDocument)

  function handleSlugChange(value: string) {
    setSlug(value.toLowerCase().replace(/[^a-z0-9-]/g, ''))
    setError(null)
  }

  async function handleEndpointSubmit(event: FormEvent) {
    event.preventDefault()
    if (!slug.trim()) return
    setLoading(true)
    setError(null)
    try {
      const result = await lookupWorkspace({ variables: { slug } })
      const workspace = result.data?.lookupWorkspace
      if (!workspace?.found || !workspace.workspace) {
        setError('Network not found. Verify the endpoint and retry.')
        return
      }

      sessionStorage.setItem('ztna_workspace_slug', slug)

      const authResult = await initiateAuth({
        variables: { provider: 'google', workspaceName: workspace.workspace.name },
      })
      const { redirectUrl, state } = authResult.data!.initiateAuth
      sessionStorage.setItem('ztna_oauth_state', state)
      window.location.href = redirectUrl
    } catch {
      setError('Authentication failed. Check the endpoint and try again.')
    } finally {
      setLoading(false)
    }
  }

  async function handleEmailSubmit(event: FormEvent) {
    event.preventDefault()
    if (!email.trim() || !email.includes('@')) return

    setLoading(true)
    setError(null)
    setShowWorkspaces(false)
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
    try {
      sessionStorage.setItem('ztna_workspace_slug', workspaceSlug)
      const authResult = await initiateAuth({
        variables: { provider: 'google', workspaceName },
      })
      const { redirectUrl, state } = authResult.data!.initiateAuth
      sessionStorage.setItem('ztna_oauth_state', state)
      window.location.href = redirectUrl
    } catch {
      setError('Authentication failed. Please retry.')
      setLoading(false)
    }
  }

  return (
    <AuthShell>
      <AuthCard>
        <BrandBlock subtitle="Zero Trust Network Access" />
        <ModeTabs
          mode={mode}
          onChange={(nextMode) => {
            setMode(nextMode)
            setError(null)
            setShowWorkspaces(false)
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

            {showWorkspaces ? (
              <div className="mt-4 space-y-2 rounded-2xl border border-border bg-secondary p-3">
                <div className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">
                  Available Workspaces
                </div>
                {foundWorkspaces.map((workspace) => (
                  <button
                    key={workspace.id}
                    type="button"
                    onClick={() => startOAuth(workspace.name, workspace.slug)}
                    className="flex w-full items-center justify-between rounded-xl border border-transparent bg-card px-4 py-3 text-left transition hover:border-primary/50 hover:bg-accent"
                  >
                    <div>
                      <div className="text-sm font-semibold">{workspace.name}</div>
                      <div className="mt-1 text-xs text-muted-foreground">{workspace.slug}.zecurity.in</div>
                    </div>
                    <span className="text-xs font-semibold text-primary">Continue</span>
                  </button>
                ))}
              </div>
            ) : null}
          </form>
        )}

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
          }}
        />
      </AuthCard>
    </AuthShell>
  )
}
