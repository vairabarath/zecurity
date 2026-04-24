import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useMutation, useQuery } from '@apollo/client/react'
import { ChevronRight, Clock3, Plus, ShieldOff, Trash2 } from 'lucide-react'
import {
  ConnectorStatus,
  DeleteConnectorDocument,
  GetConnectorsDocument,
  GetRemoteNetworkDocument,
  RevokeConnectorDocument,
} from '@/generated/graphql'
import type {
  DeleteConnectorMutationVariables,
  RevokeConnectorMutationVariables,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { InstallCommandModal } from '@/components/InstallCommandModal'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, EntityIcon, StatusPill, relativeTime } from '@/lib/console'

function statusTone(status: ConnectorStatus): 'ok' | 'warn' | 'danger' | 'muted' {
  if (status === ConnectorStatus.Active) return 'ok'
  if (status === ConnectorStatus.Disconnected) return 'warn'
  if (status === ConnectorStatus.Revoked) return 'danger'
  return 'muted'
}

export default function Connectors() {
  const { id } = useParams<{ id: string }>()
  const [showInstall, setShowInstall] = useState(false)

  const { data: networkData } = useQuery(GetRemoteNetworkDocument, {
    variables: { id: id! },
    skip: !id,
  })

  const { data, loading } = useQuery(GetConnectorsDocument, {
    variables: { remoteNetworkId: id! },
    skip: !id,
    pollInterval: 30000,
  })

  const [revokeConnector] = useMutation(RevokeConnectorDocument, {
    refetchQueries: [{ query: GetConnectorsDocument, variables: { remoteNetworkId: id! } }],
  })
  const [deleteConnector] = useMutation(DeleteConnectorDocument, {
    refetchQueries: [{ query: GetConnectorsDocument, variables: { remoteNetworkId: id! } }],
  })

  async function handleRevoke(connectorId: string) {
    await revokeConnector({ variables: { id: connectorId } as RevokeConnectorMutationVariables })
  }

  async function handleDelete(connectorId: string) {
    await deleteConnector({ variables: { id: connectorId } as DeleteConnectorMutationVariables })
  }

  const networkName = networkData?.remoteNetwork?.name ?? 'Network'
  const connectors = data?.connectors ?? []

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <Link to="/remote-networks" className="transition hover:text-foreground">Remote Networks</Link>
        <ChevronRight className="h-4 w-4" />
        <span>{networkName}</span>
        <ChevronRight className="h-4 w-4" />
        <span className="text-foreground">Connectors</span>
      </div>

      <div className="page-header">
        <div>
          <h2 className="page-title">Connectors</h2>
          <p className="page-subtitle">Connectors currently assigned to {networkName}.</p>
        </div>
        <Button onClick={() => setShowInstall(true)} className="gap-2">
          <Plus className="h-4 w-4" />
          Add Connector
        </Button>
      </div>

      {id ? (
        <InstallCommandModal remoteNetworkId={id} open={showInstall} onClose={() => setShowInstall(false)} />
      ) : null}

      <div className="table-shell">
        <div className="table-scroll">
          <div className="table-head grid min-w-[930px] grid-cols-[1.2fr_130px_130px_180px_110px_120px_160px] gap-4 px-5 py-3">
            {['Name', 'Status', 'Last Seen', 'Hostname', 'Version', 'Public IP', 'Actions'].map((label, index) => (
              <div key={label + index} className={`table-head-label ${index === 6 ? 'text-right' : ''}`}>{label}</div>
            ))}
          </div>

          {loading && !data ? (
            <div className="min-w-[930px] p-5 space-y-3">
              {Array.from({ length: 4 }).map((_, index) => (
                <Skeleton key={index} className="h-14 rounded-2xl bg-secondary" />
              ))}
            </div>
          ) : connectors.length === 0 ? (
            <EmptyState
              title="No connectors for this network"
              description="Generate the first install command to start receiving heartbeat traffic."
              action={<Button onClick={() => setShowInstall(true)}>Add Connector</Button>}
            />
          ) : (
            <div className="min-w-[930px]">
              {connectors.map((connector) => {
                const canRevoke = connector.status === ConnectorStatus.Active || connector.status === ConnectorStatus.Disconnected
                const canDelete = connector.status === ConnectorStatus.Pending || connector.status === ConnectorStatus.Revoked

                return (
                  <div key={connector.id} className="admin-table-row grid grid-cols-[1.2fr_130px_130px_180px_110px_120px_160px] gap-4 px-5 py-4">
                    <div className="flex min-w-0 items-center gap-3">
                      <EntityIcon type="connector" />
                      <div className="min-w-0">
                        <div className="truncate text-sm font-semibold">{connector.name}</div>
                        <div className="truncate text-xs text-muted-foreground">{connector.lanAddr ?? 'LAN unavailable'}</div>
                      </div>
                    </div>
                    <div><StatusPill label={connector.status.toLowerCase()} tone={statusTone(connector.status)} /></div>
                    <div className="text-sm text-muted-foreground">
                      <span className="inline-flex items-center gap-1">
                        <Clock3 className="h-3.5 w-3.5" />
                        {relativeTime(connector.lastSeenAt)}
                      </span>
                    </div>
                    <div className="truncate text-sm text-muted-foreground">{connector.hostname ?? '—'}</div>
                    <div className="text-sm text-muted-foreground">{connector.version ?? '—'}</div>
                    <div className="text-sm text-muted-foreground">{connector.publicIp ?? '—'}</div>
                    <div className="flex items-center justify-end gap-2">
                      <Link to={`/connectors/${connector.id}`} className="text-sm font-semibold text-primary">Manage</Link>
                      {canRevoke ? (
                        <button
                          onClick={() => handleRevoke(connector.id)}
                          className="rounded-xl border border-[oklch(0.85_0.13_80/0.28)] bg-[oklch(0.85_0.13_80/0.12)] p-2 text-[oklch(0.85_0.13_80)]"
                        >
                          <ShieldOff className="h-4 w-4" />
                        </button>
                      ) : null}
                      {canDelete ? (
                        <button
                          onClick={() => handleDelete(connector.id)}
                          className="rounded-xl border border-[oklch(0.75_0.16_25/0.28)] bg-[oklch(0.75_0.16_25/0.12)] p-2 text-[oklch(0.75_0.16_25)]"
                        >
                          <Trash2 className="h-4 w-4" />
                        </button>
                      ) : null}
                    </div>
                  </div>
                )
              })}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
