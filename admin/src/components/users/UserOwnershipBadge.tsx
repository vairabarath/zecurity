import { StatusPill } from '@/lib/console'

// Directory-owned users (provisioningOwner === 'scim') are managed by the IdP,
// so their directory-owned attributes must not be edited in the admin UI.
// Derive "managed by the directory" from provisioningOwner only — never from
// `provider` (ADR-025 §5 / schema.graphqls comment). `unmanaged` is an explicit
// value and must NOT be treated as directory-owned.
const PROVIDER_LABEL: Record<string, string> = {
  okta: 'Okta',
  entra: 'Microsoft Entra',
  'microsoft-entra': 'Microsoft Entra',
  google: 'Google Workspace',
  'google-workspace': 'Google Workspace',
  jumpcloud: 'JumpCloud',
  keycloak: 'Keycloak',
  generic: 'the directory',
}

function providerLabel(provider: string): string {
  const known = PROVIDER_LABEL[provider.toLowerCase()]
  if (known) return known
  return provider.charAt(0).toUpperCase() + provider.slice(1)
}

export type UserOwnershipBadgeProps = {
  provisioningOwner: string
  provider: string
}

export function UserOwnershipBadge({
  provisioningOwner,
  provider,
}: UserOwnershipBadgeProps) {
  if (provisioningOwner !== 'scim') return null

  return (
    <StatusPill
      label={`Managed by ${providerLabel(provider)}`}
      tone="info"
    />
  )
}
