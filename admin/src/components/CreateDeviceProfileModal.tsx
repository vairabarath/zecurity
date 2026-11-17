import { useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import {
  CreateDeviceProfileDocument,
  AddProfileRequirementDocument,
  GetDeviceProfilesDocument,
  GetSupportedPostureChecksDocument,
  SetProfilePlatformsDocument,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { AlertTriangle, Loader2, Plug, ShieldCheck, X } from 'lucide-react'
import { toast } from 'sonner'
import { CountryRuleEditor } from '@/components/CountryRuleEditor'
import { PlatformPostureCard } from '@/components/PlatformPostureCard'
import { COUNTRY_CHECK_ID } from '@/lib/countries'
import { EMPTY_COUNTRY_RULE, countryRuleProblem, type CountryRuleValue } from '@/lib/countryRule'
import { PROFILE_PLATFORMS, isMinOSCheck, validMinVersion } from '@/lib/profilePlatforms'

interface CreateDeviceProfileModalProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onSuccess?: () => void
}

// Same layout as EditDeviceProfileModal (ADR-031): the name, the server-side
// requirements, and one card per platform ("Approved Operating Systems") with
// its posture checks.
export function CreateDeviceProfileModal({
  open,
  onOpenChange,
  onSuccess,
}: CreateDeviceProfileModalProps) {
  const [name, setName] = useState('')
  const [checkedIds, setCheckedIds] = useState<Set<string>>(new Set())
  const [platforms, setPlatforms] = useState<Set<string>>(new Set())
  // ADR-031 D-11: the minimum of each Minimum OS Version check, by check id.
  const [minVersions, setMinVersions] = useState<Record<string, string>>({})
  // PENDING-20: the country check has its own editor (allow/deny lists).
  const [countryRule, setCountryRule] = useState<CountryRuleValue>(EMPTY_COUNTRY_RULE)
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  const { data: checksData } = useQuery(GetSupportedPostureChecksDocument, { skip: !open })
  const checksFor = (platform: string) =>
    (checksData?.supportedPostureChecks ?? []).filter((check) => check.platform === platform)
  // Server-side checks (ADR-030), e.g. "Approved by an admin". The country
  // check (PENDING-20) is server-side too but has its own editor below.
  const serverChecks = (checksData?.supportedPostureChecks ?? []).filter(
    (check) => check.platform === 'any' && check.id !== COUNTRY_CHECK_ID,
  )
  const countryCheckAvailable = (checksData?.supportedPostureChecks ?? []).some(
    (check) => check.id === COUNTRY_CHECK_ID,
  )
  const countryProblem = countryRuleProblem(countryRule)

  const [createDeviceProfile] = useMutation(CreateDeviceProfileDocument, {
    refetchQueries: [{ query: GetDeviceProfilesDocument }],
  })
  const [addProfileRequirement] = useMutation(AddProfileRequirementDocument)
  const [setProfilePlatforms] = useMutation(SetProfilePlatformsDocument)

  function reset() {
    setName('')
    setCheckedIds(new Set())
    setPlatforms(new Set())
    setMinVersions({})
    setCountryRule(EMPTY_COUNTRY_RULE)
    setError(null)
  }

  function handleClose(isOpen: boolean) {
    if (!isOpen) reset()
    onOpenChange(isOpen)
  }

  function toggleCheck(checkId: string) {
    setCheckedIds((prev) => {
      const next = new Set(prev)
      if (next.has(checkId)) next.delete(checkId)
      else next.add(checkId)
      return next
    })
  }

  function setPlatformEnabled(platform: string, enabled: boolean) {
    setPlatforms((prev) => {
      const next = new Set(prev)
      if (enabled) next.add(platform)
      else next.delete(platform)
      return next
    })
  }

  function comingSoon() {
    toast('Trust method integrations are coming soon.')
  }

  // Only the checks of platforms that are switched on are saved.
  const platformOf = (checkId: string) => checkId.split('.')[0]
  const savedChecks = [...checkedIds].filter(
    (id) => !PROFILE_PLATFORMS.some((p) => p.id === platformOf(id)) || platforms.has(platformOf(id)),
  )

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)

    if (!name.trim()) {
      setError('Profile name is required')
      return
    }
    if (platforms.size === 0) {
      setError('Turn on at least one platform under Approved Operating Systems.')
      return
    }
    if (countryProblem) {
      setError(countryProblem)
      return
    }
    const badMinimum = savedChecks.find((id) => isMinOSCheck(id) && !validMinVersion(minVersions[id] ?? ''))
    if (badMinimum) {
      const platform = PROFILE_PLATFORMS.find((p) => badMinimum.startsWith(`${p.id}.`))?.label ?? ''
      setError(
        `Enter the minimum ${platform} version: numbers separated by dots, for example 10.0.22631 or 14.`,
      )
      return
    }

    setSubmitting(true)
    try {
      const result = await createDeviceProfile({
        variables: { name: name.trim(), manualTrust: true },
      })
      const profileId = result.data?.createDeviceProfile.id
      if (!profileId) throw new Error('Profile was not created')

      for (const checkId of savedChecks) {
        await addProfileRequirement({
          variables: {
            profileId,
            checkId,
            allowUnsupported: false,
            ...(isMinOSCheck(checkId) ? { minVersion: minVersions[checkId].trim() } : {}),
          },
        })
      }
      if (countryRule.enabled) {
        await addProfileRequirement({
          variables: {
            profileId,
            checkId: COUNTRY_CHECK_ID,
            allowUnsupported: false,
            countries: { allow: countryRule.allow, deny: countryRule.deny },
          },
        })
      }

      // ADR-031: a new profile allows no platform until one is turned on.
      // The platforms go last, after their requirements are in place.
      await setProfilePlatforms({
        variables: {
          profileId,
          platforms: PROFILE_PLATFORMS.map((p) => p.id).filter((id) => platforms.has(id)),
        },
      })

      toast.success(`Device profile "${name.trim()}" created`)
      onSuccess?.()
      handleClose(false)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create device profile')
    } finally {
      setSubmitting(false)
    }
  }

  const isValid = name.trim().length > 0 && platforms.size > 0 && countryProblem === null

  if (!open) return null

  return (
    <div className="fixed inset-0 z-50">
      <div
        className="absolute inset-0 bg-black/50 backdrop-blur-sm"
        onClick={() => handleClose(false)}
      />
      <div className="absolute right-0 top-0 h-full w-full max-w-3xl app-panel animate-slide-in">
        <form onSubmit={handleSubmit} className="flex h-full flex-col">
          <div className="flex items-center gap-4 border-b border-border p-5">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-[oklch(0.78_0.10_235/0.16)] text-[oklch(0.78_0.10_235)]">
              <ShieldCheck className="h-5 w-5" />
            </div>
            <div className="flex-1">
              <h2 className="text-lg font-semibold">Create Device Profile</h2>
              <p className="text-sm text-muted-foreground">
                Choose which platforms may connect and what each must meet.
              </p>
            </div>
            <button
              type="button"
              onClick={() => handleClose(false)}
              aria-label="Close"
              className="flex h-9 w-9 items-center justify-center rounded-lg border border-border text-muted-foreground transition hover:bg-secondary hover:text-foreground"
            >
              <X className="h-4 w-4" />
            </button>
          </div>

          <div className="flex-1 space-y-6 overflow-y-auto p-5">
            <div className="space-y-2">
              <Label className="text-sm font-semibold">Profile Name</Label>
              <Input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Profile Name"
                className="h-11 font-medium"
                autoFocus
              />
              <p className="text-xs text-muted-foreground">Required. It can't be changed later.</p>
            </div>

            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <Label className="text-sm font-semibold">Verification Requirements</Label>
                <button
                  type="button"
                  onClick={comingSoon}
                  className="text-xs font-semibold text-primary underline-offset-2 hover:underline"
                >
                  Learn More
                </button>
              </div>

              {serverChecks.map((check) => (
                <div key={check.id} className="rounded-2xl border border-border p-4">
                  <div className="flex items-center justify-between">
                    <div className="flex items-center gap-2 text-sm font-semibold">
                      <ShieldCheck className="h-4 w-4 text-muted-foreground" />
                      {check.label}
                    </div>
                    <Switch
                      checked={checkedIds.has(check.id)}
                      onCheckedChange={() => toggleCheck(check.id)}
                      aria-label={check.label}
                    />
                  </div>
                  <p className="mt-2 text-xs text-muted-foreground">
                    Admits only devices an admin approved on the Devices page. Leave
                    off to allow personal devices (BYOD).
                  </p>
                </div>
              ))}

              {countryCheckAvailable && (
                <CountryRuleEditor value={countryRule} onChange={setCountryRule} />
              )}

              <button
                type="button"
                onClick={comingSoon}
                className="flex w-full items-center justify-between rounded-2xl border border-border p-4 text-left transition hover:bg-secondary"
              >
                <span className="flex items-center gap-2 text-sm font-semibold">
                  <Plug className="h-4 w-4 text-muted-foreground" />
                  Connect Trust Methods
                </span>
                <span className="text-muted-foreground">&rarr;</span>
              </button>
            </div>

            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <Label className="text-sm font-semibold">Approved Operating Systems</Label>
                <button
                  type="button"
                  onClick={comingSoon}
                  className="text-xs font-semibold text-primary underline-offset-2 hover:underline"
                >
                  Learn More
                </button>
              </div>
              <p className="text-xs text-muted-foreground">
                Turn on each platform this profile allows, then choose what its devices must meet.
              </p>

              {!checksData ? (
                <p className="text-sm text-muted-foreground">Loading posture checks…</p>
              ) : (
                <div className="grid gap-3 sm:grid-cols-2">
                  {PROFILE_PLATFORMS.map((platform) => (
                    <PlatformPostureCard
                      key={platform.id}
                      platform={platform}
                      enabled={platforms.has(platform.id)}
                      onEnabledChange={(on) => setPlatformEnabled(platform.id, on)}
                      checks={checksFor(platform.id)}
                      required={checkedIds}
                      onToggleRequired={toggleCheck}
                      minVersions={minVersions}
                      onMinVersionChange={(checkId, version) =>
                        setMinVersions((prev) => ({ ...prev, [checkId]: version }))
                      }
                    />
                  ))}
                </div>
              )}
            </div>

            {error && (
              <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">
                <AlertTriangle className="h-4 w-4 mt-0.5 shrink-0" />
                <span>{error}</span>
              </div>
            )}
          </div>

          <div className="flex items-center justify-end gap-2 border-t border-border p-5">
            <Button type="button" variant="outline" onClick={() => handleClose(false)} disabled={submitting}>
              Cancel
            </Button>
            <Button type="submit" disabled={!isValid || submitting}>
              {submitting && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
              Create Profile
            </Button>
          </div>
        </form>
      </div>

      <style>{`
        @keyframes slide-in {
          from {
            transform: translateX(100%);
          }
          to {
            transform: translateX(0);
          }
        }
        .animate-slide-in {
          animation: slide-in 0.3s ease-out;
        }
      `}</style>
    </div>
  )
}
