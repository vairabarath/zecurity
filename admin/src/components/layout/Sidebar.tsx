import { NavLink } from 'react-router-dom'
import { motion } from 'framer-motion'
import { 
  LayoutDashboard, 
  Network, 
  Settings, 
  Shield,
  Activity,
  Globe,
  Plus,
  CheckCircle2,
} from 'lucide-react'
import { cn } from '@/lib/utils'

const sections = [
  {
    label: 'Overview',
    items: [
      { to: '/dashboard', label: 'Dashboard', icon: LayoutDashboard },
      { to: '/activity', label: 'Activity', icon: Activity },
    ],
  },
  {
    label: 'Infrastructure',
    items: [
      { to: '/remote-networks', label: 'Remote Networks', icon: Network },
      { to: '/connectors', label: 'Connectors', icon: Globe },
    ],
  },
  {
    label: 'System',
    items: [
      { to: '/settings', label: 'Settings', icon: Settings },
    ],
  },
]

function NavItem({ to, label, icon: Icon }: { to: string; label: string; icon: React.ElementType }) {
  return (
    <NavLink
      key={to}
      to={to}
      className="group flex items-center justify-between gap-2.5 px-3 py-2.5 rounded-lg text-sm transition-all duration-200"
    >
      {({ isActive }) => (
        <>
          <div className="flex items-center gap-3">
            <div className={cn(
              "flex items-center justify-center w-8 h-8 rounded-lg transition-all duration-200",
              isActive ? "bg-primary/10" : "bg-transparent group-hover:bg-muted"
            )}>
              <Icon className={cn(
                "w-4 h-4 transition-colors duration-200",
                isActive ? "text-primary" : "text-muted-foreground group-hover:text-foreground"
              )} />
            </div>
            <span className={cn(
              "transition-colors duration-200",
              isActive ? "text-foreground font-medium" : "text-muted-foreground group-hover:text-foreground"
            )}>
              {label}
            </span>
          </div>
          
          {isActive && (
            <motion.div
              initial={{ opacity: 0, x: -4 }}
              animate={{ opacity: 1, x: 0 }}
              className="text-primary"
            >
              <div className="w-1.5 h-1.5 rounded-full bg-primary" />
            </motion.div>
          )}
        </>
      )}
    </NavLink>
  )
}

export function Sidebar() {
  return (
    <aside className="w-60 flex flex-col bg-card border-r border-border">
      {/* Logo */}
      <div className="px-5 py-5 border-b border-border">
        <div className="flex items-center gap-3">
          <div className="flex items-center justify-center w-10 h-10 rounded-xl bg-primary shadow-[0_2px_8px_rgba(99,102,241,0.25)]">
            <Shield className="w-5 h-5 text-white" strokeWidth={1.5} />
          </div>
          <div>
            <span className="font-display font-semibold text-lg tracking-tight text-foreground">
              ZECURITY
            </span>
            <p className="text-[10px] text-muted-foreground -mt-0.5">Zero Trust</p>
          </div>
        </div>
      </div>

      {/* Quick Action */}
      <div className="px-4 pt-4">
        <button className="w-full flex items-center justify-center gap-2 px-4 py-2.5 rounded-xl bg-primary text-white text-sm font-medium hover:bg-primary/90 transition-colors shadow-sm">
          <Plus className="w-4 h-4" />
          New Network
        </button>
      </div>

      {/* Navigation */}
      <nav className="flex-1 px-3 py-4 overflow-y-auto">
        {sections.map((section, sectionIndex) => (
          <motion.div
            key={section.label}
            initial={{ opacity: 0, y: 8 }}
            animate={{ opacity: 1, y: 0 }}
            transition={{ delay: sectionIndex * 0.1, duration: 0.3 }}
            className="mb-4"
          >
            <div className="px-3 pb-2 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
              {section.label}
            </div>
            <div className="space-y-0.5">
              {section.items.map(({ to, label, icon: Icon }) => (
                <NavItem key={to} to={to} label={label} icon={Icon} />
              ))}
            </div>
          </motion.div>
        ))}
      </nav>

      {/* Status */}
      <div className="p-4 border-t border-border">
        <div className="flex items-center gap-2.5 p-3 rounded-xl bg-muted/50">
          <CheckCircle2 className="w-4 h-4 text-secure" />
          <div className="flex-1">
            <p className="text-xs font-medium text-foreground">Operational</p>
            <p className="text-[10px] text-muted-foreground">All systems normal</p>
          </div>
          <div className="flex items-center gap-2">
            <div className="w-12 h-1.5 rounded-full bg-muted overflow-hidden">
              <div className="w-[85%] h-full rounded-full bg-secure" />
            </div>
          </div>
        </div>
      </div>
    </aside>
  )
}