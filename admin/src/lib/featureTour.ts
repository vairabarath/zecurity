import { Box, History, KeyRound, Laptop, Network, ShieldCheck, Users, type LucideIcon } from 'lucide-react'

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
  /** "Show me" buttons: each runs a walk-through from guidedTours.featureTours. */
  guided?: { label: string; tour: string }[]
}

export const tourFeatures: TourFeature[] = [
  {
    id: 'networks',
    title: 'Remote networks and connectors',
    icon: Network,
    summary:
      'Connect a location (an office LAN, a cloud VPC) to Zecurity. A connector inside it carries users’ traffic to the resources there.',
    guided: [{ label: 'Show me: add a network and a connector', tour: 'network-connector' }],
    sections: [
      {
        title: 'Create a remote network',
        steps: ['Open Remote Networks and click Add Network.', 'Give it a name, for example Office LAN, and click Create.'],
        link: { label: 'Open Remote Networks', to: '/remote-networks' },
      },
      {
        title: 'Install a connector',
        steps: [
          'Open Connectors and click Add Connector.',
          'Name it, choose the network and platform, and add it.',
          'Run the install command on a machine inside that network. It shows online within a minute.',
        ],
        link: { label: 'Open Connectors', to: '/connectors' },
      },
    ],
    notes: ['Keep at least one connector online per network; users reach resources only through a connector.'],
  },
  {
    id: 'resources',
    title: 'Resources and shields',
    icon: Box,
    summary:
      'A resource is a service on a remote network (host and port). A shield on its machine firewalls it so only traffic through Zecurity gets in.',
    guided: [{ label: 'Show me: add a resource', tour: 'create-resource' }],
    sections: [
      {
        title: 'Add a resource',
        steps: [
          'Open Resources and click Add Resource.',
          'Choose its remote network, give it a name, and enter the host (IPv4 address or DNS name), protocol and port.',
        ],
        link: { label: 'Open Resources', to: '/resources' },
      },
      {
        title: 'Protect it with a shield',
        steps: [
          'Install a shield on the machine that runs the service (Shields → Add Shield).',
          'Open the resource and click Protect this resource. The shield then blocks traffic that does not come through Zecurity.',
        ],
        link: { label: 'Open Shields', to: '/shields' },
      },
      {
        title: 'Find services automatically',
        steps: ['Resource Discovery runs connector-side scans and lists services that shields have seen; promote one to a resource.'],
        link: { label: 'Open Resource Discovery', to: '/resource-discovery' },
      },
    ],
  },
  {
    id: 'users-groups',
    title: 'Users, groups and access',
    icon: Users,
    summary:
      'Access is default-deny. A user reaches a resource only through a group that has it: user → group → resource.',
    guided: [
      { label: 'Show me: add a user', tour: 'add-user' },
      { label: 'Show me: create a group', tour: 'create-group' },
      { label: 'Show me: give a group access', tour: 'group-access' },
    ],
    sections: [
      {
        title: 'Add a user',
        steps: ['Open Users and click Add User.', 'Enter their email and send the invite. They sign in with your identity provider.'],
        link: { label: 'Open Users', to: '/users' },
      },
      {
        title: 'Create a group',
        steps: ['Open Groups and click New Group.', 'Name it (for example Engineering) and save.'],
        link: { label: 'Open Groups', to: '/groups' },
      },
      {
        title: 'Add members and assign resources',
        steps: [
          'Click Manage on the group.',
          'Add Member to put users in it; Assign Resource to give the group a resource.',
          'Members can now reach those resources, if their device meets each resource’s policy.',
        ],
        link: { label: 'Open Groups', to: '/groups' },
      },
    ],
  },
  {
    id: 'posture',
    title: 'Policies and device posture',
    icon: ShieldCheck,
    summary:
      'Decide which devices may reach which resources. Device profiles say what a device must meet; resource policies pick the profiles their resources accept.',
    guided: [
      { label: 'Show me: create a device profile', tour: 'create-device-profile' },
      { label: 'Show me: create a resource policy', tour: 'create-policy' },
    ],
    sections: [
      {
        title: 'Create a device profile',
        steps: [
          'Open Policies → Device Profiles and click Create Device Profile.',
          'Name it, optionally require admin approval or allowed countries.',
          'Turn on each platform it allows (Windows, Linux, iOS, Android) and mark the checks its devices must pass.',
        ],
        link: { label: 'Open Policies', to: '/policies' },
      },
      {
        title: 'Create a resource policy',
        steps: [
          'Open Policies and click Create Resource Policy.',
          'Tick the device profiles it accepts (any one is enough), and set the sign-in frequency and MFA if needed.',
          'The Enforcement Preview shows how many devices would pass today.',
        ],
        link: { label: 'Open Policies', to: '/policies' },
      },
      {
        title: 'Apply it to resources',
        steps: ['Open a resource and choose its Resource policy. Resources without one use the Default policy.'],
        link: { label: 'Open Resources', to: '/resources' },
      },
    ],
  },
  {
    id: 'mfa',
    guided: [{ label: 'Take the guided tour', tour: 'mfa' }],
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
  {
    id: 'devices',
    title: 'Devices',
    icon: Laptop,
    summary: 'Every device that enrolled the Zecurity client, with its platform, posture, last sign-in and approval.',
    sections: [
      {
        title: 'Approve or revoke devices',
        steps: [
          'When a device profile requires approval, approve devices on the Devices page.',
          'Revoke a lost or stolen device: it loses access at once and must enrol again.',
        ],
        link: { label: 'Open Devices', to: '/devices' },
      },
    ],
  },
  {
    id: 'logs',
    title: 'Access and audit logs',
    icon: History,
    summary: 'See who reached what, and every administrative change.',
    sections: [
      {
        title: 'Access log',
        steps: ['Every tunnel attempt across your connectors, allowed or denied. Refreshes every 10 seconds.'],
        link: { label: 'Open Access Log', to: '/access-log' },
      },
      {
        title: 'Audit log',
        steps: [
          'Every admin change and sign-in, hash-chained. Filter by an action prefix such as “auth.”.',
          'Send it to a SIEM from Settings → Audit export.',
        ],
        link: { label: 'Open Audit Log', to: '/audit-log' },
      },
    ],
  },
]
