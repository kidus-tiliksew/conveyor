import { type GroupKey, taskStateLabels } from './contracts'
import type {
  ActivityItem,
  ActivitySummary,
  Intervention,
  Job,
  TaskAssignee,
  TaskEvent,
  TaskRelation,
  WorkspaceMembership,
  WorkOrder,
} from './types'

// The name a person is known by, falling through the identity fields the task
// projection carries. The raw user ID is the last resort: it identifies an
// account, it does not name a colleague, so it never displaces a real name
// (REQ-4, AC-4.3).
export function assigneeName(assignee: TaskAssignee): string {
  return assignee.display_name || assignee.email || assignee.user_id
}

// The store refuses a claim on an assigned task by naming the assignee's raw
// account ID ("task T is assigned to usr_x; only that assignee may claim its
// work orders", conveyor:internal/store/store.go). That sentence is written for
// an agent's error channel, not for a person reading a task, so every surface
// that would otherwise print it says who holds the task instead (AC-4.1,
// AC-4.3). The hydrated assignee supplies the name; without one the account ID
// is still better than the transport sentence around it.
const claimRefusalPattern = /task \S+ is assigned to (\S+?);\s*only that assignee may claim its work orders/i

function assigneeForUser(
  userID: string,
  assignee?: TaskAssignee,
  members: WorkspaceMembership[] = [],
): TaskAssignee | undefined {
  if (assignee?.user_id === userID) return assignee
  return members.find((member) => member.user_id === userID)
}

export function humanizeClaimRefusal(
  text: string | undefined,
  assignee?: TaskAssignee,
  members: WorkspaceMembership[] = [],
): string | undefined {
  if (!text) return text
  const match = claimRefusalPattern.exec(text)
  if (!match) return text
  const resolved = assigneeForUser(match[1], assignee, members)
  const name = resolved ? assigneeName(resolved) : match[1]
  return `Assigned to ${name} — only they can pick this up.`
}

// Feed grouping: the pipeline stage a task currently occupies.
// Human gates, approved tasks awaiting merge, parked tasks, and pending
// authority signals collect under "Awaiting human" without changing pipeline
// state (REQ-2 AC-2.2; REQ-3; design-web-dashboard); only terminal states
// archive under "Completed".
export function groupForSummary(item: ActivitySummary): GroupKey {
  const { state } = item.task
  if (item.stalled?.needed && state !== 'merged' && state !== 'closed') return 'human'
  if (item.forge_failure && state !== 'merged' && state !== 'closed') return 'human'
  if (item.pending_authority && state !== 'merged' && state !== 'closed') return 'human'
  if (state === 'awaiting_human' || state === 'approved' || state === 'parked') return 'human'
  if (state === 'merged' || state === 'closed') return 'done'
  const stage =
    item.task.state === 'queued' || item.task.state === 'running'
      ? (item.task.next_stage ?? item.latest_stage)
      : (item.latest_stage ?? item.task.next_stage)
  switch (stage) {
    case 'spec':
      return 'spec'
    case 'review':
      return 'review'
    case 'verify':
      return 'verify'
    case 'implement':
    case 'merge':
      return 'implement'
    case 'triage':
    default:
      return 'triage'
  }
}

// Provenance chip: "github:<owner>/<repo>#<n>" links out; every
// other source (cli, api, cron, monitor) renders as-is.
export function parseProvenance(source: string): { label: string; href?: string } {
  const match = /^github:([\w.-]+\/[\w.-]+)#(\d+)$/.exec(source)
  if (match) {
    return { label: `${match[1]}#${match[2]}`, href: `https://github.com/${match[1]}/issues/${match[2]}` }
  }
  return { label: source }
}

export function pullRequestURL(events: TaskEvent[]): string | undefined {
  for (let i = events.length - 1; i >= 0; i--) {
    const event = events[i]
    if (event.kind === 'pull_request.opened' && typeof event.payload?.url === 'string') {
      return event.payload.url
    }
  }
  return undefined
}

// What the merge gate is actually approving, read back off the durable record
// the pipeline already wrote: the reviewed head the branch was judged at, the
// pull request it was pushed to, the commit range that head covers, and the
// factory's own verdict. Every field is optional because every field is
// evidence — a repository delivered without GitHub has no pull request, and a
// task that reached the gate through a path that recorded no round verdict
// shows none rather than an invented one (REQ-6).
export interface MergeGateReview {
  headSHA?: string
  baseSHA?: string
  pullRequest?: { url: string; number?: number; repository?: string }
  verdict?: { verdict: 'approve' | 'changes_requested'; summary?: string; seats: number }
}

export function mergeGateReview(item: ActivityItem): MergeGateReview {
  const review: MergeGateReview = { headSHA: item.task.reviewed_head_sha || undefined }
  for (let i = item.events.length - 1; i >= 0; i--) {
    const event = item.events[i]
    const payload = event.payload ?? {}
    if (event.kind !== 'pull_request.opened') continue
    review.headSHA = review.headSHA ?? (typeof payload.head_sha === 'string' ? payload.head_sha : undefined)
    review.baseSHA = typeof payload.base_sha === 'string' ? payload.base_sha : undefined
    if (typeof payload.url === 'string' && payload.url) {
      review.pullRequest = {
        url: payload.url,
        number: typeof payload.number === 'number' ? payload.number : undefined,
        repository: typeof payload.repository === 'string' ? payload.repository : undefined,
      }
    }
    break
  }
  // The last completed round is the verdict the gate opened on; the seats that
  // reported into it say how many independent reviewers stand behind it.
  for (let i = item.events.length - 1; i >= 0; i--) {
    const event = item.events[i]
    if (event.kind !== 'review.round_completed') continue
    const payload = event.payload ?? {}
    review.verdict = {
      verdict: payload.verdict === 'changes_requested' ? 'changes_requested' : 'approve',
      summary: typeof payload.summary === 'string' && payload.summary.trim() ? payload.summary.trim() : undefined,
      seats: (Array.isArray(payload.reviews) ? payload.reviews : []).length,
    }
    break
  }
  return review
}

