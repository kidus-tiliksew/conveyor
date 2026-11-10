import claudeIcon from '@lobehub/icons-static-svg/icons/claude-color.svg?raw'
import claudeCodeIcon from '@lobehub/icons-static-svg/icons/claudecode-color.svg?raw'
import codexIcon from '@lobehub/icons-static-svg/icons/codex-color.svg?raw'
import cursorIcon from '@lobehub/icons-static-svg/icons/cursor.svg?raw'
import geminiIcon from '@lobehub/icons-static-svg/icons/gemini-color.svg?raw'
import githubCopilotIcon from '@lobehub/icons-static-svg/icons/githubcopilot.svg?raw'
import grokIcon from '@lobehub/icons-static-svg/icons/grok.svg?raw'
import openaiIcon from '@lobehub/icons-static-svg/icons/openai.svg?raw'
import opencodeIcon from '@lobehub/icons-static-svg/icons/opencode.svg?raw'
import windsurfIcon from '@lobehub/icons-static-svg/icons/windsurf.svg?raw'
import zhipuIcon from '@lobehub/icons-static-svg/icons/zhipu-color.svg?raw'
import { useQueryClient } from '@tanstack/react-query'
import { AlertTriangle, Check, ChevronDown, ChevronUp, Cpu, ExternalLink, Undo2, UserRound } from 'lucide-react'
import { type ReactNode, useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import {
  buildTimeline,
  deriveCurrentExecutionState,
  failedTriage,
  historyNoteEntries,
  type PanelSeat,
  type TimelineEntry,
  userRequestChangesReason,
} from '../../lib/activity'
import {
  defaultReasonCode,
  interventionReasonLabels,
  planRevisionDecisionLabels,
  stageLabels,
} from '../../lib/contracts'
import { type HarnessLogo, type ProviderLogo, selectModelLogo } from '../../lib/model-logo'
import { taskDetailQueryKey } from '../../lib/query-keys'
import type { TaskRouteVariant } from '../../lib/task-route'
import type {
  ActivityItem,
  InterventionAction,
  Job,
  TaskAuditKind,
  TaskEvent,
  TaskEventView,
  TaskHistorySelection,
  TaskReviewSeatMetadata,
  WorkOrder,
} from '../../lib/types'
import { absoluteTime, cn, compactTokens, duration } from '../../lib/utils'
import { useWorkspaceCapability, useWorkspaceMembers, useWorkspaceSelection } from '../app-shell'
import { useNextAction } from '../inbox/use-next-actions'
import { Badge } from '../ui/badge'
import { Disclosure } from '../ui/disclosure'
import { MarkdownProse } from '../ui/markdown-prose'
import { ImplementationEntry } from './implementation-entry'
import { hasInterruptedReviewRecovery, InterruptedReviewRecoveryCard } from './interrupted-review-recovery-card'
import { canRedispatch, FailedTriageCard, RedispatchCard } from './redispatch-card'
import {
  changesComposerHint,
  type GateTone,
  gateAnchorId,
  gateTone,
  isReviewable,
  ReviewPanel,
  useCanRequestTaskChanges,
} from './review-panel'
import { hasReviewRoundRetry, ReviewRoundRetryCard } from './review-round-retry-card'
import { ReviewRoundSummary } from './review-round-summary'
import { ReviewSeatAdd } from './review-seat-add'
import { SeatRiskTier } from './risk-tier'
import {
  interventionUserId,
  jobClaimantUserId,
  type Person,
  PersonAvatar,
  personLabel,
  RailPeople,
  type RailPerson,
  runClaimantUserId,
  stageWork,
  usePersonResolver,
} from './run-by'
import { useCollapsibleContent } from './use-collapsible-content'
import { useTaskAudit } from './use-task-detail'
import { isHistoryRestart, useTaskHistory } from './use-task-history'
import { VerificationEntry } from './verification-entry'
import { hasOrderRecovery, WorkOrderRecoveryCard } from './work-order-recovery-card'

// The gate dot pulses — the timeline's one "waiting on you" signal — in the
// gate card's own tone.
const gateDots: Record<GateTone, string> = {
  positive: 'bg-positive',
  neutral: 'bg-primary',
  alarm: 'bg-attention-dot',
}

// The execution event timeline: the audit log
// rendered as a story — one entry per stage execution, with usage, duration,
// and the harness/model/auth mode that ran it, interleaved with pipeline
// incidents and every recorded decision. Anything actionable "now" — status
// alerts, recovery actions, and the human gate itself — renders as the live
// tail: cause sits directly above prompt, and a recorded decision resolves
// in place into its intervention entry.
// `executionActions` is false for a blueprint anchor: it takes
// no work orders, so redispatch, order recovery, and review recovery
// are affordances for execution that will never happen. The story it does
// have — materialization, child progress, close — still renders in full.
export function Timeline({
  item,
  executionActions = true,
  routeVariant,
}: {
  item: ActivityItem
  executionActions?: boolean
  routeVariant?: TaskRouteVariant
}) {
  const canOperate = useWorkspaceCapability('operate_gates')
  const canRecover = useWorkspaceCapability('recover_work')
  const canRequestChanges = useCanRequestTaskChanges(item.task)
  const members = useWorkspaceMembers()
  const entries = buildTimeline(item, members.data ?? [])
  const nextActionQuery = useNextAction(item.task.id)
  const nextAction = nextActionQuery.data
  const currentExecution = deriveCurrentExecutionState(item, nextAction)
  const { workspace } = useWorkspaceSelection()
  const usageReportedOrderIDs = reportedUsageOrderIDs(item)
  const showGate = isReviewable(item)
  const recoveryGate = item.next_action?.kind === 'merge_conflict'
  const timelineRef = useRef<HTMLElement>(null)
  const gateRef = useRef<HTMLLIElement>(null)
  const [decisionScrollRequest, setDecisionScrollRequest] = useState(0)
  const priorExecutionStatus = useRef(currentExecution?.status)
  const [executionAnnouncement, setExecutionAnnouncement] = useState('')

  // The execution state joins two independently refetched reads (activity and
  // the server's next action), so a live update can pass through a render
  // where they disagree and no state derives. The last derived status is the
  // one a pause interrupts, so that gap neither announces nor resets it.
  useEffect(() => {
    if (!currentExecution) return
    if (priorExecutionStatus.current !== 'paused' && currentExecution.status === 'paused') {
      setExecutionAnnouncement(`${currentExecution.title}. ${currentExecution.nextAction}`)
    }
    priorExecutionStatus.current = currentExecution.status
  }, [currentExecution?.status, currentExecution?.title, currentExecution?.nextAction])

  // A successful human decision explicitly requests one tail scroll after its
  // refreshed activity has rendered. Live event refetches never enter this
  // path, so they preserve the reader's position in the task detail.
  useLayoutEffect(() => {
    if (decisionScrollRequest === 0) return
    const container = scrollableAncestor(timelineRef.current)
    if (container) container.scrollTop = container.scrollHeight
  }, [decisionScrollRequest])

  // Reviewable tasks open scrolled to the gate — the decision point — not
  // the top of a story the reviewer has often already read. The next-action
  // card above the timeline arrives on its own read and grows the page, so the
  // scroll repeats once when that read first lands.
  const nextActionLoaded = nextAction !== undefined
  useEffect(() => {
    if (showGate && (canOperate || (item.at_merge_gate && canRequestChanges) || (canRecover && recoveryGate))) {
      gateRef.current?.scrollIntoView({ block: 'end' })
    }
  }, [
    item.task.id,
    item.at_merge_gate,
    showGate,
    canOperate,
    canRecover,
    canRequestChanges,
    recoveryGate,
    nextActionLoaded,
  ])

  const tail = (
    executionActions
      ? [
          canRecover &&
            hasInterruptedReviewRecovery(item) && {
              key: 'interrupted-review',
              dot: 'bg-attention-dot',
              card: <InterruptedReviewRecoveryCard item={item} />,
            },
          canRecover &&
            hasReviewRoundRetry(item) && {
              key: 'review-retry',
              dot: 'bg-attention-dot',
              card: <ReviewRoundRetryCard item={item} />,
            },
          canRecover &&
            hasOrderRecovery(item, currentExecution) && {
              key: 'order-recovery',
              dot: 'bg-attention-dot',
              card: <WorkOrderRecoveryCard item={item} state={currentExecution} />,
            },
          Boolean(failedTriage(item)) && {
            key: 'failed-triage',
            dot: 'bg-attention-dot',
            card: <FailedTriageCard key={failedTriage(item)?.jobId} item={item} />,
          },
          // One button per act: while the server's next action offers a
          // redispatch-path choice, the next-action card carries it (DEC-61).
          canRecover &&
            canRedispatch(item, nextAction, nextActionQuery.isError) && {
              key: 'redispatch',
              dot: 'bg-edge',
              card: <RedispatchCard item={item} />,
            },
        ]
      : []
  ).filter((entry) => entry !== false)

  return (
    <>
      <section ref={timelineRef} aria-label="Execution event timeline">
        <p className="sr-only" role="status" aria-live="polite" aria-atomic="true">
          {executionAnnouncement}
        </p>
        {/* The current execution state and the stop affordance belong to the
            next-action card above, which prints them once (DEC-61). */}
        <h2 className="mb-4 mt-5 text-sm font-semibold tracking-tight">Activity</h2>
        <ol className="relative space-y-4 before:absolute before:bottom-4 before:left-[7px] before:top-4 before:w-px before:bg-border">
          {entries.map((entry) => (
            <TimelineRow key={keyFor(entry)} entry={entry} item={item} usageReportedOrderIDs={usageReportedOrderIDs} />
          ))}
          {item.event_page?.has_older && item.event_page.older_cursor && (
            // Remounted closed for another task or workspace, which aborts its read.
            <EarlierHistory
              key={`${workspace}:${item.task.id}`}
              item={item}
              workspace={workspace}
              usageReportedOrderIDs={usageReportedOrderIDs}
            />
          )}
          {entries.length === 0 && tail.length === 0 && !showGate && (
            <li className="pl-7 text-sm text-muted">Waiting for the first job to start.</li>
          )}
          {tail.map(({ key, dot, card }) => (
            <li key={key} className="relative pl-7">
              <TimelineDot className={dot} />
              {card}
            </li>
          ))}
          {showGate && (canOperate || (item.at_merge_gate && canRequestChanges) || (canRecover && recoveryGate)) && (
            <li ref={gateRef} id={gateAnchorId(item.task.id)} className="relative pl-7">
              <TimelineDot className={cn('animate-pulse', gateDots[gateTone(item.next_action?.kind)])} />
              <ReviewPanel
                item={item}
                routeVariant={routeVariant}
                onDecisionRecorded={() => setDecisionScrollRequest((request) => request + 1)}
              />
            </li>
          )}
        </ol>
      </section>
      {/* Remounted closed for another task or workspace, which aborts its read. */}
      <TechnicalActivity key={`${workspace}:${item.task.id}`} item={item} workspace={workspace} />
    </>
  )
}

function reportedUsageOrderIDs(item: ActivityItem): Set<string> {
  // Usage availability and aggregation are presentation-only. They never feed
  // a lifecycle or admission decision (DEC-1).
  return new Set((item.work_orders ?? []).filter((order) => order.usage_reported).map((order) => order.id))
}

function usageProvenance(order: WorkOrder): string | undefined {
  return order.self_reported ? 'self-reported' : undefined
}

function usageText(order: WorkOrder, available: boolean): string {
  if (!available) return 'Usage unavailable'
  return [`${compactTokens(order.tokens_in)} in / ${compactTokens(order.tokens_out)} out`, usageProvenance(order)]
    .filter(Boolean)
    .join(' · ')
}

// The technical activity disclosure (component-web-task-surfaces, History
// pages). Its open state is the component's own: closed, it mounts no event
// rows and issues no request; open, it pages the technical selection of the
// task's stored events, at most 50 rows at a time.
function TechnicalActivity({ item, workspace }: { item: ActivityItem; workspace: string }) {
  const [open, setOpen] = useState(false)
  const contentID = useId()
  return (
    <section className="mt-5 rounded-lg border border-border bg-surface/35 text-xs text-muted">
      <button
        type="button"
        aria-expanded={open}
        aria-controls={contentID}
        className="flex w-full cursor-pointer items-center gap-1.5 rounded-lg px-3 py-2.5 text-left font-medium text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
        onClick={() => setOpen((value) => !value)}
      >
        {open ? <ChevronUp aria-hidden className="size-3.5" /> : <ChevronDown aria-hidden className="size-3.5" />}
        Show technical activity
      </button>
      {open && (
        <div id={contentID}>
          <TaskUsage item={item} workspace={workspace} />
          <HistoryPages
            workspace={workspace}
            taskId={item.task.id}
            selection="technical"
            newest=""
            label="Technical activity history"
            emptyText="No technical activity recorded."
            renderRows={(events) => (
              <ol aria-label="Technical activity events" className="divide-y divide-border border-t border-border">
                {events.map((event) => (
                  <li key={event.id} className="px-3 py-2">
                    <div className="flex flex-wrap items-baseline gap-2">
                      <code className="text-[11px] text-foreground">{event.kind}</code>
                      <span className="text-[11px] text-faint">Event {event.id}</span>
                      <time className="ml-auto text-[11px] text-faint">{absoluteTime(event.at)}</time>
                    </div>
                    {(event.audit_available ||
                      event.payload_omitted ||
                      (event.payload && Object.keys(event.payload).length > 0)) && (
                      <AuditDisclosure
                        key={`${workspace}:${item.task.id}:event:${event.id}`}
                        taskId={item.task.id}
                        kind="event"
                        recordId={event.id}
                        label="Event payload"
                        event={event}
                      />
                    )}
                  </li>
                ))}
              </ol>
            )}
          />
        </div>
      )}
    </section>
  )
}

function TaskUsage({ item, workspace }: { item: ActivityItem; workspace: string }) {
  const reported = reportedUsageOrderIDs(item)
  const orders = item.work_orders ?? []
  if (orders.length === 0) return null
  const measured = orders.filter((order) => reported.has(order.id))
  const totals = measured.reduce(
    (sum, order) => ({
      tokensIn: sum.tokensIn + order.tokens_in,
      tokensOut: sum.tokensOut + order.tokens_out,
    }),
    { tokensIn: 0, tokensOut: 0 },
  )
  return (
    <section aria-label="Task usage telemetry" className="border-t border-border px-3 py-3">
      <p className="font-medium text-foreground">Task usage</p>
      <p className="mt-1 text-[11px] tabular-nums">
        {measured.length > 0
          ? `${compactTokens(totals.tokensIn)} in / ${compactTokens(totals.tokensOut)} out across ${measured.length} reported work ${measured.length === 1 ? 'order' : 'orders'}`
          : 'Usage unavailable for every work order'}
        {orders.length > measured.length
          ? ` · ${orders.length - measured.length} ${orders.length - measured.length === 1 ? 'order' : 'orders'} unavailable`
          : ''}
      </p>
      <ul className="mt-2 space-y-1" aria-label="Work-order usage">
        {orders.map((order) => (
          <li key={order.id} className="flex flex-wrap items-baseline gap-x-2 text-[11px] tabular-nums">
            <span className="text-foreground">{stageLabels[order.stage] ?? order.stage}</span>
            <code className="text-faint">{order.id}</code>
            <span className="ml-auto">{usageText(order, reported.has(order.id))}</span>
            <AuditDisclosure
              key={`${workspace}:${item.task.id}:work-order:${order.id}`}
              taskId={item.task.id}
              kind="work-order"
              recordId={order.id}
              label={`Work-order audit ${order.id} (current record)`}
            />
          </li>
        ))}
      </ul>
    </section>
  )
}

// Earlier human history: the bounded view holds the newest human entries, and
// this disclosure pages the older ones from the view's `older_cursor` with the
// same bound, controls, and states as technical activity. Its rows render
// through `noteFor`, so an omitted payload still renders a note.
function EarlierHistory({
  item,
  workspace,
  usageReportedOrderIDs,
}: {
  item: ActivityItem
  workspace: string
  usageReportedOrderIDs: Set<string>
}) {
  const [open, setOpen] = useState(false)
  const contentID = useId()
  const members = useWorkspaceMembers()
  const queryClient = useQueryClient()
  const newest = item.event_page?.older_cursor ?? ''
  return (
    <li className="relative pl-7">
      <TimelineDot className="bg-edge" />
      <section className="rounded-lg border border-border bg-surface/35 text-xs text-muted">
        <button
          type="button"
          aria-expanded={open}
          aria-controls={contentID}
          className="flex w-full cursor-pointer items-center gap-1.5 rounded-lg px-3 py-2.5 text-left text-sm font-medium text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
          onClick={() => setOpen((value) => !value)}
        >
          {open ? <ChevronUp aria-hidden className="size-3.5" /> : <ChevronDown aria-hidden className="size-3.5" />}
          Earlier history
        </button>
        {open && (
          <div id={contentID}>
            <HistoryPages
              workspace={workspace}
              taskId={item.task.id}
              selection="human"
              newest={newest}
              label="Earlier history pages"
              emptyText="No earlier activity recorded."
              // An expired start cursor restarts from the newest earlier page of
              // a freshly read detail view.
              resolveNewest={async () => {
                const key = taskDetailQueryKey(workspace, item.task.id)
                await queryClient.invalidateQueries({ queryKey: key, exact: true })
                return queryClient.getQueryData<ActivityItem>(key)?.event_page?.older_cursor
              }}
              renderRows={(events) => (
                <ol aria-label="Earlier history events" className="space-y-2 border-t border-border px-3 py-3">
                  {historyNoteEntries(item, events, members.data ?? []).map((entry) => (
                    <TimelineRow
                      key={entry.key}
                      entry={entry}
                      item={item}
                      usageReportedOrderIDs={usageReportedOrderIDs}
                    />
                  ))}
                </ol>
              )}
            />
          </div>
        )}
      </section>
    </li>
  )
}

// One bounded history pager shared by technical activity and Earlier history.
// Each step replaces the page, so at most one page of rows is mounted and only
// the current page's data is kept. The control that moved keeps its place in
// the viewport and its focus while the next page replaces the rows.
function HistoryPages({
  workspace,
  taskId,
  selection,
  newest,
  label,
  emptyText,
  renderRows,
  resolveNewest,
}: {
  workspace: string
  taskId: string
  selection: TaskHistorySelection
  newest: string
  label: string
  emptyText: string
  renderRows: (events: TaskEventView[]) => ReactNode
  resolveNewest?: () => Promise<string | undefined>
}) {
  const [cursor, setCursor] = useState(newest)
  const query = useTaskHistory(workspace, taskId, selection, cursor)
  const page = query.data
  const rowsRef = useRef<HTMLDivElement>(null)
  const moved = useRef<{ element: HTMLElement; top: number } | null>(null)
  const [reservedHeight, setReservedHeight] = useState<number>()
  const [restarting, setRestarting] = useState(false)
  const loading = query.isFetching || restarting

  const go = async (element: HTMLElement, next: string | undefined | (() => Promise<string | undefined>)) => {
    if (loading || next === undefined) return
    moved.current = { element, top: element.getBoundingClientRect().top }
    setReservedHeight(rowsRef.current?.offsetHeight)
    let target = next
    if (typeof target === 'function') {
      setRestarting(true)
      try {
        target = (await target()) ?? newest
      } finally {
        setRestarting(false)
      }
    }
    if (target === cursor) void query.refetch()
    else setCursor(target)
  }

  // Keep the moved control where it was and focused once the new page renders.
  useLayoutEffect(() => {
    const anchor = moved.current
    if (!anchor) return
    const delta = anchor.element.isConnected ? anchor.element.getBoundingClientRect().top - anchor.top : 0
    if (delta !== 0) {
      const container = scrollableAncestor(anchor.element)
      if (container) container.scrollTop += delta
      else window.scrollBy(0, delta)
    }
    if (anchor.element.isConnected && document.activeElement !== anchor.element) {
      anchor.element.focus({ preventScroll: true })
    }
    if (!loading) {
      moved.current = null
      setReservedHeight(undefined)
    }
  })

  const restart = isHistoryRestart(query.error)
  const newerCursor = cursor !== newest ? page?.newer_cursor : undefined
  const control = (name: string, next: string | undefined | (() => Promise<string | undefined>)) => {
    const unavailable = loading || next === undefined
    return (
      <button
        type="button"
        aria-disabled={unavailable}
        className={cn(
          'rounded-md border border-border px-2 py-1 font-medium text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary',
          unavailable ? 'cursor-not-allowed opacity-50' : 'hover:bg-raised',
        )}
        onClick={(event) => void go(event.currentTarget, next)}
      >
        {name}
      </button>
    )
  }
  return (
    <section aria-label={label} aria-busy={loading} className="border-t border-border">
      <div className="flex flex-wrap items-center gap-2 px-3 py-2">
        {control('Newest', cursor !== newest ? newest : undefined)}
        {control('Newer', newerCursor)}
        {control('Older', page?.older_cursor)}
      </div>
      <div ref={rowsRef} style={reservedHeight ? { minHeight: reservedHeight } : undefined}>
        {loading && (
          <p role="status" className="border-t border-border px-3 py-2">
            Loading activity…
          </p>
        )}
        {!loading && query.isError && restart && (
          <div role="alert" className="border-t border-border px-3 py-2">
            <p>Activity changed while paging</p>
            {control('Restart from newest', resolveNewest ?? newest)}
          </div>
        )}
        {!loading && query.isError && !restart && (
          <div role="alert" className="border-t border-border px-3 py-2">
            <p>Could not load activity: {query.error.message}</p>
            {control('Retry', cursor)}
          </div>
        )}
        {!loading && page && page.events.length === 0 && (
          <p className="border-t border-border px-3 py-2">{emptyText}</p>
        )}
        {!loading && page && page.events.length > 0 && (
          <>
            {renderRows(page.events)}
            {!page.older_cursor && <p className="border-t border-border px-3 py-2 text-faint">End of history.</p>}
          </>
        )}
      </div>
    </section>
  )
}

function AuditDisclosure({
  taskId,
  kind,
  recordId,
  label,
  event,
}: {
  taskId: string
  kind: TaskAuditKind
  recordId: string
  label: string
  event?: TaskEventView
}) {
  const [open, setOpen] = useState(false)
  return (
    <details className="mt-1 w-full" onToggle={(toggle) => setOpen(toggle.currentTarget.open)}>
      <summary className="cursor-pointer rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary">
        {label}
      </summary>
      {/* A loaded payload renders inline; an omitted snapshot or payload is
          read from the audit record by its decimal ID. */}
      {event && !event.audit_available && !event.payload_omitted ? (
        open && <AuditJSON payload={technicalPayload(event)} />
      ) : (
        <AuditPayload taskId={taskId} kind={kind} recordId={recordId} open={open} />
      )}
    </details>
  )
}

function AuditPayload({
  taskId,
  kind,
  recordId,
  open,
}: {
  taskId: string
  kind: TaskAuditKind
  recordId: string
  open: boolean
}) {
  const query = useTaskAudit(taskId, kind, recordId, open)
  if (!open) return null
  if (query.isError)
    return (
      <div role="alert" className="mt-2">
        <p>Could not load audit detail: {query.error.message}</p>
        <button type="button" className="text-primary underline" onClick={() => void query.refetch()}>
          Retry
        </button>
      </div>
    )
  if (query.isPending || query.isFetching)
    return (
      <p role="status" className="mt-2">
        Loading audit detail…
      </p>
    )
  const data = query.data
  return <AuditJSON payload={data.kind === 'event' ? technicalPayload(data.event) : data.work_order} />
}

function AuditJSON({ payload }: { payload: unknown }) {
  return (
    <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap rounded border border-border bg-background p-2 font-mono">
      {JSON.stringify(payload, null, 2)}
    </pre>
  )
}

function technicalPayload(event: Pick<TaskEvent | TaskEventView, 'kind' | 'payload'>): Record<string, unknown> {
  if (!event.kind.endsWith('.output_invalid') || !Object.hasOwn(event.payload ?? {}, 'output'))
    return event.payload ?? {}
  const { output: _rejectedOutput, ...metadata } = event.payload ?? {}
  return metadata
}

function scrollableAncestor(element: HTMLElement | null): HTMLElement | null {
  for (let candidate = element?.parentElement ?? null; candidate; candidate = candidate.parentElement) {
    if (/^(auto|scroll)$/.test(getComputedStyle(candidate).overflowY)) return candidate
  }
  return null
}

// Audit entries keep the wire action; the display label matches the gate UI
// ("redirect" surfaces as requesting changes).
const interventionLabels: Record<InterventionAction, string> = {
  approve: 'Approved',
  reject: 'Rejected',
  redirect: 'Requested changes',
  pull_to_local: 'Pulled to local',
  cancel: 'Cancelled',
}

function keyFor(entry: TimelineEntry) {
  if (entry.type === 'job') return `job-${entry.job.id}`
  if (entry.type === 'intervention') return `intervention-${entry.intervention.id}`
  return entry.key
}

// The panel's timeline dot, the same size as every other: the panel's verdict
// once it settles, otherwise the state of the round so far. The per-seat tally
// sits in the entry's header.
// The gates' own approval reason codes, read as what the person did.
const approvalReasonWords: Record<string, string> = {
  'plan-approved': 'Plan approved',
  'merge-at-gate': 'Approved and merged',
}

function panelDot(seats: PanelSeat[], resolution: { verdict: string } | undefined, deliberating: boolean) {
  if (resolution) return resolution.verdict === 'approve' ? 'bg-positive' : 'bg-attention-dot'
  if (deliberating) return 'animate-pulse bg-primary'
  if (seats.some((seat) => seat.review?.verdict === 'changes_requested')) return 'bg-attention-dot'
  if (seats.some((seat) => !seat.review && seat.status === 'failed')) return 'bg-failure'
  if (seats.some((seat) => !seat.review && (seat.status === 'stale' || seat.status === 'timed_out')))
    return 'bg-attention-dot'
  if (seats.length > 0 && seats.every((seat) => seat.review?.verdict === 'approve')) return 'bg-positive'
  return 'bg-edge'
}

const orderDots: Record<Extract<TimelineEntry, { type: 'order' }>['tone'], string> = {
  waiting: 'bg-edge',
  active: 'animate-pulse bg-primary',
  alarm: 'bg-attention-dot',
}

function TimelineRow({
  entry,
  item,
  usageReportedOrderIDs,
}: {
  entry: TimelineEntry
  item: ActivityItem
  usageReportedOrderIDs: Set<string>
}) {
  const resolve = usePersonResolver()
  // An entry with a person shows that person's avatar on the rail; an entry
  // with none keeps its dot.
  const rail = (people: RailPerson[], dot: string) =>
    people.length > 0 ? <RailPeople people={people} /> : <TimelineDot className={dot} />
  const jobPeople = (job: Job, order: WorkOrder | undefined): RailPerson[] => {
    const person = resolve(jobClaimantUserId(item, job, order))
    if (!person) return []
    const running = job.state === 'running'
    return [{ person, label: personLabel(person, stageWork(job.stage, running)), live: running }]
  }
  // The verify stage is a structured result, so like review it gets its own
  // entry: the sealed outcome, the assertions by exercise, and the pull
  // request publication, with the collection pages folded away.
  if (entry.type === 'job' && entry.job.stage === 'verify' && entry.job.started_at)
    return (
      <VerificationEntry
        item={item}
        job={entry.job}
        order={entry.order}
        dotSlot={(className) => rail(jobPeople(entry.job, entry.order), className)}
        footer={
          <JobFooter
            job={entry.job}
            model={entry.model}
            order={entry.order}
            usageAvailable={entry.order ? usageReportedOrderIDs.has(entry.order.id) : undefined}
          />
        }
      />
    )
  // An implement job is the task's delivery: its own card carries the
  // attempt, the agent's report, and the pull request and head it delivered.
  if (entry.type === 'job' && entry.job.stage === 'implement' && entry.job.started_at)
    return (
      <ImplementationEntry
        entry={entry}
        dotSlot={(className) => rail(jobPeople(entry.job, entry.order), className)}
        meta={
          <JobMeta
            job={entry.job}
            model={entry.model}
            order={entry.order}
            usageAvailable={entry.order ? usageReportedOrderIDs.has(entry.order.id) : undefined}
          />
        }
        summaryAudit={
          entry.summaryEventId && (
            // The server cut this text at 16,384 bytes; its event holds the rest.
            <AuditDisclosure
              taskId={item.task.id}
              kind="event"
              recordId={entry.summaryEventId}
              label="Full text (shortened here)"
            />
          )
        }
      />
    )
  if (entry.type === 'job')
    return (
      <JobEntry
        at={entry.at}
        job={entry.job}
        taskId={item.task.id}
        summary={entry.summary}
        summaryEventId={entry.summaryEventId}
        model={entry.model}
        tone={entry.tone}
        order={entry.order}
        usageAvailable={entry.order ? usageReportedOrderIDs.has(entry.order.id) : undefined}
        people={jobPeople(entry.job, entry.order)}
      />
    )
  if (entry.type === 'panel')
    return <PanelEntry entry={entry} item={item} usageReportedOrderIDs={usageReportedOrderIDs} />
  // A work-order entry carries no usage; per-order usage stays in TaskUsage
  // inside technical activity (component-web-task-surfaces, Work-order
  // timeline entries).
  if (entry.type === 'order') {
    const person = resolve(runClaimantUserId(entry.order.claimed_by))
    const claimed = entry.order.state === 'claimed'
    const people: RailPerson[] = person
      ? [
          {
            person,
            label: personLabel(person, stageWork(entry.order.stage, claimed)),
            live: entry.tone === 'active',
          },
        ]
      : []
    return (
      <li className="relative pl-7" data-order-entry={entry.order.state}>
        {rail(people, orderDots[entry.tone])}
        <div className="min-w-0 px-1 py-1.5">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span
              className={cn('text-sm font-medium', entry.tone === 'alarm' ? 'text-attention' : 'text-foreground/90')}
            >
              {entry.title}
            </span>
            <Badge variant={entry.badge.variant}>{entry.badge.label}</Badge>
            <time className="ml-auto text-[11px] text-faint">{absoluteTime(entry.at)}</time>
          </div>
          {entry.detail && <p className="mt-0.5 break-words text-xs text-muted">{entry.detail}</p>}
        </div>
      </li>
    )
  }
  if (entry.type === 'intervention') {
    const { intervention } = entry
    // The plan-revision gate is the one decision whose meaning lives in the
    // reason code rather than the action — both approve and decline are
    // `redirect` on the wire. Reading them back through the generic labels
    // would call an approval "Requested changes" and print the wire code
    // beside it, so they get their own plain-language line (AC-4.1).
    // A merge-gate return reads the same way: its reason code is the meaning,
    // and the entry is where the person's own feedback is kept verbatim, so it
    // repeats the promise the composer made when they sent it (REQ-6).
    const plainLabel =
      planRevisionDecisionLabels[intervention.reason_code] ??
      interventionReasonLabels[intervention.reason_code] ??
      approvalReasonWords[intervention.reason_code]
    const returnedForChanges = intervention.reason_code === userRequestChangesReason
    const person = resolve(interventionUserId(intervention))
    return (
      <li className="relative pl-7">
        {rail(person ? [{ person, label: personLabel(person, 'decided') }] : [], 'bg-foreground')}
        <div className="rounded-lg border border-edge bg-raised/40 px-4 py-3">
          <div className="flex flex-wrap items-center gap-2 text-sm">
            {!person && <UserRound className="size-3.5 text-muted" />}
            <strong className="font-semibold">
              {plainLabel ?? interventionLabels[intervention.action] ?? intervention.action.replaceAll('_', ' ')}
            </strong>
            {!plainLabel && intervention.reason_code !== defaultReasonCode[intervention.action] && (
              <Badge variant="mono">{intervention.reason_code}</Badge>
            )}
            <time className="ml-auto text-[11px] text-faint">{absoluteTime(intervention.at)}</time>
          </div>
          {intervention.comment && <p className="mt-2 text-sm leading-6 text-foreground/85">{intervention.comment}</p>}
          {returnedForChanges && <p className="mt-1.5 text-xs text-muted">{changesComposerHint}</p>}
        </div>
      </li>
    )
  }
  return (
    <li className="relative pl-7">
      <TimelineDot className={entry.alarm ? 'bg-attention-dot' : 'bg-edge'} />
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1 px-1 py-1.5">
        {entry.alarm && <AlertTriangle className="size-3.5 self-center text-attention" />}
        <span className={cn('text-sm', entry.alarm ? 'font-medium text-attention' : 'text-foreground/90')}>
          {entry.href ? (
            <a
              href={entry.href}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-primary hover:underline"
            >
              {entry.title}
              <ExternalLink className="size-3.5" />
            </a>
          ) : (
            entry.title
          )}
        </span>
        {entry.detail && !entry.href && <span className="text-xs text-muted">{entry.detail}</span>}
        <time className="ml-auto text-[11px] text-faint">{absoluteTime(entry.at)}</time>
        {entry.failureDetail && (
          <details className="basis-full text-xs text-muted">
            <summary className="cursor-pointer">Captured child error</summary>
            <pre className="mt-1 max-h-48 overflow-auto whitespace-pre-wrap rounded border border-border bg-surface p-2 font-mono">
              {entry.failureDetail}
            </pre>
          </details>
        )}
        {entry.fullProgress && (
          <details className="basis-full text-xs text-muted">
            <summary className="cursor-pointer">Show full progress report</summary>
            <pre className="mt-1 max-h-48 overflow-auto whitespace-pre-wrap rounded border border-border bg-surface p-2 font-sans text-sm text-foreground">
              {entry.fullProgress}
            </pre>
          </details>
        )}
        {entry.omittedEventId && (
          <div className="basis-full text-xs text-muted">
            <AuditDisclosure taskId={item.task.id} kind="event" recordId={entry.omittedEventId} label="Event payload" />
          </div>
        )}
      </div>
    </li>
  )
}

// The review panel as one deliberating body: seats as
// rows inside a single card, a verdict tally in the header, and — on a
// changes-requested settle — every dissenting seat's notes merged into one
// attributed round. Vocabulary is the spec's own: panel, seats, verdicts.
function PanelEntry({
  entry,
  item,
  usageReportedOrderIDs,
}: {
  entry: Extract<TimelineEntry, { type: 'panel' }>
  item: ActivityItem
  usageReportedOrderIDs: Set<string>
}) {
  const taskId = item.task.id
  const resolve = usePersonResolver()
  const { seats, resolution, seatMetadata } = entry
  const verdictsIn = seats.filter((seat) => seat.review).length
  const changes = seats.filter((seat) => seat.review?.verdict === 'changes_requested').length
  const deliberating = !resolution && seats.some((seat) => seat.status === 'deliberating')
  // Reviewer feedback surfaces as each verdict lands — not held until the
  // round settles — so nothing a reviewer wrote is ever invisible.
  const notes = seats.filter((seat) => seat.review && seat.review.feedback.trim())
  // Each seat's person as for a job; the rail draws each distinct person once,
  // live while one of their seats deliberates.
  const seatPeople = seats.map((seat) => resolve(jobClaimantUserId(item, seat.job, seat.order)))
  const people: RailPerson[] = []
  seatPeople.forEach((person, index) => {
    if (!person) return
    const live = !resolution && seats[index].status === 'deliberating'
    const known = people.find((entry) => entry.person.userId === person.userId)
    if (known) {
      if (live && !known.live) Object.assign(known, { live, label: personLabel(person, stageWork('review', true)) })
      return
    }
    people.push({ person, label: personLabel(person, stageWork('review', live)), live })
  })
  return (
    <li className="relative pl-7">
      {people.length > 0 ? (
        <RailPeople people={people} />
      ) : (
        <TimelineDot className={panelDot(seats, resolution, deliberating)} />
      )}
      <article className="rounded-lg bg-surface">
        <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-2.5">
          <span className="text-sm font-semibold text-foreground">{stageLabels.review}</span>
          {resolution && (
            <Badge variant={resolution.verdict === 'approve' ? 'positive' : 'attention'} data-review-verdict>
              {resolution.verdict === 'approve' ? 'Approved' : 'Changes requested'}
            </Badge>
          )}
          <span className="text-xs text-muted">
            <span title={seatMetadata ? reviewSeatSourceTitle(seatMetadata) : undefined}>
              {seatMetadata
                ? `Panel of ${seats.length} · minimum ${seatMetadata.minimum_seat_count}`
                : `Panel of ${seats.length}`}
            </span>
            {seatMetadata?.seat_count_source === 'risk' && ' · set by risk tier'}
            {seats.length > 1 && ' · unanimous to pass'}
            {entry.distinctModels && ' · task policy: distinct seat models'}
          </span>
          <span className="inline-flex items-center gap-[5px]" title={`${verdictsIn} of ${seats.length} verdicts in`}>
            {seats.map((seat) => (
              <span
                key={seat.seat}
                className={cn(
                  'size-2 rounded-full',
                  seat.review
                    ? seat.review.verdict === 'approve'
                      ? 'bg-positive'
                      : 'bg-attention-dot'
                    : 'border-[1.5px] border-edge',
                )}
              />
            ))}
          </span>
          <time className="ml-auto text-[11px] text-faint">
            {resolution ? `Settled ${absoluteTime(resolution.at)}` : `Convened ${absoluteTime(entry.at)}`}
          </time>
        </div>
        {entry.roundSummary && (
          <ReviewRoundSummary
            round={entry.round}
            summary={entry.roundSummary}
            audit={
              <AuditDisclosure
                taskId={taskId}
                kind="event"
                recordId={entry.roundSummary.event_id}
                label="Full summary text (shortened here)"
              />
            }
          />
        )}
        <div className="divide-y divide-border/60">
          {seats.map((seat, index) => (
            <SeatRow
              key={seat.seat}
              seat={seat}
              index={index}
              person={seatPeople[index]}
              usageAvailable={usageReportedOrderIDs.has(seat.order.id)}
            />
          ))}
        </div>
        {notes.length > 0 && (
          <div className="border-t border-border px-4 py-3">
            <div className="mb-1.5 flex flex-wrap items-baseline gap-x-3 gap-y-0.5">
              <span className="text-xs font-medium text-muted">
                {entry.round > 0 ? `Round ${entry.round} — ` : ''}the panel’s notes
              </span>
              {resolution?.verdict === 'changes_requested' && seats.length > 1 && (
                <span className="text-[11px] text-faint">counts as one round, however many seats dissented</span>
              )}
            </div>
            {notes.map((seat) => (
              <div
                key={seat.seat}
                className="grid grid-cols-[2.75rem_1fr] gap-x-2.5 border-t border-dashed border-border/70 py-2 text-sm first:border-t-0 first:pt-0 last:pb-0"
              >
                <span className="pt-0.5 text-xs text-faint">Seat {seat.seat}</span>
                <SeatNote seat={seat} taskId={taskId} />
              </div>
            ))}
          </div>
        )}
        {entry.round > 0 && (
          <ReviewSeatAdd taskId={taskId} round={entry.round} open={entry.seatAdditionOpen === true} />
        )}
        {resolution ? (
          <div
            className={cn(
              'flex items-start gap-2 rounded-b-lg border-t border-border px-4 py-2.5 text-sm font-medium',
              resolution.verdict === 'approve' ? 'bg-positive-soft text-positive' : 'bg-attention-soft text-attention',
            )}
          >
            {resolution.verdict === 'approve' ? (
              <Check className="mt-0.5 size-4 shrink-0" />
            ) : (
              <Undo2 className="mt-0.5 size-4 shrink-0" />
            )}
            {/* The icon stays beside the text however narrow the column gets. */}
            <span className="min-w-0 flex-1">
              {resolution.verdict === 'approve'
                ? seats.length > 1
                  ? `The panel is unanimous — ${seats.length} of ${seats.length} approved`
                  : 'Approved'
                : `Changes requested — ${changes} of ${seats.length} ${seats.length > 1 ? 'seats' : 'seat'}`}{' '}
              <span className="text-xs font-normal opacity-80">
                {resolution.verdict === 'changes_requested'
                  ? `feedback sent back as one round${resolution.bounce ? ` · ${resolution.bounce} used so far` : ''}`
                  : ''}
              </span>
            </span>
          </div>
        ) : (
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-border px-4 py-2 text-[11px] tabular-nums text-muted">
            <span>
              {verdictsIn} of {seats.length} verdicts in
            </span>
            {seats.length > 1 && (
              <>
                <span className="text-faint">·</span>
                <span>any “changes requested” sends the task back</span>
              </>
            )}
          </div>
        )}
      </article>
    </li>
  )
}

// The panel header's tooltip names where the round's seat count came from in
// words, never the source code (feature-review-seat-minimum RS-7). A count the
// risk tier set is also named in the header itself (req-risk-based-review
// AC-7.1).
function reviewSeatSourceTitle(metadata: TaskReviewSeatMetadata) {
  const source =
    metadata.seat_count_source === 'declared'
      ? 'Seat count declared at submission'
      : metadata.seat_count_source === 'planned'
        ? 'Seat count planned'
        : metadata.seat_count_source === 'carried'
          ? metadata.carried_from_round
            ? `Seat count carried from round ${metadata.carried_from_round}`
            : 'Seat count carried from the previous round'
          : metadata.seat_count_source === 'risk'
            ? "Seat count set by the task's risk tier"
            : 'Seat count is the workspace minimum'
  const planned = metadata.planned_seat_count
  return planned ? `${source} · Plan: ${planned} ${planned === 1 ? 'seat' : 'seats'}` : source
}

function SeatRow({
  seat,
  index,
  person,
  usageAvailable,
}: {
  seat: PanelSeat
  index: number
  person?: Person
  usageAvailable: boolean
}) {
  const progress = reviewSeatProgress(seat.order.progress)
  // Only the order's current model is a name the seat reports. A required model
  // is a historical pin, a worker-pinned order was launched on its model, and a
  // job keeps an earlier attempt's model after a release or an expiry clears
  // the order's (AC-2.12; DEC-104(4)).
  const reportedModel = seat.order.model_enforcement !== 'worker-pinned' && seat.order.model?.trim()
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 px-4 py-2.5">
      {/* The seat's person is a 16 px avatar after the label, with no name
          text; the label column widens by the avatar so the model keeps its
          place on the first line. */}
      <span
        className={cn(
          'inline-flex shrink-0 items-center gap-1 whitespace-nowrap text-xs text-faint',
          person ? 'w-[3.75rem]' : 'w-11',
        )}
      >
        Seat {seat.seat}
        {person && <PersonAvatar person={person} label={personLabel(person, `seat ${seat.seat}`)} size="inline" />}
      </span>
      <span
        className={cn(
          'flex min-w-0 flex-wrap items-baseline gap-x-1.5 text-[11px] tabular-nums text-muted sm:flex-[1_1_7rem]',
          person ? 'max-sm:basis-[calc(100%-4.5rem)]' : 'max-sm:basis-[calc(100%-3.5rem)]',
        )}
      >
        <ModelChip
          model={seat.model}
          agent={seat.order.agent}
          tokensIn={seat.job?.tokens_in || seat.order.tokens_in}
          tokensOut={seat.job?.tokens_out || seat.order.tokens_out}
          note={seat.order.required_effort ? `effort ${seat.order.required_effort}` : undefined}
          usageAvailable={usageAvailable}
          showUnavailableUsage={false}
        />
        {reportedModel && <span className="whitespace-nowrap font-sans text-faint">self-reported</span>}
      </span>
      {seat.review?.sameModelAsImplementer && (
        <span className="text-[11px] text-attention">same model as implementer</span>
      )}
      <SeatRiskTier tier={seat.review?.riskTier} reason={seat.review?.riskReason} />
      {usageAvailable && (
        <span className="text-[11px] tabular-nums text-faint">
          {compactTokens(seat.order.tokens_in)} in / {compactTokens(seat.order.tokens_out)} out
        </span>
      )}
      <span className="ml-auto">
        <SeatState seat={seat} index={index} />
      </span>
      {seat.status === 'deliberating' && progress && (
        <p className="w-full line-clamp-2 text-xs leading-5 text-muted">{progress}</p>
      )}
    </div>
  )
}

