import { type GroupKey, stageLabels, taskStateLabels, workOrderStageLabels } from './contracts'
import type {
  ActivityItem,
  ActivitySummary,
  Intervention,
  Job,
  NextAction,
  PendingProposal,
  Task,
  TaskAssignee,
  TaskEventView,
  TaskJobResult,
  TaskRelation,
  TaskReviewResult,
  TaskReviewRound,
  TaskReviewRoundSummary,
  TaskReviewSeatMetadata,
  TaskSummaries,
  WorkOrder,
  WorkOrderVerificationCheckpointGround,
  WorkOrderVerificationCheckpointPermission,
  WorkspaceMembership,
} from './types'

// The name a person is known by, falling through the identity fields the task
// projection carries. The raw user ID is the last resort: it identifies an
// account, it does not name a colleague, so it never displaces a real name
// (REQ-4, AC-4.3).
export function assigneeName(assignee: TaskAssignee): string {
  return assignee.display_name || assignee.email || assignee.user_id
}

// req-accounts-and-membership REQ-3 and AC-4.3: keep credential-derived
// attribution and resolve names only through the current workspace roster.
// The creator is the server's `summaries.creator`, the actor of the first
// task.created event in the complete ledger (component-http-api).
export function taskCreator(creator: TaskSummaries['creator'], members: WorkspaceMembership[] = []) {
  const event = creator
  if (!event) return { label: 'Unknown', detail: 'Creator information is unavailable' }
  const id = typeof event.actor_id === 'string' ? event.actor_id.trim() : ''
  const role = event.actor_role
  if (!id || !['human', 'agent', 'runner', 'system'].includes(role)) {
    return { label: 'Unknown', detail: 'Creator information is unavailable' }
  }
  const prefix = { human: 'user:', agent: 'agent:', runner: 'worker:', system: 'system:' }[
    role as 'human' | 'agent' | 'runner' | 'system'
  ]
  const identifier = id.startsWith(prefix) ? id.slice(prefix.length).trim() : id
  if (!identifier) return { label: 'Unknown', detail: 'Creator information is unavailable' }
  if (role === 'human') {
    const member = members.find((entry) => entry.user_id === identifier)
    return { label: member ? assigneeName(member) : `User · ${identifier}`, detail: `User · ${id}` }
  }
  const actorClass = { agent: 'Agent', runner: 'Runner', system: 'System' }[role as 'agent' | 'runner' | 'system']
  return { label: `${actorClass} · ${identifier}`, detail: `${actorClass} · ${id}` }
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

// The Board column a task occupies, read from server fields only (NA-6): the
// next action's kind places merge work and recovery holds, and otherwise the
// stage the task is in or about to enter. A plan approval therefore lands
// under Plan and a failed job under the stage that failed.
const mergeKinds = new Set(['merge_approval', 'merge_execution', 'merge_conflict', 'merging'])
const triageKinds = new Set(['parked', 'dispatch_exhausted'])

export function boardColumnFor(item: ActivitySummary): GroupKey {
  const { state } = item.task
  if (state === 'merged' || state === 'closed') return 'done'
  const kind = item.next_action?.kind ?? ''
  if (mergeKinds.has(kind)) return 'merge'
  if (triageKinds.has(kind)) return 'triage'
  const stage =
    state === 'queued' || state === 'running'
      ? (item.task.next_stage ?? item.latest_stage)
      : (item.latest_stage ?? item.task.next_stage)
  switch (stage) {
    case 'spec':
      return 'spec'
    case 'implement':
      return 'implement'
    case 'verify':
      return 'verify'
    case 'review':
      return 'review'
    case 'merge':
      return 'merge'
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

// The latest pull_request.opened URL, from `summaries.pull_request`.
export function pullRequestURL(summaries: TaskSummaries | undefined): string | undefined {
  const url = summaries?.pull_request?.url
  return typeof url === 'string' ? url : undefined
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

// Both facts come from the server's summaries of the complete ledger: the
// latest pull_request.opened and the latest review.round_completed, so the
// gate states them even when their events lie outside the bounded page.
export function mergeGateReview(item: ActivityItem): MergeGateReview {
  const review: MergeGateReview = { headSHA: item.task.reviewed_head_sha || undefined }
  const pullRequest = item.summaries?.pull_request
  if (pullRequest) {
    review.headSHA = review.headSHA ?? (typeof pullRequest.head_sha === 'string' ? pullRequest.head_sha : undefined)
    review.baseSHA = typeof pullRequest.base_sha === 'string' ? pullRequest.base_sha : undefined
    if (typeof pullRequest.url === 'string' && pullRequest.url) {
      review.pullRequest = {
        url: pullRequest.url,
        number: typeof pullRequest.number === 'number' ? pullRequest.number : undefined,
        repository: typeof pullRequest.repository === 'string' ? pullRequest.repository : undefined,
      }
    }
  }
  // The last completed round is the verdict the gate opened on; the seats that
  // reported into it say how many independent reviewers stand behind it.
  // Seat metadata alone describes an open round; only a stated verdict is one.
  const round = item.summaries?.review_round
  if (round && (round.verdict === 'approve' || round.verdict === 'changes_requested')) {
    review.verdict = {
      verdict: round.verdict === 'changes_requested' ? 'changes_requested' : 'approve',
      summary: typeof round.summary === 'string' && round.summary.trim() ? round.summary.trim() : undefined,
      seats: typeof round.review_count === 'number' ? round.review_count : 0,
    }
  }
  return review
}

// The reason code and bounce source the merge-gate request-changes command
// records (conveyor:internal/store/changes_requested.go). The dashboard reads
// them; it never writes them, and neither string is ever shown to a person.
export const userRequestChangesReason = 'user-request-changes'

// req-intake-and-triage REQ-3; req-operator-next-action REQ-6:
// summary projections carry stages, not execution evidence. Only detail
// evidence below may suppress a human gate.
export function failedTriageSummary(item: {
  task: Pick<Task, 'state' | 'next_stage' | 'recovery_stage'>
  latest_stage?: Task['next_stage']
}): boolean {
  return (
    item.task.state === 'awaiting_human' &&
    !item.task.next_stage &&
    (!item.latest_stage || item.latest_stage === 'triage') &&
    (!item.task.recovery_stage || item.task.recovery_stage === 'triage') &&
    (item.latest_stage === 'triage' || item.task.recovery_stage === 'triage')
  )
}

export interface FailedTriage {
  jobId: string
  reason: string
  repairRequired: boolean
  guidance: string
}

export function failedTriage(item: ActivityItem): FailedTriage | undefined {
  if (item.task.state !== 'awaiting_human' || item.task.next_stage || item.task.recovery_stage !== 'triage')
    return undefined
  const jobs = [...(item.jobs ?? [])].sort(
    (a, b) => (Date.parse(a.started_at ?? a.ended_at ?? '') || 0) - (Date.parse(b.started_at ?? b.ended_at ?? '') || 0),
  )
  const latest = jobs.at(-1)
  if (!latest || latest.stage !== 'triage') return undefined
  // The server names the latest triage job's failure and whether a later
  // dispatch, intervention, claim, job, or queued order superseded it, by
  // (at, id) over the complete ledger (component-http-api, Bounded detail view).
  // A superseded incident stays hidden even while a separately refreshed task
  // projection still carries its old recovery stage.
  const recordedFailure = item.summaries?.triage_failure
  const failure = recordedFailure?.job_id === latest.id ? recordedFailure : undefined
  if (latest.state !== 'failed' && !failure) return undefined
  if (failure?.superseded) return undefined
  const diagnostic: Record<string, unknown> = failure?.diagnostic ?? {}
  const recorded = typeof failure?.error === 'string' ? failure.error.trim() : ''
  const diagnosticText = [diagnostic.phase, diagnostic.provider_code]
    .filter((value): value is string => typeof value === 'string' && Boolean(value.trim()))
    .join(' · ')
  const reason = recorded || diagnosticText || 'The triage job failed without a recorded explanation.'
  const budget = /triage.*budget|(?:task context|model input|attachment).*(?:exceeds|exceed|limit)/i.test(reason)
  const attachment =
    diagnostic.phase === 'attachment_validation' ||
    /image preparation|invalid.*(?:image|attachment)|unsupported.*(?:image|attachment|artifact)|content does not match|context artifact|attachment preparation/i.test(
      reason,
    )
  const context = /task.context|context.*(?:invalid|missing|unavailable)|authority.*budget/i.test(reason)
  const repairRequired = budget || attachment || context
  return {
    jobId: latest.id,
    reason,
    repairRequired,
    guidance: budget
      ? 'Reduce the identified attachment or task context to fit the recorded input limit before retrying triage.'
      : attachment
        ? 'Repair or replace the identified attachment and its media type before retrying triage. An unchanged attachment will fail again.'
        : context
          ? 'Repair the task context identified in the failure before retrying triage.'
          : 'Retry triage after resolving the recorded failure. This restarts triage; no plan or implementation is being approved.',
  }
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

// Every kind `noteFor` has a case for. The type below makes the compiler hold
// this list and the switch to the same members in both directions, and
// internal/httpapi/task_detail_summaries_test.go parses these three literals,
// so each stays a single array or Set literal of single-quoted strings.
export const noteEventKinds = [
  'task.assignee.set',
  'task.assignee.cleared',
  'work_order.child_failed',
  'work_order.stalled',
  'work_order.expired',
  'work_order.recovered',
  'pull_request.opened',
  'pipeline.bounced',
  'review.completed',
  'pipeline.bounce_limit',
  'job.timeout',
  'work_order.plan_revision_requested',
  'spec.version_created',
  'spec.version_approved',
  'blueprint.materialized',
  'blueprint.closed',
  'task.dependency_unsatisfiable',
  'task.dependency_removed',
  'github_issue.publication_queued',
  'github_issue.publication_retry',
  'github_issue.publication_published',
  'github_issue.associated',
  'github_issue.publication_failed',
  'review.publication_retry',
  'review.publication_failed',
  'github.review_redirected',
  'review.round_completed',
  'work_order.released',
  'work_order.preempted',
  'merge.requested',
  'merge.confirmed',
  'merge.reconciled',
  'merge.failed',
  'merge.blocked',
  'merge.conflict_fix_dispatched',
  'approval.stale',
  'review.refresh_head_advanced',
  'review.refresh_round_created',
  'review.refresh_skipped',
  'dispatch.failed',
  'job.log_stream_degraded',
  'task.state_changed',
  'task.dependency_added',
  'task.context_proposal_confirmed',
  'task.context_proposal_dismissed',
] as const

type NoteEventKind = (typeof noteEventKinds)[number]

// Kinds `noteFor` never renders: the review cards, panels, the plan section,
// and the approval intervention already tell their story, and the remaining
// publication bookkeeping reads as noise. Technical activity keeps them all
// (component-web-task-surfaces, Task detail composition; task 261028-5c2ae2).
const hiddenNoteKinds = new Set([
  'review.completed',
  'spec.version_approved',
  'spec.version_created',
  'github_issue.publication_queued',
  'github_issue.publication_published',
  'github_issue.associated',
  'review.publication_retry',
])

// Attempt kinds that read as an entry when the factory retries the run on its
// own (req-operator-next-action AC-7.3); see automaticRetryEvent.
const automaticRetryEventKinds = new Set(['work_order.child_failed', 'work_order.stalled'])

// The effective human-timeline rule: every kind `noteFor` can render, minus the
// attempt bookkeeping `buildTimeline` skips before `noteFor` runs other than
// automatic retries, minus the kinds `noteFor` always hides. The server's human selection must equal this
// list (component-http-api, Bounded detail view).
export const humanTimelineEventKinds = noteEventKinds.filter(
  (kind) => (!attemptEventKinds.has(kind) || automaticRetryEventKinds.has(kind)) && !hiddenNoteKinds.has(kind),
)

const humanTimelineKinds = new Set<string>(humanTimelineEventKinds)

// Attempt history stays out of the timeline, except a failed or stalled run
// the factory retries on its own: that retry is a state the task passed
// through, so it reads as an entry (req-operator-next-action AC-7.3).
function automaticRetryEvent(event: TaskEventView) {
  // An omitted payload keeps its boolean fields in payload_index.
  const fields = event.payload ?? event.payload_index ?? {}
  return automaticRetryEventKinds.has(event.kind) && fields.retry_suppressed !== true
}

// An automatic retry reads as the state it is; the attempt's own failure detail
// stays in technical activity. An omitted payload keeps its omission marker and
// audit reference, so the complete record stays one disclosure away
// (component-web-task-surfaces, History pages).
function retryNote(note: NoteContent): NoteContent {
  return note.omittedEventId
    ? { title: note.title, alarm: note.alarm, detail: note.detail, omittedEventId: note.omittedEventId }
    : { title: note.title, alarm: note.alarm }
}

// The closing commands whose own record already tells the story: a rejection
// and a cancellation are interventions, a blueprint close has its own note.
const closeCommandsWithEntries = new Set(['intervention.reject', 'task.cancel', 'blueprint.close'])

export type CurrentExecutionKind =
  | 'running'
  | 'dependency_waiting'
  | 'dependency_attention'
  | 'retry_pending'
  | 'provider_usage_limit'
  | 'checkout_blocked'
  | 'released'
  | 'expired'
  | 'verification_checkpoint'
  | 'checkpoint_decision'

export type CurrentExecutionStage = 'Plan' | 'Implementation' | 'Review' | 'Verification'

// Which of a state's lines carry information beyond the server's summary. The
// next-action card prints only the marked ones beside that summary, so a fact
// prints once and an empty-state sentence never becomes a row; the recovery
// card reads the strings themselves and ignores these flags
// (component-web-task-surfaces, Next-action card; DEC-61).
export interface CurrentExecutionFacts {
  title: boolean
  blocker: boolean
  retry: boolean
  nextAction: boolean
}

const allFacts: CurrentExecutionFacts = { title: true, blocker: true, retry: true, nextAction: true }

export interface CurrentExecutionState {
  kind: CurrentExecutionKind
  status: 'progressing' | 'paused'
  stage: CurrentExecutionStage
  order: WorkOrder
  attemptId?: string
  title: string
  blocker: string
  retry: string
  // Client guidance for the recovery card and the next-action card. Absent
  // when the only guidance is the server's own summary, which NextActionCard
  // already states once per surface (DEC-61; feature-operator-next-action NA-6).
  nextAction?: string
  facts: CurrentExecutionFacts
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
// next-action card, timeline narration, and queued affordances so they
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

// The server's next action names the order that is current; this maps it onto
// the execution state the next-action card and the recovery card render. Which order is
// current, and whether it is running, retrying, waiting or paused, is the
// server's statement (feature-operator-next-action NA-6); only the paused
// variant is refined from the order's own recorded failure.
export function deriveCurrentExecutionState(
  item: ActivityItem,
  action: NextAction | undefined,
): CurrentExecutionState | undefined {
  const current = action?.current_order
  const order = current && (item.work_orders ?? []).find((candidate) => candidate.id === current.id)
  if (!action || !order) return undefined
  const attemptId = action.attempt?.attempt_id || order.last_attempt_id || order.attempt_id
  const stage: CurrentExecutionStage =
    order.stage === 'spec'
      ? 'Plan'
      : order.stage === 'review'
        ? 'Review'
        : order.stage === 'verify'
          ? 'Verification'
          : 'Implementation'
  switch (action.kind) {
    case 'working':
      if (order.state !== 'claimed') return undefined
      return {
        kind: 'running',
        status: 'progressing',
        stage,
        order,
        attemptId,
        title: `${stage} is in progress`,
        blocker: 'No current blocker.',
        retry: 'No retry is needed.',
        nextAction: 'No operator action is needed.',
        // The summary already says an agent is working; every other line is
        // an empty state.
        facts: { title: false, blocker: false, retry: false, nextAction: false },
        action: 'none',
      }
    case 'retrying':
      return {
        kind: 'retry_pending',
        status: 'progressing',
        stage,
        order,
        attemptId,
        title: 'Automatic retry scheduled',
        blocker:
          order.last_failure_category === 'provider_usage_limit'
            ? 'The provider usage limit paused the last attempt.'
            : 'The last attempt ended before completion.',
        retry: 'Conveyor will retry automatically.',
        nextAction: 'No operator action is needed.',
        facts: { title: false, blocker: true, retry: true, nextAction: false },
        action: 'none',
      }
    case 'blocked':
      return {
        kind: 'dependency_waiting',
        status: 'progressing',
        stage,
        order,
        title: 'Waiting on dependencies',
        blocker: 'Implementation is gated by unresolved task dependencies.',
        retry: 'Not applicable.',
        // No nextAction: the summary is NextActionCard's (DEC-61); the card
        // keeps only the dependency links.
        facts: { title: false, blocker: true, retry: true, nextAction: false },
        action: 'none',
        blockingDependencies: blockingDependencies(item),
      }
    case 'dependency_unsatisfiable':
      return {
        kind: 'dependency_attention',
        status: 'paused',
        stage,
        order,
        title: 'Dependency needs attention',
        blocker: 'A required dependency closed without merging.',
        retry: 'Redispatch is unavailable while the dependency is unsatisfiable.',
        nextAction: 'Unlink the dead dependency with an audit reason, or cancel this task.',
        facts: { ...allFacts, title: false },
        action: 'none',
        blockingDependencies: blockingDependencies(item),
        unsatisfiableDependencyIDs:
          (order.unsatisfiable_task_ids?.length ?? 0) > 0
            ? order.unsatisfiable_task_ids
            : action.blocked_by?.map((blocker) => blocker.task_id),
      }
    case 'agent_question':
      return (
        checkpointDecisionState(order, attemptId, stage, action) ??
        pausedExecutionState(item, order, attemptId, stage, action)
      )
    case 'stalled':
    case 'verification_disposition':
    case 'dispatch_exhausted':
      return pausedExecutionState(item, order, attemptId, stage, action)
    default:
      return undefined
  }
}

// A deliberate checkpoint is a question for the operator, not a failure: its
// title, next step, and blocker are the server's label and question and the
// agent's own decision request, and the summary stays on NextActionCard
// (component-web-task-surfaces, Implementation checkpoints;
// req-operator-next-action AC-1.7). A checkpoint without a decision request
// keeps the released wording (req-260820-394cac AC-1.3), and a verify order
// keeps its verification checkpoint presentation.
function checkpointDecisionState(
  order: WorkOrder,
  attemptId: string | undefined,
  stage: CurrentExecutionStage,
  action: NextAction,
): CurrentExecutionState | undefined {
  const decisionRequest = order.checkpoint?.decision_request?.trim()
  if (order.stage === 'verify' || !decisionRequest) return undefined
  return {
    kind: 'checkpoint_decision',
    status: 'paused',
    stage,
    order,
    attemptId,
    title: action.label,
    blocker: decisionRequest,
    retry: 'No automatic retry is pending.',
    nextAction: action.question,
    // The next step is the question, which the next-action card prints.
    facts: { ...allFacts, nextAction: false },
    action: 'recover',
  }
}

function pausedExecutionState(
  item: ActivityItem,
  order: WorkOrder,
  attemptId: string | undefined,
  stage: CurrentExecutionStage,
  action: NextAction,
): CurrentExecutionState {
  const verificationCheckpoint = order.stage === 'verify' ? order.checkpoint?.verification : undefined
  if (verificationCheckpoint && order.state === 'queued') {
    return {
      kind: 'verification_checkpoint',
      status: 'paused',
      stage,
      order,
      attemptId,
      title: 'Verification checkpoint — operator action required',
      blocker: verificationCheckpoint.reason,
      retry: 'Automatic replay is suppressed until an operator recovers this order.',
      nextAction: verificationCheckpoint.required_action,
      facts: allFacts,
      action: 'recover',
    }
  }
  if (isDirtyPrimaryCheckout(order)) {
    return withDependencyContext(
      {
        kind: 'checkout_blocked',
        status: 'paused',
        stage,
        order,
        attemptId,
        title: `${stage} paused — checkout needs attention`,
        blocker: 'The primary checkout has pre-existing changes, so Conveyor left them untouched.',
        retry: 'Conveyor will not retry automatically while this safety gate is unresolved.',
        nextAction: 'Resolve the primary checkout changes, then retry the implementation.',
        facts: allFacts,
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
        stage,
        order,
        attemptId,
        title: `${stage} paused — provider limit reached`,
        blocker: 'The provider usage or capacity limit stopped the last attempt.',
        retry: 'No automatic retry is pending.',
        nextAction: 'Retry the implementation after the provider limit has cleared.',
        facts: allFacts,
        action: 'retry_implementation',
      },
      item,
    )
  }
  const stall = action.details.stall_reason
  if (
    stall === 'stale' ||
    stall === 'timed_out' ||
    order.state === 'stale' ||
    order.state === 'timed_out' ||
    order.last_attempt_outcome === 'expired'
  ) {
    return withDependencyContext(
      {
        kind: 'expired',
        status: 'paused',
        stage,
        order,
        attemptId,
        title: `${stage} paused — recovery needed`,
        blocker:
          order.state === 'stale' || stall === 'stale'
            ? 'The order was not claimed before its queue deadline.'
            : 'The claim or execution window ended before completion.',
        retry: 'No automatic retry is pending.',
        nextAction: 'Recover the work order to try again.',
        facts: allFacts,
        action: 'recover',
      },
      item,
    )
  }
  return withDependencyContext(
    {
      kind: 'released',
      status: 'paused',
      stage,
      order,
      attemptId,
      title: `${stage} paused — recovery needed`,
      blocker:
        humanizeClaimRefusal(order.last_failure_message, item.task.assignee) ||
        'The latest attempt released the work before completion.',
      retry: 'No automatic retry is pending.',
      nextAction: 'Recover the work order to try again.',
      facts: allFacts,
      action: 'recover',
    },
    item,
  )
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
export function planRevisionRequestFrom(event: {
  payload?: Record<string, unknown> | null
}): PlanRevisionRequest | null {
  const rationale = typeof event.payload?.rationale === 'string' ? event.payload.rationale.trim() : ''
  const planVersion =
    typeof event.payload?.plan_version === 'number'
      ? event.payload.plan_version
      : Number.parseInt(String(event.payload?.plan_version ?? ''), 10)
  if (!rationale || !Number.isInteger(planVersion) || planVersion <= 0) return null
  return { planVersion, rationale }
}

// The newest plan-revision request the agent recorded, from
// `summaries.plan_revision_request`: the rationale and plan version the
// decision card lays out. Whether that request is the open decision is the
// server's next action (kind `plan_revision`), never an event walk
// (feature-operator-next-action NA-9).
export function latestPlanRevisionRequest(summaries: TaskSummaries | undefined): PlanRevisionRequest | null {
  const request = summaries?.plan_revision_request
  if (!request) return null
  return planRevisionRequestFrom({ payload: { rationale: request.rationale, plan_version: request.plan_version } })
}

export type TimelineEntry =
  | {
      type: 'job'
      at: string
      job: Job
      summary: string
      // Set when the summary text was cut at 16,384 bytes: the event whose
      // audit read returns the full text.
      summaryEventId?: string
      model: string
      tone: 'default' | 'warning'
      order?: WorkOrder
      // An implement job's attempt number, set only when the task has more
      // than one started implement job, and its delivery line.
      attempt?: number
      delivery?: ImplementationDelivery
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
      // A selected event whose payload the server omitted: the note is titled
      // from its kind and its audit disclosure loads the complete payload.
      omittedEventId?: string
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
  | {
      type: 'panel'
      at: string
      key: string
      round: number
      seats: PanelSeat[]
      resolution?: PanelResolution
      // The round's current summary from `summaries.review_round_summaries`,
      // shown above the seats (feature-review-round-summary SU-6).
      roundSummary?: TaskReviewRoundSummary
      // This round's own seat count and minimum, and whether an operator may
      // add seats to it now (feature-review-seat-minimum RS-7).
      seatMetadata?: TaskReviewSeatMetadata
      seatAdditionOpen?: boolean
    }

// One review round is rendered as a single deliberating body: seats appear
// inside one card instead of N sibling job cards. A seat is
// a review work order plus whatever exists of its job and verdict.
export type PanelSeatStatus = 'waiting' | 'deliberating' | 'verdict' | 'stale' | 'timed_out' | 'failed' | 'cancelled'

export interface PanelSeatReview {
  verdict: 'approve' | 'changes_requested'
  summary: string
  feedback: string
  at: string
  // Set when the summary or feedback was cut at 16,384 bytes.
  truncatedEventId?: string
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

// Absorption is evaluated on the payload, or on `payload_index` when the
// server omitted the payload, before any omitted-payload fallback, so an
// absorbed round stays hidden whatever its size (component-http-api).
function isPanelReviewEvent(event: TaskEventView, index: PanelIndex): boolean {
  if (event.payload) {
    const payload = event.payload
    const round = typeof payload.review_round === 'number' ? payload.review_round : undefined
    if (round !== undefined && round > 0) return index.rounds.has(round)
    const reviews = Array.isArray(payload.reviews) ? payload.reviews : []
    return reviews.some(
      (review) =>
        typeof (review as Record<string, unknown>)?.review_work_order_id === 'string' &&
        index.orderIds.has((review as Record<string, unknown>).review_work_order_id as string),
    )
  }
  const summary = event.payload_index
  if (!summary) return false
  const round = typeof summary.review_round === 'number' ? summary.review_round : undefined
  if (round !== undefined && round > 0) return index.rounds.has(round)
  const orderIDs = Array.isArray(summary.review_work_order_ids) ? summary.review_work_order_ids : []
  return orderIDs.some((orderID) => typeof orderID === 'string' && index.orderIds.has(orderID))
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
  // Seat verdicts, round resolutions, and bounce counts come from the server's
  // summaries of the complete ledger, so a panel keeps its text when its
  // events lie outside the bounded page (component-web-task-surfaces).
  const summaries = item.summaries
  const seatReview = (result: TaskReviewResult | undefined): PanelSeatReview | undefined =>
    result && {
      verdict: result.verdict === 'changes_requested' ? 'changes_requested' : 'approve',
      summary: typeof result.summary === 'string' ? result.summary : '',
      feedback: typeof result.feedback === 'string' ? result.feedback : '',
      at: result.at,
      truncatedEventId: result.truncated ? result.event_id : undefined,
    }
  const roundResolution = (round: TaskReviewRound | undefined): PanelResolution | undefined =>
    round && {
      verdict: round.verdict === 'changes_requested' ? 'changes_requested' : 'approve',
      summary: typeof round.summary === 'string' ? round.summary : undefined,
      at: round.at,
    }

  // Seats are added only to the latest positive round while it is open
  // (feature-review-seat-minimum RS-5, RS-7); the server stays authoritative.
  const latestRound = summaries?.review_round?.review_round
  const terminal = item.task.state === 'merged' || item.task.state === 'closed'

  for (const [key, group] of groups) {
    const seats: PanelSeat[] = group.orders
      .slice()
      .sort((a, b) => (a.review_seat ?? 0) - (b.review_seat ?? 0))
      .map((order) => {
        const job = jobsById.get(order.job_id)
        // A seat's verdict is found by its order, then by its job: older
        // review.completed events carry no order id.
        const review = seatReview(
          summaries?.review_results?.by_order?.[order.id] ?? summaries?.review_results?.by_job?.[order.job_id],
        )
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
    const resolution = roundResolution(
      group.round > 0
        ? summaries?.review_rounds?.by_round?.[String(group.round)]
        : summaries?.review_rounds?.by_order?.[group.orders[0].id],
    )
    if (resolution && resolution.verdict === 'changes_requested') {
      resolution.bounce = summaries?.review_bounces?.[String(group.round)]
    }
    const seatMetadata =
      group.round > 0
        ? summaries?.review_round_seats?.by_round?.[String(group.round)]
        : summaries?.review_round_seats?.by_order?.[group.orders[0].id]
    const seatAdditionOpen =
      group.round > 0 &&
      group.round === latestRound &&
      seatMetadata?.superseded !== true &&
      !resolution &&
      seats.some((seat) => !seat.review) &&
      !terminal
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
      roundSummary: group.round > 0 ? summaries?.review_round_summaries?.[String(group.round)] : undefined,
      seatMetadata,
      seatAdditionOpen,
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

interface JobSummaryText {
  text: string
  // The event whose audit read returns the full text when the server cut it.
  truncatedEventId?: string
}

// A job's text comes from `summaries.job_results`, so a job whose summary
// event is older than the bounded page keeps it (component-web-task-surfaces).
function preferredJobSummary(results: TaskJobResult | undefined): JobSummaryText | undefined {
  const summary = results?.summary
  if (summary && typeof summary.text === 'string') {
    return { text: summary.text, truncatedEventId: summary.truncated ? summary.event_id : undefined }
  }
  const stage = results?.stage_summary
  if (stage && typeof stage.text === 'string' && stage.text) {
    const feedback = typeof stage.review_feedback === 'string' ? stage.review_feedback.trim() : ''
    return {
      text: [stage.text, feedback ? `Reviewer feedback: ${feedback}` : undefined].filter(Boolean).join('\n\n'),
      truncatedEventId: stage.truncated ? stage.event_id : undefined,
    }
  }
  return undefined
}

const outputInvalidKinds = new Set(['triage.output_invalid', 'spec.output_invalid', 'review.output_invalid'])

function rejectedOutput(job: Job, results: TaskJobResult | undefined): TaskJobResult['output_rejection'] {
  if (job.state !== 'done' || preferredJobSummary(results) !== undefined) return undefined
  return results?.output_rejection
}

function jobSummary(job: Job, results: TaskJobResult | undefined): JobSummaryText {
  // Prefer the harness's own narration (job.summary), then accepted structured
  // stage outputs, then a job-specific output rejection and state fallback.
  const preferred = preferredJobSummary(results)
  if (preferred !== undefined) return preferred

  const rejected = rejectedOutput(job, results)
  if (rejected) {
    const error = typeof rejected.error === 'string' ? rejected.error.trim() : ''
    const punctuation = error && !/[.!?]$/.test(error) ? '.' : ''
    return {
      text: error
        ? `Output rejected by the pipeline — ${error}${punctuation} Retrying with this feedback.`
        : 'Output rejected by the pipeline. Retrying with this feedback.',
    }
  }

  switch (job.state) {
    case 'pending':
      return { text: job.harness === 'external-mcp' ? 'Queued for an operator-owned agent over MCP.' : 'Queued.' }
    case 'running':
      return { text: 'In progress.' }
    case 'failed': {
      const failure = results?.failure_error
      if (failure && typeof failure.text === 'string' && failure.text) {
        return { text: failure.text, truncatedEventId: failure.truncated ? failure.event_id : undefined }
      }
      return { text: 'The job failed before producing a summary.' }
    }
    default:
      return { text: 'Completed.' }
  }
}

type NoteContent = Omit<Extract<TimelineEntry, { type: 'note' }>, 'type' | 'at' | 'key'>

const contextKindWords: Record<string, string> = { requirement: 'requirement', system_design: 'System Design' }

function contextDecisionTitle(act: string, payload: Record<string, unknown>) {
  const kind =
    typeof payload.target_kind === 'string' ? (contextKindWords[payload.target_kind] ?? payload.target_kind) : ''
  const target = typeof payload.target_id === 'string' ? payload.target_id.trim() : ''
  const subject = [kind, target].filter(Boolean).join(' ')
  return subject ? `${act}: ${subject}` : act
}

function humanizeKind(kind: string) {
  const text = kind.replaceAll(/[._]/g, ' ')
  return text.charAt(0).toUpperCase() + text.slice(1)
}

// The pull request an Implementation entry shows, and the head that attempt's
// own order submitted (component-web-task-surfaces, Work-order timeline
// entries). The pull request comes from the complete-ledger summary; the head
// never does, so an earlier attempt keeps its own.
export interface ImplementationDelivery {
  number: number
  url: string
  headSha: string
}

// Started implement jobs keyed by job ID, numbered from 1 in `started_at`
// order with ties broken by job ID, so a number never depends on response
// order or the bounded event page.
export function implementationAttempts(item: ActivityItem): Map<string, number> {
  const jobs = item.jobs
    .filter((job) => job.stage === 'implement' && job.started_at)
    .sort(
      (a, b) =>
        Date.parse(a.started_at ?? '') - Date.parse(b.started_at ?? '') || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0),
    )
  return new Map(jobs.map((job, index) => [job.id, index + 1]))
}

// The delivery line of each started implement job whose order recorded a head,
// while the task's pull request summary has a number and URL.
export function implementationDeliveries(item: ActivityItem): Map<string, ImplementationDelivery> {
  const deliveries = new Map<string, ImplementationDelivery>()
  const pr = item.summaries?.pull_request
  if (typeof pr?.number !== 'number' || typeof pr.url !== 'string' || !pr.url) return deliveries
  const orderByJob = new Map((item.work_orders ?? []).map((order) => [order.job_id, order]))
  for (const jobID of implementationAttempts(item).keys()) {
    const headSha = orderByJob.get(jobID)?.head_sha
    if (headSha) deliveries.set(jobID, { number: pr.number, url: pr.url, headSha })
  }
  return deliveries
}

// A `pull_request.opened` is absorbed when its job's Implementation entry shows
// the same pull request number, read from the payload or, when the server
// omitted it, from `payload_index`. Without a job ID or a number the event
// keeps its note and audit disclosure.
function isDeliveredPullRequestEvent(event: TaskEventView, deliveries: Map<string, ImplementationDelivery>): boolean {
  if (event.kind !== 'pull_request.opened' || !event.job_id) return false
  const delivery = deliveries.get(event.job_id)
  if (!delivery) return false
  const number = event.payload ? event.payload.number : event.payload_index?.number
  return typeof number === 'number' && number === delivery.number
}

// One selected event as a timeline note. Panel and delivery absorption are
// evaluated first, on the payload or its index; a selected event whose payload
// was omitted then renders as a note titled from its kind with an audit
// disclosure, so an omitted payload never removes a human row
// (component-web-task-surfaces).
function noteFor(
  event: TaskEventView,
  panels: PanelIndex,
  deliveries: Map<string, ImplementationDelivery>,
  assignee?: TaskAssignee,
  members: WorkspaceMembership[] = [],
): NoteContent | undefined {
  if (event.kind === 'review.round_completed' && isPanelReviewEvent(event, panels)) return undefined
  if (isDeliveredPullRequestEvent(event, deliveries)) return undefined
  if (event.payload_omitted && !event.payload) {
    if (!humanTimelineKinds.has(event.kind)) return undefined
    const title = noteContent(event.kind, event.payload_index ?? {}, panels, assignee, members)?.title
    return {
      title: title ?? humanizeKind(event.kind),
      detail: 'Details omitted from this page',
      omittedEventId: event.id,
    }
  }
  return noteContent(event.kind, event.payload ?? {}, panels, assignee, members)
}

function noteContent(
  kind: string,
  payload: Record<string, unknown>,
  panels: PanelIndex,
  assignee?: TaskAssignee,
  members: WorkspaceMembership[] = [],
): NoteContent | undefined {
  const noteKind = kind as NoteEventKind
  switch (noteKind) {
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
    case 'review.completed':
      return undefined
    // A panel round carries its own bounce count. A bounce outside any panel
    // round would otherwise leave no trace, so it reads as an entry with its
    // count; a merge-gate return is told by its intervention entry instead.
    case 'pipeline.bounced': {
      const round = typeof payload.review_round === 'number' ? payload.review_round : undefined
      if (round !== undefined && panels.rounds.has(round)) return undefined
      // A review bounce without a round (older payloads name no stage at all)
      // belongs to the per-order panels that predate rounds, which tell it.
      if (round === undefined && (payload.from ?? 'review') === 'review' && panels.panels.length > 0) return undefined
      if (payload.source === userRequestChangesReason) return undefined
      const to =
        typeof payload.to === 'string'
          ? (workOrderStageLabels[payload.to] ?? stageLabels[payload.to] ?? payload.to)
          : 'implementation'
      return {
        title: `Sent back to ${to.toLowerCase()}`,
        detail: typeof payload.count === 'number' ? `Bounce ${payload.count}` : undefined,
      }
    }
    case 'task.state_changed': {
      const command = typeof payload.command === 'string' ? payload.command : ''
      if (payload.to === 'parked')
        return {
          title: 'Parked',
          detail:
            command === 'triage.park'
              ? 'Triage parked the task for an operator decision'
              : command === 'dispatch.fail_final'
                ? 'Dispatch kept failing after its retries'
                : undefined,
          alarm: true,
        }
      if (payload.to === 'closed' && !closeCommandsWithEntries.has(command))
        return { title: command === 'task.start_over' ? 'Closed — started over as a new task' : 'Closed' }
      return undefined
    }
    case 'task.dependency_added':
      return {
        title: 'Waiting on another task',
        detail: typeof payload.depends_on_task_id === 'string' ? payload.depends_on_task_id : undefined,
      }
    // A task-context decision is the operator's audited act on a proposed
    // document, whichever surface recorded it (req-operator-next-action AC-2.2;
    // component-web-task-surfaces, Work-order timeline entries). An omitted
    // payload keeps no strings, so the title falls back to the act alone.
    case 'task.context_proposal_confirmed':
      return { title: contextDecisionTitle('Context confirmed', payload) }
    case 'task.context_proposal_dismissed':
      return { title: contextDecisionTitle('Context dismissed', payload) }
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
      const request = planRevisionRequestFrom({ payload })
      if (!request) return undefined
      return {
        title: `The agent asked to revise plan v${request.planVersion}`,
        detail: request.rationale,
      }
    }
    // The plan section shows each version and the approval intervention
    // renders the richer human "Approved" card. Retain the audit events in the
    // API without repeating them in the timeline.
    case 'spec.version_created':
    case 'spec.version_approved':
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
    // Queued and associated issue publications are bookkeeping; only the
    // retry and failure alarms read as timeline notes.
    case 'github_issue.publication_queued':
    case 'github_issue.publication_published':
    case 'github_issue.associated':
      return undefined
    case 'github_issue.publication_retry':
      return {
        title: 'GitHub issue publication retrying',
        detail: forgeFailureDetail(payload, 'last_error'),
        alarm: Boolean(payload.last_error),
      }
    case 'github_issue.publication_failed':
      return {
        title: 'GitHub issue publication failed',
        detail: forgeFailureDetail(payload, 'last_error'),
        alarm: true,
      }
    // A review publication retry is the forge client's own bookkeeping; the
    // failure alarm below reads when the retries run out.
    case 'review.publication_retry':
      return undefined
    case 'review.publication_failed':
      return {
        title: 'GitHub review publication failed',
        detail: forgeFailureDetail(payload, 'last_error'),
        alarm: true,
      }
    case 'github.review_redirected':
      return { title: 'Review comments on GitHub sent the task back for changes' }
    case 'review.round_completed':
      // A round a panel absorbs never reaches here: the panel card's
      // resolution banner is this event, rendered richer (see noteFor).
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
    default: {
      // Every listed kind has a case; an unlisted kind renders no note.
      const unlisted: never = noteKind
      void unlisted
      return undefined
    }
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
  const stage = workOrderStageLabels[order.stage] ?? stageLabels[order.stage] ?? order.stage
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

// A finished job's entry reads as its completion, so it sits where the job
// ended and follows the events the job produced; a running job sits where it
// started. A finished job without an end time keeps its start. A verify job
// renders as its own entry stamped with its start, so it stays there
// (req-operator-next-action AC-6.10; component-web-task-surfaces).
export function jobTimelineAt(job: Job): string | undefined {
  if (job.stage === 'verify' || job.state === 'running' || job.state === 'pending') return job.started_at
  return job.ended_at ?? job.started_at
}

function finishedJobEntry(entry: TimelineEntry): boolean {
  return entry.type === 'job' && entry.job.ended_at !== undefined && entry.at === entry.job.ended_at
}

// Attempt bookkeeping the server can report as an order's last agent activity
// (agentActivityLabel in internal/httpapi/server.go). Technical activity keeps
// the events; the human timeline tells no story with them.
const bookkeepingActivityLabels = new Set([
  'Work order claimed',
  'Claim lease renewed',
  'Attempt worktree checkpointed',
  'Usage telemetry reported',
  'Usage and rate-limit telemetry reported',
  'Transcript uploaded',
])

const finishedOrderStates = new Set<WorkOrder['state']>(['submitted', 'completed', 'cancelled'])

// A later order for the same stage and seat replaced this one.
function supersededOrder(order: WorkOrder, orders: WorkOrder[]): boolean {
  const entered = Date.parse(order.queue_entered_at)
  return orders.some(
    (other) =>
      other.id !== order.id &&
      other.stage === order.stage &&
      (other.review_seat ?? 0) === (order.review_seat ?? 0) &&
      Date.parse(other.queue_entered_at) > entered,
  )
}

// Only a live order's own reports read as agent activity. A finished or
// superseded order's job or order entry already tells its story, and a
// progress report already printed as the order's job entry summary is not
// repeated (component-web-task-surfaces).
function agentActivityNote(
  order: WorkOrder,
  orders: WorkOrder[],
  progressShown: Set<string>,
): Extract<TimelineEntry, { type: 'note' }> | undefined {
  const label = order.last_agent_activity_label
  if (!order.last_agent_activity_at || !label) return undefined
  if (bookkeepingActivityLabels.has(label) || finishedOrderStates.has(order.state)) return undefined
  if (supersededOrder(order, orders)) return undefined
  if (label.startsWith('Progress:') && progressShown.has(order.id)) return undefined
  return {
    type: 'note',
    at: order.last_agent_activity_at,
    key: `agent-activity-${order.id}`,
    title: `Agent activity — ${label}`,
    fullProgress: order.progress && label.startsWith('Progress:') ? order.progress : undefined,
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
  const attempts = implementationAttempts(item)
  const deliveries = implementationDeliveries(item)
  // Orders whose progress report is printed as a job entry's summary.
  const progressShown = new Set<string>()
  for (const job of item.jobs) {
    if (!job.started_at || panels.jobIds.has(job.id)) continue
    const order = orderByJob.get(job.id)
    const results = item.summaries?.job_results?.[job.id]
    const text = jobSummary(job, results)
    let summary = text.text
    if (order?.progress && genericSummaries.has(summary)) {
      summary = order.progress
      progressShown.add(order.id)
    }
    // BYOA jobs carry a placeholder tier; the work order knows the model
    // the operator's agent actually ran.
    const model = job.model_tier === 'operator-owned' && order?.model ? order.model : job.model_tier
    const tone = rejectedOutput(job, results) ? 'warning' : 'default'
    entries.push({
      type: 'job',
      at: jobTimelineAt(job) ?? job.started_at,
      job,
      summary,
      summaryEventId: summary === text.text ? text.truncatedEventId : undefined,
      model,
      tone,
      order,
      attempt: attempts.size > 1 ? attempts.get(job.id) : undefined,
      delivery: deliveries.get(job.id),
    })
  }
  const orders = item.work_orders ?? []
  for (const order of orders) {
    const activity = agentActivityNote(order, orders, progressShown)
    if (activity) entries.push(activity)
    if (panels.orderIds.has(order.id)) continue
    const entry = order.id === dependencyBlockedOrder?.id ? undefined : orderEntry(order, startedJobs.has(order.job_id))
    if (entry) entries.push(entry)
  }
  for (const event of item.events) {
    const retry = automaticRetryEvent(event)
    if (attemptEventKinds.has(event.kind) && !retry) continue
    const note = noteFor(event, panels, deliveries, item.task.assignee, members)
    if (!note) continue
    const shown = retry ? retryNote(note) : note
    entries.push({ type: 'note', at: event.at, key: `event-${event.id}`, ...shown })
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
  // At an equal instant a finished job follows what it produced, such as the
  // plan version it drafted; other ties keep their insertion order.
  return entries.sort(
    (a, b) =>
      new Date(a.at).getTime() - new Date(b.at).getTime() || Number(finishedJobEntry(a)) - Number(finishedJobEntry(b)),
  )
}

// Earlier human history rows, rendered through the same `noteFor`, panel
// absorption, and delivery absorption the timeline uses
// (component-web-task-surfaces, History pages).
export function historyNoteEntries(
  item: ActivityItem,
  events: TaskEventView[],
  members: WorkspaceMembership[] = [],
): Extract<TimelineEntry, { type: 'note' }>[] {
  const panels = buildReviewPanels(item)
  const deliveries = implementationDeliveries(item)
  const entries: Extract<TimelineEntry, { type: 'note' }>[] = []
  for (const event of events) {
    const retry = automaticRetryEvent(event)
    if (attemptEventKinds.has(event.kind) && !retry) continue
    const note = noteFor(event, panels, deliveries, item.task.assignee, members)
    if (!note) continue
    const shown = retry ? retryNote(note) : note
    entries.push({ type: 'note', at: event.at, key: `event-${event.id}`, ...shown })
  }
  return entries
}

export function taskRestartLinks(task: Pick<Task, 'supersedes' | 'superseded_by'>) {
  return { from: task.supersedes?.trim() || undefined, to: task.superseded_by?.trim() || undefined }
}

export function taskWasRestarted(task: Pick<Task, 'supersedes' | 'superseded_by'>) {
  const links = taskRestartLinks(task)
  return Boolean(links.from || links.to)
}

// Match cancellation's order selection, including recoverable stale/timed-out orders
// (req-task-lifecycle-and-queue AC-7.1; component-task-lifecycle).
export function restartOrderCount(item: ActivityItem): number | undefined {
  if (!item.work_orders?.length) return undefined
  return item.work_orders.filter((order) => order.state !== 'completed' && order.state !== 'cancelled').length
}

export function taskPendingRestartProposals(proposals: PendingProposal[], taskId: string) {
  return proposals.filter(
    (proposal) =>
      proposal.origin_type === 'task' &&
      proposal.origin_id === taskId &&
      (proposal.tier === 'requirement' || proposal.tier === 'system_design'),
  )
}

export function restartPullRequestURL(item: ActivityItem) {
  const closeState = item.task.pull_request_close_state?.trim() || item.task.pull_request_close?.state?.trim()
  if (closeState === 'closed' || closeState === 'skipped' || item.task.state === 'merged') return undefined
  return pullRequestURL(item.summaries)?.trim() || undefined
}

export function restartPullRequestOutcome(task: Task): string | undefined {
  const close = task.pull_request_close
  const state = task.pull_request_close_state?.trim() || close?.state?.trim()
  const error = close?.last_error?.trim() || close?.forge_error_category?.trim()
  const labels: Record<string, string> = {
    closed: 'Pull request closed.',
    queued: 'Pull request closure is queued.',
    retrying: 'Pull request closure will be retried.',
    failed: 'Pull request closure failed; the restart remains complete.',
    skipped: 'Pull request closure was skipped.',
  }
  const label = state ? labels[state] : undefined
  if (!label && !error) return undefined
  return [label, error].filter(Boolean).join(' ')
}

const verificationGroundLabels: Record<WorkOrderVerificationCheckpointGround['kind'], string> = {
  attempt_blocked: 'Blocked attempt',
  attempt_waiting: 'Waiting attempt',
  attempt_timed_out: 'Timed-out attempt',
  attempt_cancelled: 'Cancelled attempt',
  missing_grant: 'Missing grant',
  admission_refused: 'Admission refused',
  operation_unresolved: 'Unresolved external operation',
}

// The grantable form of a declared permission: kind:binding and any path.
export function verificationPermissionText(permission: WorkOrderVerificationCheckpointPermission): string {
  const binding = permission.target_binding ? `:${permission.target_binding}` : ''
  return `${permission.kind}${binding}${permission.path ? ` ${permission.path}` : ''}`
}

// One line per checkpoint ground. A ground without an attempt states that no
// attempt ran, so absent evidence is never presented as observed (component-verification-service).
export function verificationGroundText(ground: WorkOrderVerificationCheckpointGround): string {
  const subject =
    ground.subject?.kind === 'kit'
      ? `kit ${ground.subject.kit_id ?? ''}/${ground.subject.exercise_id ?? ''}`
      : ground.subject?.obligation_id
        ? `ordinary ${ground.subject.obligation_id}`
        : 'unattributed subject'
  const attempt = ground.attempt_id ? `attempt ${ground.attempt_id}` : 'no attempt ran; evidence missing'
  const source = ground.server_verified ? '' : ' (verifier-reported)'
  return `${verificationGroundLabels[ground.kind] ?? ground.kind}: ${subject} — ${attempt}${source}`
}
