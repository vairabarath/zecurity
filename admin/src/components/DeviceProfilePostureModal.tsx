import { useQuery } from "@apollo/client/react";
import { AlertTriangle, ShieldCheck, ShieldX, X } from "lucide-react";
import { Skeleton } from "@/components/ui/skeleton";
import { GetDeviceProfilePostureDocument } from "@/generated/graphql";
import { relativeTime } from "@/lib/console";

interface DeviceProfilePostureModalProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  profile: { id: string; name: string } | null;
}

/**
 * Posture satisfaction per device for one profile.
 *
 * Deliberately reachable for any profile, whether or not a Resource Policy
 * references it: a Device Profile is a trust definition and is observable on its
 * own. Seeing that devices fail a profile is useful before anything depends on
 * it — which is the point of being able to look without binding.
 */
export function DeviceProfilePostureModal({
  open,
  onOpenChange,
  profile,
}: DeviceProfilePostureModalProps) {
  const { data, loading, error } = useQuery(GetDeviceProfilePostureDocument, {
    variables: { profileId: profile?.id ?? "" },
    skip: !open || !profile,
    fetchPolicy: "cache-and-network",
  });

  if (!open || !profile) return null;

  const rows = data?.devicePostureVisibility ?? [];

  return (
    <div className="fixed inset-0 z-50">
      <div
        className="absolute inset-0 bg-black/50 backdrop-blur-sm"
        onClick={() => onOpenChange(false)}
      />
      <div className="absolute right-0 top-0 h-full w-full max-w-md app-panel animate-slide-in">
        <div className="flex h-full flex-col">
          <div className="flex items-center gap-4 border-b border-border p-5">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-[oklch(0.78_0.10_235/0.16)] text-[oklch(0.78_0.10_235)]">
              <ShieldCheck className="h-5 w-5" />
            </div>
            <div className="flex-1 min-w-0">
              <h2 className="truncate text-lg font-semibold">{profile.name}</h2>
              <p className="text-sm text-muted-foreground">
                Device posture — visibility only, independent of any policy.
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

          <div className="flex-1 space-y-3 overflow-y-auto p-5">
            {loading && !data ? (
              Array.from({ length: 4 }).map((_, index) => (
                <Skeleton key={index} className="h-16 rounded-2xl bg-secondary" />
              ))
            ) : error && rows.length === 0 ? (
              <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">
                <AlertTriangle className="h-4 w-4 mt-0.5 shrink-0" />
                <span>Couldn't load posture for this profile.</span>
              </div>
            ) : rows.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No devices have reported posture for this profile yet.
              </p>
            ) : (
              rows.map((row) => (
                <div
                  key={row.deviceId}
                  className="rounded-2xl border border-border p-4"
                >
                  <div className="flex items-center justify-between gap-3">
                    <span className="truncate text-sm font-semibold">
                      {row.deviceName}
                    </span>
                    {row.satisfied ? (
                      <span className="inline-flex shrink-0 items-center gap-1.5 text-[13px] font-bold text-secure">
                        <ShieldCheck className="h-3.5 w-3.5" />
                        Satisfied
                      </span>
                    ) : (
                      <span className="inline-flex shrink-0 items-center gap-1.5 text-[13px] font-bold text-[oklch(0.75_0.16_25)]">
                        <ShieldX className="h-3.5 w-3.5" />
                        Not satisfied
                      </span>
                    )}
                  </div>

                  {/*
                    A stale result is not the same as a failing one: the device
                    has not reported recently enough, so the evaluation no longer
                    counts. Saying so avoids it being read as non-compliance.
                  */}
                  {row.stale && (
                    <div className="mt-1 text-xs font-semibold text-warning">
                      Stale — awaiting a fresh posture report
                    </div>
                  )}

                  {row.failureReason && (
                    <div className="mt-1 text-xs text-muted-foreground">
                      {row.failureReason}
                    </div>
                  )}

                  <div className="mt-1 text-xs text-muted-foreground opacity-70">
                    evaluated {relativeTime(row.evaluatedAt)}
                  </div>
                </div>
              ))
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
