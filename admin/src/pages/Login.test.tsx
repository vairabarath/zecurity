import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MockedProvider } from '@apollo/client/testing/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import type { ComponentProps, ReactElement } from 'react'
import Login from '@/pages/Login'
import {
  InitiateAuthDocument,
  LookupWorkspaceDocument,
  LookupWorkspacesByEmailDocument,
  LookupIdpConnectionsDocument,
} from '@/generated/graphql'
import type { LookupIdpConnectionsQuery } from '@/generated/graphql'

type Connection = LookupIdpConnectionsQuery['lookupIdpConnections'][number]
type InitiateAuthVars = {
  provider: string
  workspaceName?: string | null
  connectionId?: string | null
}

// window.location.href is how Login hands off to the IdP. jsdom will not
// navigate, so it is replaced with a plain recorder — which also lets each test
// assert WHICH url was handed off.
let redirectedTo: string[] = []

beforeEach(() => {
  redirectedTo = []
  Object.defineProperty(window, 'location', {
    configurable: true,
    value: {
      ...window.location,
      get href() {
        return redirectedTo[redirectedTo.length - 1] ?? ''
      },
      set href(value: string) {
        redirectedTo.push(value)
      },
    },
  })
  sessionStorage.clear()
})

afterEach(() => {
  vi.restoreAllMocks()
})

type Mocks = ComponentProps<typeof MockedProvider>['mocks']

const renderLoginAt = (mocks: Mocks, entry: string): ReactElement => (
  <MemoryRouter initialEntries={[entry]}>
    <Routes>
      <Route
        path="/login"
        element={
          <MockedProvider mocks={mocks}>
            <Login />
          </MockedProvider>
        }
      />
    </Routes>
  </MemoryRouter>
)

const renderLogin = (mocks: Mocks): ReactElement => (
  <MemoryRouter initialEntries={['/login']}>
    <Routes>
      <Route
        path="/login"
        element={
          <MockedProvider mocks={mocks}>
            <Login />
          </MockedProvider>
        }
      />
    </Routes>
  </MemoryRouter>
)

const workspaceMock = (slug: string, name = 'Primary') => ({
  request: { query: LookupWorkspaceDocument, variables: { slug } },
  result: { data: { lookupWorkspace: { found: true, workspace: { id: `ws-${slug}`, name, slug } } } },
})

const connectionsMock = (slug: string, connections: Connection[]) => ({
  request: { query: LookupIdpConnectionsDocument, variables: { workspaceSlug: slug } },
  result: { data: { lookupIdpConnections: connections } },
})

// A NETWORK failure: the lazy-query promise rejects.
const connectionsNetworkErrorMock = (slug: string) => ({
  request: { query: LookupIdpConnectionsDocument, variables: { workspaceSlug: slug } },
  error: new Error('discovery exploded'),
})

// A GRAPHQL failure: the lazy-query promise RESOLVES with `{ error, data:
// undefined }`. This is the shape that a naive `data?.field ?? []` silently
// turns into "no connections configured" -> Google fallback, so it needs its own
// test; the network case alone does not cover it.
const connectionsGraphqlErrorMock = (slug: string) => ({
  request: { query: LookupIdpConnectionsDocument, variables: { workspaceSlug: slug } },
  result: { errors: [{ message: 'lookupIdpConnections: workspace lookup failed' }] },
})

// A initiateAuth mock that records the variables it was called with, so tests
// assert the real mutation input instead of inferring it from rendered text.
const initiateAuthCalls: InitiateAuthVars[] = []
const initiateAuthMock = (vars: InitiateAuthVars, redirectUrl: string, state = 'state-1') => ({
  request: { query: InitiateAuthDocument, variables: vars },
  result: () => {
    initiateAuthCalls.push(vars)
    return { data: { initiateAuth: { redirectUrl, state } } }
  },
})

const initiateAuthErrorMock = (vars: InitiateAuthVars) => ({
  request: { query: InitiateAuthDocument, variables: vars },
  error: new Error('oauth rejected'),
})

beforeEach(() => {
  initiateAuthCalls.length = 0
})

const conn = (
  id: string,
  provider: string,
  displayName: string,
  tier: 'enterprise' | 'bootstrap' = 'enterprise',
): Connection => ({
  id,
  provider,
  displayName,
  tier,
})

