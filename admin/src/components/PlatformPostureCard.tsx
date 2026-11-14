import { Check, X } from "lucide-react";
import { Switch } from "@/components/ui/switch";
import { isMinOSCheck, type PROFILE_PLATFORMS } from "@/lib/profilePlatforms";

interface PlatformPostureCardProps {
  platform: (typeof PROFILE_PLATFORMS)[number];
  /** Devices of this platform are allowed by the profile. */
  enabled: boolean;
  onEnabledChange: (enabled: boolean) => void;
  checks: { id: string; label: string }[];
  required: Set<string>;
  onToggleRequired: (checkId: string) => void;
  /** Check ids that can't be turned off right now (last requirement of an in-use profile). */
  locked?: Set<string>;
  /** ADR-031 D-11: the minimum of each required Minimum OS Version check. */
  minVersions?: Record<string, string>;
  onMinVersionChange?: (checkId: string, version: string) => void;
}

/**
 * One platform in a device profile (ADR-031), laid out like an "Approved
 * Operating Systems" card: a switch that allows or denies the platform, and
 * each of its posture checks marked required or not. A required Minimum OS
 * Version check takes the minimum version inline.
 */
export function PlatformPostureCard({
  platform,
  enabled,
  onEnabledChange,
  checks,
  required,
  onToggleRequired,
  locked,
  minVersions,
  onMinVersionChange,
}: PlatformPostureCardProps) {
  const { Icon, label, comingSoon } = platform;
  return (
    // A group per card: several platforms share labels such as "Screen Lock".
    <div
      role="group"
      aria-label={`${label} posture`}
      className={`flex flex-col rounded-2xl border border-border ${comingSoon ? "opacity-60" : ""}`}
    >
      <div className="flex items-center justify-between gap-3 border-b border-border px-4 py-3.5">
        <div className="flex items-center gap-2.5 text-sm font-semibold">
          <Icon className="h-5 w-5" />
          {label}
        </div>
        {comingSoon ? (
          <span className="rounded bg-secondary px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
            Coming soon
          </span>
        ) : (
          <Switch
            checked={enabled}
            onCheckedChange={onEnabledChange}
            aria-label={`Allow ${label} devices`}
          />
        )}
      </div>
      <div className="flex-1 space-y-0.5 p-3">
        {comingSoon ? (
          <p className="px-1 py-1 text-xs text-muted-foreground">
            There is no Zecurity client for {label} yet.
          </p>
        ) : !enabled ? (
          <p className="px-1 py-1 text-xs text-muted-foreground">
            {label} devices are not allowed by this profile.
          </p>
        ) : checks.length === 0 ? (
          <p className="px-1 py-1 text-xs text-muted-foreground">
            No posture checks for {label} yet: any {label} device is allowed.
          </p>
        ) : (
          checks.map((check) => {
            const isRequired = required.has(check.id);
            const isLocked = isRequired && (locked?.has(check.id) ?? false);
            const takesVersion = isRequired && isMinOSCheck(check.id);
            return (
              <div key={check.id}>
                <button
                  type="button"
                  onClick={() => onToggleRequired(check.id)}
                  disabled={isLocked}
                  aria-pressed={isRequired}
                  aria-label={`${check.label} ${isRequired ? "required" : "not required"}`}
                  className="flex w-full items-center gap-2.5 rounded-lg px-1 py-1.5 text-left text-sm transition hover:bg-secondary disabled:cursor-not-allowed disabled:opacity-60"
                >
                  {isRequired ? (
                    <Check className="h-4 w-4 shrink-0 text-primary" />
                  ) : (
                    <X className="h-4 w-4 shrink-0 text-muted-foreground" />
                  )}
                  <span>
                    {check.label}{" "}
                    <span className="font-semibold">{isRequired ? "required" : "not required"}</span>
                  </span>
                </button>
                {takesVersion && (
                  <label className="ml-7 mt-1 mb-1.5 flex items-center gap-2 text-xs text-muted-foreground">
                    At least
                    <input
                      value={minVersions?.[check.id] ?? ""}
                      onChange={(e) => onMinVersionChange?.(check.id, e.target.value)}
                      placeholder={label === "Windows" ? "10.0.22631" : label === "Android" ? "13" : "17.5"}
                      aria-label={`${label} minimum OS version`}
                      inputMode="decimal"
                      className="h-8 w-32 rounded-md border border-border bg-transparent px-2 text-sm text-foreground outline-none focus:border-primary"
                    />
                  </label>
                )}
              </div>
            );
          })
        )}
      </div>
    </div>
  );
}
