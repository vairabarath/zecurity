// Guided tours: a spotlight on one element at a time with Back / Next / Skip.
//
// - Page tours run once, the first time someone opens that page in this
//   browser (remembered in localStorage).
// - Feature tours (started from the Feature Tour page) walk through a task,
//   possibly across pages. A step with `click` presses its target when the
//   user clicks Next, so the tour opens the dialog the next steps explain.
//   The last step leaves the dialog open for the user to finish.
//
// How a step finds its element: see lib/tourTargets.ts. A step whose
// element is not on screen is shown without a spotlight.

export interface GuidedStep {
  title: string
  body: string
  /** data-tour="…" attribute */
  target?: string
  /** CSS selector */
  selector?: string
  /** visible text of a button or link (prefix) */
  button?: string
  /** a form field: label text (prefix), placeholder or aria-label */
  field?: string
  /** the page this step is on; the tour goes there first */
  route?: string
  /** Next clicks the target before moving on (opens a dialog or page) */
  click?: boolean
  /** The target submits what the user filled in: Next clicks it only when
   *  it is enabled; otherwise the tour stays and asks for the fields. */
  submit?: boolean
}

const LIST = '.table-shell'

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
      body: 'A resource policy says which devices may reach its resources (device profiles) and how recent the sign-in must be. Every resource uses one policy; the Default policy covers the rest.',
    },
    {
      button: 'Device Profiles',
      title: 'Device profiles',
      body: 'Device profiles list what a device must meet (platform, disk encryption, firewall, approval, country, …). Policies pick the profiles they accept.',
    },
  ],
  '/users': [
    {
      button: 'Add User',
      title: 'Add people',
      body: 'Invite someone by email. They sign in with your identity provider and get the role you choose.',
    },
    {
      target: 'users-mfa-column',
      title: 'MFA status',
      body: 'Shows who has an authenticator set up. When someone loses their phone, use Reset MFA on their row.',
    },
  ],
  '/groups': [
    {
      button: 'New Group',
      title: 'Groups grant access',
      body: 'Access is default-deny: a user reaches a resource only through a group that has it. Create a group, then add members and resources to it.',
    },
    {
      selector: LIST,
      title: 'Your groups',
      body: 'Click Manage on a group to add members and assign resources.',
    },
  ],
  '/resources': [
    {
      button: 'Add Resource',
      title: 'Resources',
      body: 'A resource is a service on your network (host and port) that users reach through a connector. Add one, then protect it with a shield and give groups access.',
    },
    {
      selector: LIST,
      title: 'Your resources',
      body: 'Open a resource to see how it is secured: its shield, its policy and the groups that have access.',
    },
  ],
  '/remote-networks': [
    {
      button: 'Add Network',
      title: 'Remote networks',
      body: 'A remote network is a location you connect to Zecurity, such as an office LAN or a cloud VPC. Connectors and resources belong to one.',
    },
  ],
  '/connectors': [
    {
      button: 'Add Connector',
      title: 'Connectors',
      body: 'A connector runs inside a remote network and carries users’ traffic to its resources. Add one and run the install command it gives you on a host in that network.',
    },
    {
      selector: LIST,
      title: 'Connector status',
      body: 'Each connector’s health and last heartbeat. Keep at least one online per network.',
    },
  ],
  '/shields': [
    {
      button: 'Add Shield',
      title: 'Shields',
      body: 'A shield runs on the machine that hosts a resource and firewalls it, so only traffic through Zecurity gets in.',
    },
  ],
  '/devices': [
    {
      target: 'devices-signin-column',
      title: 'Last sign-in',
      body: 'When each device last authenticated. An MFA badge means that sign-in included a second factor.',
    },
    {
      selector: LIST,
      title: 'Enrolled devices',
      body: 'Every device that enrolled the client: its platform, certificate, status and approval. Approve devices here when a profile requires approval, or revoke a lost one.',
    },
  ],
  '/audit-log': [
    {
      target: 'audit-filter',
      title: 'Filter the audit log',
      body: 'Type a prefix such as “mfa.” or “auth.” to see authenticator set-ups, verifications, step-ups and resets.',
    },
  ],
  '/access-log': [
    {
      selector: LIST,
      title: 'Access log',
      body: 'Every tunnel attempt across your connectors, allowed or denied, refreshed every 10 seconds.',
    },
  ],
}