function reviewSeatProgress(progress?: string) {
  return progress
    ?.split('\n')
    .filter((line) => !/^\s*conveyor run mode\s*:/i.test(line))
    .join('\n')
    .trim()
}

// A seat's review feedback rendered as a clipped document block — the same
// family as the spec card's overflow treatment (see SpecCard): a fixed
// collapsed height with overflow hidden and the shared bottom fade, plus a
// small per-seat toggle shown only when the text actually overflows. State is
// session-local and independent per seat.
function SeatNote({ seat, taskId }: { seat: PanelSeat; taskId: string }) {
  const [expanded, setExpanded] = useState(false)
  const collapse = useCollapsibleContent(128, expanded)
  const viewportID = useId()

  return (
    <div>
      <div className="relative">
        <div id={viewportID} ref={collapse.viewportRef} style={collapse.viewportStyle}>
          <MarkdownProse className="markdown-demoted text-sm">{seat.review!.feedback.trim()}</MarkdownProse>
        </div>
        {collapse.clamped && (
          <div
            aria-hidden="true"
            className={cn('spec-overflow-shadow pointer-events-none absolute inset-x-0 bottom-0', collapse.fadeClass)}
          />
        )}
      </div>
      {collapse.collapsible && (
        <button
          type="button"
          aria-controls={viewportID}
          aria-expanded={expanded}
          aria-label={`${expanded ? 'Collapse' : 'Expand'} seat ${seat.seat} notes`}
          className="mt-1 inline-flex items-center gap-1 text-[11px] font-medium text-primary hover:text-primary focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary"
          onClick={() => setExpanded((value) => !value)}
        >
          {expanded ? (
            <ChevronUp aria-hidden="true" className="size-3" />
          ) : (
            <ChevronDown aria-hidden="true" className="size-3" />
          )}
          {expanded ? 'Less' : 'More'}
        </button>
      )}
      {seat.review?.truncatedEventId && (
        <div className="text-xs text-muted">
          <AuditDisclosure
            taskId={taskId}
            kind="event"
            recordId={seat.review.truncatedEventId}
            label="Full review text (shortened here)"
          />
        </div>
      )}
    </div>
  )
}

