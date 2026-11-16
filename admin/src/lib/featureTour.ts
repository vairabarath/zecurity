import { KeyRound, type LucideIcon } from 'lucide-react'

// Feature Tour content. One entry per console feature; the Feature Tour page
// renders each as a card. Add a feature by appending an entry here.

export interface TourLink {
  label: string
  to: string
}

export interface TourSection {
  title: string
  steps: string[]
  link?: TourLink
}

export interface TourFeature {
  id: string
  title: string
  icon: LucideIcon
  summary: string
  sections: TourSection[]
  notes?: string[]
  /** Key in guidedTours.featureTours for the "Take the guided tour" button. */
  guided?: string
}

export const tourFeatures: TourFeature[] = [
  {
    id: 'mfa',
    guided: 'mfa',
    title: 'Multi-factor authentication (MFA)',
    icon: KeyRound,
    summary:
      'Ask for a second factor, either a code from an authenticator app or the identity provider’s own MFA, before someone uses the admin console, reaches protected resources or takes a sensitive action.',
    sections: [
      {
        title: 'Set up your authenticator app',
        steps: [
          'Open Settings and find the Security card.',
          'Click Set up and scan the QR code with an authenticator app (Google Authenticator, Authy, 1Password, …), or type the setup key.',
          'Enter the 6-digit code the app shows. The app is now linked to your account.',
        ],
        link: { label: 'Open Settings', to: '/settings' },
      },
      {
        title: 'Require MFA for the admin console',
        steps: [
          'In Settings → Security, turn on “Require MFA for the admin console”. You are asked for a code first, so you cannot lock yourself out.',
          'From then on, everyone who signs in to the console enters a code once per session. People without an authenticator set one up at their next visit.',
          'Members who only use the client are asked too when they open the Install Client page.',
        ],
        link: { label: 'Open Settings', to: '/settings' },
      },
      {
        title: 'Require MFA for resources',
        steps: [
          'Open Policies and edit a resource policy.',
          'Under Authentication, choose how often users must sign in again (every hour up to every 31 days), then tick Require MFA.',
          'When a sign-in is older than that, the resources close on the device until the user runs `zecurity-client auth`. The browser opens and the user signs in and enters a code.',
          '“Skip for now” on the code page opens only the resources that do not require MFA.',
        ],
        link: { label: 'Open Policies', to: '/policies' },
      },
      {
        title: 'MFA for sensitive actions',
        steps: [
          'Break-glass actions on an identity provider’s SCIM mapping need a code entered in the last 15 minutes, even if the console session already passed MFA.',
          'The console asks for the code in place and then finishes the action.',
        ],
        link: { label: 'Open Identity Providers', to: '/idp-connections' },
      },
      {
        title: 'Reset a user’s MFA',
        steps: [
          'When someone loses their phone, open Users and click Reset MFA on their row. You cannot reset your own.',
          'Their authenticator is removed, their devices lose the MFA mark at once, and they set up a new authenticator at their next prompt.',
        ],
        link: { label: 'Open Users', to: '/users' },
      },
      {
        title: 'See who passed MFA',
        steps: [
          'Devices: the Sign-in column shows when each device last authenticated, with an MFA badge when a second factor was used.',
          'Users: the MFA column shows who has an authenticator set up.',
          'Audit Log: filter by “mfa.” or “auth.” to see set-ups, verifications, step-ups and resets.',
        ],
        link: { label: 'Open Devices', to: '/devices' },
      },
    ],
    notes: [
      'The identity provider’s own MFA counts for a step-up only when the provider signed the user in during that step-up. Otherwise the user enters a code.',
      'A step-up only counts for the device that started it: the browser hands a one-time code back to that client.',
      'Too many wrong codes lock code entry for a few minutes and raise the device’s risk score.',
    ],
  },
]
