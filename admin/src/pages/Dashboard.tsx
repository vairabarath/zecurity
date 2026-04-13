import { useState } from 'react'
import { useQuery } from '@apollo/client/react'
import { motion } from 'framer-motion'
import {
  MeDocument,
  GetWorkspaceDocument,
  WorkspaceStatus,
  type MeQuery,
  type GetWorkspaceQuery,
} from '@/generated/graphql'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { 
  Shield, 
  Globe, 
  Users, 
  Activity, 
  Network,
  Clock,
  ArrowUpRight,
  ArrowDownRight,
  AlertTriangle,
  CheckCircle2,
  Server,
} from 'lucide-react'
import { cn } from '@/lib/utils'

const statusVariant: Record<WorkspaceStatus, 'default' | 'secondary' | 'destructive' | 'outline'> = {
  [WorkspaceStatus.Active]: 'default',
  [WorkspaceStatus.Provisioning]: 'secondary',
  [WorkspaceStatus.Suspended]: 'destructive',
  [WorkspaceStatus.Deleted]: 'outline',
}

interface StatCardProps {
  title: string
  value: string | number
  change?: number
  trend?: 'up' | 'down'
  delay?: number
}

function StatCard({ title, value, change, trend, delay = 0 }: StatCardProps) {
  return (
    <motion.div
      initial={{ opacity: 0, y: 16 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ delay, duration: 0.4, ease: [0.22, 1, 0.36, 1] }}
      className="group relative rounded-xl border border-border bg-white p-5 hover:border-primary/30 hover:shadow-md transition-all duration-300"
    >
      <div className="flex items-center justify-between mb-3">
        <span className="text-xs font-medium text-muted-foreground">{title}</span>
        {change !== undefined && (
          <div className={cn(
            "flex items-center gap-1 text-xs font-medium",
            trend === 'up' ? "text-secure" : "text-destructive"
          )}>
            {trend === 'up' ? <ArrowUpRight className="w-3 h-3" /> : <ArrowDownRight className="w-3 h-3" />}
            {Math.abs(change)}%
          </div>
        )}
      </div>
      <div className="text-3xl font-semibold text-foreground">{value}</div>
    </motion.div>
  )
}

function ActivityItem({ type, message, time }: { type: string; message: string; time: string }) {
  const icons: Record<string, React.ReactNode> = {
    connection: <Network className="w-4 h-4 text-primary" />,
    auth: <Shield className="w-4 h-4 text-info" />,
    alert: <AlertTriangle className="w-4 h-4 text-warning" />,
    success: <CheckCircle2 className="w-4 h-4 text-secure" />,
  }
  
  return (
    <motion.div
      className="flex items-start gap-3 p-3 rounded-xl bg-muted/50 hover:bg-muted transition-colors"
      initial={{ opacity: 0, x: -8 }}
      animate={{ opacity: 1, x: 0 }}
    >
      <div className={cn(
        "flex items-center justify-center w-8 h-8 rounded-lg",
        type === 'connection' && "bg-primary/10 text-primary",
        type === 'auth' && "bg-info/10 text-info",
        type === 'alert' && "bg-warning/10 text-warning",
        type === 'success' && "bg-secure/10 text-secure"
      )}>
        {icons[type]}
      </div>
      <div className="flex-1 min-w-0">
        <p className="text-sm text-foreground truncate">{message}</p>
        <p className="text-xs text-muted-foreground mt-0.5">{time}</p>
      </div>
    </motion.div>
  )
}

function NetworkCard({ name, status, users, lastActive, delay = 0 }: { name: string; status: string; users: number; lastActive: string; delay?: number }) {
  return (
    <motion.div
      className="group relative rounded-xl border border-border bg-white p-4 hover:border-primary/30 hover:shadow-md transition-all duration-300"
      initial={{ opacity: 0, scale: 0.98 }}
      animate={{ opacity: 1, scale: 1 }}
      transition={{ delay, duration: 0.3 }}
      whileHover={{ scale: 1.01 }}
    >
      <div className="flex items-start justify-between mb-3">
        <div className="flex items-center gap-2">
          <Globe className="w-4 h-4 text-primary" />
          <span className="font-medium text-foreground">{name}</span>
        </div>
        <Badge variant={status === 'active' ? 'default' : 'secondary'} className="text-[10px]">
          {status}
        </Badge>
      </div>
      <div className="flex items-center gap-4 text-xs text-muted-foreground">
        <span className="flex items-center gap-1">
          <Users className="w-3 h-3" /> {users}
        </span>
        <span className="flex items-center gap-1">
          <Clock className="w-3 h-3" /> {lastActive}
        </span>
      </div>
    </motion.div>
  )
}

