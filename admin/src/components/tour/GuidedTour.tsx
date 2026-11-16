import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { useLocation, useNavigate } from 'react-router-dom'
import { X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { markPageTourSeen, pageTours, pageTourSeen } from '@/lib/guidedTours'
import { describeStep, findStepTarget } from '@/lib/tourTargets'
import { useGuidedTour } from '@/store/guidedTour'

// Spotlight + popover for the running guided tour. Mounted once in AppShell.
// Also starts a page's tour the first time that page is opened.

const POPOVER_WIDTH = 340
const GAP = 12
const FIND_TIMEOUT_MS = 4000
const SUBMIT_WAIT_MS = 6000
// Room the popover needs below a target before it goes above instead.
const POPOVER_ROOM = 240

type Rect = { top: number; left: number; width: number; height: number }

function isUnavailable(el: HTMLElement): boolean {
  return (el as HTMLButtonElement).disabled === true || el.getAttribute('aria-disabled') === 'true'
}

function isTyping(el: Element | null): boolean {
  return (
    el instanceof HTMLInputElement ||
    el instanceof HTMLTextAreaElement ||
    el instanceof HTMLSelectElement ||
    (el instanceof HTMLElement && el.isContentEditable)
  )
}

// Fields the user fills in during a step.
function isFillable(el: HTMLElement): boolean {
  return el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement || el instanceof HTMLSelectElement
}

export function GuidedTour() {
  const { steps, index, start, next: advance, back, goTo, stop } = useGuidedTour()
  const location = useLocation()
  const navigate = useNavigate()
  const step = steps[index]
  // The target found for a step, keyed by that step: a result for another
  // step is stale and ignored. rect is null when the target never appeared.
  const [found, setFound] = useState<{ key: string; rect: Rect | null; unavailable?: boolean } | null>(null)
  const stepKey = step ? `${index}:${describeStep(step)}:${location.pathname}` : ''
  const layerRef = useRef<HTMLDivElement>(null)
  // The element the current step points at, once found.
  const targetRef = useRef<HTMLElement | null>(null)
  const [hint, setHint] = useState<{ key: string; text: string } | null>(null)

  // Next on a `click` or `submit` step presses its button; the click
  // listener below then moves the tour on (the same as when the user clicks
  // the button themselves).
  // - click: when the button is missing or turned off, the steps inside what
  //   it would open are skipped.
  // - submit: when the button is turned off, the form is not complete yet;
  //   the tour stays and says so.
  const next = useCallback(() => {
    const { steps: all, index: at } = useGuidedTour.getState()
    const s = all[at]
    if (!s?.click && !s?.submit) {
      advance()
      return
    }
    const live = targetRef.current?.isConnected ? targetRef.current : findStepTarget(s)
    if (live && !isUnavailable(live)) {
      live.click()
      return
    }
    if (s.submit) {
      setHint({ key: `${at}`, text: 'The button is turned off: complete the form first (check its messages), then press Next or click the button.' })
      return
    }
    let j = at + 1
    while (j < all.length && !all[j].click && !all[j].submit && !all[j].route) j++
    goTo(j)
  }, [advance, goTo])

  // A click on the step's button (by the user or by Next) moves the tour on,
  // after the page has handled it. A submit moves on only once it went
  // through: its button is gone (the dialog closed or the page changed). If
  // the button is still there, the form refused it; the tour stays and
  // points at the form's message.
  useEffect(() => {
    if (!step || (!step.click && !step.submit)) return
    const at = index
    const submit = !!step.submit
    let waiting: number | undefined
    const onClick = (e: MouseEvent) => {
      const el = targetRef.current
      if (!el || !(e.target instanceof Node) || !el.contains(e.target) || isUnavailable(el)) return
      if (!submit) {
        window.setTimeout(() => {
          if (useGuidedTour.getState().index === at) goTo(at + 1)
        }, 0)
        return
      }
      const began = Date.now()
      window.clearInterval(waiting)
      waiting = window.setInterval(() => {
        if (useGuidedTour.getState().index !== at) {
          window.clearInterval(waiting)
        } else if (!el.isConnected) {
          window.clearInterval(waiting)
          goTo(at + 1)
        } else if (Date.now() - began > SUBMIT_WAIT_MS) {
          window.clearInterval(waiting)
          setHint({
            key: `${at}`,
            text: 'It didn’t go through yet. Check the message in the form, fix it, and click the button again.',
          })
        }
      }, 150)
    }
    document.addEventListener('click', onClick, true)
    return () => {
      document.removeEventListener('click', onClick, true)
      window.clearInterval(waiting)
    }
  }, [step, index, goTo])
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
    targetRef.current = null
    const measure = () => {
      if (!el || cancelled) return
      const r = el.getBoundingClientRect()
      setFound({
        key,
        rect: { top: r.top, left: r.left, width: r.width, height: r.height },
        unavailable: step.click ? isUnavailable(el) : false,
      })
    }
    const began = Date.now()
    const poll = window.setInterval(() => {
      if (cancelled) return
      el = findStepTarget(step)
      if (el) {
        targetRef.current = el
        window.clearInterval(poll)
        el.scrollIntoView?.({ block: 'center', behavior: 'instant' as ScrollBehavior })
        measure()
      } else if (Date.now() - began > FIND_TIMEOUT_MS) {
        window.clearInterval(poll)
        setFound({ key, rect: null, unavailable: !!step.click })
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
      // Arrows move the caret while the user types in a field.
      else if (isTyping(document.activeElement)) return
      else if (e.key === 'ArrowRight') next()
      else if (e.key === 'ArrowLeft') back()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [step, next, back, stop])

  const ready = !!step && found?.key === stepKey
  // A field to fill gets the focus, so the user can type at once; otherwise
  // the Next button does.
  useEffect(() => {
    if (!ready) return
    const el = targetRef.current
    if (step && !step.click && !step.submit && el && isFillable(el)) el.focus()
    else nextRef.current?.focus()
  }, [ready, stepKey, step])

  // Keep the tour's own clicks and focus from reaching the page's dialogs:
  // a Radix dialog closes on a pointer-down outside it and pulls focus back.
  useEffect(() => {
    const layer = layerRef.current
    if (!layer) return
    const stop = (e: Event) => e.stopPropagation()
    layer.addEventListener('pointerdown', stop)
    layer.addEventListener('mousedown', stop)
    layer.addEventListener('focusin', stop)
    return () => {
      layer.removeEventListener('pointerdown', stop)
      layer.removeEventListener('mousedown', stop)
      layer.removeEventListener('focusin', stop)
    }
  }, [ready])

  if (!step || !ready) return null
  const rect = found?.rect ?? null

  const pad = 6
  // Below the target when there is room, otherwise above it, anchored by its
  // bottom edge so a taller popover (a hint) never covers the target.
  let popPos: React.CSSProperties = { top: window.innerHeight / 2 - 90 }
  let popLeft = window.innerWidth / 2 - POPOVER_WIDTH / 2
  if (rect) {
    const below = rect.top + rect.height + pad + GAP
    popPos =
      below + POPOVER_ROOM < window.innerHeight
        ? { top: below }
        : { bottom: Math.max(GAP, window.innerHeight - (rect.top - pad - GAP)) }
    popLeft = Math.min(Math.max(GAP, rect.left), window.innerWidth - POPOVER_WIDTH - GAP)
  }
  const last = index === steps.length - 1

  // Back cannot undo a click the tour made (a dialog it opened, a page it
  // went to), so it is offered only when the previous step clicked nothing.
  const canGoBack = index > 0 && !steps[index - 1]?.click && !steps[index - 1]?.submit
  const shownHint = hint?.key === `${index}` ? hint.text : null
  const hole = rect
    ? { top: rect.top - pad, left: rect.left - pad, width: rect.width + pad * 2, height: rect.height + pad * 2 }
    : null
  // Blocks the page except the highlighted element, which the user can type
  // in or click. It sits under open dialogs (z-50), so a dialog the walk-
  // through opened stays usable.
  const blockers = hole
    ? [
        { top: 0, left: 0, right: 0, height: Math.max(0, hole.top) },
        { top: hole.top + hole.height, left: 0, right: 0, bottom: 0 },
        { top: hole.top, left: 0, width: Math.max(0, hole.left), height: hole.height },
        { top: hole.top, left: hole.left + hole.width, right: 0, height: hole.height },
      ]
    : [{ top: 0, left: 0, right: 0, bottom: 0 }]

  return createPortal(
    <div ref={layerRef} data-guided-tour style={{ pointerEvents: 'auto' }}>
      {blockers.map((b, i) => (
        <div key={i} data-testid="tour-blocker" className="fixed z-[45]" style={b} onClick={(e) => e.stopPropagation()} />
      ))}
      {hole ? (
        <div
          data-testid="tour-spotlight"
          className="pointer-events-none fixed z-[91] rounded-xl ring-2 ring-primary transition-all"
          style={{ ...hole, boxShadow: '0 0 0 9999px oklch(0.12 0.02 255 / 0.62)' }}
        />
      ) : (
        <div className="pointer-events-none fixed inset-0 z-[91] bg-[oklch(0.12_0.02_255/0.62)]" />
      )}
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="tour-title"
        className="fixed z-[92] rounded-2xl border border-border bg-card p-4 text-sm shadow-2xl"
        style={{ ...popPos, left: popLeft, width: POPOVER_WIDTH, maxHeight: `calc(100vh - ${GAP * 2}px)`, overflowY: 'auto' }}
      >
        <div className="mb-1 flex items-start justify-between gap-2">
          <h3 id="tour-title" className="font-semibold">{step.title}</h3>
          <button type="button" onClick={stop} aria-label="Close tour" className="text-muted-foreground hover:text-foreground">
            <X className="h-4 w-4" />
          </button>
        </div>
        <p className="text-muted-foreground">{step.body}</p>
        {shownHint && (
          <p role="status" className="mt-2 rounded-lg bg-secondary px-2.5 py-1.5 text-xs text-foreground">
            {shownHint}
          </p>
        )}
        {found?.unavailable && (
          <p className="mt-2 rounded-lg bg-secondary px-2.5 py-1.5 text-xs text-muted-foreground">
            {rect
              ? 'This button is turned off right now (for example, there is nothing left to add), so the tour skips this part.'
              : 'This isn’t on the page right now, so the tour skips this part.'}
          </p>
        )}
        <div className="mt-4 flex items-center justify-between gap-2">
          <span className="text-xs text-muted-foreground">{index + 1} of {steps.length}</span>
          <div className="flex gap-2">
            {!last && (
              <Button variant="ghost" size="sm" onClick={stop}>Skip</Button>
            )}
            {canGoBack && (
              <Button variant="outline" size="sm" onClick={back}>Back</Button>
            )}
            <Button ref={nextRef} size="sm" onClick={next}>{last ? 'Done' : 'Next'}</Button>
          </div>
        </div>
      </div>
    </div>,
    document.body,
  )
}
