import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { CreateIdpConnectionDialog } from './CreateIdpConnectionDialog'

vi.mock('@apollo/client/react', () => ({
  useMutation: () => [vi.fn(), { loading: false }],
}))

describe('CreateIdpConnectionDialog', () => {
  it('renders form fields with required inputs and masked secret', () => {
    render(
      <CreateIdpConnectionDialog
        open={true}
        onClose={vi.fn()}
        onSuccess={vi.fn()}
      />,
    )

    expect(screen.getByText('Add Identity Provider')).toBeInTheDocument()
    expect(screen.getByLabelText('Provider')).toBeInTheDocument()
    expect(screen.getByLabelText('Display Name')).toBeInTheDocument()
    // Default provider is Okta, so the issuer field is labelled "Okta Domain"
    // (Twingate-mirror) and the Advanced settings block is hidden.
    expect(screen.getByLabelText('Okta Domain')).toBeInTheDocument()
    expect(screen.queryByLabelText('OIDC Issuer URL')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Advanced settings/i })).not.toBeInTheDocument()
    expect(screen.getByLabelText('Client ID')).toBeInTheDocument()

    const secretInput = screen.getByLabelText('Client Secret')
    expect(secretInput).toBeInTheDocument()
    expect(secretInput).toHaveAttribute('type', 'password')
  })

  it('disables submit button until all required fields are populated', async () => {
    const user = userEvent.setup()
    render(
      <CreateIdpConnectionDialog
        open={true}
        onClose={vi.fn()}
        onSuccess={vi.fn()}
      />,
    )

    const submitBtn = screen.getByRole('button', { name: 'Create Connection' })
    expect(submitBtn).toBeDisabled()

    await user.type(screen.getByLabelText('Display Name'), 'Corporate Okta')
    expect(submitBtn).toBeDisabled()

    await user.type(screen.getByLabelText('Okta Domain'), 'https://acme.okta.com')
    expect(submitBtn).toBeDisabled()

    await user.type(screen.getByLabelText('Client ID'), 'client-123')
    expect(submitBtn).toBeDisabled()

    await user.type(screen.getByLabelText('Client Secret'), 'super-secret')
    await waitFor(() => expect(submitBtn).toBeEnabled())
  })

  it('toggles optional advanced settings for a non-Okta provider (entra)', async () => {
    const user = userEvent.setup()
    render(
      <CreateIdpConnectionDialog
        open={true}
        initialProvider="entra"
        onClose={vi.fn()}
        onSuccess={vi.fn()}
      />,
    )

    // Non-Okta providers keep the generic OIDC form: issuer labelled normally
    // and the Advanced settings block is available.
    expect(screen.getByLabelText('OIDC Issuer URL')).toBeInTheDocument()
    expect(screen.queryByLabelText('Discovery URL (optional)')).not.toBeInTheDocument()

    const toggle = screen.getByRole('button', { name: /Advanced settings/i })
    await user.click(toggle)

    expect(screen.getByLabelText('Discovery URL (optional)')).toBeInTheDocument()
    expect(screen.getByLabelText('Scopes')).toHaveValue('openid email profile')
    expect(screen.getByLabelText('Domain Hint (optional)')).toBeInTheDocument()
  })

  // Twingate-mirror: the Okta "Connect Okta" step must NOT surface Advanced
  // settings, matching Twingate's own three-field (Okta Domain / Client ID /
  // Client Secret) dialog.
  it('hides advanced settings on the Okta "Connect Okta" step', () => {
    render(
      <CreateIdpConnectionDialog
        open={true}
        initialProvider="okta"
        onClose={vi.fn()}
        onSuccess={vi.fn()}
      />,
    )

    expect(screen.getByLabelText('Okta Domain')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Advanced settings/i })).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Discovery URL (optional)')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Scopes')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Domain Hint (optional)')).not.toBeInTheDocument()
  })

  it('calls onClose when Cancel button is clicked', async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    render(
      <CreateIdpConnectionDialog
        open={true}
        onClose={onClose}
        onSuccess={vi.fn()}
      />,
    )

    const cancelBtn = screen.getByRole('button', { name: 'Cancel' })
    await user.click(cancelBtn)
    expect(onClose).toHaveBeenCalled()
  })

  // Twingate-style "picker step already chose the provider" behavior: when
  // initialProvider is set, the dialog behaves like a per-provider "Connect
  // Okta" step, not a generic form with an editable provider select.
  it('shows a provider-specific title and a static (non-editable) provider label when initialProvider is set', () => {
    render(
      <CreateIdpConnectionDialog
        open={true}
        initialProvider="okta"
        onClose={vi.fn()}
        onSuccess={vi.fn()}
      />,
    )

    expect(screen.getByText('Connect Okta')).toBeInTheDocument()
    expect(screen.queryByLabelText('Provider')).not.toBeInTheDocument()
    expect(screen.getByText('Okta', { selector: 'div' })).toBeInTheDocument()
  })

  it('still shows the generic title and editable Provider select when initialProvider is omitted', () => {
    render(
      <CreateIdpConnectionDialog
        open={true}
        onClose={vi.fn()}
        onSuccess={vi.fn()}
      />,
    )

    expect(screen.getByText('Add Identity Provider')).toBeInTheDocument()
    expect(screen.getByLabelText('Provider')).toBeInTheDocument()
  })
})
