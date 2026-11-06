import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import AuditLog, { AUDIT_PAGE_SIZE } from './AuditLog'

// PENDING-11 S-9: paging walks the stream by seq (nextBefore), and the action
// filter restarts from the newest page.
const seen: unknown[] = []

vi.mock('@apollo/client/react', () => ({
  useQuery: (_doc: unknown, opts: { variables: { before?: number } }) => {
    seen.push(opts.variables)
    const top = opts.variables.before ?? 101
    return {
      data: {
        auditLogs: {
          nextBefore: top > 51 ? top - AUDIT_PAGE_SIZE : null,
          entries: [{ seq: top - 1, id: `e${top}`, action: top === 101 ? 'auth.sign_in_failed' : 'group.create', actorUserId: null, actorEmail: 'a@x', targetType: 't', targetId: '1', details: null, createdAt: '2026-11-06T10:00:00Z', rowHash: 'ab' }],
        },
      },
      loading: false,
      error: undefined,
      refetch: vi.fn(),
    }
  },
}))

describe('AuditLog', () => {
  it('pages by nextBefore and filters by action prefix', async () => {
    const user = userEvent.setup()
    render(<AuditLog />)
    expect(screen.getByText('auth.sign_in_failed')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Back to newer/ })).toBeDisabled()

    await user.click(screen.getByRole('button', { name: /Older/ }))
    expect(seen.at(-1)).toMatchObject({ before: 51, limit: AUDIT_PAGE_SIZE })
    expect(screen.getByText('group.create')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Older/ })).toBeDisabled()

    await user.click(screen.getByRole('button', { name: /Back to newer/ }))
    expect(seen.at(-1)).toMatchObject({ before: undefined })

    await user.type(screen.getByLabelText('Filter by action prefix'), 'auth.')
    await user.click(screen.getByRole('button', { name: 'Filter' }))
    expect(seen.at(-1)).toMatchObject({ before: undefined, actionPrefix: 'auth.' })
  })
})