// The reason code and bounce source the merge-gate request-changes command
// records (conveyor:internal/store/changes_requested.go). The dashboard reads
// them; it never writes them, and neither string is ever shown to a person.
export const userRequestChangesReason = 'user-request-changes'

export interface ReturnedForChanges {
  feedback: string
  at: string
}

// The server's user-request-changes attention marker, re-derived from the same
// durable events it reads (store.UserRequestChangesPending): a bounce a person
// asked for stays outstanding until an implementation run claims the work it
// created. The marker itself reaches the dashboard only folded into
// needs_attention, so this predicate is what tells that signal apart from the
// other reasons a task wants a human — and it carries the feedback with it.
export function pendingUserRequestChanges(events: TaskEvent[]): ReturnedForChanges | null {
  let pending: ReturnedForChanges | null = null
  for (const event of events) {
    const payload = event.payload ?? {}
    if (event.kind === 'pipeline.bounced' && payload.source === userRequestChangesReason) {
      pending = { feedback: typeof payload.feedback === 'string' ? payload.feedback.trim() : '', at: event.at }
    }
    if (event.kind === 'work_order.claimed' && payload.stage === 'implement') pending = null
  }
  return pending
}

// Match store.UserRequestChangesHold: the latest implement claim records
// whether the bounced work originated in an explicit `conveyor run`. Hold is
// deliberately not consulted because operators may hold worker-pipeline work.
export function userRunImplementation(events: TaskEvent[]): boolean {
  for (let index = events.length - 1; index >= 0; index--) {
    const event = events[index]
    const payload = event.payload ?? {}
    if (event.kind !== 'work_order.claimed' || payload.stage !== 'implement') continue
    return typeof payload.claimant_id === 'string' && /^run:.+/.test(payload.claimant_id)
  }
  return false
}

// Board-card gate chip: says what the gate is waiting for instead of a
// generic alarm, with tone to match ("Ready to merge" is good news).
export function gateBadge(item: ActivitySummary): { label: string; variant: 'attention' | 'positive' } | undefined {
  if (item.stalled?.needed) return { label: 'Stalled', variant: 'attention' }
  if (item.task.state === 'approved') return { label: 'Ready to merge', variant: 'positive' }
  if (!item.needs_attention) return undefined
  if (item.pending_authority) return { label: 'Awaiting proposal decision', variant: 'attention' }
  if (item.task.state === 'parked') return { label: 'Needs a route', variant: 'attention' }
  if (item.task.state === 'awaiting_human') return { label: 'Awaiting review', variant: 'attention' }
  return { label: 'Needs attention', variant: 'attention' }
}

export function reviewDiagnosticBadge(
  item: ActivitySummary,
): { label: string; variant: 'attention' | 'accent' } | undefined {
  const diagnostics = item.review_diagnostics ?? []
  if (diagnostics.some((diagnostic) => diagnostic.status === 'expired_without_verdict')) {
    return { label: 'Verdict claim expired', variant: 'attention' }
  }
  if (diagnostics.some((diagnostic) => diagnostic.status === 'claimed_without_verdict')) {
    return { label: 'Awaiting verdict submission', variant: 'accent' }
  }
  return undefined
}

function forgeFailureDetail(payload: Record<string, unknown>, detailKey: 'last_error' | 'error'): string | undefined {
  const category = typeof payload.forge_error_category === 'string' ? payload.forge_error_category.trim() : ''
  const detail = typeof payload[detailKey] === 'string' ? payload[detailKey].trim() : ''
  return [category, detail].filter(Boolean).join(' · ') || undefined
}

const attemptEventKinds = new Set([
  'work_order.claimed',
  'work_order.lease_renewed',
  'work_order.child_failed',
  'work_order.stalled',
  'work_order.released',
  'work_order.expired',
  'work_order.timed_out',
  'work_order.recovered',
  'work_order.redispatched',
])

export type CurrentExecutionKind =
  | 'running'
  | 'dependency_waiting'
  | 'dependency_attention'
  | 'retry_pending'
  | 'provider_usage_limit'
  | 'checkout_blocked'
  | 'released'
  | 'expired'

export interface CurrentExecutionState {
  kind: CurrentExecutionKind
  status: 'progressing' | 'paused'
  order: WorkOrder
  attemptId?: string
  title: string
  blocker: string
  retry: string
  nextAction: string
  action: 'none' | 'retry_implementation' | 'recover' | 'resolve_checkout'
  blockingDependencies?: TaskRelation[]
  unsatisfiableDependencyIDs?: string[]
}

function orderActivityTime(order: WorkOrder): number {
  return new Date(
    order.updated_at ?? order.last_failure_at ?? order.execution_started_at ?? order.queue_entered_at,
  ).getTime()
}

export function isDirtyPrimaryCheckout(order: WorkOrder) {
  if (order.stage === 'review') return false
  return [order.last_failure_message, order.last_failure_detail].some((value) =>
    value?.includes('checkout_blocked_dirty_primary'),
  )
}

// Dependency gating suspends the implementation order's queue clock and makes
// it unclaimable. Keep this predicate shared by the
// current-state summary, timeline narration, and queued affordances so they
// cannot disagree about whether implementation is actually available.
export function dependencyBlockedImplementationOrder(item: ActivityItem): WorkOrder | undefined {
  const blockingIDs = item.task.blocking_task_ids ?? []
  if (blockingIDs.length === 0 || item.stalled?.unsatisfiable_edge) return undefined

  const orders = item.work_orders ?? []
  return [...orders]
    .filter((order) => order.stage === 'implement' && order.state === 'queued' && !order.claimable)
    .filter((order) => (order.unsatisfiable_task_ids?.length ?? 0) === 0)
    .sort((a, b) => orderActivityTime(b) - orderActivityTime(a))[0]
}

