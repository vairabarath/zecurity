import { request } from './client'

// Break-glass banner (Sprint 27 O2 with G1; D-75). Super-admin only; the
// controller decides. The acknowledge is a console write (D-30 as amended by
// D-75), listed in api-surface.test.ts on purpose.

export interface BreakGlassEvent {
  id: string
  /** The super-admin recovered with recover-admin on the controller host. */
  email: string
  reason: string
  host: string
  created_at: string
  notified_at: string | null
  notified_count: number | null
}

/** Events no super-admin has acknowledged yet, oldest first. */
export function listBreakGlass(): Promise<{ items: BreakGlassEvent[] }> {
  return request<{ items: BreakGlassEvent[] }>('GET', '/provider/break-glass')
}

/** Acknowledge one event; the note is required and audited. */
export function acknowledgeBreakGlass(id: string, note: string): Promise<void> {
  return request<void>('POST', `/provider/break-glass/${encodeURIComponent(id)}/ack`, { body: { note } })
}
