import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ArrowLeft } from 'lucide-react'
import { useEffect, useRef } from 'react'
import { fetchFeedbackReport } from '../../lib/api'
import type { FeedbackReport, InboxItem } from '../../lib/types'
import { cn, fullTime, relativeTime } from '../../lib/utils'
import { useLiveStatusRefresh } from '../feedback/use-live-status-refresh'
import { feedbackReportQueryKey } from '../reports/query-keys'
import { ReportCard, Untrusted } from '../reports/report-card'
import { reportActBlocked, useReportAct } from '../reports/use-report-act'
import { withQuerySignal } from '../task/use-task-detail'
import { DecisionDetail } from './decision-card'
import { KindPill } from './inbox-kind'
import { reportRowTitle } from './inbox-review-list'
import type { InboxSearch } from './inbox-search'
import { nextActionsQueryKey } from './use-next-actions'
import { ProposalDetail } from './proposal-card'

// The detail view beside the decision list (req-operator-next-action AC-2.8).
// From md up it is always shown and pinned while the page scrolls, with its
// body scrolling inside and its choices kept in view. Below md it replaces
// the list only while `selected` names an item, with a Back link that clears
// `selected` and keeps every other filter. The item's view is keyed by
// workspace and item, so its reads, composer and mutations start fresh on
// every selection and a late result never shows under another one. Below md
// the row that opened the item has just gone, so focus moves to the item's
// heading; it stays put from md up, where the list is still on screen.
export function InboxReviewDetail({
  item,
  itemKey,
  workspace,
  explicit,
  search,
  onActed,
}: {
  item: InboxItem | undefined
  itemKey: string | undefined
  workspace: string
  explicit: boolean
  search: InboxSearch
  onActed: () => void
}) {
  const root = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!explicit || !itemKey || window.matchMedia('(min-width: 48rem)').matches) return
    root.current?.querySelector<HTMLElement>('[data-detail-title]')?.focus({ preventScroll: true })
    root.current?.scrollIntoView({ block: 'start' })
  }, [explicit, itemKey])
  return (
    <div
      ref={root}
      data-inbox-detail={itemKey}
      className={cn(
        'min-w-0 scroll-mt-5 flex-col md:sticky md:top-4 md:flex md:max-h-[calc(100dvh-7rem)]',
        explicit ? 'flex' : 'hidden',
      )}
    >
      <Link
        to="/w/$workspaceId/inbox"
        params={{ workspaceId: workspace }}
        search={{ ...search, selected: undefined }}
        resetScroll={false}
        data-inbox-back
        className="mb-2 inline-flex items-center gap-1 self-start text-xs font-medium text-primary hover:underline md:hidden"
      >
        <ArrowLeft className="size-3.5" />
        Back to decisions
      </Link>
      {item?.kind === 'feedback_report' && item.report ? (
        <ReportDetail key={`${workspace}:${itemKey}`} item={item} report={item.report} workspace={workspace} />
      ) : item?.kind === 'document_proposal' && item.proposal ? (
        <ProposalDetail key={`${workspace}:${itemKey}`} item={item} proposal={item.proposal} onActed={onActed} />
      ) : item?.task ? (
        <DecisionDetail key={`${workspace}:${itemKey}`} item={item} task={item.task} onActed={onActed} />
      ) : null}
    </div>
  )
}

const reportDetailKinds: Record<string, string> = {
  error: 'Production error',
  alert: 'Production alert',
  message: 'Production message',
}

/**
 * The selected report from production: the report card of the task, crate,
 * and Monitor pages, with the same acts (component-feedback-reports
 * invariants 25 and 26; req-operator-next-action AC-2.12). The card reads its
 * report through the shared `['feedback-reports', workspace]` prefix, so an
 * act, a stream frame, and the read after an unknown answer refresh it as
 * they refresh every other copy, and the 60-second and pending-link reads of
 * invariant 27 time it. The Inbox answer seeds it, and a newer Inbox answer
 * replaces an older read.
 *
 * No shortcut key reaches the card: it carries no `data-shortcut` control.
 * `ReportActWatcher` follows the card's acts.
 */