export function unsatisfiableDependencyOrder(item: ActivityItem): WorkOrder | undefined {
  const blockingIDs = item.task.blocking_task_ids ?? []
  if (blockingIDs.length === 0) return undefined
  const unsatisfiable = item.stalled?.unsatisfiable_edge === true
  return [...(item.work_orders ?? [])]
    .filter((order) => order.stage === 'implement' && order.state === 'queued' && !order.claimable)
    .filter((order) => unsatisfiable || (order.unsatisfiable_task_ids?.length ?? 0) > 0)
    .sort((a, b) => orderActivityTime(b) - orderActivityTime(a))[0]
}

export function dependencyRelationLabel(state: string, blocking: boolean, unsatisfiable: boolean) {
  if (unsatisfiable) return 'Needs attention'
  if (blocking) return 'Waiting'
  if (state === 'merged') return 'Satisfied'
  return taskStateLabels[state as keyof typeof taskStateLabels] ?? state.replaceAll('_', ' ')
}

function blockingDependencies(item: ActivityItem) {
  const blockingIDs = new Set(item.task.blocking_task_ids ?? [])
  return (item.task.dependencies ?? []).filter((dependency) => blockingIDs.has(dependency.id))
}

function withDependencyContext(state: CurrentExecutionState, item: ActivityItem): CurrentExecutionState {
  const dependencies = blockingDependencies(item)
  if (dependencies.length === 0) return state
  const subject =
    dependencies.length === 1 ? dependencies[0].title || dependencies[0].id : `${dependencies.length} dependencies`
  return {
    ...state,
    blocker: `${state.blocker} ${subject} ${dependencies.length === 1 ? 'is' : 'are'} also unresolved.`,
    nextAction: `${state.nextAction} The task remains dependency-gated until ${dependencies.length === 1 ? 'that dependency is' : 'those dependencies are'} resolved.`,
    blockingDependencies: dependencies,
  }
}

export function deriveCurrentExecutionState(item: ActivityItem): CurrentExecutionState | undefined {
  const dependencyBlockedOrder = dependencyBlockedImplementationOrder(item)
  const candidates = [...(item.work_orders ?? [])]
    .filter((candidate) => !(candidate.stage === 'review' && (candidate.review_round ?? 0) > 0))
    .filter((candidate) => candidate.state === 'claimed' || ['queued', 'stale', 'timed_out'].includes(candidate.state))
    .sort((a, b) => orderActivityTime(b) - orderActivityTime(a))
  const order =
    candidates.find((candidate) => candidate.state === 'claimed') ??
    candidates.find(
      (candidate) =>
        candidate.state !== 'queued' ||
        Boolean(candidate.last_attempt_outcome || candidate.retry_suppressed || candidate.next_retry_at),
    )

  if (!order) {
    const unsatisfiableOrder = unsatisfiableDependencyOrder(item)
    if (unsatisfiableOrder) {
      const unsatisfiableDependencyIDs =
        (unsatisfiableOrder.unsatisfiable_task_ids?.length ?? 0) > 0
          ? unsatisfiableOrder.unsatisfiable_task_ids
          : item.stalled?.blocking_task_ids
      return {
        kind: 'dependency_attention',
        status: 'paused',
        order: unsatisfiableOrder,
        title: 'Dependency needs attention',
        blocker: 'A required dependency closed without merging.',
        retry: 'Redispatch is unavailable while the dependency is unsatisfiable.',
        nextAction: 'Unlink the dead dependency with an audit reason, or cancel this task.',
        action: 'none',
        blockingDependencies: blockingDependencies(item),
        unsatisfiableDependencyIDs,
      }
    }
    if (dependencyBlockedOrder) {
      const dependencies = blockingDependencies(item)
      const dependencySubject =
        dependencies.length > 1
          ? `${dependencies.length} dependencies`
          : dependencies[0]?.title || dependencies[0]?.id || 'the blocking dependency'
      return {
        kind: 'dependency_waiting',
        status: 'progressing',
        order: dependencyBlockedOrder,
        title: 'Waiting on dependencies',
        blocker: 'Implementation is gated by unresolved task dependencies.',
        retry: 'Not applicable.',
        nextAction: `Nothing — implementation starts automatically when ${dependencySubject} ${dependencies.length > 1 ? 'merge' : 'merges'}.`,
        action: 'none',
        blockingDependencies: dependencies,
      }
    }
    return undefined
  }

  const stage = order.stage === 'spec' ? 'Plan' : order.stage === 'review' ? 'Review' : 'Implementation'
  if (order.state === 'claimed') {
    return {
      kind: 'running',
      status: 'progressing',
      order,
      attemptId: order.attempt_id,
      title: `${stage} is in progress`,
      blocker: 'No current blocker.',
      retry: 'No retry is needed.',
      nextAction: 'No operator action is needed.',
      action: 'none',
    }
  }
  if (order.next_retry_at && !order.retry_suppressed) {
    const provider = order.last_failure_category === 'provider_usage_limit'
    return {
      kind: 'retry_pending',
      status: 'progressing',
      order,
      attemptId: order.last_attempt_id,
      title: 'Automatic retry scheduled',
      blocker: provider
        ? 'The provider usage limit paused the last attempt.'
        : 'The last attempt ended before completion.',
      retry: 'Conveyor will retry automatically.',
      nextAction: 'No operator action is needed.',
      action: 'none',
    }
  }
  if (isDirtyPrimaryCheckout(order)) {
    return withDependencyContext(
      {
        kind: 'checkout_blocked',
        status: 'paused',
        order,
        attemptId: order.last_attempt_id,
        title: `${stage} paused — checkout needs attention`,
        blocker: 'The primary checkout has pre-existing changes, so Conveyor left them untouched.',
        retry: 'Conveyor will not retry automatically while this safety gate is unresolved.',
        nextAction: 'Resolve the primary checkout changes, then retry the implementation.',
        action: 'resolve_checkout',
      },
      item,
    )
  }
  if (order.last_failure_category === 'provider_usage_limit') {
    return withDependencyContext(
      {
        kind: 'provider_usage_limit',
        status: 'paused',
        order,
        attemptId: order.last_attempt_id,
        title: `${stage} paused — provider limit reached`,
        blocker: 'The provider usage or capacity limit stopped the last attempt.',
        retry: 'No automatic retry is pending.',
        nextAction: 'Retry the implementation after the provider limit has cleared.',
        action: 'retry_implementation',
      },
      item,
    )
  }
  if (order.state === 'stale' || order.state === 'timed_out' || order.last_attempt_outcome === 'expired') {
    return withDependencyContext(
      {
        kind: 'expired',
        status: 'paused',
        order,
        attemptId: order.last_attempt_id,
        title: `${stage} paused — recovery needed`,
        blocker:
          order.state === 'stale'
            ? 'The order was not claimed before its queue deadline.'
            : 'The claim or execution window ended before completion.',
        retry: 'No automatic retry is pending.',
        nextAction: 'Recover the work order to try again.',
        action: 'recover',
      },
      item,
    )
  }
  return withDependencyContext(
    {
      kind: 'released',
      status: 'paused',
      order,
      attemptId: order.last_attempt_id,
      title: `${stage} paused — recovery needed`,
      blocker:
        humanizeClaimRefusal(order.last_failure_message, item.task.assignee) ||
        'The latest attempt released the work before completion.',
      retry: 'No automatic retry is pending.',
      nextAction: 'Recover the work order to try again.',
      action: 'recover',
    },
    item,
  )
}

