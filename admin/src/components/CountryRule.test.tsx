// PENDING-20 G1b: the country geofence in the device-profile editors.
import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MockedProvider } from '@apollo/client/testing/react'
import type { MockedResponse } from '@apollo/client/testing'
import { CreateDeviceProfileModal } from './CreateDeviceProfileModal'
import { EditDeviceProfileModal } from './EditDeviceProfileModal'
import {
  AddProfileRequirementDocument,
  CreateDeviceProfileDocument,
  GetDeviceProfilesDocument,
  GetSupportedPostureChecksDocument,
  RemoveProfileRequirementDocument,
  SetProfilePlatformsDocument,
  UpdateProfileRequirementCountriesDocument,
} from '@/generated/graphql'
import { searchCountries, countryLabel } from '@/lib/countries'
import { countryRuleProblem } from '@/lib/countryRule'

const CHECKS = [
  { __typename: 'PostureCheckDescriptor' as const, id: 'linux.firewall.active', label: 'Firewall active', platform: 'linux', allowUnsupportedMeaningful: true },
  { __typename: 'PostureCheckDescriptor' as const, id: 'zecurity.device.approved', label: 'Approved by an admin', platform: 'any', allowUnsupportedMeaningful: false },
  { __typename: 'PostureCheckDescriptor' as const, id: 'zecurity.network.country', label: 'Device country (public IP)', platform: 'any', allowUnsupportedMeaningful: false },
]
const checksMock: MockedResponse = {
  request: { query: GetSupportedPostureChecksDocument },
  result: { data: { supportedPostureChecks: CHECKS } },
}
const profilesRefetch: MockedResponse = {
  request: { query: GetDeviceProfilesDocument },
  result: { data: { deviceProfiles: [] } },
}
const reqResult = (id: string, requirements: { checkId: string }[] = []) => ({
  __typename: 'DeviceProfile',
  id,
  requirements: requirements.map((r, i) => ({ __typename: 'DeviceProfileRequirement', id: `r${i}`, allowUnsupported: false, ...r })),
})

describe('country helpers', () => {
  it('searches by name and code', () => {
    expect(searchCountries('india')).toContain('IN')
    expect(searchCountries('IN')[0]).toBe('IN')
    expect(searchCountries('india', ['IN'])).not.toContain('IN')
    expect(countryLabel('IN')).toBe('India (IN)')
  })

  it('requires at least one country when enabled', () => {
    expect(countryRuleProblem({ enabled: true, allow: [], deny: [] })).toMatch(/at least one/)
    expect(countryRuleProblem({ enabled: true, allow: [], deny: ['CN'] })).toBeNull()
    expect(countryRuleProblem({ enabled: false, allow: [], deny: [] })).toBeNull()
  })
})

async function pickCountry(user: ReturnType<typeof userEvent.setup>, inputLabel: string, query: string, option: string) {
  await user.type(screen.getByLabelText(inputLabel), query)
  await user.click(await screen.findByRole('option', { name: option }))
}

describe('CreateDeviceProfileModal: country check', () => {
  it('has its own editor, blocks Create until a country is chosen, and sends the lists', async () => {
    const user = userEvent.setup()
    const added = vi.fn()
    const mocks: MockedResponse[] = [
      checksMock,
      {
        request: { query: CreateDeviceProfileDocument, variables: { name: 'India only', manualTrust: true } },
        result: { data: { createDeviceProfile: { __typename: 'DeviceProfile', id: 'p-new', name: 'India only', manualTrust: true } } },
      },
      {
        request: {
          query: AddProfileRequirementDocument,
          variables: {
            profileId: 'p-new',
            checkId: 'zecurity.network.country',
            allowUnsupported: false,
            countries: { allow: ['IN'], deny: ['CN'] },
          },
        },
        result: () => {
          added()
          return { data: { addProfileRequirement: reqResult('p-new', [{ checkId: 'zecurity.network.country' }]) } }
        },
      },
      {
        // ADR-031: the new profile allows its OS once its requirements exist.
        request: { query: SetProfilePlatformsDocument, variables: { profileId: 'p-new', platforms: ['linux'] } },
        result: { data: { setProfilePlatforms: { __typename: 'DeviceProfile', id: 'p-new', allowedPlatforms: ['linux'] } } },
      },
      profilesRefetch,
    ]
    const onSuccess = vi.fn()
    render(
      <MockedProvider mocks={mocks}>
        <CreateDeviceProfileModal open onOpenChange={vi.fn()} onSuccess={onSuccess} />
      </MockedProvider>,
    )
    await user.click(screen.getByRole('button', { name: /Linux/ }))
    await user.type(screen.getByPlaceholderText('Profile Name'), 'India only')

    // Never a plain switch with the approval text.
    const countrySwitch = await screen.findByRole('switch', { name: 'Device country (public IP)' })
    expect(screen.getAllByRole('switch', { name: /Device country/ })).toHaveLength(1)
    await user.click(countrySwitch)
    expect(screen.getByText(/IP geolocation by DB-IP/)).toBeInTheDocument()
    const create = screen.getByRole('button', { name: 'Create Trusted Profile' })
    expect(create).toBeDisabled()
    expect(screen.getByText(/Choose at least one country/)).toBeInTheDocument()

    await pickCountry(user, 'Allow only these countries', 'India', 'India (IN)')
    await pickCountry(user, 'Block these countries', 'China', 'China (CN)')
    expect(create).toBeEnabled()
    await user.click(create)
    await waitFor(() => expect(onSuccess).toHaveBeenCalled())
    expect(added).toHaveBeenCalledTimes(1)
  })

  it('does not offer a country already in the other list', async () => {
    const user = userEvent.setup()
    render(
      <MockedProvider mocks={[checksMock]}>
        <CreateDeviceProfileModal open onOpenChange={vi.fn()} />
      </MockedProvider>,
    )
    await user.click(screen.getByRole('button', { name: /Linux/ }))
    await user.click(await screen.findByRole('switch', { name: 'Device country (public IP)' }))
    await pickCountry(user, 'Allow only these countries', 'India', 'India (IN)')
    await user.type(screen.getByLabelText('Block these countries'), 'India')
    expect(screen.queryByRole('option', { name: 'India (IN)' })).toBeNull()
  })
})

