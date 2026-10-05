import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, vi } from 'vitest'
import { cleanup } from '@testing-library/react'
import { useSessionStore } from '@/auth/session'

// Every test starts signed out with empty storage.
beforeEach(() => {
  sessionStorage.clear()
  localStorage.clear()
  useSessionStore.getState().endSession('logged_out')
  useSessionStore.getState().clearEndReason()
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})
