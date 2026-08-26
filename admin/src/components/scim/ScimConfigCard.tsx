import { useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import { AlertTriangle, ChevronDown, KeyRound } from 'lucide-react'
import { toast } from 'sonner'
import {
  EnableScimBreakGlassDocument,
  GetScimProviderProfilesDocument,
  UpdateScimConfigDocument,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { StatusPill } from '@/lib/console'
import { BreakGlassDialog } from '@/components/scim/BreakGlassDialog'

type ScimProviderProfile = {
  key: string
  displayName: string
  defaultSubjectClaim: string
  defaultScimIdentifier: string
  supportsCreate: boolean
  supportsDelete: boolean
  supportsPatch: boolean
  paginationOk: boolean
  supportsProbeLifecycle: boolean
  quirks: string[]
}

export type ScimConfigConnection = {
  id: string
  displayName: string
  provider: string
  managed: boolean
  subjectClaim: string
  scimIdentifier: string
  scimEnabled: boolean
}

// Extracts a displayable message from an Apollo error without inspecting
// extensions.code. updateScimConfig refuses through apperr.UserError, which the
// presenter surfaces verbatim WITHOUT a code — so code-branching would be wrong
// here. Any refusal means "the server said no"; never string-match it either.
function errorMessage(err: unknown): string {
  if (err && typeof err === 'object' && 'message' in err) {
    const msg = (err as { message?: unknown }).message
    if (typeof msg === 'string' && msg.trim()) return msg
  }
  return 'The server refused the request.'
}

// Confirms that saving a changed mapping on a SCIM-enabled connection turns SCIM
// off. This is the backend's own behaviour, not a client-side rule: editing
// subjectClaim or scimIdentifier invalidates whatever proof enabled SCIM, so the
// mutation force-disables it in the same write (ADR-025 §3.1).
function MappingChangeDialog({
  open,
  loading,
  onClose,
  onConfirm,
}: {
  open: boolean
  loading: boolean
  onClose: () => void
  onConfirm: () => void
}) {
  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) onClose() }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Saving this mapping disables SCIM</DialogTitle>
        </DialogHeader>
        <div className="space-y-3 pt-1">
          <Alert variant="destructive">
            <AlertTriangle className="h-4 w-4" />
            <AlertDescription>
              SCIM is currently enabled for this connection. Changing the subject claim or SCIM
              identifier invalidates the mapping that enabled it, so the server turns SCIM off in
              the same write.
            </AlertDescription>
          </Alert>
          <p className="text-sm text-muted-foreground">
            Re-enabling is a deliberate act. While the identity mapping cannot be proven, that means
            a break-glass override.
          </p>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={loading}>Cancel</Button>
          <Button variant="destructive" onClick={onConfirm} disabled={loading}>
            {loading ? 'Saving…' : 'Save and disable SCIM'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export function ScimConfigCard({
  connection,
  onChanged,
}: {
  connection: ScimConfigConnection
  onChanged: () => void
}) {
  const [subjectClaim, setSubjectClaim] = useState(connection.subjectClaim)
  const [scimIdentifier, setScimIdentifier] = useState(connection.scimIdentifier)
  const [presetKey, setPresetKey] = useState<string | null>(null)
  const [showMappingConfirm, setShowMappingConfirm] = useState(false)
  const [breakGlass, setBreakGlass] = useState<{ open: boolean; refusal: string | null }>({
    open: false,
    refusal: null,
  })

  const { data: profilesData } = useQuery(GetScimProviderProfilesDocument, {
    fetchPolicy: 'cache-first',
  })
  const profiles: ScimProviderProfile[] = (profilesData?.scimProviderProfiles ??
    []) as ScimProviderProfile[]

  // Which profile the connection's values are compared against: an explicitly
  // picked preset, else the profile matching the connection's provider, else
  // "generic" (always available as the fallback).
  const activeProfile =
    profiles.find((p) => p.key === presetKey) ??
    profiles.find((p) => p.key === connection.provider) ??
    profiles.find((p) => p.key === 'generic') ??
    null

  const [updateScimConfig, { loading: saving }] = useMutation(UpdateScimConfigDocument)
  const [enableBreakGlass, { loading: overriding }] = useMutation(EnableScimBreakGlassDocument)

  const readOnly = connection.managed
  const mappingDirty =
    subjectClaim !== connection.subjectClaim || scimIdentifier !== connection.scimIdentifier
  const mappingValid =
    !!subjectClaim.trim() && !!scimIdentifier.trim() && subjectClaim.trim() !== scimIdentifier.trim()

  function applyPreset(profile: ScimProviderProfile) {
    setPresetKey(profile.key)
    setSubjectClaim(profile.defaultSubjectClaim)
    setScimIdentifier(profile.defaultScimIdentifier)
  }

  async function saveMapping() {
    try {
      await updateScimConfig({
        variables: {
          connectionId: connection.id,
          input: { subjectClaim: subjectClaim.trim(), scimIdentifier: scimIdentifier.trim() },
        },
      })
      setShowMappingConfirm(false)
      toast.success(
        connection.scimEnabled
          ? 'Mapping saved. SCIM was disabled and must be re-enabled deliberately.'
          : 'Identity mapping saved.',
      )
      onChanged()
    } catch (err) {
      setShowMappingConfirm(false)
      toast.error(errorMessage(err))
    }
  }

  function handleSaveMapping() {
    if (!mappingValid) return
    // Only warn when SCIM is actually on — otherwise there is nothing to disable.
    if (connection.scimEnabled) {
      setShowMappingConfirm(true)
      return
    }
    void saveMapping()
  }

  // Disabling is always permitted: it is the fail-closed direction.
  //
  // Enabling is NOT pre-gated client-side. The gate lives server-side and is
  // fail-closed (ADR-025 §3.1); duplicating it here would either weaken it or
  // hardcode a permanent "no". We attempt the normal enable and treat ANY
  // refusal as "offer the deliberate break-glass override".
  async function handleToggle(next: boolean) {
    try {
      await updateScimConfig({
        variables: { connectionId: connection.id, input: { scimEnabled: next } },
      })
      toast.success(next ? 'SCIM enabled.' : 'SCIM disabled.')
      onChanged()
    } catch (err) {
      if (!next) {
        toast.error(errorMessage(err))
        return
      }
      setBreakGlass({ open: true, refusal: errorMessage(err) })
    }
  }

  async function handleBreakGlass(reason: string) {
    try {
      await enableBreakGlass({ variables: { connectionId: connection.id, reason } })
      setBreakGlass({ open: false, refusal: null })
      toast.success('SCIM enabled via break-glass override. The action was audited.')
      onChanged()
    } catch (err) {
      toast.error(errorMessage(err))
    }
  }

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="space-y-1.5">
            <CardTitle className="flex items-center gap-2">
              <KeyRound className="h-4 w-4 text-muted-foreground" />
              SCIM configuration
            </CardTitle>
            <CardDescription>
              The two extractors that must resolve to the same identity for the same person. A wrong
              pairing silently splits or merges accounts.
            </CardDescription>
          </div>
          <div className="flex items-center gap-3">
            <StatusPill
              label={connection.scimEnabled ? 'SCIM enabled' : 'SCIM disabled'}
              tone={connection.scimEnabled ? 'ok' : 'muted'}
            />
            <Switch
              checked={connection.scimEnabled}
              disabled={readOnly || saving}
              onCheckedChange={handleToggle}
              aria-label="Toggle SCIM provisioning"
            />
          </div>
        </div>
      </CardHeader>

      <CardContent className="space-y-5">
        {readOnly ? (
          <Alert>
            <AlertTriangle className="h-4 w-4" />
            <AlertDescription>
              This is a platform-managed connection. Its SCIM configuration is provider-managed and
              cannot be edited here.
            </AlertDescription>
          </Alert>
        ) : null}

        <div className="space-y-1.5">
          <Label>Provider preset</Label>
          <DropdownMenu>
            <DropdownMenuTrigger asChild disabled={readOnly}>
              <Button variant="outline" className="h-10 w-full justify-between rounded-lg bg-secondary">
                <span>{activeProfile?.displayName ?? 'Generic'}</span>
                <ChevronDown className="h-4 w-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent className="w-64">
              {profiles.map((profile) => (
                <DropdownMenuItem
                  key={profile.key}
                  onClick={() => applyPreset(profile)}
                  className="cursor-pointer"
                >
                  <span className="flex-1">{profile.displayName}</span>
                  <span className="ml-2 font-mono text-[11px] text-muted-foreground">
                    {profile.defaultSubjectClaim} / {profile.defaultScimIdentifier}
                  </span>
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
          {activeProfile ? (
            <p className="text-xs text-muted-foreground">
              Preset defaults: <span className="font-mono">{activeProfile.defaultSubjectClaim}</span>{' '}
              / <span className="font-mono">{activeProfile.defaultScimIdentifier}</span>
              {activeProfile.supportsProbeLifecycle
                ? null
                : ' · this provider does not support the full probe round-trip'}
            </p>
          ) : null}
          {activeProfile && activeProfile.quirks.length > 0 ? (
            <ul className="mt-1 space-y-0.5 text-xs text-muted-foreground">
              {activeProfile.quirks.map((quirk) => (
                <li key={quirk}>· {quirk}</li>
              ))}
            </ul>
          ) : null}
        </div>

        <div className="grid gap-4 sm:grid-cols-2">
          <div className="space-y-1.5">
            <Label htmlFor="subject-claim">OIDC subject claim</Label>
            <Input
              id="subject-claim"
              value={subjectClaim}
              onChange={(e) => setSubjectClaim(e.target.value)}
              disabled={readOnly}
              placeholder="sub"
            />
            <p className="text-xs text-muted-foreground">Read by the login path (Entra: oid).</p>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="scim-identifier">SCIM identifier</Label>
            <Input
              id="scim-identifier"
              value={scimIdentifier}
              onChange={(e) => setScimIdentifier(e.target.value)}
              disabled={readOnly}
              placeholder="externalId"
            />
            <p className="text-xs text-muted-foreground">
              Read by the provisioning path. Never assume it equals the subject claim.
            </p>
          </div>
        </div>

        {mappingDirty && !mappingValid ? (
          <p className="text-xs text-destructive">
            Both fields are required and they must differ from each other.
          </p>
        ) : null}

        {!readOnly ? (
          <div className="flex items-center gap-2">
            <Button onClick={handleSaveMapping} disabled={!mappingDirty || !mappingValid || saving}>
              {saving ? 'Saving…' : 'Save mapping'}
            </Button>
            {mappingDirty ? (
              <Button
                variant="outline"
                onClick={() => {
                  setSubjectClaim(connection.subjectClaim)
                  setScimIdentifier(connection.scimIdentifier)
                  setPresetKey(null)
                }}
                disabled={saving}
              >
                Reset
              </Button>
            ) : null}
          </div>
        ) : null}
      </CardContent>

      <MappingChangeDialog
        open={showMappingConfirm}
        loading={saving}
        onClose={() => setShowMappingConfirm(false)}
        onConfirm={() => void saveMapping()}
      />

      <BreakGlassDialog
        open={breakGlass.open}
        connectionName={connection.displayName}
        refusal={breakGlass.refusal}
        loading={overriding}
        onClose={() => setBreakGlass({ open: false, refusal: null })}
        onConfirm={(reason) => void handleBreakGlass(reason)}
      />
    </Card>
  )
}
