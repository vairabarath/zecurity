import type { ReactNode } from 'react'

/** Title, one-line description and right-aligned actions for a page. */
export function PageHeader({ title, description, actions }: { title: ReactNode; description?: ReactNode; actions?: ReactNode }) {
  return (
    <header className="flex flex-wrap items-end justify-between gap-4">
      <div className="flex min-w-0 flex-col gap-1.5">
        <h1 className="font-display text-[28px] font-semibold leading-tight tracking-[-0.02em]">{title}</h1>
        {description && <p className="text-sm text-subtle-foreground">{description}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-3">{actions}</div>}
    </header>
  )
}
