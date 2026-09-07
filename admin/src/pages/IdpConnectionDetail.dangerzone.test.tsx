import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { MockedProvider } from '@apollo/client/testing/react'
import {
  GetIdpConnectionsDocument,
  SetIdpConnectionStatusDocument,
} from '@/generated/graphql'
import IdpConnectionDetail from './IdpConnectionDetail'

// The SCIM cards each run their own queries; they are not what this test is
// about, and unmocked queries would only add noise.
vi.mock('@/components/scim/ScimConfigCard', () => ({ ScimConfigCard: () => null }))
vi.mock('@/components/scim/ScimBaseUrlBox', () => ({ ScimBaseUrlBox: () => null }))
vi.mock('@/components/scim/ScimTokenPanel', () => ({ ScimTokenPanel: () => null }))
vi.mock('@/components/scim/IdentityHealthBadge', () => ({ IdentityHealthBadge: () => null }))

const ID = '731ef05c-6671-4337-9ad9-9f661eac1c3c'

function connection(status: string) {
  return {
    __typename: 'IdpConnection',
    id: ID,
    protocol: 'OIDC',
    provider: 'okta',
    displayName: 'scim okta',
    issuer: 'https://trial-3724025.okta.com',
    clientId: 'cid',
    discoveryUrl: null,
    scopes: [],
    domainHint: null,
    status,
    managed: false,
    lastSyncAt: null,
    identityHealth: 'HEALTHY',
    subjectClaim: 'sub',
    scimIdentifier: 'email',
    scimEnabled: false,
  }
}

function renderDetail(mocks: readonly unknown[]) {
  return render(
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    <MockedProvider mocks={mocks as any}>
      <MemoryRouter initialEntries={[`/idp-connections/${ID}`]}>
        <Routes>
          <Route path="/idp-connections/:id" element={<IdpConnectionDetail />} />
        </Routes>
      </MemoryRouter>
    </MockedProvider>,
  )
}

describe('IdpConnectionDetail danger zone', () => {
  it('fires SetIdpConnectionStatus when Disable is clicked and flips the button', async () => {
    const user = userEvent.setup()
    let mutationFired = false

    const mocks = [
      {
        request: { query: GetIdpConnectionsDocument },
        result: { data: { idpConnections: [connection('active')] } },
        maxUsageCount: Number.POSITIVE_INFINITY,
      },
      {
        request: {
          query: SetIdpConnectionStatusDocument,
          variables: { id: ID, status: 'disabled' },
        },
        result: () => {
          mutationFired = true
          return {
            data: {
              setIdpConnectionStatus: {
                __typename: 'IdpConnection',
                id: ID,
                status: 'disabled',
              },
            },
          }
        },
      },
    ]

    renderDetail(mocks)

    const disableButton = await screen.findByRole('button', { name: 'Disable' })
    await user.click(disableButton)

    // The discriminating assertion: the mutation actually reached the link…
    await waitFor(() => expect(mutationFired).toBe(true))
    // …and the derived state re-rendered the button from the normalised cache.
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Disabled' })).toBeDisabled(),
    )
  })

  it('hides the danger zone for platform-managed connections', async () => {
    const mocks = [
      {
        request: { query: GetIdpConnectionsDocument },
        result: {
          data: {
            idpConnections: [{ ...connection('active'), managed: true }],
          },
        },
        maxUsageCount: Number.POSITIVE_INFINITY,
      },
    ]

    renderDetail(mocks)

    expect(await screen.findByText('scim okta')).toBeInTheDocument()
    expect(screen.queryByText('Danger Zone')).not.toBeInTheDocument()
  })
})
