import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ExternalLink } from 'lucide-react'
import { useWorkspaceCapability, useWorkspaceSelection } from '../components/app-shell'
import { DriftResolutionForm } from '../components/documents/drift-resolution-form'
import { Badge } from '../components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { readableError } from '../components/ui/error-state'
import { PageHeader } from '../components/ui/page-header'
import { fetchMonitorStatus, fetchRequirements } from '../lib/api'
import { fullTime } from '../lib/utils'

// The signal kinds in plain words (conveyor:internal/monitor/types.go); an
// unknown kind still reads as words rather than a raw value.
const signalKindLabels: Record<string, string> = {
  post_merge_failure: 'Post-merge failure',
  direct_push: 'Direct push',
  external_pr_merge: 'External PR merge',
  lineaged_merge: 'Tracked merge',
  revert: 'Revert',
}

function signalKindLabel(kind: string) {
  const words = kind.replaceAll('_', ' ')
  return signalKindLabels[kind] ?? words.charAt(0).toUpperCase() + words.slice(1)
}

// The row's controls rest quieter until the row is hovered or holds focus. They
// stay rendered and at full strength on touch, and never leave the tab order.
const restingControls =
  'transition-opacity pointer-fine:opacity-80 group-hover/drift:opacity-100 group-focus-within/drift:opacity-100'

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
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader
        title="Repository monitor"
        description="Post-merge signals and out-of-pipeline drift entering the normal Conveyor intake."
      >
        {status && (
          <Badge variant={status.enabled ? 'positive' : 'default'}>{status.enabled ? 'Enabled' : 'Disabled'}</Badge>
        )}
      </PageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto max-w-5xl px-4 py-6 sm:px-6">
          {error && (
            <Card className="mt-6 border-failure/35">
              <CardContent className="text-sm text-failure">
                {readableError(error, 'Something went wrong. Try again.')}
              </CardContent>
            </Card>
          )}
          <div className="grid grid-cols-2 gap-3 md:grid-cols-3 md:gap-4">
            <Metric
              label="Unreconciled drift"
              value={status ? String(status.drift_count ?? 0) : '–'}
              attention={(status?.drift_count ?? 0) > 0}
            />
            <Metric
              label="Oldest drift"
              value={!status ? '–' : status.drift_count ? duration(status.oldest_drift_age ?? 0) : 'None'}
              attention={(status?.drift_count ?? 0) > 0}
              title="Age of the oldest unreconciled signal"
            />
            <Metric
              className="max-md:col-span-2"
              label="Last successful observation"
              value={
                !status
                  ? '–'
                  : isSetTimestamp(status.last_successful_observation)
                    ? fullTime(status.last_successful_observation)
                    : 'Never'
              }
            />
          </div>
          {status?.current_error && (
            <Card className="mt-4 border-failure/35">
              <CardHeader>
                <CardTitle>Current monitor error</CardTitle>
                <Badge variant="failure" title={status.forge_error_category || undefined}>
                  {status.forge_error_category?.startsWith('forge') ? 'GitHub' : 'Monitor'}
                </Badge>
              </CardHeader>
              <CardContent>
                <p className="text-sm text-foreground">{status.current_error}</p>
                {isSetTimestamp(status.backoff_until) && (
                  <p className="mt-2 text-xs text-muted">Backoff until {fullTime(status.backoff_until)}</p>
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
            <CardContent className="divide-y divide-border py-0">
              {(status?.drift ?? []).map((item) => (
                <div
                  key={item.id}
                  data-drift-row
                  className="group/drift flex flex-wrap items-center gap-x-4 gap-y-2 py-2.5"
                >
                  <div className="min-w-0 flex-1 max-md:basis-[60%] md:basis-80">
                    <div className="flex min-w-0 flex-wrap items-baseline gap-x-2.5 gap-y-0.5">
                      <span className="text-sm font-medium">{item.repository}</span>
                      <span className="text-xs text-muted" title={item.kind}>
                        {signalKindLabel(item.kind)}
                      </span>
                      <span className="font-mono text-xs text-faint" title={item.commit_sha || item.id}>
                        {item.commit_sha ? item.commit_sha.slice(0, 8) : item.id}
                      </span>
                      <span className="text-xs text-muted" title={fullTime(item.detected_at)}>
                        Detected {age(item.detected_at)} ago
                      </span>
                      <Link
                        to="/w/$workspaceId/tasks/$taskId"
                        params={{ workspaceId: workspace, taskId: item.task_id }}
                        className="text-xs text-primary hover:underline"
                      >
                        Task
                      </Link>
                      <a
                        href={item.source_url}
                        target="_blank"
                        rel="noreferrer"
                        aria-label="Open source signal"
                        className="self-center text-muted hover:text-foreground"
                      >
                        <ExternalLink className="size-3.5" />
                      </a>
                    </div>
                    {item.system_design_id && (
                      <p data-drift-scope className="mt-0.5 truncate text-xs text-muted">
                        Design <span className="font-mono">{item.system_design_id}</span>
                        {item.system_design_version != null && ` v${item.system_design_version}`}
                      </p>
                    )}
                  </div>
                  {canManageWorkspace && (
                    <div className={`min-w-0 max-md:order-last max-md:w-full ${restingControls}`}>
                      <DriftResolutionForm
                        drift={item}
                        surface="monitor"
                        compact
                        requirements={confirmedRequirements}
                        requirementsPending={requirementsPending}
                        workspace={workspace}
                      />
                    </div>
                  )}
                </div>
              ))}
              {status && (status.drift_count ?? 0) === 0 && (
                <p className="py-4 text-center text-sm text-muted">No unreconciled repository changes.</p>
              )}
            </CardContent>
          </Card>
          <Card className="mt-4">
            <CardHeader>
              <CardTitle>Recent observations</CardTitle>
              <Badge>{status?.observations?.length ?? 0}</Badge>
            </CardHeader>
            <CardContent className="py-1">
              {status && (status.observations?.length ?? 0) === 0 && (
                <p className="py-4 text-center text-sm text-muted">
                  No signals observed yet. They appear here after the next repository check.
                </p>
              )}
              {[...(status?.observations ?? [])]
                .reverse()
                .slice(0, 20)
                .map((item) => (
                  <div
                    key={`${item.kind}:${item.occurrence_id}`}
                    className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-border py-2 last:border-0"
                  >
                    <span className="text-xs text-muted" title={item.kind}>
                      {signalKindLabel(item.kind)}
                    </span>
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
                    {item.last_error && <p className="basis-full text-xs text-failure">{item.last_error}</p>}
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
  className,
}: {
  label: string
  value: string
  attention?: boolean
  title?: string
  className?: string
}) {
  return (
    <Card className={className}>
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
  const days = Math.floor(seconds / 86400)
  if (days < 60) return `${days}d`
  if (days < 365) {
    const months = Math.round(days / 30.44)
    return `${months} months`
  }
  const years = Math.round((days / 365.25) * 10) / 10
  return `${years} ${years === 1 ? 'year' : 'years'}`
}

// isSetTimestamp returns false for absent values and for the Go zero time
// "0001-01-01T00:00:00Z" that encoding/json emits when omitempty is used on a
// time.Time field. The server now uses omitzero, but this guard keeps the UI
// correct against stale data or older server versions.
function isSetTimestamp(value?: string): value is string {
  if (!value) return false
  return new Date(value).getUTCFullYear() > 1
}
