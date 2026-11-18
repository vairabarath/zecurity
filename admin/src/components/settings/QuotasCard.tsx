import { useQuery } from '@apollo/client/react'
import { Gauge } from 'lucide-react'
import { GetWorkspaceQuotasDocument } from '@/generated/graphql'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { quotaLabel } from '@/lib/quota'

// Settings → Limits (Sprint 23 S3, D-47). Read-only: the provider sets the
// workspace's limits. Shows usage against each limit, counted by the
// controller with the same rules it enforces when something is created.
export function QuotasCard() {
  const { data, loading, error } = useQuery(GetWorkspaceQuotasDocument, { fetchPolicy: 'cache-and-network' })
  const quotas = data?.workspaceQuotas ?? []

  return (
    <Card data-testid="quotas-card">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm font-medium">
          <Gauge className="h-4 w-4" />
          Limits
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        <p className="text-muted-foreground">
          Set by your provider. When a limit is reached, nothing more of that kind can be created until something is removed or
          the provider raises the limit.
        </p>
        {loading && !data ? (
          <Skeleton className="h-24 w-full" />
        ) : error && !data ? (
          <p className="text-muted-foreground">Could not load the limits.</p>
        ) : (
          <table className="w-full text-sm" aria-label="Limits">
            <thead>
              <tr className="text-left text-xs text-muted-foreground">
                <th className="py-1 font-medium">Resource</th>
                <th className="py-1 text-right font-medium">In use</th>
                <th className="py-1 text-right font-medium">Limit</th>
              </tr>
            </thead>
            <tbody>
              {quotas.map((q) => (
                <tr key={q.key} aria-label={quotaLabel(q.key)} className="border-t border-border">
                  <td className="py-1.5">
                    <span className="inline-flex items-center gap-2">
                      {quotaLabel(q.key)}
                      {q.over ? (
                        <Badge variant="destructive">Over limit</Badge>
                      ) : q.limit !== null && q.limit !== undefined && q.used >= q.limit ? (
                        <Badge variant="secondary">Limit reached</Badge>
                      ) : null}
                    </span>
                  </td>
                  <td className="py-1.5 text-right font-mono tabular-nums">{q.used.toLocaleString()}</td>
                  <td className="py-1.5 text-right font-mono tabular-nums">
                    {q.limit === null || q.limit === undefined ? 'Unlimited' : q.limit.toLocaleString()}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </CardContent>
    </Card>
  )
}

