import { useId, useState, type PointerEvent, type ReactNode } from 'react'
import type { MetricSeriesData, MetricUnit } from '@/api/metrics'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import {
  formatMetric,
  nearestIndex,
  niceMax,
  RANGES,
  rangeDef,
  segments,
  stepMs,
  summarize,
  type MetricRange,
  type XY,
} from '@/lib/metrics'
import { cn } from '@/lib/utils'
import type { MetricState } from '@/lib/useMetricSeries'

// Charts of stored metric series (Sprint 24 M2, F23, D-49). Hand-written SVG
// (no chart library): a missing point is drawn as a break, never as a line
// across the gap or a drop to zero; hourly ranges add a faint min–max band.
// Each chart is read once when it appears and again when the range changes
// or the page is refreshed (no polling, like every console page). Metric
// reads are not audited.

export function RangePicker({ value, onChange, label = 'Time range' }: { value: MetricRange; onChange: (r: MetricRange) => void; label?: string }) {
  return (
    <div role="group" aria-label={label} className="inline-flex rounded-md border border-border p-0.5">
      {RANGES.map((r) => (
        <Button
          key={r.key}
          type="button"
          size="sm"
          variant={r.key === value ? 'secondary' : 'ghost'}
          aria-pressed={r.key === value}
          className="h-7 px-2.5 text-xs"
          onClick={() => onChange(r.key)}
        >
          {r.label}
        </Button>
      ))}
    </div>
  )
}

const TONES = {
  primary: { stroke: 'var(--color-primary)', fill: 'var(--color-primary)', dot: 'bg-primary' },
  info: { stroke: 'var(--color-info)', fill: 'var(--color-info)', dot: 'bg-info' },
  warning: { stroke: 'var(--color-warning)', fill: 'var(--color-warning)', dot: 'bg-warning' },
} as const
export type LineTone = keyof typeof TONES

export interface ChartLine {
  label: string
  state: MetricState
  tone?: LineTone
}

const W = 600
const H = 150
const PAD_TOP = 8
const PAD_BOTTOM = 4

function timeLabel(t: number, range: MetricRange): string {
  const d = new Date(t)
  return rangeDef(range).step === '1h'
    ? d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
    : d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
}

/**
 * A line chart of one or more series sharing a unit. `ceiling` draws a
 * dashed reference line (e.g. the relay's link capacity, B2). `empty` is
 * shown when no series has a point in the range.
 */
