import { describe, expect, it } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MockedProvider } from '@apollo/client/testing/react'
import ResourcePolicies from './ResourcePolicies'
import {
  GetResourcePoliciesDocument,
  GetDeviceProfilesDocument,
  PreviewResourcePolicyDocument,
} from '@/generated/graphql'
import { Role } from '@/generated/graphql'
import { setTestRole } from '@/test/roles'

const PROFILES = [
  {
    __typename: 'DeviceProfile' as const,
    id: 'prof-1',
    name: 'Corporate Linux',
    manualTrust: false,
    requirements: [
      {
        __typename: 'DeviceProfileRequirement' as const,
        id: 'req-1',
        checkId: 'linux.firewall.active',
        allowUnsupported: false,
        countries: null,
      },
    ],
    policies: [],
  },
  {
    __typename: 'DeviceProfile' as const,
    id: 'prof-empty',
    name: 'Empty Profile',
    manualTrust: false,
    requirements: [],
    policies: [],
  },
]

const POLICIES = [
  {
    __typename: 'ResourcePolicy' as const,
    id: 'pol-default',
    name: 'Default',
    description: 'Workspace default policy',
    isDefault: true,
    resourceCount: 2,
    profiles: [],
    resources: [
      { __typename: 'Resource' as const, id: 'res-1', name: 'Internal API' },
      { __typename: 'Resource' as const, id: 'res-2', name: 'Database' },
    ],
  },
  {
    __typename: 'ResourcePolicy' as const,
    id: 'pol-custom',
    name: 'Strict Prod',
    description: 'Requires corporate firewall',
    isDefault: false,
    resourceCount: 1,
    profiles: [
      {
        __typename: 'DeviceProfile' as const,
        id: 'prof-1',
        name: 'Corporate Linux',
      },
    ],
    resources: [
      { __typename: 'Resource' as const, id: 'res-3', name: 'Billing Service' },
    ],
  },
]

function renderPolicies(policies = POLICIES) {
  setTestRole(Role.Admin)
  const mocks = [
    {
      request: { query: GetResourcePoliciesDocument },
      result: { data: { resourcePolicies: policies } },
      maxUsageCount: Number.POSITIVE_INFINITY,
    },
    {
      request: { query: GetDeviceProfilesDocument },
      result: { data: { deviceProfiles: PROFILES } },
      maxUsageCount: Number.POSITIVE_INFINITY,
    },
    {
      request: {
        query: PreviewResourcePolicyDocument,
        variables: { resourceIds: ['res-3'], profileIds: ['prof-1'] },
      },
      result: {
        data: {
          previewResourcePolicy: {
            __typename: 'ResourcePolicyPreview' as const,
            devices: 5,
            passing: 3,
          },
        },
      },
      maxUsageCount: Number.POSITIVE_INFINITY,
    },
    {
      request: {
        query: PreviewResourcePolicyDocument,
        variables: { resourceIds: ['res-1', 'res-2'], profileIds: [] },
      },
      result: {
        data: {
          previewResourcePolicy: {
            __typename: 'ResourcePolicyPreview' as const,
            devices: 2,
            passing: 2,
          },
        },
      },
      maxUsageCount: Number.POSITIVE_INFINITY,
    },
  ]

  return render(
    <MockedProvider mocks={mocks}>
      <ResourcePolicies />
    </MockedProvider>,
  )
}

describe('ResourcePolicies', () => {
  it('renders the policy list with Default badge, profiles chips, and resource counts', async () => {
    renderPolicies()

    expect(await screen.findByText('Strict Prod')).toBeInTheDocument()
    expect(screen.getByText('Resource Policies')).toBeInTheDocument()
    expect(screen.getAllByText('Default').length).toBeGreaterThan(0)
    expect(screen.getByText('Corporate Linux')).toBeInTheDocument()
    expect(screen.getByText('Any device')).toBeInTheDocument()
    expect(screen.getByText('resources')).toBeInTheDocument()
    expect(screen.getByText('resource')).toBeInTheDocument()
  })

  it('does not allow deleting the Default policy', async () => {
    renderPolicies()
    await screen.findByText('Strict Prod')

    // There should be only 1 Delete button because Default cannot be deleted
    const deleteButtons = screen.getAllByRole('button', { name: 'Delete' })
    expect(deleteButtons).toHaveLength(1)
  })

  it('confirms moving resources to Default when deleting a custom policy', async () => {
    renderPolicies()
    await screen.findByText('Strict Prod')

    await userEvent.click(screen.getByRole('button', { name: 'Delete' }))

    expect(await screen.findByText('Delete resource policy')).toBeInTheDocument()
    expect(
      screen.getByText(/1 resource will move to the Default policy/),
    ).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    await waitFor(() => {
      expect(screen.queryByText('Delete resource policy')).not.toBeInTheDocument()
    })
  })

  it('disables empty profiles in the policy creation/edit modal', async () => {
    renderPolicies()
    await screen.findByText('Resource Policies')

    await userEvent.click(screen.getByRole('button', { name: /create resource policy/i }))
    expect(await screen.findByRole('heading', { name: 'Create Resource Policy' })).toBeInTheDocument()

    // Empty profile checkbox should be disabled
    const emptyCheckbox = await screen.findByRole('checkbox', {
      name: /Empty Profile/i,
    })
    expect(emptyCheckbox).toBeDisabled()
    expect(
      screen.getByText(/Empty profile — cannot be added/),
    ).toBeInTheDocument()

    // Valid profile should be enabled
    const validCheckbox = screen.getByRole('checkbox', {
      name: /Corporate Linux/i,
    })
    expect(validCheckbox).not.toBeDisabled()
  })

  it('prevents renaming the Default policy when editing', async () => {
    renderPolicies()
    await screen.findByText('Strict Prod')

    const editButtons = screen.getAllByRole('button', { name: 'Edit' })
    // First edit button is for Default policy
    await userEvent.click(editButtons[0])

    expect(await screen.findByText('Edit Policy: Default')).toBeInTheDocument()
    expect(screen.getByDisplayValue('Default')).toBeDisabled()
    expect(screen.getByText('The Default policy cannot be renamed.')).toBeInTheDocument()
  })
})
