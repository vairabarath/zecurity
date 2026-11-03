import { cn } from '@/lib/utils'

const PALETTE = [
  'bg-[#2a2550] text-[#d6cfff]',
  'bg-[#132b33] text-[#8fe3f0]',
  'bg-[#2b2414] text-[#f5c56b]',
  'bg-[#14281f] text-[#7fe5b5]',
  'bg-[#2e1a22] text-[#f7a8c0]',
]

/** Up to two initials from the name's words (or its first two letters). */
function initials(name: string): string {
  const words = name.trim().split(/\s+/).filter(Boolean)
  if (words.length >= 2) return (words[0][0] + words[1][0]).toUpperCase()
  return (words[0] ?? '?').slice(0, 2).toUpperCase()
}

/** A decorative tile for a tenant; the colour is stable per name. */
export function TenantAvatar({ name, size = 'md' }: { name: string; size?: 'md' | 'lg' }) {
  let h = 0
  for (const ch of name) h = (h * 31 + ch.charCodeAt(0)) >>> 0
  return (
    <span
      aria-hidden
      className={cn(
        'inline-flex shrink-0 items-center justify-center font-semibold',
        size === 'lg' ? 'size-12 rounded-xl text-[17px]' : 'size-[30px] rounded-lg text-xs',
        PALETTE[h % PALETTE.length],
      )}
    >
      {initials(name)}
    </span>
  )
}
