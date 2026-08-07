import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import {
  AlertTriangle,
  Check,
  ChevronDown,
  ChevronUp,
  CircleDashed,
  Cpu,
  ExternalLink,
  Pin,
  Undo2,
  UserRound,
} from 'lucide-react'
import claudeIcon from '@lobehub/icons-static-svg/icons/claude-color.svg?raw'
import geminiIcon from '@lobehub/icons-static-svg/icons/gemini-color.svg?raw'
import grokIcon from '@lobehub/icons-static-svg/icons/grok.svg?raw'
import openaiIcon from '@lobehub/icons-static-svg/icons/openai.svg?raw'
import {
  buildTimeline,
  dependencyRelationLabel,
  deriveCurrentExecutionState,
  technicalActivity,
  type CurrentExecutionState,
  type PanelSeat,
  type TimelineEntry,
} from '../../lib/activity'
import { defaultReasonCode, stageLabels } from '../../lib/contracts'
import { relatedTaskRoute, type TaskRouteVariant } from '../../lib/task-route'
import type { ActivityItem, InterventionAction, Job, WorkOrder } from '../../lib/types'
import { absoluteTime, cn, compactTokens, duration } from '../../lib/utils'
import { Badge } from '../ui/badge'
import { MarkdownProse } from '../ui/markdown-prose'
import { ReviewPanel, gateTone, isReviewable, type GateTone } from './review-panel'
import { RedispatchCard, canRedispatch } from './redispatch-card'
import { WorkOrderRecoveryCard, hasWorkerRecovery } from './work-order-recovery-card'
import { ReviewRoundRetryCard, hasReviewRoundRetry } from './review-round-retry-card'
import { InterruptedReviewRecoveryCard, hasInterruptedReviewRecovery } from './interrupted-review-recovery-card'
import { WorkerStatusCard, hasWorkerAlert } from './worker-status-card'
import { WorkOrderPreemptCard, claimedWorkOrder } from './work-order-preempt-card'

// The gate dot pulses — the timeline's one "waiting on you" signal — in the
// gate card's own tone.
const gateDots: Record<GateTone, string> = {
  positive: 'bg-positive',
  neutral: 'bg-primary',
  alarm: 'bg-attention-dot',
}

