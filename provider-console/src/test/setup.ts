import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, vi } from 'vitest'
import { cleanup } from '@testing-library/react'
import { useSessionStore } from '@/auth/session'

// jsdom lacks the pointer-capture and scrollIntoView APIs Radix Select calls.
// Test-only no-ops; the app itself never needs them stubbed.
if (!Element.prototype.hasPointerCapture) {
  Element.prototype.hasPointerCapture = () => false
  Element.prototype.setPointerCapture = () => {}
  Element.prototype.releasePointerCapture = () => {}
}
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {}
}

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
