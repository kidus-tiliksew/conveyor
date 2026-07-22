import type { GroupKey } from './contracts'
import type { ActivityItem, ActivitySummary, Intervention, Job, Task, TaskEvent, WorkOrder } from './types'

// Feed grouping (spec §13.3): the pipeline stage a task currently occupies.
// Human gates, approved tasks awaiting merge, and parked tasks collect under
// "Awaiting human"; only terminal states archive under "Completed".
export function groupForSummary(item: ActivitySummary): GroupKey {
  const { state } = item.task
  if (item.stalled?.needed && state !== 'merged' && state !== 'closed') return 'human'
  if (state === 'awaiting_human' || state === 'approved' || state === 'parked') return 'human'
  if (state === 'merged' || state === 'closed') return 'done'
  const stage = item.task.state === 'queued' ? (item.task.next_stage ?? item.latest_stage) : (item.latest_stage ?? item.task.next_stage)
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

// Provenance chip (spec §9): "github:<owner>/<repo>#<n>" links out; every
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

// Board-card gate chip: says what the gate is waiting for instead of a
// generic alarm, with tone to match ("Ready to merge" is good news).
export function gateBadge(item: ActivitySummary): { label: string; variant: 'attention' | 'positive' } | undefined {
  if (item.stalled?.needed) return { label: 'Stalled', variant: 'attention' }
  if (item.task.state === 'approved') return { label: 'Ready to merge', variant: 'positive' }
  if (!item.needs_attention) return undefined
  if (item.task.state === 'parked') return { label: 'Needs a route', variant: 'attention' }
  if (item.task.state === 'awaiting_human') return { label: 'Awaiting review', variant: 'attention' }
  return { label: 'Needs attention', variant: 'attention' }
}

export function reviewDiagnosticBadge(item: ActivitySummary): { label: string; variant: 'attention' | 'accent' } | undefined {
	const diagnostics = item.review_diagnostics ?? []
	if (diagnostics.some((diagnostic) => diagnostic.status === 'expired_without_verdict')) {
		return { label: 'Verdict claim expired', variant: 'attention' }
	}
	if (diagnostics.some((diagnostic) => diagnostic.status === 'claimed_without_verdict')) {
		return { label: 'Awaiting verdict submission', variant: 'accent' }
	}
	return undefined
}

// Why a task is at a human gate — the detail the "Needs attention" badge
// alone doesn't carry.
export function attentionReason(task: Task, events: TaskEvent[]): string {
  if (task.state === 'parked') return 'Parked by triage — needs a human route'
  // Walk back to the most recent incident, but stop at any human decision or
  // fresh dispatch — those supersede older incidents.
  for (let i = events.length - 1; i >= 0; i--) {
    const kind = events[i].kind
    if (kind === 'pipeline.bounce_limit') return 'Review check-in — the loop paused for a human look'
    if (kind === 'job.timeout') return 'Job hit its wall-clock timeout'
    if (kind === 'job.created' || kind.startsWith('intervention.')) break
  }
  if (task.state === 'approved') return 'Approved — awaiting merge'
  return 'At a human gate — review the work below'
}

export type TimelineEntry =
  | { type: 'job'; at: string; job: Job; summary: string; model: string; tone: 'default' | 'warning'; order?: WorkOrder }
  | { type: 'note'; at: string; key: string; title: string; detail?: string; failureDetail?: string; href?: string; alarm?: boolean }
  | { type: 'order'; at: string; key: string; title: string; detail?: string; tone: 'waiting' | 'active' | 'alarm' }
  | { type: 'intervention'; at: string; intervention: Intervention }
  | { type: 'panel'; at: string; key: string; round: number; seats: PanelSeat[]; resolution?: PanelResolution }

// One review round rendered as a single deliberating body (spec §21.12
// change 4): seats inside one card instead of N sibling job cards. A seat is
// a review work order plus whatever exists of its job and verdict.
export type PanelSeatStatus =
  | 'waiting'
  | 'deliberating'
  | 'verdict'
  | 'stale'
  | 'timed_out'
  | 'failed'
  | 'cancelled'

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
  return reviews.some((review) =>
    typeof (review as Record<string, unknown>)?.review_work_order_id === 'string' &&
    index.orderIds.has((review as Record<string, unknown>).review_work_order_id as string))
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
    if (event.kind === 'pipeline.bounced' && typeof payload.review_round === 'number' && typeof payload.count === 'number') {
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
      at: group.orders.reduce((min, order) => (order.queue_entered_at < min ? order.queue_entered_at : min), group.orders[0].queue_entered_at),
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
    if ((event.kind === 'triage.completed' || event.kind === 'review.completed') && typeof event.payload?.summary === 'string' && event.payload.summary) {
      const feedback = event.kind === 'review.completed' && typeof event.payload?.feedback === 'string'
        ? event.payload.feedback.trim()
        : ''
      return [event.payload.summary, feedback ? `Reviewer feedback: ${feedback}` : undefined].filter(Boolean).join('\n\n')
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

function noteFor(event: TaskEvent, panels: PanelIndex): Omit<Extract<TimelineEntry, { type: 'note' }>, 'type' | 'at' | 'key'> | undefined {
  const payload = event.payload ?? {}
  switch (event.kind) {
    case 'work_order.child_failed':
      return {
        title: payload.retry_suppressed === true ? 'Worker child failed — automatic retry suppressed' : 'Worker child failed — retry scheduled',
        detail: [payload.reason, payload.suppression_reason].filter((value): value is string => typeof value === 'string' && value.length > 0).join(' · ') || undefined,
        failureDetail: typeof payload.detail === 'string' && payload.detail.trim() ? payload.detail : undefined,
        alarm: true,
      }
    case 'work_order.expired':
      return { title: 'Worker claim expired — operator recovery required', alarm: true }
    case 'work_order.released':
      return { title: 'Worker attempt released', detail: typeof payload.reason === 'string' ? payload.reason : undefined, alarm: true }
    case 'work_order.recovered':
      return { title: 'Work order recovered by operator', detail: typeof payload.prior_outcome === 'string' ? `Prior outcome: ${payload.prior_outcome}` : undefined }
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
        title: 'Wall-clock timeout',
        detail: typeof payload.timeout === 'string' ? `Killed after ${payload.timeout}` : undefined,
        alarm: true,
      }
    case 'spec.version_created':
      return { title: `Spec v${typeof payload.version === 'number' ? payload.version : '?'} drafted` }
    case 'spec.version_approved':
      // The approval intervention renders the richer human "Approved" card.
      // Retain the audit event in the API without repeating it in the timeline.
      return undefined
    case 'github_issue.publication_queued':
      return { title: 'GitHub issue publication queued' }
    case 'github_issue.publication_retry':
      return { title: 'GitHub issue publication retrying', detail: typeof payload.last_error === 'string' ? payload.last_error : undefined, alarm: Boolean(payload.last_error) }
    case 'github_issue.publication_published':
    case 'github_issue.associated':
      return { title: 'GitHub issue associated', detail: typeof payload.issue_url === 'string' ? payload.issue_url : undefined, href: typeof payload.issue_url === 'string' ? payload.issue_url : undefined }
    case 'github_issue.publication_failed':
      return { title: 'GitHub issue publication failed', detail: typeof payload.last_error === 'string' ? payload.last_error : undefined, alarm: true }
    case 'github.review_redirected':
      return { title: 'GitHub review comments redirected the task (spec §9)' }
    case 'review.round_completed':
      // The panel card's resolution banner is this event, rendered richer.
      if (isPanelReviewEvent(payload, panels)) return undefined
      return { title: `Review panel: ${String(payload.verdict ?? 'completed')}`, detail: typeof payload.summary === 'string' ? payload.summary : undefined }
	case 'work_order.released':
		return {
			title: 'Work-order claim released',
			detail: typeof payload.reason === 'string' ? payload.reason : undefined,
			alarm: payload.reason === 'harness exited without terminal verdict submission',
		}
    case 'merge.requested':
      return { title: 'Pull request merge requested', detail: typeof payload.url === 'string' ? payload.url : undefined, href: typeof payload.url === 'string' ? payload.url : undefined }
    case 'merge.confirmed':
      return { title: 'Pull request merge confirmed by GitHub', detail: typeof payload.url === 'string' ? payload.url : undefined, href: typeof payload.url === 'string' ? payload.url : undefined }
    case 'merge.reconciled':
      return { title: 'Already-merged pull request reconciled', detail: typeof payload.url === 'string' ? payload.url : undefined, href: typeof payload.url === 'string' ? payload.url : undefined }
    case 'merge.failed':
      return { title: 'Merge needs operator action', detail: typeof payload.error === 'string' ? payload.error : undefined, alarm: true }
	case 'merge.blocked':
		return { title: 'Merge blocked — conflict fix required', detail: typeof payload.reason_code === 'string' ? `Reason: ${payload.reason_code}` : undefined, alarm: true }
	case 'merge.conflict_fix_dispatched':
		return { title: 'Conflict fix dispatched to implementation', detail: typeof payload.reason_code === 'string' ? `Reason: ${payload.reason_code}` : undefined }
	case 'approval.stale':
		return { title: 'Approval became stale after the PR head changed', detail: typeof payload.review_scope === 'string' ? `Refresh scope: ${payload.review_scope}` : undefined, alarm: true }
	case 'review.refresh_head_advanced':
		return { title: 'Refresh review retargeted to the newly pushed head', detail: typeof payload.new_head === 'string' ? `Head: ${payload.new_head.slice(0, 8)}` : undefined }
	case 'review.refresh_round_created':
		return { title: `Refresh review round ${String(payload.review_round ?? '')} started`, detail: typeof payload.review_scope === 'string' ? `Scope: ${payload.review_scope}` : undefined }
	case 'review.refresh_skipped':
		return { title: 'Clean head update re-armed without refresh review', detail: typeof payload.reason_code === 'string' ? `Reason: ${payload.reason_code}` : undefined }
    case 'dispatch.failed':
      return {
        title: 'Dispatch failed',
        detail: typeof payload.error === 'string' ? payload.error : undefined,
        alarm: true,
      }
    case 'job.log_stream_degraded':
      return { title: 'Log stream degraded — falling back to artifacts' }
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

// Work orders fold into the timeline (spec §21.4) instead of a separate
// block: narration and attribution enrich the matching stage entry, and only
// states a job entry cannot carry — waiting for an agent, stale, timed out —
// become entries of their own.
function orderEntry(order: WorkOrder, hasJobEntry: boolean): Extract<TimelineEntry, { type: 'order' }> | undefined {
  const stage = order.stage === 'spec' ? 'Spec' : order.stage === 'implement' ? 'Implementation' : 'Review'
  const base = { type: 'order' as const, at: order.queue_entered_at, key: `order-${order.id}` }
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

// The costed event timeline (spec §13.3): one entry per stage execution,
// interleaved with the notable pipeline events and every human/agent
// decision, in wall-clock order. This is the audit log rendered as a story.
export function buildTimeline(item: ActivityItem): TimelineEntry[] {
	const entries: TimelineEntry[] = []
	const panels = buildReviewPanels(item)
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
    if (panels.orderIds.has(order.id)) continue
    const entry = orderEntry(order, startedJobs.has(order.job_id))
    if (entry) entries.push(entry)
  }
  for (const event of item.events) {
    const note = noteFor(event, panels)
    if (note) entries.push({ type: 'note', at: event.at, key: `event-${event.id}`, ...note })
  }
	for (const diagnostic of item.review_diagnostics ?? []) {
		// Active claims already appear as deliberating seats in the review panel.
		// Keep the diagnostic in the API, but do not repeat it in the timeline.
		if (diagnostic.status === 'claimed_without_verdict') continue
		const seat = diagnostic.review_seat ? `seat ${diagnostic.review_seat}` : 'review seat'
		entries.push({
			type: 'note',
			at: diagnostic.status === 'expired_without_verdict'
				? (diagnostic.lease_expires_at ?? diagnostic.claimed_at ?? item.task.created_at)
				: (diagnostic.claimed_at ?? item.task.created_at),
			key: `review-diagnostic-${diagnostic.status}-${diagnostic.work_order_id}`,
			title: diagnostic.status === 'expired_without_verdict'
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
    if (roundActor && (panels.rounds.has(Number(roundActor[1])) || (Number(roundActor[1]) === 0 && panels.orderIds.size > 0))) continue
    entries.push({ type: 'intervention', at: intervention.at, intervention })
  }
  return entries.sort((a, b) => new Date(a.at).getTime() - new Date(b.at).getTime())
}
