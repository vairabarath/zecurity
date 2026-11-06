// Test helpers: put a user of a given console role in the auth store, with the
// capabilities the server would compute for it (PENDING-19 §4).
import { Capability, Role } from '@/generated/graphql'
import { useAuthStore } from '@/store/auth'

export const TEST_USER_ID = '00000000-0000-0000-0000-00000000a11c'

export const ROLE_CAPABILITIES: Record<Role, Capability[]> = {
  [Role.Admin]: Object.values(Capability),
  [Role.Viewer]: [Capability.ConsoleRead],
  [Role.Devops]: [Capability.ConsoleRead, Capability.NetworkWrite],
  [Role.Security]: [
    Capability.ConsoleRead,
    Capability.AccessWrite,
    Capability.PostureWrite,
    Capability.AuditAdmin,
  ],
  [Role.Helpdesk]: [Capability.ConsoleRead, Capability.DeviceWrite],
  [Role.Member]: [],
}

export function setTestRole(role: Role, id: string = TEST_USER_ID) {
  useAuthStore.getState().setUser({
    __typename: 'User',
    id,
    email: `${role.toLowerCase()}@test.example`,
    role,
    provider: 'google',
    createdAt: '2026-11-02T00:00:00Z',
    capabilities: ROLE_CAPABILITIES[role],
  })
}
