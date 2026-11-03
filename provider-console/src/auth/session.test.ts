import { describe, expect, it, vi } from 'vitest'
import { superAdminMe } from '@/test/fetch'
import { SESSION_STORAGE_KEY, readStoredSession, useSessionStore } from './session'

describe('session store', () => {
  it('keeps the token in memory and sessionStorage only', () => {
    useSessionStore.getState().startSession('tok-secret', 900, false)
    const stored = JSON.parse(sessionStorage.getItem(SESSION_STORAGE_KEY)!)
    expect(stored).toMatchObject({ token: 'tok-secret', pwcOnly: false })
    expect(localStorage.length).toBe(0)
    expect(window.location.href).not.toContain('tok-secret')
    expect(useSessionStore.getState()).toMatchObject({ token: 'tok-secret', status: 'loading' })
  })

  it('a password-change-only token goes straight to the password-change state', () => {
    useSessionStore.getState().startSession('tok-pwc', 900, true)
    expect(useSessionStore.getState()).toMatchObject({ status: 'password_change', pwcOnly: true })
  })

  it('setMe completes sign-in', () => {
    useSessionStore.getState().startSession('t', 900, false)
    useSessionStore.getState().setMe(superAdminMe)
    expect(useSessionStore.getState()).toMatchObject({ status: 'authenticated', me: superAdminMe })
  })

  it('endSession clears memory and storage and records the reason', () => {
    useSessionStore.getState().startSession('t', 900, false)
    useSessionStore.getState().setMe(superAdminMe)
    useSessionStore.getState().endSession('logged_out')
    expect(useSessionStore.getState()).toMatchObject({
      status: 'anonymous',
      token: undefined,
      expiresAt: undefined,
      me: undefined,
      pwcOnly: false,
      endReason: 'logged_out',
    })
    expect(sessionStorage.getItem(SESSION_STORAGE_KEY)).toBeNull()
  })

  it('ends the session at expiry', () => {
    vi.useFakeTimers()
    useSessionStore.getState().startSession('t', 900, false)
    useSessionStore.getState().setMe(superAdminMe)
    vi.advanceTimersByTime(899_000)
    expect(useSessionStore.getState().status).toBe('authenticated')
    vi.advanceTimersByTime(1_000)
    expect(useSessionStore.getState()).toMatchObject({ status: 'anonymous', endReason: 'expired', token: undefined })
    expect(sessionStorage.getItem(SESSION_STORAGE_KEY)).toBeNull()
  })

  it('a new session replaces the previous expiry timer', () => {
    vi.useFakeTimers()
    useSessionStore.getState().startSession('old', 60, false)
    vi.advanceTimersByTime(30_000)
    useSessionStore.getState().startSession('new', 900, false)
    vi.advanceTimersByTime(60_000)
    expect(useSessionStore.getState().token).toBe('new')
  })

  describe('readStoredSession', () => {
    it('returns a valid entry', () => {
      const expiresAt = Date.now() + 60_000
      sessionStorage.setItem(SESSION_STORAGE_KEY, JSON.stringify({ token: 't', expiresAt, pwcOnly: false }))
      expect(readStoredSession()).toEqual({ token: 't', expiresAt, pwcOnly: false })
    })

    it.each([
      ['expired', JSON.stringify({ token: 't', expiresAt: Date.now() - 1, pwcOnly: false })],
      ['not JSON', '{nope'],
      ['missing token', JSON.stringify({ expiresAt: Date.now() + 60_000, pwcOnly: false })],
      ['wrong types', JSON.stringify({ token: 1, expiresAt: '2', pwcOnly: 'no' })],
    ])('drops and removes an entry that is %s', (_name, raw) => {
      sessionStorage.setItem(SESSION_STORAGE_KEY, raw)
      expect(readStoredSession()).toBeUndefined()
      expect(sessionStorage.getItem(SESSION_STORAGE_KEY)).toBeNull()
    })
  })
})
