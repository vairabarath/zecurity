import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import {
  CreateResourcePolicyDocument,
  UpdateResourcePolicyDocument,
  SetResourcePolicyProfilesDocument,
  GetResourcePoliciesDocument,
  GetDeviceProfilesDocument,
  PreviewResourcePolicyDocument,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { AlertTriangle, Info, Loader2, Shield, X } from 'lucide-react'
import { toast } from 'sonner'
import { toastMutationError } from '@/lib/mutationErrors'

export interface ResourcePolicyModalProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  policy?: {
    id: string
    name: string
    description?: string | null
    isDefault: boolean
    profiles: { id: string; name: string }[]
    resources?: { id: string; name: string }[]
  } | null
  onSuccess?: () => void
}

export function ResourcePolicyModal({
  open,
  onOpenChange,
  policy,
  onSuccess,
}: ResourcePolicyModalProps) {
  const isEdit = !!policy
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [selectedProfileIds, setSelectedProfileIds] = useState<string[]>([])
  const [confirmReduction, setConfirmReduction] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  // Reset form when modal opens or policy changes
  useEffect(() => {
    if (open) {
      if (policy) {
        setName(policy.name)
        setDescription(policy.description ?? '')
        setSelectedProfileIds(policy.profiles.map((p) => p.id))
      } else {
        setName('')
        setDescription('')
        setSelectedProfileIds([])
      }
      setConfirmReduction(false)
      setError(null)
    }
  }, [open, policy])

  const { data: profilesData, loading: profilesLoading } = useQuery(GetDeviceProfilesDocument, {
    skip: !open,
    fetchPolicy: 'cache-and-network',
  })

  const availableProfiles = useMemo(
    () => profilesData?.deviceProfiles ?? [],
    [profilesData],
  )

  const resourceIds = useMemo(
    () => (policy?.resources ?? []).map((r) => r.id),
    [policy],
  )

  // Preview query for existing policy with resources
  const { data: previewData, loading: previewLoading } = useQuery(
    PreviewResourcePolicyDocument,
    {
      variables: {
        resourceIds,
        profileIds: selectedProfileIds,
      },
      skip: !open || !isEdit || resourceIds.length === 0,
      fetchPolicy: 'cache-and-network',
    },
  )

  const preview = previewData?.previewResourcePolicy
  const hasReduction = !!preview && preview.devices > 0 && preview.passing < preview.devices

  const [createPolicy] = useMutation(CreateResourcePolicyDocument, {
    onError: toastMutationError,
    refetchQueries: [{ query: GetResourcePoliciesDocument }],
  })

  const [updatePolicy] = useMutation(UpdateResourcePolicyDocument, {
    onError: toastMutationError,
    refetchQueries: [{ query: GetResourcePoliciesDocument }],
  })

  const [setProfiles] = useMutation(SetResourcePolicyProfilesDocument, {
    onError: toastMutationError,
    refetchQueries: [{ query: GetResourcePoliciesDocument }],
  })

  function toggleProfile(profileId: string) {
    setSelectedProfileIds((prev) => {
      const next = prev.includes(profileId)
        ? prev.filter((id) => id !== profileId)
        : [...prev, profileId]
      return next
    })
    setConfirmReduction(false)
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)

    const trimmedName = name.trim()
    if (!trimmedName && (!isEdit || !policy.isDefault)) {
      setError('Policy name is required')
      return
    }

    if (hasReduction && !confirmReduction) {
      setError('Please confirm that you accept the access reduction before saving.')
      return
    }

    setSubmitting(true)
    try {
      if (isEdit) {
        // If name or description changed
        const nameChanged = !policy.isDefault && trimmedName !== policy.name
        const descChanged = (description.trim() || null) !== (policy.description || null)
        if (nameChanged || descChanged) {
          await updatePolicy({
            variables: {
              id: policy.id,
              name: policy.isDefault ? undefined : trimmedName,
              description: description.trim() || null,
            },
          })
        }

        // Check if profiles changed
        const currentProfileIds = new Set(policy.profiles.map((p) => p.id))
        const changed =
          selectedProfileIds.length !== currentProfileIds.size ||
          selectedProfileIds.some((id) => !currentProfileIds.has(id))

        if (changed) {
          await setProfiles({
            variables: {
              id: policy.id,
              profileIds: selectedProfileIds,
            },
          })
        }

        toast.success('Resource policy updated')
      } else {
        const created = await createPolicy({
          variables: {
            name: trimmedName,
            description: description.trim() || null,
          },
        })

        const newId = created.data?.createResourcePolicy.id
        if (newId && selectedProfileIds.length > 0) {
          await setProfiles({
            variables: {
              id: newId,
              profileIds: selectedProfileIds,
            },
          })
        }

        toast.success('Resource policy created')
      }

      onSuccess?.()
      onOpenChange(false)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to save resource policy')
    } finally {
      setSubmitting(false)
    }
  }

  if (!open) return null

  return (
    <div className="fixed inset-0 z-50">
      <div
        className="absolute inset-0 bg-black/50 backdrop-blur-sm"
        onClick={() => onOpenChange(false)}
      />
      <div className="absolute right-0 top-0 h-full w-full max-w-lg app-panel animate-slide-in">
        <form onSubmit={handleSubmit} className="flex h-full flex-col">
          <div className="flex items-center gap-4 border-b border-border p-5">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-[oklch(0.78_0.09_310/0.16)] text-[oklch(0.78_0.09_310)]">
              <Shield className="h-5 w-5" />
            </div>
            <div className="flex-1">
              <h2 className="text-lg font-semibold">
                {isEdit ? `Edit Policy: ${policy.name}` : 'Create Resource Policy'}
              </h2>
              <p className="text-sm text-muted-foreground">
                Define required device profiles for assigned resources.
              </p>
            </div>
            <button
              type="button"
              onClick={() => onOpenChange(false)}
              className="flex h-9 w-9 items-center justify-center rounded-lg border border-border text-muted-foreground transition hover:bg-secondary hover:text-foreground"
            >
              <X className="h-4 w-4" />
            </button>
          </div>

          <div className="flex-1 space-y-6 overflow-y-auto p-5">
            {error && (
              <div className="flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">
                <AlertTriangle className="h-4 w-4 shrink-0" />
                <span>{error}</span>
              </div>
            )}

            <div className="space-y-2">
              <Label htmlFor="policy-name" className="text-sm font-semibold">
                Policy Name
              </Label>
              {isEdit && policy.isDefault ? (
                <>
                  <Input
                    id="policy-name"
                    value={name}
                    disabled
                    className="cursor-not-allowed bg-secondary/50 font-medium text-muted-foreground"
                  />
                  <p className="text-xs text-muted-foreground">
                    The Default policy cannot be renamed.
                  </p>
                </>
              ) : (
                <Input
                  id="policy-name"
                  placeholder="e.g. Production Services"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  maxLength={100}
                  required
                />
              )}
            </div>

            <div className="space-y-2">
              <Label htmlFor="policy-description" className="text-sm font-semibold">
                Description <span className="font-normal text-muted-foreground">(optional)</span>
              </Label>
              <Input
                id="policy-description"
                placeholder="What resources or posture requirements this policy covers"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
              />
            </div>

            <div className="space-y-3">
              <div className="flex items-center justify-between">
                <div>
                  <Label className="text-sm font-semibold">Device Profiles</Label>
                  <p className="text-xs text-muted-foreground">
                    Selected profiles are OR'd together. A device satisfying ANY selected profile is allowed.
                  </p>
                </div>
              </div>

              {profilesLoading && availableProfiles.length === 0 ? (
                <div className="flex items-center gap-2 py-4 text-sm text-muted-foreground">
                  <Loader2 className="h-4 w-4 animate-spin" />
                  Loading profiles…
                </div>
              ) : availableProfiles.length === 0 ? (
                <div className="rounded-lg border border-border bg-secondary/30 p-4 text-sm text-muted-foreground">
                  No device profiles defined. This policy will allow any device until profiles are added.
                </div>
              ) : (
                <div className="space-y-2">
                  {availableProfiles.map((prof) => {
                    const isEmpty = prof.requirements.length === 0
                    const isChecked = selectedProfileIds.includes(prof.id)

                    return (
                      <div
                        key={prof.id}
                        className={`flex items-start justify-between rounded-lg border p-3 transition ${
                          isEmpty
                            ? 'border-border/50 bg-secondary/20 opacity-60 cursor-not-allowed'
                            : isChecked
                            ? 'border-primary/40 bg-primary/5'
                            : 'border-border bg-card hover:bg-secondary/20'
                        }`}
                      >
                        <label
                          htmlFor={`profile-${prof.id}`}
                          className={`flex flex-1 items-start gap-3 ${
                            isEmpty ? 'cursor-not-allowed' : 'cursor-pointer'
                          }`}
                        >
                          <input
                            type="checkbox"
                            id={`profile-${prof.id}`}
                            checked={isChecked}
                            disabled={isEmpty}
                            onChange={() => toggleProfile(prof.id)}
                            className="mt-1 h-4 w-4 rounded border-border text-primary focus:ring-primary"
                          />
                          <div>
                            <div className="text-sm font-medium leading-tight text-foreground">
                              {prof.name}
                            </div>
                            <div className="mt-1 text-xs text-muted-foreground">
                              {isEmpty ? (
                                <span className="text-amber-500 font-medium">
                                  Empty profile — cannot be added (evaluates unsatisfied and would block all devices)
                                </span>
                              ) : (
                                `${prof.requirements.length} ${
                                  prof.requirements.length === 1 ? 'requirement' : 'requirements'
                                }`
                              )}
                            </div>
                          </div>
                        </label>
                      </div>
                    )
                  })}
                </div>
              )}

              {selectedProfileIds.length === 0 && (
                <div className="flex items-center gap-2 rounded-lg border border-border bg-secondary/40 p-3 text-xs text-muted-foreground">
                  <Info className="h-4 w-4 shrink-0 text-muted-foreground" />
                  <span>
                    <strong>Any Device:</strong> With no profiles selected, any device can access resources under this policy.
                  </span>
                </div>
              )}
            </div>

            {/* Preview Section (P-5) */}
            {isEdit && resourceIds.length > 0 && (
              <div className="space-y-3 rounded-xl border border-border bg-secondary/20 p-4">
                <div className="flex items-center justify-between">
                  <div className="text-xs font-bold uppercase tracking-wider text-muted-foreground">
                    Enforcement Preview
                  </div>
                  {previewLoading && <Loader2 className="h-3.5 w-3.5 animate-spin text-muted-foreground" />}
                </div>

                {preview ? (
                  <div className="space-y-2">
                    <p className="text-sm font-medium">
                      <span className="font-bold text-foreground">{preview.passing}</span> of{' '}
                      <span className="font-bold text-foreground">{preview.devices}</span>{' '}
                      devices that can reach these resources today would pass
                    </p>

                    {hasReduction && (
                      <div className="space-y-3 pt-2">
                        <div className="flex items-start gap-2.5 rounded-lg border border-amber-500/30 bg-amber-500/10 p-3 text-xs text-amber-500">
                          <AlertTriangle className="h-4 w-4 shrink-0 mt-0.5" />
                          <div>
                            <span className="font-semibold">Access will be reduced:</span>{' '}
                            {preview.devices - preview.passing}{' '}
                            {preview.devices - preview.passing === 1 ? 'device' : 'devices'} will lose access immediately.
                          </div>
                        </div>

                        <label className="flex items-start gap-2.5 cursor-pointer text-xs font-medium text-foreground">
                          <input
                            type="checkbox"
                            checked={confirmReduction}
                            onChange={(e) => setConfirmReduction(e.target.checked)}
                            className="mt-0.5 h-4 w-4 rounded border-border text-primary focus:ring-primary"
                          />
                          <span>
                            I understand that {preview.devices - preview.passing}{' '}
                            {preview.devices - preview.passing === 1 ? 'device' : 'devices'} will lose access immediately.
                          </span>
                        </label>
                      </div>
                    )}
                  </div>
                ) : (
                  <p className="text-xs text-muted-foreground">
                    Calculating device evaluations…
                  </p>
                )}
              </div>
            )}
          </div>

          <div className="flex items-center justify-end gap-3 border-t border-border p-5">
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={submitting}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={submitting || (hasReduction && !confirmReduction)}
              className="gap-2"
            >
              {submitting ? (
                <>
                  <Loader2 className="h-4 w-4 animate-spin" />
                  Saving…
                </>
              ) : isEdit ? (
                'Save Policy'
              ) : (
                'Create Policy'
              )}
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
