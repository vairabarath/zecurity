import { describe, expect, it } from 'vitest'
import { Kind, parse, type ObjectTypeDefinitionNode, type ObjectTypeExtensionNode } from 'graphql'
import { Capability, Role } from '@/generated/graphql'
import { ROLE_CAPABILITIES } from '@/test/roles'
import {
  MUTATION_CAPABILITY,
  ROLE_INFO,
  ROLE_ORDER,
  canUseConsole,
  capabilityFor,
} from '@/lib/capabilities'
import { SetMemberRoleDocument, RevokeDeviceDocument, InitiateAuthDocument } from '@/generated/graphql'
import mutationsSource from '@/graphql/mutations.graphql?raw'

// The server's schema: every controller/graph/*.graphqls (codegen reads the
// same files), loaded as text by Vite.
const SERVER_SCHEMA = import.meta.glob('../../../controller/graph/*.graphqls', {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

function serverMutationCapabilities(): Map<string, string | null> {
  const out = new Map<string, string | null>()
  for (const source of Object.values(SERVER_SCHEMA)) {
    const doc = parse(source)
    for (const def of doc.definitions) {
      if (
        (def.kind !== Kind.OBJECT_TYPE_DEFINITION && def.kind !== Kind.OBJECT_TYPE_EXTENSION) ||
        (def as ObjectTypeDefinitionNode | ObjectTypeExtensionNode).name.value !== 'Mutation'
      ) {
        continue
      }
      for (const field of (def as ObjectTypeDefinitionNode).fields ?? []) {
        const req = field.directives?.find((d) => d.name.value === 'requires')
        const arg = req?.arguments?.find((a) => a.name.value === 'capability')
        out.set(field.name.value, arg && arg.value.kind === Kind.ENUM ? arg.value.value : null)
      }
    }
  }
  return out
}

describe('capability map parity with the server (PENDING-19 §4.1)', () => {
  const server = serverMutationCapabilities()

  it('found the server schema', () => {
    expect(Object.keys(SERVER_SCHEMA).length).toBeGreaterThanOrEqual(10)
    expect(server.size).toBeGreaterThan(40)
  })

  it('every gated server mutation is in MUTATION_CAPABILITY with the same capability', () => {
    for (const [field, cap] of server) {
      if (cap === null) continue // public (initiateAuth)
      expect(MUTATION_CAPABILITY[field], field).toBe(cap)
    }
  })

  it('MUTATION_CAPABILITY has no entry the server does not gate the same way', () => {
    for (const [field, cap] of Object.entries(MUTATION_CAPABILITY)) {
      expect(server.get(field), field).toBe(cap)
    }
  })

  it('every mutation document the console sends resolves to a capability (or is public)', () => {
    const doc = parse(mutationsSource)
    for (const def of doc.definitions) {
      if (def.kind !== Kind.OPERATION_DEFINITION || def.operation !== 'mutation') continue
      const field = def.selectionSet.selections.find((s) => s.kind === Kind.FIELD)
      const name = field && field.kind === Kind.FIELD ? field.name.value : ''
      expect(server.has(name), `${name} not in server schema`).toBe(true)
      if (server.get(name) !== null) {
        expect(MUTATION_CAPABILITY[name], name).toBeDefined()
      }
    }
  })

  it('reads the root field of a document', () => {
    expect(capabilityFor(SetMemberRoleDocument)).toBe(Capability.MembersAdmin)
    expect(capabilityFor(RevokeDeviceDocument)).toBe(Capability.DeviceAdmin)
    expect(capabilityFor(InitiateAuthDocument)).toBeUndefined()
  })
})

describe('console access', () => {
  it('needs CONSOLE_READ: every role but Member', () => {
    for (const role of Object.values(Role)) {
      const user = { capabilities: ROLE_CAPABILITIES[role] }
      expect(canUseConsole(user), role).toBe(role !== Role.Member)
    }
  })

  it('denies by default', () => {
    expect(canUseConsole(null)).toBe(false)
    expect(canUseConsole(undefined)).toBe(false)
    expect(canUseConsole({ capabilities: [] })).toBe(false)
  })

  it('describes every role, in the dialog order', () => {
    expect([...ROLE_ORDER].sort()).toEqual(Object.values(Role).sort())
    for (const role of ROLE_ORDER) expect(ROLE_INFO[role]?.description).toBeTruthy()
  })
})
