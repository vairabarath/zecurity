import type { ReactNode } from 'react'
import { AlertTriangle, Box, Plug, Shield } from 'lucide-react'
import { cn } from '@/lib/utils'

export function relativeTime(dateStr: string | null | undefined): string {
  if (!dateStr) return 'Never'
  const diff = Date.now() - new Date(dateStr).getTime()
  if (diff < 0) return 'just now'
  const seconds = Math.floor(diff / 1000)
  if (seconds < 60) return `${seconds}s ago`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  if (days < 30) return `${days}d ago`
  return `${Math.floor(days / 30)}mo ago`
}

export function StatusPill({
  label,
  tone,
}: {
  label: string
  tone: 'ok' | 'warn' | 'danger' | 'muted' | 'info'
}) {
  const toneClass = {
    ok: 'border-[oklch(0.82_0.12_160/0.28)] bg-[oklch(0.82_0.12_160/0.12)] text-[oklch(0.82_0.12_160)]',
    warn: 'border-[oklch(0.85_0.13_80/0.28)] bg-[oklch(0.85_0.13_80/0.12)] text-[oklch(0.85_0.13_80)]',
    danger: 'border-[oklch(0.75_0.16_25/0.3)] bg-[oklch(0.75_0.16_25/0.12)] text-[oklch(0.75_0.16_25)]',
    muted: 'border-border bg-secondary text-muted-foreground',
    info: 'border-[oklch(0.78_0.10_235/0.28)] bg-[oklch(0.78_0.10_235/0.12)] text-[oklch(0.78_0.10_235)]',
  }[tone]

  const dotClass = {
    ok: 'bg-[oklch(0.82_0.12_160)]',
    warn: 'bg-[oklch(0.85_0.13_80)]',
    danger: 'bg-[oklch(0.75_0.16_25)]',
    muted: 'bg-muted-foreground',
    info: 'bg-[oklch(0.78_0.10_235)]',
  }[tone]

  return (
    <span className={cn('status-pill', toneClass)}>
      <span className={cn('status-pill-dot', dotClass)} />
      <span className="capitalize">{label}</span>
    </span>
  )
}

export function EmptyState({
  icon,
  title,
  description,
  action,
}: {
  icon?: ReactNode
  title: string
  description: string
  action?: ReactNode
}) {
  return (
    <div className="flex flex-col items-center justify-center px-6 py-20 text-center">
      <div className="mb-4 grid h-14 w-14 place-items-center rounded-full border border-primary/20 bg-primary/10 text-primary">
        {icon ?? <AlertTriangle className="h-6 w-6" />}
      </div>
      <h3 className="text-lg font-semibold">{title}</h3>
      <p className="mt-2 max-w-md text-sm text-muted-foreground">{description}</p>
      {action ? <div className="mt-5">{action}</div> : null}
    </div>
  )
}

export function EntityIcon({ type }: { type: 'network' | 'connector' | 'shield' | 'resource' }) {
  const config = {
    network: {
      icon: <span className="text-sm font-bold">N</span>,
      className: 'bg-primary/12 text-primary border border-primary/20',
    },
    connector: {
      icon: <Plug className="h-4 w-4" />,
      className: 'bg-[oklch(0.86_0.095_175/0.14)] text-[oklch(0.86_0.095_175)] border border-[oklch(0.86_0.095_175/0.25)]',
    },
    shield: {
      icon: <Shield className="h-4 w-4" />,
      className: 'bg-[oklch(0.78_0.10_235/0.14)] text-[oklch(0.78_0.10_235)] border border-[oklch(0.78_0.10_235/0.25)]',
    },
    resource: {
      icon: <Box className="h-4 w-4" />,
      className: 'bg-[oklch(0.78_0.09_310/0.14)] text-[oklch(0.78_0.09_310)] border border-[oklch(0.78_0.09_310/0.25)]',
    },
  }[type]

  return (
    <span className={cn('grid h-9 w-9 place-items-center rounded-xl', config.className)}>
      {config.icon}
    </span>
  )
}
