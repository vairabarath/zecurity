import { type ReactNode, useEffect, useState } from 'react'
import { Check, ChevronRight, Globe, Home, Lock, Mail, Shield, Building2, Fingerprint } from 'lucide-react'
import { cn } from '@/lib/utils'

export function AuthShell({
  children,
  version = 'v2.4.1',
}: {
  children: ReactNode
  version?: string
}) {
  const [time, setTime] = useState(() => new Date())

  useEffect(() => {
    const timer = window.setInterval(() => setTime(new Date()), 1000)
    return () => window.clearInterval(timer)
  }, [])

  return (
    <div className="auth-shell">
      <div className="auth-topbar">
        <div className="flex items-center gap-2 text-primary">
          <span className="h-2 w-2 rounded-full bg-primary shadow-[0_0_0_4px_oklch(0.86_0.095_175/0.16),0_0_12px_oklch(0.86_0.095_175/0.55)]" />
          <span>System Secure</span>
        </div>
        <div className="flex items-center gap-4">
          <span>{version}</span>
          <span className="font-mono tabular-nums">
            {time.toLocaleTimeString('en-GB', { hour12: false })}
          </span>
        </div>
      </div>
      <div className="auth-stage">{children}</div>
    </div>
  )
}

export function AuthCard({ children }: { children: ReactNode }) {
  return <div className="auth-card">{children}</div>
}

export function BrandBlock({ subtitle }: { subtitle: string }) {
  return (
    <div className="mb-7 flex flex-col items-center gap-3 text-center">
      <div className="relative grid h-14 w-14 place-items-center rounded-2xl bg-[linear-gradient(135deg,oklch(0.86_0.095_175)_0%,oklch(0.70_0.10_175)_100%)] text-[oklch(0.22_0.02_200)] shadow-[0_8px_24px_oklch(0.86_0.095_175/0.35)]">
        <Shield className="h-6 w-6" />
        <span className="absolute -right-1 -top-1 grid h-4 w-4 place-items-center rounded-full border-2 border-card bg-primary text-[oklch(0.22_0.02_200)]">
          <Check className="h-2.5 w-2.5" />
        </span>
      </div>
      <div>
        <h1 className="text-[26px] font-extrabold tracking-[0.08em]">ZECURITY</h1>
        <p className="mt-1 text-xs font-medium tracking-[0.04em] text-muted-foreground">{subtitle}</p>
      </div>
    </div>
  )
}

export function ModeTabs({
  mode,
  onChange,
}: {
  mode: 'endpoint' | 'email'
  onChange: (mode: 'endpoint' | 'email') => void
}) {
  return (
    <div className="mb-6 grid grid-cols-2 rounded-xl bg-secondary p-1">
      <button
        type="button"
        onClick={() => onChange('endpoint')}
        className={cn(
          'flex items-center justify-center gap-2 rounded-[9px] px-3 py-2.5 text-[13px] font-semibold transition-colors',
          mode === 'endpoint' ? 'bg-card text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground',
        )}
      >
        <Globe className="h-4 w-4" />
        Endpoint
      </button>
      <button
        type="button"
        onClick={() => onChange('email')}
        className={cn(
          'flex items-center justify-center gap-2 rounded-[9px] px-3 py-2.5 text-[13px] font-semibold transition-colors',
          mode === 'email' ? 'bg-card text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground',
        )}
      >
        <Fingerprint className="h-4 w-4" />
        Identity
      </button>
    </div>
  )
}

export function Field({
  label,
  suffix,
  hint,
  error,
  children,
}: {
  label: string
  suffix?: string
  hint?: string
  error?: string | null
  children: ReactNode
}) {
  return (
    <div className="mb-4">
      <label className="mb-2 block text-[12px] font-semibold text-foreground">{label}</label>
      <div className="flex items-center overflow-hidden rounded-xl border border-border bg-secondary px-3 shadow-[inset_0_1px_0_oklch(1_0_0/0.02)] focus-within:border-primary">
        <div className="min-w-0 flex-1">{children}</div>
        {suffix ? <span className="pl-3 text-sm font-semibold text-muted-foreground">{suffix}</span> : null}
      </div>
      {error ? (
        <p className="mt-2 text-xs font-medium text-destructive">{error}</p>
      ) : hint ? (
        <p className="mt-2 text-xs text-muted-foreground">{hint}</p>
      ) : null}
    </div>
  )
}

