import { hashKey, useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { type ReactNode, useEffect, useState } from 'react'
import { feedbackReportPageSize, fetchFeedbackReports } from '../../lib/api'
import type { FeedbackReport, FeedbackReportFilter } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { useLiveStatusRefresh } from '../feedback/use-live-status-refresh'
import { withQuerySignal } from '../task/use-task-detail'
import { Button } from '../ui/button'
import { feedbackReportListQueryKey } from './query-keys'
import { ReportCard, ReportTaskLinkContext, type ReportTaskLinkProps } from './report-card'

/**
 * One page of a report list and its refresh (component-feedback-reports
 * invariants 26 and 27). The page is one read of 50 reports at the shown
 * offset, so every report it shows comes from one answer, and the served
 * offset and total reach every report the server holds, in the server's
 * order.
 *
 * - A frame of the workspace stream refreshes it through the
 *   `['feedback-reports', workspace]` prefix. While it shows at least one
 *   open report and the tab is visible it reads again every 60 seconds.
 *   While a served report carries `links_pending` it reads again after 10
 *   seconds, at most six times in a row. A read that shows no report sets no
 *   timer. The timer is `useLiveStatusRefresh`, so the visibility, online,
 *   in-flight, and unmount rules of the live-status reads hold here too. No
 *   focus, reconnect, or automatic retry read runs, so nothing skips that
 *   budget.
 * - A refresh keeps the position. When the served total no longer reaches the
 *   position, the list moves to the last page that the total holds.
 * - Another workspace or other filters start again at the first page.
 * - While a page loads, the list keeps the page before it only when it has
 *   the same workspace and filters. Otherwise it shows that it loads.
 */
export function useReportList(filter: FeedbackReportFilter) {
  const { workspace } = useWorkspaceSelection()
  const listKey = feedbackReportListQueryKey(workspace, filter)
  const listHash = hashKey(listKey)
  const [position, setPosition] = useState({ list: listHash, offset: 0 })
  // Other filters start at the first page, and the position of the filters
  // before is dropped, so coming back to them starts at the first page too.
  if (position.list !== listHash) setPosition({ list: listHash, offset: 0 })
  const offset = position.list === listHash ? position.offset : 0
  const queryKey = [...listKey, offset] as const
  const reports = useQuery({
    queryKey,
    queryFn: (context) => withQuerySignal(context, (signal) => fetchFeedbackReports(workspace, filter, offset, signal)),
    enabled: Boolean(workspace),
    placeholderData: (previous, previousQuery) =>
      previousQuery && hashKey(previousQuery.queryKey.slice(0, listKey.length)) === listHash ? previous : undefined,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
  const page = reports.data
  const items = page?.items ?? []
  const total = page?.total ?? 0
  const servedOffset = page?.offset ?? offset
  useLiveStatusRefresh({
    queryKey,
    shown: items.filter((report) => report.state === 'open').length,
    pending: items.some((report) => report.links_pending),
    hasAnswer: page !== undefined,
    dataUpdatedAt: reports.dataUpdatedAt,
    errorUpdatedAt: reports.errorUpdatedAt,
    isFetching: reports.isFetching,
    isPaused: reports.isPaused,
    refetch: reports.refetch,
  })
  // A refresh whose total no longer reaches this page moves to the last
  // page that the total holds.
  const beyond = !reports.isPlaceholderData && page !== undefined && items.length === 0 && servedOffset > 0
  useEffect(() => {
    if (!beyond) return
    const last = total > 0 ? Math.floor((total - 1) / feedbackReportPageSize) * feedbackReportPageSize : 0
    setPosition({ list: listHash, offset: last })
  }, [beyond, total, listHash])
  const move = (next: number) => setPosition({ list: listHash, offset: Math.max(0, next) })
  const next = servedOffset + items.length < total ? () => move(servedOffset + items.length) : undefined
  const previous = servedOffset > 0 ? () => move(servedOffset - feedbackReportPageSize) : undefined
  return { reports, items, total, servedOffset, next, previous }
}

/**
 * A list of report cards with its load, failure, and pages. `quiet` renders
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
  const { reports, items, total, servedOffset, next, previous } = useReportList(filter)
  // Reading the update time re-renders the cards after each timed read, so
  // their relative times stay current even when the answer is unchanged.
  void reports.dataUpdatedAt
  const refreshFailed = reports.isRefetchError && (
    <p role="status" data-report-refresh-error className="text-xs text-failure">
      The last refresh of the reports failed. This list shows the reports from the read before it.
    </p>
  )
  if (reports.data === undefined && reports.isFetching) {
    if (quiet) return null
    return (
      <p role="status" data-report-list-loading className="py-3 text-sm text-muted">
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
  if (items.length === 0 && total === 0) {
    if (quiet) return refreshFailed ? <div className={className}>{refreshFailed}</div> : null
    return (
      <div className={cn('space-y-1 py-3', className)}>
        <p className="text-sm text-muted">{emptyText}</p>
        {refreshFailed}
      </div>
    )
  }
  const loadingPage = reports.isPlaceholderData
  return (
    <section aria-label={label} data-report-list className={cn('min-w-0 space-y-2', className)}>
      {heading}
      <ul className={cn('min-w-0 space-y-2', loadingPage && 'opacity-60')} aria-busy={loadingPage || undefined}>
        {items.map((report) => (
          <li key={report.id} className="min-w-0">
            <ReportCard report={report} extra={extra?.(report)} />
          </li>
        ))}
      </ul>
      {refreshFailed}
      {(next || previous) && (
        <nav
          aria-label={`Pages of ${label.toLowerCase()}`}
          className="flex flex-wrap items-center gap-2 text-xs text-muted"
        >
          <span data-report-list-count>
            {loadingPage
              ? 'Loading the next page…'
              : `This list shows reports ${servedOffset + 1} to ${servedOffset + items.length} of ${total}, newest first.`}
          </span>
          <Button size="sm" variant="secondary" disabled={!previous || reports.isFetching} onClick={previous}>
            Previous page
          </Button>
          <Button size="sm" variant="secondary" disabled={!next || reports.isFetching} onClick={next}>
            Next page
          </Button>
        </nav>
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
