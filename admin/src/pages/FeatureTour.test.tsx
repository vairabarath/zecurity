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
    expect(screen.getByText('Open Users').closest('a')).toHaveAttribute('href', '/users')
    expect(screen.getByText('zecurity-client auth').tagName).toBe('CODE')
  })

  it('collapses and expands a card', () => {
    render(<MemoryRouter><FeatureTour /></MemoryRouter>)
    const toggle = screen.getByRole('button', { expanded: true })
    fireEvent.click(toggle)
    expect(screen.queryByText('Require MFA for resources')).not.toBeInTheDocument()
    fireEvent.click(toggle)
    expect(screen.getByText('Require MFA for resources')).toBeInTheDocument()
  })

  it('links only to console routes that exist', () => {
    const known = ['/settings', '/policies', '/idp-connections', '/users', '/devices']
    for (const f of tourFeatures) {
      for (const s of f.sections) {
        if (s.link) expect(known, `${f.id}: ${s.link.to}`).toContain(s.link.to)
      }
    }
  })
})
