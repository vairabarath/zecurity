import { beforeEach, describe, expect, it, vi } from 'vitest'
import { CombinedGraphQLErrors } from '@apollo/client/errors'

const toastError = vi.fn()
vi.mock('sonner', () => ({ toast: { error: (...a: unknown[]) => toastError(...a) } }))

const { mutationErrorMessage, toastMutationError } = await import('./mutationErrors')

const gql = (m: string) => new CombinedGraphQLErrors({ errors: [{ message: m }] })

describe('toastMutationError', () => {
  beforeEach(() => toastError.mockReset())

  it("shows the server's message", () => {
    toastMutationError(gql('createGroup: a group with this name already exists'))
    expect(toastError).toHaveBeenCalledWith('Action failed', {
      description: 'createGroup: a group with this name already exists',
    })
  })

  it('leaves capability refusals to the error link (no double toast)', () => {
    expect(mutationErrorMessage(gql('forbidden: requires capability ACCESS_WRITE'))).toBeNull()
    toastMutationError(gql('forbidden: requires capability ACCESS_WRITE'))
    expect(toastError).not.toHaveBeenCalled()
  })

  it('reports network failures', () => {
    expect(mutationErrorMessage(new TypeError('Failed to fetch'))).toBe('Failed to fetch')
  })
})

// Every mutation hook must surface its errors: either an onError on the hook,
// or handling at the call site (try/catch, the hook's error state) listed here
// with where it is shown. A new silent mutation fails this test.
const HANDLED_AT_CALL_SITE: Record<string, string> = {
  'components/CreateDeviceProfileModal.tsx createDeviceProfile': 'try/catch → setError',
  'components/CreateDeviceProfileModal.tsx addProfileRequirement': 'try/catch → setError',
  'components/DiscoveredServicesPanel.tsx promoteOne': 'Promise.allSettled → summary toast',
  'components/EditDeviceProfileModal.tsx addProfileRequirement': 'try/catch → setError',
  'components/EditDeviceProfileModal.tsx removeProfileRequirement': 'try/catch → setError',
  'components/InstallCommandModal.tsx generateConnectorToken': 'try/catch → setError',
  'components/InstallCommandModal.tsx generateShieldToken': 'try/catch → setError',
  'components/ScanModal.tsx triggerScan': 'hook error state → combinedError',
  'components/idp/CreateIdpConnectionDialog.tsx createConnection': 'try/catch → setError',
  'components/scim/ConflictRow.tsx accept': 'try/catch → setError',
  'components/scim/ConflictRow.tsx reject': 'try/catch → setError',
  'components/scim/ConflictRow.tsx reopen': 'try/catch → setError',
  'components/scim/GrantBreakGlassButton.tsx grantBreakGlass': 'try/catch → toast',
  'components/scim/ScimConfigCard.tsx updateScimConfig': 'try/catch → toast',
  'components/scim/ScimConfigCard.tsx enableBreakGlass': 'try/catch → dialog refusal',
  'components/scim/ScimTokenPanel.tsx mintToken': 'try/catch → toast',
  'components/scim/ScimTokenPanel.tsx rotateToken': 'try/catch → toast',
  'components/scim/ScimTokenPanel.tsx revokeToken': 'try/catch → toast',
  'components/users/ManageRoleDialog.tsx setMemberRole': 'try/catch → alert',
  'pages/DeviceManagement.tsx revokeDevice': 'try/catch → toast',
  'pages/DeviceManagement.tsx approveDevice': 'try/catch → toast',
  'pages/DeviceManagement.tsx unapproveDevice': 'try/catch → toast',
  'pages/IdpConnectionDetail.tsx deleteConnection': 'try/catch → refusal panel',
  'pages/InviteAccept.tsx initiateAuth': 'try/catch → setError',
  'pages/Login.tsx initiateAuth': 'try/catch in callers → setError',
  'pages/signup/Step3Auth.tsx initiateAuth': 'try/catch + hook error state',
}

const SOURCES = import.meta.glob(['../**/*.tsx', '!../**/*.test.tsx'], {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

function mutationHooks(): { key: string; hasOnError: boolean }[] {
  const out: { key: string; hasOnError: boolean }[] = []
  for (const [path, src] of Object.entries(SOURCES)) {
    const file = path.replace(/^\.\.\//, '')
    for (const m of src.matchAll(/const \[(\w+)[^\]]*\]\s*=\s*useMutation(?:<[^>]*>)?\(/g)) {
      let i = m.index! + m[0].length
      let depth = 1
      while (depth > 0 && i < src.length) {
        if (src[i] === '(') depth++
        else if (src[i] === ')') depth--
        i++
      }
      // Options only (not the destructure: `mutationError` contains "onError").
      const options = src.slice(m.index! + m[0].length, i)
      out.push({ key: `${file} ${m[1]}`, hasOnError: /\bonError\b/.test(options) })
    }
  }
  return out
}

describe('no silent mutations', () => {
  const hooks = mutationHooks()

  it('found the mutation hooks', () => {
    expect(hooks.length).toBeGreaterThan(40)
  })

  it('every mutation hook has onError or is handled at its call site', () => {
    const silent = hooks.filter((h) => !h.hasOnError && !HANDLED_AT_CALL_SITE[h.key]).map((h) => h.key)
    expect(silent).toEqual([])
  })

  it('the allowlist has no stale entries', () => {
    const keys = new Set(hooks.filter((h) => !h.hasOnError).map((h) => h.key))
    expect(Object.keys(HANDLED_AT_CALL_SITE).filter((k) => !keys.has(k))).toEqual([])
  })
})
