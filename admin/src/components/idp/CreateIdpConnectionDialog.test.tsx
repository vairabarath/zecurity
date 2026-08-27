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
    expect(screen.getByLabelText('OIDC Issuer URL')).toBeInTheDocument()
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

    await user.type(screen.getByLabelText('OIDC Issuer URL'), 'https://acme.okta.com')
    expect(submitBtn).toBeDisabled()

    await user.type(screen.getByLabelText('Client ID'), 'client-123')
    expect(submitBtn).toBeDisabled()

    await user.type(screen.getByLabelText('Client Secret'), 'super-secret')
    await waitFor(() => expect(submitBtn).toBeEnabled())
  })

  it('toggles optional advanced settings', async () => {
    const user = userEvent.setup()
    render(
      <CreateIdpConnectionDialog
        open={true}
        onClose={vi.fn()}
        onSuccess={vi.fn()}
      />,
    )

    expect(screen.queryByLabelText('Discovery URL (optional)')).not.toBeInTheDocument()

    const toggle = screen.getByRole('button', { name: /Advanced settings/i })
    await user.click(toggle)

    expect(screen.getByLabelText('Discovery URL (optional)')).toBeInTheDocument()
    expect(screen.getByLabelText('Scopes')).toHaveValue('openid email profile')
    expect(screen.getByLabelText('Domain Hint (optional)')).toBeInTheDocument()
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
})
