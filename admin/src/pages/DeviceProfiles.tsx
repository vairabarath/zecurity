import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@apollo/client/react";
import { Plus, ShieldCheck } from "lucide-react";
import { DeviceProfileMode, GetDeviceProfilesDocument } from "@/generated/graphql";
import { Button } from "@/components/ui/button";
import { CreateDeviceProfileModal } from "@/components/CreateDeviceProfileModal";
import { Skeleton } from "@/components/ui/skeleton";
import {
  EmptyState,
  ErrorState,
  EntityIcon,
  StatusPill,
} from "@/lib/console";

function modeTone(mode: DeviceProfileMode): "ok" | "muted" {
  return mode === DeviceProfileMode.Enforce ? "ok" : "muted";
}

export default function DeviceProfiles() {
  const navigate = useNavigate();
  const [showAdd, setShowAdd] = useState(false);

  const { data, loading, error, refetch } = useQuery(GetDeviceProfilesDocument, {
    fetchPolicy: "cache-and-network",
    pollInterval: 30000,
  });

  const deviceProfiles = useMemo(() => data?.deviceProfiles ?? [], [data]);
  const enforceCount = useMemo(
    () => deviceProfiles.filter((p) => p.mode === DeviceProfileMode.Enforce).length,
    [deviceProfiles],
  );

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Device Profiles</h2>
          <p className="page-subtitle">
            Manage device posture profiles.
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="status-pill border-[oklch(0.82_0.12_160/0.28)] bg-[oklch(0.82_0.12_160/0.12)] text-secure">
            <span className="status-pill-dot bg-secure" />
            <span className="font-bold">{enforceCount}</span> enforcing
          </span>
          <span className="status-pill border-border bg-secondary text-muted-foreground">
            <span className="font-bold text-foreground">
              {deviceProfiles.length}
            </span>{" "}
            total
          </span>
          <Button onClick={() => setShowAdd(true)} className="gap-2">
            <Plus className="h-4 w-4" />
            Create Device Profile
          </Button>
        </div>
      </div>

      <div className="table-shell">
        <div className="table-scroll">
          <div className="table-head grid min-w-300 items-center grid-cols-[1.5fr_140px_160px_160px_120px] gap-4 px-5 py-4">
            {["Name", "Mode", "Requirements", "Bound Resources", "Actions"].map(
              (label, index) => (
                <div
                  key={label + index}
                  className={`table-head-label ${index === 4 ? "text-right" : ""}`}
                >
                  {label}
                </div>
              ),
            )}
          </div>

          {loading && !data ? (
            <div className="min-w-300 p-5 space-y-3">
              {Array.from({ length: 5 }).map((_, index) => (
                <Skeleton
                  key={index}
                  className="h-14 rounded-2xl bg-secondary"
                />
              ))}
            </div>
          ) : error && deviceProfiles.length === 0 ? (
            <ErrorState
              title="Couldn't load device profiles"
              description="Something went wrong fetching your device profiles. This is a load error, not an empty workspace."
              action={<Button onClick={() => refetch()}>Retry</Button>}
            />
          ) : deviceProfiles.length === 0 ? (
            <EmptyState
              title="No device profiles defined"
              description="Create a device profile to start gating resource access on posture."
              action={
                <Button onClick={() => setShowAdd(true)}>
                  Create Device Profile
                </Button>
              }
            />
          ) : (
            <div className="min-w-300">
              {deviceProfiles.map((profile) => (
                <div
                  key={profile.id}
                  className="admin-table-row group grid items-center grid-cols-[1.5fr_140px_160px_160px_120px] gap-4 px-5 py-4"
                >
                  <div className="flex min-w-0 items-center gap-3">
                    <EntityIcon type="resource" />
                    <div className="min-w-0">
                      <div className="truncate text-[15px] font-bold leading-tight">
                        {profile.name}
                      </div>
                    </div>
                  </div>
                  <div>
                    <StatusPill label={profile.mode} tone={modeTone(profile.mode)} />
                  </div>
                  <div className="text-[13px] font-semibold text-muted-foreground">
                    {profile.requirements.length === 0 ? (
                      <span className="italic opacity-60">none</span>
                    ) : (
                      <span className="inline-flex items-center gap-1.5">
                        <ShieldCheck className="h-3.5 w-3.5" />
                        {profile.requirements.length}
                      </span>
                    )}
                  </div>
                  <div className="text-[13px] font-semibold text-muted-foreground">
                    {profile.boundResources.length === 0 ? (
                      <span className="italic opacity-60">none</span>
                    ) : (
                      profile.boundResources.length
                    )}
                  </div>
                  <div className="text-right">
                    <button
                      onClick={() => navigate(`/device-profiles/${profile.id}`)}
                      className="inline-flex items-center gap-1 text-[13px] font-bold text-primary transition hover:opacity-80"
                    >
                      Manage{" "}
                      <span className="transition-transform group-hover:translate-x-0.5">
                        →
                      </span>
                    </button>
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      <CreateDeviceProfileModal
        open={showAdd}
        onOpenChange={setShowAdd}
        onSuccess={() => refetch()}
      />
    </div>
  );
}
