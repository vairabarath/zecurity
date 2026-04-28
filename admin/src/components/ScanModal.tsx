import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import { Loader2, Plus, Radar, Search, X } from 'lucide-react'
import { useNavigate } from 'react-router-dom'
import {
  GetScanResultsDocument,
  TriggerScanDocument,
  type GetScanResultsQuery,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { relativeTime } from '@/lib/console'

interface ScanModalProps {
  connectorId: string
  remoteNetworkId: string
  connectorName: string
  onClose: () => void
}

function parseTargets(value: string): string[] {
  return Array.from(
    new Set(
      value
        .split(/[\n,]/)
        .map((item) => item.trim())
        .filter(Boolean),
    ),
  )
}

function parsePorts(value: string): { ports: number[]; invalid: boolean } {
  const raw = value
    .split(/[,\s]+/)
    .map((item) => item.trim())
    .filter(Boolean)

  let invalid = false
  const ports = Array.from(
    new Set(
      raw.flatMap((item) => {
        const parsed = Number.parseInt(item, 10)
        if (!Number.isInteger(parsed) || parsed < 1 || parsed > 65535) {
          invalid = true
          return []
        }
        return [parsed]
      }),
    ),
  )

  return { ports, invalid }
}

export function ScanModal({
  connectorId,
  remoteNetworkId,
  connectorName,
  onClose,
}: ScanModalProps) {
  const navigate = useNavigate()
  const [targetsText, setTargetsText] = useState('')
  const [portsText, setPortsText] = useState('22, 80, 443')
  const [requestId, setRequestId] = useState<string | null>(null)
  const [formError, setFormError] = useState<string | null>(null)
  const [pollingExpired, setPollingExpired] = useState(false)

  const parsedTargets = useMemo(() => parseTargets(targetsText), [targetsText])
  const parsedPorts = useMemo(() => parsePorts(portsText), [portsText])
  const isScanning = Boolean(requestId) && !pollingExpired

  const [triggerScan, { loading: startingScan, error: mutationError }] = useMutation(TriggerScanDocument, {
    onCompleted: (data) => {
      setRequestId(data.triggerScan)
      setPollingExpired(false)
    },
  })

  const { data, loading: loadingResults, error: resultsError, startPolling, stopPolling } = useQuery(
    GetScanResultsDocument,
    {
      variables: { requestId: requestId ?? '' },
      skip: !requestId,
      fetchPolicy: 'cache-and-network',
      notifyOnNetworkStatusChange: true,
    },
  )

  useEffect(() => {
    if (!requestId || pollingExpired) {
      stopPolling()
      return
    }
    startPolling(3000)
    const timeoutId = window.setTimeout(() => {
      setPollingExpired(true)
      stopPolling()
    }, 60000)

    return () => {
      window.clearTimeout(timeoutId)
      stopPolling()
    }
  }, [pollingExpired, requestId, startPolling, stopPolling])

  async function handleStartScan() {
    setFormError(null)

    if (parsedTargets.length === 0) {
      setFormError('Enter at least one target IP or CIDR.')
      return
    }
    if (parsedPorts.ports.length === 0) {
      setFormError('Enter at least one port.')
      return
    }
    if (parsedPorts.invalid) {
      setFormError('Ports must be whole numbers between 1 and 65535.')
      return
    }
    if (parsedPorts.ports.length > 16) {
      setFormError('A scan can include at most 16 ports.')
      return
    }

    await triggerScan({
      variables: {
        connectorId,
        targets: parsedTargets,
        ports: parsedPorts.ports,
      },
    })
  }

  function handleCreateResource(result: GetScanResultsQuery['getScanResults'][number]) {
    navigate('/resources', {
      state: {
        createResourceDefaults: {
          remoteNetworkId,
          name: result.serviceName ? `${result.serviceName.toLowerCase()}-${result.ip}` : undefined,
          host: result.ip,
          protocol: result.protocol,
          portFrom: result.port,
          portTo: result.port,
        },
      },
    })
    onClose()
  }

  const results = data?.getScanResults ?? []
  const combinedError = formError ?? mutationError?.message ?? resultsError?.message ?? null

  return (
    <div className="fixed inset-0 z-50">
      <div className="absolute inset-0 bg-black/50 backdrop-blur-sm" onClick={onClose} />
      <div className="absolute left-1/2 top-1/2 flex h-[min(90vh,760px)] w-[min(960px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 flex-col overflow-hidden rounded-3xl border border-border bg-background shadow-2xl">
        <div className="flex items-start justify-between gap-4 border-b border-border px-6 py-5">
          <div className="flex items-start gap-4">
            <div className="grid h-12 w-12 place-items-center rounded-2xl border border-primary/20 bg-primary/10 text-primary">
              <Radar className="h-5 w-5" />
            </div>
            <div>
              <h2 className="text-lg font-semibold">Scan Network</h2>
              <p className="mt-1 text-sm text-muted-foreground">
                Launch a connector-side TCP scan via <span className="font-semibold text-foreground">{connectorName}</span>.
              </p>
            </div>
          </div>
          <button
            onClick={onClose}
            className="flex h-9 w-9 items-center justify-center rounded-lg border border-border text-muted-foreground transition hover:bg-secondary hover:text-foreground"
          >
            <X className="h-4 w-4" />
          </button>
        </div>

        <div className="grid min-h-0 flex-1 gap-0 lg:grid-cols-[340px_minmax(0,1fr)]">
          <div className="border-b border-border p-6 lg:border-b-0 lg:border-r">
            <div className="space-y-5">
              <div className="space-y-2">
                <Label className="text-sm font-semibold">Target IPs / CIDRs</Label>
                <textarea
                  value={targetsText}
                  onChange={(event) => setTargetsText(event.target.value)}
                  placeholder={'192.168.1.0/24\n10.0.0.15'}
                  className="min-h-36 w-full rounded-xl border border-border bg-secondary px-3 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-primary/30"
                />
                <p className="text-xs text-muted-foreground">
                  One per line or comma separated. Use full CIDR (e.g. <span className="font-mono">192.168.1.0/24</span>) or a bare IP (e.g. <span className="font-mono">10.0.0.15</span>).
                </p>
              </div>

              <div className="space-y-2">
                <Label className="text-sm font-semibold">Ports</Label>
                <Input
                  value={portsText}
                  onChange={(event) => setPortsText(event.target.value)}
                  placeholder="22, 80, 443, 3306"
                  className="h-11 font-mono text-sm"
                />
                <p className="text-xs text-muted-foreground">Comma or space separated. Max 16 ports.</p>
              </div>

              {combinedError ? (
                <div className="rounded-xl border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">
                  {combinedError}
                </div>
              ) : null}

              <div className="rounded-2xl border border-border bg-secondary/40 p-4 text-xs text-muted-foreground">
                <div className="font-semibold text-foreground">Parsed request</div>
                <div className="mt-2">{parsedTargets.length} targets</div>
                <div>{parsedPorts.ports.length} ports</div>
              </div>

              <Button onClick={handleStartScan} disabled={startingScan} className="h-11 w-full gap-2">
                {startingScan ? (
                  <>
                    <Loader2 className="h-4 w-4 animate-spin" />
                    Starting scan...
                  </>
                ) : (
                  <>
                    <Search className="h-4 w-4" />
                    Start Scan
                  </>
                )}
              </Button>
            </div>
          </div>

          <div className="min-h-0 p-6">
            {!requestId ? (
              <div className="flex h-full items-center justify-center rounded-3xl border border-dashed border-border bg-secondary/20 p-8 text-center">
                <div>
                  <div className="mx-auto grid h-14 w-14 place-items-center rounded-full border border-primary/20 bg-primary/10 text-primary">
                    <Radar className="h-6 w-6" />
                  </div>
                  <h3 className="mt-4 text-lg font-semibold">Ready to scan</h3>
                  <p className="mt-2 max-w-sm text-sm text-muted-foreground">
                    Submit a target scope and ports to start discovery from the selected connector.
                  </p>
                </div>
              </div>
            ) : (
              <div className="flex h-full min-h-0 flex-col">
                <div className="mb-4 flex items-center justify-between gap-3">
                  <div>
                    <div className="text-sm font-semibold">Results</div>
                    <div className="mt-1 text-xs text-muted-foreground">
                      Request <span className="font-mono">{requestId}</span>
                    </div>
                  </div>
                  <div className="text-xs text-muted-foreground">
                    {isScanning ? 'Scanning… polling every 3s' : 'Polling stopped'}
                  </div>
                </div>

                {loadingResults && results.length === 0 ? (
                  <div className="space-y-3">
                    {Array.from({ length: 4 }).map((_, index) => (
                      <Skeleton key={index} className="h-12 rounded-2xl bg-secondary" />
                    ))}
                  </div>
                ) : results.length === 0 ? (
                  <div className="flex h-full items-center justify-center rounded-3xl border border-dashed border-border bg-secondary/20 p-8 text-center text-sm text-muted-foreground">
                    {pollingExpired ? (
                      <div className="space-y-2">
                        <div className="font-semibold text-foreground">No live services found in the given scope.</div>
                        <div className="text-xs">
                          Verify that <span className="font-semibold">{connectorName}</span> can reach the target subnet
                          and that at least one host is listening on the chosen ports. CIDR must be full (e.g. <span className="font-mono">192.168.1.0/24</span>).
                        </div>
                      </div>
                    ) : (
                      <div className="space-y-2">
                        <div>Scanning… waiting for first results.</div>
                        <div className="text-xs">A /24 sweep typically completes in 5–60 s; results may arrive in batches.</div>
                      </div>
                    )}
                  </div>
                ) : (
                  <div className="min-h-0 overflow-hidden rounded-2xl border border-border">
                    <div className="grid grid-cols-[minmax(0,1fr)_minmax(0,1.5fr)_40px] gap-4 border-b border-border bg-secondary px-4 py-3 text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">
                      <div>Host</div>
                      <div>Service</div>
                      <div />
                    </div>
                    <div className="max-h-full overflow-y-auto">
                      {results.map((result) => (
                        <div
                          key={`${result.requestId}-${result.ip}-${result.port}`}
                          className="grid grid-cols-[minmax(0,1fr)_minmax(0,1.5fr)_40px] items-center gap-4 border-b border-border px-4 py-3 last:border-b-0"
                        >
                          <div className="font-mono text-xs">
                            {result.ip}
                            <span className="text-muted-foreground">:{result.port}</span>
                          </div>
                          <div className="min-w-0">
                            <div className="truncate font-medium">
                              {result.serviceName || <span className="text-muted-foreground">Unknown</span>}
                            </div>
                            <div className="text-xs text-muted-foreground">{relativeTime(result.firstSeen)}</div>
                          </div>
                          <div>
                            <button
                              onClick={() => handleCreateResource(result)}
                              title="Create Resource"
                              className="flex h-8 w-8 items-center justify-center rounded-lg border border-border bg-background text-primary transition hover:bg-secondary"
                            >
                              <Plus className="h-3.5 w-3.5" />
                            </button>
                          </div>
                        </div>
                      ))}
                    </div>
                  </div>
                )}
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
