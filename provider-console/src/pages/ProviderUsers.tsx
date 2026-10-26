import { useCallback, useEffect, useState } from 'react'
import { MoreHorizontal, ShieldCheck, UserPlus } from 'lucide-react'
import { Navigate } from 'react-router-dom'
import { ApiError } from '@/api/client'
import {
  changeOperatorRole,
  createOperator,
  disableOperator,
  enableOperator,
  listOperators,
  resetOperatorPassword,
  resetOperatorTotp,
} from '@/api/operators'
import { ErrorCode, type OperatorView } from '@/api/types'
import { ROLE_LABELS, type ProviderRole } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { AddOperatorDialog, ChangeRoleDialog, ConfirmDialog, ResetMfaDialog } from '@/components/OperatorDialogs'
import { PageHeader } from '@/components/PageHeader'
import { RoleBadge } from '@/components/RoleBadge'
import { TemporaryPasswordDialog, type TemporaryCredential } from '@/components/TemporaryPasswordDialog'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { toast } from '@/components/ui/use-toast'
import { formatRelative } from '@/lib/expiry'
import { formatDate, formatDateTime } from '@/lib/format'
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
  | { kind: 'reset-mfa'; op: OperatorView }
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
      <PageHeader
        title="Operators"
        description="People who can sign in to the Provider Console. Super-admins only."
        actions={
          <Button onClick={() => open({ kind: 'add' })}>
            <UserPlus aria-hidden />
            Add operator
          </Button>
        }
      />

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
        <>
          <OperatorCounts operators={list.operators} />
          <OperatorsTable operators={list.operators} myId={myId} onAction={(a) => open(a)} />
          <div className="flex gap-3 rounded-xl border border-border bg-sidebar px-4 py-3.5 text-[13px] leading-relaxed text-subtle-foreground">
            <ShieldCheck className="mt-0.5 size-[18px] shrink-0 text-[#c2b8ff]" strokeWidth={1.8} aria-hidden />
            <p>
              You can’t change your own account here, and the last active super-admin can’t be demoted or disabled. A role change, disable, enable or
              password reset signs that operator out at once. Enabling clears the old password, so follow it with a reset. Temporary passwords are shown
              once and must be changed at first sign-in. Every change is audited.
            </p>
          </div>
        </>
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

      {action?.kind === 'reset-mfa' && (
        <ResetMfaDialog
          operator={action.op}
          pending={pending}
          error={actionError}
          onCancel={close}
          onConfirm={(reason) =>
            void run(() => resetOperatorTotp(action.op.id, reason), () => done(`${action.op.email} must set up two-factor authentication again.`))
          }
        />
      )}

      <TemporaryPasswordDialog credential={credential} onClose={() => setCredential(null)} />
    </div>
  )
}

/** Counts from the roster already loaded: no extra request, nothing inferred. */
function OperatorCounts({ operators }: { operators: OperatorView[] }) {
  const active = operators.filter((o) => o.disabled_at === null)
  const tiles = [
    { label: 'Super-admins', value: active.filter((o) => o.role === 'super-admin').length, note: 'Everything, including this page' },
    { label: 'Relay ops', value: active.filter((o) => o.role === 'relay-ops').length, note: 'Relays only' },
    { label: 'Need a password', value: active.filter((o) => !o.has_password || o.must_change_password).length, note: 'Must change it, or enabled without one' },
    { label: 'Disabled', value: operators.length - active.length, note: 'Can’t sign in' },
  ]
  return (
    <section aria-label="Operator counts" className="flex flex-wrap gap-4">
      {tiles.map((t) => (
        <Card key={t.label} className="flex flex-[1_1_200px] flex-col gap-1.5 px-[18px] py-4">
          <span className="text-xs font-medium tracking-wide text-muted-foreground">{t.label}</span>
          <span className="text-[26px] font-semibold">{t.value}</span>
          <span className="text-[13px] text-muted-foreground">{t.note}</span>
        </Card>
      ))}
    </section>
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
    <Card>
      <Table aria-label="Provider operators">
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>Operator</TableHead>
            <TableHead>Role</TableHead>
            <TableHead>Account</TableHead>
            <TableHead>Last sign-in</TableHead>
            <TableHead>Added</TableHead>
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
                <TableCell className="max-w-72">
                  <div className="flex items-center gap-3">
                    <span aria-hidden className="inline-flex size-8 shrink-0 items-center justify-center rounded-full bg-[#1f2533] text-xs font-semibold text-[#c9cfdb]">
                      {op.email.slice(0, 2).toUpperCase()}
                    </span>
                    <div className="flex min-w-0 flex-wrap items-center gap-2">
                      <span className="truncate font-medium">{op.email}</span>
                      {self && <span className="text-xs text-muted-foreground">(you)</span>}
                    </div>
                  </div>
                </TableCell>
                <TableCell>
                  <RoleBadge role={op.role} />
                </TableCell>
                <TableCell>
                  {/* Badges never break mid-label; a narrow screen scrolls the table instead. */}
                  <div className="flex gap-1.5 whitespace-nowrap">
                    {disabled ? <Badge variant="secondary">Disabled</Badge> : <Badge variant="success">Active</Badge>}
                    {!disabled && !op.has_password && <Badge variant="destructive">No password</Badge>}
                    {!disabled && op.has_password && op.must_change_password && (
                      <Badge variant="warning">Must change password</Badge>
                    )}
                    {op.totp_enabled && <Badge variant="info">MFA on</Badge>}
                  </div>
                </TableCell>
                <TableCell className="whitespace-nowrap text-subtle-foreground" title={formatDateTime(op.last_login_at)}>
                  {op.last_login_at ? formatRelative(op.last_login_at) : 'Never'}
                </TableCell>
                <TableCell className="whitespace-nowrap text-subtle-foreground" title={formatDateTime(op.created_at)}>
                  {formatDate(op.created_at)}
                </TableCell>
                <TableCell className="text-right">
                  {self ? (
                    <span className="whitespace-nowrap text-xs text-muted-foreground">Use the account menu</span>
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
                            {op.totp_enabled && (
                              <DropdownMenuItem onSelect={() => onAction({ kind: 'reset-mfa', op })}>Reset MFA</DropdownMenuItem>
                            )}
                            <DropdownMenuItem
                              className="text-critical-text focus:text-critical-text"
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
    </Card>
  )
}
