import { create } from 'zustand'
import type { GuidedStep } from '@/lib/guidedTours'

// The one running guided tour (see lib/guidedTours.ts and
// components/tour/GuidedTour.tsx).

interface GuidedTourState {
  steps: GuidedStep[]
  index: number
  start: (steps: GuidedStep[]) => void
  next: () => void
  back: () => void
  goTo: (index: number) => void
  stop: () => void
}

export const useGuidedTour = create<GuidedTourState>((set, get) => ({
  steps: [],
  index: 0,
  start: (steps) => set({ steps, index: 0 }),
  next: () => {
    const { index, steps } = get()
    if (index + 1 >= steps.length) set({ steps: [], index: 0 })
    else set({ index: index + 1 })
  },
  back: () => set((s) => ({ index: Math.max(0, s.index - 1) })),
  goTo: (index) => {
    const { steps } = get()
    if (index >= steps.length) set({ steps: [], index: 0 })
    else set({ index })
  },
  stop: () => set({ steps: [], index: 0 }),
}))
