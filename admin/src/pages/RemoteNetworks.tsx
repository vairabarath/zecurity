import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useMutation, useQuery } from '@apollo/client/react'
import {
  ArrowRight,
  Building2,
  ChevronDown,
  Cloud,
  Home,
  MapPin,
  Plus,
  Trash2,
} from 'lucide-react'
import {
  ConnectorStatus,
  CreateRemoteNetworkDocument,
  DeleteRemoteNetworkDocument,
  GetRemoteNetworksDocument,
  NetworkHealth,
  NetworkLocation,
  RemoteNetworkStatus,
  ShieldStatus,
} from '@/generated/graphql'
import type {
  CreateRemoteNetworkMutationVariables,
  DeleteRemoteNetworkMutationVariables,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, EntityIcon, StatusPill } from '@/lib/console'

const locationConfig: Record<NetworkLocation, { label: string; icon: typeof Home }> = {
  [NetworkLocation.Home]: { label: 'Home', icon: Home },
  [NetworkLocation.Office]: { label: 'Office', icon: Building2 },
  [NetworkLocation.Aws]: { label: 'AWS', icon: Cloud },
  [NetworkLocation.Gcp]: { label: 'GCP', icon: Cloud },
  [NetworkLocation.Azure]: { label: 'Azure', icon: Cloud },
  [NetworkLocation.Other]: { label: 'Other', icon: MapPin },
}

const healthTone: Record<NetworkHealth, 'ok' | 'warn' | 'danger'> = {
  [NetworkHealth.Online]: 'ok',
  [NetworkHealth.Degraded]: 'warn',
  [NetworkHealth.Offline]: 'danger',
}