export function technicalActivity(item: ActivityItem): TaskEvent[] {
  return [...item.events].sort((a, b) => new Date(a.at).getTime() - new Date(b.at).getTime())
}

// The release reason the plan-revision command records, mirrored here so the
// timeline can tell that one released claim apart from an ordinary one.
const planRevisionReleaseReason = 'plan revision requested'

export interface PlanRevisionRequest {
  planVersion: number
  rationale: string
}

// The request event is the durable record of what is being contested: the
// agent's rationale and the plan version it names. A payload missing either is
// not a decidable request, so nothing is invented from it (REQ-1 AC-1.3).
export function planRevisionRequestFrom(event: TaskEvent): PlanRevisionRequest | null {
  const rationale = typeof event.payload?.rationale === 'string' ? event.payload.rationale.trim() : ''
  const planVersion =
    typeof event.payload?.plan_version === 'number'
      ? event.payload.plan_version
      : Number.parseInt(String(event.payload?.plan_version ?? ''), 10)
  if (!rationale || !Number.isInteger(planVersion) || planVersion <= 0) return null
  return { planVersion, rationale }
}

// A plan-revision request remains the active human decision until an audited
// intervention supersedes it. Later release/state events are lifecycle detail,
// so they must not hide the request the operator is being asked to judge
// (REQ-2 AC-2.1).
export function pendingPlanRevisionRequest(events: TaskEvent[]): PlanRevisionRequest | null {
  for (let i = events.length - 1; i >= 0; i--) {
    const event = events[i]
    if (event.kind.startsWith('intervention.')) return null
    if (event.kind !== 'work_order.plan_revision_requested') continue
    return planRevisionRequestFrom(event)
  }
  return null
}

export type TimelineEntry =
  | {
      type: 'job'
      at: string
      job: Job
      summary: string
      model: string
      tone: 'default' | 'warning'
      order?: WorkOrder
    }
  | {
      type: 'note'
      at: string
      key: string
      title: string
      detail?: string
      failureDetail?: string
      fullProgress?: string
      href?: string
      alarm?: boolean
    }
  | {
      type: 'order'
      at: string
      key: string
      title: string
      detail?: string
      tone: 'waiting' | 'active' | 'alarm'
      order: WorkOrder
    }
  | { type: 'intervention'; at: string; intervention: Intervention }
  | { type: 'panel'; at: string; key: string; round: number; seats: PanelSeat[]; resolution?: PanelResolution }

// One review round is rendered as a single deliberating body: seats appear
// inside one card instead of N sibling job cards. A seat is
// a review work order plus whatever exists of its job and verdict.
export type PanelSeatStatus = 'waiting' | 'deliberating' | 'verdict' | 'stale' | 'timed_out' | 'failed' | 'cancelled'

export interface PanelSeatReview {
  verdict: 'approve' | 'changes_requested'
  summary: string
  feedback: string
  at: string
}

export interface PanelSeat {
  seat: number
  order: WorkOrder
  job?: Job
  model: string
  status: PanelSeatStatus
  review?: PanelSeatReview
}

export interface PanelResolution {
  verdict: 'approve' | 'changes_requested'
  summary?: string
  at: string
  bounce?: number
}

// Group review work orders carrying seat assignments into panel entries.
// Rounds ≥ 1 aggregate as one panel; round 0 predates rounds and aggregates
// per order (matching store.aggregateReviewRound), so each forms a
// single-seat panel. Everything a panel absorbs — its jobs, its orders, its
// per-seat and aggregate review events, the synthetic round intervention —
// is suppressed from the flat timeline to keep the story told once.
interface PanelIndex {
  panels: Extract<TimelineEntry, { type: 'panel' }>[]
  jobIds: Set<string>
  orderIds: Set<string>
  rounds: Set<number>
}

