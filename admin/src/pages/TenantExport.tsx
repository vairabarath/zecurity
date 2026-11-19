import { useEffect, useRef, useState } from 'react'
import { tokenFromHash } from '@/lib/tenantExport'

// Sprint 23 S4 (2026-11-18 design): a suspended workspace scheduled for
// deletion — its admin downloads the workspace's own logs until the
// deletion. Reached from the emailed link or an export-only sign-in, both
// with the token in the URL fragment (#token=…), which never reaches a server
// log. The page drops the fragment from the address bar and history at once,
// and sends the token only in a POST body. No session exists here.

interface ExportInfo {
  workspace_name: string
  slug: string
  delete_after: string
}

type State =
  | { status: 'loading' }
  | { status: 'ready'; info: ExportInfo }
  | { status: 'invalid' }
  | { status: 'error' }

export default function TenantExport() {
  const token = useRef(tokenFromHash(window.location.hash))
  const [state, setState] = useState<State>({ status: 'loading' })
  const [downloading, setDownloading] = useState(false)
  const [downloadError, setDownloadError] = useState<string | null>(null)
  const [done, setDone] = useState(false)

  useEffect(() => {
    // Keep the token out of the address bar, history and any later Referer.
    if (window.location.hash) window.history.replaceState(null, '', window.location.pathname)
    if (!token.current) {
      setState({ status: 'invalid' })
      return
    }
    fetch('/api/tenant-export/info', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: token.current }),
    })
      .then(async (r) => {
        if (r.status === 404 || r.status === 400) return setState({ status: 'invalid' })
        if (!r.ok) return setState({ status: 'error' })
        setState({ status: 'ready', info: (await r.json()) as ExportInfo })
      })
      .catch(() => setState({ status: 'error' }))
  }, [])

  async function download() {
    if (downloading) return
    setDownloading(true)
    setDownloadError(null)
    try {
      const r = await fetch('/api/tenant-export/download', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: token.current }),
      })
      if (r.status === 404) {
        setDownloadError('This link has expired or was already used. Sign in again to get a new download.')
        return
      }
      if (!r.ok) {
        setDownloadError('The download failed. Try again.')
        return
      }
      const blob = await r.blob()
      const name = /filename="([^"]+)"/.exec(r.headers.get('Content-Disposition') ?? '')?.[1] ?? 'zecurity-logs.tar.gz'
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = name
      a.click()
      URL.revokeObjectURL(url)
      setDone(true)
    } catch {
      setDownloadError('The download failed. Check your connection and try again.')
    } finally {
      setDownloading(false)
    }
  }

  if (state.status === 'loading') {
    return (
      <div className="flex min-h-screen items-center justify-center">
        <p className="text-sm text-muted-foreground">Checking your link…</p>
      </div>
    )
  }
  if (state.status !== 'ready') {
    return (
      <div className="flex min-h-screen flex-col items-center justify-center gap-4 px-4 text-center">
        <h1 className="text-xl font-semibold">Download link not valid</h1>
        <p className="max-w-sm text-sm text-muted-foreground">
          {state.status === 'invalid'
            ? 'This link has expired, was already used, or the workspace is no longer awaiting deletion. Admins can sign in again to get a new download, until the deletion.'
            : 'We couldn’t check your link. Try again in a moment.'}
        </p>
      </div>
    )
  }

  const { info } = state
  const deadline = new Date(info.delete_after).toLocaleString()
  return (
    <div className="flex min-h-screen flex-col items-center justify-center gap-6 px-4">
      <div className="max-w-md text-center">
        <h1 className="text-2xl font-semibold tracking-tight">Workspace suspended</h1>
        <p className="mt-2 text-sm text-muted-foreground">
          <span className="font-medium text-foreground">{info.workspace_name}</span> is suspended by your provider and
          scheduled for deletion on <span className="font-medium text-foreground">{deadline}</span>. Until then you can
          download your workspace’s logs: the admin audit log, the access log and the certificate history.
        </p>
      </div>
      {downloadError && (
        <p role="alert" className="max-w-sm text-center text-sm text-destructive">
          {downloadError}
        </p>
      )}
      {done && (
        <p role="status" className="max-w-sm text-center text-sm text-muted-foreground">
          Download started. After the deletion, the workspace and its data can’t be recovered.
        </p>
      )}
      <button
        onClick={() => void download()}
        disabled={downloading}
        className="flex w-full max-w-sm items-center justify-center gap-2 rounded-xl bg-primary px-6 py-3 text-sm font-semibold text-primary-foreground shadow transition hover:opacity-90 disabled:opacity-50"
      >
        {downloading ? 'Preparing your logs…' : 'Download logs'}
      </button>
      <p className="max-w-sm text-center text-xs text-muted-foreground">
        Questions about the suspension or the deletion? Contact your provider.
      </p>
    </div>
  )
}
