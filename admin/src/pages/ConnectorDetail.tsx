import { useEffect, useRef, useState } from 'react'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { useQuery, useMutation } from '@apollo/client/react'
import { motion } from 'framer-motion'
import {
  GetRemoteNetworksDocument,
  RevokeConnectorDocument,
  DeleteConnectorDocument,
  ConnectorStatus,
} from '@/generated/graphql'
import type {
  RevokeConnectorMutationVariables,
  DeleteConnectorMutationVariables,
} from '@/generated/graphql'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from '@/components/ui/card'
import { useAuthStore } from '@/store/auth'
import { cn } from '@/lib/utils'
import {
  ArrowLeft,
  Server,
  Terminal,
  ShieldOff,
  Trash2,
  CircleDot,
  CircleDotDashed,
  Ban,
  AlertTriangle,
  CheckCircle,
  ChevronRight,
  Copy,
  RefreshCw,
  Loader2,
  Globe,
  Cpu,
  Clock,
  Calendar,
  Wifi,
  Network,
} from 'lucide-react'

function relativeTime(dateStr: string | null | undefined): string {
  if (!dateStr) return 'Never'
  const diff = Date.now() - new Date(dateStr).getTime()
  if (diff < 0) return 'just now'
  const s = Math.floor(diff / 1000)
  if (s < 60) return `${s}s ago`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d}d ago`
  return `${Math.floor(d / 30)}mo ago`
}

const statusConfig = {
  [ConnectorStatus.Active]: {
    label: 'Active',
    className: 'text-emerald-600 bg-emerald-500/10 border-emerald-500/20',
    icon: <CircleDot className="h-3 w-3 fill-emerald-500 text-emerald-500" />,
  },
  [ConnectorStatus.Disconnected]: {
    label: 'Disconnected',
    className: 'text-amber-600 bg-amber-500/10 border-amber-500/20',
    icon: <CircleDotDashed className="h-3 w-3 fill-amber-500 text-amber-500" />,
  },
  [ConnectorStatus.Pending]: {
    label: 'Pending',
    className: 'text-gray-600 bg-gray-500/10 border-gray-500/20',
    icon: <CircleDotDashed className="h-3 w-3 fill-gray-400 text-gray-400" />,
  },
  [ConnectorStatus.Revoked]: {
    label: 'Revoked',
    className: 'text-red-600 bg-red-500/10 border-red-500/20',
    icon: <Ban className="h-3 w-3 text-red-500" />,
  },
}

interface InfoRowProps {
  icon: React.ReactNode
  label: string
  value: React.ReactNode
}

function InfoRow({ icon, label, value }: InfoRowProps) {
  return (
    <div className="flex flex-col space-y-1.5">
      <div className="flex items-center gap-1.5">
        <span className="text-muted-foreground/60">{icon}</span>
        <Label className="text-xs text-muted-foreground">{label}</Label>
      </div>
      <div className="text-sm font-medium">{value}</div>
    </div>
  )
}

export default function ConnectorDetail() {
  const { connectorId } = useParams<{ connectorId: string }>()
  const navigate = useNavigate()
  const accessToken = useAuthStore((s) => s.accessToken)

  const { data, loading } = useQuery(GetRemoteNetworksDocument, {
    pollInterval: 10000,
    fetchPolicy: 'cache-and-network',
  })

  const found = data?.remoteNetworks
    .flatMap((n) => n.connectors.map((c) => ({ ...c, networkId: n.id, networkName: n.name })))
    .find((c) => c.id === connectorId)

  const connector = found
  const networkId = found?.networkId
  const networkName = found?.networkName ?? 'Network'

  // Install command state
  const [tokenLoading, setTokenLoading] = useState(false)
  const [tokenError, setTokenError] = useState<string | null>(null)
  const [installCommand, setInstallCommand] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)
  const didFetch = useRef(false)

  const fetchInstallCommand = async () => {
    if (!connectorId || !accessToken) return
    setTokenLoading(true)
    setTokenError(null)
    try {
      const resp = await fetch(`/api/connectors/${connectorId}/token`, {
        method: 'POST',
        credentials: 'include',
        headers: { Authorization: `Bearer ${accessToken}` },
      })
      if (!resp.ok) {
        const text = await resp.text()
        throw new Error(text || 'Failed to generate token')
      }
      const result = await resp.json()
      setInstallCommand(result.install_command)
    } catch (e: unknown) {
      setTokenError(e instanceof Error ? e.message : 'Failed to generate token')
    } finally {
      setTokenLoading(false)
    }
  }

  useEffect(() => {
    if (connector?.status === ConnectorStatus.Pending && !didFetch.current) {
      didFetch.current = true
      fetchInstallCommand()
    }
  }, [connector?.status])

  function handleCopy() {
    if (!installCommand) return
    navigator.clipboard.writeText(installCommand)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const [revokeConnector, { loading: revoking }] = useMutation(RevokeConnectorDocument, {
    refetchQueries: [{ query: GetRemoteNetworksDocument }],
  })

  const [deleteConnector, { loading: deleting }] = useMutation(DeleteConnectorDocument, {
    refetchQueries: [{ query: GetRemoteNetworksDocument }],
    onCompleted: () => navigate(networkId ? `/remote-networks/${networkId}/connectors` : '/connectors'),
  })

  async function handleRevoke() {
    if (!connectorId) return
    await revokeConnector({ variables: { id: connectorId } as RevokeConnectorMutationVariables })
  }

  async function handleDelete() {
    if (!connectorId) return
    await deleteConnector({ variables: { id: connectorId } as DeleteConnectorMutationVariables })
  }

  // Install card (shared between "not found" and "pending" states)
  const installCard = (
    <Card className="mt-8 mx-auto max-w-2xl text-left">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Terminal className="h-5 w-5" />
          Installation Command
        </CardTitle>
        <CardDescription>
          Copy and run the command below on your server.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {tokenLoading && (
          <div className="flex items-center gap-2 py-2 text-sm text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" />
            Generating enrollment token...
          </div>
        )}

        {tokenError && (
          <div className="space-y-2 py-2">
            <p className="text-sm text-destructive">{tokenError}</p>
            <Button variant="outline" size="sm" className="gap-2" onClick={fetchInstallCommand}>
              <RefreshCw className="h-4 w-4" />
              Retry
            </Button>
          </div>
        )}

        {installCommand && (
          <>
            <div className="flex justify-end">
              <Button variant="ghost" size="sm" className="gap-2" onClick={handleCopy}>
                {copied ? (
                  <>
                    <CheckCircle className="h-4 w-4 text-emerald-500" />
                    <span className="text-emerald-600">Copied</span>
                  </>
                ) : (
                  <>
                    <Copy className="h-4 w-4" />
                    Copy command
                  </>
                )}
              </Button>
            </div>
            <div className="relative rounded-lg border border-border/50 bg-muted/40 overflow-hidden">
              <div className="flex items-center justify-between px-4 py-2 border-b border-border/30 bg-muted/20">
                <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">
                  Install Command
                </span>
              </div>
              <pre className="p-4 text-xs font-mono text-foreground/90 overflow-x-auto whitespace-pre-wrap break-all leading-relaxed">
                {installCommand}
              </pre>
            </div>
          </>
        )}
      </CardContent>
    </Card>
  )

  if (loading && !data) {
    return (
      <div className="flex items-center justify-center p-16">
        <div className="flex flex-col items-center gap-3">
          <Loader2 className="h-6 w-6 animate-spin text-primary" />
          <p className="text-xs text-muted-foreground font-mono tracking-wider">Loading connector...</p>
        </div>
      </div>
    )
  }

  if (!loading && !connector) {
    return (
      <div className="space-y-6">
        <Link
          to="/connectors"
          className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors"
        >
          <ArrowLeft className="w-4 h-4" />
          Back to Connectors
        </Link>
        <div className="text-center py-20">
          <AlertTriangle className="mx-auto h-16 w-16 text-destructive/40" />
          <h2 className="mt-4 text-2xl font-bold">Connector Not Found</h2>
          <p className="mt-2 text-muted-foreground">
            This connector no longer exists or was deleted.
          </p>
        </div>
      </div>
    )
  }

  const st = statusConfig[connector!.status]
  const isPending = connector!.status === ConnectorStatus.Pending
  const isRevoked = connector!.status === ConnectorStatus.Revoked
  const canRevoke = connector!.status === ConnectorStatus.Active || connector!.status === ConnectorStatus.Disconnected
  const canDelete = isPending || isRevoked

  if (isPending) {
    return (
      <div className="space-y-6">
        <Link
          to="/connectors"
          className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors"
        >
          <ArrowLeft className="w-4 h-4" />
          Back to Connectors
        </Link>
        <div className="text-center py-12">
          <AlertTriangle className="mx-auto h-16 w-16 text-muted-foreground/30" />
          <h2 className="mt-4 text-2xl font-bold">Connector Added, Not Installed</h2>
          <p className="mt-2 text-muted-foreground">
            This connector is registered but not installed yet. Run the command below on your server.
          </p>
          <div className="mt-4 flex justify-center gap-2">
            <Button
              variant="outline"
              size="sm"
              className="gap-2 text-destructive border-destructive/30 hover:bg-destructive/5"
              onClick={handleDelete}
              disabled={deleting}
            >
              <Trash2 className="w-4 h-4" />
              {deleting ? 'Deleting...' : 'Delete Connector'}
            </Button>
          </div>
          {installCard}
        </div>
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <motion.div
        className="flex items-center gap-1.5 text-sm text-muted-foreground"
        initial={{ opacity: 0, y: -6 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.3 }}
      >
        <Link to="/connectors" className="hover:text-foreground transition-colors">
          Connectors
        </Link>
        <ChevronRight className="w-3.5 h-3.5 opacity-40" />
        <span className="text-foreground font-medium">{connector!.name}</span>
      </motion.div>

      <motion.div
        className="flex items-center justify-between"
        initial={{ opacity: 0, y: -8 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ delay: 0.05, duration: 0.4 }}
      >
        <div className="space-y-1">
          <div className="flex items-center gap-2.5">
            <h1 className="text-2xl font-display font-bold tracking-tight">
              {connector!.name}
            </h1>
            <Badge variant="outline" className={cn('gap-1', st.className)}>
              {st.icon}
              {st.label}
            </Badge>
          </div>
          <p className="text-xs text-muted-foreground font-mono">{connector!.id}</p>
        </div>

        <div className="flex items-center gap-2">
          {canRevoke && (
            <Button
              variant="outline"
              className="gap-2 text-orange-500 border-orange-500/40 hover:text-orange-600 hover:border-orange-600"
              onClick={handleRevoke}
              disabled={revoking}
            >
              {revoking ? <Loader2 className="h-4 w-4 animate-spin" /> : <ShieldOff className="h-4 w-4" />}
              Revoke
            </Button>
          )}
          {canDelete && (
            <Button
              variant="destructive"
              className="gap-2"
              onClick={handleDelete}
              disabled={deleting}
            >
              {deleting ? <Loader2 className="h-4 w-4 animate-spin" /> : <Trash2 className="h-4 w-4" />}
              Delete
            </Button>
          )}
        </div>
      </motion.div>

      <motion.div
        initial={{ opacity: 0, y: 16 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ delay: 0.1, duration: 0.4 }}
      >
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Server className="h-5 w-5" />
              Connector Details
            </CardTitle>
            <CardDescription>Information about this connector.</CardDescription>
          </CardHeader>
          <CardContent className="grid grid-cols-1 sm:grid-cols-3 gap-6">
            <InfoRow
              icon={<Server className="w-3.5 h-3.5" />}
              label="Name"
              value={connector!.name}
            />
            <InfoRow
              icon={<CircleDot className="w-3.5 h-3.5" />}
              label="Status"
              value={
                <Badge variant="outline" className={cn('gap-1 text-[11px]', st.className)}>
                  {st.icon}
                  {st.label}
                </Badge>
              }
            />
            <InfoRow
              icon={<Cpu className="w-3.5 h-3.5" />}
              label="Version"
              value={<span className="font-mono text-muted-foreground text-xs">{connector!.version ?? '—'}</span>}
            />
            <InfoRow
              icon={<Clock className="w-3.5 h-3.5" />}
              label="Last Seen"
              value={<span className="text-muted-foreground text-xs">{relativeTime(connector!.lastSeenAt)}</span>}
            />
            <InfoRow
              icon={<Network className="w-3.5 h-3.5" />}
              label="Remote Network"
              value={
                <Link
                  to={`/remote-networks/${networkId}/connectors`}
                  className="text-primary hover:underline flex items-center gap-1 text-sm"
                >
                  <Globe className="w-3.5 h-3.5" />
                  {networkName}
                </Link>
              }
            />
            <InfoRow
              icon={<Wifi className="w-3.5 h-3.5" />}
              label="Hostname"
              value={<span className="font-mono text-muted-foreground text-xs">{connector!.hostname ?? '—'}</span>}
            />
            <InfoRow
              icon={<Globe className="w-3.5 h-3.5" />}
              label="Public IP"
              value={<span className="font-mono text-muted-foreground text-xs">{connector!.publicIp ?? '—'}</span>}
            />
            <InfoRow
              icon={<Wifi className="w-3.5 h-3.5" />}
              label="LAN Address"
              value={<span className="font-mono text-muted-foreground text-xs">{connector!.lanAddr ?? '—'}</span>}
            />
            <InfoRow
              icon={<Calendar className="w-3.5 h-3.5" />}
              label="Cert Expires"
              value={
                <span className="font-mono text-muted-foreground text-xs">
                  {connector!.certNotAfter ? new Date(connector!.certNotAfter).toLocaleString() : '—'}
                </span>
              }
            />
            <InfoRow
              icon={<Calendar className="w-3.5 h-3.5" />}
              label="Created"
              value={
                <span className="font-mono text-muted-foreground text-xs">
                  {new Date(connector!.createdAt).toLocaleString()}
                </span>
              }
            />
          </CardContent>
        </Card>
      </motion.div>
    </div>
  )
}
