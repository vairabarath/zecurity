import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery, useMutation } from '@apollo/client/react'
import {
  GetRemoteNetworksDocument,
  CreateRemoteNetworkDocument,
  DeleteRemoteNetworkDocument,
  NetworkLocation,
  RemoteNetworkStatus,
} from '@/generated/graphql'
import type {
  CreateRemoteNetworkMutationVariables,
  DeleteRemoteNetworkMutationVariables,
} from '@/generated/graphql'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import {
  Plus,
  Network,
  Trash2,
  ArrowRight,
  Home,
  Building2,
  Cloud,
  MapPin,
  ChevronDown,
  X,
} from 'lucide-react'
import { cn } from '@/lib/utils'

const locationConfig: Record<NetworkLocation, { label: string; icon: typeof Home; color: string }> = {
  [NetworkLocation.Home]: { label: 'Home', icon: Home, color: 'text-blue-400 bg-blue-400/10 border-blue-400/20' },
  [NetworkLocation.Office]: { label: 'Office', icon: Building2, color: 'text-violet-400 bg-violet-400/10 border-violet-400/20' },
  [NetworkLocation.Aws]: { label: 'AWS', icon: Cloud, color: 'text-amber-400 bg-amber-400/10 border-amber-400/20' },
  [NetworkLocation.Gcp]: { label: 'GCP', icon: Cloud, color: 'text-sky-400 bg-sky-400/10 border-sky-400/20' },
  [NetworkLocation.Azure]: { label: 'Azure', icon: Cloud, color: 'text-cyan-400 bg-cyan-400/10 border-cyan-400/20' },
  [NetworkLocation.Other]: { label: 'Other', icon: MapPin, color: 'text-gray-400 bg-gray-400/10 border-gray-400/20' },
}

