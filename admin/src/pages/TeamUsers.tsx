import { useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import { Mail, MoreHorizontal, Plus, UserCircle2 } from 'lucide-react'
import {
  CreateInvitationDocument,
  GetGroupsDocument,
  GetUsersDocument,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { EmptyState, StatusPill } from '@/lib/console'
import { cn } from '@/lib/utils'

type User = {
  id: string
  email: string
  role: string
  createdAt: string
}

function roleTone(role: string): 'ok' | 'info' | 'warn' | 'muted' {
  switch (role.toUpperCase()) {
    case 'ADMIN':   return 'warn'
    case 'MEMBER':  return 'info'
    default:        return 'muted'
  }
}

function roleLabel(role: string): string {
  switch (role.toUpperCase()) {
    case 'ADMIN':  return 'admin'
    case 'MEMBER': return 'member'
    case 'VIEWER': return 'viewer'
    default:       return role.toLowerCase()
  }
}

function UserAvatar({ email }: { email: string }) {
  const initials = email.slice(0, 2).toUpperCase()
  return (
    <span className="grid h-8 w-8 shrink-0 place-items-center rounded-[10px] bg-[oklch(0.78_0.10_235/0.14)] text-[10px] font-bold text-[oklch(0.78_0.10_235)] border border-[oklch(0.78_0.10_235/0.25)]">
      {initials}
    </span>
  )
}

function InviteDialog({
  open,
  onClose,
}: {
  open: boolean
  onClose: () => void
}) {
  const [email, setEmail] = useState('')
  const [sent, setSent] = useState(false)

  const [createInvitation, { loading }] = useMutation(CreateInvitationDocument, {
    onCompleted: () => { setSent(true) },
  })

  function handleClose() {
    setEmail('')
    setSent(false)
    onClose()
  }

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!email.trim()) return
    createInvitation({ variables: { email: email.trim() } })
  }

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) handleClose() }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{sent ? 'Invitation sent' : 'Invite user'}</DialogTitle>
        </DialogHeader>
        {sent ? (
          <>
            <p className="text-sm text-muted-foreground">
              An invitation email has been sent to <span className="font-semibold text-foreground">{email}</span>.
            </p>
            <DialogFooter>
              <Button onClick={handleClose}>Done</Button>
            </DialogFooter>
          </>
        ) : (
          <form onSubmit={handleSubmit} className="space-y-4 pt-1">
            <div className="space-y-1.5">
              <Label htmlFor="invite-email">Email address</Label>
              <Input
                id="invite-email"
                type="email"
                placeholder="colleague@company.com"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                required
              />
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={handleClose} disabled={loading}>
                Cancel
              </Button>
              <Button type="submit" disabled={loading || !email.trim()}>
                {loading ? 'Sending…' : 'Send invite'}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}

export default function TeamUsers() {
  const [showInvite, setShowInvite] = useState(false)

  const { data: usersData, loading: usersLoading } = useQuery(GetUsersDocument, {
    fetchPolicy: 'cache-and-network',
  })
  const { data: groupsData } = useQuery(GetGroupsDocument, {
    fetchPolicy: 'cache-and-network',
  })

  const users: User[] = (usersData?.users ?? []) as User[]
  const groups = groupsData?.groups ?? []

  function groupCountForUser(userId: string): number {
    return groups.filter((g) =>
      (g.members ?? []).some((m: { id: string }) => m.id === userId)
    ).length
  }

  const activeCount = users.length

  return (
    <div className="space-y-6">
      {/* Page header */}
      <div className="page-header">
        <div className="flex items-center gap-4">
          <span className={cn(
            'grid h-11 w-11 place-items-center rounded-xl shrink-0',
            'bg-[oklch(0.78_0.10_235/0.14)] text-[oklch(0.78_0.10_235)] border border-[oklch(0.78_0.10_235/0.25)]',
          )}>
            <UserCircle2 className="h-5 w-5" />
          </span>
          <div>
            <h2 className="page-title">Users</h2>
            <p className="page-subtitle">Identity subjects for access control</p>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="status-pill border-border bg-secondary text-muted-foreground">
            <span className="font-bold text-foreground">{activeCount}</span>&nbsp;active
          </span>
          <Button variant="outline" className="gap-2" onClick={() => setShowInvite(true)}>
            <Mail className="h-4 w-4" />
            Invite
          </Button>
          <Button className="gap-2" onClick={() => setShowInvite(true)}>
            <Plus className="h-4 w-4" />
            Add User
          </Button>
        </div>
      </div>

      {/* Table */}
      <div className="table-shell">
        <div className="table-scroll">
          <div className="table-head grid min-w-[860px] items-center grid-cols-[2fr_2fr_110px_110px_150px_160px_60px] gap-4 px-5 py-4">
            {['Name', 'Email', 'Role', 'Status', 'Number of Groups', 'Created', 'Activity'].map((label, i) => (
              <div key={label + i} className={cn('table-head-label', i === 6 && 'text-right')}>
                {label}
              </div>
            ))}
          </div>

          {usersLoading && !usersData ? (
            <div className="min-w-[860px] p-5 space-y-3">
              {Array.from({ length: 4 }).map((_, i) => (
                <Skeleton key={i} className="h-14 rounded-2xl bg-secondary" />
              ))}
            </div>
          ) : users.length === 0 ? (
            <EmptyState
              icon={<UserCircle2 className="h-6 w-6" />}
              title="No users yet"
              description="Invite team members to give them access to the admin console."
              action={<Button onClick={() => setShowInvite(true)}>Invite user</Button>}
            />
          ) : (
            <div className="min-w-[860px]">
              {users.map((user) => {
                const count = groupCountForUser(user.id)
                return (
                  <div
                    key={user.id}
                    className="admin-table-row grid items-center grid-cols-[2fr_2fr_110px_110px_150px_160px_60px] gap-4 px-5 py-4"
                  >
                    {/* Name */}
                    <div className="flex min-w-0 items-center gap-3">
                      <UserAvatar email={user.email} />
                      <span className="truncate text-[14px] font-semibold">{user.email}</span>
                    </div>

                    {/* Email */}
                    <div className="truncate text-[13px] text-muted-foreground">{user.email}</div>

                    {/* Role */}
                    <div>
                      <StatusPill label={roleLabel(user.role)} tone={roleTone(user.role)} />
                    </div>

                    {/* Status */}
                    <div>
                      <StatusPill label="active" tone="ok" />
                    </div>

                    {/* Number of Groups */}
                    <div className="text-[13px] text-muted-foreground">
                      {count === 0 ? 'No groups' : `${count} group${count > 1 ? 's' : ''}`}
                    </div>

                    {/* Created */}
                    <div className="font-mono text-[12px] text-muted-foreground">
                      {user.createdAt ?? '—'}
                    </div>

                    {/* Activity */}
                    <div className="flex justify-end">
                      <button className="grid h-7 w-7 place-items-center rounded-lg text-muted-foreground transition hover:bg-secondary hover:text-foreground">
                        <MoreHorizontal className="h-4 w-4" />
                      </button>
                    </div>
                  </div>
                )
              })}
            </div>
          )}
        </div>
      </div>

      <InviteDialog open={showInvite} onClose={() => setShowInvite(false)} />
    </div>
  )
}