function isPanelReviewEvent(payload: Record<string, unknown>, index: PanelIndex): boolean {
  const round = typeof payload.review_round === 'number' ? payload.review_round : undefined
  if (round !== undefined && round > 0) return index.rounds.has(round)
  const reviews = Array.isArray(payload.reviews) ? payload.reviews : []
  return reviews.some(
    (review) =>
      typeof (review as Record<string, unknown>)?.review_work_order_id === 'string' &&
      index.orderIds.has((review as Record<string, unknown>).review_work_order_id as string),
  )
}

function buildReviewPanels(item: ActivityItem): PanelIndex {
  const index: PanelIndex = { panels: [], jobIds: new Set(), orderIds: new Set(), rounds: new Set() }
  const seatOrders = (item.work_orders ?? []).filter(
    (order) => order.stage === 'review' && (order.review_seat ?? 0) > 0,
  )
  if (seatOrders.length === 0) return index

  const groups = new Map<string, { round: number; orders: WorkOrder[] }>()
  for (const order of seatOrders) {
    const round = order.review_round ?? 0
    const key = round > 0 ? `panel-${round}` : `panel-0-${order.id}`
    const group = groups.get(key) ?? { round, orders: [] }
    group.orders.push(order)
    groups.set(key, group)
  }

  const jobsById = new Map(item.jobs.map((job) => [job.id, job]))
  const reviewByOrder = new Map<string, PanelSeatReview>()
  const reviewByJob = new Map<string, PanelSeatReview>()
  const resolutionByRound = new Map<number, PanelResolution>()
  const resolutionByOrder = new Map<string, PanelResolution>()
  const bounceByRound = new Map<number, number>()
  for (const event of item.events) {
    const payload = event.payload ?? {}
    if (event.kind === 'review.completed') {
      const review: PanelSeatReview = {
        verdict: payload.verdict === 'changes_requested' ? 'changes_requested' : 'approve',
        summary: typeof payload.summary === 'string' ? payload.summary : '',
        feedback: typeof payload.feedback === 'string' ? payload.feedback : '',
        at: event.at,
      }
      if (typeof payload.review_work_order_id === 'string') reviewByOrder.set(payload.review_work_order_id, review)
      // Older events carry no order id; the event's job is the review job.
      if (event.job_id) reviewByJob.set(event.job_id, review)
    }
    if (event.kind === 'review.round_completed') {
      const resolution: PanelResolution = {
        verdict: payload.verdict === 'changes_requested' ? 'changes_requested' : 'approve',
        summary: typeof payload.summary === 'string' ? payload.summary : undefined,
        at: event.at,
      }
      const round = typeof payload.review_round === 'number' ? payload.review_round : 0
      if (round > 0) resolutionByRound.set(round, resolution)
      for (const review of Array.isArray(payload.reviews) ? payload.reviews : []) {
        const orderID = (review as Record<string, unknown>)?.review_work_order_id
        if (typeof orderID === 'string') resolutionByOrder.set(orderID, resolution)
      }
    }
    if (
      event.kind === 'pipeline.bounced' &&
      typeof payload.review_round === 'number' &&
      typeof payload.count === 'number'
    ) {
      bounceByRound.set(payload.review_round, payload.count)
    }
  }

  for (const [key, group] of groups) {
    const seats: PanelSeat[] = group.orders
      .slice()
      .sort((a, b) => (a.review_seat ?? 0) - (b.review_seat ?? 0))
      .map((order) => {
        const job = jobsById.get(order.job_id)
        const review = reviewByOrder.get(order.id) ?? reviewByJob.get(order.job_id)
        index.jobIds.add(order.job_id)
        index.orderIds.add(order.id)
        return {
          seat: order.review_seat ?? 0,
          order,
          job,
          model: order.model || order.required_model || job?.model_tier || '—',
          status: seatStatus(order, job, review),
          review,
        }
      })
    if (group.round > 0) index.rounds.add(group.round)
    const resolution = group.round > 0 ? resolutionByRound.get(group.round) : resolutionByOrder.get(group.orders[0].id)
    if (resolution && resolution.verdict === 'changes_requested') {
      resolution.bounce = bounceByRound.get(group.round)
    }
    index.panels.push({
      type: 'panel',
      at: group.orders.reduce(
        (min, order) => (order.queue_entered_at < min ? order.queue_entered_at : min),
        group.orders[0].queue_entered_at,
      ),
      key,
      round: group.round,
      seats,
      resolution,
    })
  }
  return index
}

function seatStatus(order: WorkOrder, job: Job | undefined, review?: PanelSeatReview): PanelSeatStatus {
  if (review) return 'verdict'
  switch (order.state) {
    case 'queued':
      return 'waiting'
    case 'stale':
      return 'stale'
    case 'timed_out':
      return 'timed_out'
    case 'cancelled':
      return 'cancelled'
    default:
      return job?.state === 'failed' ? 'failed' : 'deliberating'
  }
}

function preferredJobSummary(job: Job, events: TaskEvent[]): string | undefined {
  for (let i = events.length - 1; i >= 0; i--) {
    const event = events[i]
    if (event.job_id !== job.id) continue
    if (event.kind === 'job.summary' && typeof event.payload?.summary === 'string') {
      return event.payload.summary
    }
  }
  for (let i = events.length - 1; i >= 0; i--) {
    const event = events[i]
    if (event.job_id !== job.id) continue
    if (
      (event.kind === 'triage.completed' || event.kind === 'review.completed') &&
      typeof event.payload?.summary === 'string' &&
      event.payload.summary
    ) {
      const feedback =
        event.kind === 'review.completed' && typeof event.payload?.feedback === 'string'
          ? event.payload.feedback.trim()
          : ''
      return [event.payload.summary, feedback ? `Reviewer feedback: ${feedback}` : undefined]
        .filter(Boolean)
        .join('\n\n')
    }
  }
  return undefined
}

const outputInvalidKinds = new Set(['triage.output_invalid', 'spec.output_invalid', 'review.output_invalid'])

