import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AddIdentityProviderMenu } from './AddIdentityProviderMenu'

describe('AddIdentityProviderMenu', () => {
  it('renders the trigger button, closed, with no provider list visible yet', () => {
    render(<AddIdentityProviderMenu onSelect={vi.fn()} />)
    expect(screen.getByRole('button', { name: /Add Identity Provider/i })).toBeInTheDocument()
    expect(screen.queryByText('Okta')).not.toBeInTheDocument()
  })

  it('opens the provider popup on click and lists every supported provider', async () => {
    const user = userEvent.setup()
    render(<AddIdentityProviderMenu onSelect={vi.fn()} />)

    await user.click(screen.getByRole('button', { name: /Add Identity Provider/i }))

    for (const label of ['Okta', 'Microsoft Entra ID', 'JumpCloud', 'Keycloak', 'Generic OIDC']) {
      expect(screen.getByText(label)).toBeInTheDocument()
    }
  })

  it('calls onSelect with "okta" when Okta is chosen from the popup', async () => {
    const user = userEvent.setup()
    const onSelect = vi.fn()
    render(<AddIdentityProviderMenu onSelect={onSelect} />)

    await user.click(screen.getByRole('button', { name: /Add Identity Provider/i }))
    await user.click(screen.getByText('Okta'))

    expect(onSelect).toHaveBeenCalledWith('okta')
  })
})
