// Viewer sweep (PENDING-19 R2): a read-only role sees every console page with
// data, including pending connectors/shields and transitional resources, and
// no write control on any of them. The gating was applied page by page; this
// pins that none was missed. The server refuses every write regardless.
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import type { DocumentNode } from 'graphql'
import type { ComponentType } from 'react'
import {
  ConnectorStatus,
  GetAllResourcesDocument,
  GetConnectorDocument,
  GetConnectorsDocument,
  GetDeviceProfilesDocument,
  GetDiscoveredServicesDocument,
  GetGroupsDocument,
  GetIdpConnectionsDocument,
  GetRemoteNetworkDocument,
  GetRemoteNetworksDocument,
  GetScimConflictsDocument,
  GetScimProviderProfilesDocument,
  GetScimTokensDocument,
  GetShieldDocument,
  GetShieldsDocument,
  Role,
  ShieldStatus,
} from '@/generated/graphql'
import { setTestRole } from '@/test/roles'
import { useAuthStore } from '@/store/auth'

const data = new Map<DocumentNode, unknown>()
const fetchSpy = vi.fn()

vi.mock('@apollo/client/react', () => ({
  useQuery: (doc: DocumentNode) => ({
    data: data.get(doc),
    loading: false,
    error: undefined,
    refetch: vi.fn(),
    startPolling: vi.fn(),
    stopPolling: vi.fn(),
  }),
  useLazyQuery: () => [vi.fn(), { data: undefined, loading: false }],
  useMutation: () => [vi.fn(), { loading: false }],
}))

const NOW = '2026-11-02T10:00:00Z'

const connector = (id: string, status: ConnectorStatus) => ({
  __typename: 'Connector', id, name: `conn-${id}`, status, lastSeenAt: NOW, version: '1.0',
  hostname: 'h', publicIp: '1.2.3.4', lanAddr: '10.0.0.5', certNotAfter: NOW, createdAt: NOW,
  remoteNetworkId: 'rn-1',
})
const shield = (id: string, status: ShieldStatus) => ({
  __typename: 'Shield', id, name: `shield-${id}`, status, lanIp: '10.0.0.9', lastSeenAt: NOW,
  version: '1.0', hostname: 'sh', interfaceAddr: '10.0.0.9', connectorId: 'c-active',
  certNotAfter: NOW, createdAt: NOW, remoteNetworkId: 'rn-1',
})
const resource = (id: string, status: string, shieldStatus = ShieldStatus.Active) => ({
  __typename: 'Resource', id, name: `res-${id}`, description: null, host: '10.0.0.9',
  protocol: 'tcp', portFrom: 22, portTo: 22, status, errorMessage: null, appliedAt: NOW,
  lastVerifiedAt: NOW, createdAt: NOW,
  shield: { __typename: 'Shield', id: 's-active', name: 'shield-s-active', status: shieldStatus, lanIp: '10.0.0.9' },
  remoteNetwork: { __typename: 'RemoteNetwork', id: 'rn-1', name: 'office' },
  groups: [],
})
const conn = {
  __typename: 'IdpConnection', id: 'idp-1', protocol: 'oidc', provider: 'okta', displayName: 'Okta',
  issuer: 'https://acme.okta.com', clientId: 'cid', discoveryUrl: null, scopes: ['openid'],
  domainHint: null, status: 'active', managed: false, lastSyncAt: NOW, identityHealth: 'Healthy',
  subjectClaim: 'sub', scimIdentifier: 'externalId', scimEnabled: true,
}