export default function RemoteNetworks() {
  const [showAdd, setShowAdd] = useState(false)
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

  async function handleCreate() {
    if (!name.trim()) return
    await createNetwork({
      variables: { name: name.trim(), location } as CreateRemoteNetworkMutationVariables,
    })
    setName('')
    setLocation(NetworkLocation.Office)
    setShowAdd(false)
  }

  async function handleDelete(id: string) {
    await deleteNetwork({
      variables: { id } as DeleteRemoteNetworkMutationVariables,
    })
  }

  const networks = data?.remoteNetworks ?? []
  const selectedLoc = locationConfig[location]

  return (
    <div className="space-y-6">
      {/* Page Header */}
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-display font-bold tracking-tight">Remote Networks</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Manage your network locations and connected infrastructure.
          </p>
        </div>
        <Button
          onClick={() => setShowAdd(!showAdd)}
          className="gap-2"
          variant={showAdd ? 'outline' : 'default'}
        >
          {showAdd ? <X className="w-4 h-4" /> : <Plus className="w-4 h-4" />}
          {showAdd ? 'Cancel' : 'Add Network'}
        </Button>
      </div>

      {/* Add Network Panel */}
      {showAdd && (
        <Card className="border-primary/20 bg-card/80 backdrop-blur-sm animate-fade-up">
          <CardHeader className="pb-3">
            <CardTitle className="text-base font-display">New Remote Network</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="flex items-end gap-3">
              <div className="flex-1 space-y-1.5">
                <label className="text-xs text-muted-foreground font-mono uppercase tracking-wider">
                  Network Name
                </label>
                <Input
                  placeholder="e.g. Production VPC"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  onKeyDown={(e) => e.key === 'Enter' && handleCreate()}
                  className="bg-background/50"
                />
              </div>
              <div className="space-y-1.5">
                <label className="text-xs text-muted-foreground font-mono uppercase tracking-wider">
                  Location
                </label>
                <DropdownMenu>
                  <DropdownMenuTrigger asChild>
                    <Button variant="outline" className="gap-2 min-w-[140px] justify-between">
                      <span className="flex items-center gap-2">
                        <selectedLoc.icon className="w-3.5 h-3.5" />
                        {selectedLoc.label}
                      </span>
                      <ChevronDown className="w-3 h-3 text-muted-foreground" />
                    </Button>
                  </DropdownMenuTrigger>
                  <DropdownMenuContent>
                    {Object.entries(locationConfig).map(([key, cfg]) => (
                      <DropdownMenuItem
                        key={key}
                        onClick={() => setLocation(key as NetworkLocation)}
                        className="gap-2"
                      >
                        <cfg.icon className="w-3.5 h-3.5" />
                        {cfg.label}
                      </DropdownMenuItem>
                    ))}
                  </DropdownMenuContent>
                </DropdownMenu>
              </div>
              <Button onClick={handleCreate} disabled={!name.trim() || creating} className="gap-2">
                {creating ? 'Creating...' : 'Create'}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Loading Skeletons */}
      {loading && !data && (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          {Array.from({ length: 6 }).map((_, i) => (
            <Card key={i} className="bg-card/60">
              <CardContent className="p-5 space-y-3">
                <Skeleton className="h-5 w-3/4" />
                <Skeleton className="h-4 w-1/2" />
                <div className="flex gap-2">
                  <Skeleton className="h-5 w-16" />
                  <Skeleton className="h-5 w-20" />
                </div>
                <Skeleton className="h-8 w-full mt-2" />
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      {/* Empty State */}
      {!loading && networks.length === 0 && (
        <div className="flex flex-col items-center justify-center py-20 text-center">
          <div className="rounded-full p-4 bg-primary/5 border border-primary/10 mb-4">
            <Network className="w-8 h-8 text-primary/40" />
          </div>
          <h3 className="text-lg font-display font-semibold text-foreground/80">No remote networks</h3>
          <p className="text-sm text-muted-foreground mt-1 max-w-sm">
            Create your first remote network to start connecting your infrastructure.
          </p>
        </div>
      )}

      {/* Network Cards */}
      {networks.length > 0 && (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          {networks.map((network, i) => {
            const loc = locationConfig[network.location]
            const connectorCount = network.connectors.length
            const isActive = network.status === RemoteNetworkStatus.Active

            return (
              <Card
                key={network.id}
                className={cn(
                  'group relative bg-card/60 backdrop-blur-sm border-border/50 transition-all duration-300 hover:border-primary/20 hover:scale-[1.01]',
                )}
                style={{ animationDelay: `${i * 80}ms` }}
              >
                <CardContent className="p-5 space-y-4">
                  {/* Name + Status */}
                  <div className="flex items-start justify-between">
                    <div className="space-y-1 min-w-0">
                      <h3 className="font-display font-semibold text-base truncate">{network.name}</h3>
                      <div className="flex items-center gap-2">
                        <Badge
                          variant="outline"
                          className={cn(
                            'text-[10px] font-mono border',
                            loc.color,
                          )}
                        >
                          <loc.icon className="w-3 h-3 mr-1" />
                          {loc.label}
                        </Badge>
                        <Badge
                          variant="outline"
                          className={cn(
                            'text-[10px] font-mono',
                            isActive
                              ? 'text-emerald-400 bg-emerald-400/10 border-emerald-400/20'
                              : 'text-gray-400 bg-gray-400/10 border-gray-400/20',
                          )}
                        >
                          {isActive ? 'Active' : 'Inactive'}
                        </Badge>
                      </div>
                    </div>
                  </div>

                  {/* Connector count */}
                  <div className="flex items-center justify-between text-sm">
                    <span className="text-muted-foreground">
                      {connectorCount} connector{connectorCount !== 1 ? 's' : ''}
                    </span>
                  </div>

                  {/* Actions */}
                  <div className="flex items-center gap-2 pt-1 border-t border-border/30">
                    <Link
                      to={`/remote-networks/${network.id}/connectors`}
                      className="flex-1"
                    >
                      <Button variant="outline" size="sm" className="w-full gap-2 text-xs group/btn">
                        View Connectors
                        <ArrowRight className="w-3 h-3 transition-transform group-hover/btn:translate-x-0.5" />
                      </Button>
                    </Link>
                    {connectorCount === 0 && (
                      <Button
                        variant="outline"
                        size="sm"
                        className="text-destructive hover:text-destructive hover:bg-destructive/10 border-destructive/20"
                        onClick={() => handleDelete(network.id)}
                      >
                        <Trash2 className="w-3.5 h-3.5" />
                      </Button>
                    )}
                  </div>
                </CardContent>
              </Card>
            )
          })}
        </div>
      )}
    </div>
  )
}
