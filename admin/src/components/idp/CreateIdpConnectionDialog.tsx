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
import { PROVIDER_OPTIONS, providerLabel, type ProviderKey } from '@/components/idp/providers'

export function CreateIdpConnectionDialog({
  open,
  onClose,
  onSuccess,
  initialProvider,
}: {
  open: boolean
  onClose: () => void
  onSuccess: (createdId: string) => void
  /**
   * When set, this dialog behaves like Twingate's "Connect Okta" step: the
   * provider was already chosen in a prior picker step, so the provider
   * selector is not shown here — the dialog title and a static label reflect
   * the choice instead. When omitted, the provider selector is shown inline
   * (kept for robustness / direct usage without a picker step).
   */
  initialProvider?: ProviderKey
}) {
  // Only used as the editable dropdown's own state when initialProvider is
  // NOT set (direct-usage fallback). When initialProvider IS set, the
  // effective provider is derived from the prop on every render below
  // instead of being synced into state via an effect — the prop is already
  // the single source of truth in that case, so there is nothing to
  // synchronize and no cascading-render risk.
  const [provider, setProvider] = useState<ProviderKey>(initialProvider ?? 'okta')
  const effectiveProvider = initialProvider ?? provider
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
    setProvider(initialProvider ?? 'okta')
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
            provider: effectiveProvider,
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

      // Say exactly what the server proved. createIdpConnection validates the
      // issuer's OIDC discovery document BEFORE persisting (it refuses to save
      // an unreachable/invalid issuer), so "discovery verified" is accurate.
      // It does NOT validate the client ID, client secret or redirect URI —
      // never word this as "credentials verified".
      toast.success('Connection created — OIDC discovery verified.')
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
            <DialogTitle>
              {initialProvider ? `Connect ${providerLabel(initialProvider)}` : 'Add Identity Provider'}
            </DialogTitle>
          </div>
          <DialogDescription>
            {initialProvider
              ? `Enter the ${providerLabel(initialProvider)} connection details below.`
              : 'Configure an Enterprise OIDC identity provider connection for your workspace.'}
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
            {initialProvider ? (
              <div className="space-y-1.5">
                <Label>Provider</Label>
                <div className="flex h-9 items-center rounded-md border border-input bg-muted/40 px-3 text-sm text-muted-foreground">
                  {providerLabel(initialProvider)}
                </div>
              </div>
            ) : (
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
            )}

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
            {/* Twingate-mirror: when the provider is Okta (the per-provider
              "Connect Okta" step), the field is labelled "Okta Domain" and the
              Advanced settings block is hidden — exactly matching Twingate's
              own dialog (Okta Domain / Client ID / Client Secret only). For
              every other provider the generic OIDC form is unchanged. */}
            <Label htmlFor="idp-issuer">
              {effectiveProvider === 'okta' ? 'Okta Domain' : 'OIDC Issuer URL'}
            </Label>
            <Input
              id="idp-issuer"
              type="url"
              placeholder="https://acme.okta.com"
              value={issuer}
              onChange={(e) => setIssuer(e.target.value)}
              disabled={loading}
              required
            />
            {effectiveProvider !== 'okta' ? (
              <p className="text-[11px] text-muted-foreground">
                The canonical base URL of your identity provider.
              </p>
            ) : null}
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
          {/* Honesty about the scope of the create-time check (ADR-025 / OIDC
            distinction): the server validates issuer reachability + the OIDC
            discovery document and refuses to save a bad one, but discovery is
            an unauthenticated endpoint — no credential is sent, so nothing here
            proves the client ID/secret or the redirect URI. The first real
            proof of those is a sign-in. This must not be softened into
            "credentials verified". */}
          <p className="text-[11px] text-muted-foreground">
            On save, Zecurity verifies that the{' '}
            {effectiveProvider === 'okta' ? 'domain' : 'issuer'} serves a valid OpenID Connect
            discovery document, and will not create the connection if it does not. The client ID,
            client secret and redirect URI are not verified until the first sign-in.
          </p>

          {/* Twingate-mirror: the Advanced settings block (Discovery URL,
            Scopes, Domain Hint) is hidden for the Okta "Connect Okta" step,
            because Twingate's own Okta dialog shows only Okta Domain / Client
            ID / Client Secret. For every other provider the block stays, so
            those connections can still be tuned. */}
          {effectiveProvider !== 'okta' ? (
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
          ) : null}

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
              {/* The mutation performs a live OIDC discovery request before it
                saves, so the pending label names that step rather than
                implying the row is already being written. */}
              {loading ? 'Verifying…' : 'Create Connection'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