// Deliberating seats pulse out of phase with one another — independent
// minds, not one process with several labels.
function SeatState({ seat, index }: { seat: PanelSeat; index: number }) {
  const { review, status, job } = seat
  if (review) {
    const approved = review.verdict === 'approve'
    const took = job?.started_at ? duration(job.started_at, job.ended_at ?? review.at) : undefined
    return (
      <span className="flex items-center justify-end gap-2">
        <Disclosure
          title={review.summary.trim() || undefined}
          content={review.summary.trim() || undefined}
          triggerClassName="justify-center"
        >
          {/* The button keeps its touch target; only this pill is visible. */}
          <span
            className={cn(
              'inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium',
              approved ? 'bg-positive-soft text-positive' : 'bg-attention-soft text-attention',
            )}
          >
            {approved ? <Check className="size-3" /> : <Undo2 className="size-3" />}
            {approved ? 'Approved' : 'Changes'}
          </span>
        </Disclosure>
        {took && <span className="text-[11px] tabular-nums text-muted">{took}</span>}
      </span>
    )
  }
  if (status === 'deliberating') {
    return (
      <span className="flex items-center justify-end gap-2">
        <span className="inline-flex items-center gap-1.5 text-xs text-primary">
          <span
            className="size-1.5 animate-pulse rounded-full bg-primary"
            style={{ animationDelay: `${index * 0.5}s` }}
          />
          Deliberating
        </span>
        {job?.started_at && <span className="text-[11px] tabular-nums text-muted">{duration(job.started_at)}</span>}
      </span>
    )
  }
  if (status === 'waiting') {
    return (
      <span
        className="flex items-center justify-end gap-1.5 text-xs text-faint"
        title="Any agent connected over MCP can claim this seat. The server URL is in Settings."
      >
        <span className="size-1.5 rounded-full border-[1.5px] border-edge" />
        Waiting for an agent
      </span>
    )
  }
  const label =
    status === 'stale'
      ? 'Went stale in the queue'
      : status === 'timed_out'
        ? 'Timed out'
        : status === 'failed'
          ? 'Failed'
          : 'Cancelled'
  return (
    <span
      className={cn(
        'flex items-center justify-end text-xs',
        status === 'cancelled' ? 'text-faint' : status === 'failed' ? 'text-failure' : 'text-attention',
      )}
    >
      {label}
    </span>
  )
}

