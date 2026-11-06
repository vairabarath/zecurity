// Console capabilities (PENDING-19 §4). The server checks every call with the
// caller's LIVE role; this module only decides what the console OFFERS, so a
// control that would always be refused isn't shown. It is never the
// authorization control.
import type { DocumentNode, OperationDefinitionNode, FieldNode } from 'graphql'
import { Capability, Role, type MeQuery } from '@/generated/graphql'
import { useAuthStore } from '@/store/auth'

export { Capability, Role }

type ConsoleUser = Pick<MeQuery['me'], 'capabilities'> | null | undefined

// Mutation ROOT FIELD → capability, mirroring the server's @requires. Kept in
// sync by capabilities.test.ts, which parses ../controller/graph/*.graphqls.
export const MUTATION_CAPABILITY: Record<string, Capability> = {
  // NETWORK_WRITE — DevOps
  createRemoteNetwork: Capability.NetworkWrite,
  deleteRemoteNetwork: Capability.NetworkWrite,
  generateConnectorToken: Capability.NetworkWrite,
  revokeConnector: Capability.NetworkWrite,
  deleteConnector: Capability.NetworkWrite,
  generateShieldToken: Capability.NetworkWrite,
  revokeShield: Capability.NetworkWrite,
  deleteShield: Capability.NetworkWrite,
  createResource: Capability.NetworkWrite,
  updateResource: Capability.NetworkWrite,
  protectResource: Capability.NetworkWrite,
  unprotectResource: Capability.NetworkWrite,
  deleteResource: Capability.NetworkWrite,
  forceDeleteResource: Capability.NetworkWrite,
  triggerScan: Capability.NetworkWrite,
  promoteDiscoveredService: Capability.NetworkWrite,
  // ACCESS_WRITE — Security
  createGroup: Capability.AccessWrite,
  updateGroup: Capability.AccessWrite,
  deleteGroup: Capability.AccessWrite,
  addGroupMember: Capability.AccessWrite,
  removeGroupMember: Capability.AccessWrite,
  assignResourceToGroup: Capability.AccessWrite,
  unassignResourceFromGroup: Capability.AccessWrite,
  // POSTURE_WRITE — Security
  createDeviceProfile: Capability.PostureWrite,
  updateDeviceProfileManualTrust: Capability.PostureWrite,
  addProfileRequirement: Capability.PostureWrite,
  removeProfileRequirement: Capability.PostureWrite,
  deleteDeviceProfile: Capability.PostureWrite,
  updateProfileRequirementCountries: Capability.PostureWrite, // PENDING-20 G-10
  // PENDING-16 resource policies (D-2: assigning a resource is POSTURE_WRITE too)
  createResourcePolicy: Capability.PostureWrite,
  updateResourcePolicy: Capability.PostureWrite,
  setResourcePolicyProfiles: Capability.PostureWrite,
  deleteResourcePolicy: Capability.PostureWrite,
  setResourcePolicy: Capability.PostureWrite,
  // DEVICE_WRITE — Helpdesk
  approveDevice: Capability.DeviceWrite,
  unapproveDevice: Capability.DeviceWrite,
  // DEVICE_ADMIN — Admin (R-8)
  revokeDevice: Capability.DeviceAdmin,
  // IDENTITY_ADMIN — Admin
  createIdpConnection: Capability.IdentityAdmin,
  updateIdpConnection: Capability.IdentityAdmin,
  setIdpConnectionStatus: Capability.IdentityAdmin,
  deleteIdpConnection: Capability.IdentityAdmin,
  testIdpConnection: Capability.IdentityAdmin,
  updateScimConfig: Capability.IdentityAdmin,
  setPlatformLoginEnabled: Capability.IdentityAdmin,
  mintScimToken: Capability.IdentityAdmin,
  rotateScimToken: Capability.IdentityAdmin,
  revokeScimToken: Capability.IdentityAdmin,
  acceptScimConflict: Capability.IdentityAdmin,
  rejectScimConflict: Capability.IdentityAdmin,
  reopenScimConflict: Capability.IdentityAdmin,
  grantPermission: Capability.IdentityAdmin,
  enableScimBreakGlass: Capability.IdentityAdmin,
  // MEMBERS_ADMIN — Admin (R-10)
  createInvitation: Capability.MembersAdmin,
  setMemberRole: Capability.MembersAdmin,
  // AUDIT_ADMIN — Admin + Security (PENDING-11 S-7): SIEM export sinks
  createAuditExportSink: Capability.AuditAdmin,
  updateAuditExportSink: Capability.AuditAdmin,
  rotateAuditExportSinkSecret: Capability.AuditAdmin,
  deleteAuditExportSink: Capability.AuditAdmin,
  testAuditExportSink: Capability.AuditAdmin,
}

