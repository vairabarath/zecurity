import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { CheckCircle2, Copy, Monitor, Terminal } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { GetWorkspaceDocument, MeDocument, MyDevicesDocument } from '@/generated/graphql'
import { relativeTime } from '@/lib/console'

const GITHUB_REPO = 'vairabarath/zecurity'
const INSTALL_SCRIPT_URL = `https://raw.githubusercontent.com/${GITHUB_REPO}/main/client/scripts/client-install.sh`

function CopyBlock({ label, command }: { label?: string; command: string }) {
  const [copied, setCopied] = useState(false)

  async function handleCopy() {
    await navigator.clipboard.writeText(command)
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1600)
  }

  return (
    <div className="rounded-xl border border-border bg-muted/30 overflow-hidden">
      {label && (
        <div className="flex items-center justify-between border-b border-border px-4 py-2 bg-muted/50">
          <div className="flex items-center gap-2 text-xs font-medium text-muted-foreground">
            <Terminal className="h-3.5 w-3.5" />
            {label}
          </div>
          <Button variant="ghost" size="sm" onClick={handleCopy} className="h-6 gap-1.5 px-2 text-xs">
            {copied
              ? <><CheckCircle2 className="h-3.5 w-3.5 text-primary" /> Copied</>
              : <><Copy className="h-3.5 w-3.5" /> Copy</>
            }
          </Button>
        </div>
      )}
      {!label && (
        <div className="flex justify-end px-3 pt-2">
          <Button variant="ghost" size="sm" onClick={handleCopy} className="h-6 gap-1.5 px-2 text-xs">
            {copied
              ? <><CheckCircle2 className="h-3.5 w-3.5 text-primary" /> Copied</>
              : <><Copy className="h-3.5 w-3.5" /> Copy</>
            }
          </Button>
        </div>
      )}
      <pre className="overflow-x-auto px-4 pb-4 pt-2 text-[13px] leading-6 text-foreground font-mono whitespace-pre-wrap break-all">
        <code>{command}</code>
      </pre>
    </div>
  )
}

function Step({ number, title, children }: { number: number; title: string; children: React.ReactNode }) {
  return (
    <section className="flex gap-5">
      <div className="flex flex-col items-center">
        <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-primary text-[13px] font-semibold text-primary-foreground">
          {number}
        </div>
        <div className="mt-2 w-px flex-1 bg-border" />
      </div>
      <div className="pb-10 pt-1 min-w-0 flex-1">
        <h2 className="text-[15px] font-semibold mb-3">{title}</h2>
        {children}
      </div>
    </section>
  )
}

