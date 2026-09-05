import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

// The dialog must describe EXACTLY what the server verifies on create — the
// issuer's OIDC discovery document — and must never imply that the OAuth client
// ID, client secret or redirect URI were checked. createIdpConnection validates
// discovery before persisting; discovery is an unauthenticated endpoint, so no
// credential is ever exercised.
//
// Own mocks (rather than extending the sibling test file) so the success path
// can be driven and the toast wording asserted.

const createMock = vi.fn()
const toastSuccess = vi.fn()

vi.mock('@apollo/client/react', () => ({
  useMutation: () => [createMock, { loading: false }],
}))
vi.mock('sonner', () => ({
  toast: {
    success: (...args: unknown[]) => toastSuccess(...args),
    error: vi.fn(),
  },
}))

const { CreateIdpConnectionDialog } = await import('./CreateIdpConnectionDialog')

// The overclaim to guard against is pairing a CREDENTIAL noun with a
// verified/valid predicate — "client secret verified", "credentials are valid".
// Blacklisting whole phrases would be brittle: "OIDC discovery verified" is
// honest and must stay allowed, so the assertion targets the pattern instead of
// a phrase list. Each credential noun is checked against each predicate within
// a short window, so unrelated sentences do not collide.
const CREDENTIAL_NOUNS = ['client secret', 'client id', 'credential', 'password', 'redirect uri']
const VERIFIED_PREDICATES = ['verified', 'is valid', 'are valid', 'validated', 'confirmed']

// expectNoCredentialVerificationClaim fails if any credential noun appears
// within 40 characters of a verified/valid predicate — close enough to read as
// a claim about that credential. Negations ("not verified until the first
// sign-in") are excluded, since that is exactly the honest wording we require.
function expectNoCredentialVerificationClaim(text: string) {
  const haystack = text.toLowerCase()
  for (const noun of CREDENTIAL_NOUNS) {
    let from = 0
    for (;;) {
      const at = haystack.indexOf(noun, from)
      if (at === -1) break
      const window = haystack.slice(at, at + noun.length + 40)
      for (const predicate of VERIFIED_PREDICATES) {
        const hit = window.indexOf(predicate)
        if (hit === -1) continue
        // Allow explicit negations — "are not verified", "never verified".
        const between = window.slice(noun.length, hit)
        if (/\b(not|never|aren.t|isn.t|no)\b/.test(between)) continue
        throw new Error(
          `copy claims a credential was verified: "${noun}" near "${predicate}" in ${JSON.stringify(window)}`,
        )
      }
      from = at + noun.length
    }
  }
}

function fillRequiredFields(user: ReturnType<typeof userEvent.setup>) {
  return (async () => {
    await user.type(screen.getByLabelText('Display Name'), 'Corporate Okta')
    await user.type(screen.getByLabelText('Okta Domain'), 'https://acme.okta.com')
    await user.type(screen.getByLabelText('Client ID'), 'client-123')
    await user.type(screen.getByLabelText('Client Secret'), 'super-secret')
  })()
}

describe('CreateIdpConnectionDialog verification honesty', () => {
  beforeEach(() => {
    createMock.mockReset()
    toastSuccess.mockReset()
  })

  // Meta-test: a guard that cannot fail is worse than no guard. This pins that
  // expectNoCredentialVerificationClaim actually catches an overclaim, and
  // still permits both the honest negation and "OIDC discovery verified".
  it('has a credential-overclaim guard that actually detects overclaims', () => {
    expect(() =>
      expectNoCredentialVerificationClaim('Your client secret has been verified.'),
    ).toThrow(/claims a credential was verified/)
    expect(() =>
      expectNoCredentialVerificationClaim('Okta credentials are valid.'),
    ).toThrow(/claims a credential was verified/)

    // Honest wording must pass.
    expect(() =>
      expectNoCredentialVerificationClaim(
        'The client ID, client secret and redirect URI are not verified until the first sign-in.',
      ),
    ).not.toThrow()
    expect(() =>
      expectNoCredentialVerificationClaim('Connection created — OIDC discovery verified.'),
    ).not.toThrow()
  })

  it('states that discovery is validated on save and that credentials are not', () => {
    render(
      <CreateIdpConnectionDialog
        open={true}
        initialProvider="okta"
        onClose={vi.fn()}
        onSuccess={vi.fn()}
      />,
    )

    expect(
      screen.getByText(/serves a valid OpenID Connect\s+discovery document/i),
    ).toBeInTheDocument()
    expect(
      screen.getByText(/not verified until the first sign-in/i),
    ).toBeInTheDocument()
  })

  it('never claims the client ID or client secret was verified', () => {
    render(
      <CreateIdpConnectionDialog
        open={true}
        initialProvider="okta"
        onClose={vi.fn()}
        onSuccess={vi.fn()}
      />,
    )

    expectNoCredentialVerificationClaim(document.body.textContent ?? '')
  })

  it('reports OIDC discovery — not credentials — as what was verified on success', async () => {
    const user = userEvent.setup()
    createMock.mockResolvedValue({ data: { createIdpConnection: { id: 'conn-1' } } })
    const onSuccess = vi.fn()

    render(
      <CreateIdpConnectionDialog
        open={true}
        initialProvider="okta"
        onClose={vi.fn()}
        onSuccess={onSuccess}
      />,
    )

    await fillRequiredFields(user)
    await user.click(screen.getByRole('button', { name: 'Create Connection' }))

    await waitFor(() => expect(onSuccess).toHaveBeenCalledWith('conn-1'))

    expect(toastSuccess).toHaveBeenCalledTimes(1)
    const msg = String(toastSuccess.mock.calls[0][0])
    // It must name what WAS proven...
    expect(msg.toLowerCase()).toContain('discovery')
    // ...and must not imply a credential was checked.
    expectNoCredentialVerificationClaim(msg)
  })

  it('surfaces the server refusal and does not report success when discovery fails', async () => {
    const user = userEvent.setup()
    // Shape of the real refusal: createIdpConnection returns a user-safe error
    // and persists nothing.
    createMock.mockRejectedValue(
      new Error(
        'OIDC discovery failed for issuer "https://acme.okta.com": fetch discovery: no such host. ' +
          'The connection was NOT created.',
      ),
    )
    const onSuccess = vi.fn()

    render(
      <CreateIdpConnectionDialog
        open={true}
        initialProvider="okta"
        onClose={vi.fn()}
        onSuccess={onSuccess}
      />,
    )

    await fillRequiredFields(user)
    await user.click(screen.getByRole('button', { name: 'Create Connection' }))

    await waitFor(() =>
      expect(screen.getByText(/OIDC discovery failed/i)).toBeInTheDocument(),
    )
    expect(screen.getByText('Could not create connection')).toBeInTheDocument()
    // The wizard must not advance and no success must be announced.
    expect(onSuccess).not.toHaveBeenCalled()
    expect(toastSuccess).not.toHaveBeenCalled()
  })
})