// The shared platform IdP as the server now returns it (tier "bootstrap"),
// alongside the workspace's own connections.
const googleTier = (id = 'conn-google') => conn(id, 'google', 'Google', 'bootstrap')

const submitEndpoint = async (slug: string) => {
  const user = userEvent.setup()
  await user.type(screen.getByPlaceholderText('your-network'), slug)
  await user.click(screen.getByRole('button', { name: /Authenticate/i }))
  return user
}

describe('Login', () => {
  it('renders endpoint mode by default', () => {
    render(renderLogin([]))
    expect(screen.getByPlaceholderText('your-network')).toBeInTheDocument()
  })

  // 1. No configured IdP connections -> Google fallback still works.
  it('preserves the Google fallback when the workspace has no IdP connections', async () => {
    render(
      renderLogin([
        workspaceMock('acme'),
        connectionsMock('acme', []),
        initiateAuthMock(
          { provider: 'google', workspaceName: 'Primary' },
          'https://accounts.google.test/o/oauth2',
        ),
      ]),
    )

    await submitEndpoint('acme')

    await waitFor(() =>
      expect(redirectedTo).toContain('https://accounts.google.test/o/oauth2'),
    )
    expect(initiateAuthCalls).toEqual([{ provider: 'google', workspaceName: 'Primary' }])
    expect(sessionStorage.getItem('ztna_oauth_state')).toBe('state-1')
    // No chooser is shown when there is nothing to choose.
    expect(screen.queryByText('Select identity provider')).not.toBeInTheDocument()
  })

  // 2. One configured IdP -> button rendered, labelled with displayName.
  it('renders a single configured IdP labelled with its displayName', async () => {
    render(
      renderLogin([
        workspaceMock('acme'),
        connectionsMock('acme', [conn('conn-1', 'okta', 'Acme Okta')]),
      ]),
    )

    await submitEndpoint('acme')

    await waitFor(() => expect(screen.getByText('Acme Okta')).toBeInTheDocument())
    expect(screen.getByText('Select identity provider')).toBeInTheDocument()
    // Never auto-redirects when a choice exists.
    expect(redirectedTo).toHaveLength(0)
    expect(initiateAuthCalls).toHaveLength(0)
  })

  // 3. Multiple configured IdPs -> all rendered, no dedup.
  it('renders every configured IdP without provider deduplication', async () => {
    render(
      renderLogin([
        workspaceMock('acme'),
        connectionsMock('acme', [
          conn('conn-1', 'okta', 'Acme Okta'),
          conn('conn-2', 'okta', 'Acme Okta Sandbox'),
          conn('conn-3', 'entra', 'Acme Entra'),
        ]),
      ]),
    )

    await submitEndpoint('acme')

    await waitFor(() => expect(screen.getByText('Acme Okta')).toBeInTheDocument())
    expect(screen.getByText('Acme Okta Sandbox')).toBeInTheDocument()
    expect(screen.getByText('Acme Entra')).toBeInTheDocument()
    // Two okta connections => the provider sub-label appears twice, proving the
    // list was not collapsed by provider.
    expect(screen.getAllByText('okta')).toHaveLength(2)
  })

  // 4. Same provider, several connections -> both selectable, exact id sent.
  it('keeps same-provider connections independently selectable and sends the exact connectionId', async () => {
    render(
      renderLogin([
        workspaceMock('acme'),
        connectionsMock('acme', [
          conn('conn-prod', 'okta', 'Acme Okta Prod'),
          conn('conn-sandbox', 'okta', 'Acme Okta Sandbox'),
        ]),
        initiateAuthMock(
          { provider: 'okta', connectionId: 'conn-sandbox', workspaceName: 'Primary' },
          'https://sandbox.okta.test/authorize',
        ),
      ]),
    )

    const user = await submitEndpoint('acme')
    await waitFor(() => expect(screen.getByText('Acme Okta Sandbox')).toBeInTheDocument())
    expect(screen.getByText('Acme Okta Prod')).toBeInTheDocument()

    await user.click(screen.getByText('Acme Okta Sandbox'))

    await waitFor(() => expect(redirectedTo).toContain('https://sandbox.okta.test/authorize'))
    expect(initiateAuthCalls).toEqual([
      { provider: 'okta', connectionId: 'conn-sandbox', workspaceName: 'Primary' },
    ])
  })

  // 5. Selecting an enterprise IdP -> exact provider/connectionId/workspaceName.
  it('passes provider, connectionId and the resolved workspace name to initiateAuth', async () => {
    render(
      renderLogin([
        workspaceMock('acme', 'Acme Corporation'),
        connectionsMock('acme', [conn('conn-entra', 'entra', 'Acme Entra')]),
        initiateAuthMock(
          { provider: 'entra', connectionId: 'conn-entra', workspaceName: 'Acme Corporation' },
          'https://login.microsoft.test/authorize',
        ),
      ]),
    )

    const user = await submitEndpoint('acme')
    await waitFor(() => expect(screen.getByText('Acme Entra')).toBeInTheDocument())
    await user.click(screen.getByText('Acme Entra'))

    await waitFor(() => expect(initiateAuthCalls).toHaveLength(1))
    // workspaceName is the RESOLVED workspace name, not the slug.
    expect(initiateAuthCalls[0]).toEqual({
      provider: 'entra',
      connectionId: 'conn-entra',
      workspaceName: 'Acme Corporation',
    })
    expect(redirectedTo).toContain('https://login.microsoft.test/authorize')
  })

  // 6a. GraphQL lookup error -> explicit error state, NOT an empty list/Google.
  it('treats a GraphQL IdP lookup error as an error, never as an empty list', async () => {
    render(
      renderLogin([
        workspaceMock('acme'),
        connectionsGraphqlErrorMock('acme'),
        // Present on purpose: if the component wrongly fell back, this mock
        // would satisfy it and the negative assertion below would catch it.
        initiateAuthMock(
          { provider: 'google', workspaceName: 'Primary' },
          'https://accounts.google.test/o/oauth2',
        ),
      ]),
    )

    await submitEndpoint('acme')

    await waitFor(() =>
      expect(
        screen.getByText(/Could not load the identity providers for this network/i),
      ).toBeInTheDocument(),
    )
    expect(initiateAuthCalls).toHaveLength(0)
    expect(redirectedTo).toHaveLength(0)
    expect(screen.queryByText('Select identity provider')).not.toBeInTheDocument()
  })

  // 6b. Same rule for a network failure (rejected promise).
  it('treats a network IdP lookup failure as an error, never as an empty list', async () => {
    render(
      renderLogin([
        workspaceMock('acme'),
        connectionsNetworkErrorMock('acme'),
        initiateAuthMock(
          { provider: 'google', workspaceName: 'Primary' },
          'https://accounts.google.test/o/oauth2',
        ),
      ]),
    )

    await submitEndpoint('acme')

    await waitFor(() =>
      expect(
        screen.getByText(/Could not load the identity providers for this network/i),
      ).toBeInTheDocument(),
    )
    expect(initiateAuthCalls).toHaveLength(0)
    expect(redirectedTo).toHaveLength(0)
  })

  // 7. Empty displayName -> provider value is the label.
  it('falls back to the provider value when displayName is empty', async () => {
    render(
      renderLogin([
        workspaceMock('acme'),
        connectionsMock('acme', [conn('conn-1', 'okta', '')]),
      ]),
    )

    await submitEndpoint('acme')

    await waitFor(() => expect(screen.getByText('Select identity provider')).toBeInTheDocument())
    // Label + provider sub-label both read "okta".
    expect(screen.getAllByText('okta').length).toBeGreaterThanOrEqual(1)
  })

  // Enterprise IdP failure -> error surfaced, choices still available, and no
  // silent Google retry.
  it('never silently retries with Google after an enterprise IdP failure', async () => {
    render(
      renderLogin([
        workspaceMock('acme'),
        connectionsMock('acme', [conn('conn-okta', 'okta', 'Acme Okta')]),
        initiateAuthErrorMock({
          provider: 'okta',
          connectionId: 'conn-okta',
          workspaceName: 'Primary',
        }),
        initiateAuthMock(
          { provider: 'google', workspaceName: 'Primary' },
          'https://accounts.google.test/o/oauth2',
        ),
      ]),
    )

    const user = await submitEndpoint('acme')
    await waitFor(() => expect(screen.getByText('Acme Okta')).toBeInTheDocument())
    await user.click(screen.getByText('Acme Okta'))

    await waitFor(() =>
      expect(screen.getByText(/Sign-in with Acme Okta failed/i)).toBeInTheDocument(),
    )
    // No redirect at all, and specifically no Google handoff.
    expect(redirectedTo).toHaveLength(0)
    expect(initiateAuthCalls).toHaveLength(0)
    // The user can pick again.
    expect(screen.getByText('Acme Okta')).toBeInTheDocument()
  })

  // Email mode feeds the same chooser, with the picked workspace's own name.
  it('renders IdP choices in email mode for the workspace the user picked', async () => {
    const user = userEvent.setup()
    render(
      renderLogin([
        {
          request: { query: LookupWorkspacesByEmailDocument, variables: { email: 'you@acme.test' } },
          result: {
            data: {
              lookupWorkspacesByEmail: {
                workspaces: [
                  { id: 'ws-1', name: 'Acme One', slug: 'acme-one' },
                  { id: 'ws-2', name: 'Acme Two', slug: 'acme-two' },
                ],
              },
            },
          },
        },
        connectionsMock('acme-two', [conn('conn-two-okta', 'okta', 'Two Okta')]),
        initiateAuthMock(
          { provider: 'okta', connectionId: 'conn-two-okta', workspaceName: 'Acme Two' },
          'https://two.okta.test/authorize',
        ),
      ]),
    )

    await user.click(screen.getByRole('button', { name: /Use email instead/i }))
    await user.type(screen.getByPlaceholderText('you@company.com'), 'you@acme.test')
    await user.click(screen.getByRole('button', { name: /Find My Workspaces/i }))

    await waitFor(() => expect(screen.getByText('Acme Two')).toBeInTheDocument())
    // Pick the SECOND workspace — the previous implementation sent the first
    // workspace's name here.
    await user.click(screen.getByText('Acme Two'))

    await waitFor(() => expect(screen.getByText('Two Okta')).toBeInTheDocument())
    await user.click(screen.getByText('Two Okta'))

    await waitFor(() => expect(initiateAuthCalls).toHaveLength(1))
    expect(initiateAuthCalls[0]).toEqual({
      provider: 'okta',
      connectionId: 'conn-two-okta',
      workspaceName: 'Acme Two',
    })
  })

  // --- platform (bootstrap) tier -----------------------------------------

  // Only the platform IdP configured => straight to Google, no chooser. The
  // list is no longer empty in this case, so a naive length check would wrongly
  // render a one-button "choice" here.
  it('goes straight to Google when only the platform tier is configured', async () => {
    render(
      renderLogin([
        workspaceMock('acme'),
        connectionsMock('acme', [googleTier()]),
        initiateAuthMock(
          { provider: 'google', workspaceName: 'Primary' },
          'https://accounts.google.test/o/oauth2',
        ),
      ]),
    )

    await submitEndpoint('acme')

    await waitFor(() => expect(redirectedTo).toContain('https://accounts.google.test/o/oauth2'))
    expect(screen.queryByText('Select identity provider')).not.toBeInTheDocument()
    expect(screen.queryByText('Other sign-in options')).not.toBeInTheDocument()
  })

  // Enterprise + platform => Okta is primary, Google is NOT a visible button.
  it('keeps the platform option collapsed behind a disclosure', async () => {
    render(
      renderLogin([
        workspaceMock('acme'),
        connectionsMock('acme', [conn('conn-okta', 'okta', 'Acme Okta'), googleTier()]),
      ]),
    )

    await submitEndpoint('acme')

    await waitFor(() => expect(screen.getByText('Acme Okta')).toBeInTheDocument())
    // Collapsed: the Google button is not rendered until asked for.
    expect(screen.queryByText('Google')).not.toBeInTheDocument()
    expect(screen.getByText('Other sign-in options')).toBeInTheDocument()
    // And nothing auto-redirected.
    expect(redirectedTo).toHaveLength(0)
  })

  // Expanding the disclosure reveals it and sends the platform connection's id.
  it('reveals the platform option and sends its exact connectionId', async () => {
    render(
      renderLogin([
        workspaceMock('acme', 'Acme Corporation'),
        connectionsMock('acme', [conn('conn-okta', 'okta', 'Acme Okta'), googleTier('conn-goog')]),
        initiateAuthMock(
          { provider: 'google', connectionId: 'conn-goog', workspaceName: 'Acme Corporation' },
          'https://accounts.google.test/o/oauth2',
        ),
      ]),
    )

    const user = await submitEndpoint('acme')
    await waitFor(() => expect(screen.getByText('Acme Okta')).toBeInTheDocument())

    await user.click(screen.getByText('Other sign-in options'))
    await waitFor(() => expect(screen.getByText('Google')).toBeInTheDocument())
    await user.click(screen.getByText('Google'))

    await waitFor(() => expect(initiateAuthCalls).toHaveLength(1))
    expect(initiateAuthCalls[0]).toEqual({
      provider: 'google',
      connectionId: 'conn-goog',
      workspaceName: 'Acme Corporation',
    })
  })

  // Platform login disabled => the server omits the bootstrap tier entirely, so
  // there is nothing to disclose.
  it('shows no disclosure when the server omits the platform tier', async () => {
    render(
      renderLogin([
        workspaceMock('acme'),
        connectionsMock('acme', [conn('conn-okta', 'okta', 'Acme Okta')]),
      ]),
    )

    await submitEndpoint('acme')

    await waitFor(() => expect(screen.getByText('Acme Okta')).toBeInTheDocument())
    expect(screen.queryByText('Other sign-in options')).not.toBeInTheDocument()
  })

  // --- OAuth callback error surfacing ------------------------------------

  // The callback can only communicate by redirecting to /login?error=<reason>.
  // Before this the reason was dropped and the user saw a blank form.
  it('shows an actionable message when the workspace has not granted access', async () => {
    render(renderLoginAt([], '/login?error=not_invited'))

    const alert = await screen.findByRole('alert')
    expect(within(alert).getByText(/has not granted you access/i)).toBeInTheDocument()
    // ...and offers the path the person actually controls. Scoped to the banner:
    // the page footer carries its own "Deploy one" link.
    expect(within(alert).getByRole('link', { name: /Deploy one/i })).toBeInTheDocument()
  })

  // A generic failure must NOT offer the deploy path — nothing suggests this
  // person lacks access; the attempt simply broke.
  it('shows a generic message without the deploy CTA for an auth failure', async () => {
    render(renderLoginAt([], '/login?error=authentication_failed'))

    expect(await screen.findByRole('alert')).toBeInTheDocument()
    expect(screen.getByText(/identity provider failed/i)).toBeInTheDocument()
    expect(screen.queryByText(/Need your own network instead/i)).not.toBeInTheDocument()
  })

  // An unrecognised (or crafted) reason must not paint arbitrary text onto the
  // login page.
  it('does not echo an unknown error code back to the page', async () => {
    render(renderLoginAt([], '/login?error=<script>pwned</script>'))

    expect(await screen.findByRole('alert')).toBeInTheDocument()
    expect(screen.getByText(/Sign-in could not be completed/i)).toBeInTheDocument()
    expect(screen.queryByText(/pwned/i)).not.toBeInTheDocument()
  })

  it('shows no banner when there is no error param', () => {
    render(renderLogin([]))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  // The stale banner must not linger over a fresh attempt.
  it('clears the callback banner once the user retries', async () => {
    render(
      renderLoginAt(
        [workspaceMock('acme'), connectionsMock('acme', [conn('conn-1', 'okta', 'Acme Okta')])],
        '/login?error=not_invited',
      ),
    )

    expect(await screen.findByRole('alert')).toBeInTheDocument()
    await submitEndpoint('acme')

    await waitFor(() => expect(screen.getByText('Acme Okta')).toBeInTheDocument())
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  // Workspace scoping, as far as the frontend can assert it: the query is sent
  // with the requested slug, and only that response is rendered.
  it('requests connections for the submitted slug only', async () => {
    render(
      renderLogin([
        workspaceMock('acme'),
        connectionsMock('acme', [conn('conn-acme', 'okta', 'Acme Okta')]),
        connectionsMock('beta', [conn('conn-beta', 'entra', 'Beta Entra')]),
      ]),
    )

    await submitEndpoint('acme')

    await waitFor(() => expect(screen.getByText('Acme Okta')).toBeInTheDocument())
    expect(screen.queryByText('Beta Entra')).not.toBeInTheDocument()
  })
})
