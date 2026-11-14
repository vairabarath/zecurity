// Simple operating-system glyphs for the device profile cards (ADR-031).
// Drawn in currentColor so they follow the theme; not official logos.

interface IconProps {
  className?: string;
}

export function WindowsIcon({ className }: IconProps) {
  return (
    <svg viewBox="0 0 24 24" className={className} fill="currentColor" aria-hidden="true">
      <path d="M3 5.1 10.4 4v7.2H3zM11.4 3.9 21 2.5v8.7h-9.6zM3 12.2h7.4v7.2L3 18.3zM11.4 12.2H21v8.7l-9.6-1.4z" />
    </svg>
  );
}

export function AppleIcon({ className }: IconProps) {
  return (
    <svg viewBox="0 0 24 24" className={className} fill="currentColor" aria-hidden="true">
      <path d="M16.4 12.6c0-2.3 1.9-3.4 2-3.5-1.1-1.6-2.8-1.8-3.4-1.8-1.4-.2-2.8.8-3.5.8-.7 0-1.8-.8-3-.8-1.5 0-3 .9-3.8 2.3-1.6 2.8-.4 7 1.2 9.3.8 1.1 1.7 2.4 2.9 2.3 1.2 0 1.6-.7 3-.7s1.8.7 3 .7c1.3 0 2-1.1 2.8-2.3.9-1.3 1.2-2.5 1.3-2.6-.1 0-2.5-1-2.5-3.7z" />
      <path d="M14.2 5.8c.6-.8 1.1-1.8 1-2.8-.9 0-2 .6-2.7 1.4-.6.7-1.1 1.7-1 2.7 1 .1 2-.5 2.7-1.3z" />
    </svg>
  );
}

export function LinuxIcon({ className }: IconProps) {
  return (
    <svg viewBox="0 0 24 24" className={className} aria-hidden="true">
      {/* A simple penguin: body, belly, eyes, beak and feet. */}
      <ellipse cx="12" cy="13" rx="6.2" ry="8.5" fill="currentColor" />
      <ellipse cx="12" cy="15.2" rx="3.9" ry="5.6" fill="var(--color-card, #fff)" />
      <circle cx="10.2" cy="8.4" r="1.1" fill="var(--color-card, #fff)" />
      <circle cx="13.8" cy="8.4" r="1.1" fill="var(--color-card, #fff)" />
      <path d="M10.6 10.3h2.8L12 12z" fill="#e8a33d" />
      <path d="M6.5 20.5c1-1.4 3-1.6 4 0zM13.5 20.5c1-1.6 3-1.4 4 0z" fill="#e8a33d" />
    </svg>
  );
}

export function AndroidIcon({ className }: IconProps) {
  return (
    <svg viewBox="0 0 24 24" className={className} fill="currentColor" aria-hidden="true">
      <path d="M5 16.5a7 7 0 0 1 14 0z" />
      <path d="m7.4 9.6-1.6-2.8.8-.4 1.6 2.8zM16.6 9.6l1.6-2.8-.8-.4-1.6 2.8z" />
      <circle cx="9.3" cy="13.4" r=".9" fill="var(--color-card, #fff)" />
      <circle cx="14.7" cy="13.4" r=".9" fill="var(--color-card, #fff)" />
    </svg>
  );
}

export function IOSIcon({ className }: IconProps) {
  return (
    <svg viewBox="0 0 24 24" className={className} aria-hidden="true">
      <rect x="2" y="4" width="20" height="16" rx="4" fill="currentColor" />
      <text
        x="12"
        y="15.2"
        textAnchor="middle"
        fontSize="8"
        fontWeight="700"
        fontFamily="system-ui, sans-serif"
        fill="var(--color-card, #fff)"
      >
        iOS
      </text>
    </svg>
  );
}
