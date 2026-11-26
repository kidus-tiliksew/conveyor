import { useQuery } from '@tanstack/react-query'
import { Rocket } from 'lucide-react'
import { fetchEnvironments, fetchTaskLiveStatus } from '../../lib/api'
import { environmentsQueryKey, taskLiveStatusQueryKey } from '../../lib/query-keys'
import type { CrateEnvironmentLine, EnvironmentRow, LiveStatus, TaskEnvironmentStatus } from '../../lib/types'
import { absoluteTime, cn, relativeTime } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { withQuerySignal } from '../task/use-task-detail'
import { Badge } from '../ui/badge'
import { LIVE_STATUS_REFRESH_MS, useLiveStatusRefresh } from './use-live-status-refresh'
import { WatcherState } from './watcher-state'

/**
 * The interval of the environment rows read: 60 seconds while its answer
 * shows a row, and no timer for an empty answer, so a page with nothing to
 * show sends no timed request (component-web-dashboard invariant 15;
 * `req-quiet-idle-runtime` REQ-1). TanStack Query stops an interval while the
 * tab is hidden, because no query sets `refetchIntervalInBackground`, and
 * stops it when the observer unmounts. The task live-status read and the
 * crate read use `useLiveStatusRefresh`, which adds the pending reads.
 */
export function liveStatusInterval(shown: number | undefined) {
  return shown ? LIVE_STATUS_REFRESH_MS : false
}

const statusLabels: Record<LiveStatus, string> = {
  live: 'Live',
  not_live_yet: 'Not live yet',
  no_longer_live: 'No longer live',
  unknown: 'Unknown',
}

function statusLabel(status: string) {
  return statusLabels[status as LiveStatus] ?? status.replaceAll('_', ' ')
}

function ServedTime({ value }: { value: string }) {
  return (
    <time dateTime={value} title={absoluteTime(value)}>
      {relativeTime(value)}
    </time>
  )
}

/**
 * A merged task's status in each relevant environment (req-production-feedback
 * AC-2.2, AC-3.2, AC-3.6; component-production-feedback invariant 22). Shared
 * task facts render it, so the sheet and the full page show the same lines. It
 * reads `GET /v1/tasks/{id}`, the task read that serves `environments`, and
 * the workspace's environment rows for each line's listening state, joined by
 * environment ID. Both reads need view_workspace. An empty or failed answer
 * renders nothing, so a workspace with no environment shows the page as before.
 * The lines are records to read: no delivery or gate control reads them
 * (req-production-feedback AC-11.1).
 */
