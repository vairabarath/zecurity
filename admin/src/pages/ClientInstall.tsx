import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { CheckCircle2, Copy, Download, ExternalLink } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { GetWorkspaceDocument, MeDocument, MyDevicesDocument } from '@/generated/graphql'

const RELEASE_URL = 'https://github.com/yourorg/ztna/releases/latest'

function CopyBlock({ command }: { command: string }) {
  const [copied, setCopied] = useState(false)

  async function handleCopy() {
    await navigator.clipboard.writeText(command)
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1600)
  }

  return (
    <div className="rounded-lg border border-border bg-card">
      <div className="flex items-center justify-between border-b border-border px-4 py-2">
        <span className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
          Terminal
        </span>
        <Button variant="ghost" size="sm" onClick={handleCopy} className="h-7 px-2">
          {copied ? <CheckCircle2 className="h-4 w-4 text-primary" /> : <Copy className="h-4 w-4" />}
          {copied ? 'Copied' : 'Copy'}
        </Button>
      </div>
      <pre className="overflow-x-auto px-4 py-3 text-sm leading-6 text-foreground">
        <code>{command}</code>
      </pre>
    </div>
  )
}

export default function ClientInstall() {
  const navigate = useNavigate()
  const { data: meData } = useQuery(MeDocument)
  const { data: workspaceData } = useQuery(GetWorkspaceDocument)
  const { data: devicesData } = useQuery(MyDevicesDocument)

  const workspace = workspaceData?.workspace
  const workspaceName = workspace?.name ?? 'your workspace'
  const workspaceSlug = workspace?.slug ?? '<workspace-slug>'
  const devices = devicesData?.myDevices ?? []
  const isAdmin = meData?.me?.role === 'ADMIN'

  const controllerAddr = useMemo(() => {
    if (typeof window === 'undefined') return 'controller.example.com:9090'
    return `${window.location.hostname}:9090`
  }, [])

  const installCommand = `sudo CONTROLLER_ADDR=${controllerAddr} \\
  ./client-local-install.sh zecurity-client`
  const setupCommand = `zecurity-client setup --workspace ${workspaceSlug}
zecurity-client login`

  return (
    <main className="mx-auto w-full max-w-4xl px-4 py-8 sm:px-6 lg:px-8 h-screen overflow-y-auto">
      <div className="flex flex-col gap-3 border-b border-border pb-6 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <p className="text-sm text-muted-foreground">Zecurity</p>
          <h1 className="mt-2 text-3xl font-semibold tracking-tight">
            Welcome to {workspaceName}
          </h1>
          <p className="mt-3 max-w-2xl text-sm leading-6 text-muted-foreground">
            You have been added as a workspace member. Install the Zecurity client on your device to access protected resources on this network.
          </p>
        </div>
        {meData?.me?.email && (
          <Badge variant="secondary" className="w-fit">
            {meData.me.email}
          </Badge>
        )}
      </div>

      <section className="mt-8 space-y-4">
        <div>
          <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Step 1
          </p>
          <h2 className="mt-1 text-xl font-semibold">Download</h2>
        </div>
        <div className="grid gap-3 sm:grid-cols-2">
          <Button asChild variant="outline" className="h-auto justify-between px-4 py-4">
            <a href={RELEASE_URL} target="_blank" rel="noreferrer">
              <span className="flex items-center gap-3">
                <Download className="h-4 w-4" />
                Linux amd64
              </span>
              <ExternalLink className="h-4 w-4 text-muted-foreground" />
            </a>
          </Button>
          <Button asChild variant="outline" className="h-auto justify-between px-4 py-4">
            <a href={RELEASE_URL} target="_blank" rel="noreferrer">
              <span className="flex items-center gap-3">
                <Download className="h-4 w-4" />
                Linux arm64
              </span>
              <ExternalLink className="h-4 w-4 text-muted-foreground" />
            </a>
          </Button>
        </div>
      </section>

      <section className="mt-10 space-y-4">
        <div>
          <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Step 2
          </p>
          <h2 className="mt-1 text-xl font-semibold">Install</h2>
        </div>
        <CopyBlock command={installCommand} />
      </section>

      <section className="mt-10 space-y-4">
        <div>
          <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Step 3
          </p>
          <h2 className="mt-1 text-xl font-semibold">Authenticate</h2>
        </div>
        <CopyBlock command={setupCommand} />
      </section>

      {devices.length > 0 && (
        <section className="mt-10">
          <h2 className="text-xl font-semibold">Your devices</h2>
          <div className="mt-4 overflow-hidden rounded-lg border border-border">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border bg-muted/40 text-left text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                  <th className="px-4 py-3">Name</th>
                  <th className="px-4 py-3">OS</th>
                  <th className="px-4 py-3">Enrolled</th>
                </tr>
              </thead>
              <tbody>
                {devices.map((device, index) => (
                  <tr
                    key={device.id}
                    className={index < devices.length - 1 ? 'border-b border-border' : ''}
                  >
                    <td className="px-4 py-3 font-medium">{device.name}</td>
                    <td className="px-4 py-3 text-muted-foreground">{device.os}</td>
                    <td className="px-4 py-3 text-muted-foreground">
                      {new Date(device.createdAt).toLocaleDateString()}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}

      <div className="mt-10 flex flex-col gap-3 border-t border-border pt-6 text-sm text-muted-foreground sm:flex-row sm:items-center sm:justify-between">
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