// A stage that produced no narration of its own ("Completed.", "Queued.") has
// nothing to read: it collapses to one line so the stages that did say
// something keep the reader's attention.
const placeholderSummaries = new Set([
  'Queued.',
  'Queued for an operator-owned agent over MCP.',
  'In progress.',
  'Completed.',
])

// The job footer keeps the operator-facing duration and model, while the model
// chip retains usage and dispatch detail on hover. The claim's harness shows
// only as the chip's logo fallback; auth mode, confinement, and actor plumbing
// stay in the API.
function JobEntry({
  at,
  job,
  taskId,
  summary,
  summaryEventId,
  model,
  tone,
  order,
  usageAvailable,
  people = [],
}: {
  // The instant the timeline placed this entry: its end once finished.
  at: string
  job: Job
  taskId: string
  summary: string
  summaryEventId?: string
  model: string
  tone: Extract<TimelineEntry, { type: 'job' }>['tone']
  order?: WorkOrder
  usageAvailable?: boolean
  people?: RailPerson[]
}) {
  if (!job.started_at) return null
  const running = job.state === 'running'
  const warning = tone === 'warning'
  const providerUsage = job.runner === 'in-process'
  const stage = stageLabels[job.stage] ?? job.stage
  const dotClass = cn(
    'bg-edge',
    job.state === 'done' && !warning && 'bg-positive',
    warning && 'bg-attention-dot',
    job.state === 'failed' && 'bg-failure',
    running && 'animate-pulse bg-primary',
  )
  // The claimant's avatar replaces the dot; the badges and the compact row's
  // "completed" text carry the status.
  const dot = people.length > 0 ? <RailPeople people={people} /> : <TimelineDot className={dotClass} />
  if (job.state === 'done' && !warning && placeholderSummaries.has(summary)) {
    // The plan section and the approval intervention tell a finished plan
    // stage, so its placeholder row would only repeat them.
    if (job.stage === 'spec') return null
    return (
      <li className="relative pl-7">
        {dot}
        <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1 px-1 py-1.5 text-sm">
          <span className="text-foreground/90">{stage} completed</span>
          <span className="text-[11px] tabular-nums text-faint">{duration(job.started_at, job.ended_at)}</span>
          <time className="ml-auto text-[11px] text-faint">{absoluteTime(at)}</time>
          {order && !providerUsage && (
            <span className="basis-full text-[11px] tabular-nums text-faint">
              {usageText(order, usageAvailable === true)}
            </span>
          )}
        </div>
      </li>
    )
  }
  return (
    <li className="relative pl-7">
      {dot}
      <article className={cn('rounded-lg bg-surface', warning && 'border border-attention/30 bg-attention-soft')}>
        {/* One header line: the stage, then model and duration, then the time.
            The entry has no separate footer (component-web-task-surfaces,
            Work-order timeline entries). */}
        <header className="flex flex-wrap items-center gap-x-2.5 gap-y-1 px-4 pt-3" data-job-header>
          <span className="text-sm font-semibold text-foreground">{stage}</span>
          {order?.review_seat ? <span className="text-xs text-muted">Seat {order.review_seat}</span> : null}
          {job.state === 'failed' && <Badge variant="failure">Failed</Badge>}
          {running && <Badge variant="accent">Running</Badge>}
          <JobMeta job={job} model={model} order={order} usageAvailable={usageAvailable} />
          <time className="ml-auto text-[11px] text-faint">{absoluteTime(at)}</time>
        </header>
        <MarkdownProse
          className="px-4 pt-1.5 pb-3 text-sm leading-6 text-foreground/85"
          components={{ p: ({ children }) => <p className="whitespace-pre-line">{children}</p> }}
        >
          {summary}
        </MarkdownProse>
        {summaryEventId && (
          // The server cut this text at 16,384 bytes; its event holds the rest.
          <div className="px-4 pb-3 text-xs text-muted">
            <AuditDisclosure
              taskId={taskId}
              kind="event"
              recordId={summaryEventId}
              label="Full text (shortened here)"
            />
          </div>
        )}
      </article>
    </li>
  )
}