describe('EditDeviceProfileModal: country check', () => {
  const PROFILE = {
    id: 'p-1',
    name: 'India only',
    manualTrust: true,
    requirements: [
      { checkId: 'linux.firewall.active' },
      { checkId: 'zecurity.network.country', countries: { allow: ['IN'], deny: [] } },
    ],
  }

  it('loads the rule and saves a changed list as one in-place update (G-10)', async () => {
    const user = userEvent.setup()
    const updated = vi.fn()
    const removed = vi.fn()
    const mocks: MockedResponse[] = [
      checksMock,
      {
        request: {
          query: UpdateProfileRequirementCountriesDocument,
          variables: { profileId: 'p-1', countries: { allow: ['IN', 'US'], deny: [] } },
        },
        result: () => {
          updated()
          return { data: { updateProfileRequirementCountries: reqResult('p-1') } }
        },
      },
      {
        request: { query: RemoveProfileRequirementDocument, variables: { profileId: 'p-1', checkId: 'zecurity.network.country' } },
        result: () => {
          removed()
          return { data: { removeProfileRequirement: reqResult('p-1') } }
        },
      },
    ]
    const onSuccess = vi.fn()
    render(
      <MockedProvider mocks={mocks}>
        <EditDeviceProfileModal open profile={PROFILE} onOpenChange={vi.fn()} onSuccess={onSuccess} />
      </MockedProvider>,
    )
    const sw = await screen.findByRole('switch', { name: 'Device country (public IP)' })
    expect(sw).toHaveAttribute('aria-checked', 'true')
    expect(screen.getByRole('button', { name: /Remove India \(IN\) from Allow/ })).toBeInTheDocument()

    await pickCountry(user, 'Allow only these countries', 'United States', 'United States (US)')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(onSuccess).toHaveBeenCalled())
    expect(updated).toHaveBeenCalledTimes(1)
    expect(removed).not.toHaveBeenCalled()
  })

  it('switching the check off removes it', async () => {
    const user = userEvent.setup()
    const removed = vi.fn()
    const mocks: MockedResponse[] = [
      checksMock,
      {
        request: { query: RemoveProfileRequirementDocument, variables: { profileId: 'p-1', checkId: 'zecurity.network.country' } },
        result: () => {
          removed()
          return { data: { removeProfileRequirement: reqResult('p-1', [{ checkId: 'linux.firewall.active' }]) } }
        },
      },
      profilesRefetch,
    ]
    const onSuccess = vi.fn()
    render(
      <MockedProvider mocks={mocks}>
        <EditDeviceProfileModal open profile={PROFILE} onOpenChange={vi.fn()} onSuccess={onSuccess} />
      </MockedProvider>,
    )
    await user.click(await screen.findByRole('switch', { name: 'Device country (public IP)' }))
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(onSuccess).toHaveBeenCalled())
    expect(removed).toHaveBeenCalledTimes(1)
  })
})

describe('Devices page: last observed country', () => {
  it('shows the country, unknown, or not seen', async () => {
    const { default: DeviceManagement } = await import('@/pages/DeviceManagement')
    const { GetClientDevicesDocument } = await import('@/generated/graphql')
    const dev = (id: string, observedCountry: string | null, observedAt: string | null) => ({
      __typename: 'ClientDevice' as const, id, userId: 'u', name: `dev-${id}`, commonName: id, os: 'linux',
      spiffeId: null, certNotAfter: null, lastSeenAt: null, createdAt: '2026-11-02T00:00:00Z', revokedAt: null,
      approvedAt: null, approvedByEmail: null, observedCountry, observedNetwork: observedAt ? '49.36.200.0/24' : null, observedAt,
    })
    render(
      <MockedProvider
        mocks={[{
          request: { query: GetClientDevicesDocument },
          result: { data: { clientDevices: [dev('a', 'IN', '2026-11-02T10:00:00Z'), dev('b', null, '2026-11-02T10:00:00Z'), dev('c', null, null)] } },
        }]}
      >
        <DeviceManagement />
      </MockedProvider>,
    )
    expect(await screen.findByText('India (IN)')).toBeInTheDocument()
    expect(screen.getByText('unknown')).toBeInTheDocument()
    expect(screen.getByText('India (IN)').closest('[title]')?.getAttribute('title')).toMatch(/49\.36\.200\.0\/24/)
  })
})