beforeEach(() => {
  fetchSpy.mockReset()
  fetchSpy.mockResolvedValue({ ok: false, text: async () => 'refused', json: async () => ({}) })
  vi.stubGlobal('fetch', fetchSpy)
  // The detail pages only mint an install token with a session token present.
  useAuthStore.setState({ accessToken: 'test-access-token' })
  data.clear()
  const connectors = [connector('c-active', ConnectorStatus.Active), connector('c-pending', ConnectorStatus.Pending)]
  const shields = [shield('s-active', ShieldStatus.Active), shield('s-pending', ShieldStatus.Pending)]
  data.set(GetRemoteNetworksDocument, {
    remoteNetworks: [{
      __typename: 'RemoteNetwork', id: 'rn-1', name: 'office', location: 'OFFICE', status: 'ACTIVE',
      createdAt: NOW, networkHealth: 'ONLINE', connectors, shields,
    }],
  })
  data.set(GetRemoteNetworkDocument, { remoteNetwork: { __typename: 'RemoteNetwork', id: 'rn-1', name: 'office', location: 'OFFICE', status: 'ACTIVE' } })
  data.set(GetConnectorsDocument, { connectors })
  data.set(GetConnectorDocument, { connector: connector('c-pending', ConnectorStatus.Pending) })
  data.set(GetShieldDocument, { shield: shield('s-pending', ShieldStatus.Pending) })
  data.set(GetShieldsDocument, { shields })
  data.set(GetAllResourcesDocument, {
    allResources: [resource('r-unprotected', 'pending'), resource('r-protected', 'protected'), resource('r-stuck', 'protecting')],
  })
  data.set(GetGroupsDocument, {
    groups: [{
      __typename: 'Group', id: 'g-1', name: 'ops', description: null, origin: 'manual', connectionId: null,
      createdAt: NOW, updatedAt: NOW, members: [], resources: [],
    }],
  })
  data.set(GetIdpConnectionsDocument, { idpConnections: [conn] })
  data.set(GetDeviceProfilesDocument, {
    deviceProfiles: [{ __typename: 'DeviceProfile', id: 'dp-1', name: 'laptops', manualTrust: false, requirements: [], policies: [] }],
  })
  data.set(GetScimConflictsDocument, {
    scimConflicts: [
      { __typename: 'ScimConflict', id: 'sc-1', workspaceId: 'w', connectionId: 'idp-1', userId: 'u', canonicalKey: 'k1', scimExternalId: null, scimUsernameSnapshot: 'a@x', scimEmailSnapshot: null, status: 'pending', resolutionReason: null, createdAt: NOW, resolvedAt: null },
      { __typename: 'ScimConflict', id: 'sc-2', workspaceId: 'w', connectionId: 'idp-1', userId: 'u', canonicalKey: 'k2', scimExternalId: null, scimUsernameSnapshot: 'b@x', scimEmailSnapshot: null, status: 'rejected', resolutionReason: 'no', createdAt: NOW, resolvedAt: NOW },
    ],
  })
  data.set(GetScimTokensDocument, {
    scimTokens: [{ __typename: 'ScimToken', id: 't-1', workspaceId: 'w', connectionId: 'idp-1', label: 'okta', createdBy: null, createdAt: NOW, lastUsedAt: null, expiresAt: null, revokedAt: null }],
  })
  data.set(GetScimProviderProfilesDocument, { scimProviderProfiles: [] })
  data.set(GetDiscoveredServicesDocument, {
    getDiscoveredServices: [{ __typename: 'DiscoveredService', shieldId: 's-active', protocol: 'tcp', port: 8080, boundIp: '0.0.0.0', serviceName: 'http-alt', firstSeen: NOW, lastSeen: NOW }],
  })
})

const WRITE_CONTROL =
  /^(Add|Create|New|Delete|Force delete|Revoke|Remove|Approve|Protect|Unprotect|Edit|Scan|Assign|Unassign|Mint|Rotate|Invite|Manage role|Accept|Reject|Reopen|Disable|Enable|Save|Grant|Shield$|Connector$|\+)/i

async function load(path: string, route: string, mod: () => Promise<{ default: ComponentType }>) {
  const { default: Page } = await mod()
  render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path={route} element={<Page />} />
      </Routes>
    </MemoryRouter>,
  )
}

