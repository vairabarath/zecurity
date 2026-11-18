// Relay link capacity helpers (Sprint 23 B2, F5; D-42). The API uses bytes
// per second, like F5's bandwidth; network links are sold and configured in
// bits per second, so the console shows and takes Mbps / Gbps:
// 1 Mbps = 125,000 B/s, 1 Gbps = 125,000,000 B/s.

export type LinkUnit = 'Mbps' | 'Gbps'

const BYTES_PER_UNIT: Record<LinkUnit, number> = { Mbps: 125_000, Gbps: 125_000_000 }

/** The server's bound: 400 Gbit/s in bytes per second. */
export const MAX_LINK_CAPACITY_BPS = 50_000_000_000

/** An Overview attention item when a relay uses at least this share of its link. */
export const NEAR_CAPACITY = 0.9

/** "1 Gbps", "100 Mbps", "2.5 Gbps": a link speed from bytes per second. */
export function formatLinkSpeed(bytesPerSec: number): string {
  const mbps = bytesPerSec / BYTES_PER_UNIT.Mbps
  if (mbps >= 1000) {
    const gbps = mbps / 1000
    return `${gbps.toLocaleString(undefined, { maximumFractionDigits: gbps >= 100 ? 0 : 2 })} Gbps`
  }
  return `${mbps.toLocaleString(undefined, { maximumFractionDigits: mbps >= 100 ? 0 : 2 })} Mbps`
}

/** The form value for an existing capacity: whole Gbps when exact, else Mbps. */
export function linkSpeedInput(bytesPerSec: number | null): { value: string; unit: LinkUnit } {
  if (bytesPerSec === null) return { value: '', unit: 'Gbps' }
  if (bytesPerSec % BYTES_PER_UNIT.Gbps === 0) return { value: String(bytesPerSec / BYTES_PER_UNIT.Gbps), unit: 'Gbps' }
  return { value: String(bytesPerSec / BYTES_PER_UNIT.Mbps), unit: 'Mbps' }
}

/**
 * Parses "1", "2.5", "100" in a unit into whole bytes per second; null when it
 * isn't a positive number or exceeds the server's bound.
 */
export function parseLinkSpeed(value: string, unit: LinkUnit): number | null {
  const t = value.trim()
  if (!/^\d+(\.\d+)?$/.test(t)) return null
  const bps = Math.round(Number(t) * BYTES_PER_UNIT[unit])
  if (!Number.isFinite(bps) || bps <= 0 || bps > MAX_LINK_CAPACITY_BPS) return null
  return bps
}

/** "88 %": utilization as a whole percentage. */
export function formatUtilization(u: number): string {
  return `${Math.round(u * 100)} %`
}
