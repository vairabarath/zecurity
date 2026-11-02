// Per-role rendering (PENDING-19 R2): each page offers only the write controls
// the role's capabilities allow, plus the "not on yourself" rules R-9, R-12
// and R-13. The server enforces all of it; these tests pin what the console
// offers so a role never sees a control that will always be refused.
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import type { DocumentNode } from 'graphql'
import {
  GetAllResourcesDocument,
  GetClientDevicesDocument,
  GetGroupDocument,
  GetGroupsDocument,
  GetIdpConnectionsDocument,
  GetRemoteNetworksDocument,
  GetUsersDocument,
  Role,
} from '@/generated/graphql'
import { TEST_USER_ID, setTestRole } from '@/test/roles'

const data = new Map<DocumentNode, unknown>()

vi.mock('@apollo/client/react', () => ({
  useQuery: (doc: DocumentNode) => ({
    data: data.get(doc),
    loading: false,
    error: undefined,
    refetch: vi.fn(),
    startPolling: vi.fn(),
  }),
  useMutation: () => [vi.fn(), { loading: false }],
}))

const { default: DeviceManagement } = await import('@/pages/DeviceManagement')
const { default: TeamUsers } = await import('@/pages/TeamUsers')
const { default: GroupDetail } = await import('@/pages/GroupDetail')
const { default: RemoteNetworks } = await import('@/pages/RemoteNetworks')

const OTHER = '00000000-0000-0000-0000-0000000000b0'

function device(id: string, userId: string) {
  return {
    __typename: 'ClientDevice', id, userId, name: `dev-${id}`, commonName: id, os: 'linux',
    spiffeId: null, certNotAfter: null, lastSeenAt: null, createdAt: '2026-11-02T00:00:00Z',
    revokedAt: null, approvedAt: null, approvedByEmail: null,
  }
}

function user(id: string, email: string, role: string) {
  return {
    __typename: 'User', id, email, role, createdAt: '2026-11-02', provider: 'google',
    provisioningOwner: 'jit', provisionedBy: 'jit',
  }
}

function group(memberIds: string[]) {
  return {
    __typename: 'Group', id: 'g-1', name: 'ops', description: null, origin: 'manual',
    connectionId: null, createdAt: '2026-11-02', updatedAt: '2026-11-02',
    members: memberIds.map((id) => ({ __typename: 'User', id, email: `${id}@x`, role: 'MEMBER' })),
    resources: [{ __typename: 'Resource', id: 'r-1', name: 'db', host: '10.0.0.1', protocol: 'tcp', portFrom: 5432, portTo: 5432 }],
  }
}

beforeEach(() => {
  data.clear()
  data.set(GetClientDevicesDocument, { clientDevices: [device('mine', TEST_USER_ID), device('theirs', OTHER)] })
  data.set(GetUsersDocument, { users: [user(TEST_USER_ID, 'me@x', 'ADMIN'), user(OTHER, 'other@x', 'MEMBER')] })
  data.set(GetGroupsDocument, { groups: [] })
  data.set(GetAllResourcesDocument, {
    allResources: [{ __typename: 'Resource', id: 'r-2', name: 'web', host: '10.0.0.2', protocol: 'tcp', portFrom: 80, portTo: 80 }],
  })
  data.set(GetIdpConnectionsDocument, { idpConnections: [] })
  data.set(GetRemoteNetworksDocument, { remoteNetworks: [] })
})

function renderGroupDetail() {
  render(
    <MemoryRouter initialEntries={['/groups/g-1']}>
      <Routes>
        <Route path="/groups/:id" element={<GroupDetail />} />
      </Routes>
    </MemoryRouter>,
  )
}

const q = {
  approve: () => screen.queryAllByRole('button', { name: /^Approve$/ }),
  revoke: () => screen.queryAllByRole('button', { name: /^Revoke$/ }),
}

