import { useNavigate, useParams } from 'react-router-dom'
import { useState } from 'react'
import { useQuery, useMutation } from '@apollo/client/react'
import { ArrowLeft, KeyRound } from 'lucide-react'
import {
  GetIdpConnectionsDocument,
  DeleteIdpConnectionDocument,
  SetIdpConnectionStatusDocument,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, ErrorState, StatusPill } from '@/lib/console'
import { ScimConfigCard, type ScimConfigConnection } from '@/components/scim/ScimConfigCard'
import { ScimBaseUrlBox } from '@/components/scim/ScimBaseUrlBox'
import { ScimTokenPanel } from '@/components/scim/ScimTokenPanel'
import { IdentityHealthBadge } from '@/components/scim/IdentityHealthBadge'

type IdpConnectionDetailData = ScimConfigConnection & {
  protocol: string
  issuer: string
  status: string
  identityHealth: string
  lastSyncAt?: string | null
  scimEnabled: boolean
}

export default function IdpConnectionDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()

  // There is no single-connection query in the schema — idpConnections returns
  // the workspace's full (small) list, so we select from it rather than adding a
  // backend field this phase does not own.
  const { data, loading, error, refetch } = useQuery(GetIdpConnectionsDocument, {
    fetchPolicy: 'cache-and-network',
  })

  // Every hook must run on every render — the loading / error / not-found
  // early returns below sit *after* this block for exactly that reason.
  const [deleteRefusal, setDeleteRefusal] = useState<string | null>(null)
  const [deletePending, setDeletePending] = useState<boolean>(false)

  const [deleteConnection] = useMutation(DeleteIdpConnectionDocument, {
    refetchQueries: [{ query: GetIdpConnectionsDocument }],
  })
  const [setConnectionStatus, { loading: statusPending }] = useMutation(
    SetIdpConnectionStatusDocument,
    { refetchQueries: [{ query: GetIdpConnectionsDocument }] },
  )

  const connection = (data?.idpConnections ?? []).find(
    (c) => c.id === id,
  ) as IdpConnectionDetailData | undefined

  async function attemptDelete(force: boolean) {
    if (!connection) return
    setDeletePending(true)
    try {
      const res = await deleteConnection({
        variables: { id: connection.id, force },
      })
      // errorPolicy may surface a refusal on the result rather than by
      // rejecting, depending on the client config — handle both shapes.
      if (res?.error) {
        setDeleteRefusal(res.error.message)
        return
      }
      // Success — navigate away; the deleted connection will not appear in the
      // admin list because ListWorkspaceConnections already filters status='deleted'.
      navigate('/idp-connections')
    } catch (err: unknown) {
      // Apollo UserError carries the server's verbatim refusal; show it in a
      // confirmation dialog so the user can decide to force-delete.
      // Apollo Client 4 exposes the GraphQL errors on `.errors`
      // (CombinedGraphQLErrors); `.graphQLErrors` is the removed v3 field and is
      // kept only as a fallback. The err.message fallback below already covered
      // this, so the symptom was invisible — the branch was simply dead.
      let msg = ''
      const gqlErrors =
        (err as { errors?: unknown[] })?.errors ??
        (err as { graphQLErrors?: unknown[] })?.graphQLErrors
      const gqe = gqlErrors?.[0]
      if (gqe && typeof gqe === 'object' && 'message' in gqe) {
        msg = String((gqe as { message: unknown }).message)
      }
      if (!msg && err && typeof err === 'object' && 'message' in err) {
        msg = String((err as { message: unknown }).message)
      }
      setDeleteRefusal(msg || 'Server refused to delete the connection')
    } finally {
      setDeletePending(false)
    }
  }

  // Both directions go through the same mutation; the backend only accepts
  // 'active' or 'disabled' (terminal deletion is a separate guarded mutation).
  async function handleSetStatus(status: 'active' | 'disabled') {
    if (!connection) return
    await setConnectionStatus({
      variables: { id: connection.id, status },
    })
  }

  if (loading && !connection) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-10 w-64" />
        <Skeleton className="h-48 w-full" />
        <Skeleton className="h-32 w-full" />
      </div>
    )
  }

  if (error && !connection) {
    return (
      <ErrorState
        title="Could not load connection"
        description={error.message}
        action={
          <Button variant="outline" onClick={() => void refetch()}>
            Retry
          </Button>
        }
      />
    )
  }

  if (!connection) {
    return (
      <EmptyState
        icon={<KeyRound className="h-6 w-6" />}
        title="Connection not found"
        description="This identity-provider connection does not exist, or you no longer have access to it."
        action={
          <Button variant="outline" onClick={() => navigate('/idp-connections')}>
            Back to Identity Providers
          </Button>
        }
      />
    )
  }

  // Derived from the cache-normalised entity rather than held in local state,
  // so the button reflects the server's view after the mutation lands.
  //
  // 'deleted' is terminal and NOT re-enableable — setIdpConnectionStatus only
  // accepts 'active' or 'disabled', so a soft-deleted connection must never be
  // offered an Enable button (nor described as merely "disabled").
  const isDeleted = connection.status === 'deleted'
  const isDisabled = connection.status !== 'active' && !isDeleted

  return (
    <div className="space-y-6">
      <div>
        <button
          onClick={() => navigate('/idp-connections')}
          className="mb-3 inline-flex items-center gap-1.5 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
        >
          <ArrowLeft className="h-3.5 w-3.5" />
          Identity Providers
        </button>

        <div className="page-header">
          <div className="min-w-0">
            <h2 className="page-title truncate">{connection!.displayName}</h2>
            <p className="page-subtitle truncate">
              {connection!.provider} · {connection!.protocol} · {connection!.issuer}
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <StatusPill
              label={connection!.status}
              tone={connection!.status === 'active' ? 'ok' : 'muted'}
            />
            {connection!.managed ? (
              <span className="status-pill border-border bg-secondary text-muted-foreground">
                platform-managed
              </span>
            ) : null}
            <IdentityHealthBadge
              identityHealth={connection!.identityHealth}
              lastSyncAt={connection!.lastSyncAt}
              scimEnabled={connection!.scimEnabled}
            />
          </div>
        </div>
      </div>

      <ScimConfigCard connection={connection!} onChanged={() => void refetch()} />
      <ScimBaseUrlBox />
      <ScimTokenPanel connectionId={connection!.id} />
      {connection!.scimEnabled ? (
        <div>
          <Button variant="outline" onClick={() => navigate(`/scim-conflicts?connectionId=${connection!.id}`)}>
            View provisioning conflicts
          </Button>
        </div>
      ) : null}

      {/* ---------------------------------------------------------------
          Danger Zone — only shown for workspace (non-managed) connections.
          Platform/managed connections are provider-managed and must not expose
          destructive controls.
          --------------------------------------------------------------- */}
      {connection!.managed ? null : (
        <div className="border-t border-border/60 pt-4">
          <h3 className="text-sm font-medium text-muted-foreground mb-3">
            Danger Zone
          </h3>

          {/* Enable / disable connection. Terminal 'deleted' offers neither. */}
          {!isDeleted && (
            <div className="space-y-2">
              <label className="text-xs font-medium text-muted-foreground">
                {isDisabled ? 'Enable connection' : 'Disable connection'}
              </label>
              <Button
                variant="outline"
                onClick={() => void handleSetStatus(isDisabled ? 'active' : 'disabled')}
                disabled={statusPending}
              >
                {statusPending
                  ? 'Working…'
                  : isDisabled
                    ? 'Enable'
                    : 'Disable'}
              </Button>
              {isDisabled && (
                <p className="text-xs text-muted-foreground">
                  Connection is disabled. Sessions revoked; SCIM-managed users suspended.
                  Re-enabling restores the login path only — affected users stay suspended
                  and <code>provisioning_owner=&apos;unmanaged&apos;</code>, so the directory&apos;s
                  next push raises one entry per user in Provisioning conflicts. Accept each
                  (requires <code>identity.mapping.break_glass</code>) to return them to SCIM
                  control.
                </p>
              )}
            </div>
          )}

          {/* Delete connection */}
          <div className="space-y-2">
            {connection!.status === 'deleted' ? (
              <p className="text-xs text-destructive">
                This connection has been soft-deleted (status='deleted').
              </p>
            ) : (
              <Button
                variant="destructive"
                onClick={() => void attemptDelete(false)}
                disabled={deletePending}
              >
                Delete
              </Button>
            )}
          </div>

          {/* Delete confirmation dialog — shows verbatim server refusal */}
          {deleteRefusal && !deletePending && (
            <div className="space-y-3 pt-3" role="dialog" aria-modal="true">
              <p className="text-sm text-muted-foreground">
                The server refused to delete this connection:{' '}
                <strong>{deleteRefusal}</strong>
              </p>
              <div className="flex gap-2">
                <Button
                  variant="outline"
                  onClick={() => setDeleteRefusal(null)}
                  disabled={deletePending}
                >
                  Cancel
                </Button>
                <Button
                  variant="destructive"
                  onClick={() => void attemptDelete(true)}
                  disabled={deletePending}
                >
                  Force delete anyway
                </Button>
              </div>
            </div>
          )}

          {deletePending && (
            <p className="text-xs text-muted-foreground">
              Deleting… connection will no longer appear in the admin list.
            </p>
          )}
        </div>
      )}

      {loading ? (
        <div className="space-y-4">
          <Skeleton className="h-10 w-64" />
          <Skeleton className="h-48 w-full" />
          <Skeleton className="h-32 w-full" />
        </div>
      ) : null}
    </div>
  )
}