// REST routes (controller/cmd/server/main.go → RequireCapability).
export const REST_CAPABILITY = {
  createInvitation: Capability.MembersAdmin, // POST /api/invitations
  connectorToken: Capability.NetworkWrite, // POST /api/connectors/{id}/token
  shieldToken: Capability.NetworkWrite, // POST /api/shields/{id}/token
} as const

export function hasCapability(user: ConsoleUser, cap: Capability): boolean {
  return !!user?.capabilities?.includes(cap)
}

// The one rule for console access: CONSOLE_READ. Deny-by-default — no user or
// no capabilities means no console (Members land on /install).
export function canUseConsole(user: ConsoleUser): boolean {
  return hasCapability(user, Capability.ConsoleRead)
}

// rootField returns the first root field of a single-operation document.
export function rootField(doc: DocumentNode): string | undefined {
  const op = doc.definitions.find(
    (d): d is OperationDefinitionNode => d.kind === 'OperationDefinition',
  )
  const field = op?.selectionSet.selections.find((s): s is FieldNode => s.kind === 'Field')
  return field?.name.value
}

// capabilityFor returns the capability a mutation document needs. An unknown
// mutation needs a capability nobody can match, so it is never offered.
export function capabilityFor(doc: DocumentNode): Capability | undefined {
  const f = rootField(doc)
  return f ? MUTATION_CAPABILITY[f] : undefined
}

export function useCan(cap: Capability): boolean {
  const user = useAuthStore((s) => s.user)
  return hasCapability(user, cap)
}

// useIsAdmin: the full Admin role, for rules where only Admin is exempt
// (R-9 self group membership, R-12 own group's resources, R-13 own device).
export function useIsAdmin(): boolean {
  const user = useAuthStore((s) => s.user)
  return user?.role === 'ADMIN'
}

export function useCurrentUserId(): string | undefined {
  return useAuthStore((s) => s.user?.id)
}

// Display labels and descriptions for the six roles (PENDING-19 §3).
export const ROLE_INFO: Record<string, { label: string; description: string }> = {
  ADMIN: {
    label: 'Admin',
    description: 'Everything, including identity (IdP, SCIM), members and roles.',
  },
  VIEWER: {
    label: 'Viewer',
    description: 'Read-only access to the whole console. No secrets are shown.',
  },
  DEVOPS: {
    label: 'DevOps',
    description:
      'Viewer, plus remote networks, connectors, shields, resources, discovery and install tokens.',
  },
  SECURITY: {
    label: 'Security',
    description:
      'Viewer, plus groups, resource access, device posture profiles and audit export (SIEM).',
  },
  HELPDESK: {
    label: 'Helpdesk',
    description: "Viewer, plus approving and unapproving other people's devices.",
  },
  MEMBER: {
    label: 'Member',
    description: 'No console access. Uses the Zecurity client only.',
  },
}

export function roleLabel(role: string | undefined | null): string {
  if (!role) return ''
  return ROLE_INFO[role.toUpperCase()]?.label ?? role.toLowerCase()
}

// Server limit for the audited role-change reason (controller maxRoleReasonLen).
export const MAX_ROLE_REASON = 500

// Manage Role dialog order: most to least access, Member last.
export const ROLE_ORDER: Role[] = [
  Role.Admin,
  Role.Viewer,
  Role.Devops,
  Role.Security,
  Role.Helpdesk,
  Role.Member,
]
