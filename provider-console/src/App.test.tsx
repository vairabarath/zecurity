import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { AppRoutes } from './App'

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AppRoutes />
    </MemoryRouter>,
  )
}

describe('provider console scaffold', () => {
  it('renders the Provider Console identity at /', () => {
    renderAt('/')
    expect(screen.getByText('Provider Console')).toBeInTheDocument()
    expect(screen.getByText('Zecurity')).toBeInTheDocument()
  })

  it('renders a not-found page for unknown routes', () => {
    renderAt('/does-not-exist')
    expect(screen.getByRole('heading', { name: 'Page not found' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /back to the provider console/i })).toHaveAttribute('href', '/')
  })
})
