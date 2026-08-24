import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { Activity, ExternalLink, GitCommitHorizontal } from 'lucide-react'
import { useWorkspaceCapability, useWorkspaceSelection } from '../components/app-shell'
import { DriftResolutionForm } from '../components/documents/drift-resolution-form'
import { Badge } from '../components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { fetchMonitorStatus, fetchRequirements } from '../lib/api'

export function MonitorPage() {
  const { workspace } = useWorkspaceSelection()
  const canManageWorkspace = useWorkspaceCapability('manage_workspace')
  const { data: status, error } = useQuery({
    queryKey: ['monitor', workspace],
    queryFn: fetchMonitorStatus,
    enabled: Boolean(workspace),
    refetchInterval: 15_000,
  })
  const { data: requirements = [], isPending: requirementsPending } = useQuery({
    queryKey: ['requirements', workspace, 'monitor-resolution'],
    queryFn: fetchRequirements,
    enabled: Boolean(workspace && (status?.drift_count ?? 0) > 0),
  })
  const confirmedRequirements = requirements.filter((item) => item.current_version?.confirmed)
  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-5xl px-6 py-8">
        <div className="flex items-start justify-between gap-4">
          <div>
            <h1 className="text-xl font-semibold">Repository monitor</h1>
            <p className="mt-1 text-sm text-muted">
              Post-merge signals and out-of-pipeline drift entering the normal Conveyor intake.
            </p>
          </div>
          <Badge variant={status?.enabled ? 'positive' : 'default'}>{status?.enabled ? 'Enabled' : 'Disabled'}</Badge>
        </div>
        {error && (
          <Card className="mt-6 border-failure">
            <CardContent className="text-sm text-failure">{String(error)}</CardContent>
          </Card>
        )}
        <div className="mt-6 grid gap-4 md:grid-cols-3">
          <Metric
            label="Unreconciled drift"
            value={String(status?.drift_count ?? 0)}
            attention={(status?.drift_count ?? 0) > 0}
          />
          <Metric
            label="Oldest drift"
            value={duration(status?.oldest_drift_age ?? 0)}
            attention={(status?.drift_count ?? 0) > 0}
          />
          <Metric
            label="Last successful observation"
            value={
              status?.last_successful_observation
                ? new Date(status.last_successful_observation).toLocaleString()
                : 'Never'
            }
          />
        </div>
        {status?.current_error && (
          <Card className="mt-4 border-failure">
            <CardHeader>
              <CardTitle>Current monitor error</CardTitle>
              <Badge variant="failure">{status.forge_error_category || 'monitor'}</Badge>
            </CardHeader>
            <CardContent>
              <p className="text-sm text-failure">{status.current_error}</p>
              {status.backoff_until && (
                <p className="mt-2 text-xs text-muted">
                  Backoff until {new Date(status.backoff_until).toLocaleString()}
                </p>
              )}
            </CardContent>
          </Card>
        )}
        <Card className="mt-6">
          <CardHeader>
            <CardTitle>Unreconciled changes</CardTitle>
            <Badge variant={(status?.drift_count ?? 0) > 0 ? 'attention' : 'positive'}>
              {status?.drift_count ?? 0}
            </Badge>
          </CardHeader>
          <CardContent className="space-y-2">
            {(status?.drift ?? []).map((item) => (
              <div key={item.id} className="flex items-start gap-3 rounded-lg border border-border p-3">
                <GitCommitHorizontal className="mt-0.5 size-4 text-primary" />
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge variant="mono">{item.kind}</Badge>
                    <span className="text-sm font-medium">{item.repository}</span>
                  </div>
                  <p className="mt-1 truncate font-mono text-xs text-faint">{item.commit_sha || item.id}</p>
                  <p className="mt-1 text-xs text-muted">Detected {new Date(item.detected_at).toLocaleString()}</p>
                  {canManageWorkspace && (
                    <DriftResolutionForm
                      drift={item}
                      surface="monitor"
                      requirements={confirmedRequirements}
                      requirementsPending={requirementsPending}
                      workspace={workspace}
                    />
                  )}
                </div>
                <div className="flex gap-2">
                  <Link
                    to="/tasks/$taskId"
                    params={{ taskId: item.task_id }}
                    className="text-xs text-primary hover:underline"
                  >
                    Task
                  </Link>
                  <a href={item.source_url} target="_blank" rel="noreferrer" aria-label="Open source signal">
                    <ExternalLink className="size-4 text-muted" />
                  </a>
                </div>
              </div>
            ))}
            {(status?.drift_count ?? 0) === 0 && (
              <p className="py-4 text-center text-sm text-muted">No unreconciled repository changes.</p>
            )}
          </CardContent>
        </Card>
        <Card className="mt-4">
          <CardHeader>
            <CardTitle>Recent observations</CardTitle>
            <Badge>{status?.observations?.length ?? 0}</Badge>
          </CardHeader>
          <CardContent className="space-y-2">
            {[...(status?.observations ?? [])]
              .reverse()
              .slice(0, 20)
              .map((item) => (
                <div
                  key={`${item.kind}:${item.occurrence_id}`}
                  className="flex items-center gap-3 border-b border-border py-2 last:border-0"
                >
                  <Activity className="size-4 text-muted" />
                  <Badge variant="mono">{item.kind}</Badge>
                  <span className="min-w-0 flex-1 truncate text-xs">
                    {item.repository} · {item.occurrence_id}
                  </span>
                  {item.task_outcome && (
                    <Badge variant={item.task_outcome === 'created' ? 'accent' : 'default'}>{item.task_outcome}</Badge>
                  )}
                  {item.deduplicated_count > 0 && (
                    <Badge>
                      {item.deduplicated_count} duplicate{item.deduplicated_count === 1 ? '' : 's'}
                    </Badge>
                  )}
                  {item.task_id && (
                    <Link
                      to="/tasks/$taskId"
                      params={{ taskId: item.task_id }}
                      className="text-xs text-primary hover:underline"
                    >
                      Task
                    </Link>
                  )}
                </div>
              ))}
          </CardContent>
        </Card>
      </div>
    </div>
  )
}

function Metric({ label, value, attention = false }: { label: string; value: string; attention?: boolean }) {
  return (
    <Card>
      <CardContent>
        <p className="text-xs text-muted">{label}</p>
        <p className={`mt-2 text-lg font-semibold ${attention ? 'text-attention' : ''}`}>{value}</p>
      </CardContent>
    </Card>
  )
}

function duration(nanoseconds: number) {
  if (!nanoseconds) return '0'
  const seconds = Math.floor(nanoseconds / 1_000_000_000)
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`
  return `${Math.floor(seconds / 86400)}d`
}