export default function Dashboard() {
  const { data: meData, loading: meLoading } = useQuery<MeQuery>(MeDocument)
  const { data: wsData, loading: wsLoading } = useQuery<GetWorkspaceQuery>(GetWorkspaceDocument)
  
  const [activities] = useState([
    { type: 'connection', message: 'New connection from 192.168.1.105', time: '2 min ago' },
    { type: 'auth', message: 'User john@acme.com authenticated', time: '5 min ago' },
    { type: 'alert', message: 'Failed login attempt detected', time: '12 min ago' },
    { type: 'success', message: 'Network "prod-us-east" synced', time: '1 hour ago' },
  ])
  
  const [networks] = useState([
    { name: 'Production US-East', status: 'active', users: 24, lastActive: 'now' },
    { name: 'Staging EU-West', status: 'active', users: 8, lastActive: '5m ago' },
    { name: 'Dev Network', status: 'inactive', users: 0, lastActive: '2d ago' },
  ])

  return (
    <div className="space-y-6">
      {/* Header */}
      <motion.div
        className="flex items-center justify-between"
        initial={{ opacity: 0, y: -8 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.4 }}
      >
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Dashboard</h1>
          <p className="text-sm text-muted-foreground mt-1">Monitor your zero trust network</p>
        </div>
        <div className="flex items-center gap-2">
          <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <span className="relative flex h-2 w-2">
              <span className="absolute inline-flex h-full w-full rounded-full bg-secure opacity-50 animate-ping" />
              <span className="relative inline-flex rounded-full h-2 w-2 bg-secure" />
            </span>
            Live
          </span>
        </div>
      </motion.div>

      {/* Stats */}
      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4">
        <StatCard title="Active Networks" value="12" change={8} trend="up" delay={0.1} />
        <StatCard title="Total Users" value="1,248" change={12} trend="up" delay={0.15} />
        <StatCard title="Connections" value="3,846" change={-3} trend="down" delay={0.2} />
        <StatCard title="Threats Blocked" value="7" change={-25} trend="down" delay={0.25} />
      </div>

      {/* Main Grid */}
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
        {/* Activity */}
        <motion.div
          className="lg:col-span-2 rounded-xl border border-border bg-white overflow-hidden"
          initial={{ opacity: 0, y: 16 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ delay: 0.3, duration: 0.4 }}
        >
          <div className="flex items-center justify-between px-5 py-4 border-b border-border">
            <div className="flex items-center gap-2">
              <Activity className="w-4 h-4 text-primary" />
              <h2 className="font-semibold text-foreground">Activity Feed</h2>
            </div>
            <button className="text-xs text-primary hover:underline">View all</button>
          </div>
          <div className="p-4 space-y-2">
            {activities.map((activity, i) => (
              <ActivityItem key={i} {...activity} />
            ))}
          </div>
        </motion.div>

        {/* Workspace */}
        <motion.div
          initial={{ opacity: 0, y: 16 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ delay: 0.35, duration: 0.4 }}
        >
          <div className="rounded-xl border border-border bg-white overflow-hidden h-full">
            <div className="flex items-center gap-2 px-5 py-4 border-b border-border">
              <Server className="w-4 h-4 text-primary" />
              <h2 className="font-semibold text-foreground">Workspace</h2>
            </div>
            <div className="p-5 space-y-4">
              {wsLoading ? (
                <div className="space-y-3">
                  <Skeleton className="h-5 w-32" />
                  <Skeleton className="h-4 w-24" />
                </div>
              ) : (
                <>
                  <div>
                    <p className="text-xs text-muted-foreground mb-1">Name</p>
                    <p className="font-semibold text-foreground">{wsData?.workspace.name}</p>
                  </div>
                  <div>
                    <p className="text-xs text-muted-foreground mb-1">Endpoint</p>
                    <p className="text-sm font-mono text-primary">{wsData?.workspace.slug}.ztna.io</p>
                  </div>
                  <div>
                    <Badge variant={statusVariant[wsData?.workspace.status!] ?? 'outline'}>
                      {wsData?.workspace.status}
                    </Badge>
                  </div>
                  <div className="pt-3 border-t border-border">
                    <p className="text-xs text-muted-foreground mb-1">Account</p>
                    {meLoading ? (
                      <Skeleton className="h-4 w-40" />
                    ) : (
                      <p className="text-sm text-foreground truncate">{meData?.me.email}</p>
                    )}
                  </div>
                </>
              )}
            </div>
          </div>
        </motion.div>
      </div>

      {/* Networks */}
      <motion.div
        initial={{ opacity: 0, y: 16 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ delay: 0.4, duration: 0.4 }}
      >
        <div className="flex items-center justify-between mb-4">
          <div className="flex items-center gap-2">
            <Network className="w-4 h-4 text-primary" />
            <h2 className="font-semibold text-foreground">Networks</h2>
          </div>
          <button className="text-xs text-primary hover:underline">Add network</button>
        </div>
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          {networks.map((network, i) => (
            <NetworkCard key={i} {...network} delay={i * 0.05} />
          ))}
        </div>
      </motion.div>
    </div>
  )
}