function rejectedOutputEvent(job: Job, events: TaskEvent[]): TaskEvent | undefined {
  if (job.state !== 'done' || preferredJobSummary(job, events) !== undefined) return undefined
  for (let i = events.length - 1; i >= 0; i--) {
    const event = events[i]
    if (event.job_id === job.id && outputInvalidKinds.has(event.kind)) return event
  }
  return undefined
}

function jobSummary(job: Job, events: TaskEvent[]): string {
  // Prefer the harness's own narration (job.summary), then accepted structured
  // stage outputs, then a job-specific output rejection and state fallback.
  const preferred = preferredJobSummary(job, events)
  if (preferred !== undefined) return preferred

  const rejected = rejectedOutputEvent(job, events)
  if (rejected) {
    const error = typeof rejected.payload?.error === 'string' ? rejected.payload.error.trim() : ''
    const punctuation = error && !/[.!?]$/.test(error) ? '.' : ''
    return error
      ? `Output rejected by the pipeline — ${error}${punctuation} Retrying with this feedback.`
      : 'Output rejected by the pipeline. Retrying with this feedback.'
  }

  switch (job.state) {
    case 'pending':
      return job.harness === 'external-mcp' ? 'Queued for an operator-owned agent over MCP.' : 'Queued.'
    case 'running':
      return 'In progress.'
    case 'failed': {
      for (let i = events.length - 1; i >= 0; i--) {
        const event = events[i]
        if (event.job_id !== job.id) continue
        if (event.kind === 'job.failed' && typeof event.payload?.error === 'string' && event.payload.error) {
          return event.payload.error
        }
      }
      return 'The job failed before producing a summary.'
    }
    default:
      return 'Completed.'
  }
}

