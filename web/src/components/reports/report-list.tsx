import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { type ReactNode, useState } from 'react'
import { feedbackReportPageMax, feedbackReportPageStep, fetchFeedbackReports } from '../../lib/api'
import type { FeedbackReport, FeedbackReportFilter } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { useLiveStatusRefresh } from '../feedback/use-live-status-refresh'
import { withQuerySignal } from '../task/use-task-detail'
import { Button } from '../ui/button'
import { feedbackReportListQueryKey } from './query-keys'
import { ReportCard, ReportTaskLinkContext, type ReportTaskLinkProps } from './report-card'

/**
 * One report read and its refresh (component-feedback-reports invariant 27):
 * a frame of the workspace stream refreshes it through the
 * `['feedback-reports', workspace]` prefix; while it shows at least one open
 * report and the tab is visible it reads again every 60 seconds; while a
 * served report carries `links_pending` it reads again after 10 seconds, at
 * most six times in a row. A read that shows no report sets no timer. The
 * timer is `useLiveStatusRefresh`, so the visibility, online, in-flight, and
 * unmount rules of the live-status reads hold here too. No focus, reconnect,
 * or automatic retry read runs, so nothing skips that budget.
 *
 * The list is one read of the newest `limit` reports, so every report it
 * shows comes from one answer.
 */
