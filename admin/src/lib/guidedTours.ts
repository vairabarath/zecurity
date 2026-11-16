// Guided tours: a spotlight on one element at a time with Back / Next / Skip.
//
// - Page tours run once, the first time someone opens that page in this
//   browser (remembered in localStorage).
// - Feature tours (started from the Feature Tour page) can span pages: each
//   step names the route it lives on and the tour navigates there.
//
// Targets are `data-tour="<id>"` attributes on the page's elements. A step
// whose target is not on screen is shown without a spotlight.

export interface GuidedStep {
  target: string
  title: string
  body: string
  route?: string
}

export const pageTours: Record<string, GuidedStep[]> = {
  '/settings': [
    {
      target: 'security-card',
      title: 'Security',
      body: 'Your second factor and the console’s MFA rule live here.',
    },
    {
      target: 'mfa-authenticator',
      title: 'Your authenticator app',
      body: 'Set up an authenticator app (scan the QR code, enter one code). You need it for resources that require MFA and for break-glass actions.',
    },
    {
      target: 'mfa-console',
      title: 'Require MFA for the admin console',
      body: 'Turn this on and everyone who signs in to the console must also enter a code. You are asked for a code first, so you cannot lock yourself out.',
    },
  ],
  '/policies': [
    {
      target: 'policies-list',
      title: 'Resource policies',
      body: 'Edit a policy to set its Authentication rule: how often users must sign in again, and whether that sign-in needs MFA.',
    },
  ],
  '/users': [
    {
      target: 'users-mfa-column',
      title: 'MFA status',
      body: 'Shows who has an authenticator set up. When someone loses their phone, use Reset MFA on their row.',
    },
  ],
  '/devices': [
    {
      target: 'devices-signin-column',
      title: 'Last sign-in',
      body: 'When each device last authenticated. An MFA badge means that sign-in included a second factor.',
    },
  ],
  '/audit-log': [
    {
      target: 'audit-filter',
      title: 'Filter the audit log',
      body: 'Type a prefix such as “mfa.” or “auth.” to see authenticator set-ups, verifications, step-ups and resets.',
    },
  ],
}

// The MFA walk-through from the Feature Tour page, across pages.
export const featureTours: Record<string, GuidedStep[]> = {
  mfa: [
    ...pageTours['/settings'].map((s) => ({ ...s, route: '/settings' })),
    { ...pageTours['/policies'][0], route: '/policies' },
    { ...pageTours['/users'][0], route: '/users' },
    { ...pageTours['/devices'][0], route: '/devices' },
    { ...pageTours['/audit-log'][0], route: '/audit-log' },
  ],
}

const SEEN_PREFIX = 'zecurity.tour.seen.v1:'

export function pageTourSeen(path: string): boolean {
  try {
    return localStorage.getItem(SEEN_PREFIX + path) === '1'
  } catch {
    // No storage: treat as seen so the tour never nags on every visit.
    return true
  }
}

export function markPageTourSeen(path: string): void {
  try {
    localStorage.setItem(SEEN_PREFIX + path, '1')
  } catch {
    // Ignore: storage is a convenience.
  }
}

export function resetPageTours(): void {
  try {
    for (const path of Object.keys(pageTours)) localStorage.removeItem(SEEN_PREFIX + path)
  } catch {
    // Ignore.
  }
}
