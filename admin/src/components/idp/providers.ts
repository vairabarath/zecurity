export const PROVIDER_OPTIONS = [
  { value: 'okta', label: 'Okta' },
  { value: 'entra', label: 'Microsoft Entra ID' },
  { value: 'jumpcloud', label: 'JumpCloud' },
  { value: 'keycloak', label: 'Keycloak' },
  { value: 'oidc', label: 'Generic OIDC' },
] as const

export type ProviderKey = (typeof PROVIDER_OPTIONS)[number]['value']

export function providerLabel(key: ProviderKey): string {
  return PROVIDER_OPTIONS.find((opt) => opt.value === key)?.label ?? key
}
