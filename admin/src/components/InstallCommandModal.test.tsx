import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MockedProvider } from '@apollo/client/testing/react'
import type { MockedResponse } from '@apollo/client/testing'
import { MemoryRouter } from 'react-router-dom'
import { InstallCommandModal } from './InstallCommandModal'
import {
  ConnectorStatus,
  GenerateShieldTokenDocument,
  GetRemoteNetworksDocument,
} from '@/generated/graphql'

const connector = (id: string, status: ConnectorStatus) => ({
  __typename: 'Connector' as const,
  id,
  name: id,
  status,
  lastSeenAt: null,
  version: null,
  hostname: null,
  publicIp: null,
  lanAddr: null,
  certNotAfter: null,
  createdAt: '2026-11-16T00:00:00Z',
})

const network = (id: string, name: string, connectors: ReturnType<typeof connector>[]) => ({
  __typename: 'RemoteNetwork' as const,
  id,
  name,
  location: 'OFFICE',
  status: 'ACTIVE',
  createdAt: '2026-11-16T00:00:00Z',
  networkHealth: 'HEALTHY',
  connectors,
  shields: [],
})

const networksMock: MockedResponse = {
  request: { query: GetRemoteNetworksDocument },
  result: {
    data: {
      remoteNetworks: [
        network('n-pending', 'New LAN', [connector('c1', ConnectorStatus.Pending)]),
        network('n-online', 'Office LAN', [connector('c2', ConnectorStatus.Active)]),
      ],
    },
  },
  maxUsageCount: 5,
}

function renderShieldModal(extra: MockedResponse[] = []) {
  const onClose = vi.fn()
  render(
    <MockedProvider mocks={[networksMock, ...extra]}>
      <MemoryRouter>
        <InstallCommandModal variant="shield" open onClose={onClose} />
      </MemoryRouter>
    </MockedProvider>,
  )
  return { onClose }
}

describe('InstallCommandModal (shield)', () => {
  it('warns about a network with no connector online and keeps Add Shield off', async () => {
    const user = userEvent.setup()
    renderShieldModal()
    await user.type(screen.getByLabelText('Shield Name *'), 'db-01')
    const select = await screen.findByRole('combobox')
    await waitFor(() => expect(screen.getByRole('option', { name: 'New LAN' })).toBeInTheDocument())

    await user.selectOptions(select, 'n-pending')
    expect(screen.getByRole('alert')).toHaveTextContent('New LAN has no connector online yet')
    expect(screen.getByRole('button', { name: /Add Shield/ })).toBeDisabled()

    await user.selectOptions(select, 'n-online')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Add Shield/ })).toBeEnabled()
  })

  it('stays open with an error when no shield comes back', async () => {
    const user = userEvent.setup()
    const { onClose } = renderShieldModal([
      {
        request: {
          query: GenerateShieldTokenDocument,
          variables: { remoteNetworkId: 'n-online', shieldName: 'db-01' },
        },
        result: { data: { generateShieldToken: { __typename: 'ShieldToken', shieldId: '' } } },
      },
    ])
    await user.type(screen.getByLabelText('Shield Name *'), 'db-01')
    await waitFor(() => expect(screen.getByRole('option', { name: 'Office LAN' })).toBeInTheDocument())
    await user.selectOptions(screen.getByRole('combobox'), 'n-online')
    await user.click(screen.getByRole('button', { name: /Add Shield/ }))
    expect(await screen.findByText(/The shield was not created/)).toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
  })
})
