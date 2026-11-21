import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { MetricSeriesData } from '@/api/metrics'
import { MetricChart, Sparkline } from '@/components/MetricChart'
import type { MetricState } from '@/lib/useMetricSeries'

// Sprint 24 M2 (F23, D-49): gaps are breaks, hourly ranges get a min–max
// band, the link capacity is a dashed ceiling, and failures say why.

const FROM = '2026-11-21T10:00:00Z'
const TO = '2026-11-21T11:00:00Z'

function ready(points: MetricSeriesData['points'], over: Partial<MetricSeriesData> = {}): MetricState {
  return { status: 'ready', data: { series: 's', subject: 'r', unit: 'count', step: '1m', from: FROM, to: TO, points, ...over } }
}
const pt = (min: number, value: number, extra: object = {}) => ({ ts: new Date(Date.parse(FROM) + min * 60_000).toISOString(), value, ...extra })

describe('MetricChart', () => {
  it('draws one segment per run: a missing minute breaks the line', () => {
    const { container } = render(
      <MetricChart title="Connections" unit="count" range="1h" lines={[{ label: 'Connections', state: ready([pt(0, 1), pt(1, 2), pt(10, 5), pt(11, 6)]) }]} />,
    )
    expect(container.querySelectorAll('[data-segment]')).toHaveLength(2)
    expect(container.querySelectorAll('polyline')).toHaveLength(2)
  })

  it('summarizes for screen readers: latest, low and high', () => {
    render(<MetricChart title="Connections" unit="count" range="1h" lines={[{ label: 'Connections', state: ready([pt(0, 4), pt(1, 2), pt(2, 3)]) }]} />)
    expect(screen.getByRole('img', { name: /Connections: latest 3, low 2, high 4/ })).toBeInTheDocument()
  })

  it('hourly points get a min–max band', () => {
    const { container } = render(
      <MetricChart
        title="Bandwidth"
        unit="bytes_per_sec"
        range="7d"
        lines={[{ label: 'To clients', state: ready([pt(0, 5, { min: 1, max: 9 }), pt(60, 6, { min: 2, max: 8 })], { step: '1h', to: '2026-11-21T13:00:00Z' }) }]}
      />,
    )
    expect(container.querySelector('[data-band]')).not.toBeNull()
  })

  it('draws the link capacity as a dashed ceiling with its label when the data comes near it', () => {
    const { container } = render(
      <MetricChart title="Bandwidth" unit="bytes_per_sec" range="1h" lines={[{ label: 'Both directions', state: ready([pt(0, 40_000_000), pt(1, 90_000_000)]) }]} ceiling={{ value: 125_000_000, label: 'Link capacity 1 Gbps' }} />,
    )
    expect(container.querySelector('[data-ceiling]')).not.toBeNull()
    expect(container.querySelector('[data-ceiling-label]')).toHaveTextContent('— Link capacity 1 Gbps')
  })

  it('far below the capacity, scales to the data and names the ceiling as off scale (small traffic never reads as zero)', () => {
    const { container } = render(
      <MetricChart title="Bandwidth" unit="bytes_per_sec" range="1h" lines={[{ label: 'Both directions', state: ready([pt(0, 1_000_000), pt(1, 2_000_000)]) }]} ceiling={{ value: 1_250_000_000, label: 'Link capacity 10 Gbps' }} />,
    )
    expect(container.querySelector('[data-ceiling]')).toBeNull()
    expect(container.querySelector('[data-ceiling-label]')).toHaveTextContent('Link capacity 10 Gbps (above this scale)')
    const ys = container.querySelector('polyline')!.getAttribute('points')!.split(' ').map((p) => Number(p.split(',')[1]))
    // The higher point sits in the upper half of the 150-unit-tall chart, not on the baseline.
    expect(Math.min(...ys)).toBeLessThan(75)
  })

  it('the hover tooltip is not a live region (it changes on every pointer move)', () => {
    const { container } = render(<MetricChart title="Connections" unit="count" range="1h" lines={[{ label: 'Connections', state: ready([pt(0, 1), pt(1, 2)]) }]} />)
    const svg = container.querySelector('svg')!
    svg.getBoundingClientRect = () => ({ left: 0, top: 0, width: 600, height: 150, right: 600, bottom: 150, x: 0, y: 0, toJSON: () => ({}) })
    fireEvent.pointerMove(svg, { clientX: 5 })
    const tip = container.querySelector('[data-tooltip]')
    expect(tip).not.toBeNull()
    expect(tip).toHaveAttribute('aria-hidden', 'true')
    expect(tip).not.toHaveAttribute('role')
  })

  it('on 24 h the axis ends show the day, so they differ', () => {
    const { container } = render(
      <MetricChart title="Connections" unit="count" range="24h" lines={[{ label: 'Connections', state: ready([pt(0, 1), pt(1, 2)], { from: '2026-11-20T11:00:00Z', to: TO }) }]} />,
    )
    const labels = [...container.querySelectorAll('[data-axis] > span')].map((s) => s.textContent)
    expect(labels).toHaveLength(2)
    expect(labels[0]).not.toBe(labels[1])
  })

  it('no point in the range shows the empty text, never a zero line', () => {
    const { container } = render(<MetricChart title="Relay traffic" unit="bytes_per_sec" range="1h" lines={[{ label: 'To clients', state: ready([]) }]} empty="Not reported." />)
    expect(screen.getByText('Not reported.')).toBeInTheDocument()
    expect(container.querySelector('svg')).toBeNull()
  })

  it('a refused read says so; a failed one says to refresh', () => {
    const { rerender } = render(<MetricChart title="Sessions" unit="count" range="1h" lines={[{ label: 'S', state: { status: 'forbidden' } }]} />)
    expect(screen.getByRole('alert')).toHaveTextContent('Not available for your role.')
    rerender(<MetricChart title="Sessions" unit="count" range="1h" lines={[{ label: 'S', state: { status: 'error', error: new Error('x') } }]} />)
    expect(screen.getByRole('alert')).toHaveTextContent('Couldn’t load this chart.')
  })

  it('shows a skeleton while loading', () => {
    render(<MetricChart title="Sessions" unit="count" range="1h" lines={[{ label: 'S', state: { status: 'loading' } }]} />)
    expect(screen.getByRole('figure', { busy: true })).toBeInTheDocument()
  })
})

describe('Sparkline', () => {
  it('shows the latest value and breaks at gaps', () => {
    const { container } = render(<Sparkline label="Active sessions" unit="count" state={ready([pt(0, 2), pt(1, 3), pt(20, 7), pt(21, 9)])} note="Last 24 hours" />)
    expect(screen.getByText('9')).toBeInTheDocument()
    expect(container.querySelectorAll('[data-segment]')).toHaveLength(2)
    // A stored sample, not a live count: its time is shown.
    expect(screen.getByText(/Last 24 hours · latest point /)).toBeInTheDocument()
  })
  it('no data yet says so', () => {
    render(<Sparkline label="Active sessions" unit="count" state={ready([])} note="Last 24 hours" />)
    expect(screen.getByText('No data yet.')).toBeInTheDocument()
  })
})
