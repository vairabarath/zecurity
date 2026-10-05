import { useCallback, useEffect, useState } from 'react'
import { MoreHorizontal, UserPlus } from 'lucide-react'
import { Navigate } from 'react-router-dom'
import { ApiError } from '@/api/client'
import {
  changeOperatorRole,
  createOperator,
  disableOperator,
  enableOperator,
  listOperators,
  resetOperatorPassword,
} from '@/api/operators'
import { ErrorCode, type OperatorView } from '@/api/types'
import { ROLE_LABELS, type ProviderRole } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { AddOperatorDialog, ChangeRoleDialog, ConfirmDialog } from '@/components/OperatorDialogs'
import { RoleBadge } from '@/components/RoleBadge'
import { TemporaryPasswordDialog, type TemporaryCredential } from '@/components/TemporaryPasswordDialog'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { toast } from '@/components/ui/use-toast'
import { formatDateTime } from '@/lib/format'
import { operatorErrorMessage } from '@/lib/operator-errors'

// Provider users (C5, super-admin only). The route is wrapped in
// RequireRole(['super-admin']) and the controller authorizes every call; the
// UI only hides what the server would refuse (no controls on your own row).
//
// Rules: every mutation is confirmed first; after a success the list is
// re-read from the server (no optimistic edits); enable and reset-password
// are separate calls; temporary passwords live only in `credential` state
// while their dialog is open.

type Action =
  | { kind: 'add' }
  | { kind: 'role'; op: OperatorView }
  | { kind: 'disable'; op: OperatorView }
  | { kind: 'enable'; op: OperatorView }
  | { kind: 'reset'; op: OperatorView }
  /** After a successful enable: the operator has no password until a reset. */
  | { kind: 'reset-offer'; op: OperatorView }

type ListState =
  | { status: 'loading' }
  | { status: 'ready'; operators: OperatorView[] }
  | { status: 'error'; message: string }
  | { status: 'forbidden' }

export function ProviderUsers() {
  const myId = useSessionStore((s) => s.me?.user_id)
  const [list, setList] = useState<ListState>({ status: 'loading' })
  const [action, setAction] = useState<Action | null>(null)
  const [pending, setPending] = useState(false)
  const [actionError, setActionError] = useState<string>()
  const [credential, setCredential] = useState<TemporaryCredential | null>(null)

  const reload = useCallback(async () => {
    try {
      setList({ status: 'ready', operators: await listOperators() })
    } catch (err) {
      if (err instanceof ApiError && err.sessionEnded) return
      if (err instanceof ApiError && err.status === 403) {
        setList({ status: 'forbidden' })
        return
      }
      setList({ status: 'error', message: operatorErrorMessage(err) })
    }
  }, [])

  useEffect(() => {
    // Initial load; reload() only sets state once the request settles.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void reload()
  }, [reload])

  function open(next: Action) {
    setActionError(undefined)
    setAction(next)
  }

  function close() {
    if (pending) return
    setActionError(undefined)
    setAction(null)
  }

  /**
   * Runs a confirmed mutation. On success: re-read the list, then `after`
   * decides what comes next (close, show a credential, offer a reset).
   */
  async function run<T>(call: () => Promise<T>, after: (result: T) => void) {
    setPending(true)
    setActionError(undefined)
    try {
      const result = await call()
      await reload()
      after(result)
    } catch (err) {
      if (err instanceof ApiError && err.sessionEnded) return // the guards show Login
      if (err instanceof ApiError && err.status === 403) {
        setAction(null)
        setList({ status: 'forbidden' })
        return
      }
      if (err instanceof ApiError && err.code === ErrorCode.notFound) void reload()
      setActionError(operatorErrorMessage(err))
    } finally {
      setPending(false)
    }
  }

  function done(message: string) {
    setAction(null)
    toast({ title: message })
  }

  if (list.status === 'forbidden') return <Navigate to="/forbidden" replace />

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="font-display text-2xl font-semibold tracking-tight">Provider users</h1>
          <p className="text-sm text-muted-foreground">Operators who can sign in to the Provider Console.</p>
        </div>
        <Button onClick={() => open({ kind: 'add' })}>
          <UserPlus aria-hidden />
          Add operator
        </Button>
      </div>

      {list.status === 'loading' && <Skeleton className="h-40 w-full" aria-label="Loading operators" />}

      {list.status === 'error' && (
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
            <span>Couldn’t load operators. {list.message}</span>
            <Button variant="outline" size="sm" onClick={() => void reload()}>
              Retry
            </Button>
          </AlertDescription>
        </Alert>
      )}

      {list.status === 'ready' && (
        <OperatorsTable
          operators={list.operators}
          myId={myId}
          onAction={(a) => open(a)}
        />
      )}

      <AddOperatorDialog
        open={action?.kind === 'add'}
        pending={pending}
        error={action?.kind === 'add' ? actionError : undefined}
        onCancel={close}
        onSubmit={(email, role) =>
          void run(
            () => createOperator(email, role),
            (res) => {
              setAction(null)
              setCredential({ email: res.user.email, password: res.temporary_password })
            },
          )
        }
      />

      {action?.kind === 'role' && (
        <ChangeRoleDialog
          operator={action.op}
          pending={pending}
          error={actionError}
          onCancel={close}
          onConfirm={(role: ProviderRole) =>
            void run(
              () => changeOperatorRole(action.op.id, role),
              () => done(`${action.op.email} is now ${ROLE_LABELS[role]}.`),
            )
          }
        />
      )}

      {action?.kind === 'disable' && (
        <ConfirmDialog
          title="Disable operator"
          description={
            <>
              <strong className="text-foreground">{action.op.email}</strong> will be signed out everywhere and
              won’t be able to sign in until re-enabled.
            </>
          }
          confirmLabel="Disable"
          pendingLabel="Disabling…"
          destructive
          pending={pending}
          error={actionError}
          onCancel={close}
          onConfirm={() => void run(() => disableOperator(action.op.id), () => done(`${action.op.email} is disabled.`))}
        />
      )}

      {action?.kind === 'enable' && (
        <ConfirmDialog
          title="Enable operator"
          description={
            <>
              <strong className="text-foreground">{action.op.email}</strong> will be re-enabled{' '}
              <strong className="text-foreground">without a password</strong>: their old password is cleared. Reset
              the password afterwards so they can sign in.
            </>
          }
          confirmLabel="Enable"
          pendingLabel="Enabling…"
          pending={pending}
          error={actionError}
          onCancel={close}
          onConfirm={() =>
            void run(
              () => enableOperator(action.op.id),
              () => {
                toast({ title: `${action.op.email} is enabled.` })
                setAction({ kind: 'reset-offer', op: action.op })
              },
            )
          }
        />
      )}

      {(action?.kind === 'reset' || action?.kind === 'reset-offer') && (
        <ConfirmDialog
          title={action.kind === 'reset-offer' ? 'Operator enabled' : 'Reset password'}
          description={
            action.kind === 'reset-offer' ? (
              <>
                <strong className="text-foreground">{action.op.email}</strong> has no password and can’t sign in
                yet. Reset the password now to issue a temporary one, shown once.
              </>
            ) : (
              <>
                <strong className="text-foreground">{action.op.email}</strong> will be signed out everywhere and
                given a new temporary password, shown once.
              </>
            )
          }
          confirmLabel={action.kind === 'reset-offer' ? 'Reset password now' : 'Reset password'}
          cancelLabel={action.kind === 'reset-offer' ? 'Later' : 'Cancel'}
          pendingLabel="Resetting…"
          destructive={action.kind === 'reset'}
          pending={pending}
          error={actionError}
          onCancel={close}
          onConfirm={() =>
            void run(
              () => resetOperatorPassword(action.op.id),
              (res) => {
                setAction(null)
                setCredential({ email: action.op.email, password: res.temporary_password })
              },
            )
          }
        />
      )}

      <TemporaryPasswordDialog credential={credential} onClose={() => setCredential(null)} />
    </div>
  )
}