// A verification entry keeps the duration and model in its own footer.
function JobFooter(props: { job: Job; model: string; order?: WorkOrder; usageAvailable?: boolean }) {
  if (!props.job.started_at) return null
  return (
    <footer className="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-border px-4 py-2 text-[11px] tabular-nums text-muted">
      <JobMeta {...props} />
    </footer>
  )
}

// The operator-facing model and duration, while the model chip retains usage
// and dispatch detail on hover. The claim's harness shows only as the chip's
// logo fallback; auth mode, confinement, and actor plumbing stay in the API.
function JobMeta({
  job,
  model,
  order,
  usageAvailable,
}: {
  job: Job
  model: string
  order?: WorkOrder
  usageAvailable?: boolean
}) {
  if (!job.started_at) return null
  const providerUsage = job.runner === 'in-process'
  const note = [
    order?.required_effort ? `effort ${order.required_effort}` : undefined,
    // A historical order may still carry `worker-pinned` (DEC-97(1)).
    order?.model_enforcement === 'worker-pinned'
      ? 'model pinned at launch'
      : order?.model_enforcement === 'self-reported'
        ? 'model self-reported by the agent'
        : undefined,
  ]
    .filter(Boolean)
    .join(' · ')
  return (
    <span className="inline-flex min-w-0 items-center gap-x-2.5 text-[11px] tabular-nums text-muted">
      <span className="min-w-0">
        <ModelChip
          model={model}
          agent={order?.agent}
          tokensIn={job.tokens_in}
          tokensOut={job.tokens_out}
          note={note || undefined}
          usageAvailable={
            providerUsage
              ? job.tokens_in + job.tokens_out > 0
              : order
                ? usageAvailable
                : job.tokens_in + job.tokens_out > 0
          }
          usageProvenance={providerUsage ? 'provider-reported' : order ? usageProvenance(order) : 'provider-reported'}
        />
      </span>
      <span className="shrink-0 text-faint" data-job-duration>
        {duration(job.started_at, job.ended_at)}
      </span>
    </span>
  )
}

