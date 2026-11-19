import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import TenantExport from './TenantExport'
import { tokenFromHash } from '@/lib/tenantExport'

// Sprint 23 S4: a suspended workspace's admin downloads its own logs. The
// token comes in the URL fragment, is removed from the address bar at once,
// and is sent only in POST bodies.

type Call = { url: string; init?: RequestInit }

function stubFetch(responses: Record<string, Response | (() => Response)>) {
  const calls: Call[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url, init })
      const r = responses[url]
      if (!r) throw new Error('unexpected ' + url)
      return typeof r === 'function' ? r() : r
    }),
  )
  return calls
}

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

describe('tenant export page', () => {
  beforeEach(() => {
    window.history.replaceState(null, '', '/tenant-export#token=tok-1234567890abcdefghijkl')
    URL.createObjectURL = vi.fn(() => 'blob:x')
    URL.revokeObjectURL = vi.fn()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('reads the token from the fragment only', () => {
    expect(tokenFromHash('#token=abc')).toBe('abc')
    expect(tokenFromHash('')).toBe('')
  })

  it('drops the token from the URL, shows the workspace and deadline, downloads by POST', async () => {
    const calls = stubFetch({
      '/api/tenant-export/info': json(200, { workspace_name: 'Acme', slug: 'acme', delete_after: '2026-12-18T10:00:00Z' }),
      '/api/tenant-export/download': () =>
        new Response('gz', { status: 200, headers: { 'Content-Type': 'application/gzip', 'Content-Disposition': 'attachment; filename="zecurity-acme-logs.tar.gz"' } }),
    })
    render(<TenantExport />)
    expect(await screen.findByText('Workspace suspended')).toBeInTheDocument()
    expect(screen.getByText('Acme')).toBeInTheDocument()
    expect(window.location.hash).toBe('')
    expect(window.location.href).not.toContain('tok-')
    await userEvent.setup().click(screen.getByRole('button', { name: 'Download logs' }))
    expect(await screen.findByText(/Download started/)).toBeInTheDocument()
    expect(calls.map((c) => [c.url, c.init?.method, c.init?.body])).toEqual([
      ['/api/tenant-export/info', 'POST', JSON.stringify({ token: 'tok-1234567890abcdefghijkl' })],
      ['/api/tenant-export/download', 'POST', JSON.stringify({ token: 'tok-1234567890abcdefghijkl' })],
    ])
    expect(calls.every((c) => !c.url.includes('tok-'))).toBe(true)
  })

  it('an expired or used link explains how to get a new one', async () => {
    stubFetch({ '/api/tenant-export/info': json(404, { error: 'invalid_or_expired' }) })
    render(<TenantExport />)
    expect(await screen.findByText('Download link not valid')).toBeInTheDocument()
    expect(screen.getByText(/sign in again to get a new download/i)).toBeInTheDocument()
  })

  it('a used sign-in token on download says so', async () => {
    stubFetch({
      '/api/tenant-export/info': json(200, { workspace_name: 'Acme', slug: 'acme', delete_after: '2026-12-18T10:00:00Z' }),
      '/api/tenant-export/download': json(404, { error: 'invalid_or_expired' }),
    })
    render(<TenantExport />)
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Download logs' }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/expired or was already used/))
  })
})