export default function RemoteNetworks() {
  const [showComposer, setShowComposer] = useState(false)
  const [name, setName] = useState('')
  const [location, setLocation] = useState<NetworkLocation>(NetworkLocation.Office)

  const { data, loading } = useQuery(GetRemoteNetworksDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 30000,
  })

  const [createNetwork, { loading: creating }] = useMutation(CreateRemoteNetworkDocument, {
    refetchQueries: [{ query: GetRemoteNetworksDocument }],
  })
  const [deleteNetwork] = useMutation(DeleteRemoteNetworkDocument, {
    refetchQueries: [{ query: GetRemoteNetworksDocument }],
  })

  const networks = data?.remoteNetworks ?? []
  const selectedLocation = locationConfig[location]

  async function handleCreate() {
    if (!name.trim()) return
    await createNetwork({
      variables: { name: name.trim(), location } as CreateRemoteNetworkMutationVariables,
    })
    setName('')
    setLocation(NetworkLocation.Office)
    setShowComposer(false)
  }

  async function handleDelete(id: string) {
    if (!window.confirm('Delete this remote network?')) return
    await deleteNetwork({
      variables: { id } as DeleteRemoteNetworkMutationVariables,
    })
  }

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Remote Networks</h2>
          <p className="page-subtitle">Segmented locations, connector coverage, and shield posture.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="metric-chip"><strong>{networks.length}</strong> total</span>
          <Button
            onClick={() => setShowComposer((open) => !open)}
            variant={showComposer ? 'outline' : 'default'}
            className="gap-2"
          >
            <Plus className="h-4 w-4" />
            {showComposer ? 'Close' : 'Add Network'}
          </Button>
        </div>
      </div>

      {showComposer ? (
        <div className="surface-card p-5">
          <div className="mb-4">
            <div className="text-sm font-semibold">Create Remote Network</div>
            <div className="mt-1 text-xs text-muted-foreground">Start a new edge segment and enroll connectors against it.</div>
          </div>
          <div className="grid gap-3 md:grid-cols-[minmax(0,1fr)_180px_auto]">
            <Input
              value={name}
              onChange={(event) => setName(event.target.value)}
              onKeyDown={(event) => event.key === 'Enter' && handleCreate()}
              placeholder="Production VPC"
              className="h-11 rounded-xl border-border bg-secondary px-4"
            />

            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="outline" className="h-11 justify-between rounded-xl bg-secondary">
                  <span className="flex items-center gap-2">
                    <selectedLocation.icon className="h-4 w-4" />
                    {selectedLocation.label}
                  </span>
                  <ChevronDown className="h-4 w-4" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent className="w-48">
                {Object.entries(locationConfig).map(([key, config]) => (
                  <DropdownMenuItem
                    key={key}
                    onClick={() => setLocation(key as NetworkLocation)}
                    className="cursor-pointer gap-2"
                  >
                    <config.icon className="h-4 w-4" />
                    {config.label}
                  </DropdownMenuItem>
                ))}
              </DropdownMenuContent>
            </DropdownMenu>

            <Button onClick={handleCreate} disabled={!name.trim() || creating} className="h-11 rounded-xl">
              {creating ? 'Creating...' : 'Create'}
            </Button>
          </div>
        </div>
      ) : null}

      {loading && !data ? (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {Array.from({ length: 6 }).map((_, index) => (
            <div key={index} className="surface-card p-5">
              <Skeleton className="h-6 w-40 bg-secondary" />
              <Skeleton className="mt-3 h-4 w-24 bg-secondary" />
              <Skeleton className="mt-6 h-24 w-full rounded-2xl bg-secondary" />
            </div>
          ))}
        </div>
      ) : networks.length === 0 ? (
        <div className="surface-card">
          <EmptyState
            title="No remote networks yet"
            description="Create the first network to start enrolling connectors and shields."
            action={<Button onClick={() => setShowComposer(true)}>Add Network</Button>}
          />
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {networks.map((network) => {
            const locationMeta = locationConfig[network.location]
            const activeConnectors = network.connectors.filter((connector) => connector.status === ConnectorStatus.Active).length
            const activeShields = network.shields.filter((shield) => shield.status === ShieldStatus.Active).length
            const canDelete = network.connectors.length === 0 && network.shields.length === 0
            const isActive = network.status === RemoteNetworkStatus.Active

            return (
              <div key={network.id} className="surface-card flex flex-col p-5">
                <div className="flex items-start justify-between gap-3">
                  <div className="min-w-0">
                    <div className="flex items-center gap-3">
                      <EntityIcon type="network" />
                      <div className="min-w-0">
                        <div className="truncate text-base font-semibold">{network.name}</div>
                        <div className="mt-1 flex flex-wrap items-center gap-2">
                          <StatusPill label={locationMeta.label} tone="info" />
                          <StatusPill label={network.networkHealth.toLowerCase()} tone={healthTone[network.networkHealth]} />
                          <StatusPill label={isActive ? 'active' : 'inactive'} tone={isActive ? 'ok' : 'muted'} />
                        </div>
                      </div>
                    </div>
                  </div>

                  <button
                    onClick={() => handleDelete(network.id)}
                    disabled={!canDelete}
                    className="rounded-xl border border-border bg-secondary p-2 text-muted-foreground transition hover:text-destructive disabled:cursor-not-allowed disabled:opacity-40"
                    title={canDelete ? 'Delete network' : 'Delete disabled while connectors or shields still exist'}
                  >
                    <Trash2 className="h-4 w-4" />
                  </button>
                </div>

                <div className="mt-5 grid gap-3 sm:grid-cols-2">
                  <div className="section-card p-4">
                    <div className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">Connectors</div>
                    <div className="mt-2 text-lg font-semibold">{activeConnectors} / {network.connectors.length}</div>
                    <div className="mt-1 text-xs text-muted-foreground">active coverage</div>
                  </div>
                  <div className="section-card p-4">
                    <div className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">Shields</div>
                    <div className="mt-2 text-lg font-semibold">{activeShields} / {network.shields.length}</div>
                    <div className="mt-1 text-xs text-muted-foreground">host agents online</div>
                  </div>
                </div>

                <div className="mt-4 section-card p-4 text-sm">
                  <div className="font-semibold">Topology</div>
                  <div className="mt-2 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                    <span>{locationMeta.label} segment</span>
                    <span>•</span>
                    <span>{network.connectors.length} connectors</span>
                    <span>•</span>
                    <span>{network.shields.length} shields</span>
                  </div>
                </div>

                <div className="mt-5 flex flex-wrap items-center gap-3">
                  <Link
                    to={`/remote-networks/${network.id}`}
                    className="inline-flex items-center gap-2 text-sm font-semibold text-primary"
                  >
                    Open topology
                    <ArrowRight className="h-4 w-4" />
                  </Link>
                  <Link to={`/remote-networks/${network.id}/connectors`} className="text-sm text-muted-foreground transition hover:text-foreground">
                    Connectors
                  </Link>
                  <Link to={`/remote-networks/${network.id}/shields`} className="text-sm text-muted-foreground transition hover:text-foreground">
                    Shields
                  </Link>
                </div>
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}
