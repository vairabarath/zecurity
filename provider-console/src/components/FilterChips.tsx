import { cn } from '@/lib/utils'

export interface ChipOption {
  value: string
  label: string
}

/** A single-choice row of pill buttons (aria-pressed marks the current one). */
export function FilterChips({
  label,
  options,
  value,
  onChange,
  className,
}: {
  /** Accessible name of the group. */
  label: string
  options: readonly ChipOption[]
  value: string
  onChange: (value: string) => void
  className?: string
}) {
  return (
    <div role="group" aria-label={label} className={cn('flex flex-wrap gap-2', className)}>
      {options.map((o) => {
        const on = o.value === value
        return (
          <button
            key={o.value}
            type="button"
            aria-pressed={on}
            onClick={() => onChange(o.value)}
            className={cn(
              'inline-flex min-h-[34px] items-center rounded-full border px-3 text-[13px] font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring',
              on ? 'border-[#3b3570] bg-primary-soft text-[#ece9ff]' : 'border-input text-subtle-foreground hover:bg-muted hover:text-foreground',
            )}
          >
            {o.label}
          </button>
        )
      })}
    </div>
  )
}