export default function ClientInstall() {
  const navigate = useNavigate()
  const { data: meData }        = useQuery(MeDocument)
  const { data: workspaceData } = useQuery(GetWorkspaceDocument)
  const { data: devicesData }   = useQuery(MyDevicesDocument)

  const workspace     = workspaceData?.workspace
  const workspaceName = workspace?.name ?? 'your workspace'
  const workspaceSlug = workspace?.slug ?? '<workspace-slug>'
  const devices       = devicesData?.myDevices ?? []
  const isAdmin       = meData?.me?.role === 'ADMIN'

  const controllerAddr = useMemo(() => {
    if (typeof window === 'undefined') return 'controller.example.com:9090'
    const hostname = window.location.hostname
    // nip.io hostnames (e.g. 192-168-1-223.nip.io) require external DNS.
    // Convert back to raw IP so the install command works on LAN machines
    // without internet-dependent DNS, and matches the cert's IP SAN.
    const nipMatch = hostname.match(/^(\d+-\d+-\d+-\d+)\.nip\.io$/)
    if (nipMatch) return `${nipMatch[1].replace(/-/g, '.')}:9090`
    return `${hostname}:9090`
  }, [])

  const installCmd = `curl -fsSL ${INSTALL_SCRIPT_URL} | sudo CONTROLLER_ADDR=${controllerAddr} bash`
  const setupCmd   = `zecurity-client setup --workspace ${workspaceSlug}`
  const loginCmd   = `zecurity-client login`
  const upCmd      = `zecurity-client up`

  return (
    <main className="mx-auto w-full max-w-2xl px-4 py-10 sm:px-6">

      {/* Header */}
      <div className="mb-10 flex flex-col gap-1">
        <div className="flex items-center gap-2 mb-2">
          <div className="grid h-9 w-9 place-items-center rounded-xl bg-primary/10 text-primary">
            <Monitor className="h-5 w-5" />
          </div>
          <span className="text-[13px] text-muted-foreground font-medium">Zecurity</span>
        </div>
        <h1 className="text-2xl font-semibold tracking-tight">Welcome to {workspaceName}</h1>
        <p className="text-[13.5px] text-muted-foreground leading-relaxed">
          You've been added as a workspace member. Follow the steps below to install the Zecurity client and access protected resources.
        </p>
        {meData?.me?.email && (
          <Badge variant="secondary" className="w-fit mt-1 text-[12px]">
            {meData.me.email}
          </Badge>
        )}
      </div>

      {/* Steps */}
      <div>
        <Step number={1} title="Install the client daemon">
          <p className="text-[13px] text-muted-foreground mb-3">
            Run this one-liner in your terminal. It downloads the binary, installs the systemd daemon, and pre-configures the controller address.
          </p>
          <CopyBlock label="bash" command={installCmd} />
          <p className="mt-2 text-[12px] text-muted-foreground">
            Supports Linux x86_64 and arm64. Requires <code className="bg-muted px-1 py-0.5 rounded text-[11px]">curl</code>, <code className="bg-muted px-1 py-0.5 rounded text-[11px]">systemd</code>, and sudo.
          </p>
        </Step>

        <Step number={2} title="Connect to your workspace">
          <p className="text-[13px] text-muted-foreground mb-3">
            Tell the daemon which workspace to connect to.
          </p>
          <CopyBlock label="bash" command={setupCmd} />
        </Step>

        <Step number={3} title="Log in">
          <p className="text-[13px] text-muted-foreground mb-3">
            Authenticate with your account. A browser window will open to complete login.
          </p>
          <CopyBlock label="bash" command={loginCmd} />
        </Step>

        <Step number={4} title="Connect">
          <p className="text-[13px] text-muted-foreground mb-3">
            Start the tunnel. Protected resources on this workspace are now reachable at their configured addresses.
          </p>
          <CopyBlock label="bash" command={upCmd} />
          <p className="mt-2 text-[12px] text-muted-foreground">
            Run <code className="bg-muted px-1 py-0.5 rounded text-[11px]">zecurity-client status</code> to see active resources,{' '}
            <code className="bg-muted px-1 py-0.5 rounded text-[11px]">zecurity-client down</code> to disconnect.
          </p>
        </Step>
      </div>

      {/* Enrolled devices */}
      {devices.length > 0 && (
        <section className="mt-2 rounded-xl border border-border overflow-hidden">
          <div className="px-4 py-3 border-b border-border bg-muted/40">
            <h3 className="text-[13px] font-semibold">Your enrolled devices</h3>
          </div>
          <table className="w-full text-[13px]">
            <thead>
              <tr className="border-b border-border text-left text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
                <th className="px-4 py-2.5">Name</th>
                <th className="px-4 py-2.5">OS</th>
                <th className="px-4 py-2.5">Last seen</th>
                <th className="px-4 py-2.5">Enrolled</th>
              </tr>
            </thead>
            <tbody>
              {devices.map((device, i) => (
                <tr key={device.id} className={i < devices.length - 1 ? 'border-b border-border' : ''}>
                  <td className="px-4 py-3 font-medium">{device.name}</td>
                  <td className="px-4 py-3 capitalize text-muted-foreground">{device.os}</td>
                  <td className="px-4 py-3 text-muted-foreground">{device.lastSeenAt ? relativeTime(device.lastSeenAt) : '—'}</td>
                  <td className="px-4 py-3 text-muted-foreground">{relativeTime(device.createdAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}

      {/* Footer */}
      <div className="mt-10 flex flex-col gap-3 border-t border-border pt-6 text-[13px] text-muted-foreground sm:flex-row sm:items-center sm:justify-between">
        <p>Need help? Contact your workspace admin.</p>
        {isAdmin && (
          <Button variant="ghost" size="sm" onClick={() => navigate('/dashboard')}>
            Go to admin console
          </Button>
        )}
      </div>

    </main>
  )
}
