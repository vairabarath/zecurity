import type { RelayBandwidth } from '@/api/types'
import { formatDateTime, formatRate, windowText } from '@/lib/format'

// Relay bandwidth (F5), shown exactly as the API reports it: bytes per second
// forwarded on bridged client streams in each direction, averaged over the
// window between two stored heartbeats. Nothing is computed or extrapolated
// here. "Not reported" (an older relay, revoked or deleted) and "Measuring"
// (a first sample, or after a restart) are told apart, so a missing value is
// never shown as zero traffic.

type Rates = { toClient: number; toConnector: number; window: number }

function rates(b: RelayBandwidth): Rates | null {
  if (b.to_client_bytes_per_sec === null || b.to_connector_bytes_per_sec === null || b.window_seconds === null) return null
  return { toClient: b.to_client_bytes_per_sec, toConnector: b.to_connector_bytes_per_sec, window: b.window_seconds }
}

/** Compact two-line cell for the relay list. */
export function BandwidthCell({ bandwidth }: { bandwidth: RelayBandwidth | null }) {
  if (!bandwidth) return <span className="text-muted-foreground">Not reported</span>
  const r = rates(bandwidth)
  if (!r) return <span className="text-muted-foreground" title={`Sampled ${formatDateTime(bandwidth.measured_at)}`}>Measuring…</span>
  return (
    <div
      className="flex flex-col whitespace-nowrap font-mono text-xs tabular-nums"
      aria-label="Bandwidth"
      title={`Average over ${windowText(r.window)}, as of ${formatDateTime(bandwidth.measured_at)}`}
    >
      <span>
        <span className="font-sans text-muted-foreground">to clients </span>
        {formatRate(r.toClient)}
      </span>
      <span>
        <span className="font-sans text-muted-foreground">to connectors </span>
        {formatRate(r.toConnector)}
      </span>
    </div>
  )
}

/** Detail-page block: both directions, the window and when it was measured. */
export function BandwidthDetail({ bandwidth }: { bandwidth: RelayBandwidth | null }) {
  if (!bandwidth) {
    return <p className="text-[13px] text-muted-foreground">Not reported. This relay doesn't send byte counters (an older relay version), or it is revoked.</p>
  }
  const r = rates(bandwidth)
  if (!r) {
    return (
      <p className="text-[13px] text-muted-foreground">
        Measuring. A rate needs two heartbeat samples; the first arrived {formatDateTime(bandwidth.measured_at)} (or the relay restarted).
      </p>
    )
  }
  return (
    <div className="flex flex-col gap-2.5">
      <dl className="grid grid-cols-2 gap-3 text-[13px]">
        <div className="flex flex-col gap-0.5">
          <dt className="text-xs text-muted-foreground">To clients</dt>
          <dd className="font-mono tabular-nums">{formatRate(r.toClient)}</dd>
        </div>
        <div className="flex flex-col gap-0.5">
          <dt className="text-xs text-muted-foreground">To connectors</dt>
          <dd className="font-mono tabular-nums">{formatRate(r.toConnector)}</dd>
        </div>
      </dl>
      <p className="text-[13px] text-subtle-foreground">
        Average over {windowText(r.window)}, as of {formatDateTime(bandwidth.measured_at)}. Bridged client traffic only.
      </p>
    </div>
  )
}
