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
// The admin SPA and the controller API may be served from different origins
// (e.g. SPA at an edge host, controller behind a tunnel). The SCIM base URL
// must be the *controller's* publicly reachable origin — that is what Okta/
// Entra/Entra/JumpCloud/Keycloak will call, not the admin app's origin.
//
// Source order: VITE_API_ORIGIN (controller/API origin, set in prod), else fall
// back to window.location.origin, which is only correct when the SPA and the
// controller share an origin (single-binary / dev). The path is always /scim/v2.
const CONTROLLER_ORIGIN = import.meta.env.VITE_API_ORIGIN ?? window.location.origin

export function ScimBaseUrlBox() {
  const [copied, setCopied] = useState(false)
  const baseUrl = `${CONTROLLER_ORIGIN}/scim/v2`

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