// The execution event timeline (spec §13.3 elements 2 and 3): the audit log
// rendered as a story — one entry per stage execution, with usage, duration,
// and the harness/model/auth mode that ran it, interleaved with pipeline
// incidents and every recorded decision. Anything actionable "now" — status
// alerts, recovery actions, and the human gate itself — renders as the live
// tail: cause sits directly above prompt, and a recorded decision resolves
// in place into its intervention entry.
// `executionActions` is false for a blueprint anchor (spec §21.49): it takes
// no work orders, so redispatch, worker serviceability, and review recovery
// are affordances for execution that will never happen. The story it does
// have — materialization, child progress, close — still renders in full.
export function Timeline({
  item,
  executionActions = true,
  routeVariant = 'full',
}: {
  item: ActivityItem
  executionActions?: boolean
  routeVariant?: TaskRouteVariant
}) {
  const entries = buildTimeline(item)
  const currentExecution = deriveCurrentExecutionState(item)
  const technicalEvents = technicalActivity(item)
  const usageReportedOrderIDs = reportedUsageOrderIDs(item)
  const showGate = isReviewable(item.task)
  const timelineRef = useRef<HTMLElement>(null)
  const gateRef = useRef<HTMLLIElement>(null)
  const [decisionScrollRequest, setDecisionScrollRequest] = useState(0)
  const priorExecutionStatus = useRef(currentExecution?.status)
  const [executionAnnouncement, setExecutionAnnouncement] = useState('')

  useEffect(() => {
    if (priorExecutionStatus.current === 'progressing' && currentExecution?.status === 'paused') {
      setExecutionAnnouncement(`${currentExecution.title}. ${currentExecution.nextAction}`)
    }
    priorExecutionStatus.current = currentExecution?.status
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
  // the top of a story the reviewer has often already read.
  useEffect(() => {
    if (showGate) gateRef.current?.scrollIntoView({ block: 'end' })
  }, [item.task.id, showGate])

  const tail = (
    executionActions
      ? [
          hasWorkerAlert(item) && {
            key: 'worker-alert',
            dot: 'bg-attention-dot',
            card: <WorkerStatusCard item={item} />,
          },
          claimedWorkOrder(item) && {
            key: 'work-order-preempt',
            dot: 'bg-attention-dot',
            card: <WorkOrderPreemptCard item={item} />,
          },
          hasInterruptedReviewRecovery(item) && {
            key: 'interrupted-review',
            dot: 'bg-attention-dot',
            card: <InterruptedReviewRecoveryCard item={item} />,
          },
          hasReviewRoundRetry(item) && {
            key: 'review-retry',
            dot: 'bg-attention-dot',
            card: <ReviewRoundRetryCard item={item} />,
          },
          hasWorkerRecovery(item) && {
            key: 'order-recovery',
            dot: 'bg-attention-dot',
            card: <WorkOrderRecoveryCard item={item} />,
          },
          canRedispatch(item) && { key: 'redispatch', dot: 'bg-edge', card: <RedispatchCard item={item} /> },
        ]
      : []
  ).filter((entry) => entry !== false)

  return (
    <>
      <section ref={timelineRef} aria-label="Execution event timeline">
        <p className="sr-only" role="status" aria-live="polite" aria-atomic="true">
          {executionAnnouncement}
        </p>
        {currentExecution && <CurrentExecutionSummary state={currentExecution} routeVariant={routeVariant} />}
        <h2 className="mb-4 mt-5 text-sm font-semibold tracking-tight">Activity</h2>
        <ol className="relative space-y-4 before:absolute before:bottom-4 before:left-[7px] before:top-4 before:w-px before:bg-border">
          {entries.map((entry) => (
            <TimelineRow key={keyFor(entry)} entry={entry} usageReportedOrderIDs={usageReportedOrderIDs} />
          ))}
          {entries.length === 0 && tail.length === 0 && !showGate && (
            <li className="pl-7 text-sm text-muted">Waiting for the first job to start.</li>
          )}
          {tail.map(({ key, dot, card }) => (
            <li key={key} className="relative pl-7">
              <TimelineDot className={dot} />
              {card}
            </li>
          ))}
          {showGate && (
            <li ref={gateRef} className="relative pl-7">
              <TimelineDot
                className={cn('animate-pulse', gateDots[gateTone(item.task, item.events, item.merge_readiness)])}
              />
              <ReviewPanel item={item} onDecisionRecorded={() => setDecisionScrollRequest((request) => request + 1)} />
            </li>
          )}
        </ol>
      </section>
      {(technicalEvents.length > 0 || (item.work_orders?.length ?? 0) > 0) && <TechnicalActivity item={item} />}
    </>
  )
}

function CurrentExecutionSummary({
  state,
  routeVariant,
}: {
  state: CurrentExecutionState
  routeVariant: TaskRouteVariant
}) {
  const paused = state.status === 'paused'
  const relatedRoute = relatedTaskRoute(routeVariant)
  const blockerDecision = state.blocker.match(/\bDEC-[1-9][0-9]*\b/)?.[0]
  return (
    <section
      aria-labelledby="current-execution-title"
      className={cn(
        'mb-4 rounded-lg border px-4 py-3',
        paused ? 'border-attention/55 bg-attention-soft' : 'border-primary/25 bg-primary-soft/35',
      )}
    >
      <div className="flex items-start gap-2">
        {paused ? (
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-attention" aria-hidden />
        ) : (
          <CircleDashed className="mt-0.5 size-4 shrink-0 animate-pulse text-primary" aria-hidden />
        )}
        <div className="min-w-0">
          <p className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted">
            Current state · {paused ? 'Paused — needs attention' : 'Progressing'}
          </p>
          <h2
            id="current-execution-title"
            className={cn('mt-0.5 text-sm font-semibold', paused ? 'text-attention' : 'text-foreground')}
          >
            {state.title}
          </h2>
        </div>
      </div>
      <dl className="mt-3 grid gap-2 text-xs leading-5 sm:grid-cols-3">
        <div>
          <dt className="font-medium text-foreground">Current blocker</dt>
          <dd className="text-muted">
            {state.blockingDependencies?.length ? (
              <span className="flex flex-col items-start">
                {state.blockingDependencies.map((dependency) => (
                  <Link
                    key={dependency.id}
                    to={relatedRoute}
                    params={{ taskId: dependency.id }}
                    className="text-primary hover:underline"
                  >
                    {dependency.title || dependency.id} ·{' '}
                    {dependencyRelationLabel(
                      dependency.state,
                      true,
                      state.unsatisfiableDependencyIDs?.includes(dependency.id) === true,
                    )}
                  </Link>
                ))}
              </span>
            ) : (
              <DecisionBlockerText blocker={state.blocker} decisionID={blockerDecision} />
            )}
          </dd>
        </div>
        <div>
          <dt className="font-medium text-foreground">Retry</dt>
          <dd className="text-muted">{state.retry}</dd>
        </div>
        <div>
          <dt className="font-medium text-foreground">What to do next</dt>
          <dd className="text-muted">{state.nextAction}</dd>
        </div>
      </dl>
    </section>
  )
}

function DecisionBlockerText({ blocker, decisionID }: { blocker: string; decisionID?: string }) {
  if (!decisionID) return blocker
  const [before, ...after] = blocker.split(decisionID)
  return (
    <>
      {before}
      <Link
        to="/system-design"
        hash={`decision-${decisionID.toLowerCase()}`}
        className="text-primary hover:underline"
        aria-label={`Open ${decisionID} in System Design`}
      >
        {decisionID}
      </Link>
      {after.join(decisionID)}
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

function TechnicalActivity({ item }: { item: ActivityItem }) {
  const events = technicalActivity(item)
  const reported = reportedUsageOrderIDs(item)
  const orders = item.work_orders ?? []
  const measured = orders.filter((order) => reported.has(order.id))
  const totals = measured.reduce(
    (sum, order) => ({
      tokensIn: sum.tokensIn + order.tokens_in,
      tokensOut: sum.tokensOut + order.tokens_out,
    }),
    { tokensIn: 0, tokensOut: 0 },
  )
  return (
    <details className="mt-5 rounded-lg border border-border bg-surface/35 text-xs text-muted">
      <summary className="cursor-pointer rounded-lg px-3 py-2.5 font-medium text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary">
        Show technical activity
      </summary>
      {orders.length > 0 && (
        <section aria-label="Task usage telemetry" className="border-t border-border px-3 py-3">
          <p className="font-medium text-foreground">Task usage</p>
          <p className="mt-1 font-mono text-[11px] tabular-nums">
            {measured.length > 0
              ? `${compactTokens(totals.tokensIn)} in / ${compactTokens(totals.tokensOut)} out across ${measured.length} reported work ${measured.length === 1 ? 'order' : 'orders'}`
              : 'Usage unavailable for every work order'}
            {orders.length > measured.length
              ? ` · ${orders.length - measured.length} ${orders.length - measured.length === 1 ? 'order' : 'orders'} unavailable`
              : ''}
          </p>
          <ul className="mt-2 space-y-1" aria-label="Work-order usage">
            {orders.map((order) => (
              <li key={order.id} className="flex flex-wrap items-baseline gap-x-2 font-mono text-[11px] tabular-nums">
                <span className="text-foreground">{stageLabels[order.stage] ?? order.stage}</span>
                <code className="text-faint">{order.id}</code>
                <span className="ml-auto">{usageText(order, reported.has(order.id))}</span>
              </li>
            ))}
          </ul>
        </section>
      )}
      <ol className="divide-y divide-border border-t border-border">
        {events.map((event) => (
          <li key={event.id} className="px-3 py-2">
            <div className="flex flex-wrap items-baseline gap-2">
              <code className="text-[11px] text-foreground">{event.kind}</code>
              <time className="ml-auto text-[11px] text-faint">{absoluteTime(event.at)}</time>
            </div>
            {event.payload && Object.keys(event.payload).length > 0 && <EventPayloadDisclosure event={event} />}
          </li>
        ))}
      </ol>
    </details>
  )
}

function EventPayloadDisclosure({ event }: { event: ActivityItem['events'][number] }) {
  const [open, setOpen] = useState(false)
  return (
    <details className="mt-1" onToggle={(toggle) => setOpen(toggle.currentTarget.open)}>
      <summary className="cursor-pointer rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary">
        Event payload
      </summary>
      {open && (
        <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap rounded border border-border bg-background p-2 font-mono">
          {JSON.stringify(technicalPayload(event), null, 2)}
        </pre>
      )}
    </details>
  )
}

function technicalPayload(event: ActivityItem['events'][number]): Record<string, unknown> {
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

// The panel's timeline dot: a segmented ring, one arc per seat, filling as
// verdicts arrive — the collapsed, feed-glanceable form of the header tally.
function ringGradient(seats: PanelSeat[]): string | undefined {
  if (seats.length < 2) return undefined
  const gap = 16
  const segment = (360 - seats.length * gap) / seats.length
  const stops: string[] = []
  let angle = 0
  for (const seat of seats) {
    const color = seat.review
      ? seat.review.verdict === 'approve'
        ? 'var(--color-positive)'
        : 'var(--color-attention-dot)'
      : seat.status === 'stale' || seat.status === 'timed_out'
        ? 'var(--color-attention-dot)'
        : seat.status === 'failed'
          ? 'var(--color-failure)'
          : 'var(--color-edge)'
    stops.push(
      `${color} ${angle}deg ${angle + segment}deg`,
      `transparent ${angle + segment}deg ${angle + segment + gap}deg`,
    )
    angle += segment + gap
  }
  return `conic-gradient(from -90deg, ${stops.join(', ')})`
}

const orderDots: Record<Extract<TimelineEntry, { type: 'order' }>['tone'], string> = {
  waiting: 'bg-edge',
  active: 'animate-pulse bg-primary',
  alarm: 'bg-attention-dot',
}

function TimelineRow({ entry, usageReportedOrderIDs }: { entry: TimelineEntry; usageReportedOrderIDs: Set<string> }) {
  if (entry.type === 'job')
    return (
      <JobEntry
        job={entry.job}
        summary={entry.summary}
        model={entry.model}
        tone={entry.tone}
        order={entry.order}
        usageAvailable={entry.order ? usageReportedOrderIDs.has(entry.order.id) : undefined}
      />
    )
  if (entry.type === 'panel') return <PanelEntry entry={entry} usageReportedOrderIDs={usageReportedOrderIDs} />
  if (entry.type === 'order') {
    return (
      <li className="relative pl-7">
        <TimelineDot className={orderDots[entry.tone]} />
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1 px-1 py-1.5">
          <span className={cn('text-sm font-medium', entry.tone === 'alarm' ? 'text-attention' : 'text-foreground/90')}>
            {entry.title}
          </span>
          {entry.detail && <span className="text-xs text-muted">{entry.detail}</span>}
          <span className="font-mono text-[11px] tabular-nums text-faint">
            {usageText(entry.order, usageReportedOrderIDs.has(entry.order.id))}
          </span>
          <time className="ml-auto text-[11px] text-faint">{absoluteTime(entry.at)}</time>
        </div>
      </li>
    )
  }
  if (entry.type === 'intervention') {
    const { intervention } = entry
    return (
      <li className="relative pl-7">
        <TimelineDot className="bg-foreground" />
        <div className="rounded-lg border border-edge bg-raised/40 px-4 py-3">
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <UserRound className="size-3.5 text-muted" />
            <strong className="font-semibold">
              {interventionLabels[intervention.action] ?? intervention.action.replaceAll('_', ' ')}
            </strong>
            {intervention.reason_code !== defaultReasonCode[intervention.action] && (
              <Badge variant="mono">{intervention.reason_code}</Badge>
            )}
            <time className="ml-auto text-[11px] text-faint">{absoluteTime(intervention.at)}</time>
          </div>
          {intervention.comment && <p className="mt-2 text-sm leading-6 text-foreground/85">{intervention.comment}</p>}
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
      </div>
    </li>
  )
}

// The review panel as one deliberating body (spec §21.12 change 4): seats as
// rows inside a single card, a verdict tally in the header, and — on a
// changes-requested settle — every dissenting seat's notes merged into one
// attributed round. Vocabulary is the spec's own: panel, seats, verdicts.
function PanelEntry({
  entry,
  usageReportedOrderIDs,
}: {
  entry: Extract<TimelineEntry, { type: 'panel' }>
  usageReportedOrderIDs: Set<string>
}) {
  const { seats, resolution } = entry
  const verdictsIn = seats.filter((seat) => seat.review).length
  const changes = seats.filter((seat) => seat.review?.verdict === 'changes_requested').length
  const deliberating = !resolution && seats.some((seat) => seat.status === 'deliberating')
  // Reviewer feedback surfaces as each verdict lands — not held until the
  // round settles — so nothing a reviewer wrote is ever invisible.
  const notes = seats.filter((seat) => seat.review && seat.review.feedback.trim())
  const gradient = ringGradient(seats)
  const single = seats[0]
  return (
    <li className="relative pl-7">
      {gradient ? (
        <span
          aria-hidden
          className={cn('absolute -left-0.5 top-2.5 size-[19px] rounded-full', deliberating && 'animate-pulse')}
          style={{ background: gradient }}
        >
          <span className="absolute inset-1 rounded-full bg-background" />
        </span>
      ) : (
        <TimelineDot
          className={cn(
            'bg-edge',
            single?.review?.verdict === 'approve' && 'bg-positive',
            single?.review?.verdict === 'changes_requested' && 'bg-attention-dot',
            !single?.review && (single?.status === 'stale' || single?.status === 'timed_out') && 'bg-attention-dot',
            !single?.review && single?.status === 'failed' && 'bg-failure',
            deliberating && 'animate-pulse bg-primary',
          )}
        />
      )}
      <article className="rounded-lg border border-border bg-card">
        <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-2.5">
          <span className="text-xs font-semibold uppercase tracking-[0.1em] text-foreground">{stageLabels.review}</span>
          <span className="text-xs text-muted">
            {seats.length > 1 ? `Panel of ${seats.length} · unanimous to pass` : 'Panel of 1'}
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
        <div className="divide-y divide-border/60">
          {seats.map((seat, index) => (
            <SeatRow
              key={seat.seat}
              seat={seat}
              index={index}
              usageAvailable={usageReportedOrderIDs.has(seat.order.id)}
            />
          ))}
        </div>
        {notes.length > 0 && (
          <div className="border-t border-border px-4 py-3">
            <div className="mb-1.5 flex flex-wrap items-baseline gap-x-3 gap-y-0.5">
              <span className="font-mono text-[10px] uppercase tracking-[0.1em] text-faint">
                {entry.round > 0 ? `Round ${entry.round} — ` : ''}the panel’s notes
              </span>
              {resolution?.verdict === 'changes_requested' && seats.length > 1 && (
                <span className="text-[11px] text-faint">counts as one round, however many seats dissented</span>
              )}
            </div>
            {notes.map((seat) => (
              <div
                key={seat.seat}
                className="grid grid-cols-[auto_1fr] gap-x-2.5 border-t border-dashed border-border/70 py-2 text-sm first:border-t-0 first:pt-0 last:pb-0"
              >
                <span className="pt-1 font-mono text-[10px] uppercase tracking-[0.08em] text-faint">
                  Seat {seat.seat}
                </span>
                <SeatNote seat={seat} />
              </div>
            ))}
          </div>
        )}
        {resolution ? (
          <div
            className={cn(
              'flex flex-wrap items-center gap-x-2 gap-y-1 rounded-b-lg border-t border-border px-4 py-2.5 text-sm font-medium',
              resolution.verdict === 'approve' ? 'bg-positive-soft text-positive' : 'bg-attention-soft text-attention',
            )}
          >
            {resolution.verdict === 'approve' ? (
              <Check className="size-4 shrink-0" />
            ) : (
              <Undo2 className="size-4 shrink-0" />
            )}
            {resolution.verdict === 'approve'
              ? seats.length > 1
                ? `The panel is unanimous — ${seats.length} of ${seats.length} approved`
                : 'Approved'
              : `Changes requested — ${changes} of ${seats.length} ${seats.length > 1 ? 'seats' : 'seat'}`}
            <span className="text-xs font-normal opacity-80">
              {resolution.verdict === 'changes_requested'
                ? `feedback sent back as one round${resolution.bounce ? ` · ${resolution.bounce} used so far` : ''}`
                : ''}
            </span>
          </div>
        ) : (
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-border px-4 py-2 font-mono text-[11px] tabular-nums text-muted">
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

function SeatRow({ seat, index, usageAvailable }: { seat: PanelSeat; index: number; usageAvailable: boolean }) {
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 px-4 py-2.5">
      <span className="w-11 shrink-0 font-mono text-[10px] uppercase tracking-[0.08em] text-faint">
        Seat {seat.seat}
      </span>
      <span className="min-w-0 flex-[1_1_7rem] font-mono text-[11px] tabular-nums text-muted">
        <ModelChip
          model={seat.model}
          tokensIn={seat.job?.tokens_in || seat.order.tokens_in}
          tokensOut={seat.job?.tokens_out || seat.order.tokens_out}
          note={seat.order.required_effort ? `effort ${seat.order.required_effort}` : undefined}
          usageAvailable={usageAvailable}
          usageProvenance={usageProvenance(seat.order)}
        />
      </span>
      <span className="font-mono text-[11px] tabular-nums text-faint">{usageText(seat.order, usageAvailable)}</span>
      <EnforcementChip enforcement={seat.order.model_enforcement} />
      <span className="ml-auto">
        <SeatState seat={seat} index={index} />
      </span>
      {seat.status === 'deliberating' && seat.order.progress && (
        <p className="w-full line-clamp-2 text-xs leading-5 text-muted">{seat.order.progress}</p>
      )}
    </div>
  )
}

// A seat's review feedback rendered as a clipped document block — the same
// family as the spec card's overflow treatment (see SpecCard): a fixed
// collapsed height with overflow hidden and the shared bottom fade, plus a
// small per-seat toggle shown only when the text actually overflows. State is
// session-local and independent per seat (spec task 260720-29ffcc; AC-1..AC-3).
function SeatNote({ seat }: { seat: PanelSeat }) {
  const [expanded, setExpanded] = useState(false)
  const [hasOverflow, setHasOverflow] = useState(false)
  const viewportRef = useRef<HTMLDivElement>(null)
  const viewportID = useId()

  useLayoutEffect(() => {
    // Only measure while collapsed; expanding keeps the last overflow reading so
    // the collapse control stays visible (mirrors SpecCard's contentExpanded).
    const viewport = viewportRef.current
    if (!viewport || expanded) return
    const measure = () => setHasOverflow(viewport.scrollHeight > viewport.clientHeight + 1)
    measure()

    const observer = new ResizeObserver(measure)
    observer.observe(viewport)
    if (viewport.firstElementChild) observer.observe(viewport.firstElementChild)
    return () => observer.disconnect()
  }, [expanded, seat.review])

  return (
    <div>
      <div className="relative">
        <div id={viewportID} ref={viewportRef} className={cn(!expanded && 'max-h-32 overflow-hidden')}>
          <p className="whitespace-pre-line leading-6 text-foreground/85">{seat.review!.feedback.trim()}</p>
        </div>
        {hasOverflow && !expanded && (
          <div
            aria-hidden="true"
            className="spec-overflow-shadow pointer-events-none absolute inset-x-0 bottom-0 h-12"
          />
        )}
      </div>
      {hasOverflow && (
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
    </div>
  )
}

// Enforcement rendered honestly (spec §21.12 change 4): a pin for the model
// the worker invoked itself, a dashed circle for what a claiming agent
// merely reported — with the plain-words difference behind a hover.
function EnforcementChip({ enforcement }: { enforcement?: WorkOrder['model_enforcement'] }) {
  if (!enforcement) return null
  const pinned = enforcement === 'worker-pinned'
  return (
    <span className="group/enforce relative inline-flex cursor-default items-center gap-1 whitespace-nowrap text-[11px] text-faint">
      {pinned ? (
        <Pin aria-hidden className="size-3 shrink-0" />
      ) : (
        <CircleDashed aria-hidden className="size-3 shrink-0" />
      )}
      <span>{pinned ? 'pinned' : 'self-reported'}</span>
      <span
        role="tooltip"
        className="pointer-events-none absolute bottom-full left-0 z-10 mb-1.5 w-60 rounded-md bg-foreground px-2.5 py-1.5 text-[11px] leading-4 text-background opacity-0 shadow-md transition-opacity duration-150 after:absolute after:left-3 after:top-full after:border-4 after:border-transparent after:border-t-foreground group-hover/enforce:opacity-100"
      >
        {pinned
          ? 'Executed by your worker, invoked with this exact model — enforcement Conveyor can vouch for.'
          : 'Claimed by an operator-attached agent. The model is what the session reported — Conveyor can’t enforce it.'}
      </span>
    </span>
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
        <span
          title={review.summary.trim() || undefined}
          className={cn(
            'inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium',
            approved ? 'bg-positive-soft text-positive' : 'bg-attention-soft text-attention',
          )}
        >
          {approved ? <Check className="size-3" /> : <Undo2 className="size-3" />}
          {approved ? 'Approved' : 'Changes'}
        </span>
        {took && <span className="font-mono text-[11px] tabular-nums text-muted">{took}</span>}
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
        {job?.started_at && (
          <span className="font-mono text-[11px] tabular-nums text-muted">{duration(job.started_at)}</span>
        )}
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

// The job footer keeps the operator-facing facts — duration, model, and
// explicit work-order usage — while the model chip retains dispatch detail on
// hover. Harness, auth mode, confinement, and actor plumbing stay in the API.
function JobEntry({
  job,
  summary,
  model,
  tone,
  order,
  usageAvailable,
}: {
  job: Job
  summary: string
  model: string
  tone: Extract<TimelineEntry, { type: 'job' }>['tone']
  order?: WorkOrder
  usageAvailable?: boolean
}) {
  if (!job.started_at) return null
  const running = job.state === 'running'
  const warning = tone === 'warning'
  const providerUsage = job.runner === 'in-process'
  const stage = stageLabels[job.stage] ?? job.stage
  const note = [
    order?.required_effort ? `effort ${order.required_effort}` : undefined,
    order?.model_enforcement === 'worker-pinned'
      ? 'model pinned by your worker'
      : order?.model_enforcement === 'self-reported'
        ? 'model self-reported by the agent'
        : undefined,
  ]
    .filter(Boolean)
    .join(' · ')
  const dot = (
    <TimelineDot
      className={cn(
        'bg-edge',
        job.state === 'done' && !warning && 'bg-positive',
        warning && 'bg-attention-dot',
        job.state === 'failed' && 'bg-failure',
        running && 'animate-pulse bg-primary',
      )}
    />
  )
  if (job.state === 'done' && !warning && placeholderSummaries.has(summary)) {
    return (
      <li className="relative pl-7">
        {dot}
        <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1 px-1 py-1.5 text-sm">
          <span className="text-foreground/90">{stage} completed</span>
          <span className="font-mono text-[11px] tabular-nums text-faint">
            {duration(job.started_at, job.ended_at)}
          </span>
          <time className="ml-auto text-[11px] text-faint">{absoluteTime(job.started_at)}</time>
          {order && !providerUsage && (
            <span className="basis-full font-mono text-[11px] tabular-nums text-faint">
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
      <article
        className={cn('rounded-lg border border-border bg-card', warning && 'border-attention/40 bg-attention-soft')}
      >
        <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-2.5">
          <span className="text-xs font-semibold uppercase tracking-[0.1em] text-foreground">{stage}</span>
          {order?.review_seat ? <span className="text-xs text-muted">Seat {order.review_seat}</span> : null}
          {job.state === 'failed' && <Badge variant="failure">Failed</Badge>}
          {running && <Badge variant="accent">Running</Badge>}
          <time className="ml-auto text-[11px] text-faint">{absoluteTime(job.started_at)}</time>
        </div>
        <MarkdownProse
          className="px-4 py-3 text-sm leading-6 text-foreground/85"
          components={{ p: ({ children }) => <p className="whitespace-pre-line">{children}</p> }}
        >
          {summary}
        </MarkdownProse>
        <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 border-t border-border px-4 py-2 font-mono text-[11px] tabular-nums text-muted">
          <span>{duration(job.started_at, job.ended_at)}</span>
          <ModelChip
            model={model}
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
          {order && !providerUsage && <span>{usageText(order, usageAvailable === true)}</span>}
        </div>
      </article>
    </li>
  )
}

// Provider logo keyed off the model name (bundled SVGs, no network fetch).
function providerLogo(model: string): { svg: string; className?: string } | undefined {
  const name = model.toLowerCase()
  if (/^(gpt|o\d|codex|davinci)/.test(name) || name.includes('openai'))
    return { svg: openaiIcon, className: 'text-foreground' }
  if (/claude|fable|opus|sonnet|haiku|anthropic/.test(name)) return { svg: claudeIcon }
  if (/gemini|google/.test(name)) return { svg: geminiIcon }
  if (/grok|xai|x\.ai/.test(name)) return { svg: grokIcon, className: 'text-foreground' }
  return undefined
}

function ModelChip({
  model,
  tokensIn,
  tokensOut,
  note,
  usageAvailable,
  usageProvenance,
}: {
  model: string
  tokensIn: number
  tokensOut: number
  note?: string
  usageAvailable?: boolean
  usageProvenance?: string
}) {
  const logo = providerLogo(model)
  const usage = [
    usageAvailable === false
      ? 'Usage unavailable'
      : usageAvailable || tokensIn + tokensOut > 0
        ? `${compactTokens(tokensIn)} in / ${compactTokens(tokensOut)} out`
        : undefined,
    usageAvailable ? usageProvenance : undefined,
    note,
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
      <span className="truncate" title={model}>
        {model}
      </span>
      {usage && (
        <span
          role="tooltip"
          className="pointer-events-none absolute bottom-full right-0 z-10 mb-1.5 whitespace-nowrap rounded-md bg-foreground px-2 py-1 font-mono text-[11px] leading-4 text-background opacity-0 shadow-md transition-opacity duration-150 after:absolute after:right-3 after:top-full after:border-4 after:border-transparent after:border-t-foreground group-hover/model:opacity-100"
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