export const featureTours: Record<string, GuidedStep[]> = {
  mfa: [
    ...pageTours['/settings'].map((s) => ({ ...s, route: '/settings' })),
    { ...pageTours['/policies'][0], route: '/policies' },
    { ...pageTours['/users'][1], route: '/users' },
    { ...pageTours['/devices'][0], route: '/devices' },
    { ...pageTours['/audit-log'][0], route: '/audit-log' },
  ],

  'add-user': [
    {
      route: '/users',
      button: 'Add User',
      click: true,
      title: 'Add a user',
      body: 'Click Add User. (Press Next and the tour clicks it for you.)',
    },
    {
      field: 'Email address',
      title: 'Their email address',
      body: 'Enter the email they sign in with at your identity provider.',
    },
    {
      button: 'Send invite',
      title: 'Send the invite',
      body: 'They get an invitation link. Once they accept, they appear in Users. Put them in a group to give them access.',
    },
  ],

  'create-group': [
    {
      route: '/groups',
      button: 'New Group',
      click: true,
      title: 'Create a group',
      body: 'Click New Group.',
    },
    { field: 'Name', title: 'Name', body: 'A short name, for example Engineering.' },
    { field: 'Description', title: 'Description', body: 'Optional: what the group is for.' },
    {
      button: 'Save',
      title: 'Save',
      body: 'Save the group, then open it with Manage to add members and resources.',
    },
  ],

  'group-access': [
    {
      route: '/groups',
      button: 'Manage',
      click: true,
      title: 'Open a group',
      body: 'Click Manage on a group to see its members and resources.',
    },
    {
      button: 'Add Member',
      click: true,
      title: 'Add members',
      body: 'Click Add Member.',
    },
    { field: 'User', title: 'Pick a user', body: 'Choose who joins the group. Members get every resource the group has.' },
    {
      button: 'Cancel',
      click: true,
      title: 'Add, or cancel for now',
      body: 'Click Add to add them. The tour cancels this one so it can show you resources next.',
    },
    {
      target: 'group-tab-resources',
      click: true,
      title: 'The group’s resources',
      body: 'Open the Resources tab. A group with no resources gives its members no access (default-deny).',
    },
    {
      button: 'Assign Resource',
      click: true,
      title: 'Assign resources',
      body: 'Click Assign Resource.',
    },
    { field: 'Resource', title: 'Pick a resource', body: 'Choose a resource. Every member of the group can reach it (if their device meets its policy).' },
    { button: 'Assign', title: 'Assign', body: 'Click Assign. That is all access needs: user → group → resource.' },
  ],

  'create-resource': [
    {
      route: '/resources',
      button: 'Add Resource',
      click: true,
      title: 'Add a resource',
      body: 'Click Add Resource.',
    },
    { field: 'Remote Network', title: 'Remote network', body: 'The network the service is in. Its connector carries the traffic.' },
    { field: 'Name', title: 'Name', body: 'A name users will see, for example prod-web-01.' },
    { field: 'Host', title: 'Host', body: 'The service’s IPv4 address or DNS name inside that network.' },
    { field: 'Protocol', title: 'Protocol', body: 'TCP or UDP.' },
    { field: 'Port From', title: 'Port', body: 'The port, or the first port of a range (fill Port To for a range).' },
    {
      button: 'Add Resource',
      title: 'Add it',
      body: 'Add the resource. Then open it to protect it with a shield and choose its policy, and assign it to a group.',
    },
  ],

  'create-policy': [
    {
      route: '/policies',
      button: 'Create Resource Policy',
      click: true,
      title: 'Create a resource policy',
      body: 'Click Create Resource Policy.',
    },
    { field: 'Policy Name', title: 'Name', body: 'For example Production Services.' },
    {
      field: 'Device Profiles',
      title: 'Device profiles',
      body: 'Tick the profiles a device may match. A device that meets any one of them is allowed. With none ticked, any device is allowed.',
    },
    {
      field: 'Authentication',
      title: 'Authentication',
      body: 'How often users must sign in again for these resources, and whether that sign-in needs MFA.',
    },
    {
      button: 'Create Policy',
      title: 'Create it',
      body: 'Create the policy, then choose it on each resource it should cover (open the resource, Resource policy).',
    },
  ],

  'create-device-profile': [
    {
      route: '/policies',
      button: 'Device Profiles',
      click: true,
      title: 'Device profiles',
      body: 'Open the Device Profiles tab.',
    },
    {
      button: 'Create Device Profile',
      click: true,
      title: 'Create a device profile',
      body: 'Click Create Device Profile.',
    },
    { field: 'Profile Name', title: 'Name', body: 'For example Company laptops. It cannot be changed later.' },
    {
      field: 'Verification Requirements',
      title: 'Verification',
      body: 'Optional: require an admin to approve each device, or allow only some countries.',
    },
    {
      field: 'Approved Operating Systems',
      title: 'Platforms',
      body: 'Turn on each platform the profile allows, then mark the checks its devices must pass (disk encryption, firewall, minimum OS version, …).',
    },
    {
      button: 'Create Profile',
      title: 'Create it',
      body: 'Create the profile, then tick it in a resource policy so the policy requires it.',
    },
  ],

  'add-shield': [
    {
      route: '/shields',
      button: 'Add Shield',
      click: true,
      title: 'Add a shield',
      body: 'A shield runs on the machine that hosts your resources and firewalls them, so only traffic through Zecurity gets in. It needs a remote network with a connector online. Click Add Shield (or press Next).',
    },
    {
      field: 'Shield Name',
      title: 'Shield name',
      body: 'Type a name for the machine it will run on, for example db-server-01.',
    },
    {
      field: 'Remote Network',
      title: 'Its network',
      body: 'Choose the remote network the machine is in. It must have a connector online: the shield binds to it at install time.',
    },
    {
      field: 'Platform',
      title: 'Platform',
      body: 'Linux is the platform for shields.',
    },
    {
      button: 'Add Shield',
      submit: true,
      title: 'Add the shield',
      body: 'Click Add Shield (or press Next). You go to the shield’s page. If the network has no connector online yet, the form says so: install the connector first.',
    },
    {
      target: 'shield-install',
      title: 'Install it on the resource’s machine',
      body: 'Copy this command and run it on the machine that runs your services (not on the connector). The shield shows Active once it checks in.',
    },
    {
      route: '/resources',
      selector: '.table-shell',
      title: 'Then protect resources',
      body: 'Open a resource on that machine and click Protect this resource. The shield then blocks every connection that does not come through Zecurity.',
    },
  ],

  'network-connector': [
    {
      route: '/remote-networks',
      button: 'Add Network',
      click: true,
      title: 'Add a remote network',
      body: 'A remote network is a place you connect: an office LAN, a cloud VPC. Click Add Network (or press Next).',
    },
    {
      target: 'network-name',
      title: 'Name the network',
      body: 'Type a name in the box, for example Office LAN or Production VPC.',
    },
    {
      target: 'network-location',
      title: 'Where it is',
      body: 'Optional: pick where this network lives (on-premise, a cloud, …). Click the box to choose.',
    },
    {
      target: 'network-create',
      submit: true,
      title: 'Create it',
      body: 'Click Create (or press Next). The network appears in the list.',
    },
    {
      route: '/connectors',
      button: 'Add Connector',
      click: true,
      title: 'Add a connector',
      body: 'A connector runs on a machine inside the network and carries users’ traffic to it. Click Add Connector (or press Next).',
    },
    {
      field: 'Connector Name',
      title: 'Connector name',
      body: 'Type a name for it, for example office-connector-01.',
    },
    {
      field: 'Remote Network',
      title: 'Its network',
      body: 'Choose the network you just created.',
    },
    {
      field: 'Platform',
      title: 'Platform',
      body: 'Linux is the platform for connectors.',
    },
    {
      button: 'Add Connector',
      submit: true,
      title: 'Add the connector',
      body: 'Click Add Connector (or press Next). You go to the connector’s page.',
    },
    {
      target: 'connector-install',
      title: 'Install it',
      body: 'Copy this command and run it on a Linux machine inside the network. The connector shows Online within a minute; then add resources in that network.',
    },
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