export function MetricChart({
  title,
  unit,
  lines,
  range,
  ceiling,
  empty = 'No data in this range yet.',
}: {
  title: string
  unit: MetricUnit
  lines: ChartLine[]
  range: MetricRange
  ceiling?: { value: number; label: string } | null
  empty?: ReactNode
}) {
  const id = useId()
  const [hoverT, setHoverT] = useState<number | null>(null)

  const header = (
    <div className="flex flex-wrap items-center justify-between gap-2">
      <h3 id={`${id}-title`} className="text-sm font-medium">
        {title}
      </h3>
      {lines.length > 1 && (
        <ul className="flex flex-wrap gap-3 text-xs text-muted-foreground">
          {lines.map((l) => (
            <li key={l.label} className="inline-flex items-center gap-1.5">
              <span aria-hidden className={cn('size-2 rounded-full', TONES[l.tone ?? 'primary'].dot)} />
              {l.label}
            </li>
          ))}
        </ul>
      )}
    </div>
  )

  if (lines.some((l) => l.state.status === 'loading')) {
    return (
      <figure className="flex flex-col gap-2" aria-busy="true" aria-labelledby={`${id}-title`}>
        {header}
        <Skeleton className="h-[150px] w-full" />
      </figure>
    )
  }
  const failed = lines.find((l) => l.state.status !== 'ready')
  if (failed) {
    const text =
      failed.state.status === 'forbidden'
        ? 'Not available for your role.'
        : failed.state.status === 'not_found'
          ? 'This series is not available.'
          : 'Couldn’t load this chart. Refresh the page to try again.'
    return (
      <figure className="flex flex-col gap-2" aria-labelledby={`${id}-title`}>
        {header}
        <p role="alert" className="flex h-[150px] items-center justify-center rounded-md border border-dashed border-border text-[13px] text-muted-foreground">
          {text}
        </p>
      </figure>
    )
  }

  const data = lines.map((l) => (l.state as { status: 'ready'; data: MetricSeriesData }).data)
  const step = data[0].step
  const from = Date.parse(data[0].from)
  const to = Date.parse(data[0].to)
  const runs = data.map((d) => segments(d.points, d.step))
  const flat = runs.map((r) => r.flat())
  const hasPoints = flat.some((f) => f.length > 0)
  const summaries = data.map((d) => summarize(d.points))
  const top = niceMax(Math.max(0, ...summaries.map((s) => s?.max ?? 0), ceiling?.value ?? 0))
  const x = (t: number) => ((t - from) / (to - from)) * W
  const y = (v: number) => PAD_TOP + (1 - v / top) * (H - PAD_TOP - PAD_BOTTOM)
  const summaryText = lines
    .map((l, i) => {
      const s = summaries[i]
      return s
        ? `${l.label}: latest ${formatMetric(unit, s.latest)}, low ${formatMetric(unit, s.min)}, high ${formatMetric(unit, s.max)}`
        : `${l.label}: no data`
    })
    .join('; ')

  if (!hasPoints) {
    return (
      <figure className="flex flex-col gap-2" aria-labelledby={`${id}-title`}>
        {header}
        <p className="flex h-[150px] items-center justify-center rounded-md border border-dashed border-border text-[13px] text-muted-foreground">
          {empty}
        </p>
      </figure>
    )
  }

  const onMove = (e: PointerEvent<SVGSVGElement>) => {
    const box = e.currentTarget.getBoundingClientRect()
    if (box.width <= 0) return
    setHoverT(from + ((e.clientX - box.left) / box.width) * (to - from))
  }
  const hover =
    hoverT === null
      ? null
      : flat.map((f) => {
          const i = nearestIndex(f, hoverT)
          return i >= 0 && Math.abs(f[i].t - hoverT) <= stepMs(step) * 1.5 ? f[i] : null
        })
  const hoverAt = hover?.find((p): p is XY => p !== null)?.t ?? null

  return (
    <figure className="flex flex-col gap-2" aria-labelledby={`${id}-title`}>
      {header}
      <div className="relative">
        <svg
          viewBox={`0 0 ${W} ${H}`}
          preserveAspectRatio="none"
          className="h-[150px] w-full overflow-visible"
          role="img"
          aria-label={`${title}. ${summaryText}.`}
          onPointerMove={onMove}
          onPointerLeave={() => setHoverT(null)}
        >
          {[0, 0.5, 1].map((f) => (
            <line key={f} x1={0} x2={W} y1={y(top * f)} y2={y(top * f)} stroke="var(--color-border)" strokeWidth={1} vectorEffect="non-scaling-stroke" />
          ))}
          {ceiling && (
            <line
              data-ceiling
              x1={0}
              x2={W}
              y1={y(ceiling.value)}
              y2={y(ceiling.value)}
              stroke="var(--color-warning)"
              strokeDasharray="6 4"
              strokeWidth={1.25}
              vectorEffect="non-scaling-stroke"
            />
          )}
          {runs.map((segs, li) =>
            segs.map((seg, si) => {
              const tone = TONES[lines[li].tone ?? 'primary']
              const band =
                step === '1h' && seg.every((p) => p.min !== undefined && p.max !== undefined)
                  ? [...seg.map((p) => `${x(p.t)},${y(p.max!)}`), ...[...seg].reverse().map((p) => `${x(p.t)},${y(p.min!)}`)].join(' ')
                  : null
              return (
                <g key={`${li}-${si}`} data-segment={lines[li].label}>
                  {band && <polygon data-band points={band} fill={tone.fill} fillOpacity={0.14} stroke="none" />}
                  {seg.length === 1 ? (
                    <circle cx={x(seg[0].t)} cy={y(seg[0].v)} r={2.5} fill={tone.stroke} />
                  ) : (
                    <polyline
                      points={seg.map((p) => `${x(p.t)},${y(p.v)}`).join(' ')}
                      fill="none"
                      stroke={tone.stroke}
                      strokeWidth={1.75}
                      strokeLinejoin="round"
                      vectorEffect="non-scaling-stroke"
                    />
                  )}
                </g>
              )
            }),
          )}
          {hoverAt !== null && <line x1={x(hoverAt)} x2={x(hoverAt)} y1={0} y2={H} stroke="var(--color-muted-foreground)" strokeWidth={1} vectorEffect="non-scaling-stroke" />}
        </svg>
        <span className="pointer-events-none absolute right-0 top-0 font-mono text-[11px] text-muted-foreground">{formatMetric(unit, top)}</span>
        {hoverAt !== null && hover && (
          <div role="status" className="pointer-events-none absolute left-2 top-2 rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs shadow-sm">
            <div className="text-muted-foreground">{new Date(hoverAt).toLocaleString()}</div>
            {lines.map((l, i) => (
              <div key={l.label} className="font-mono tabular-nums">
                {lines.length > 1 && <span className="font-sans text-muted-foreground">{l.label}: </span>}
                {hover[i] ? formatMetric(unit, hover[i]!.v) : 'no data'}
              </div>
            ))}
          </div>
        )}
      </div>
      <div className="flex justify-between text-[11px] text-muted-foreground">
        <span>{timeLabel(from, range)}</span>
        {ceiling && <span className="text-warning-text">— {ceiling.label}</span>}
        <span>{timeLabel(to, range)}</span>
      </div>
      <figcaption className="sr-only">{summaryText}</figcaption>
    </figure>
  )
}

