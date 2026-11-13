import { Check, X } from "lucide-react";
import { Switch } from "@/components/ui/switch";
import type { PROFILE_PLATFORMS } from "@/lib/profilePlatforms";

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
}

/**
 * One platform in a device profile (ADR-031): a switch that allows or denies
 * the platform, and each of its posture checks marked required or not.
 */
export function PlatformPostureCard({
  platform,
  enabled,
  onEnabledChange,
  checks,
  required,
  onToggleRequired,
  locked,
}: PlatformPostureCardProps) {
  const { Icon, label } = platform;
  return (
    <div className="rounded-2xl border border-border">
      <div className="flex items-center justify-between p-4">
        <div className="flex items-center gap-2 text-sm font-semibold">
          <Icon className="h-4 w-4 text-muted-foreground" />
          {label}
        </div>
        <Switch
          checked={enabled}
          onCheckedChange={onEnabledChange}
          aria-label={`Allow ${label} devices`}
        />
      </div>
      <div className="space-y-1 border-t border-border p-3">
        {!enabled ? (
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
            return (
              <button
                key={check.id}
                type="button"
                onClick={() => onToggleRequired(check.id)}
                disabled={isLocked}
                aria-pressed={isRequired}
                aria-label={`${check.label} ${isRequired ? "required" : "not required"}`}
                className="flex w-full items-center gap-2.5 rounded-lg px-1 py-1.5 text-left text-sm transition hover:bg-secondary disabled:cursor-not-allowed disabled:opacity-60"
              >
                {isRequired ? (
                  <Check className="h-4 w-4 text-primary" />
                ) : (
                  <X className="h-4 w-4 text-muted-foreground" />
                )}
                <span>
                  {check.label} <span className="font-semibold">{isRequired ? "required" : "not required"}</span>
                </span>
              </button>
            );
          })
        )}
      </div>
    </div>
  );
}
