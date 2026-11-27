import { useState } from 'react'
import { planErrorMessage } from '@/lib/plan-format'

/** Runs one write; keeps the dialog open with a message on failure (plan and billing dialogs). */
export function useWrite() {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  async function run(fn: () => Promise<void>) {
    setPending(true)
    setError(null)
    try {
      await fn()
    } catch (err) {
      setError(planErrorMessage(err))
    } finally {
      setPending(false)
    }
  }
  return { pending, error, run }
}
