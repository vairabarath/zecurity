import { useNavigate } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { MeDocument, MyDevicesDocument } from '@/generated/graphql'

export default function ClientInstall() {
  const navigate = useNavigate()
  const { data: meData } = useQuery(MeDocument)
  const { data: devicesData } = useQuery(MyDevicesDocument)

  const isAdmin = meData?.me?.role === 'ADMIN'
  const devices = devicesData?.myDevices ?? []

  return (
    <div className="mx-auto max-w-2xl py-12 px-4">
      <h1 className="text-3xl font-bold tracking-tight">Install Zecurity Client</h1>
      <p className="mt-2 text-sm text-muted-foreground">
        Download and install the client to connect to your workspace.
      </p>

      {/* Download cards */}
      <div className="mt-8 grid grid-cols-3 gap-4">
        {[
          { os: 'Linux', icon: '🐧', sub: 'x86_64 / arm64' },
          { os: 'macOS', icon: '🍎', sub: 'Apple Silicon / Intel' },
          { os: 'Windows', icon: '🪟', sub: 'x86_64' },
        ].map(({ os, icon, sub }) => (
          <a
            key={os}
            href="#"
            className="flex flex-col items-center gap-2 rounded-2xl border border-border bg-card p-5 text-center transition hover:border-primary/60 hover:bg-accent"
          >
            <span className="text-3xl">{icon}</span>
            <span className="text-sm font-semibold">{os}</span>
            <span className="text-xs text-muted-foreground">{sub}</span>
          </a>
        ))}
      </div>

      {/* Setup instructions */}
      <div className="mt-10">
        <h2 className="text-sm font-semibold uppercase tracking-widest text-muted-foreground">
          Setup
        </h2>
        <pre className="mt-3 overflow-x-auto rounded-xl bg-[oklch(0.14_0.01_250)] px-5 py-4 text-sm leading-relaxed text-[oklch(0.85_0.09_145)]">
{`# 1. Write config (one-time)
zecurity-client setup \\
  --controller controller.example.com:9090 \\
  --workspace  your-workspace-slug

# 2. Connect
zecurity-client login`}
        </pre>
      </div>

      {/* Enrolled devices */}
      {devices.length > 0 && (
        <div className="mt-10">
          <h2 className="text-sm font-semibold uppercase tracking-widest text-muted-foreground">
            Your Enrolled Devices
          </h2>
          <div className="mt-3 overflow-hidden rounded-xl border border-border">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border bg-secondary/50 text-left text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                  <th className="px-4 py-3">Name</th>
                  <th className="px-4 py-3">OS</th>
                  <th className="px-4 py-3">Enrolled</th>
                </tr>
              </thead>
              <tbody>
                {devices.map((d, i) => (
                  <tr
                    key={d.id}
                    className={i < devices.length - 1 ? 'border-b border-border' : ''}
                  >
                    <td className="px-4 py-3 font-medium">{d.name}</td>
                    <td className="px-4 py-3 text-muted-foreground">{d.os}</td>
                    <td className="px-4 py-3 text-muted-foreground">
                      {new Date(d.createdAt).toLocaleDateString()}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* Admin shortcut */}
      {isAdmin && (
        <div className="mt-10 border-t border-border pt-6">
          <p className="text-xs text-muted-foreground">
            You're an admin — you can also manage your workspace from the console.
          </p>
          <button
            onClick={() => navigate('/dashboard')}
            className="mt-2 text-sm font-semibold text-primary hover:underline"
          >
            Go to Admin Console →
          </button>
        </div>
      )}
    </div>
  )
}
