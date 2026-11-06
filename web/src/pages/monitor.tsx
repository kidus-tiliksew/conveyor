import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { Activity, ExternalLink, GitCommitHorizontal } from 'lucide-react'
import { useWorkspaceCapability, useWorkspaceSelection } from '../components/app-shell'
import { DriftResolutionForm } from '../components/documents/drift-resolution-form'
import { Badge } from '../components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { fetchMonitorStatus, fetchRequirements } from '../lib/api'
import { PageHeader } from '../components/ui/page-header'
import { dateTimeLabel } from '../lib/utils'

export function MonitorPage() {
  const { workspace } = useWorkspaceSelection()
  const canManageWorkspace = useWorkspaceCapability('manage_workspace')
  const { data: status, error } = useQuery({
    queryKey: ['monitor', workspace],
    queryFn: () => fetchMonitorStatus(workspace),
    enabled: Boolean(workspace),
    refetchInterval: 15_000,
  })
  const { data: requirements = [], isPending: requirementsPending } = useQuery({
    queryKey: ['requirements', workspace, 'monitor-resolution'],
    queryFn: () => fetchRequirements(workspace),
    enabled: Boolean(workspace && (status?.drift_count ?? 0) > 0),
  })
  const confirmedRequirements = requirements.filter((item) => item.current_version?.confirmed)
  return (
    <div className="flex h-full flex-col">
      <PageHeader
        title="Repository monitor"
        meta={
          <Badge variant={status?.enabled ? 'positive' : 'default'}>{status?.enabled ? 'Enabled' : 'Disabled'}</Badge>
        }
        description="Post-merge signals and out-of-pipeline drift entering the normal Conveyor intake."
      />
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="max-w-6xl px-4 py-5 lg:px-6">
          {error && (
            <Card className="mb-4 border-failure/40">
              <CardContent className="text-sm text-failure">{String(error)}</CardContent>
            </Card>
          )}
          <div className="grid gap-3 md:grid-cols-3">
            <Metric
              label="Unreconciled drift"
              value={String(status?.drift_count ?? 0)}
              attention={(status?.drift_count ?? 0) > 0}
            />
            <Metric
              label="Oldest drift"
              value={duration(status?.oldest_drift_age ?? 0)}
              attention={(status?.drift_count ?? 0) > 0}
              title="Age of the oldest unreconciled signal"
            />
            <Metric
              label="Last successful observation"
              value={
                isSetTimestamp(status?.last_successful_observation)
                  ? new Date(status.last_successful_observation).toLocaleString()
                  : 'Never'
              }
            />
          </div>
          {status?.current_error && (
            <Card className="mt-3 border-failure/40">
              <CardHeader>
                <CardTitle>Current monitor error</CardTitle>
                <Badge variant="failure">{status.forge_error_category || 'monitor'}</Badge>
              </CardHeader>
              <CardContent>
                <p className="break-words font-mono text-xs leading-5 text-foreground/85">{status.current_error}</p>
                {isSetTimestamp(status.backoff_until) && (
                  <p className="mt-2 text-xs text-muted">Backoff until {dateTimeLabel(status.backoff_until)}</p>
                )}
              </CardContent>
            </Card>
          )}
          <Card className="mt-3">
            <CardHeader>
              <CardTitle>Unreconciled changes</CardTitle>
              <Badge variant={(status?.drift_count ?? 0) > 0 ? 'attention' : 'positive'}>
                {status?.drift_count ?? 0}
              </Badge>
            </CardHeader>
            <CardContent className="divide-y divide-border py-0">
              {(status?.drift ?? []).map((item) => (
                <div key={item.id} className="flex flex-wrap items-center gap-x-6 gap-y-2 py-3">
                  <div className="min-w-0 flex-[1_1_18rem]">
                    <div className="flex flex-wrap items-center gap-2">
                      <Badge variant="mono">{item.kind}</Badge>
                      <span className="text-[13px] font-medium">{item.repository}</span>
                    </div>
                    <p className="mt-1 flex min-w-0 items-center gap-2 text-xs text-muted">
                      <GitCommitHorizontal aria-hidden="true" className="size-3.5 shrink-0 text-faint" />
                      <span className="min-w-0 truncate font-mono text-[11px]" title={item.commit_sha || item.id}>
                        {item.commit_sha ? item.commit_sha.slice(0, 12) : item.id}
                      </span>
                      <span className="shrink-0" title={new Date(item.detected_at).toLocaleString()}>
                        Detected {age(item.detected_at)} ago
                      </span>
                    </p>
                  </div>
                  <div className="flex shrink-0 items-center gap-2 [&_select]:border-transparent [&_select]:bg-transparent [&_select]:shadow-none [&_select:hover]:border-edge [&_select:focus]:border-primary">
                    {canManageWorkspace && (
                      <DriftResolutionForm
                        drift={item}
                        surface="monitor"
                        requirements={confirmedRequirements}
                        requirementsPending={requirementsPending}
                        workspace={workspace}
                      />
                    )}
                    <Link
                      to="/w/$workspaceId/tasks/$taskId"
                      params={{ workspaceId: workspace, taskId: item.task_id }}
                      className="text-xs font-medium text-primary hover:underline"
                    >
                      Task
                    </Link>
                    <a href={item.source_url} target="_blank" rel="noreferrer" aria-label="Open source signal">
                      <ExternalLink className="size-4 text-faint hover:text-foreground" />
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
                      <Badge variant={item.task_outcome === 'created' ? 'accent' : 'default'}>
                        {item.task_outcome}
                      </Badge>
                    )}
                    {item.deduplicated_count > 0 && (
                      <Badge>
                        {item.deduplicated_count} duplicate{item.deduplicated_count === 1 ? '' : 's'}
                      </Badge>
                    )}
                    {item.task_id && (
                      <Link
                        to="/w/$workspaceId/tasks/$taskId"
                        params={{ workspaceId: workspace, taskId: item.task_id }}
                        className="text-xs text-primary hover:underline"
                      >
                        Task
                      </Link>
                    )}
                    {item.last_error && <p className="max-w-md text-xs text-failure">{item.last_error}</p>}
                  </div>
                ))}
            </CardContent>
          </Card>
        </div>
      </div>
    </div>
  )
}

function Metric({
  label,
  value,
  attention = false,
  title,
}: {
  label: string
  value: string
  attention?: boolean
  title?: string
}) {
  return (
    <Card>
      <CardContent>
        <p className="text-xs text-muted">{label}</p>
        <p className={`mt-2 text-lg font-semibold ${attention ? 'text-attention' : ''}`} title={title}>
          {value}
        </p>
      </CardContent>
    </Card>
  )
}

function age(detectedAt: string) {
  return duration(Math.max(0, Date.now() - new Date(detectedAt).getTime()) * 1_000_000)
}

function duration(nanoseconds: number) {
  if (!nanoseconds) return '0'
  const seconds = Math.floor(nanoseconds / 1_000_000_000)
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`
  return `${Math.floor(seconds / 86400)}d`
}

// isSetTimestamp returns false for absent values and for the Go zero time
// "0001-01-01T00:00:00Z" that encoding/json emits when omitempty is used on a
// time.Time field. The server now uses omitzero, but this guard keeps the UI
// correct against stale data or older server versions.
function isSetTimestamp(value?: string): value is string {
  if (!value) return false
  return new Date(value).getUTCFullYear() > 1
}
