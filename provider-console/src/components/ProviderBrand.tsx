import { cn } from '@/lib/utils'

// The Provider Console identity: the violet Z mark, the product name and
// "Provider Console", so operators can always tell this control-plane app
// apart from a tenant's admin dashboard.
export function ProviderBrand({ className, size = 'md' }: { className?: string; size?: 'md' | 'lg' }) {
  const mark = size === 'lg' ? 'size-9' : 'size-7'
  return (
    <div className={cn('flex items-center gap-2.5', className)}>
      <svg viewBox="0 0 28 28" className={cn(mark, 'shrink-0')} aria-hidden>
        <rect width="28" height="28" rx="8" fill="var(--color-primary)" />
        <path d="M8 9h12l-12 10h12" stroke="var(--color-primary-foreground)" strokeWidth="2.4" fill="none" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
      <div className="flex flex-col leading-tight">
        <span className={cn('font-display font-semibold tracking-tight', size === 'lg' ? 'text-[17px]' : 'text-[15px]')}>Zecurity</span>
        <span className={cn('text-muted-foreground', size === 'lg' ? 'text-[13px]' : 'text-xs')}>Provider Console</span>
      </div>
    </div>
  )
}
