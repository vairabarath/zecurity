import { useEffect, useState } from "react";
import { useMutation, useQuery } from "@apollo/client/react";
import {
  AddProfileRequirementDocument,
  RemoveProfileRequirementDocument,
  GetDeviceProfilesDocument,
  GetSupportedPostureChecksDocument,
} from "@/generated/graphql";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { AlertTriangle, Loader2, Plug, ShieldCheck, X } from "lucide-react";
import { toast } from "sonner";

interface EditDeviceProfileModalProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  profile: {
    id: string;
    name: string;
    manualTrust: boolean;
    requirements: { checkId: string }[];
  } | null;
  onSuccess?: () => void;
}

export function EditDeviceProfileModal({
  open,
  onOpenChange,
  profile,
  onSuccess,
}: EditDeviceProfileModalProps) {
  const [checkedIds, setCheckedIds] = useState<Set<string>>(new Set());
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (profile) {
      setCheckedIds(new Set(profile.requirements.map((r) => r.checkId)));
      setError(null);
    }
  }, [profile]);

  const { data: checksData } = useQuery(GetSupportedPostureChecksDocument, {
    skip: !open,
  });
  const linuxChecks = (checksData?.supportedPostureChecks ?? []).filter(
    (check) => check.platform === "linux",
  );

  const [addProfileRequirement] = useMutation(AddProfileRequirementDocument);
  const [removeProfileRequirement] = useMutation(RemoveProfileRequirementDocument, {
    refetchQueries: [{ query: GetDeviceProfilesDocument }],
  });

  function toggleCheck(checkId: string) {
    setCheckedIds((prev) => {
      const next = new Set(prev);
      if (next.has(checkId)) next.delete(checkId);
      else next.add(checkId);
      return next;
    });
  }

  function comingSoon() {
    toast("Trust method integrations are coming soon.");
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    if (!profile) return;

    const originalIds = new Set(profile.requirements.map((r) => r.checkId));
    const toAdd = [...checkedIds].filter((id) => !originalIds.has(id));
    const toRemove = [...originalIds].filter((id) => !checkedIds.has(id));

    setSaving(true);
    try {
      for (const checkId of toAdd) {
        await addProfileRequirement({
          variables: { profileId: profile.id, checkId, allowUnsupported: false },
        });
      }
      for (const checkId of toRemove) {
        await removeProfileRequirement({
          variables: { profileId: profile.id, checkId },
        });
      }

      toast.success("Device profile updated");
      onSuccess?.();
      onOpenChange(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to update device profile");
    } finally {
      setSaving(false);
    }
  }

  if (!open || !profile) return null;

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
              <ShieldCheck className="h-5 w-5" />
            </div>
            <div className="flex-1">
              <h2 className="text-lg font-semibold">Edit Trusted Linux Profile</h2>
              <p className="text-sm text-muted-foreground">
                Update verification and posture requirements for Linux devices.
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
            <div className="space-y-2">
              <Label className="text-sm font-semibold">Profile Name</Label>
              <div className="rounded-lg border border-border bg-secondary/40 px-3 py-2 text-sm text-muted-foreground">
                {profile.name}
              </div>
              <p className="text-xs text-muted-foreground">
                Profile name is set at creation and can't be changed.
              </p>
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
                Trusted Profiles must have at least one verification requirement.{" "}
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
            <Button type="submit" disabled={saving}>
              {saving && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
              Save
            </Button>
          </div>
        </form>
      </div>
    </div>
  );
}
