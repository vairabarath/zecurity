import { useEffect } from 'react'

// Unsaved-changes guard (Sprint 26 P4). While `dirty`, leaving the page
// (reload, closing the tab, typing another address) asks the browser's own
// "Leave site?" prompt. The console uses a plain <BrowserRouter>, not a data
// router, so react-router's useBlocker isn't available and in-app links are
// not blocked; dialogs guard their own close instead.
export function useUnsavedChanges(dirty: boolean) {
  useEffect(() => {
    if (!dirty) return
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault()
      // Older browsers need returnValue set to show the prompt.
      e.returnValue = ''
    }
    window.addEventListener('beforeunload', onBeforeUnload)
    return () => window.removeEventListener('beforeunload', onBeforeUnload)
  }, [dirty])
}

/** True when two form snapshots differ (plain JSON-safe input values). */
export function changedFrom<T>(initial: T, current: T): boolean {
  return JSON.stringify(initial) !== JSON.stringify(current)
}
