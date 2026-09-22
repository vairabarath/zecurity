import { useEffect, useState } from "react";
import { useMutation, useQuery } from "@apollo/client/react";
import { AlertTriangle, Layers, Loader2, X } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  AddProfileToResourcePolicyDocument,
  GetDeviceProfilesDocument,
  RemoveProfileFromResourcePolicyDocument,
  UpdateResourcePolicyDocument,
} from "@/generated/graphql";

interface EditResourcePolicyModalProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  policy: {
    id: string;
    name: string;
    deviceProfiles: { id: string; name: string }[];
  } | null;
  onSuccess?: () => void;
}

/**
 * Edits what the policy *is*: its name and which device profiles it requires.
 * Resource membership is managed separately, from the list page.
 *
 * Profile selection is diffed against the original set and applied with the
 * add/remove mutations, mirroring how EditDeviceProfileModal diffs requirements.
 */
export function EditResourcePolicyModal({
  open,
  onOpenChange,
  policy,
  onSuccess,
}: EditResourcePolicyModalProps) {
  const [name, setName] = useState("");
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const { data: profileData, loading: profilesLoading } = useQuery(
    GetDeviceProfilesDocument,
    { fetchPolicy: "cache-and-network", skip: !open },
  );

  const [updateResourcePolicy] = useMutation(UpdateResourcePolicyDocument);
  const [addProfileToResourcePolicy] = useMutation(
    AddProfileToResourcePolicyDocument,
  );
  const [removeProfileFromResourcePolicy] = useMutation(
    RemoveProfileFromResourcePolicyDocument,
  );

  useEffect(() => {
    if (policy) {
      setName(policy.name);
      setSelectedIds(new Set(policy.deviceProfiles.map((p) => p.id)));
      setError(null);
    }
  }, [policy]);

  function toggle(profileId: string) {
    setSelectedIds((current) => {
      const next = new Set(current);
      if (next.has(profileId)) {
        next.delete(profileId);
      } else {
        next.add(profileId);
      }
      return next;
    });
  }

  async function handleSubmit(event: React.FormEvent) {
    event.preventDefault();
    if (!policy) return;

    setError(null);
    setSaving(true);
    try {
      if (name.trim() !== policy.name) {
        await updateResourcePolicy({
          variables: { id: policy.id, name: name.trim() },
        });
      }

      const original = new Set(policy.deviceProfiles.map((p) => p.id));
      const toAdd = [...selectedIds].filter((id) => !original.has(id));
      const toRemove = [...original].filter((id) => !selectedIds.has(id));

      for (const profileId of toAdd) {
        await addProfileToResourcePolicy({
          variables: { policyId: policy.id, profileId },
        });
      }
      for (const profileId of toRemove) {
        await removeProfileFromResourcePolicy({
          variables: { policyId: policy.id, profileId },
        });
      }

      toast.success("Resource policy updated");
      onSuccess?.();
      onOpenChange(false);
    } catch (err) {
      setError(
        err instanceof Error ? err.message : "Failed to update resource policy",
      );
    } finally {
      setSaving(false);
    }
  }

  if (!open || !policy) return null;

  const profiles = profileData?.deviceProfiles ?? [];
  const isValid = name.trim().length > 0;

  return (
    <div className="fixed inset-0 z-50">
      <div
        className="absolute inset-0 bg-black/50 backdrop-blur-sm"
        onClick={() => onOpenChange(false)}
      />
      <div className="absolute right-0 top-0 h-full w-full max-w-md app-panel animate-slide-in">
        <form onSubmit={handleSubmit} className="flex h-full flex-col">
          <div className="flex items-center gap-4 border-b border-border p-5">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-[oklch(0.78_0.10_235/0.16)] text-[oklch(0.78_0.10_235)]">
              <Layers className="h-5 w-5" />
            </div>
            <div className="flex-1">
              <h2 className="text-lg font-semibold">Edit Resource Policy</h2>
              <p className="text-sm text-muted-foreground">
                What this policy requires of a device.
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

          <div className="flex-1 space-y-5 overflow-y-auto p-5">
            <div className="space-y-2">
              <Label className="text-sm font-semibold">
                Policy Name <span className="text-destructive">*</span>
              </Label>
              <Input value={name} onChange={(e) => setName(e.target.value)} />
            </div>

            <div className="space-y-2">
              <Label className="text-sm font-semibold">Device Profiles</Label>

              {/*
                Stating the two semantics inline. They are the parts of the model
                an admin is most likely to get backwards: an empty selection is a
                deliberate "Any Device", not a misconfiguration, and several
                profiles are alternatives rather than a combined requirement.
              */}
              <p className="text-xs text-muted-foreground">
                {selectedIds.size === 0
                  ? "No profiles selected — this policy allows Any Device."
                  : selectedIds.size === 1
                    ? "A device must satisfy this profile."
                    : `A device must satisfy any one of these ${selectedIds.size} profiles.`}
              </p>

              {profilesLoading && profiles.length === 0 ? (
                <p className="text-sm text-muted-foreground">Loading profiles…</p>
              ) : profiles.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  No device profiles exist yet. Create one from the Device
                  Profiles tab to require it here.
                </p>
              ) : (
                <div className="space-y-2">
                  {profiles.map((profile) => (
                    <label
                      key={profile.id}
                      className="flex cursor-pointer items-center justify-between rounded-2xl border border-border p-4"
                    >
                      <div className="min-w-0">
                        <div className="truncate text-sm font-semibold">
                          {profile.name}
                        </div>
                        <div className="text-xs text-muted-foreground">
                          {profile.requirements.length === 0
                            ? "no posture requirements"
                            : `${profile.requirements.length} posture requirement${profile.requirements.length === 1 ? "" : "s"}`}
                        </div>
                      </div>
                      <input
                        type="checkbox"
                        checked={selectedIds.has(profile.id)}
                        onChange={() => toggle(profile.id)}
                        className="h-4 w-4 shrink-0 accent-[oklch(0.78_0.10_235)]"
                      />
                    </label>
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
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={saving}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={!isValid || saving}>
              {saving && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
              Save
            </Button>
          </div>
        </form>
      </div>
    </div>
  );
}
