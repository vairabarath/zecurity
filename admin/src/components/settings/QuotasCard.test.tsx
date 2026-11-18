import { describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import { MockedProvider } from '@apollo/client/testing/react'
import { GetWorkspaceQuotasDocument } from '@/generated/graphql'
import { QuotasCard } from './QuotasCard'

// Sprint 23 S3 (D-47): Settings shows the workspace's limits and usage,
// read-only, with "Limit reached" and "Over limit" marks.

const q = (key: string, limit: number | null, used: number, over = false) => ({ __typename: 'WorkspaceQuota' as const, key, limit, used, over })

describe('QuotasCard', () => {
  it('shows usage against each limit', async () => {
    const mocks = [
      {
        request: { query: GetWorkspaceQuotasDocument },
        result: {
          data: {
            workspaceQuotas: [
              q('users', 5, 2),
              q('resources', 50, 50),
              q('remote_networks', 3, 4, true),
              q('connectors', null, 7),
              q('shields', 10, 0),
              q('client_devices', 25, 1),
            ],
          },
        },
      },
    ]
    render(
      <MockedProvider mocks={mocks}>
        <QuotasCard />
      </MockedProvider>,
    )
    const table = await screen.findByRole('table', { name: 'Limits' })
    const row = (name: string) => within(within(table).getByRole('row', { name }))
    expect(row('Users').getByText('2')).toBeInTheDocument()
    expect(row('Users').getByText('5')).toBeInTheDocument()
    expect(row('Resources').getByText('Limit reached')).toBeInTheDocument()
    expect(row('Remote networks').getByText('Over limit')).toBeInTheDocument()
    expect(row('Connectors').getByText('Unlimited')).toBeInTheDocument()
    expect(row('Shields').queryByText(/Limit reached|Over limit/)).not.toBeInTheDocument()
  })
})
