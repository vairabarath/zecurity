import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useMutation, useQuery } from '@apollo/client/react'
import { Plus, Users } from 'lucide-react'
import {
  CreateGroupDocument,
  DeleteGroupDocument,
  GetGroupsDocument,
  UpdateGroupDocument,
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
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { EmptyState, relativeTime } from '@/lib/console'
import { cn } from '@/lib/utils'

type Group = {
  id: string
  name: string
  description?: string | null
  createdAt: string
  updatedAt: string
  members: { id: string }[]
  resources: { id: string }[]
}

function GroupIcon() {
  return (
    <span className={cn('grid h-9 w-9 place-items-center rounded-xl', 'bg-[oklch(0.85_0.13_80/0.14)] text-[oklch(0.85_0.13_80)] border border-[oklch(0.85_0.13_80/0.25)]')}>
      <Users className="h-4 w-4" />
    </span>
  )
}

type FormState = { name: string; description: string }

function GroupFormDialog({
  open,
  title,
  initial,
  loading,
  onClose,
  onSubmit,
}: {
  open: boolean
  title: string
  initial: FormState
  loading: boolean
  onClose: () => void
  onSubmit: (values: FormState) => void
}) {
  const [values, setValues] = useState<FormState>(initial)

  function handleOpenChange(next: boolean) {
    if (!next) onClose()
  }

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!values.name.trim()) return
    onSubmit(values)
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4 pt-1">
          <div className="space-y-1.5">
            <Label htmlFor="group-name">Name</Label>
            <Input
              id="group-name"
              placeholder="e.g. Engineering"
              value={values.name}
              onChange={(e) => setValues((v) => ({ ...v, name: e.target.value }))}
              required
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="group-desc">Description <span className="text-muted-foreground">(optional)</span></Label>
            <Input
              id="group-desc"
              placeholder="What is this group for?"
              value={values.description}
              onChange={(e) => setValues((v) => ({ ...v, description: e.target.value }))}
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose} disabled={loading}>
              Cancel
            </Button>
            <Button type="submit" disabled={loading || !values.name.trim()}>
              {loading ? 'Saving…' : 'Save'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function DeleteDialog({
  group,
  onClose,
  onConfirm,
  loading,
}: {
  group: Group | null
  onClose: () => void
  onConfirm: () => void
  loading: boolean
}) {
  return (
    <Dialog open={!!group} onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete group</DialogTitle>
        </DialogHeader>
        <p className="text-sm text-muted-foreground">
          Are you sure you want to delete <span className="font-semibold text-foreground">{group?.name}</span>? This will remove all member assignments and access rules.
        </p>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={loading}>Cancel</Button>
          <Button variant="destructive" onClick={onConfirm} disabled={loading}>
            {loading ? 'Deleting…' : 'Delete'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export default function Groups() {
  const navigate = useNavigate()

  const [showCreate, setShowCreate] = useState(false)
  const [editTarget, setEditTarget] = useState<Group | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<Group | null>(null)

  const { data, loading, refetch } = useQuery(GetGroupsDocument, {
    fetchPolicy: 'cache-and-network',
  })
  const groups: Group[] = (data?.groups ?? []) as Group[]

  const [createGroup, { loading: creating }] = useMutation(CreateGroupDocument, {
    onCompleted: () => { setShowCreate(false); refetch() },
  })

  const [updateGroup, { loading: updating }] = useMutation(UpdateGroupDocument, {
    onCompleted: () => { setEditTarget(null); refetch() },
  })

  const [deleteGroup, { loading: deleting }] = useMutation(DeleteGroupDocument, {
    onCompleted: () => { setDeleteTarget(null); refetch() },
  })

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Groups</h2>
          <p className="page-subtitle">Organise users and assign resource access by group.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="status-pill border-border bg-secondary text-muted-foreground">
            <span className="font-bold text-foreground">{groups.length}</span> total
          </span>
          <Button onClick={() => setShowCreate(true)} className="gap-2">
            <Plus className="h-4 w-4" />
            New Group
          </Button>
        </div>
      </div>

      <div className="table-shell">
        <div className="table-scroll">
          <div className="table-head grid min-w-[860px] items-center grid-cols-[1.5fr_1fr_100px_100px_140px_130px] gap-4 px-5 py-4">
            {['Name', 'Description', 'Members', 'Resources', 'Created', 'Actions'].map((label, index) => (
              <div key={label + index} className={`table-head-label ${index === 5 ? 'text-right' : ''}`}>{label}</div>
            ))}
          </div>

          {loading && !data ? (
            <div className="min-w-[860px] p-5 space-y-3">
              {Array.from({ length: 4 }).map((_, index) => (
                <Skeleton key={index} className="h-14 rounded-2xl bg-secondary" />
              ))}
            </div>
          ) : groups.length === 0 ? (
            <EmptyState
              icon={<Users className="h-6 w-6" />}
              title="No groups yet"
              description="Create a group, add users, and assign resources to control who can access what."
              action={<Button onClick={() => setShowCreate(true)}>New Group</Button>}
            />
          ) : (
            <div className="min-w-[860px]">
              {groups.map((group) => (
                <div
                  key={group.id}
                  className="admin-table-row group grid items-center grid-cols-[1.5fr_1fr_100px_100px_140px_130px] gap-4 px-5 py-4"
                >
                  <div className="flex min-w-0 items-center gap-3">
                    <GroupIcon />
                    <div className="min-w-0">
                      <div className="truncate text-[15px] font-bold leading-tight">{group.name}</div>
                    </div>
                  </div>

                  <div className="truncate text-[13px] text-muted-foreground">
                    {group.description || <span className="italic opacity-50">No description</span>}
                  </div>

                  <div className="text-[13px] font-semibold text-foreground">
                    {group.members.length}
                  </div>

                  <div className="text-[13px] font-semibold text-foreground">
                    {group.resources.length === 0 ? (
                      <span className="text-muted-foreground italic text-[12px]">none</span>
                    ) : group.resources.length}
                  </div>

                  <div className="font-mono text-[12.5px] text-muted-foreground">
                    {relativeTime(group.createdAt)}
                  </div>

                  <div className="flex items-center justify-end gap-3">
                    <button
                      onClick={() => setEditTarget(group)}
                      className="text-[12.5px] font-semibold text-muted-foreground transition hover:text-foreground"
                    >
                      Edit
                    </button>
                    <button
                      onClick={() => setDeleteTarget(group)}
                      className="text-[12.5px] font-semibold text-[oklch(0.75_0.16_25)] transition hover:opacity-80"
                    >
                      Delete
                    </button>
                    <button
                      onClick={() => navigate(`/groups/${group.id}`)}
                      className="inline-flex items-center gap-1 text-[13px] font-bold text-primary transition hover:opacity-80"
                    >
                      Manage <span className="transition-transform group-hover:translate-x-0.5">→</span>
                    </button>
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      <GroupFormDialog
        open={showCreate}
        title="New Group"
        initial={{ name: '', description: '' }}
        loading={creating}
        onClose={() => setShowCreate(false)}
        onSubmit={({ name, description }) =>
          createGroup({ variables: { name, description: description || undefined } })
        }
      />

      {editTarget && (
        <GroupFormDialog
          open
          title="Edit Group"
          initial={{ name: editTarget.name, description: editTarget.description ?? '' }}
          loading={updating}
          onClose={() => setEditTarget(null)}
          onSubmit={({ name, description }) =>
            updateGroup({ variables: { id: editTarget.id, name, description: description || undefined } })
          }
        />
      )}

      <DeleteDialog
        group={deleteTarget}
        loading={deleting}
        onClose={() => setDeleteTarget(null)}
        onConfirm={() => deleteTarget && deleteGroup({ variables: { id: deleteTarget.id } })}
      />
    </div>
  )
}
