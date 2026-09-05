import { useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import {
  CreateDeviceProfileDocument,
  AddProfileRequirementDocument,
  GetDeviceProfilesDocument,
  GetSupportedPostureChecksDocument,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import {
  AlertTriangle,
  ChevronLeft,
  Laptop,
  Loader2,
  MonitorSmartphone,
  Plug,
  ShieldCheck,
  X,
} from 'lucide-react'
import { toast } from 'sonner'

interface CreateDeviceProfileModalProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onSuccess?: () => void
}

type OS = 'linux' | 'windows' | 'macos'

export function CreateDeviceProfileModal({
  open,
  onOpenChange,
  onSuccess,
}: CreateDeviceProfileModalProps) {
  const [os, setOs] = useState<OS | null>(null)
  const [name, setName] = useState('')
  const [checkedIds, setCheckedIds] = useState<Set<string>>(new Set())
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  const { data: checksData } = useQuery(GetSupportedPostureChecksDocument, {
    skip: os !== 'linux',
  })
  const linuxChecks = (checksData?.supportedPostureChecks ?? []).filter(
    (check) => check.platform === 'linux',
  )

  const [createDeviceProfile] = useMutation(CreateDeviceProfileDocument, {
    refetchQueries: [{ query: GetDeviceProfilesDocument }],
  })
  const [addProfileRequirement] = useMutation(AddProfileRequirementDocument)

  function reset() {
    setOs(null)
    setName('')
    setCheckedIds(new Set())
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

  function comingSoon() {
    toast('Trust method integrations are coming soon.')
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)

    if (!name.trim()) {
      setError('Profile name is required')
      return
    }

    setSubmitting(true)
    try {
      const result = await createDeviceProfile({
        variables: { name: name.trim(), manualTrust: true },
      })
      const profileId = result.data?.createDeviceProfile.id
      if (!profileId) throw new Error('Profile was not created')

      for (const checkId of checkedIds) {
        await addProfileRequirement({
          variables: { profileId, checkId, allowUnsupported: false },
        })
      }

      toast.success(`Device profile "${name.trim()}" created`)
      onSuccess?.()
      handleClose(false)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create device profile')
    } finally {
      setSubmitting(false)
    }
  }

  const isValid = name.trim().length > 0

  if (!open) return null

  return (
    <div className="fixed inset-0 z-50">
      <div
        className="absolute inset-0 bg-black/50 backdrop-blur-sm"
        onClick={() => handleClose(false)}
      />
      <div className="absolute right-0 top-0 h-full w-full max-w-md app-panel animate-slide-in">
        <div className="flex h-full flex-col">
          <div className="flex items-center gap-4 border-b border-border p-5">
            {os && (
              <button
                type="button"
                onClick={() => setOs(null)}
                className="flex h-9 w-9 items-center justify-center rounded-lg border border-border text-muted-foreground transition hover:bg-secondary hover:text-foreground"
              >
                <ChevronLeft className="h-4 w-4" />
              </button>
            )}
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-[oklch(0.78_0.10_235/0.16)] text-[oklch(0.78_0.10_235)]">
              <ShieldCheck className="h-5 w-5" />
            </div>
            <div className="flex-1">
              <h2 className="text-lg font-semibold">
                {os === 'linux' ? 'Create Trusted Linux Profile' : 'Create Device Profile'}
              </h2>
              <p className="text-sm text-muted-foreground">
                {os === 'linux'
                  ? 'Define verification and posture requirements for Linux devices.'
                  : 'Choose the operating system this profile applies to.'}
              </p>
            </div>
            <button
              onClick={() => handleClose(false)}
              className="flex h-9 w-9 items-center justify-center rounded-lg border border-border text-muted-foreground transition hover:bg-secondary hover:text-foreground"
            >
              <X className="h-4 w-4" />
            </button>
          </div>

          <div className="flex-1 overflow-y-auto p-5">
            {!os ? (
              <div className="space-y-2">
                <Label className="text-sm font-semibold">Operating System</Label>
                <button
                  type="button"
                  onClick={() => setOs('linux')}
                  className="flex w-full items-center gap-3 rounded-2xl border border-border p-4 text-left transition hover:border-primary/40 hover:bg-secondary"
                >
                  <Laptop className="h-5 w-5 text-primary" />
                  <span className="flex-1 text-sm font-semibold">Linux</span>
                </button>
                <button
                  type="button"
                  disabled
                  title="Windows profiles are coming soon"
                  className="flex w-full cursor-not-allowed items-center gap-3 rounded-2xl border border-border p-4 text-left opacity-50"
                >
                  <MonitorSmartphone className="h-5 w-5 text-muted-foreground" />
                  <span className="flex-1 text-sm font-semibold text-muted-foreground">Windows</span>
                  <span className="rounded bg-secondary px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                    Soon
                  </span>
                </button>
                <button
                  type="button"
                  disabled
                  title="macOS profiles are coming soon"
                  className="flex w-full cursor-not-allowed items-center gap-3 rounded-2xl border border-border p-4 text-left opacity-50"
                >
                  <MonitorSmartphone className="h-5 w-5 text-muted-foreground" />
                  <span className="flex-1 text-sm font-semibold text-muted-foreground">macOS</span>
                  <span className="rounded bg-secondary px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                    Soon
                  </span>
                </button>
              </div>
            ) : (
              <form onSubmit={handleSubmit} className="space-y-6">
                <div className="space-y-2">
                  <Label className="text-sm font-semibold">Profile Name</Label>
                  <Input
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    placeholder="Profile Name"
                    className="h-11 font-medium"
                    autoFocus
                  />
                  <p className="text-xs text-muted-foreground">Required</p>
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

                  <div className="flex items-center justify-between rounded-2xl border border-border p-4">
                    <div className="flex items-center gap-2 text-sm font-semibold">
                      <ShieldCheck className="h-4 w-4 text-muted-foreground" />
                      Manual Trust
                    </div>
                    <Switch checked disabled aria-label="Manual Trust (always enabled)" />
                  </div>

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

                  <p className="text-xs text-muted-foreground">
                    Trusted Profiles must have at least one verification requirement.{' '}
                    <button
                      type="button"
                      onClick={comingSoon}
                      className="underline-offset-2 hover:underline"
                    >
                      Manage verification methods
                    </button>
                  </p>
                </div>

                <div className="space-y-2">
                  <div className="flex items-center justify-between">
                    <Label className="text-sm font-semibold">Device Posture Checks</Label>
                    <button
                      type="button"
                      onClick={comingSoon}
                      className="text-xs font-semibold text-primary underline-offset-2 hover:underline"
                    >
                      Learn More
                    </button>
                  </div>

                  {linuxChecks.length === 0 ? (
                    <p className="text-sm text-muted-foreground">Loading posture checks…</p>
                  ) : (
                    linuxChecks.map((check) => (
                      <div
                        key={check.id}
                        className="flex items-center justify-between rounded-2xl border border-border p-4"
                      >
                        <span className="text-sm font-semibold">{check.label}</span>
                        <Switch
                          checked={checkedIds.has(check.id)}
                          onCheckedChange={() => toggleCheck(check.id)}
                          aria-label={check.label}
                        />
                      </div>
                    ))
                  )}
                </div>

                {error && (
                  <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">
                    <AlertTriangle className="h-4 w-4 mt-0.5 shrink-0" />
                    <span>{error}</span>
                  </div>
                )}
              </form>
            )}
          </div>

          {os && (
            <div className="flex items-center justify-between gap-3 border-t border-border p-5">
              <Button
                type="button"
                variant="outline"
                onClick={() => handleClose(false)}
                className="h-11 flex-1"
              >
                Cancel
              </Button>
              <Button
                onClick={handleSubmit}
                disabled={!isValid || submitting}
                className="h-11 flex-1 gap-2"
              >
                {submitting ? (
                  <span className="flex items-center gap-2">
                    <Loader2 className="h-4 w-4 animate-spin" />
                    Creating...
                  </span>
                ) : (
                  'Create Trusted Profile'
                )}
              </Button>
            </div>
          )}
        </div>
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
