import { useMemo, useState } from "react";
import { Skeleton } from "@/components/ui/skeleton";
import { useMutation, useQuery } from "@apollo/client/react";
import { Plus, ShieldCheck } from "lucide-react";
import {
  DeleteDeviceProfileDocument,
  GetDeviceProfilesDocument,
} from "@/generated/graphql";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { CreateDeviceProfileModal } from "@/components/CreateDeviceProfileModal";
import { EditDeviceProfileModal } from "@/components/EditDeviceProfileModal";

import { EmptyState, ErrorState, EntityIcon } from "@/lib/console";

type EditableProfile = {
  id: string;
  name: string;
  manualTrust: boolean;
  requirements: { checkId: string }[];
};

function DeleteDeviceProfileDialog({
  profile,
  onClose,
  onConfirm,
  loading,
}: {
  profile: { id: string; name: string } | null;
  onClose: () => void;
  onConfirm: () => void;
  loading: boolean;
}) {
  return (
    <Dialog open={!!profile} onOpenChange={(open) => { if (!open) onClose(); }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete device profile</DialogTitle>
        </DialogHeader>
        <p className="text-sm text-muted-foreground">
          Are you sure you want to delete{" "}
          <span className="font-semibold text-foreground">{profile?.name}</span>?
          Devices bound through this profile will no longer be gated by it.
        </p>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={loading}>
            Cancel
          </Button>
          <Button variant="destructive" onClick={onConfirm} disabled={loading}>
            {loading ? "Deleting…" : "Delete"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export default function DeviceProfiles() {
  const [showAdd, setShowAdd] = useState(false);
  const [editingProfile, setEditingProfile] = useState<EditableProfile | null>(null);
  const [deletingProfile, setDeletingProfile] = useState<{ id: string; name: string } | null>(null);

  const { data, loading, error, refetch } = useQuery(
    GetDeviceProfilesDocument,
    {
      fetchPolicy: "cache-and-network",
      pollInterval: 30000,
    },
  );

  const [deleteDeviceProfile, { loading: deleting }] = useMutation(
    DeleteDeviceProfileDocument,
    {
      onCompleted: () => {
        setDeletingProfile(null);
        refetch();
      },
      refetchQueries: [{ query: GetDeviceProfilesDocument }],
    },
  );

  const deviceProfiles = useMemo(() => data?.deviceProfiles ?? [], [data]);

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Device Profiles</h2>
          <p className="page-subtitle">Manage device posture profiles.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
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
          <div className="table-head grid min-w-300 items-center grid-cols-[1.5fr_160px_160px_120px] gap-4 px-5 py-4">
            {["Name", "Requirements", "Bound Resources", "Actions"].map(
              (label, index) => (
                <div
                  key={label + index}
                  className={`table-head-label ${index === 3 ? "text-right" : ""}`}
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
                  className="admin-table-row group grid items-center grid-cols-[1.5fr_160px_160px_120px] gap-4 px-5 py-4"
                >
                  <div className="flex min-w-0 items-center gap-3">
                    <EntityIcon type="resource" />
                    <div className="min-w-0">
                      <div className="truncate text-[15px] font-bold leading-tight">
                        {profile.name}
                      </div>
                    </div>
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

                  <div className="flex items-center justify-end gap-3">
                    <button
                      onClick={() =>
                        setEditingProfile({
                          id: profile.id,
                          name: profile.name,
                          manualTrust: profile.manualTrust,
                          requirements: profile.requirements,
                        })
                      }
                      className="text-[13px] font-bold text-muted-foreground transition hover:text-foreground"
                    >
                      Edit
                    </button>
                    <button
                      onClick={() => setDeletingProfile({ id: profile.id, name: profile.name })}
                      className="text-[13px] font-bold text-[oklch(0.75_0.16_25)] transition hover:opacity-80"
                    >
                      Delete
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

      <EditDeviceProfileModal
        open={editingProfile !== null}
        onOpenChange={(open) => {
          if (!open) {
            setEditingProfile(null);
          }
        }}
        profile={editingProfile}
        onSuccess={() => {
          setEditingProfile(null);
          refetch();
        }}
      />

      <DeleteDeviceProfileDialog
        profile={deletingProfile}
        loading={deleting}
        onClose={() => setDeletingProfile(null)}
        onConfirm={() => {
          if (deletingProfile) {
            deleteDeviceProfile({ variables: { id: deletingProfile.id } });
          }
        }}
      />
    </div>
  );
}
