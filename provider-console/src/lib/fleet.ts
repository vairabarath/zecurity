import { listRelays } from '@/api/reads'
import type { Page, RelaySummary } from '@/api/types'

export const RELAY_PAGE_LIMIT = 200
export const MAX_RELAY_PAGES = 10

export interface Fleet {
  relays: RelaySummary[]
  /** More relays exist than were read (MAX_RELAY_PAGES reached). */
  truncated: boolean
}

/** Every relay, page by page (GET /provider/relays; relay.read: both roles), up to 2,000. */
export async function fetchFleet(): Promise<Fleet> {
  const relays: RelaySummary[] = []
  let cursor: string | undefined
  for (let i = 0; i < MAX_RELAY_PAGES; i++) {
    const page: Page<RelaySummary> = await listRelays({ limit: RELAY_PAGE_LIMIT, cursor })
    relays.push(...page.items)
    if (!page.next_cursor) return { relays, truncated: false }
    cursor = page.next_cursor
  }
  return { relays, truncated: true }
}
