import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { useLocation, useNavigate } from 'react-router-dom'
import { X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { markPageTourSeen, pageTours, pageTourSeen } from '@/lib/guidedTours'
import { useGuidedTour } from '@/store/guidedTour'

// Spotlight + popover for the running guided tour. Mounted once in AppShell.
// Also starts a page's tour the first time that page is opened.

const POPOVER_WIDTH = 340
const GAP = 12
const FIND_TIMEOUT_MS = 4000

type Rect = { top: number; left: number; width: number; height: number }

function findTarget(id: string): HTMLElement | null {
  return document.querySelector<HTMLElement>(`[data-tour="${CSS.escape(id)}"]`)
}

export function GuidedTour() {
  const { steps, index, start, next, back, stop } = useGuidedTour()
  const location = useLocation()
  const navigate = useNavigate()
  const step = steps[index]
  // The target found for a step, keyed by that step: a result for another
  // step is stale and ignored. rect is null when the target never appeared.
  const [found, setFound] = useState<{ key: string; rect: Rect | null } | null>(null)
  const stepKey = step ? `${index}:${step.target}:${location.pathname}` : ''
  const nextRef = useRef<HTMLButtonElement>(null)

  // First visit to a page with a tour: start it.
  useEffect(() => {
    const path = location.pathname
    if (steps.length > 0 || !pageTours[path] || pageTourSeen(path)) return
    markPageTourSeen(path)
    start(pageTours[path])
  }, [location.pathname, steps.length, start])

  // Feature tours span pages: go to the step's page. A page a tour has shown
  // counts as seen, so ending a tour there does not start the page's own.
  useEffect(() => {
    if (step?.route && location.pathname !== step.route) navigate(step.route)
    else if (step && pageTours[location.pathname]) markPageTourSeen(location.pathname)
  }, [step, location.pathname, navigate])

  // Find the target (pages load their data first), bring it into view, and
  // follow it while the page scrolls or resizes.
  useLayoutEffect(() => {
    if (!step || (step.route && location.pathname !== step.route)) return
    const key = stepKey
    let el: HTMLElement | null = null
    let cancelled = false
    const measure = () => {
      if (!el || cancelled) return
      const r = el.getBoundingClientRect()
      setFound({ key, rect: { top: r.top, left: r.left, width: r.width, height: r.height } })
    }
    const began = Date.now()
    const poll = window.setInterval(() => {
      if (cancelled) return
      el = findTarget(step.target)
      if (el) {
        window.clearInterval(poll)
        el.scrollIntoView?.({ block: 'center', behavior: 'instant' as ScrollBehavior })
        measure()
      } else if (Date.now() - began > FIND_TIMEOUT_MS) {
        window.clearInterval(poll)
        setFound({ key, rect: null })
      }
    }, 100)
    window.addEventListener('resize', measure)
    window.addEventListener('scroll', measure, true)
    return () => {
      cancelled = true
      window.clearInterval(poll)
      window.removeEventListener('resize', measure)
      window.removeEventListener('scroll', measure, true)
    }
  }, [step, stepKey, location.pathname])

  // Keyboard: Esc ends the tour, arrows move.
  useEffect(() => {
    if (!step) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') stop()
      else if (e.key === 'ArrowRight') next()
      else if (e.key === 'ArrowLeft') back()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [step, next, back, stop])

  const ready = !!step && found?.key === stepKey
  useEffect(() => {
    if (ready) nextRef.current?.focus()
  }, [ready, stepKey])

  if (!step || !ready) return null
  const rect = found?.rect ?? null

  const pad = 6
  let popTop = window.innerHeight / 2 - 90
  let popLeft = window.innerWidth / 2 - POPOVER_WIDTH / 2
  if (rect) {
    const below = rect.top + rect.height + pad + GAP
    popTop = below + 190 < window.innerHeight ? below : Math.max(GAP, rect.top - pad - GAP - 190)
    popLeft = Math.min(Math.max(GAP, rect.left), window.innerWidth - POPOVER_WIDTH - GAP)
  }
  const last = index === steps.length - 1

  return createPortal(
    <>
      {/* Blocks clicks on the page while the tour runs. */}
      <div className="fixed inset-0 z-[90]" onClick={(e) => e.stopPropagation()} />
      {rect ? (
        <div
          data-testid="tour-spotlight"
          className="pointer-events-none fixed z-[91] rounded-xl ring-2 ring-primary transition-all"
          style={{
            top: rect.top - pad,
            left: rect.left - pad,
            width: rect.width + pad * 2,
            height: rect.height + pad * 2,
            boxShadow: '0 0 0 9999px oklch(0.12 0.02 255 / 0.62)',
          }}
        />
      ) : (
        <div className="fixed inset-0 z-[91] bg-[oklch(0.12_0.02_255/0.62)]" />
      )}
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="tour-title"
        className="fixed z-[92] rounded-2xl border border-border bg-card p-4 text-sm shadow-2xl"
        style={{ top: popTop, left: popLeft, width: POPOVER_WIDTH }}
      >
        <div className="mb-1 flex items-start justify-between gap-2">
          <h3 id="tour-title" className="font-semibold">{step.title}</h3>
          <button type="button" onClick={stop} aria-label="Close tour" className="text-muted-foreground hover:text-foreground">
            <X className="h-4 w-4" />
          </button>
        </div>
        <p className="text-muted-foreground">{step.body}</p>
        <div className="mt-4 flex items-center justify-between gap-2">
          <span className="text-xs text-muted-foreground">{index + 1} of {steps.length}</span>
          <div className="flex gap-2">
            {!last && (
              <Button variant="ghost" size="sm" onClick={stop}>Skip</Button>
            )}
            {index > 0 && (
              <Button variant="outline" size="sm" onClick={back}>Back</Button>
            )}
            <Button ref={nextRef} size="sm" onClick={next}>{last ? 'Done' : 'Next'}</Button>
          </div>
        </div>
      </div>
    </>,
    document.body,
  )
}
