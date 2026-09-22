import { useMemo, useState } from "react";
import { useMutation, useQuery } from "@apollo/client/react";
import { Plus, Layers, Server } from "lucide-react";
import { Skeleton } from "@/components/ui/skeleton";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  DeleteResourcePolicyDocument,
  GetResourcePoliciesDocument,
  GetAllResourcesDocument,
  AssignResourcePolicyDocument,
  UnassignResourcePolicyDocument,
} from "@/generated/graphql";
import { CreateResourcePolicyModal } from "@/components/CreateResourcePolicyModal";
import { EditResourcePolicyModal } from "@/components/EditResourcePolicyModal";
import { EmptyState, ErrorState, EntityIcon } from "@/lib/console";

export type EditablePolicy = {
  id: string;
  name: string;
  deviceProfiles: { id: string; name: string }[];
};

type PolicyRef = { id: string; name: string };

/**
 * Deleting a policy that a resource still uses is refused by the server, so the
 * copy says what the admin has to do first rather than implying it might work.
 */
function DeleteResourcePolicyDialog({
  policy,
  onClose,
  onConfirm,
  loading,
}: {
  policy: PolicyRef | null;
  onClose: () => void;
  onConfirm: () => void;
  loading: boolean;
}) {
  return (
    <Dialog open={!!policy} onOpenChange={(open) => { if (!open) onClose(); }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete resource policy</DialogTitle>
        </DialogHeader>
        <p className="text-sm text-muted-foreground">
          Are you sure you want to delete{" "}
          <span className="font-semibold text-foreground">{policy?.name}</span>?
          Any resource still using it must be unassigned first — the server will
          refuse the delete otherwise.
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

/**
 * Resource membership, kept separate from the policy's own definition.
 *
 * The candidate list is pre-filtered to resources that carry no policy at all,
 * which is how "a resource has exactly one policy" is expressed in the UI. The
 * server enforces it independently, so its refusal is surfaced rather than
 * assumed impossible.
 */
function ManageResourcesDialog({
  policy,
  assigned,
  unassigned,
  onClose,
  onAssign,
  onUnassign,
  busy,
  error,
}: {
  policy: PolicyRef | null;
  assigned: { id: string; name: string }[];
  unassigned: { id: string; name: string }[];
  onClose: () => void;
  onAssign: (resourceId: string) => void;
  onUnassign: (resourceId: string) => void;
  busy: boolean;
  error: string | null;
}) {
  const [selected, setSelected] = useState("");

  return (
    <Dialog
      open={!!policy}
      onOpenChange={(open) => {
        if (!open) {
          setSelected("");
          onClose();
        }
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Resources using {policy?.name}</DialogTitle>
        </DialogHeader>

        <div className="space-y-4 pt-1">
          <div className="space-y-2">
            {assigned.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No resources use this policy yet.
              </p>
            ) : (
              assigned.map((resource) => (
                <div
                  key={resource.id}
                  className="flex items-center justify-between rounded-2xl border border-border p-3"
                >
                  <span className="truncate text-sm font-semibold">
                    {resource.name}
                  </span>
                  <button
                    onClick={() => onUnassign(resource.id)}
                    disabled={busy}
                    className="text-[13px] font-bold text-[oklch(0.75_0.16_25)] transition hover:opacity-80 disabled:opacity-50"
                  >
                    Remove
                  </button>
                </div>
              ))
            )}
          </div>

          <div className="space-y-1.5">
            <label
              htmlFor="resource-policy-resource-select"
              className="text-sm font-semibold"
            >
              Assign a resource
            </label>
            <select
              id="resource-policy-resource-select"
              value={selected}
              onChange={(e) => setSelected(e.target.value)}
              disabled={busy || unassigned.length === 0}
              className="w-full rounded-xl border border-border bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-primary/30 disabled:cursor-not-allowed disabled:opacity-50"
            >
              <option value="">
                {unassigned.length === 0
                  ? "Every resource already has a policy"
                  : "Select a resource…"}
              </option>
              {unassigned.map((resource) => (
                <option key={resource.id} value={resource.id}>
                  {resource.name}
                </option>
              ))}
            </select>
            <p className="text-xs text-muted-foreground">
              Only resources without a policy are listed — a resource can have
              exactly one.
            </p>
          </div>

          {error && (
            <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">
              <span>{error}</span>
            </div>
          )}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Close
          </Button>
          <Button
            disabled={!selected || busy}
            onClick={() => {
              onAssign(selected);
              setSelected("");
            }}
          >
            {busy ? "Assigning…" : "Assign"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export default function ResourcePolicies() {
  const [showAdd, setShowAdd] = useState(false);
  const [editingPolicy, setEditingPolicy] = useState<EditablePolicy | null>(null);
  const [deletingPolicy, setDeletingPolicy] = useState<PolicyRef | null>(null);
  const [managingPolicy, setManagingPolicy] = useState<PolicyRef | null>(null);
  const [assignError, setAssignError] = useState<string | null>(null);

  const { data, loading, error, refetch } = useQuery(
    GetResourcePoliciesDocument,
    {
      fetchPolicy: "cache-and-network",
      pollInterval: 30000,
    },
  );

  // The full resource list is only needed to work out which resources are still
  // unassigned, for the assignment picker.
  const { data: resourceData } = useQuery(GetAllResourcesDocument, {
    fetchPolicy: "cache-and-network",
  });

  const [deleteResourcePolicy, { loading: deleting }] = useMutation(
    DeleteResourcePolicyDocument,
    {
      onCompleted: () => {
        setDeletingPolicy(null);
        refetch();
      },
      onError: () => setDeletingPolicy(null),
      refetchQueries: [{ query: GetResourcePoliciesDocument }],
    },
  );

  const [assignResourcePolicy, { loading: assigning }] = useMutation(
    AssignResourcePolicyDocument,
    {
      onCompleted: () => {
        setAssignError(null);
        refetch();
      },
      onError: (err) => setAssignError(err.message),
      refetchQueries: [
        { query: GetResourcePoliciesDocument },
        { query: GetAllResourcesDocument },
      ],
    },
  );

  const [unassignResourcePolicy, { loading: unassigning }] = useMutation(
    UnassignResourcePolicyDocument,
    {
      onCompleted: () => {
        setAssignError(null);
        refetch();
      },
      onError: (err) => setAssignError(err.message),
      refetchQueries: [
        { query: GetResourcePoliciesDocument },
        { query: GetAllResourcesDocument },
      ],
    },
  );

  const resourcePolicies = useMemo(() => data?.resourcePolicies ?? [], [data]);

  // A resource is assignable only if no policy already claims it.
  const unassignedResources = useMemo(() => {
    const claimed = new Set(
      resourcePolicies.flatMap((policy) => policy.resources.map((r) => r.id)),
    );
    return (resourceData?.allResources ?? [])
      .filter((resource) => !claimed.has(resource.id))
      .map((resource) => ({ id: resource.id, name: resource.name }));
  }, [resourcePolicies, resourceData]);

  const managedResources = useMemo(() => {
    if (!managingPolicy) return [];
    return (
      resourcePolicies.find((policy) => policy.id === managingPolicy.id)
        ?.resources ?? []
    );
  }, [managingPolicy, resourcePolicies]);

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Resource Policies</h2>
          <p className="page-subtitle">
            A resource policy is the access requirement for a resource. Device
            profiles define device trust; the policy decides what a resource
            requires.
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="status-pill border-border bg-secondary text-muted-foreground">
            <span className="font-bold text-foreground">
              {resourcePolicies.length}
            </span>{" "}
            total
          </span>
          <Button onClick={() => setShowAdd(true)} className="gap-2">
            <Plus className="h-4 w-4" />
            Create Resource Policy
          </Button>
        </div>
      </div>

      <div className="table-shell">
        <div className="table-scroll">
          <div className="table-head grid min-w-300 items-center grid-cols-[1.3fr_1.4fr_160px_190px] gap-4 px-5 py-4">
            {["Name", "Device Requirement", "Resources", "Actions"].map(
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
                <Skeleton key={index} className="h-14 rounded-2xl bg-secondary" />
              ))}
            </div>
          ) : error && resourcePolicies.length === 0 ? (
            <ErrorState
              title="Couldn't load resource policies"
              description="Something went wrong fetching your resource policies. This is a load error, not an empty workspace."
              action={<Button onClick={() => refetch()}>Retry</Button>}
            />
          ) : resourcePolicies.length === 0 ? (
            <EmptyState
              title="No resource policies defined"
              description="Create a resource policy to describe what a resource requires of a device."
              action={
                <Button onClick={() => setShowAdd(true)}>
                  Create Resource Policy
                </Button>
              }
            />
          ) : (
            <div className="min-w-300">
              {resourcePolicies.map((policy) => (
                <div
                  key={policy.id}
                  className="admin-table-row group grid items-center grid-cols-[1.3fr_1.4fr_160px_190px] gap-4 px-5 py-4"
                >
                  <div className="flex min-w-0 items-center gap-3">
                    <EntityIcon type="resource" />
                    <div className="min-w-0">
                      <div className="truncate text-[15px] font-bold leading-tight">
                        {policy.name}
                      </div>
                    </div>
                  </div>

                  {/*
                    The two semantics that are easiest to misread are spelled out
                    in words rather than left as a count: no profiles means Any
                    Device (never deny-all), and several profiles are OR'd.
                  */}
                  <div className="min-w-0 text-[13px] font-semibold text-muted-foreground">
                    {policy.deviceProfiles.length === 0 ? (
                      <span className="inline-flex items-center gap-1.5 text-foreground">
                        <Layers className="h-3.5 w-3.5" />
                        Any Device
                      </span>
                    ) : (
                      <div className="min-w-0">
                        <div className="truncate">
                          {policy.deviceProfiles.map((p) => p.name).join(" or ")}
                        </div>
                        {policy.deviceProfiles.length > 1 && (
                          <div className="text-xs opacity-70">
                            any one of {policy.deviceProfiles.length} profiles
                          </div>
                        )}
                      </div>
                    )}
                  </div>

                  <div className="text-[13px] font-semibold text-muted-foreground">
                    {policy.resources.length === 0 ? (
                      <span className="italic opacity-60">none</span>
                    ) : (
                      <span className="inline-flex items-center gap-1.5">
                        <Server className="h-3.5 w-3.5" />
                        {policy.resources.length}
                      </span>
                    )}
                  </div>

                  <div className="flex items-center justify-end gap-3">
                    <button
                      onClick={() => {
                        setAssignError(null);
                        setManagingPolicy({ id: policy.id, name: policy.name });
                      }}
                      className="text-[13px] font-bold text-muted-foreground transition hover:text-foreground"
                    >
                      Resources
                    </button>
                    <button
                      onClick={() =>
                        setEditingPolicy({
                          id: policy.id,
                          name: policy.name,
                          deviceProfiles: policy.deviceProfiles,
                        })
                      }
                      className="text-[13px] font-bold text-muted-foreground transition hover:text-foreground"
                    >
                      Edit
                    </button>
                    <button
                      onClick={() =>
                        setDeletingPolicy({ id: policy.id, name: policy.name })
                      }
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

      <CreateResourcePolicyModal
        open={showAdd}
        onOpenChange={setShowAdd}
        onSuccess={() => refetch()}
      />

      <EditResourcePolicyModal
        open={editingPolicy !== null}
        onOpenChange={(open) => {
          if (!open) {
            setEditingPolicy(null);
          }
        }}
        policy={editingPolicy}
        onSuccess={() => {
          setEditingPolicy(null);
          refetch();
        }}
      />

      <DeleteResourcePolicyDialog
        policy={deletingPolicy}
        loading={deleting}
        onClose={() => setDeletingPolicy(null)}
        onConfirm={() => {
          if (deletingPolicy) {
            deleteResourcePolicy({ variables: { id: deletingPolicy.id } });
          }
        }}
      />

      <ManageResourcesDialog
        policy={managingPolicy}
        assigned={managedResources}
        unassigned={unassignedResources}
        busy={assigning || unassigning}
        error={assignError}
        onClose={() => {
          setManagingPolicy(null);
          setAssignError(null);
        }}
        onAssign={(resourceId) => {
          if (managingPolicy) {
            assignResourcePolicy({
              variables: { resourceId, policyId: managingPolicy.id },
            });
          }
        }}
        onUnassign={(resourceId) =>
          unassignResourcePolicy({ variables: { resourceId } })
        }
      />
    </div>
  );
}
