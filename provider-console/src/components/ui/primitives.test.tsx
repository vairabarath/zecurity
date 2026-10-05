import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from './dialog'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from './select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from './table'

// Smoke tests for the C-b primitives: they render accessibly and work in
// jsdom, so the Provider users page tests can rely on them.

describe('Dialog', () => {
  function Harness() {
    const [open, setOpen] = useState(true)
    return (
      <>
        <p>dialog is {open ? 'open' : 'closed'}</p>
        <Dialog open={open} onOpenChange={setOpen}>
          <DialogContent>
            <DialogTitle>Disable operator</DialogTitle>
            <DialogDescription>ops@provider.test will be signed out.</DialogDescription>
            <DialogFooter>
              <button onClick={() => setOpen(false)}>Cancel</button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      </>
    )
  }

  it('is a labelled, described modal dialog', () => {
    render(<Harness />)
    const dialog = screen.getByRole('dialog', { name: 'Disable operator' })
    expect(dialog).toHaveAccessibleDescription('ops@provider.test will be signed out.')
  })

  it('closes from its close button and from Escape, and unmounts its content', async () => {
    const user = userEvent.setup()
    const { unmount } = render(<Harness />)
    await user.click(screen.getByRole('button', { name: 'Close' }))
    expect(screen.getByText('dialog is closed')).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    unmount()

    render(<Harness />)
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })
})

describe('Select', () => {
  function Harness() {
    const [role, setRole] = useState('relay-ops')
    return (
      <>
        <p>selected {role}</p>
        <Select value={role} onValueChange={setRole}>
          <SelectTrigger aria-label="New role">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="super-admin">Super-admin</SelectItem>
            <SelectItem value="relay-ops">Relay ops</SelectItem>
          </SelectContent>
        </Select>
      </>
    )
  }

  it('shows the current value and changes it', async () => {
    const user = userEvent.setup()
    render(<Harness />)
    const trigger = screen.getByRole('combobox', { name: 'New role' })
    expect(trigger).toHaveTextContent('Relay ops')
    await user.click(trigger)
    await user.click(await screen.findByRole('option', { name: 'Super-admin' }))
    expect(screen.getByText('selected super-admin')).toBeInTheDocument()
    expect(trigger).toHaveTextContent('Super-admin')
  })

  it('can also be changed from the keyboard', async () => {
    const user = userEvent.setup()
    render(<Harness />)
    screen.getByRole('combobox', { name: 'New role' }).focus()
    await user.keyboard('{Enter}')
    await screen.findByRole('option', { name: 'Super-admin' })
    await user.keyboard('{ArrowUp}{Enter}')
    expect(screen.getByText('selected super-admin')).toBeInTheDocument()
  })
})

describe('Table', () => {
  it('renders a semantic table with column headers', () => {
    render(
      <Table aria-label="Operators">
        <TableHeader>
          <TableRow>
            <TableHead>Email</TableHead>
            <TableHead>Role</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          <TableRow>
            <TableCell>ops@provider.test</TableCell>
            <TableCell>Relay ops</TableCell>
          </TableRow>
        </TableBody>
      </Table>,
    )
    const table = screen.getByRole('table', { name: 'Operators' })
    expect(screen.getAllByRole('columnheader').map((h) => h.textContent)).toEqual(['Email', 'Role'])
    expect(table.querySelectorAll('tbody tr')).toHaveLength(1)
  })
})