export function useReportList(filter: FeedbackReportFilter) {
  const { workspace } = useWorkspaceSelection()
  const [limit, setLimit] = useState(feedbackReportPageStep)
  const queryKey = [...feedbackReportListQueryKey(workspace, filter), limit] as const
  const reports = useQuery({
    queryKey,
    queryFn: (context) => withQuerySignal(context, (signal) => fetchFeedbackReports(workspace, filter, limit, signal)),
    enabled: Boolean(workspace),
    // A larger limit keeps the shown reports until its answer arrives.
    placeholderData: keepPreviousData,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
  const items = reports.data?.items ?? []
  const total = reports.data?.total ?? 0
  useLiveStatusRefresh({
    queryKey,
    shown: items.filter((report) => report.state === 'open').length,
    pending: items.some((report) => report.links_pending),
    hasAnswer: reports.data !== undefined,
    dataUpdatedAt: reports.dataUpdatedAt,
    errorUpdatedAt: reports.errorUpdatedAt,
    isFetching: reports.isFetching,
    isPaused: reports.isPaused,
    refetch: reports.refetch,
  })
  const showMore =
    total > items.length && limit < feedbackReportPageMax
      ? () => setLimit((current) => Math.min(current + feedbackReportPageStep, feedbackReportPageMax))
      : undefined
  return { reports, items, total, limit, showMore }
}

/**
 * A list of report cards with its load, failure, and size. `quiet` renders
 * nothing while the read loads or when it serves no report, so a page with no
 * report shows no new part (req-production-feedback AC-6.1).
 */
export function ReportList({
  filter,
  label,
  quiet = false,
  emptyText,
  extra,
  heading,
  className,
}: {
  filter: FeedbackReportFilter
  label: string
  quiet?: boolean
  emptyText?: string
  extra?: (report: FeedbackReport) => ReactNode
  // Shown above the list, and only when the list renders something.
  heading?: ReactNode
  className?: string
}) {
  const { reports, items, total, limit, showMore } = useReportList(filter)
  // Reading the update time re-renders the cards after each timed read, so
  // their relative times stay current even when the answer is unchanged.
  void reports.dataUpdatedAt
  const refreshFailed = reports.isRefetchError && (
    <p role="status" data-report-refresh-error className="text-xs text-failure">
      The last refresh of the reports failed. This list shows the reports from the read before it.
    </p>
  )
  if (reports.isPending) {
    if (quiet) return null
    return (
      <p role="status" className="py-3 text-sm text-muted">
        Loading reports…
      </p>
    )
  }
  if (reports.data === undefined)
    return (
      <div className={className}>
        {heading}
        <div role="alert" data-report-list-error className="flex flex-wrap items-center gap-2 py-2 text-sm">
          <span className="text-failure">The reports did not load.</span>
          <Button size="sm" variant="secondary" disabled={reports.isFetching} onClick={() => void reports.refetch()}>
            {reports.isFetching ? 'Loading…' : 'Try again'}
          </Button>
        </div>
      </div>
    )
  if (items.length === 0) {
    if (quiet) return refreshFailed ? <div className={className}>{refreshFailed}</div> : null
    return (
      <div className={cn('space-y-1 py-3', className)}>
        <p className="text-sm text-muted">{emptyText}</p>
        {refreshFailed}
      </div>
    )
  }
  return (
    <section aria-label={label} data-report-list className={cn('min-w-0 space-y-2', className)}>
      {heading}
      <ul className="min-w-0 space-y-2">
        {items.map((report) => (
          <li key={report.id} className="min-w-0">
            <ReportCard report={report} extra={extra?.(report)} />
          </li>
        ))}
      </ul>
      {refreshFailed}
      {total > items.length && (
        <div className="flex flex-wrap items-center gap-2 text-xs text-muted">
          <span data-report-list-count>
            This list shows the newest {items.length} of {total} reports.
            {!showMore && limit >= feedbackReportPageMax && ` It shows at most ${feedbackReportPageMax} at a time.`}
          </span>
          {showMore && (
            <Button size="sm" variant="secondary" disabled={reports.isFetching} onClick={showMore}>
              {reports.isFetching && reports.isPlaceholderData ? 'Loading…' : 'Show more reports'}
            </Button>
          )}
        </div>
      )}
    </section>
  )
}

/**
 * A merged task's open reports, after its live status
 * (component-feedback-reports invariant 26; req-production-feedback AC-6.1).
 * A task with no open report shows no new part.
 */
export function TaskReports({ taskId }: { taskId: string }) {
  return (
    <ReportList
      filter={{ task_id: taskId }}
      label="Open reports"
      quiet
      className="mt-4"
      heading={<h3 className="text-[11px] font-medium uppercase tracking-wide text-faint">Open reports</h3>}
    />
  )
}

/**
 * The crate's open reports, each with the member task it belongs to the crate
 * through, or the crate link itself (component-feedback-reports invariants 21
 * and 26; req-production-feedback AC-6.2). A task the cards name opens in the
 * crate page's task sheet, as the crate's own task links do
 * (component-crates invariant 37).
 */
export function CrateReports({ crateId }: { crateId: string }) {
  return (
    <ReportTaskLinkContext.Provider value={CrateTaskLink}>
      <ReportList
        filter={{ crate_id: crateId }}
        label="Open reports of this crate"
        quiet
        heading={
          <h2 data-crate-reports className="text-sm font-semibold">
            Open reports
          </h2>
        }
        extra={(report) => <CrateMembership report={report} crateId={crateId} />}
      />
    </ReportTaskLinkContext.Provider>
  )
}

function CrateTaskLink({ taskId, className, children }: ReportTaskLinkProps) {
  return (
    <Link to="." search={(previous: Record<string, unknown>) => ({ ...previous, task: taskId })} className={className}>
      {children}
    </Link>
  )
}

function CrateMembership({ report, crateId }: { report: FeedbackReport; crateId: string }) {
  const members = report.links.filter((link) => link.task?.crate?.id === crateId)
  const direct = report.links.some((link) => link.crate?.id === crateId)
  return (
    <p data-report-crate-member className="min-w-0 text-xs text-muted">
      {members.length > 0 && (
        <>
          In this crate through {members.length === 1 ? 'the member task' : 'the member tasks'}{' '}
          {members.map((link, index) => (
            <span key={link.id}>
              {index > 0 && ', '}
              <bdi className="text-foreground [overflow-wrap:anywhere]">{link.task?.title}</bdi>
            </span>
          ))}
          .{direct && ' '}
        </>
      )}
      {direct && 'The report links to this crate itself.'}
    </p>
  )
}
