import { Plus } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { PROVIDER_OPTIONS, type ProviderKey } from '@/components/idp/providers'

/**
 * The first step of the Twingate-style "Add Identity Provider" flow: a small
 * popup listing the supported providers (Okta, Entra ID, JumpCloud, Keycloak,
 * Generic OIDC). Choosing one is the ONLY thing this popup does — it does not
 * collect any connection details itself. The caller opens
 * CreateIdpConnectionDialog with `initialProvider` set to the chosen value as
 * the second step, mirroring "click Add Identity Provider → pick Okta from
 * the menu → the Connect Okta dialog with input fields opens".
 */
export function AddIdentityProviderMenu({
  onSelect,
  label = 'Add Identity Provider',
  variant = 'default',
}: {
  onSelect: (provider: ProviderKey) => void
  label?: string
  variant?: 'default' | 'outline'
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant={variant}>
          <Plus className="mr-1.5 h-4 w-4" />
          {label}
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="w-56">
        {PROVIDER_OPTIONS.map((opt) => (
          <DropdownMenuItem key={opt.value} onClick={() => onSelect(opt.value)}>
            {opt.label}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