/** A small trend line with the latest value, for the Overview. */
export function Sparkline({ label, unit, state, note }: { label: string; unit: MetricUnit; state: MetricState; note: string }) {
  if (state.status === 'loading') return <Skeleton className="h-12 w-full" />
  if (state.status !== 'ready') return <span className="text-[12.5px] text-muted-foreground">Couldn’t load {label.toLowerCase()}.</span>
  const d = state.data
  const runs = segments(d.points, d.step)
  const s = summarize(d.points)
  if (!s) return <span className="text-[12.5px] text-muted-foreground">No data yet.</span>
  const from = Date.parse(d.from)
  const to = Date.parse(d.to)
  const top = niceMax(s.max)
  const x = (t: number) => ((t - from) / (to - from)) * 200
  const y = (v: number) => 2 + (1 - v / top) * 36
  return (
    <div className="flex flex-col gap-1">
      <span className="text-[26px] font-semibold leading-tight tracking-[-0.02em]">{formatMetric(unit, s.latest)}</span>
      <svg viewBox="0 0 200 40" preserveAspectRatio="none" className="h-10 w-full" role="img" aria-label={`${label}, ${note}: latest ${formatMetric(unit, s.latest)}, high ${formatMetric(unit, s.max)}`}>
        {runs.map((seg, i) =>
          seg.length === 1 ? (
            <circle key={i} cx={x(seg[0].t)} cy={y(seg[0].v)} r={1.5} fill="var(--color-primary)" />
          ) : (
            <polyline key={i} data-segment points={seg.map((p) => `${x(p.t)},${y(p.v)}`).join(' ')} fill="none" stroke="var(--color-primary)" strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
          ),
        )}
      </svg>
      <span className="text-[12.5px] text-muted-foreground">{note}</span>
    </div>
  )
}