export function TaskLiveStatus({ taskId }: { taskId: string }) {
  const { workspace } = useWorkspaceSelection()
  const queryKey = taskLiveStatusQueryKey(workspace, taskId)
  const statuses = useQuery({
    queryKey,
    queryFn: (context) => withQuerySignal(context, (signal) => fetchTaskLiveStatus(workspace, taskId, signal)),
    enabled: Boolean(workspace && taskId),
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
  // Reading the update time re-renders the lines after each successful timed
  // read, so their relative times stay current even when the answer is
  // unchanged. A failed read keeps the last served lines.
  void statuses.dataUpdatedAt
  const lines = statuses.data?.environments ?? []
  // While the answer is pending, the read runs again after 10 seconds, at
  // most six times in a row, and otherwise every 60 seconds while a line
  // shows (component-production-feedback invariant 23).
  useLiveStatusRefresh({
    queryKey,
    shown: lines.length,
    pending: statuses.data?.environments_pending === true,
    hasAnswer: statuses.data !== undefined,
    dataUpdatedAt: statuses.dataUpdatedAt,
    errorUpdatedAt: statuses.errorUpdatedAt,
    isFetching: statuses.isFetching,
    isPaused: statuses.isPaused,
    refetch: statuses.refetch,
  })
  const environments = useQuery({
    queryKey: environmentsQueryKey(workspace, false),
    queryFn: (context) => withQuerySignal(context, (signal) => fetchEnvironments(workspace, false, signal)),
    enabled: Boolean(workspace) && lines.length > 0,
    retry: false,
    refetchOnWindowFocus: false,
    refetchInterval: lines.length > 0 ? LIVE_STATUS_REFRESH_MS : false,
  })
  if (lines.length === 0) return null
  const rows = new Map((environments.data ?? []).map((row) => [row.id, row]))
  return (
    <section aria-label="Live status" data-task-live-status className="mt-4">
      <h3 className="text-[11px] font-medium uppercase tracking-wide text-faint">Live status</h3>
      <ul className="mt-1.5 divide-y divide-border rounded-md border border-border">
        {lines.map((line) => (
          <TaskLiveLine key={line.environment.id} line={line} row={rows.get(line.environment.id)} />
        ))}
      </ul>
    </section>
  )
}

function TaskLiveLine({ line, row }: { line: TaskEnvironmentStatus; row: EnvironmentRow | undefined }) {
  const services = line.services ?? []
  return (
    <li
      data-live-environment={line.environment.id}
      data-live-status={line.status}
      className="space-y-1 px-3 py-2 text-sm"
    >
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        <span className="min-w-0 truncate font-medium">{line.environment.name}</span>
        <Badge variant={line.status === 'live' ? 'positive' : 'default'}>{statusLabel(line.status)}</Badge>
      </div>
      <p className="text-xs leading-5 text-muted">
        {line.status === 'live' && (
          <>
            {services.length > 0 && <span>Runs in {services.join(', ')}. </span>}
            {line.live_since && (
              <span>
                Live since <ServedTime value={line.live_since} />.{' '}
              </span>
            )}
          </>
        )}
        {line.status === 'not_live_yet' && line.on_tracked_branch === true && (
          <span>A tracked branch has the merge. </span>
        )}
        {line.status === 'not_live_yet' && line.on_tracked_branch === false && (
          <span>No tracked branch has the merge yet. </span>
        )}
        {line.status === 'no_longer_live' && <span>A past deploy had this task. No service runs it now. </span>}
        {line.status === 'unknown' && <span>The repository history does not have the merge commit. </span>}
        {line.as_of && (
          <span data-live-as-of>
            As of <ServedTime value={line.as_of} />.
          </span>
        )}
      </p>
      {/* Only an explicit false says that no watcher is listening. A row
          without a listening state shows nothing (req-production-feedback
          AC-2.2). */}
      {row?.listening === false && (
        <WatcherState listening={false} lastSignalAt={row.last_signal_at} className="text-xs" />
      )}
    </li>
  )
}

/**
 * The crate's line for each relevant environment, under the page header
 * (req-production-feedback AC-3.4, AC-3.6; component-production-feedback
 * invariants 18 and 22). It prints the served counts, the served newest live
 * time, the served listening state, and the served as-of time. An empty list
 * renders nothing.
 */
export function CrateLiveLines({ lines }: { lines: CrateEnvironmentLine[] }) {
  if (lines.length === 0) return null
  return (
    <section aria-label="Live status" data-crate-live-status className="rounded-lg border border-border bg-card">
      <ul className="divide-y divide-border">
        {lines.map((line) => (
          <li
            key={line.environment.id}
            data-crate-live-environment={line.environment.id}
            className="flex flex-col gap-1 px-3 py-2 text-sm sm:flex-row sm:flex-wrap sm:items-baseline sm:gap-x-3"
          >
            <span className="inline-flex min-w-0 items-center gap-1.5 font-medium">
              <Rocket aria-hidden className="size-3.5 shrink-0 text-faint" />
              <span className="truncate">{line.environment.name}</span>
            </span>
            <span data-crate-live-counts className="tabular-nums">
              {line.live_task_count} of {line.merged_task_count} merged{' '}
              {line.merged_task_count === 1 ? 'task' : 'tasks'} {line.live_task_count === 1 ? 'is' : 'are'} live.
            </span>
            <span className="text-muted">
              {line.newest_live_at ? (
                <>
                  Newest went live <ServedTime value={line.newest_live_at} />.
                </>
              ) : (
                'No task is live here yet.'
              )}
            </span>
            {/* The served listening state and last signal of each line, both
                when a watcher is listening and when none is. A line whose
                answer carries no listening state shows none
                (req-production-feedback AC-2.2, AC-3.4). */}
            {typeof line.listening === 'boolean' && (
              <WatcherState listening={line.listening} lastSignalAt={line.last_signal_at} className="text-xs" />
            )}
            {line.as_of && (
              <span data-live-as-of className="text-xs text-faint">
                As of <ServedTime value={line.as_of} />.
              </span>
            )}
          </li>
        ))}
      </ul>
    </section>
  )
}

/**
 * Where a merged crate member is live, from the served `live_in`
 * (req-production-feedback AC-3.4). The crate page shows it only when the crate
 * has environment lines, so a workspace with no environment adds nothing.
 */
export function LiveIn({ names, className }: { names: string[]; className?: string }) {
  return (
    <span data-task-live-in className={cn('flex min-w-0 items-center gap-1', className)}>
      <Rocket aria-hidden className="size-3 shrink-0" />
      <span className="min-w-0 truncate">
        {names.length > 0 ? `Live in ${names.join(', ')}` : 'Not live in an environment'}
      </span>
    </span>
  )
}
