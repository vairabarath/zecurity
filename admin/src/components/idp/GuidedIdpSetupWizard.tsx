import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { ArrowLeft, ArrowRight, Check, KeyRound, Ticket } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from '@/components/ui/dialog'
import {
  GetIdpConnectionsDocument,
  type GetIdpConnectionsQuery,
} from '@/generated/graphql'
import { CreateIdpConnectionDialog } from '@/components/idp/CreateIdpConnectionDialog'
import { ScimConfigCard, type ScimConfigConnection } from '@/components/scim/ScimConfigCard'
import { ScimBaseUrlBox } from '@/components/scim/ScimBaseUrlBox'
import { ScimTokenPanel } from '@/components/scim/ScimTokenPanel'
import type { ProviderKey } from '@/components/idp/providers'

// Optional guided entry point (Sprint 17 FE Phase 6 — UX nice-to-have).
//
// It is ADDITIVE only: it wraps the already-shipped FE-0 (CreateIdpConnectionDialog)
// and FE-1 (ScimConfigCard / ScimBaseUrlBox / ScimTokenPanel) components into a
// single multi-step flow. It does NOT own or replace any of them, and the direct
// paths (the "Add Identity Provider" dialog and the connection detail page) remain
// fully available. No new GraphQL operations, no backend change.
//
// SCIM enablement stays server-authoritative (ADR-025 §3.1): Step 2's
// ScimConfigCard already attempts updateScimConfig and surfaces the break-glass
// dialog on the server's refusal — this shell never re-implements the MappingGate.
type Step = 1 | 2 | 3

type ConnectionRow = GetIdpConnectionsQuery['idpConnections'][number]

function toScimConfigConnection(c: ConnectionRow): ScimConfigConnection {
  return {
    id: c.id,
    displayName: c.displayName,
    provider: c.provider,
    managed: c.managed,
    subjectClaim: c.subjectClaim ?? '',
    scimIdentifier: c.scimIdentifier ?? '',
    scimEnabled: c.scimEnabled,
  }
}

export function GuidedIdpSetupWizard({
  open,
  onClose,
  initialProvider,
}: {
  open: boolean
  onClose: () => void
  /**
   * Provider chosen in the AddIdentityProviderMenu popup that precedes this
   * wizard (Twingate-parity entry point: click "Add Identity Provider" → pick
   * a provider from the popup → this wizard opens straight into Step 1 for
   * that provider, then continues into SCIM automatically).
   */
  initialProvider?: ProviderKey
}) {
  const navigate = useNavigate()
  const [step, setStep] = useState<Step>(1)
  const [createdId, setCreatedId] = useState<string | null>(null)
  // Re-read the freshly created connection from the workspace list so Step 2
  // receives a fully-populated ScimConfigConnection (subjectClaim / scimIdentifier /
  // managed) — the same shape ScimConfigCard expects on the detail page.
  const [connection, setConnection] = useState<ScimConfigConnection | null>(null)

  function reset() {
    setStep(1)
    setCreatedId(null)
    setConnection(null)
  }

  function handleClose() {
    reset()
    onClose()
  }

  function handleCreated(id: string) {
    setCreatedId(id)
    setStep(2)
  }

  function handleStep2Changed() {
    // ScimConfigCard performed an update; nothing to do here beyond letting the
    // user continue. The detail page will refetch on visit.
  }

  function finish() {
    const id = createdId ?? connection?.id
    reset()
    if (id) {
      navigate(`/idp-connections/${id}`)
    }
    onClose()
  }

  // Once we have a created id, derive the connection row from the cache so Step 2
  // is pre-populated. We look it up lazily inside the render of Step 2.
  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) handleClose() }}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <div className="flex items-center gap-2">
            <span className="grid h-8 w-8 place-items-center rounded-lg border border-primary/25 bg-primary/10 text-primary">
              <KeyRound className="h-4 w-4" />
            </span>
            <DialogTitle>Set up identity provider</DialogTitle>
          </div>
          <DialogDescription>
            Create the OIDC connection, configure SCIM mapping, and mint a token — in one guided flow.
          </DialogDescription>
        </DialogHeader>

        {step === 1 ? (
          <div className="pt-1">
            <Step1Note />
            <CreateIdpConnectionDialog
              open={true}
              initialProvider={initialProvider}
              onClose={handleClose}
              onSuccess={handleCreated}
            />
          </div>
        ) : null}

        {step === 2 && createdId ? (
          <Step2
            connectionId={createdId}
            onChanged={handleStep2Changed}
            onBack={() => setStep(1)}
            onSkip={finish}
            onNext={() => setStep(3)}
          />
        ) : null}

        {step === 3 && createdId ? (
          <Step3 connectionId={createdId} onBack={() => setStep(2)} onFinish={finish} />
        ) : null}
      </DialogContent>
    </Dialog>
  )
}

