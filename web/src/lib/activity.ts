import type { GroupKey } from './contracts'
import type { ActivityItem, ActivitySummary, Intervention, Job, Task, TaskEvent } from './types'

// Feed grouping (spec §13.3): the pipeline stage a task currently occupies.
// Human gates and parked tasks collect under "Awaiting human"; terminal
// states archive under "Completed".
export function groupForSummary(item: ActivitySummary): GroupKey {
  const { state } = item.task
  if (state === 'awaiting_human' || state === 'parked') return 'human'
  if (state === 'approved' || state === 'merged' || state === 'closed') return 'done'
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

// Why a task is at a human gate — the detail the "Needs attention" badge
// alone doesn't carry.
export function attentionReason(task: Task, events: TaskEvent[]): string {
  if (task.state === 'parked') return 'Parked by triage — needs a human route'
  // Walk back to the most recent incident, but stop at any human decision or
  // fresh dispatch — those supersede older incidents.
  for (let i = events.length - 1; i >= 0; i--) {
    const kind = events[i].kind
    if (kind === 'pipeline.bounce_limit') return 'Bounce limit reached — review loop needs a human'
    if (kind === 'job.timeout') return 'Job hit its wall-clock timeout'
    if (kind === 'job.created' || kind.startsWith('intervention.')) break
  }
  if (task.state === 'approved') return 'Approved — awaiting merge'
  return 'At a human gate — review the work below'
}

export type TimelineEntry =
  | { type: 'job'; at: string; job: Job; summary: string }
  | { type: 'note'; at: string; key: string; title: string; detail?: string; href?: string; alarm?: boolean }
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
      return event.payload.summary
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
      return { title: 'Bounce limit reached — parked at the human gate', alarm: true }
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
    case 'github.review_redirected':
      return { title: 'GitHub review comments redirected the task (spec §9)' }
    case 'review.completed':
      return { title: `Independent review: ${String(payload.verdict ?? 'completed')}`, detail: `session ${String(payload.reviewer_session ?? 'unknown')} · model ${String(payload.reviewer_model ?? 'unknown')} · same model ${String(payload.same_model_as_implementer ?? 'unknown')}` }
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

// The costed event timeline (spec §13.3): one entry per stage execution,
// interleaved with the notable pipeline events and every human/agent
// decision, in wall-clock order. This is the audit log rendered as a story.
export function buildTimeline(item: ActivityItem): TimelineEntry[] {
	const entries: TimelineEntry[] = []
	for (const job of item.jobs) {
		if (!job.started_at) continue
		entries.push({ type: 'job', at: job.started_at, job, summary: jobSummary(job, item.events) })
  }
  for (const event of item.events) {
    const note = noteFor(event)
    if (note) entries.push({ type: 'note', at: event.at, key: `event-${event.id}`, ...note })
  }
  for (const intervention of item.interventions) {
    entries.push({ type: 'intervention', at: intervention.at, intervention })
  }
  return entries.sort((a, b) => new Date(a.at).getTime() - new Date(b.at).getTime())
}
