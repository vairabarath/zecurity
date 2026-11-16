import { describe, expect, it } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import FeatureTour from './FeatureTour'
import { tourFeatures } from '@/lib/featureTour'

describe('FeatureTour', () => {
  it('renders the MFA card with its sections and links', () => {
    render(<MemoryRouter><FeatureTour /></MemoryRouter>)
    expect(screen.getByText('Multi-factor authentication (MFA)')).toBeInTheDocument()
    expect(screen.getByText('Require MFA for the admin console')).toBeInTheDocument()
    expect(screen.getByText('Reset a user’s MFA')).toBeInTheDocument()
    for (const link of screen.getAllByText('Open Users')) expect(link.closest('a')).toHaveAttribute('href', '/users')
    expect(screen.getByText('zecurity-client auth').tagName).toBe('CODE')
  })

  it('collapses and expands a card', () => {
    render(<MemoryRouter><FeatureTour /></MemoryRouter>)
    const toggle = screen.getByRole('button', { name: /Multi-factor authentication/ })
    fireEvent.click(toggle)
    expect(screen.queryByText('Require MFA for resources')).not.toBeInTheDocument()
    fireEvent.click(toggle)
    expect(screen.getByText('Require MFA for resources')).toBeInTheDocument()
  })

  it('has a card per feature with its walk-through buttons', () => {
    render(<MemoryRouter><FeatureTour /></MemoryRouter>)
    for (const label of [
      'Show me: add a network and a connector',
      'Show me: add a shield',
      'Show me: add a resource',
      'Show me: add a user',
      'Show me: create a group',
      'Show me: give a group access',
      'Show me: create a device profile',
      'Show me: create a resource policy',
      'Take the guided tour',
    ]) {
      expect(screen.getByRole('button', { name: label })).toBeInTheDocument()
    }
  })

  it('links only to console routes that exist', () => {
    const known = ['/settings', '/policies', '/idp-connections', '/users', '/devices', '/remote-networks', '/connectors', '/shields', '/resources', '/resource-discovery', '/groups', '/access-log', '/audit-log']
    for (const f of tourFeatures) {
      for (const s of f.sections) {
        if (s.link) expect(known, `${f.id}: ${s.link.to}`).toContain(s.link.to)
      }
    }
  })
})
