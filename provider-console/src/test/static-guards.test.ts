import { describe, expect, it } from 'vitest'

// Source-level guards for the Phase C invariants, over every non-test file in
// src/. They catch a regression at review time even where no behaviour test
// happens to exercise it.

const sources = import.meta.glob(['/src/**/*.{ts,tsx}', '!/src/**/*.test.{ts,tsx}', '!/src/test/**'], {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

function filesMatching(re: RegExp): string[] {
  return Object.entries(sources)
    .filter(([, text]) => re.test(text))
    .map(([path]) => path)
}

describe('source guards', () => {
  it('scans the app sources', () => {
    expect(Object.keys(sources)).toContain('/src/api/client.ts')
  })

  it('only the API client calls fetch', () => {
    expect(filesMatching(/\bfetch\s*\(/)).toEqual(['/src/api/client.ts'])
  })

  it('nothing uses localStorage', () => {
    expect(filesMatching(/\blocalStorage\s*[.[]/)).toEqual([])
  })

  it('only the session store touches sessionStorage', () => {
    expect(filesMatching(/\bsessionStorage\s*[.[]/)).toEqual(['/src/auth/session.ts'])
  })

  it('nothing logs to the console', () => {
    expect(filesMatching(/\bconsole\s*\./)).toEqual([])
  })

  it('only the temporary-password dialog touches the clipboard', () => {
    expect(filesMatching(/\bclipboard\b/)).toEqual(['/src/components/TemporaryPasswordDialog.tsx'])
  })

  it('never calls tenant endpoints (/graphql, tenant /auth/*)', () => {
    expect(filesMatching(/\/graphql\b/)).toEqual([])
    expect(filesMatching(/['"`]\/auth\//)).toEqual([])
  })
})