const PAGES: [string, string, string, () => Promise<{ default: ComponentType }>][] = [
  ['RemoteNetworks', '/remote-networks', '/remote-networks', () => import('@/pages/RemoteNetworks')],
  ['RemoteNetworkDetail', '/remote-networks/rn-1', '/remote-networks/:id', () => import('@/pages/RemoteNetworkDetail')],
  ['Connectors', '/remote-networks/rn-1/connectors', '/remote-networks/:id/connectors', () => import('@/pages/Connectors')],
  ['Shields', '/remote-networks/rn-1/shields', '/remote-networks/:id/shields', () => import('@/pages/Shields')],
  ['AllConnectors', '/connectors', '/connectors', () => import('@/pages/AllConnectors')],
  ['AllShields', '/shields', '/shields', () => import('@/pages/AllShields')],
  ['ConnectorDetail (pending)', '/connectors/c-pending', '/connectors/:connectorId', () => import('@/pages/ConnectorDetail')],
  ['ShieldDetail (pending)', '/shields/s-pending', '/shields/:shieldId', () => import('@/pages/ShieldDetail')],
  ['Resources', '/resources', '/resources', () => import('@/pages/Resources')],
  ['ResourceDetail (unprotected)', '/resources/r-unprotected', '/resources/:resourceId', () => import('@/pages/ResourceDetail')],
  ['ResourceDetail (protected)', '/resources/r-protected', '/resources/:resourceId', () => import('@/pages/ResourceDetail')],
  ['ResourceDetail (stuck)', '/resources/r-stuck', '/resources/:resourceId', () => import('@/pages/ResourceDetail')],
  ['ResourceDiscovery', '/resource-discovery', '/resource-discovery', () => import('@/pages/ResourceDiscovery')],
  ['Groups', '/groups', '/groups', () => import('@/pages/Groups')],
  ['DeviceProfiles', '/policies', '/policies', () => import('@/pages/DeviceProfiles')],
  ['IdpConnections', '/idp-connections', '/idp-connections', () => import('@/pages/IdpConnections')],
  ['IdpConnectionDetail', '/idp-connections/idp-1', '/idp-connections/:id', () => import('@/pages/IdpConnectionDetail')],
  ['ScimConflicts', '/scim-conflicts', '/scim-conflicts', () => import('@/pages/ScimConflicts')],
]

function writeControls(): string[] {
  return screen
    .queryAllByRole('button')
    .map((b) => (b.textContent ?? '').trim() || b.getAttribute('aria-label') || '')
    .filter((label) => WRITE_CONTROL.test(label))
}

describe('Viewer sees no write control on any page', () => {
  it.each(PAGES)('%s', async (_name, path, route, mod) => {
    setTestRole(Role.Viewer)
    await load(path, route, mod)
    // The page rendered its data (not an empty or error state).
    expect(document.body.textContent).toMatch(/office|res-|ops|laptops|Okta|a@x|conn-|shield-/)
    expect(writeControls()).toEqual([])
    expect(document.body.textContent).not.toMatch(/curl -fsSL|zecurity-connector|ENROLLMENT_TOKEN/)
    // No install token is minted for a role without NETWORK_WRITE.
    await new Promise((r) => setTimeout(r, 0))
    expect(fetchSpy.mock.calls.filter(([url]) => String(url).includes('/token'))).toEqual([])
  })
})

// Sanity check that the sweep can see controls at all: the same pages as an
// Admin show write controls.
describe('Admin sees write controls (sweep sanity check)', () => {
  it.each(PAGES.filter(([n]) => !n.startsWith('ScimConflicts')))('%s', async (_name, path, route, mod) => {
    setTestRole(Role.Admin)
    await load(path, route, mod)
    expect(writeControls().length).toBeGreaterThan(0)
    if (path.includes('pending')) {
      // A pending connector/shield fetches its install command for an Admin.
      await new Promise((r) => setTimeout(r, 0))
      expect(fetchSpy.mock.calls.some(([url]) => String(url).includes('/token'))).toBe(true)
    }
  })
})