function OperatorsTable({
  operators,
  myId,
  onAction,
}: {
  operators: OperatorView[]
  myId: string | undefined
  onAction: (a: Action) => void
}) {
  if (operators.length === 0) {
    return <p className="text-sm text-muted-foreground">No operators yet.</p>
  }
  return (
    <Table aria-label="Provider operators">
      <TableHeader>
        <TableRow>
          <TableHead>Email</TableHead>
          <TableHead>Role</TableHead>
          <TableHead>Status</TableHead>
          <TableHead>Last sign-in</TableHead>
          <TableHead>
            <span className="sr-only">Actions</span>
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {operators.map((op) => {
          const self = op.id === myId
          const disabled = op.disabled_at !== null
          return (
            <TableRow key={op.id} aria-label={op.email}>
              <TableCell className="max-w-64">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="truncate">{op.email}</span>
                  {self && <span className="text-xs text-muted-foreground">(you)</span>}
                </div>
              </TableCell>
              <TableCell>
                <RoleBadge role={op.role} />
              </TableCell>
              <TableCell>
                <div className="flex flex-wrap gap-1.5">
                  {disabled ? <Badge variant="secondary">Disabled</Badge> : <Badge variant="outline">Active</Badge>}
                  {!disabled && !op.has_password && <Badge variant="warning">No password</Badge>}
                  {!disabled && op.has_password && op.must_change_password && (
                    <Badge variant="outline">Must change password</Badge>
                  )}
                </div>
              </TableCell>
              <TableCell className="whitespace-nowrap text-muted-foreground">
                {formatDateTime(op.last_login_at)}
              </TableCell>
              <TableCell className="text-right">
                {self ? (
                  <span className="text-xs text-muted-foreground">Use the account menu</span>
                ) : (
                  // modal={false}: the menu opens dialogs, and a modal menu
                  // would leave the page inert after it closes.
                  <DropdownMenu modal={false}>
                    <DropdownMenuTrigger asChild>
                      <Button variant="ghost" size="sm" aria-label={`Actions for ${op.email}`}>
                        <MoreHorizontal aria-hidden />
                      </Button>
                    </DropdownMenuTrigger>
                    <DropdownMenuContent align="end">
                      <DropdownMenuItem onSelect={() => onAction({ kind: 'role', op })}>Change role</DropdownMenuItem>
                      {disabled ? (
                        <DropdownMenuItem onSelect={() => onAction({ kind: 'enable', op })}>Enable</DropdownMenuItem>
                      ) : (
                        <>
                          <DropdownMenuItem onSelect={() => onAction({ kind: 'reset', op })}>
                            Reset password
                          </DropdownMenuItem>
                          <DropdownMenuItem
                            className="text-destructive"
                            onSelect={() => onAction({ kind: 'disable', op })}
                          >
                            Disable
                          </DropdownMenuItem>
                        </>
                      )}
                    </DropdownMenuContent>
                  </DropdownMenu>
                )}
              </TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}