export function AuthInput(props: React.InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      {...props}
      className={cn(
        'h-11 w-full border-0 bg-transparent px-0 text-[14px] text-foreground outline-none placeholder:text-muted-foreground',
        props.className,
      )}
    />
  )
}

export function PrimaryButton({
  children,
  disabled,
  onClick,
  type = 'button',
}: {
  children: ReactNode
  disabled?: boolean
  onClick?: () => void
  type?: 'button' | 'submit'
}) {
  return (
    <button
      type={type}
      disabled={disabled}
      onClick={onClick}
      className="mt-2 flex h-11 w-full items-center justify-center gap-2 rounded-xl bg-primary px-4 text-[14px] font-bold text-primary-foreground shadow-[0_12px_24px_oklch(0.86_0.095_175/0.22)] transition hover:brightness-95 disabled:cursor-not-allowed disabled:opacity-50 disabled:shadow-none"
    >
      {children}
    </button>
  )
}

export function LinkHint({ children }: { children: ReactNode }) {
  return <div className="mt-4 text-center text-[12.5px] text-muted-foreground">{children}</div>
}

export function WizardHeader({
  step,
  title,
  description,
}: {
  step: string
  title: string
  description: string
}) {
  return (
    <div className="mb-6">
      <div className="text-[11px] font-bold uppercase tracking-[0.08em] text-muted-foreground">{step}</div>
      <h2 className="mt-1.5 text-[22px] font-bold tracking-[-0.01em]">{title}</h2>
      <p className="mt-1 text-[13px] text-muted-foreground">{description}</p>
    </div>
  )
}

export function AccountTypeCard({
  type,
  active,
  onClick,
}: {
  type: 'home' | 'office'
  active: boolean
  onClick: () => void
}) {
  const copy = type === 'home'
    ? { title: 'Home', desc: 'Personal devices and home lab', icon: Home }
    : { title: 'Office', desc: 'Team and company resources', icon: Building2 }
  const Icon = copy.icon

  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        'rounded-2xl border p-4 text-left transition-colors',
        active
          ? 'border-primary bg-primary/10 shadow-[0_8px_20px_oklch(0.86_0.095_175/0.12)]'
          : 'border-border bg-secondary hover:border-primary/50',
      )}
    >
      <div className={cn(
        'mb-3 grid h-10 w-10 place-items-center rounded-xl',
        active ? 'bg-primary text-[oklch(0.22_0.02_200)]' : 'bg-accent text-muted-foreground',
      )}>
        <Icon className="h-5 w-5" />
      </div>
      <div className="text-[14px] font-semibold">{copy.title}</div>
      <div className="mt-1 text-[12px] text-muted-foreground">{copy.desc}</div>
    </button>
  )
}

export function PreviewPanel({ label, value, suffix }: { label: string; value: string; suffix?: string }) {
  return (
    <div className="rounded-2xl border border-border bg-secondary px-4 py-3">
      <div className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">{label}</div>
      <div className="mt-1 text-[15px] font-semibold">
        <span className="text-primary">{value}</span>
        {suffix ? <span className="text-muted-foreground">{suffix}</span> : null}
      </div>
    </div>
  )
}

export function FooterLink({
  text,
  cta,
  onClick,
}: {
  text: string
  cta: string
  onClick: () => void
}) {
  return (
    <LinkHint>
      {text}{' '}
      <button type="button" onClick={onClick} className="font-semibold text-primary">
        {cta}
      </button>
    </LinkHint>
  )
}

export function ContinueLabel() {
  return <ChevronRight className="h-4 w-4" />
}

export function LockLabel() {
  return <Lock className="h-4 w-4" />
}

export function MailLabel() {
  return <Mail className="h-4 w-4" />
}
