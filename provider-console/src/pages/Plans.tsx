import { useState } from 'react'
import { Plus, RefreshCw } from 'lucide-react'
import { Navigate } from 'react-router-dom'
import { listPlans } from '@/api/reads'
import { PLAN_LIMIT_KEYS, type PlanSummary } from '@/api/types'
import { PageHeader } from '@/components/PageHeader'
import { CreatePlanDialog, DeletePlanDialog, EditPlanDialog, MakeDefaultPlanDialog } from '@/components/PlanDialogs'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatLimit, PLAN_LIMIT_LABELS } from '@/lib/plan-format'
import { useDetailQuery } from '@/lib/useDetailQuery'

// Plans (Sprint 23 B1, D-43; super-admin only). A plan is a name and a
// limit for each of the six quota keys (a number, or unlimited); no prices.
// Every new workspace starts on the default plan. Limits are enforced by
// quotas (S3); until then they are shown, not enforced.

const fetchPlans = () => listPlans()

type Action = { kind: 'edit' | 'default' | 'delete'; plan: PlanSummary }

export function Plans() {
  const { state, reload } = useDetailQuery('plans', fetchPlans)
  const [creating, setCreating] = useState(false)
  const [action, setAction] = useState<Action | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  const done = (text: string) => {
    setAction(null)
    setNotice(text)
    reload()
  }

  let body
  switch (state.status) {
    case 'loading':
      body = (
        <div aria-busy="true" aria-label="Loading plans" className="flex flex-col gap-2">
          <Skeleton className="h-8 w-full" />
          <Skeleton className="h-8 w-full" />
        </div>
      )
      break
    case 'session_ended':
      return null
    case 'forbidden':
      return <Navigate to="/forbidden" replace />
    case 'not_found':
    case 'error':
      body = (
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
            <span>Couldn’t load plans.</span>
            <Button variant="outline" size="sm" onClick={reload}>
              Retry
            </Button>
          </AlertDescription>
        </Alert>
      )
      break
    case 'ready':
      body = <PlanTable plans={state.data.items} onAction={setAction} />
      break
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Plans"
        description="Each tenant is on one plan. A plan sets the most users, resources, remote networks, connectors, shields and client devices a tenant may have. New workspaces start on the default plan."
        actions={
          <>
            <Button
              variant="outline"
              onClick={() => {
                setNotice(null)
                reload()
              }}
              aria-label="Refresh plans"
            >
              <RefreshCw aria-hidden />
              Refresh
            </Button>
            <Button onClick={() => setCreating(true)}>
              <Plus aria-hidden />
              Create plan
            </Button>
          </>
        }
      />
      {notice && (
        <Alert role="status">
          <AlertDescription>{notice}</AlertDescription>
        </Alert>
      )}
      {body}
      {creating && (
        <CreatePlanDialog
          onCancel={() => setCreating(false)}
          onCreated={(p) => {
            setCreating(false)
            setNotice(`Plan ${p.name} created. Assign it to tenants from their tenant page.`)
            reload()
          }}
        />
      )}
      {action?.kind === 'edit' && <EditPlanDialog plan={action.plan} onCancel={() => setAction(null)} onDone={done} />}
      {action?.kind === 'default' && <MakeDefaultPlanDialog plan={action.plan} onCancel={() => setAction(null)} onDone={done} />}
      {action?.kind === 'delete' && <DeletePlanDialog plan={action.plan} onCancel={() => setAction(null)} onDone={done} />}
    </div>
  )
}

function PlanTable({ plans, onAction }: { plans: PlanSummary[]; onAction: (a: Action) => void }) {
  if (plans.length === 0) return <p className="py-6 text-sm text-muted-foreground">No plans.</p>
  return (
    <Card>
      <Table aria-label="Plans">
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>Plan</TableHead>
            {PLAN_LIMIT_KEYS.map((k) => (
              <TableHead key={k} className="text-right">
                {PLAN_LIMIT_LABELS[k]}
              </TableHead>
            ))}
            <TableHead className="text-right">Tenants</TableHead>
            <TableHead className="text-right">
              <span className="sr-only">Actions</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {plans.map((p) => (
            <TableRow key={p.id} aria-label={p.name}>
              <TableCell>
                <span className="inline-flex flex-col gap-0.5">
                  <span className="inline-flex flex-wrap items-center gap-2 font-medium">
                    {p.name}
                    {p.is_default && (
                      <Badge variant="secondary" title="New workspaces start on this plan">
                        Default
                      </Badge>
                    )}
                  </span>
                  {p.description && <span className="text-[13px] text-muted-foreground">{p.description}</span>}
                </span>
              </TableCell>
              {PLAN_LIMIT_KEYS.map((k) => (
                <TableCell key={k} className="text-right font-mono text-[13px] tabular-nums" data-limit={k}>
                  {formatLimit(p.limits[k])}
                </TableCell>
              ))}
              <TableCell className="text-right font-mono text-[13px] tabular-nums">{p.tenants}</TableCell>
              <TableCell className="text-right">
                <span className="inline-flex flex-wrap justify-end gap-2">
                  <Button variant="outline" size="sm" onClick={() => onAction({ kind: 'edit', plan: p })} aria-label={`Edit ${p.name}`}>
                    Edit
                  </Button>
                  {!p.is_default && (
                    <Button variant="outline" size="sm" onClick={() => onAction({ kind: 'default', plan: p })} aria-label={`Make ${p.name} the default`}>
                      Make default
                    </Button>
                  )}
                  {!p.is_default && (
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => onAction({ kind: 'delete', plan: p })}
                      aria-label={`Delete ${p.name}`}
                      disabled={p.tenants > 0}
                      title={p.tenants > 0 ? 'Move its tenants to another plan first' : undefined}
                    >
                      Delete
                    </Button>
                  )}
                </span>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Card>
  )
}