function noteFor(
  event: TaskEvent,
  panels: PanelIndex,
  assignee?: TaskAssignee,
  members: WorkspaceMembership[] = [],
): Omit<Extract<TimelineEntry, { type: 'note' }>, 'type' | 'at' | 'key'> | undefined {
  const payload = event.payload ?? {}
  switch (event.kind) {
    // Assignment is an audited operator act, so it folds into the timeline
    // beside the other decisions rather than being legible only as a header
    // field that silently changed (REQ-4, AC-1.2). The event payload carries
    // the account ID alone; the task's hydrated assignee is what supplies a
    // name, so a still-current assignment reads as a person.
    case 'task.assignee.set': {
      const userID = typeof payload.assignee_user_id === 'string' ? payload.assignee_user_id : ''
      const resolved = assigneeForUser(userID, assignee, members)
      const name = resolved ? assigneeName(resolved) : userID
      return { title: name ? `Assigned to ${name}` : 'Assignee set' }
    }
    // Revoking a member's workspace binding clears their assignments too, so
    // this entry is not always a deliberate unassignment; it states what
    // happened and leaves the cause to the surrounding entries.
    case 'task.assignee.cleared':
      return { title: 'Assignee cleared' }
    case 'work_order.child_failed':
      return {
        title:
          payload.retry_suppressed === true
            ? 'The agent’s run failed — not retried automatically'
            : 'The agent’s run failed — retrying',
        detail:
          [payload.reason, payload.suppression_reason]
            .filter((value): value is string => typeof value === 'string' && value.length > 0)
            .join(' · ') || undefined,
        failureDetail: typeof payload.detail === 'string' && payload.detail.trim() ? payload.detail : undefined,
        alarm: true,
      }
    case 'work_order.stalled':
      return {
        title:
          payload.retry_suppressed === true
            ? 'The agent stopped responding — not retried automatically'
            : 'The agent stopped responding — retrying',
        detail:
          [payload.reason, payload.suppression_reason]
            .filter((value): value is string => typeof value === 'string' && value.length > 0)
            .join(' · ') || undefined,
        alarm: true,
      }
    case 'work_order.expired':
      return { title: 'The agent’s claim expired — needs your recovery', alarm: true }
    case 'work_order.recovered':
      return {
        title: 'Recovered by an operator',
        detail: typeof payload.prior_outcome === 'string' ? `Prior outcome: ${payload.prior_outcome}` : undefined,
      }
    case 'pull_request.opened':
      return {
        title: 'Pull request opened',
        detail: typeof payload.url === 'string' ? payload.url : undefined,
        href: typeof payload.url === 'string' ? payload.url : undefined,
      }
    // Review cards and panels already render outcome, feedback, and bounce
    // context. Keep the audit events, but do not duplicate these two semantic
    // review-transition records as standalone timeline notes.
    case 'pipeline.bounced':
    case 'review.completed':
      return undefined
    case 'pipeline.bounce_limit': {
      const source = typeof payload.source === 'string' ? payload.source.trim() : ''
      const sourceLabel = outputInvalidKinds.has(source)
        ? `${source.slice(0, source.indexOf('.'))} output validation`
        : source
      const maximum = typeof payload.max_bounces === 'number' ? `maximum ${payload.max_bounces} bounces` : ''
      return {
        title: 'Review check-in — paused after the configured rounds',
        detail: sourceLabel ? [`Source: ${sourceLabel}`, maximum].filter(Boolean).join(' · ') : undefined,
        alarm: true,
      }
    }
    case 'job.timeout':
      return {
        title: 'Timed out before finishing',
        detail: typeof payload.timeout === 'string' ? `Stopped after ${payload.timeout}` : undefined,
        alarm: true,
      }
    case 'work_order.plan_revision_requested': {
      // The request opens the decision gate, so the rationale reads inline
      // rather than behind a disclosure — it is the whole case the operator is
      // being asked to judge (REQ-4 AC-4.1).
      const request = planRevisionRequestFrom(event)
      if (!request) return undefined
      return {
        title: `The agent asked to revise plan v${request.planVersion}`,
        detail: request.rationale,
      }
    }
    case 'spec.version_created':
      return { title: `Plan v${typeof payload.version === 'number' ? payload.version : '?'} drafted` }
    case 'spec.version_approved':
      // The approval intervention renders the richer human "Approved" card.
      // Retain the audit event in the API without repeating it in the timeline.
      return undefined
    case 'blueprint.materialized':
      return {
        title: `${typeof payload.children_total === 'number' ? payload.children_total : (payload.children_created ?? '?')} tasks created from the blueprint`,
      }
    case 'blueprint.closed':
      return { title: 'Blueprint completed — all child tasks are finished' }
    case 'task.dependency_unsatisfiable':
      return {
        title: 'Dependency needs attention',
        detail:
          typeof payload.depends_on_task_id === 'string'
            ? `${payload.depends_on_task_id} closed without merging`
            : undefined,
        alarm: true,
      }
    case 'task.dependency_removed':
      return {
        title: 'Dependency removed',
        detail: typeof payload.reason === 'string' ? payload.reason : undefined,
      }
    case 'github_issue.publication_queued':
      return { title: 'GitHub issue publication queued' }
    case 'github_issue.publication_retry':
      return {
        title: 'GitHub issue publication retrying',
        detail: forgeFailureDetail(payload, 'last_error'),
        alarm: Boolean(payload.last_error),
      }
    case 'github_issue.publication_published':
    case 'github_issue.associated':
      return {
        title: 'GitHub issue associated',
        detail: typeof payload.issue_url === 'string' ? payload.issue_url : undefined,
        href: typeof payload.issue_url === 'string' ? payload.issue_url : undefined,
      }
    case 'github_issue.publication_failed':
      return {
        title: 'GitHub issue publication failed',
        detail: forgeFailureDetail(payload, 'last_error'),
        alarm: true,
      }
    case 'review.publication_retry':
      return {
        title: 'GitHub review publication retrying',
        detail: forgeFailureDetail(payload, 'last_error'),
        alarm: Boolean(payload.last_error),
      }
    case 'review.publication_failed':
      return {
        title: 'GitHub review publication failed',
        detail: forgeFailureDetail(payload, 'last_error'),
        alarm: true,
      }
    case 'github.review_redirected':
      return { title: 'Review comments on GitHub sent the task back for changes' }
    case 'review.round_completed':
      // The panel card's resolution banner is this event, rendered richer.
      if (isPanelReviewEvent(payload, panels)) return undefined
      return {
        title: `Review panel: ${String(payload.verdict ?? 'completed')}`,
        detail: typeof payload.summary === 'string' ? payload.summary : undefined,
      }
    case 'work_order.released':
      // A plan-revision request releases the claim in the same transaction that
      // opens the gate. The revision entry above already tells that story, so
      // the release is not repeated a second later as its own line (AC-4.1).
      if (payload.reason === planRevisionReleaseReason) return undefined
      return {
        title: 'Work-order claim released',
        detail: typeof payload.reason === 'string' ? payload.reason : undefined,
        alarm: payload.reason === 'harness exited without terminal verdict submission',
      }
    case 'work_order.preempted':
      return {
        title: 'Work-order attempt preempted by operator',
        detail:
          typeof payload.reason === 'string' ? `${payload.reason} · stops within one renewal interval` : undefined,
      }
    case 'merge.requested':
      return {
        title: 'Pull request merge requested',
        detail: typeof payload.url === 'string' ? payload.url : undefined,
        href: typeof payload.url === 'string' ? payload.url : undefined,
      }
    case 'merge.confirmed':
      return {
        title: 'Pull request merge confirmed by GitHub',
        detail: typeof payload.url === 'string' ? payload.url : undefined,
        href: typeof payload.url === 'string' ? payload.url : undefined,
      }
    case 'merge.reconciled':
      return {
        title: 'Already-merged pull request reconciled',
        detail: typeof payload.url === 'string' ? payload.url : undefined,
        href: typeof payload.url === 'string' ? payload.url : undefined,
      }
    case 'merge.failed':
      return { title: 'Merge needs operator action', detail: forgeFailureDetail(payload, 'error'), alarm: true }
    case 'merge.blocked':
      return {
        title: 'Merge blocked — conflict fix required',
        detail: typeof payload.reason_code === 'string' ? `Reason: ${payload.reason_code}` : undefined,
        alarm: true,
      }
    case 'merge.conflict_fix_dispatched':
      return {
        title: 'Conflict fix dispatched to implementation',
        detail: typeof payload.reason_code === 'string' ? `Reason: ${payload.reason_code}` : undefined,
      }
    case 'approval.stale':
      return {
        title: 'Approval became stale after the PR head changed',
        detail: typeof payload.review_scope === 'string' ? `Refresh scope: ${payload.review_scope}` : undefined,
        alarm: true,
      }
    case 'review.refresh_head_advanced':
      return {
        title: 'Refresh review retargeted to the newly pushed head',
        detail: typeof payload.new_head === 'string' ? `Head: ${payload.new_head.slice(0, 8)}` : undefined,
      }
    case 'review.refresh_round_created':
      return {
        title: `Refresh review round ${String(payload.review_round ?? '')} started`,
        detail: typeof payload.review_scope === 'string' ? `Scope: ${payload.review_scope}` : undefined,
      }
    case 'review.refresh_skipped':
      return {
        title: 'Clean head update re-armed without refresh review',
        detail: typeof payload.reason_code === 'string' ? `Reason: ${payload.reason_code}` : undefined,
      }
    case 'dispatch.failed':
      return {
        title: 'Dispatch failed',
        detail: typeof payload.error === 'string' ? payload.error : undefined,
        alarm: true,
      }
    case 'job.log_stream_degraded':
      return { title: 'Live logs unavailable — the full transcript is still archived' }
    default:
      return undefined
  }
}