describe('DeviceManagement by role', () => {
  it('Admin: approve on every device (own included) and revoke', () => {
    setTestRole(Role.Admin)
    render(<DeviceManagement />)
    expect(q.approve()).toHaveLength(2)
    expect(q.revoke()).toHaveLength(2)
  })

  it("Helpdesk: approve only other people's devices; never revoke (R-8, R-13)", () => {
    setTestRole(Role.Helpdesk)
    render(<DeviceManagement />)
    expect(q.approve()).toHaveLength(1)
    expect(q.revoke()).toHaveLength(0)
  })

  it.each([Role.Viewer, Role.Devops, Role.Security])('%s: no device actions', (role) => {
    setTestRole(role)
    render(<DeviceManagement />)
    expect(q.approve()).toHaveLength(0)
    expect(q.revoke()).toHaveLength(0)
  })
})

describe('TeamUsers by role', () => {
  it('Admin: invite and Manage role, disabled on your own row', () => {
    setTestRole(Role.Admin)
    render(<MemoryRouter><TeamUsers /></MemoryRouter>)
    expect(screen.getByRole('button', { name: /Invite/ })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Manage role for me@x' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Manage role for other@x' })).toBeEnabled()
  })

  it.each([Role.Viewer, Role.Devops, Role.Security, Role.Helpdesk])(
    '%s: no invite, no Manage role (R-10)',
    (role) => {
      setTestRole(role)
      render(<MemoryRouter><TeamUsers /></MemoryRouter>)
      expect(screen.queryByRole('button', { name: /Invite/ })).toBeNull()
      expect(screen.queryByRole('button', { name: /Manage role/ })).toBeNull()
      expect(screen.getAllByText('other@x').length).toBeGreaterThan(0)
    },
  )
})

describe('GroupDetail by role', () => {
  it('Security, not a member: add members and assign resources', () => {
    setTestRole(Role.Security)
    data.set(GetGroupDocument, { group: group([OTHER]) })
    renderGroupDetail()
    expect(screen.getByRole('button', { name: /Add Member/ })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Remove/ })).toBeInTheDocument()
  })

  it("Security, a member: can't remove itself (R-9) or change the group's resources (R-12)", async () => {
    setTestRole(Role.Security)
    data.set(GetGroupDocument, { group: group([TEST_USER_ID]) })
    renderGroupDetail()
    expect(screen.queryByRole('button', { name: /Remove/ })).toBeNull()
    screen.getByRole('button', { name: 'resources' }).click()
    expect(await screen.findByText(/only an Admin can change its resources/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Assign Resource/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /Unassign/ })).toBeNull()
  })

  it('Admin, a member: everything, including itself', async () => {
    setTestRole(Role.Admin)
    data.set(GetGroupDocument, { group: group([TEST_USER_ID]) })
    renderGroupDetail()
    expect(screen.getByRole('button', { name: /Remove/ })).toBeInTheDocument()
    screen.getByRole('button', { name: 'resources' }).click()
    expect(await screen.findByRole('button', { name: /Assign Resource/ })).toBeInTheDocument()
  })

  it.each([Role.Viewer, Role.Devops, Role.Helpdesk])('%s: read-only', async (role) => {
    setTestRole(role)
    data.set(GetGroupDocument, { group: group([OTHER]) })
    renderGroupDetail()
    expect(screen.queryByRole('button', { name: /Add Member/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /Remove/ })).toBeNull()
    screen.getByRole('button', { name: 'resources' }).click()
    expect(await screen.findByText('db')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Assign Resource/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /Unassign/ })).toBeNull()
  })
})

describe('RemoteNetworks by role', () => {
  it.each([
    [Role.Admin, true],
    [Role.Devops, true],
    [Role.Security, false],
    [Role.Viewer, false],
    [Role.Helpdesk, false],
  ])('%s: Add Network shown = %s (NETWORK_WRITE)', (role, shown) => {
    setTestRole(role)
    render(<MemoryRouter><RemoteNetworks /></MemoryRouter>)
    expect(screen.queryAllByRole('button', { name: /Add Network/ }).length > 0).toBe(shown)
  })
})
