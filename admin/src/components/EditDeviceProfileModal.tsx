import { useEffect, useState } from "react";
import { useMutation, useQuery } from "@apollo/client/react";
import {
  AddProfileRequirementDocument,
  RemoveProfileRequirementDocument,
  GetDeviceProfilesDocument,
  GetSupportedPostureChecksDocument,
  UpdateProfileRequirementCountriesDocument,
} from "@/generated/graphql";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { AlertTriangle, Loader2, Plug, ShieldCheck, X } from "lucide-react";
import { toast } from "sonner";
import { CountryRuleEditor } from "@/components/CountryRuleEditor";
import { COUNTRY_CHECK_ID } from "@/lib/countries";
import {
  EMPTY_COUNTRY_RULE,
  countryRuleChanged,
  countryRuleProblem,
  type CountryRuleValue,
} from "@/lib/countryRule";

interface EditDeviceProfileModalProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  profile: {
    id: string;
    name: string;
    manualTrust: boolean;
    requirements: {
      checkId: string;
      countries?: { allow: string[]; deny: string[] } | null;
    }[];
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
  // PENDING-20: the country check is edited with its own lists.
  const [countryRule, setCountryRule] = useState<CountryRuleValue>(EMPTY_COUNTRY_RULE);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (profile) {
      setCheckedIds(
        new Set(profile.requirements.map((r) => r.checkId).filter((id) => id !== COUNTRY_CHECK_ID)),
      );
      setCountryRule(originalCountryRule(profile));
      setError(null);
    }
  }, [profile]);

  const { data: checksData } = useQuery(GetSupportedPostureChecksDocument, {
    skip: !open,
  });
  const linuxChecks = (checksData?.supportedPostureChecks ?? []).filter(
    (check) => check.platform === "linux",
  );
  // Server-side checks (ADR-030), e.g. "Approved by an admin". The country
  // check (PENDING-20) is server-side too but has its own editor.
  const serverChecks = (checksData?.supportedPostureChecks ?? []).filter(
    (check) => check.platform === "any" && check.id !== COUNTRY_CHECK_ID,
  );
  const countryCheckAvailable = (checksData?.supportedPostureChecks ?? []).some(
    (check) => check.id === COUNTRY_CHECK_ID,
  );
  const countryProblem = countryRuleProblem(countryRule);

  const [addProfileRequirement] = useMutation(AddProfileRequirementDocument);
  const [updateCountries] = useMutation(UpdateProfileRequirementCountriesDocument);
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

    if (countryProblem) {
      setError(countryProblem);
      return;
    }

    const originalIds = new Set(
      profile.requirements.map((r) => r.checkId).filter((id) => id !== COUNTRY_CHECK_ID),
    );
    const toAdd = [...checkedIds].filter((id) => !originalIds.has(id));
    const toRemove = [...originalIds].filter((id) => !checkedIds.has(id));
    const originalCountry = originalCountryRule(profile);
    const countries = { allow: countryRule.allow, deny: countryRule.deny };

    setSaving(true);
    try {
      // Adds first and removes last, so an enforce profile never passes
      // through "no requirements" (the server refuses that).
      for (const checkId of toAdd) {
        await addProfileRequirement({
          variables: { profileId: profile.id, checkId, allowUnsupported: false },
        });
      }
      if (countryRule.enabled && !originalCountry.enabled) {
        await addProfileRequirement({
          variables: { profileId: profile.id, checkId: COUNTRY_CHECK_ID, allowUnsupported: false, countries },
        });
      } else if (countryRule.enabled && countryRuleChanged(countryRule, originalCountry)) {
        // G-10: replace the lists in place — no window without a country check.
        await updateCountries({ variables: { profileId: profile.id, countries } });
      }
      for (const checkId of toRemove) {
        await removeProfileRequirement({
          variables: { profileId: profile.id, checkId },
        });
      }
      if (!countryRule.enabled && originalCountry.enabled) {
        await removeProfileRequirement({
          variables: { profileId: profile.id, checkId: COUNTRY_CHECK_ID },
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
            <Button type="submit" disabled={saving || countryProblem !== null}>
              {saving && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
              Save
            </Button>
          </div>
        </form>
      </div>
    </div>
  );
}

type EditableProfile = NonNullable<EditDeviceProfileModalProps["profile"]>;

function originalCountryRule(profile: EditableProfile): CountryRuleValue {
  const req = profile.requirements.find((r) => r.checkId === COUNTRY_CHECK_ID);
  if (!req) return EMPTY_COUNTRY_RULE;
  return {
    enabled: true,
    allow: [...(req.countries?.allow ?? [])],
    deny: [...(req.countries?.deny ?? [])],
  };
}