function ReportDetail({ item, report, workspace }: { item: InboxItem; report: FeedbackReport; workspace: string }) {
  const queryClient = useQueryClient()
  const queryKey = feedbackReportQueryKey(workspace, report.id)
  const inboxUpdatedAt = queryClient.getQueryState(nextActionsQueryKey(workspace))?.dataUpdatedAt ?? 0
  const query = useQuery({
    queryKey,
    queryFn: (context) => withQuerySignal(context, (signal) => fetchFeedbackReport(workspace, report.id, signal)),
    initialData: report,
    initialDataUpdatedAt: inboxUpdatedAt,
    staleTime: Number.POSITIVE_INFINITY,
    retry: false,
    refetchOnMount: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
  // A newer Inbox answer replaces an older report read, so the card never
  // shows less than the row beside it.
  useEffect(() => {
    const state = queryClient.getQueryState(queryKey)
    if (!state || state.dataUpdatedAt >= inboxUpdatedAt || state.fetchStatus === 'fetching') return
    queryClient.setQueryData(queryKey, report, { updatedAt: inboxUpdatedAt })
  }, [queryClient, queryKey, report, inboxUpdatedAt])
  const shown = query.data ?? report
  useLiveStatusRefresh({
    queryKey,
    shown: shown.state === 'open' ? 1 : 0,
    pending: shown.links_pending,
    hasAnswer: query.data !== undefined,
    dataUpdatedAt: query.dataUpdatedAt,
    errorUpdatedAt: query.errorUpdatedAt,
    isFetching: query.isFetching,
    isPaused: query.isPaused,
    refetch: query.refetch,
  })

  const title = reportRowTitle(shown.summary)
  return (
    <article
      data-inbox-item="feedback_report"
      aria-label={title || 'Report from production'}
      className="flex min-h-0 min-w-0 flex-1 flex-col rounded-xl border border-border bg-card md:overflow-hidden"
    >
      <div className="min-h-0 min-w-0 flex-1 space-y-3 overflow-y-auto px-5 pb-5 pt-5">
        <div className="flex flex-wrap items-center gap-2">
          <KindPill kind="feedback_report" label={reportDetailKinds[shown.kind] ?? 'Production report'} />
          {item.last_event_at && (
            <time
              data-report-entered
              dateTime={item.last_event_at}
              title={fullTime(item.last_event_at)}
              className="ml-auto text-xs text-faint"
            >
              Added {relativeTime(item.last_event_at)}
            </time>
          )}
        </div>
        {/* The heading is the row's title, clamped to three lines; the card
            below prints the whole summary. */}
        <h2
          data-detail-title
          tabIndex={-1}
          title={title}
          className="line-clamp-3 break-words text-lg font-semibold leading-snug outline-none"
        >
          <Untrusted>{title}</Untrusted>
        </h2>
        <ReportCard report={shown} />
      </div>
    </article>
  )
}

/**
 * Follows the acts of one report the Inbox lists. The card's act state is
 * shared by workspace and report outside any component, so this sees an act
 * sent from the detail even after the selection moved on. When an act that
 * was on its way or unresolved ends, the Inbox and the attention count of the
 * captured workspace read again. `recorded` is true only for an act this page
 * sent that the server recorded (200). A refusal, a retryable answer, and an
 * unknown answer resolve nothing (component-next-action invariant 37).
 */
export function ReportActWatcher({
  workspace,
  reportId,
  onSettled,
}: {
  workspace: string
  reportId: string
  onSettled: (recorded: boolean) => void
}) {
  const queryClient = useQueryClient()
  const { state } = useReportAct(workspace, reportId)
  const settled = useRef(onSettled)
  settled.current = onSettled
  const previous = useRef(state)
  useEffect(() => {
    const before = previous.current
    previous.current = state
    if (before === state || !reportActBlocked(before) || reportActBlocked(state)) return
    // A 200 ends `sending` with `idle`; every other answer ends it another way.
    settled.current(before.phase === 'sending' && state.phase === 'idle')
    void queryClient.invalidateQueries({ queryKey: nextActionsQueryKey(workspace) })
    void queryClient.invalidateQueries({ queryKey: ['pending-proposals', workspace] })
  }, [state, queryClient, workspace])
  return null
}
