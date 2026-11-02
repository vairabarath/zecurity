import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MockedProvider } from '@apollo/client/testing/react'
import type { MockedResponse } from '@apollo/client/testing'
import { GraphQLError } from 'graphql'
import { ManageRoleDialog } from './ManageRoleDialog'
import { Capability, GetUsersDocument, Role, SetMemberRoleDocument } from '@/generated/graphql'

const target = { id: 'u-2', email: 'dana@example.test', role: 'VIEWER' }

const usersRefetch: MockedResponse = {
  request: { query: GetUsersDocument },
  result: { data: { users: [] } },
}

function renderDialog(mocks: MockedResponse[], onClose = vi.fn()) {
  render(
    <MockedProvider mocks={mocks}>
      <ManageRoleDialog target={target} onClose={onClose} />
    </MockedProvider>,
  )
  return onClose
}

describe('ManageRoleDialog (PENDING-19 §7)', () => {
  it('offers the six roles with the current one selected', () => {
    renderDialog([])
    for (const label of ['Admin', 'Viewer', 'DevOps', 'Security', 'Helpdesk', 'Member']) {
      expect(screen.getByRole('radio', { name: new RegExp(`^${label}`) })).toBeInTheDocument()
    }
    expect(screen.getAllByRole('radio')).toHaveLength(6)
    expect(screen.getByRole('radio', { name: /^Viewer/ })).toBeChecked()
  })

  it('needs a changed role and a reason before Save', async () => {
    const user = userEvent.setup()
    renderDialog([])
    const save = screen.getByRole('button', { name: 'Save role' })
    expect(save).toBeDisabled()
    await user.click(screen.getByRole('radio', { name: /^DevOps/ }))
    expect(save).toBeDisabled() // no reason yet
    await user.type(screen.getByLabelText(/Reason/), '   ')
    expect(save).toBeDisabled() // blank after trim
    await user.type(screen.getByLabelText(/Reason/), 'on-call')
    expect(save).toBeEnabled()
  })

  it('warns that Member loses the console', async () => {
    const user = userEvent.setup()
    renderDialog([])
    await user.click(screen.getByRole('radio', { name: /^Member/ }))
    expect(screen.getByText(/will lose the console/)).toBeInTheDocument()
  })

  it('sends userId, role and the trimmed reason, then closes', async () => {
    const user = userEvent.setup()
    const mock: MockedResponse = {
      request: {
        query: SetMemberRoleDocument,
        variables: { userId: 'u-2', role: Role.Devops, reason: 'joined the network team' },
      },
      result: {
        data: {
          setMemberRole: {
            __typename: 'User',
            id: 'u-2',
            role: Role.Devops,
            capabilities: [Capability.ConsoleRead, Capability.NetworkWrite],
          },
        },
      },
    }
    const onClose = renderDialog([mock, usersRefetch])
    await user.click(screen.getByRole('radio', { name: /^DevOps/ }))
    await user.type(screen.getByLabelText(/Reason/), '  joined the network team  ')
    await user.click(screen.getByRole('button', { name: 'Save role' }))
    await waitFor(() => expect(onClose).toHaveBeenCalled())
  })

  it("shows the server's refusal and stays open", async () => {
    const user = userEvent.setup()
    const mock: MockedResponse = {
      request: {
        query: SetMemberRoleDocument,
        variables: { userId: 'u-2', role: Role.Member, reason: 'leaving' },
      },
      result: {
        errors: [new GraphQLError("can't remove the last active admin; make someone else Admin first")],
      },
    }
    const onClose = renderDialog([mock])
    await user.click(screen.getByRole('radio', { name: /^Member/ }))
    await user.type(screen.getByLabelText(/Reason/), 'leaving')
    await user.click(screen.getByRole('button', { name: 'Save role' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('last active admin')
    expect(onClose).not.toHaveBeenCalled()
  })
})