// Fallback summaries a work order's self-reported progress may replace.
const genericSummaries = new Set([
  'Queued.',
  'Queued for an operator-owned agent over MCP.',
  'In progress.',
  'Completed.',
  'The job failed before producing a summary.',
])

// Work orders fold into the timeline instead of a separate
// block: narration and attribution enrich the matching stage entry, and only
// states a job entry cannot carry — waiting for an agent, stale, timed out —
// become entries of their own.
function orderEntry(order: WorkOrder, hasJobEntry: boolean): Extract<TimelineEntry, { type: 'order' }> | undefined {
  const stage = order.stage === 'spec' ? 'Plan' : order.stage === 'implement' ? 'Implementation' : 'Review'
  const base = { type: 'order' as const, at: order.queue_entered_at, key: `order-${order.id}`, order }
  switch (order.state) {
    case 'queued':
      return {
        ...base,
        tone: 'waiting',
        title: `${stage} — waiting for an operator agent`,
        detail: 'Any agent connected over MCP can claim this. The server URL is in Settings.',
      }
    case 'claimed':
      // A claimed order normally has a running job entry carrying the story.
      if (hasJobEntry) return undefined
      return {
        ...base,
        tone: 'active',
        title: `${stage} — in progress`,
        detail: [order.claimed_by, order.agent, order.model].filter(Boolean).join(' · ') || undefined,
      }
    case 'stale':
      return {
        ...base,
        tone: 'alarm',
        title: `${stage} — went stale in the queue`,
        detail: 'No agent claimed it before queue retention elapsed. Redispatch to offer it again.',
      }
    case 'timed_out':
      return {
        ...base,
        at: order.execution_deadline ?? order.queue_entered_at,
        tone: 'alarm',
        title: `${stage} — timed out`,
        detail: 'The execution deadline elapsed before the agent finished; the retry policy applies.',
      }
    default:
      // submitted/completed/cancelled: the job entry and review events tell it.
      return undefined
  }
}

// The costed event timeline: one entry per stage execution,
// interleaved with the notable pipeline events and every human/agent
// decision, in wall-clock order. This is the audit log rendered as a story.
export function buildTimeline(item: ActivityItem, members: WorkspaceMembership[] = []): TimelineEntry[] {
  const entries: TimelineEntry[] = []
  const panels = buildReviewPanels(item)
  const dependencyBlockedOrder = dependencyBlockedImplementationOrder(item)
  entries.push(...panels.panels)
  const orderByJob = new Map((item.work_orders ?? []).map((order) => [order.job_id, order]))
  const startedJobs = new Set(item.jobs.filter((job) => job.started_at).map((job) => job.id))
  for (const job of item.jobs) {
    if (!job.started_at || panels.jobIds.has(job.id)) continue
    const order = orderByJob.get(job.id)
    let summary = jobSummary(job, item.events)
    if (order?.progress && genericSummaries.has(summary)) summary = order.progress
    // BYOA jobs carry a placeholder tier; the work order knows the model
    // the operator's agent actually ran.
    const model = job.model_tier === 'operator-owned' && order?.model ? order.model : job.model_tier
    const tone = rejectedOutputEvent(job, item.events) ? 'warning' : 'default'
    entries.push({ type: 'job', at: job.started_at, job, summary, model, tone, order })
  }
  for (const order of item.work_orders ?? []) {
    if (order.last_agent_activity_at && order.last_agent_activity_label) {
      entries.push({
        type: 'note',
        at: order.last_agent_activity_at,
        key: `agent-activity-${order.id}`,
        title: `Agent activity — ${order.last_agent_activity_label}`,
        fullProgress:
          order.progress && order.last_agent_activity_label.startsWith('Progress:') ? order.progress : undefined,
      })
    }
    if (panels.orderIds.has(order.id)) continue
    const entry = order.id === dependencyBlockedOrder?.id ? undefined : orderEntry(order, startedJobs.has(order.job_id))
    if (entry) entries.push(entry)
  }
  for (const event of item.events) {
    if (attemptEventKinds.has(event.kind)) continue
    const note = noteFor(event, panels, item.task.assignee, members)
    if (note) entries.push({ type: 'note', at: event.at, key: `event-${event.id}`, ...note })
  }
  for (const diagnostic of item.review_diagnostics ?? []) {
    // Active claims already appear as deliberating seats in the review panel.
    // Keep the diagnostic in the API, but do not repeat it in the timeline.
    if (diagnostic.status === 'claimed_without_verdict') continue
    const seat = diagnostic.review_seat ? `seat ${diagnostic.review_seat}` : 'review seat'
    entries.push({
      type: 'note',
      at:
        diagnostic.status === 'expired_without_verdict'
          ? (diagnostic.lease_expires_at ?? diagnostic.claimed_at ?? item.task.created_at)
          : (diagnostic.claimed_at ?? item.task.created_at),
      key: `review-diagnostic-${diagnostic.status}-${diagnostic.work_order_id}`,
      title:
        diagnostic.status === 'expired_without_verdict'
          ? 'Review claim expired without verdict submission'
          : 'Review claimed without terminal verdict submission',
      detail: `${seat} · ${diagnostic.work_order_id} · ${diagnostic.reason}`,
      alarm: diagnostic.status === 'expired_without_verdict',
    })
  }
  for (const intervention of item.interventions) {
    // The synthetic redirect a panel bounce records (actor "review:round:N")
    // duplicates the panel card's merged notes — the audit row stays in the
    // API, the story is told once here.
    const roundActor = /^review:round:(\d+)$/.exec(intervention.actor_id)
    if (
      roundActor &&
      (panels.rounds.has(Number(roundActor[1])) || (Number(roundActor[1]) === 0 && panels.orderIds.size > 0))
    )
      continue
    entries.push({ type: 'intervention', at: intervention.at, intervention })
  }
  return entries.sort((a, b) => new Date(a.at).getTime() - new Date(b.at).getTime())
}
