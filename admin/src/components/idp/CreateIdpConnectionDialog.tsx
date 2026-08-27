import { useState } from 'react'
import { useMutation } from '@apollo/client/react'
import { toast } from 'sonner'
import { ChevronDown, ChevronRight, KeyRound } from 'lucide-react'
import {
  CreateIdpConnectionDocument,
  GetIdpConnectionsDocument,
  type CreateIdpConnectionMutation,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'

const PROVIDER_OPTIONS = [
  { value: 'okta', label: 'Okta' },
  { value: 'entra', label: 'Microsoft Entra ID' },
  { value: 'jumpcloud', label: 'JumpCloud' },
  { value: 'keycloak', label: 'Keycloak' },
  { value: 'oidc', label: 'Generic OIDC' },
] as const

type ProviderKey = (typeof PROVIDER_OPTIONS)[number]['value']

export function CreateIdpConnectionDialog({
  open,
  onClose,
  onSuccess,
}: {
  open: boolean
  onClose: () => void
  onSuccess: (createdId: string) => void
}) {
  const [provider, setProvider] = useState<ProviderKey>('okta')
  const [displayName, setDisplayName] = useState('')
  const [issuer, setIssuer] = useState('')
  const [clientId, setClientId] = useState('')
  const [clientSecret, setClientSecret] = useState('')
  const [discoveryUrl, setDiscoveryUrl] = useState('')
  const [scopes, setScopes] = useState('openid email profile')
  const [domainHint, setDomainHint] = useState('')
  const [showAdvanced, setShowAdvanced] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const [createConnection, { loading }] = useMutation<CreateIdpConnectionMutation>(
    CreateIdpConnectionDocument,
    {
      refetchQueries: [{ query: GetIdpConnectionsDocument }],
    },
  )

  function resetForm() {
    setProvider('okta')
    setDisplayName('')
    setIssuer('')
    setClientId('')
    setClientSecret('')
    setDiscoveryUrl('')
    setScopes('openid email profile')
    setDomainHint('')
    setShowAdvanced(false)
    setError(null)
  }

  function handleOpenChange(next: boolean) {
    if (!next) {
      resetForm()
      onClose()
    }
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)

    const trimmedName = displayName.trim()
    const trimmedIssuer = issuer.trim()
    const trimmedClientId = clientId.trim()
    const trimmedSecret = clientSecret.trim()

    if (!trimmedName || !trimmedIssuer || !trimmedClientId || !trimmedSecret) {
      setError('Please fill in all required fields.')
      return
    }

    try {
      const res = await createConnection({
        variables: {
          input: {
            provider,
            displayName: trimmedName,
            issuer: trimmedIssuer,
            clientId: trimmedClientId,
            clientSecret: trimmedSecret,
            discoveryUrl: discoveryUrl.trim() || undefined,
            scopes: scopes.trim() || undefined,
            domainHint: domainHint.trim() || undefined,
          },
        },
      })

      const created = res.data?.createIdpConnection
      if (!created) {
        throw new Error('Connection could not be created.')
      }

      toast.success('Identity provider connection created.')
      const newId = created.id
      resetForm()
      onSuccess(newId)
    } catch (err: unknown) {
      const msg =
        err && typeof err === 'object' && 'message' in err
          ? String((err as { message: unknown }).message)
          : 'Failed to create identity provider connection.'
      setError(msg)
    }
  }

  const isValid =
    displayName.trim().length > 0 &&
    issuer.trim().length > 0 &&
    clientId.trim().length > 0 &&
    clientSecret.trim().length > 0

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <div className="flex items-center gap-2">
            <span className="grid h-8 w-8 place-items-center rounded-lg border border-primary/25 bg-primary/10 text-primary">
              <KeyRound className="h-4 w-4" />
            </span>
            <DialogTitle>Add Identity Provider</DialogTitle>
          </div>
          <DialogDescription>
            Configure an Enterprise OIDC identity provider connection for your workspace.
          </DialogDescription>
        </DialogHeader>

        <form onSubmit={(e) => void handleSubmit(e)} className="space-y-4 pt-1">
          {error ? (
            <Alert variant="destructive">
              <AlertTitle>Could not create connection</AlertTitle>
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          ) : null}

          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="idp-provider">Provider</Label>
              <select
                id="idp-provider"
                value={provider}
                onChange={(e) => setProvider(e.target.value as ProviderKey)}
                className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/30"
                disabled={loading}
              >
                {PROVIDER_OPTIONS.map((opt) => (
                  <option key={opt.value} value={opt.value}>
                    {opt.label}
                  </option>
                ))}
              </select>
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="idp-display-name">Display Name</Label>
              <Input
                id="idp-display-name"
                placeholder="e.g. Corporate Okta"
                value={displayName}
                onChange={(e) => setDisplayName(e.target.value)}
                disabled={loading}
                required
              />
            </div>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="idp-issuer">OIDC Issuer URL</Label>
            <Input
              id="idp-issuer"
              type="url"
              placeholder="https://acme.okta.com"
              value={issuer}
              onChange={(e) => setIssuer(e.target.value)}
              disabled={loading}
              required
            />
            <p className="text-[11px] text-muted-foreground">
              The canonical base URL of your identity provider.
            </p>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="idp-client-id">Client ID</Label>
              <Input
                id="idp-client-id"
                placeholder="OAuth client ID"
                value={clientId}
                onChange={(e) => setClientId(e.target.value)}
                disabled={loading}
                required
              />
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="idp-client-secret">Client Secret</Label>
              <Input
                id="idp-client-secret"
                type="password"
                placeholder="••••••••••••"
                value={clientSecret}
                onChange={(e) => setClientSecret(e.target.value)}
                disabled={loading}
                autoComplete="new-password"
                required
              />
            </div>
          </div>
          <p className="text-[11px] text-muted-foreground">
            Client secret is write-only and encrypted at rest. It will never be displayed again.
          </p>

          <div className="border-t border-border/60 pt-2">
            <button
              type="button"
              onClick={() => setShowAdvanced(!showAdvanced)}
              className="inline-flex items-center gap-1 text-xs font-medium text-muted-foreground hover:text-foreground"
            >
              {showAdvanced ? (
                <ChevronDown className="h-3.5 w-3.5" />
              ) : (
                <ChevronRight className="h-3.5 w-3.5" />
              )}
              Advanced settings
            </button>

            {showAdvanced ? (
              <div className="space-y-3 pt-3">
                <div className="space-y-1.5">
                  <Label htmlFor="idp-discovery-url">Discovery URL (optional)</Label>
                  <Input
                    id="idp-discovery-url"
                    type="url"
                    placeholder="https://acme.okta.com/.well-known/openid-configuration"
                    value={discoveryUrl}
                    onChange={(e) => setDiscoveryUrl(e.target.value)}
                    disabled={loading}
                  />
                </div>

                <div className="grid grid-cols-2 gap-3">
                  <div className="space-y-1.5">
                    <Label htmlFor="idp-scopes">Scopes</Label>
                    <Input
                      id="idp-scopes"
                      placeholder="openid email profile"
                      value={scopes}
                      onChange={(e) => setScopes(e.target.value)}
                      disabled={loading}
                    />
                  </div>

                  <div className="space-y-1.5">
                    <Label htmlFor="idp-domain-hint">Domain Hint (optional)</Label>
                    <Input
                      id="idp-domain-hint"
                      placeholder="e.g. acme.com"
                      value={domainHint}
                      onChange={(e) => setDomainHint(e.target.value)}
                      disabled={loading}
                    />
                  </div>
                </div>
              </div>
            ) : null}
          </div>

          <DialogFooter className="pt-2">
            <Button
              type="button"
              variant="outline"
              onClick={() => handleOpenChange(false)}
              disabled={loading}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={loading || !isValid}>
              {loading ? 'Creating…' : 'Create Connection'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
