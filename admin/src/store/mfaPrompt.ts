import { create } from 'zustand'

// The one MFA prompt of the console (PENDING-06). Anything that meets
// MFA_REQUIRED (the Apollo error link, a settings button) calls requestMfa and
// waits: true once a code was verified and the session carries the MFA mark,
// false if the user closed the prompt.
//
// The dashboard fires several queries at once and they are all refused
// together, so concurrent callers share one prompt and one answer.

export type MfaPromptReason = 'console' | 'step-up' | 'self'

interface MfaPromptState {
  open: boolean
  reason: MfaPromptReason
  pending: Promise<boolean> | null
  resolve: ((ok: boolean) => void) | null
  requestMfa: (reason: MfaPromptReason) => Promise<boolean>
  finish: (ok: boolean) => void
}

export const useMfaPrompt = create<MfaPromptState>((set, get) => ({
  open: false,
  reason: 'step-up',
  pending: null,
  resolve: null,

  requestMfa: (reason) => {
    const current = get().pending
    if (current) return current
    let resolve!: (ok: boolean) => void
    const pending = new Promise<boolean>((r) => {
      resolve = r
    })
    set({ open: true, reason, pending, resolve })
    return pending
  },

  finish: (ok) => {
    const { resolve } = get()
    set({ open: false, pending: null, resolve: null })
    resolve?.(ok)
  },
}))

export function requestMfa(reason: MfaPromptReason): Promise<boolean> {
  return useMfaPrompt.getState().requestMfa(reason)
}
