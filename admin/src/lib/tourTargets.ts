import type { GuidedStep } from '@/lib/guidedTours'

// Finds the element a guided-tour step points at. A step names its target
// one of four ways, in this order:
//   target   a data-tour="…" attribute
//   selector a CSS selector
//   button   the visible text of a button or link (prefix match)
//   field    a form field, by its <label> text (prefix match), placeholder
//            or aria-label; falls back to the label itself
// When a dialog is open, matches inside it win. The tour's own popover is
// never a match.

const TOUR_ROOT = '[data-guided-tour]'

function visible(el: Element): boolean {
  return el.getClientRects().length > 0 || import.meta.env.MODE === 'test'
}

function usable(el: Element): el is HTMLElement {
  return el instanceof HTMLElement && !el.closest(TOUR_ROOT) && visible(el)
}

function norm(text: string | null | undefined): string {
  return (text ?? '').replace(/\s+/g, ' ').trim()
}

/** Open dialogs, innermost (last rendered) first. */
function dialogScopes(): HTMLElement[] {
  return [...document.querySelectorAll<HTMLElement>('[role="dialog"], [data-tour-dialog]')]
    .filter((d) => usable(d))
    .reverse()
}

function inScopes<T extends HTMLElement>(pick: (root: ParentNode) => T | null): T | null {
  for (const scope of dialogScopes()) {
    const hit = pick(scope)
    if (hit) return hit
  }
  return pick(document)
}

function lastMatch<T extends HTMLElement>(els: Iterable<Element>, ok: (el: HTMLElement) => boolean): T | null {
  let hit: T | null = null
  for (const el of els) if (usable(el) && ok(el)) hit = el as T
  return hit
}

function findButton(text: string): HTMLElement | null {
  return inScopes((root) =>
    lastMatch(root.querySelectorAll('button, a, [role="button"], [role="tab"]'), (el) =>
      norm(el.textContent).startsWith(text),
    ),
  )
}

function findField(text: string): HTMLElement | null {
  return inScopes((root) => {
    const control = (el: HTMLElement): HTMLElement | null => {
      if (el instanceof HTMLLabelElement && el.htmlFor) {
        const byId = document.getElementById(el.htmlFor)
        if (byId) return byId
      }
      return el.parentElement?.querySelector<HTMLElement>('input, select, textarea, [role="combobox"]') ?? null
    }
    const label = lastMatch<HTMLElement>(root.querySelectorAll('label'), (el) => norm(el.textContent).startsWith(text))
    if (label) return control(label) ?? label
    return lastMatch(
      root.querySelectorAll('input, select, textarea'),
      (el) => el.getAttribute('placeholder') === text || el.getAttribute('aria-label') === text,
    )
  })
}

export function findStepTarget(step: GuidedStep): HTMLElement | null {
  if (step.target) {
    const el = document.querySelector(`[data-tour="${CSS.escape(step.target)}"]`)
    return el && usable(el) ? el : null
  }
  if (step.selector) {
    const el = inScopes((root) => root.querySelector<HTMLElement>(step.selector!))
    return el && usable(el) ? el : null
  }
  if (step.button) return findButton(step.button)
  if (step.field) return findField(step.field)
  return null
}

/** A stable description of what a step points at (for keys and tests). */
export function describeStep(step: GuidedStep): string {
  return step.target ?? step.selector ?? (step.button ? `button:${step.button}` : `field:${step.field ?? ''}`)
}
