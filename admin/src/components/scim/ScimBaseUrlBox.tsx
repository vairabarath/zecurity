import { useState } from 'react'
import { Check, Copy, Link2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'

// The SCIM base URL an admin pastes into the IdP (Okta/Entra/JumpCloud/Keycloak).
//
// ADR-025 §7: the path is IDENTICAL for every connection. Every SCIM request is
// scoped to (workspace_id, connection_id) derived from the presented bearer
// token, never from the URL. There is deliberately no per-connection path — the
// token is what distinguishes connections.
export function ScimBaseUrlBox() {
  const [copied, setCopied] = useState(false)
  const baseUrl = `${window.location.origin}/scim/v2`

  async function handleCopy() {
    try {
      await navigator.clipboard.writeText(baseUrl)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 2000)
    } catch {
      // Clipboard unavailable (insecure context / denied permission). The value
      // is visible and selectable in the input, so silent failure is acceptable.
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Link2 className="h-4 w-4 text-muted-foreground" />
          SCIM base URL
        </CardTitle>
        <CardDescription>
          Paste this into your identity provider&apos;s SCIM provisioning settings, together with a
          token minted below.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex items-center gap-2">
          <code
            data-testid="scim-base-url"
            className="flex h-10 flex-1 items-center overflow-x-auto rounded-lg border border-border bg-secondary px-3 font-mono text-sm text-foreground"
          >
            {baseUrl}
          </code>
          <Button type="button" variant="outline" onClick={handleCopy} aria-label="Copy SCIM base URL">
            {copied ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
          </Button>
        </div>
        <p className="text-xs text-muted-foreground">
          This URL is the same for every connection in this workspace. The bearer token you present
          is what identifies the connection — not the path.
        </p>
      </CardContent>
    </Card>
  )
}
