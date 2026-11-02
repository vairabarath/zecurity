// Every console role reads every page (CONSOLE_READ), so the sidebar offers the
// same navigation to all of them (PENDING-19 R2; found missing in the R3 run:
// Helpdesk had no link to Devices).
import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Role } from '@/generated/graphql'
import { setTestRole } from '@/test/roles'
import { Sidebar } from './Sidebar'

describe('Sidebar navigation by role', () => {
  it.each([Role.Admin, Role.Viewer, Role.Devops, Role.Security, Role.Helpdesk])('%s', (role) => {
    setTestRole(role)
    render(<MemoryRouter><Sidebar /></MemoryRouter>)
    for (const label of ['Identity Providers', 'Access Log', 'Devices', 'Policies', 'Remote Networks', 'Resources']) {
      expect(screen.getByText(label), label).toBeInTheDocument()
    }
  })
})
