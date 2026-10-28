import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { ConfirmDangerDialog, type DangerError } from './ConfirmDangerDialog'

// Sprint 22 Phase X (D-17): a destructive action needs a reason (1–500
// characters after trimming, counted like the server: Unicode code points)
// and the exact name typed to confirm. Reused by Phase D (drain).

function setup(props: { pending?: boolean; error?: DangerError } = {}) {
  const onConfirm = vi.fn()
  const onCancel = vi.fn()
  render(
    <ConfirmDangerDialog
      title="Revoke relay"
      description="Its certificates go on the CRL."
      confirmText="relay-a"
      confirmLabel="Revoke relay"
      pendingLabel="Revoking…"
      pending={props.pending ?? false}
      error={props.error}
      onConfirm={onConfirm}
      onCancel={onCancel}
    />,
  )
  const dialog = screen.getByRole('dialog', { name: 'Revoke relay' })
  return {
    user: userEvent.setup(),
    onConfirm,
    onCancel,
    reason: screen.getByLabelText('Reason'),
    name: screen.getByLabelText(/Type relay-a to confirm/),
    button: () => screen.getByRole('button', { name: 'Revoke relay' }),
    dialog,
  }
}

describe('ConfirmDangerDialog', () => {
  it('stays disabled until the reason is filled and the exact name is typed', async () => {
    const { user, reason, name, button, onConfirm } = setup()
    expect(button()).toBeDisabled()
    await user.type(reason, 'compromised host')
    expect(button()).toBeDisabled()
    await user.type(name, 'relay-')
    expect(button()).toBeDisabled()
    await user.type(name, 'a')
    expect(button()).toBeEnabled()
    await user.click(button())
    expect(onConfirm).toHaveBeenCalledTimes(1)
    expect(onConfirm).toHaveBeenCalledWith('compromised host', 'relay-a')
  })

  it('a whitespace-only reason does not count; the sent reason is trimmed like the server', async () => {
    const { user, reason, name, button, onConfirm } = setup()
    await user.type(name, 'relay-a')
    await user.type(reason, '   ')
    expect(button()).toBeDisabled()
    await user.type(reason, ' lost key  ')
    await user.click(button())
    expect(onConfirm).toHaveBeenCalledWith('lost key', 'relay-a')
  })

  it('the name match is exact: case and surrounding spaces matter', async () => {
    const { user, reason, name, button } = setup()
    await user.type(reason, 'r')
    for (const typed of ['Relay-A', 'RELAY-A', ' relay-a', 'relay-a ']) {
      await user.clear(name)
      await user.type(name, typed)
      expect(button(), typed).toBeDisabled()
    }
  })

  it('counts code points like the server: 300 emoji are fine, 501 letters are too long', async () => {
    const { user, reason, name, button } = setup()
    await user.type(name, 'relay-a')
    await user.click(reason)
    await user.paste('😀'.repeat(300)) // 600 UTF-16 units, 300 code points
    expect(screen.getByText('300 / 500')).toBeInTheDocument()
    expect(button()).toBeEnabled()
    await user.clear(reason)
    await user.paste('x'.repeat(501))
    expect(screen.getByText('501 / 500')).toBeInTheDocument()
    expect(screen.getByText(/Keep the reason to 500 characters or fewer/)).toBeInTheDocument()
    expect(button()).toBeDisabled()
  })

  it('pending disables confirming, so a second submit is impossible', async () => {
    const { onConfirm } = setup({ pending: true })
    expect(screen.getByRole('button', { name: 'Revoking…' })).toBeDisabled()
    expect(screen.queryByRole('button', { name: 'Revoke relay' })).not.toBeInTheDocument()
    expect(onConfirm).not.toHaveBeenCalled()
  })

  it('cancel sends nothing', async () => {
    const { user, onCancel, onConfirm } = setup()
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(onCancel).toHaveBeenCalledTimes(1)
    expect(onConfirm).not.toHaveBeenCalled()
  })

  it('shows field errors next to their field and other errors in an alert', () => {
    setup({ error: { field: 'confirm', message: 'That is not the relay’s name.' } })
    const name = screen.getByLabelText(/Type relay-a to confirm/)
    expect(name).toHaveAttribute('aria-invalid', 'true')
    expect(name).toHaveAccessibleDescription(/That is not the relay’s name\./)
  })

  it('a reason error is attached to the reason field', () => {
    setup({ error: { field: 'reason', message: 'Enter a reason.' } })
    expect(screen.getByLabelText('Reason')).toHaveAccessibleDescription(/Enter a reason\./)
  })

  it('a general error is announced as an alert', () => {
    setup({ error: { message: 'Only super-admins can do this.' } })
    expect(screen.getByRole('alert')).toHaveTextContent('Only super-admins can do this.')
  })
})
