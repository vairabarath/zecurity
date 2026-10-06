import { ShieldCheck } from 'lucide-react'
import { cn } from '@/lib/utils'

// The Provider Console identity: product name plus a "PROVIDER CONSOLE" badge
// in the violet accent, so operators can always tell this control-plane app
// apart from a tenant's admin dashboard.
export function ProviderBrand({ className }: { className?: string }) {
  return (
    <div className={cn('flex items-center gap-2.5', className)}>
      <ShieldCheck className="size-6 text-primary" aria-hidden />
      <span className="font-display text-lg font-semibold tracking-tight">Zecurity</span>
      <span className="rounded-md border border-primary/40 bg-primary/10 px-2 py-0.5 text-[11px] font-semibold uppercase tracking-[0.12em] text-primary">
        Provider Console
      </span>
    </div>
  )
}
