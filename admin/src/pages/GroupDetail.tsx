import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { useMutation, useQuery } from '@apollo/client/react'
import { ArrowLeft, Box, Loader2, Minus, Plus, Users } from 'lucide-react'
import {
  AddGroupMemberDocument,
  AssignResourceToGroupDocument,
  GetAllResourcesDocument,
  GetGroupDocument,
  GetUsersDocument,
  RemoveGroupMemberDocument,
  UnassignResourceFromGroupDocument,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { cn } from '@/lib/utils'

type Tab = 'members' | 'resources'

function SectionCard({ children }: { children: React.ReactNode }) {
  return (
    <div className="surface-card overflow-hidden">
      {children}
    </div>
  )
}

function SectionHeader({
  title,
  subtitle,
  action,
}: {
  title: string
  subtitle?: string
  action?: React.ReactNode
}) {
  return (
    <div className="flex items-start justify-between border-b border-border px-5 py-4">
      <div>
        <div className="text-[1.15rem] font-bold">{title}</div>
        {subtitle && <div className="mt-1 text-sm text-muted-foreground">{subtitle}</div>}
      </div>
      {action}
    </div>
  )
}

export default function GroupDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const [tab, setTab] = useState<Tab>('members')
  const [showAddMember, setShowAddMember] = useState(false)
  const [showAssignResource, setShowAssignResource] = useState(false)
  const [selectedUserId, setSelectedUserId] = useState('')
  const [selectedResourceId, setSelectedResourceId] = useState('')

  const { data, loading, refetch } = useQuery(GetGroupDocument, {
    variables: { id: id! },
    skip: !id,
    fetchPolicy: 'cache-and-network',
  })

  const { data: resourcesData } = useQuery(GetAllResourcesDocument, {
    fetchPolicy: 'cache-and-network',
  })

  const { data: usersData } = useQuery(GetUsersDocument, {
    fetchPolicy: 'cache-and-network',
  })

  const group = data?.group
  const allResources = resourcesData?.allResources ?? []

  const assignedResourceIds = new Set((group?.resources ?? []).map((r) => r.id))
  const unassignedResources = allResources.filter((r) => !assignedResourceIds.has(r.id))

  const allUsers = usersData?.users ?? []
  const memberIds = new Set((group?.members ?? []).map((m) => m.id))
  const nonMembers = allUsers.filter((u) => !memberIds.has(u.id))

  const [addMember, { loading: addingMember }] = useMutation(AddGroupMemberDocument, {
    onCompleted: () => { setShowAddMember(false); setSelectedUserId(''); refetch() },
  })

  const [removeMember, { loading: removingMember }] = useMutation(RemoveGroupMemberDocument, {
    onCompleted: () => refetch(),
  })

  const [assignResource, { loading: assigning }] = useMutation(AssignResourceToGroupDocument, {
    onCompleted: () => { setShowAssignResource(false); setSelectedResourceId(''); refetch() },
  })

  const [unassignResource, { loading: unassigning }] = useMutation(UnassignResourceFromGroupDocument, {
    onCompleted: () => refetch(),
  })

  if (loading && !group) {
    return (
      <div className="flex items-center justify-center p-16">
        <div className="flex flex-col items-center gap-3">
          <Loader2 className="h-6 w-6 animate-spin text-primary" />
          <p className="text-xs font-mono tracking-[0.14em] text-muted-foreground">Loading group...</p>
        </div>
      </div>
    )
  }

  if (!group) {
    return (
      <div className="space-y-6">
        <Link to="/groups" className="inline-flex items-center gap-2 text-sm text-muted-foreground transition hover:text-foreground">
          <ArrowLeft className="h-4 w-4" />
          Back to Groups
        </Link>
        <div className="surface-card px-6 py-20 text-center">
          <Users className="mx-auto h-14 w-14 text-destructive/40" />
          <h2 className="mt-4 text-2xl font-bold">Group not found</h2>
          <p className="mt-2 text-muted-foreground">This group no longer exists or was deleted.</p>
          <Button className="mt-6" onClick={() => navigate('/groups')}>Back to Groups</Button>
        </div>
      </div>
    )
  }

  return (
    <div className="space-y-6">
      {/* Breadcrumb */}
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <Link to="/groups" className="transition hover:text-foreground">Groups</Link>
        <span>/</span>
        <span className="text-foreground">{group.name}</span>
      </div>

      <Link to="/groups" className="inline-flex items-center gap-2 text-sm text-muted-foreground transition hover:text-foreground">
        <ArrowLeft className="h-4 w-4" />
        Back to Groups
      </Link>

      {/* Header */}
      <div className="flex items-start gap-4">
        <div className="grid h-14 w-14 place-items-center rounded-[16px] bg-[oklch(0.85_0.13_80/0.14)] text-[oklch(0.85_0.13_80)]">
          <Users className="h-7 w-7" />
        </div>
        <div className="min-w-0">
          <h1 className="text-[2.2rem] font-bold tracking-[-0.03em]">{group.name}</h1>
          {group.description && (
            <p className="mt-1 text-sm text-muted-foreground">{group.description}</p>
          )}
          <div className="mt-2 flex flex-wrap items-center gap-3 text-sm text-muted-foreground">
            <span className="font-mono text-[13px] opacity-70">{group.id}</span>
            <span className="opacity-40">•</span>
            <span><span className="font-semibold text-foreground">{group.members.length}</span> members</span>
            <span className="opacity-40">•</span>
            <span><span className="font-semibold text-foreground">{group.resources.length}</span> resources</span>
          </div>
        </div>
      </div>

      {/* Tabs */}
      <div className="flex items-center gap-1.5">
        {(['members', 'resources'] as Tab[]).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={cn(
              'rounded-full px-4 py-1.5 text-xs font-bold capitalize transition',
              tab === t
                ? 'border border-primary/30 bg-primary/12 text-primary'
                : 'bg-secondary text-muted-foreground hover:text-foreground',
            )}
          >
            {t}
          </button>
        ))}
      </div>

      {/* Members Tab */}
      {tab === 'members' && (
        <SectionCard>
          <SectionHeader
            title="Members"
            subtitle={`${group.members.length} user${group.members.length === 1 ? '' : 's'} in this group`}
            action={
              <Button size="sm" className="gap-2" onClick={() => setShowAddMember(true)} disabled={nonMembers.length === 0}>
                <Plus className="h-4 w-4" />
                Add Member
              </Button>
            }
          />

          {group.members.length === 0 ? (
            <div className="px-5 py-14 text-center text-sm text-muted-foreground">
              No members yet. Add users to grant them access to group resources.
            </div>
          ) : (
            <div>
              {group.members.map((member) => (
                <div
                  key={member.id}
                  className="admin-table-row flex items-center justify-between gap-4 px-5 py-3.5"
                >
                  <div className="flex items-center gap-3">
                    <div className="grid h-8 w-8 place-items-center rounded-[10px] bg-[oklch(0.78_0.09_310/0.14)] text-[oklch(0.78_0.09_310)] text-[11px] font-bold border border-[oklch(0.78_0.09_310/0.25)]">
                      {member.email.slice(0, 2).toUpperCase()}
                    </div>
                    <div>
                      <div className="text-[14px] font-semibold">{member.email}</div>
                      <div className="text-[11.5px] capitalize text-muted-foreground">{member.role.toLowerCase()}</div>
                    </div>
                  </div>
                  <button
                    onClick={() => removeMember({ variables: { groupId: group.id, userId: member.id } })}
                    disabled={removingMember}
                    className="inline-flex items-center gap-1.5 text-[12.5px] font-semibold text-[oklch(0.75_0.16_25)] transition hover:opacity-80 disabled:opacity-40"
                  >
                    <Minus className="h-3.5 w-3.5" />
                    Remove
                  </button>
                </div>
              ))}
            </div>
          )}
        </SectionCard>
      )}

      {/* Resources Tab */}
      {tab === 'resources' && (
        <SectionCard>
          <SectionHeader
            title="Resources"
            subtitle={
              group.resources.length === 0
                ? 'No resources assigned — members have no access (default-deny)'
                : `${group.resources.length} resource${group.resources.length === 1 ? '' : 's'} accessible by this group`
            }
            action={
              <Button size="sm" className="gap-2" onClick={() => setShowAssignResource(true)} disabled={unassignedResources.length === 0}>
                <Plus className="h-4 w-4" />
                Assign Resource
              </Button>
            }
          />

          {group.resources.length === 0 ? (
            <div className="px-5 py-14 text-center">
              <p className="text-sm font-semibold text-[oklch(0.85_0.13_80)]">Default-deny active</p>
              <p className="mt-1 text-sm text-muted-foreground">No resources are assigned. Members of this group cannot access anything.</p>
              {unassignedResources.length > 0 && (
                <Button size="sm" className="mt-4 gap-2" onClick={() => setShowAssignResource(true)}>
                  <Plus className="h-4 w-4" />
                  Assign Resource
                </Button>
              )}
            </div>
          ) : (
            <div>
              {group.resources.map((resource) => (
                <div
                  key={resource.id}
                  className="admin-table-row flex items-center justify-between gap-4 px-5 py-3.5"
                >
                  <div className="flex items-center gap-3">
                    <span className="grid h-9 w-9 place-items-center rounded-xl bg-[oklch(0.78_0.09_310/0.14)] text-[oklch(0.78_0.09_310)] border border-[oklch(0.78_0.09_310/0.25)]">
                      <Box className="h-4 w-4" />
                    </span>
                    <div>
                      <div className="text-[14px] font-semibold">{resource.name}</div>
                      <div className="font-mono text-[11.5px] text-muted-foreground">
                        {resource.host} · {resource.protocol.toUpperCase()} {resource.portFrom}
                        {resource.portFrom !== resource.portTo ? `–${resource.portTo}` : ''}
                      </div>
                    </div>
                  </div>
                  <button
                    onClick={() => unassignResource({ variables: { resourceId: resource.id, groupId: group.id } })}
                    disabled={unassigning}
                    className="inline-flex items-center gap-1.5 text-[12.5px] font-semibold text-[oklch(0.75_0.16_25)] transition hover:opacity-80 disabled:opacity-40"
                  >
                    <Minus className="h-3.5 w-3.5" />
                    Unassign
                  </button>
                </div>
              ))}
            </div>
          )}
        </SectionCard>
      )}

      {/* Add Member Dialog */}
      <Dialog open={showAddMember} onOpenChange={(open) => { if (!open) { setShowAddMember(false); setSelectedUserId('') } }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add Member</DialogTitle>
          </DialogHeader>
          <div className="space-y-1.5 pt-1">
            <Label htmlFor="user-select">User</Label>
            <select
              id="user-select"
              value={selectedUserId}
              onChange={(e) => setSelectedUserId(e.target.value)}
              className="w-full rounded-xl border border-border bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-primary/30"
            >
              <option value="">Select a user…</option>
              {nonMembers.map((u) => (
                <option key={u.id} value={u.id}>
                  {u.email} ({u.role.toLowerCase()})
                </option>
              ))}
            </select>
            {nonMembers.length === 0 && (
              <p className="text-[11.5px] text-muted-foreground">All workspace users are already members.</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => { setShowAddMember(false); setSelectedUserId('') }} disabled={addingMember}>
              Cancel
            </Button>
            <Button
              disabled={!selectedUserId || addingMember}
              onClick={() => addMember({ variables: { groupId: group.id, userId: selectedUserId } })}
            >
              {addingMember ? 'Adding…' : 'Add'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Assign Resource Dialog */}
      <Dialog open={showAssignResource} onOpenChange={(open) => { if (!open) { setShowAssignResource(false); setSelectedResourceId('') } }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Assign Resource</DialogTitle>
          </DialogHeader>
          <div className="space-y-1.5 pt-1">
            <Label htmlFor="resource-select">Resource</Label>
            <select
              id="resource-select"
              value={selectedResourceId}
              onChange={(e) => setSelectedResourceId(e.target.value)}
              className="w-full rounded-xl border border-border bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-primary/30"
            >
              <option value="">Select a resource…</option>
              {unassignedResources.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.name} — {r.host} ({r.protocol.toUpperCase()} {r.portFrom})
                </option>
              ))}
            </select>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => { setShowAssignResource(false); setSelectedResourceId('') }} disabled={assigning}>
              Cancel
            </Button>
            <Button
              disabled={!selectedResourceId || assigning}
              onClick={() => assignResource({ variables: { resourceId: selectedResourceId, groupId: group.id } })}
            >
              {assigning ? 'Assigning…' : 'Assign'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