function Step1Note() {
  return (
    <p className="pb-2 text-xs text-muted-foreground">
      Step 1 of 3 — provide the OIDC connection details, then continue to SCIM configuration.
    </p>
  )
}

// Step 2: SCIM mapping configuration. Reuses ScimConfigCard (which itself handles
// the server-authoritative enable + break-glass flow). We resolve the connection
// row from the workspace list query so the card gets a fully-populated shape.
function Step2({
  connectionId,
  onChanged,
  onBack,
  onSkip,
  onNext,
}: {
  connectionId: string
  onChanged: () => void
  onBack: () => void
  onSkip: () => void
  onNext: () => void
}) {
  // Read-only lookup from the cache; matches how IdpConnectionDetail derives its
  // connection. No new backend field.
  const { data } = useQueryConnection()
  const row = (data?.idpConnections ?? []).find((c) => c.id === connectionId)
  const conn = row ? toScimConfigConnection(row) : null

  return (
    <div className="space-y-4 pt-1">
      <div className="flex items-center justify-between">
        <p className="text-xs text-muted-foreground">
          Step 2 of 3 — configure the identity mapping, then enable SCIM (or skip).
        </p>
        <div className="flex items-center gap-2">
          <Button variant="ghost" size="sm" onClick={onSkip}>
            Skip SCIM
          </Button>
          <Button variant="outline" size="sm" onClick={onBack} disabled={!conn}>
            <ArrowLeft className="mr-1 h-4 w-4" /> Back
          </Button>
        </div>
      </div>
      {conn ? (
        <ScimConfigCard connection={conn} onChanged={onChanged} />
      ) : (
        <p className="text-sm text-muted-foreground">Loading connection…</p>
      )}
      <div className="flex justify-end">
        <Button onClick={onNext} disabled={!conn}>
          Next: mint token <ArrowRight className="ml-1 h-4 w-4" />
        </Button>
      </div>
    </div>
  )
}

// Step 3: SCIM base URL + token mint. Reuses the existing detail-page pieces
// (ScimBaseUrlBox carries no secret; ScimTokenPanel keeps its no-cache show-once
// secret handling — we never touch that behavior here).
function Step3({
  connectionId,
  onBack,
  onFinish,
}: {
  connectionId: string
  onBack: () => void
  onFinish: () => void
}) {
  return (
    <div className="space-y-4 pt-1">
      <div className="flex items-center justify-between">
        <p className="text-xs text-muted-foreground">
          Step 3 of 3 — copy the SCIM base URL and mint a token to paste into your identity provider.
        </p>
        <Button variant="outline" size="sm" onClick={onBack}>
          <ArrowLeft className="mr-1 h-4 w-4" /> Back
        </Button>
      </div>
      <ScimBaseUrlBox />
      <ScimTokenPanel connectionId={connectionId} />
      <div className="flex items-center justify-end gap-2">
        <span className="mr-auto inline-flex items-center gap-1 text-xs text-muted-foreground">
          <Check className="h-3.5 w-3.5" /> Setup complete
        </span>
        <Button onClick={onFinish}>
          <Ticket className="mr-1 h-4 w-4" /> Finish
        </Button>
      </div>
    </div>
  )
}

// Local query hook kept inside this file so the component stays self-contained
// and we do not thread the query through props. Mirrors IdpConnectionDetail's
// cache-and-network read of GetIdpConnections.
function useQueryConnection() {
  return useQuery<GetIdpConnectionsQuery>(GetIdpConnectionsDocument, {
    fetchPolicy: 'cache-and-network',
  })
}
