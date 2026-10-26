import type { ReactNode } from 'react'
import { ProviderBrand } from '@/components/ProviderBrand'

/** The centred panel used by the change-password page. */
export function AuthCard({ title, description, children }: { title: string; description?: ReactNode; children: ReactNode }) {
  return (
    <main className="flex min-h-screen flex-col items-center justify-center gap-8 p-4">
      <ProviderBrand size="lg" />
      <div className="w-full max-w-[420px] rounded-2xl border border-border-strong bg-card p-6 sm:p-7">
        <div className="mb-5 flex flex-col gap-2">
          <h1 className="font-display text-[22px] font-semibold tracking-[-0.02em]">{title}</h1>
          {description && <div className="text-sm text-subtle-foreground">{description}</div>}
        </div>
        {children}
      </div>
    </main>
  )
}