// Bundled logo SVGs (no network fetch). Monochrome icons inherit the
// foreground color; selection order lives in lib/model-logo.
const logoAssets: Record<ProviderLogo | HarnessLogo, { svg: string; className?: string }> = {
  openai: { svg: openaiIcon, className: 'text-foreground' },
  claude: { svg: claudeIcon },
  gemini: { svg: geminiIcon },
  grok: { svg: grokIcon, className: 'text-foreground' },
  zhipu: { svg: zhipuIcon },
  cursor: { svg: cursorIcon, className: 'text-foreground' },
  'claude-code': { svg: claudeCodeIcon },
  codex: { svg: codexIcon },
  opencode: { svg: opencodeIcon, className: 'text-foreground' },
  windsurf: { svg: windsurfIcon, className: 'text-foreground' },
  'github-copilot': { svg: githubCopilotIcon, className: 'text-foreground' },
}

function ModelChip({
  model,
  agent,
  tokensIn,
  tokensOut,
  note,
  usageAvailable,
  usageProvenance,
  showUnavailableUsage = true,
}: {
  model: string
  agent?: string
  tokensIn: number
  tokensOut: number
  note?: string
  usageAvailable?: boolean
  usageProvenance?: string
  showUnavailableUsage?: boolean
}) {
  const selected = selectModelLogo(model, agent)
  const logo = selected ? logoAssets[selected.logo] : undefined
  const usage = [
    usageAvailable === false
      ? showUnavailableUsage
        ? 'Usage unavailable'
        : undefined
      : usageAvailable || tokensIn + tokensOut > 0
        ? `${compactTokens(tokensIn)} in / ${compactTokens(tokensOut)} out`
        : undefined,
    usageAvailable ? usageProvenance : undefined,
    note,
    selected?.source === 'harness' ? `via ${selected.harness}` : undefined,
  ]
    .filter(Boolean)
    .join(' · ')
  return (
    <span className="group/model relative inline-flex max-w-full cursor-default items-center gap-1.5">
      {logo ? (
        <span
          aria-hidden
          className={cn('inline-flex shrink-0 [&_svg]:size-3.5', logo.className)}
          dangerouslySetInnerHTML={{ __html: logo.svg }}
        />
      ) : (
        <Cpu aria-hidden className="size-3.5 shrink-0 text-faint" />
      )}
      <Disclosure triggerClassName="truncate" title={model} content={model}>
        {model}
      </Disclosure>
      {usage && (
        <span
          role="tooltip"
          className="pointer-events-none absolute bottom-full left-0 z-10 mb-1.5 whitespace-nowrap rounded-md bg-foreground px-2 py-1 font-mono text-[11px] leading-4 text-background opacity-0 shadow-md transition-opacity duration-150 after:absolute after:left-3 after:top-full after:border-4 after:border-transparent after:border-t-foreground group-hover/model:opacity-100"
        >
          {usage}
        </span>
      )}
    </span>
  )
}

function TimelineDot({ className }: { className?: string }) {
  return <span className={cn('absolute left-0 top-3 size-[15px] rounded-full border-4 border-background', className)} />
}
