import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { MockedProvider } from '@apollo/client/testing/react'
import ResourceDetail from './ResourceDetail'
import {
  GetAllResourcesDocument,
  GetShieldsDocument,
  GetResourcePoliciesDocument,
  SetResourcePolicyDocument,
  PreviewResourcePolicyDocument,
  Role,
} from '@/generated/graphql'
import { setTestRole } from '@/test/roles'

const RN = 'rn-1'
const HOST = '192.168.1.41'

const POLICIES = [
  {
    __typename: 'ResourcePolicy' as const,
    id: 'pol-default',
    name: 'Default',
    description: 'Workspace default',
    isDefault: true,
    resourceCount: 1,
    profiles: [],
    resources: [{ __typename: 'Resource' as const, id: 'res-1', name: 'web' }],
  },
  {
    __typename: 'ResourcePolicy' as const,
    id: 'pol-strict',
    name: 'Strict Policy',
    description: 'Restricted corporate devices only',
    isDefault: false,
    resourceCount: 0,
    profiles: [
      {
        __typename: 'DeviceProfile' as const,
        id: 'prof-1',
        name: 'Corporate Linux',
      },
    ],
    resources: [],
  },
]

function mockResource() {
  return {
    __typename: 'Resource' as const,
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
    shield: null,
    remoteNetwork: { __typename: 'RemoteNetwork' as const, id: RN, name: 'k2-net' },
    groups: [],
    policy: {
      __typename: 'ResourcePolicy' as const,
      id: 'pol-default',
      name: 'Default',
      description: null,
      isDefault: true,
      profiles: [],
    },
  }
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function renderDetail(mocks: any[] = []) {
  setTestRole(Role.Admin)
  const baseMocks = [
    {
      request: { query: GetAllResourcesDocument },
      result: { data: { allResources: [mockResource()] } },
      maxUsageCount: Number.POSITIVE_INFINITY,
    },
    {
      request: { query: GetShieldsDocument, variables: { remoteNetworkId: RN } },
      result: { data: { shields: [] } },
      maxUsageCount: Number.POSITIVE_INFINITY,
    },
    {
      request: { query: GetResourcePoliciesDocument },
      result: { data: { resourcePolicies: POLICIES } },
      maxUsageCount: Number.POSITIVE_INFINITY,
    },
    ...mocks,
  ]

  return render(
    <MockedProvider mocks={baseMocks}>
      <MemoryRouter initialEntries={['/resources/res-1']}>
        <Routes>
          <Route path="/resources/:resourceId" element={<ResourceDetail />} />
        </Routes>
      </MemoryRouter>
    </MockedProvider>,
  )
}

describe('ResourceDetail - Resource Policy section', () => {
  it('displays the assigned policy and profile requirements', async () => {
    renderDetail()

    expect(await screen.findByRole('heading', { name: 'web' })).toBeInTheDocument()
    expect(screen.getByText('Resource Policy')).toBeInTheDocument()
    expect(screen.getByText('Any device allowed (ungated)')).toBeInTheDocument()
  })

  it('allows changing policy with preview and reduction confirmation', async () => {
    const previewMock = {
      request: {
        query: PreviewResourcePolicyDocument,
        variables: { resourceIds: ['res-1'], profileIds: ['prof-1'] },
      },
      result: {
        data: {
          previewResourcePolicy: {
            __typename: 'ResourcePolicyPreview' as const,
            devices: 4,
            passing: 2,
          },
        },
      },
      maxUsageCount: Number.POSITIVE_INFINITY,
    }

    const setPolicyMock = {
      request: {
        query: SetResourcePolicyDocument,
        variables: { resourceId: 'res-1', policyId: 'pol-strict' },
      },
      result: {
        data: {
          setResourcePolicy: {
            __typename: 'Resource' as const,
            id: 'res-1',
            policy: {
              __typename: 'ResourcePolicy' as const,
              id: 'pol-strict',
              name: 'Strict Policy',
              isDefault: false,
              profiles: [
                {
                  __typename: 'DeviceProfile' as const,
                  id: 'prof-1',
                  name: 'Corporate Linux',
                },
              ],
            },
          },
        },
      },
    }

    renderDetail([previewMock, setPolicyMock])

    expect(await screen.findByText('Change Policy')).toBeInTheDocument()
    await userEvent.click(screen.getByText('Change Policy'))

    // Policy selector appears
    const select = screen.getByRole('combobox', { name: /resource policy/i })
    expect(select).toBeInTheDocument()

    // Select "Strict Policy"
    await userEvent.selectOptions(select, 'pol-strict')

    // Preview should load
    expect(
      await screen.findByText(/devices that can reach this resource today would pass/),
    ).toBeInTheDocument()
    expect(
      screen.getByText(/2 device\(s\) will lose access under this policy/),
    ).toBeInTheDocument()

    // Apply button should be disabled until reduction is confirmed
    const applyButton = screen.getByRole('button', { name: /apply policy/i })
    expect(applyButton).toBeDisabled()

    // Check confirmation checkbox
    const confirmCheckbox = screen.getByRole('checkbox', {
      name: /i understand that devices will lose access immediately/i,
    })
    await userEvent.click(confirmCheckbox)
    expect(applyButton).toBeEnabled()

    // Click Apply Policy
    await userEvent.click(applyButton)
  })
})
