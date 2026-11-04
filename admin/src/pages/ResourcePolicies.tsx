import { useMemo, useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import { Plus, ShieldCheck } from 'lucide-react'
import {
  GetResourcePoliciesDocument,
  DeleteResourcePolicyDocument,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { ResourcePolicyModal } from '@/components/ResourcePolicyModal'
import { Capability, useCan } from '@/lib/capabilities'
import { EmptyState, ErrorState, EntityIcon } from '@/lib/console'
import { toastMutationError } from '@/lib/mutationErrors'
import { toast } from 'sonner'

type PolicyRow = {
  id: string
  name: string
  description?: string | null
  isDefault: boolean
  resourceCount: number
  profiles: { id: string; name: string }[]
  resources: { id: string; name: string }[]
}

function DeletePolicyDialog({
  policy,
  onClose,
  onConfirm,
  loading,
}: {
  policy: PolicyRow | null
  onClose: () => void
  onConfirm: () => void
  loading: boolean
}) {
  return (
    <Dialog open={!!policy} onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete resource policy</DialogTitle>
        </DialogHeader>
        <div className="space-y-3 text-sm text-muted-foreground">
          <p>
            Are you sure you want to delete{' '}
            <span className="font-semibold text-foreground">{policy?.name}</span>?
          </p>
          {policy && policy.resourceCount > 0 ? (
            <div className="rounded-lg border border-amber-500/30 bg-amber-500/10 p-3 text-amber-500">
              <span className="font-semibold">
                {policy.resourceCount}{' '}
                {policy.resourceCount === 1 ? 'resource' : 'resources'} will move to the Default policy
              </span>{' '}
              (which allows any device until profiles are assigned to it).
            </div>
          ) : (
            <p>No resources are currently assigned to this policy.</p>
          )}
          <p className="text-xs">This action is audited and cannot be undone.</p>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={loading}>
            Cancel
          </Button>
          <Button variant="destructive" onClick={onConfirm} disabled={loading}>
            {loading ? 'Deleting…' : 'Delete Policy'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export default function ResourcePolicies() {
  const canWrite = useCan(Capability.PostureWrite)
  const [showCreate, setShowCreate] = useState(false)
  const [editingPolicy, setEditingPolicy] = useState<PolicyRow | null>(null)
  const [deletingPolicy, setDeletingPolicy] = useState<PolicyRow | null>(null)

  const { data, loading, error, refetch } = useQuery(GetResourcePoliciesDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 30000,
  })

  const [deletePolicy, { loading: deleting }] = useMutation(DeleteResourcePolicyDocument, {
    onError: toastMutationError,
    onCompleted: () => {
      toast.success('Resource policy deleted')
      setDeletingPolicy(null)
      refetch()
    },
    refetchQueries: [{ query: GetResourcePoliciesDocument }],
  })

  const policies = useMemo(() => data?.resourcePolicies ?? [], [data])

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Resource Policies</h2>
          <p className="page-subtitle">Manage device posture requirements for resources.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="status-pill border-border bg-secondary text-muted-foreground">
            <span className="font-bold text-foreground">{policies.length}</span> total
          </span>
          {canWrite && (
            <Button onClick={() => setShowCreate(true)} className="gap-2">
              <Plus className="h-4 w-4" />
              Create Resource Policy
            </Button>
          )}
        </div>
      </div>

      <div className="table-shell">
        <div className="table-scroll">
          <div className="table-head grid min-w-300 items-center grid-cols-[1.5fr_2fr_120px_120px] gap-4 px-5 py-4">
            {['Name', 'Profiles', 'Resources', 'Actions'].map((label, index) => (
              <div
                key={label + index}
                className={`table-head-label ${index === 3 ? 'text-right' : ''}`}
              >
                {label}
              </div>
            ))}
          </div>

          {loading && !data ? (
            <div className="min-w-300 p-5 space-y-3">
              {Array.from({ length: 4 }).map((_, index) => (
                <Skeleton key={index} className="h-14 rounded-2xl bg-secondary" />
              ))}
            </div>
          ) : error && policies.length === 0 ? (
            <ErrorState
              title="Couldn't load resource policies"
              description="Something went wrong fetching resource policies."
              action={<Button onClick={() => refetch()}>Retry</Button>}
            />
          ) : policies.length === 0 ? (
            <EmptyState
              title="No resource policies"
              description="Every workspace has a Default policy. Create policies to define required posture for specific resources."
              action={
                canWrite ? (
                  <Button onClick={() => setShowCreate(true)}>Create Resource Policy</Button>
                ) : undefined
              }
            />
          ) : (
            <div className="min-w-300">
              {policies.map((pol) => (
                <div
                  key={pol.id}
                  className="admin-table-row group grid items-center grid-cols-[1.5fr_2fr_120px_120px] gap-4 px-5 py-4"
                >
                  <div className="flex min-w-0 items-center gap-3">
                    <EntityIcon type="resource" />
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <span className="truncate text-[15px] font-bold leading-tight">
                          {pol.name}
                        </span>
                        {pol.isDefault && (
                          <span className="status-pill border-primary/30 bg-primary/10 text-primary text-[11px] font-bold py-0.5 px-2">
                            Default
                          </span>
                        )}
                      </div>
                      {pol.description && (
                        <div className="truncate text-xs text-muted-foreground mt-0.5">
                          {pol.description}
                        </div>
                      )}
                    </div>
                  </div>

                  {/* Profiles chips */}
                  <div className="flex flex-wrap items-center gap-1.5 min-w-0">
                    {pol.profiles.length === 0 ? (
                      <span className="rounded-md border border-border bg-secondary/50 px-2 py-0.5 text-xs font-medium text-muted-foreground">
                        Any device
                      </span>
                    ) : (
                      pol.profiles.map((p) => (
                        <span
                          key={p.id}
                          className="inline-flex items-center gap-1 rounded-md border border-primary/30 bg-primary/5 px-2 py-0.5 text-xs font-semibold text-primary"
                        >
                          <ShieldCheck className="h-3 w-3" />
                          {p.name}
                        </span>
                      ))
                    )}
                  </div>

                  {/* Resources count */}
                  <div className="text-[13px] font-semibold text-muted-foreground">
                    {pol.resourceCount}{' '}
                    <span className="text-xs font-normal">
                      {pol.resourceCount === 1 ? 'resource' : 'resources'}
                    </span>
                  </div>

                  {/* Actions */}
                  <div className="flex items-center justify-end gap-3">
                    {canWrite && (
                      <button
                        onClick={() => setEditingPolicy(pol)}
                        className="text-[13px] font-bold text-muted-foreground transition hover:text-foreground"
                      >
                        Edit
                      </button>
                    )}
                    {canWrite && !pol.isDefault && (
                      <button
                        onClick={() => setDeletingPolicy(pol)}
                        className="text-[13px] font-bold text-[oklch(0.75_0.16_25)] transition hover:opacity-80"
                      >
                        Delete
                      </button>
                    )}
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      <ResourcePolicyModal
        open={showCreate}
        onOpenChange={setShowCreate}
        onSuccess={() => refetch()}
      />

      <ResourcePolicyModal
        open={editingPolicy !== null}
        onOpenChange={(open) => {
          if (!open) setEditingPolicy(null)
        }}
        policy={editingPolicy}
        onSuccess={() => {
          setEditingPolicy(null)
          refetch()
        }}
      />

      <DeletePolicyDialog
        policy={deletingPolicy}
        loading={deleting}
        onClose={() => setDeletingPolicy(null)}
        onConfirm={() => {
          if (deletingPolicy) {
            deletePolicy({ variables: { id: deletingPolicy.id } })
          }
        }}
      />
    </div>
  )
}
