import { describe, expect, it } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { MockedProvider } from '@apollo/client/testing/react'
import ResourceDetail from './ResourceDetail'
import { GetAllResourcesDocument, GetShieldsDocument, ShieldStatus } from '@/generated/graphql'

const RN = 'rn-1'
const HOST = '192.168.1.41'

function resource(shield: null | { id: string; name: string; status: ShieldStatus; lanIp: string }) {
  return {
    __typename: 'Resource',
    id: 'res-1',
    name: 'web',
    description: null,
    host: HOST,
    protocol: 'tcp',
    portFrom: 8090,
    portTo: 8090,
    status: 'unprotected',
    errorMessage: null,
    appliedAt: null,
    lastVerifiedAt: null,
    createdAt: '2026-10-22T00:00:00Z',
    shield: shield && { __typename: 'Shield', ...shield },
    remoteNetwork: { __typename: 'RemoteNetwork', id: RN, name: 'k2-net' },
    groups: [],
  }
}

function shieldRow(id: string, status: ShieldStatus, lanIp: string) {
  return {
    __typename: 'Shield',
    id,
    name: `shield-${id}`,
    status,
    lanIp,
    lastSeenAt: null,
    version: null,
    hostname: null,
    interfaceAddr: null,
    connectorId: null,
  }
}

function renderDetail(res: ReturnType<typeof resource>, shields: ReturnType<typeof shieldRow>[]) {
  const mocks = [
    {
      request: { query: GetAllResourcesDocument },
      result: { data: { allResources: [res] } },
      maxUsageCount: Number.POSITIVE_INFINITY,
    },
    {
      request: { query: GetShieldsDocument, variables: { remoteNetworkId: RN } },
      result: { data: { shields } },
      maxUsageCount: Number.POSITIVE_INFINITY,
    },
  ]
  return render(
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    <MockedProvider mocks={mocks as any}>
      <MemoryRouter initialEntries={['/resources/res-1']}>
        <Routes>
          <Route path="/resources/:resourceId" element={<ResourceDetail />} />
        </Routes>
      </MemoryRouter>
    </MockedProvider>,
  )
}

const protectButtons = () =>
  screen.getAllByRole('button', { name: /protect this resource/i })

describe('ResourceDetail protect gate (Fix 10)', () => {
  it('a resource created before its shield can be protected once an active shield runs on its host', async () => {
    renderDetail(resource(null), [shieldRow('s1', ShieldStatus.Active, HOST)])
    await screen.findByRole('heading', { name: 'web' })
    await waitFor(() => {
      for (const b of protectButtons()) expect(b).toBeEnabled()
    })
    expect(screen.getAllByText(/shield-s1/).length).toBeGreaterThan(0)
  })

  it('stays disabled when no active shield is on the host', async () => {
    renderDetail(resource(null), [shieldRow('s2', ShieldStatus.Disconnected, HOST)])
    await screen.findByRole('heading', { name: 'web' })
    await waitFor(() => {
      for (const b of protectButtons()) expect(b).toBeDisabled()
    })
    expect(screen.getAllByText(/shield-s2/).length).toBeGreaterThan(0)
  })

  it('a revoked bound shield does not block a new active shield on the host', async () => {
    renderDetail(
      resource({ id: 'old', name: 'shield-old', status: ShieldStatus.Revoked, lanIp: HOST }),
      [shieldRow('old', ShieldStatus.Revoked, HOST), shieldRow('new', ShieldStatus.Active, HOST)],
    )
    await screen.findByRole('heading', { name: 'web' })
    await waitFor(() => {
      for (const b of protectButtons()) expect(b).toBeEnabled()
    })
    expect(screen.getAllByText(/shield-new/).length).toBeGreaterThan(0)
  })
})
