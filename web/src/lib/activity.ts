import type { GroupKey } from './contracts'
import type { ActivityItem, ActivitySummary, Intervention, Job, Task, TaskEvent, WorkOrder } from './types'

// Feed grouping (spec §13.3): the pipeline stage a task currently occupies.
// Human gates, approved tasks awaiting merge, and parked tasks collect under
// "Awaiting human"; only terminal states archive under "Completed".
export function groupForSummary(item: ActivitySummary): GroupKey {
  const { state } = item.task
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
  | { type: 'job'; at: string; job: Job; summary: string; model: string; order?: WorkOrder }
  | { type: 'note'; at: string; key: string; title: string; detail?: string; href?: string; alarm?: boolean }
  | { type: 'order'; at: string; key: string; title: string; detail?: string; tone: 'waiting' | 'active' | 'alarm' }
  | { type: 'intervention'; at: string; intervention: Intervention }

function jobSummary(job: Job, events: TaskEvent[]): string {
  // Prefer the harness's own narration (job.summary), then the structured
  // stage outputs, then a state-derived fallback.
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
  switch (job.state) {
    case 'pending':
      return job.harness === 'external-mcp' ? 'Queued for an operator-owned agent over MCP.' : 'Queued.'
    case 'running':
      return 'In progress.'
    case 'failed':
      return 'The job failed before producing a summary.'
    default:
      return 'Completed.'
  }
}

function noteFor(event: TaskEvent): Omit<Extract<TimelineEntry, { type: 'note' }>, 'type' | 'at' | 'key'> | undefined {
  const payload = event.payload ?? {}
  switch (event.kind) {
    case 'pull_request.opened':
      return {
        title: 'Pull request opened',
        detail: typeof payload.url === 'string' ? payload.url : undefined,
        href: typeof payload.url === 'string' ? payload.url : undefined,
      }
    case 'pipeline.bounced': {
      const count = typeof payload.count === 'number' ? ` (bounce ${payload.count})` : ''
      const reason = typeof payload.reason_code === 'string' ? payload.reason_code : ''
      const feedback = typeof payload.feedback === 'string' ? payload.feedback : ''
      return {
        title: `Bounced back to implement${count}`,
        detail: [reason, feedback].filter(Boolean).join(' — '),
      }
    }
    case 'pipeline.bounce_limit':
      return { title: 'Review check-in — paused after the configured rounds', alarm: true }
    case 'job.timeout':
      return {
        title: 'Wall-clock timeout',
        detail: typeof payload.timeout === 'string' ? `Killed after ${payload.timeout}` : undefined,
        alarm: true,
      }
    case 'spec.version_created':
      return { title: `Spec v${typeof payload.version === 'number' ? payload.version : '?'} drafted` }
    case 'spec.version_approved':
      return { title: `Spec v${typeof payload.version === 'number' ? payload.version : '?'} approved` }
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
    case 'review.completed': {
      // Session ids and boolean flags are audit payload, not narrative; only
      // the reviewer model and a same-model caveat matter to a reader.
      const parts = [
        typeof payload.review_seat === 'number' && payload.review_seat > 0 ? `seat ${payload.review_seat}` : undefined,
        typeof payload.reviewer_model === 'string' ? payload.reviewer_model : undefined,
        typeof payload.required_effort === 'string' && payload.required_effort.trim()
          ? `effort ${payload.required_effort.trim()}`
          : undefined,
        typeof payload.model_enforcement === 'string' ? payload.model_enforcement : undefined,
        payload.same_model_as_implementer === true ? 'same model as the implementer' : undefined,
        typeof payload.feedback === 'string' && payload.feedback.trim() ? `feedback: ${payload.feedback.trim()}` : undefined,
      ].filter(Boolean)
      return { title: `Independent review: ${String(payload.verdict ?? 'completed')}`, detail: parts.join(' · ') || undefined }
    }
    case 'review.round_completed':
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
  const stage = order.stage === 'implement' ? 'Implementation' : 'Review'
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
	const orderByJob = new Map((item.work_orders ?? []).map((order) => [order.job_id, order]))
	const startedJobs = new Set(item.jobs.filter((job) => job.started_at).map((job) => job.id))
	for (const job of item.jobs) {
		if (!job.started_at) continue
		const order = orderByJob.get(job.id)
		let summary = jobSummary(job, item.events)
		if (order?.progress && genericSummaries.has(summary)) summary = order.progress
		// BYOA jobs carry a placeholder tier; the work order knows the model
		// the operator's agent actually ran.
		const model = job.model_tier === 'operator-owned' && order?.model ? order.model : job.model_tier
		entries.push({ type: 'job', at: job.started_at, job, summary, model, order })
  }
  for (const order of item.work_orders ?? []) {
    const entry = orderEntry(order, startedJobs.has(order.job_id))
    if (entry) entries.push(entry)
  }
  for (const event of item.events) {
    const note = noteFor(event)
    if (note) entries.push({ type: 'note', at: event.at, key: `event-${event.id}`, ...note })
  }
	for (const diagnostic of item.review_diagnostics ?? []) {
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
    entries.push({ type: 'intervention', at: intervention.at, intervention })
  }
  return entries.sort((a, b) => new Date(a.at).getTime() - new Date(b.at).getTime())
}
