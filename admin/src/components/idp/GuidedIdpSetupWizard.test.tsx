import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { GuidedIdpSetupWizard } from './GuidedIdpSetupWizard'

// The wizard composes components that call useMutation / useQuery at the top
// level and uses useNavigate, so we mock Apollo and wrap in a Router. The tests
// exercise the wizard's own step orchestration, not the sub-component submits
// (those have their own suites: CreateIdpConnectionDialog.test / ScimConfigCard
// usage on the detail page).
//
// The create mutation resolves successfully so Step 1 can advance to Step 2.
const createResolve = { data: { createIdpConnection: { id: 'new-conn-1' } } }
vi.mock('@apollo/client/react', () => ({
  useMutation: () => [vi.fn().mockResolvedValue(createResolve), { loading: false }],
  useQuery: () => ({ data: undefined, loading: false, error: undefined, refetch: vi.fn() }),
}))

function renderWizard(onClose: () => void = vi.fn(), initialProvider?: 'okta' | 'entra') {
  return render(
    <MemoryRouter>
      <GuidedIdpSetupWizard open={true} onClose={onClose} initialProvider={initialProvider} />
    </MemoryRouter>,
  )
}

describe('GuidedIdpSetupWizard', () => {
  it('renders Step 1 (the embedded create dialog) when opened', () => {
    renderWizard()
    expect(screen.getByText('Add Identity Provider')).toBeInTheDocument()
    expect(
      screen.getByText(/provide the OIDC connection details, then continue to SCIM configuration/i),
    ).toBeInTheDocument()
  })

  it('calls onClose when Step 1 is cancelled (direct path untouched)', async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    renderWizard(onClose)
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(onClose).toHaveBeenCalled()
  })

  it('advances to Step 2 after a successful create, exposing the Skip SCIM option', async () => {
    const user = userEvent.setup()
    renderWizard()

    await user.type(screen.getByLabelText('Display Name'), 'Corporate Okta')
    // Step 1 defaults to provider=okta (no initialProvider passed here), so the
    // issuer field is labelled "Okta Domain" (Twingate-mirror), not the
    // generic "OIDC Issuer URL" label used by non-Okta providers.
    await user.type(screen.getByLabelText('Okta Domain'), 'https://acme.okta.com')
    await user.type(screen.getByLabelText('Client ID'), 'client-123')
    await user.type(screen.getByLabelText('Client Secret'), 'super-secret')

    await user.click(screen.getByRole('button', { name: 'Create Connection' }))

    // Step 2 carries the SCIM-mapping card and a Skip option.
    await waitFor(() => expect(screen.getByText('Skip SCIM')).toBeInTheDocument())
    expect(screen.getByText(/configure the identity mapping, then enable SCIM/i)).toBeInTheDocument()
  })

  // Twingate-parity entry point: the AddIdentityProviderMenu popup on
  // IdpConnections.tsx passes the chosen provider straight into this wizard,
  // so Step 1 must show the provider-specific "Connect Okta" step, not the
  // generic form — proves the merged single-entry-point flow actually wires
  // the popup selection all the way through.
  it('shows the provider-specific "Connect Okta" step when opened with initialProvider="okta"', () => {
    renderWizard(vi.fn(), 'okta')
    expect(screen.getByText('Connect Okta')).toBeInTheDocument()
    expect(screen.queryByLabelText('Provider')).not.toBeInTheDocument()
  })
})
