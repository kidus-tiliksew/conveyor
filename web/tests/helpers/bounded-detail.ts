import { test as base, type Page, type Route } from '@playwright/test'
import type {
  ActivityItem,
  Job,
  TaskEventPayloadIndex,
  TaskEventView,
  TaskHistoryPage,
  TaskHistorySelection,
  TaskJobResult,
  TaskReviewResult,
  TaskReviewRound,
  TaskSummaries,
  WorkOrder,
  WorkOrderTranscriptCapture,
  WorkOrderTranscriptCaptureContent,
} from '../../src/lib/types'

// A fake server for the bounded task-detail view and task history pages
// (component-http-api, Bounded detail view and Task history pages). Specs keep
// writing complete-history activity fixtures, the shape `GET /activity` served
// before the bounded view; this module turns each into what the server sends
// for `?view=bounded`: the newest human-timeline events in the event wire form,
// `event_page`, and `summaries` folded from the complete ledger by (`at`, `id`).
// It also pages the same ledger for `GET /history` with opaque cursors. It
// stands in for internal/httpapi; the dashboard itself derives none of this.

// The server's human-timeline kinds (internal/httpapi/task_detail_summaries.go).
// The dashboard's `humanTimelineEventKinds` must equal this list.
export const serverHumanTimelineEventKinds = [
  'task.assignee.set',
  'task.assignee.cleared',
  'pull_request.opened',
  'pipeline.bounce_limit',
  'job.timeout',
  'work_order.plan_revision_requested',
  'blueprint.materialized',
  'blueprint.closed',
  'task.dependency_unsatisfiable',
  'task.dependency_removed',
  'github_issue.publication_retry',
  'github_issue.publication_failed',
  'review.publication_failed',
  'github.review_redirected',
  'review.round_completed',
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
  'pipeline.bounced',
  'task.state_changed',
  'task.dependency_added',
  'task.context_proposal_confirmed',
  'task.context_proposal_dismissed',
  'work_order.child_failed',
  'work_order.stalled',
] as const

const humanKinds = new Set<string>(serverHumanTimelineEventKinds)

export const payloadOmissionBytes = 8_192
export const eventViewMaxBytes = 16_384
export const eventSectionMaxBytes = 65_536
export const summaryTextMaxBytes = 16_384
export const boundedEventLimit = 100
export const historyPageLimit = 50

// One stored event as a complete-history fixture writes it. IDs may be numbers
// (older fixtures) or decimal strings (IDs above 2^53).
export interface CompleteEvent {
  id: number | string
  task_id?: string
  job_id?: string
  kind: string
  actor_id?: string
  actor_role?: string
  payload: Record<string, unknown> | null
  at: string
  audit_available?: boolean
}

export type CompleteActivity = Omit<ActivityItem, 'events' | 'event_page' | 'summaries'> & {
  events: CompleteEvent[]
}

const bytes = (value: unknown) => Buffer.byteLength(JSON.stringify(value))

// The server's (at, id) order. The fake server may compare IDs numerically;
// the dashboard never does.
function atMicros(at: string): bigint {
  const match = /^(.*?)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$/.exec(at)
  if (!match) return BigInt(Date.parse(at)) * 1000n
  const seconds = BigInt(Date.parse(`${match[1]}${match[3]}`))
  const fraction = (match[2] ?? '').padEnd(6, '0').slice(0, 6)
  return seconds * 1000n + BigInt(fraction)
}

interface Ordered {
  event: CompleteEvent
  at: bigint
  id: bigint
}

function compareOrdered(a: Ordered, b: Ordered) {
  if (a.at !== b.at) return a.at < b.at ? -1 : 1
  if (a.id !== b.id) return a.id < b.id ? -1 : 1
  return 0
}

const orderedCache = new WeakMap<CompleteEvent[], Ordered[]>()

// Events in ascending (at, id) order; cached per fixture array.
export function orderedEvents(events: CompleteEvent[]): Ordered[] {
  const cached = orderedCache.get(events)
  if (cached) return cached
  const ordered = events.map((event) => ({ event, at: atMicros(event.at), id: BigInt(event.id) })).sort(compareOrdered)
  orderedCache.set(events, ordered)
  return ordered
}

function payloadIndex(event: CompleteEvent, payload: Record<string, unknown>) {
  const index: TaskEventPayloadIndex = {}
  let truncated = false
  let fields = 0
  for (const [key, value] of Object.entries(payload)) {
    if (typeof value !== 'number' && typeof value !== 'boolean') continue
    if (fields === 16) {
      truncated = true
      break
    }
    index[key] = value
    fields++
  }
  if (event.kind === 'review.round_completed') {
    const ids: string[] = []
    for (const review of Array.isArray(payload.reviews) ? payload.reviews : []) {
      const id = (review as Record<string, unknown> | null)?.review_work_order_id
      if (typeof id !== 'string') continue
      if (ids.length === 64 || Buffer.byteLength(id) > 128) {
        truncated = true
        continue
      }
      ids.push(id)
    }
    if (ids.length > 0) index.review_work_order_ids = ids
  }
  return { index, truncated }
}

// The event wire form: decimal string IDs, payloads above 8,192 bytes omitted
// with their index, and an event above 16,384 bytes in its minimal form.
export function presentEvent(event: CompleteEvent): TaskEventView {
  const view: TaskEventView = {
    id: String(event.id),
    ...(event.task_id === undefined ? {} : { task_id: event.task_id }),
    ...(event.job_id ? { job_id: event.job_id } : {}),
    kind: event.kind,
    ...(event.actor_id === undefined ? {} : { actor_id: event.actor_id }),
    ...(event.actor_role === undefined ? {} : { actor_role: event.actor_role as TaskEventView['actor_role'] }),
    payload: event.payload,
    at: event.at,
    ...(event.audit_available ? { audit_available: true } : {}),
  }
  const stored = event.payload === null ? 0 : bytes(event.payload)
  if (stored > payloadOmissionBytes && event.payload) {
    const { index, truncated } = payloadIndex(event, event.payload)
    view.payload = null
    view.payload_omitted = true
    view.payload_bytes = stored
    view.audit_available = true
    if (Object.keys(index).length > 0) view.payload_index = index
    if (truncated) view.payload_index_truncated = true
  }
  if (bytes(view) > eventViewMaxBytes) {
    return { id: view.id, at: view.at, kind: view.kind, payload: null, payload_omitted: true, audit_available: true }
  }
  return view
}

// ---- History cursors -------------------------------------------------------

interface CursorState {
  v: 1
  task: string
  selection: TaskHistorySelection
  limit: number
  boundary: { max_id: string; count: number }
  seek: { at: string; id: string }
  direction: 'older' | 'newer'
  issued: number
}

export function encodeHistoryCursor(state: CursorState) {
  const body = Buffer.from(JSON.stringify(state)).toString('base64url')
  return `${body}.${Buffer.from(`tag:${body.length}:${state.task}`).toString('base64url')}`
}

export function decodeHistoryCursor(cursor: string): CursorState | undefined {
  const [body, tag] = cursor.split('.')
  if (!body || !tag) return undefined
  try {
    const state = JSON.parse(Buffer.from(body, 'base64url').toString()) as CursorState
    if (Buffer.from(tag, 'base64url').toString() !== `tag:${body.length}:${state.task}`) return undefined
    return state
  } catch {
    return undefined
  }
}

export class HistoryRefusal extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
  ) {
    super(code)
  }
}

const selectionCache = new WeakMap<CompleteEvent[], Map<TaskHistorySelection, Ordered[]>>()

function selected(events: CompleteEvent[], selection: TaskHistorySelection): Ordered[] {
  const bySelection = selectionCache.get(events) ?? new Map<TaskHistorySelection, Ordered[]>()
  selectionCache.set(events, bySelection)
  const cached = bySelection.get(selection)
  if (cached) return cached
  const ordered = orderedEvents(events)
  const result = selection === 'human' ? ordered.filter((entry) => humanKinds.has(entry.event.kind)) : ordered
  bySelection.set(selection, result)
  return result
}

function boundaryOf(entries: Ordered[]) {
  let max = 0n
  for (const entry of entries) if (entry.id > max) max = entry.id
  return { max_id: String(max), count: entries.length }
}

// First index whose (at, id) is strictly after `seek`.
function upperBound(entries: Ordered[], seek: { at: bigint; id: bigint }) {
  let low = 0
  let high = entries.length
  while (low < high) {
    const middle = (low + high) >> 1
    if (compareOrdered(entries[middle], { event: entries[middle].event, ...seek }) <= 0) low = middle + 1
    else high = middle
  }
  return low
}

// First index whose (at, id) is at or after `seek`.
function lowerBound(entries: Ordered[], seek: { at: bigint; id: bigint }) {
  let low = 0
  let high = entries.length
  while (low < high) {
    const middle = (low + high) >> 1
    if (compareOrdered(entries[middle], { event: entries[middle].event, ...seek }) < 0) low = middle + 1
    else high = middle
  }
  return low
}

// One history page over a complete ledger, as the server pages it: a first
// page captures the boundary and returns the newest events; `older_cursor`
// and `newer_cursor` seek strictly before or after the page; the envelope
// stays within 65,536 bytes by dropping events farthest from the cursor.
export function historyPage(
  events: CompleteEvent[],
  request: { taskId: string; selection?: string | null; cursor?: string | null; limit?: string | number | null },
): TaskHistoryPage {
  const selection = (request.selection || 'technical') as TaskHistorySelection
  if (selection !== 'technical' && selection !== 'human') throw new HistoryRefusal(400, 'invalid_history_request')
  let limit = request.limit === undefined || request.limit === null || request.limit === '' ? 0 : Number(request.limit)
  if (limit !== 0 && (!Number.isInteger(limit) || limit < 1 || limit > historyPageLimit))
    throw new HistoryRefusal(400, 'invalid_history_request')
  const all = selected(events, selection)
  let state: CursorState | undefined
  if (request.cursor) {
    state = decodeHistoryCursor(request.cursor)
    if (!state || state.task !== request.taskId || state.selection !== selection)
      throw new HistoryRefusal(400, 'history_cursor_invalid')
    if (limit !== 0 && limit !== state.limit) throw new HistoryRefusal(400, 'invalid_history_request')
    limit = state.limit
  }
  if (limit === 0) limit = historyPageLimit
  const boundary = state?.boundary ?? boundaryOf(all)
  const maxID = BigInt(boundary.max_id)
  const inBoundary = all.length > 0 && all[all.length - 1].id <= maxID ? all : all.filter((entry) => entry.id <= maxID)
  let page: Ordered[]
  if (!state) {
    page = inBoundary.slice(Math.max(0, inBoundary.length - limit))
  } else {
    const seek = { at: atMicros(state.seek.at), id: BigInt(state.seek.id) }
    if (state.direction === 'older') {
      const end = lowerBound(inBoundary, seek)
      page = inBoundary.slice(Math.max(0, end - limit), end)
    } else {
      const start = upperBound(inBoundary, seek)
      page = inBoundary.slice(start, start + limit)
    }
  }
  const issued = state?.issued ?? 0
  const build = (entries: Ordered[]): TaskHistoryPage => {
    const first = entries[0]
    const last = entries[entries.length - 1]
    const firstIndex = first ? inBoundary.indexOf(first) : -1
    const lastIndex = last ? inBoundary.indexOf(last) : -1
    const hasOlder = firstIndex > 0
    const hasNewer = lastIndex >= 0 && lastIndex < inBoundary.length - 1
    const cursor = (entry: Ordered, direction: 'older' | 'newer') =>
      encodeHistoryCursor({
        v: 1,
        task: request.taskId,
        selection,
        limit,
        boundary,
        seek: { at: entry.event.at, id: String(entry.event.id) },
        direction,
        issued,
      })
    return {
      selection,
      limit,
      events: entries.map((entry) => presentEvent(entry.event)),
      boundary,
      has_older: hasOlder,
      has_newer: hasNewer,
      ...(hasOlder && first ? { older_cursor: cursor(first, 'older') } : {}),
      ...(hasNewer && last ? { newer_cursor: cursor(last, 'newer') } : {}),
    }
  }
  let result = build(page)
  while (page.length > 1 && bytes(result) > eventSectionMaxBytes) {
    page = state?.direction === 'newer' ? page.slice(0, -1) : page.slice(1)
    result = build(page)
  }
  return result
}

// ---- Summaries -------------------------------------------------------------

function cut(text: string): { text: string; truncated?: true } {
  if (Buffer.byteLength(text) <= summaryTextMaxBytes) return { text }
  let end = summaryTextMaxBytes
  const buffer = Buffer.from(text)
  // Step back to a rune boundary.
  while (end > 0 && (buffer[end] & 0xc0) === 0x80) end--
  return { text: buffer.subarray(0, end).toString(), truncated: true }
}

const outputInvalidKinds = new Set(['triage.output_invalid', 'spec.output_invalid', 'review.output_invalid'])

function planRevision(payload: Record<string, unknown>) {
  const rationale = typeof payload.rationale === 'string' ? payload.rationale.trim() : ''
  const version =
    typeof payload.plan_version === 'number'
      ? payload.plan_version
      : Number.parseInt(String(payload.plan_version ?? ''), 10)
  if (!rationale || !Number.isInteger(version) || version <= 0) return undefined
  return { rationale, plan_version: version }
}

function triageFailure(item: CompleteActivity, ordered: Ordered[]): TaskSummaries['triage_failure'] {
  const jobs = [...(item.jobs ?? [])].sort(
    (a, b) => (Date.parse(a.started_at ?? a.ended_at ?? '') || 0) - (Date.parse(b.started_at ?? b.ended_at ?? '') || 0),
  )
  const latest = jobs.at(-1)
  if (!latest || latest.stage !== 'triage') return undefined
  let failure: Ordered | undefined
  for (const entry of ordered)
    if (entry.event.job_id === latest.id && entry.event.kind === 'job.failed') failure = entry
  if (latest.state !== 'failed' && !failure) return undefined
  const failureTime = failure?.event.at ?? latest.ended_at ?? latest.started_at ?? ''
  const superseding = (kind: string) =>
    kind.startsWith('intervention.') ||
    kind === 'pipeline.dispatched' ||
    kind === 'job.created' ||
    kind === 'work_order.claimed'
  const superseded =
    ordered.some(
      (entry) =>
        superseding(entry.event.kind) &&
        (failure ? compareOrdered(entry, failure) > 0 : Date.parse(entry.event.at) > Date.parse(failureTime)),
    ) ||
    (item.work_orders ?? []).some(
      (order: WorkOrder) => Date.parse(order.created_at ?? order.queue_entered_at) > Date.parse(failureTime),
    )
  const payload = failure?.event.payload ?? {}
  const diagnostic =
    payload.diagnostic && typeof payload.diagnostic === 'object' ? (payload.diagnostic as Record<string, unknown>) : {}
  return {
    job_id: latest.id,
    ...(failure ? { event_id: String(failure.event.id) } : {}),
    error: typeof payload.error === 'string' ? payload.error : '',
    diagnostic: {
      ...(typeof diagnostic.phase === 'string' ? { phase: diagnostic.phase } : {}),
      ...(typeof diagnostic.provider_code === 'string' ? { provider_code: diagnostic.provider_code } : {}),
    },
    superseded,
    at: failureTime,
  }
}

// The park reason the server folds from the latest triage.completed: present
// only when it parked the task, with the non-empty brief questions and risks.
function parkReason(payload: Record<string, unknown>, id: string, at: string): TaskSummaries['park_reason'] {
  if (payload.route !== 'parked') return undefined
  let truncated = false
  const brief = (payload.brief ?? {}) as Record<string, unknown>
  const list = (key: string) => {
    const out: string[] = []
    for (const entry of Array.isArray(brief[key]) ? brief[key] : []) {
      const value = typeof entry === 'string' ? entry.trim() : ''
      if (!value) continue
      if (out.length === 20) {
        truncated = true
        break
      }
      const bounded = cut(value)
      truncated ||= Boolean(bounded.truncated)
      out.push(bounded.text)
    }
    return out
  }
  const summary = cut(typeof payload.summary === 'string' ? payload.summary.trim() : '')
  truncated ||= Boolean(summary.truncated)
  const questions = list('questions')
  const risks = list('risks')
  return { summary: summary.text, questions, risks, event_id: id, at, ...(truncated ? { truncated } : {}) }
}

// The summaries the server folds from the complete ledger in (at, id) order.
export function taskSummaries(item: CompleteActivity): TaskSummaries {
  const ordered = orderedEvents(item.events ?? [])
  const summaries: TaskSummaries = { run_implementation: false }
  const jobResults: Record<string, TaskJobResult> = {}
  const byOrder: Record<string, TaskReviewResult> = {}
  const byJob: Record<string, TaskReviewResult> = {}
  const roundsByRound: Record<string, TaskReviewRound> = {}
  const roundsByOrder: Record<string, TaskReviewRound> = {}
  const bounces: Record<string, number> = {}
  const result = (jobID: string) => {
    jobResults[jobID] ??= {}
    return jobResults[jobID]
  }
  for (const { event } of ordered) {
    const payload = event.payload ?? {}
    const id = String(event.id)
    switch (event.kind) {
      case 'task.created':
        if (!summaries.creator)
          summaries.creator = { actor_id: event.actor_id ?? '', actor_role: event.actor_role ?? '' }
        break
      case 'pull_request.opened':
        summaries.pull_request = {
          ...(typeof payload.url === 'string' ? { url: payload.url } : {}),
          ...(typeof payload.number === 'number' ? { number: payload.number } : {}),
          ...(typeof payload.repository === 'string' ? { repository: payload.repository } : {}),
          ...(typeof payload.head_sha === 'string' ? { head_sha: payload.head_sha } : {}),
          ...(typeof payload.base_sha === 'string' ? { base_sha: payload.base_sha } : {}),
          event_id: id,
          at: event.at,
        }
        break
      case 'review.round_completed': {
        const verdict = payload.verdict === 'changes_requested' ? 'changes_requested' : 'approve'
        summaries.review_round = {
          verdict,
          ...(typeof payload.summary === 'string' ? { summary: payload.summary } : {}),
          review_count: Array.isArray(payload.reviews) ? payload.reviews.length : 0,
          event_id: id,
          at: event.at,
        }
        const round: TaskReviewRound = {
          event_id: id,
          verdict,
          ...(typeof payload.summary === 'string' ? { summary: payload.summary } : {}),
          at: event.at,
        }
        if (typeof payload.review_round === 'number' && payload.review_round > 0)
          roundsByRound[String(payload.review_round)] = round
        for (const review of Array.isArray(payload.reviews) ? payload.reviews : []) {
          const orderID = (review as Record<string, unknown> | null)?.review_work_order_id
          if (typeof orderID === 'string') roundsByOrder[orderID] = round
        }
        break
      }
      case 'pipeline.bounced':
        if (payload.source === 'user-request-changes')
          summaries.returned_for_changes = {
            feedback: typeof payload.feedback === 'string' ? payload.feedback.trim() : '',
            at: event.at,
          }
        if (typeof payload.review_round === 'number' && typeof payload.count === 'number')
          bounces[String(payload.review_round)] = payload.count
        break
      case 'work_order.claimed':
        if (payload.stage === 'implement') {
          summaries.returned_for_changes = undefined
          summaries.run_implementation = typeof payload.claimant_id === 'string' && /^run:.+/.test(payload.claimant_id)
        }
        break
      case 'work_order.plan_revision_requested': {
        const request = planRevision(payload)
        summaries.plan_revision_request = request ? { ...request, event_id: id, at: event.at } : undefined
        break
      }
      case 'triage.completed':
        summaries.park_reason = parkReason(payload, id, event.at)
        break
      case 'dispatch.failed':
        if (typeof payload.error === 'string' && payload.error)
          summaries.dispatch_failure = {
            ...(({ text, truncated }) => ({ error: text, ...(truncated ? { truncated } : {}) }))(cut(payload.error)),
            ...(typeof payload.attempt === 'number' ? { attempt: payload.attempt } : {}),
            ...(typeof payload.max_attempts === 'number' ? { max_attempts: payload.max_attempts } : {}),
            event_id: id,
            at: event.at,
          }
        break
      case 'review.completed': {
        summaries.latest_review_completed_event_id = id
        const summary = cut(typeof payload.summary === 'string' ? payload.summary : '')
        const feedback = cut(typeof payload.feedback === 'string' ? payload.feedback : '')
        const review: TaskReviewResult = {
          verdict: payload.verdict === 'changes_requested' ? 'changes_requested' : 'approve',
          summary: summary.text,
          feedback: feedback.text,
          at: event.at,
          event_id: id,
          ...(summary.truncated || feedback.truncated ? { truncated: true } : {}),
        }
        if (typeof payload.review_work_order_id === 'string') byOrder[payload.review_work_order_id] = review
        if (event.job_id) byJob[event.job_id] = review
        break
      }
    }
    if (!event.job_id) continue
    if (event.kind === 'job.summary' && typeof payload.summary === 'string')
      result(event.job_id).summary = { ...cut(payload.summary), event_id: id }
    if (
      (event.kind === 'triage.completed' || event.kind === 'review.completed') &&
      typeof payload.summary === 'string' &&
      payload.summary
    ) {
      const feedback =
        event.kind === 'review.completed' && typeof payload.feedback === 'string' ? payload.feedback.trim() : ''
      result(event.job_id).stage_summary = {
        ...cut(payload.summary),
        ...(feedback ? { review_feedback: feedback } : {}),
        event_id: id,
      }
    }
    if (outputInvalidKinds.has(event.kind))
      result(event.job_id).output_rejection = {
        event_id: id,
        error: typeof payload.error === 'string' ? payload.error.trim() : '',
      }
    if (event.kind === 'job.failed' && typeof payload.error === 'string' && payload.error)
      result(event.job_id).failure_error = { ...cut(payload.error), event_id: id }
  }
  const failure = triageFailure(item, ordered)
  if (failure) summaries.triage_failure = failure
  if (Object.keys(jobResults).length > 0) summaries.job_results = jobResults
  if (Object.keys(byOrder).length + Object.keys(byJob).length > 0)
    summaries.review_results = { by_order: byOrder, by_job: byJob }
  if (Object.keys(roundsByRound).length + Object.keys(roundsByOrder).length > 0)
    summaries.review_rounds = { by_round: roundsByRound, by_order: roundsByOrder }
  if (Object.keys(bounces).length > 0) summaries.review_bounces = bounces
  for (const key of Object.keys(summaries) as (keyof TaskSummaries)[])
    if (summaries[key] === undefined) delete summaries[key]
  return summaries
}

// ---- The bounded view ------------------------------------------------------

export function isBoundedView(value: unknown): value is ActivityItem {
  const item = value as Partial<ActivityItem> | null
  return Boolean(item && typeof item === 'object' && item.summaries && item.event_page)
}

// A capture as the bounded view presents it: metadata and the stored
// content's UTF-8 length, never the content (component-http-api, Bounded
// detail view). Work orders also appear inside stalled and recovery states, so
// every transcript_captures array outside the event fields is projected.
function projectTranscriptCaptures(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(projectTranscriptCaptures)
  if (!value || typeof value !== 'object') return value
  const projected: Record<string, unknown> = {}
  for (const [key, child] of Object.entries(value)) {
    projected[key] =
      key === 'transcript_captures' && Array.isArray(child)
        ? child.map((capture: Partial<WorkOrderTranscriptCapture>) => ({
            attempt_id: capture.attempt_id,
            termination_reason: capture.termination_reason,
            truncated: capture.truncated,
            captured_at: capture.captured_at,
            content_bytes: capture.content_bytes ?? Buffer.byteLength(capture.content ?? '', 'utf8'),
          }))
        : projectTranscriptCaptures(child)
  }
  return projected
}

function boundedCaptures(item: ActivityItem): ActivityItem {
  const projected: Record<string, unknown> = {}
  for (const [key, value] of Object.entries(item)) {
    projected[key] =
      key === 'events' || key === 'event_page' || key === 'summaries' ? value : projectTranscriptCaptures(value)
  }
  return projected as unknown as ActivityItem
}

// The content read of one capture in a complete-history fixture, or
// undefined when the task, order, or attempt has none (component-http-api,
// Transcript capture content read).
export function transcriptCaptureContent(
  item: CompleteActivity | undefined,
  workOrderId: string,
  attemptId: string,
): WorkOrderTranscriptCaptureContent | undefined {
  const order = (item?.work_orders ?? []).find((candidate) => candidate.id === workOrderId)
  const capture = order?.transcript_captures?.find((candidate) => candidate.attempt_id === attemptId)
  if (!capture) return undefined
  const content = capture.content ?? ''
  return {
    attempt_id: capture.attempt_id,
    content,
    termination_reason: capture.termination_reason,
    truncated: capture.truncated,
    captured_at: capture.captured_at,
    content_bytes: Buffer.byteLength(content, 'utf8'),
  }
}

// The bounded view of a complete-history fixture.
export function boundedView(item: CompleteActivity | ActivityItem, taskId = item.task?.id): ActivityItem {
  if (isBoundedView(item)) return item
  const complete = item as CompleteActivity
  const events = complete.events ?? []
  const human = selected(events, 'human')
  const boundary = boundaryOf(human)
  const page: TaskEventView[] = []
  let oldest: Ordered | undefined
  let size = 2
  for (let index = human.length - 1; index >= 0 && page.length < boundedEventLimit; index--) {
    const view = presentEvent(human[index].event)
    const next = bytes(view) + 1
    if (size + next + 160 > eventSectionMaxBytes && page.length > 0) break
    size += next
    page.unshift(view)
    oldest = human[index]
  }
  const hasOlder = oldest ? human.indexOf(oldest) > 0 : false
  return boundedCaptures({
    ...(complete as unknown as ActivityItem),
    events: page,
    event_page: {
      limit: boundedEventLimit,
      returned: page.length,
      kinds: 'human',
      has_older: hasOlder,
      ...(hasOlder && oldest
        ? {
            older_cursor: encodeHistoryCursor({
              v: 1,
              task: taskId ?? '',
              selection: 'human',
              limit: historyPageLimit,
              boundary,
              seek: { at: oldest.event.at, id: String(oldest.event.id) },
              direction: 'older',
              issued: 0,
            }),
          }
        : {}),
    },
    summaries: taskSummaries(complete),
  })
}

// ---- Route installation ----------------------------------------------------

const taskActivityPath = /^\/v1\/tasks\/([^/]+)\/activity$/
const taskHistoryPath = /^\/v1\/tasks\/([^/]+)\/history$/
const taskTranscriptPath = /^\/v1\/tasks\/([^/]+)\/work-orders\/([^/]+)\/transcript-captures\/([^/]+)$/
const completeByPage = new WeakMap<Page, Map<string, CompleteActivity>>()

// The newest complete-history fixture the page was served for a task.
export function completeActivityFor(page: Page, taskId: string): CompleteActivity | undefined {
  return completeByPage.get(page)?.get(taskId)
}

function mentions(url: Parameters<Page['route']>[0], word: string) {
  if (typeof url === 'string') return url.includes(word)
  if (url instanceof RegExp) return url.source.includes(word)
  return false
}

type FulfillOptions = Parameters<Route['fulfill']>[0]

function boundedResponse(page: Page, taskId: string, response: FulfillOptions): FulfillOptions {
  if (!response || (response.status !== undefined && (response.status < 200 || response.status >= 300))) return response
  let value: unknown
  let viaBody = false
  if ('json' in response && response.json !== undefined) value = response.json
  else if (typeof response.body === 'string' || Buffer.isBuffer(response.body)) {
    try {
      value = JSON.parse(response.body.toString())
      viaBody = true
    } catch {
      return response
    }
  } else return response
  const item = value as Partial<ActivityItem> | null
  if (!item || typeof item !== 'object' || !item.task || !Array.isArray(item.events) || isBoundedView(item))
    return response
  const complete = item as unknown as CompleteActivity
  const store = completeByPage.get(page) ?? new Map<string, CompleteActivity>()
  completeByPage.set(page, store)
  store.set(taskId, complete)
  const bounded = boundedView(complete, taskId)
  const { json: _json, body: _body, ...rest } = response as FulfillOptions & { json?: unknown; body?: unknown }
  return viaBody
    ? { ...rest, contentType: rest.contentType ?? 'application/json', body: JSON.stringify(bounded) }
    : { ...rest, json: bounded }
}

// Installs the fake server on a page: every activity body any later route
// serves for a task becomes its bounded view, `/history` reads page the
// newest complete fixture served for that task, and a transcript capture read
// returns that fixture's capture content or 404. A spec that routes a pattern
// naming `history` or `transcript` serves those reads itself; broader
// catch-all routes fall through to this fake server for them.
export async function installBoundedDetail(page: Page) {
  const routeHandledBy = new WeakSet<Route>()
  await page.route(/\/v1\/tasks\/[^/]+\/history(\?|$)/, async (route) => {
    const url = new URL(route.request().url())
    const taskId = decodeURIComponent(url.pathname.match(taskHistoryPath)?.[1] ?? '')
    const complete = completeActivityFor(page, taskId)
    try {
      const body = historyPage(complete?.events ?? [], {
        taskId,
        selection: url.searchParams.get('selection'),
        cursor: url.searchParams.get('cursor'),
        limit: url.searchParams.get('limit'),
      })
      await route.fulfill({ json: body, headers: { 'Cache-Control': 'no-store' } })
    } catch (error) {
      if (!(error instanceof HistoryRefusal)) throw error
      await route.fulfill({ status: error.status, json: { error: error.code, message: error.code } })
    }
  })
  await page.route(/\/v1\/tasks\/[^/]+\/work-orders\/[^/]+\/transcript-captures\/[^/?]+(\?|$)/, async (route) => {
    const match = new URL(route.request().url()).pathname.match(taskTranscriptPath)
    const [taskId, workOrderId, attemptId] = (match?.slice(1) ?? []).map(decodeURIComponent)
    const capture = transcriptCaptureContent(
      completeActivityFor(page, taskId ?? ''),
      workOrderId ?? '',
      attemptId ?? '',
    )
    if (!capture) {
      await route.fulfill({ status: 404, contentType: 'text/plain', body: '404 page not found\n' })
      return
    }
    await route.fulfill({ json: capture, headers: { 'Cache-Control': 'no-store' } })
  })
  const route = page.route.bind(page)
  page.route = ((url, handler, options) =>
    route(
      url,
      (target, request) => {
        const path = new URL(request.url()).pathname
        if (taskHistoryPath.test(path) && !mentions(url, 'history')) return target.fallback()
        if (taskTranscriptPath.test(path) && !mentions(url, 'transcript')) return target.fallback()
        const taskId = path.match(taskActivityPath)?.[1]
        if (taskId && !routeHandledBy.has(target)) {
          routeHandledBy.add(target)
          const fulfill = target.fulfill.bind(target)
          target.fulfill = (response) => fulfill(boundedResponse(page, decodeURIComponent(taskId), response))
        }
        return handler(target, request)
      },
      options,
    )) as Page['route']
}

// The Playwright `test` with the bounded-detail fake server installed on every
// page before the spec's own routes.
export const test = base.extend({
  page: async ({ page }, use) => {
    await installBoundedDetail(page)
    await use(page)
  },
})

// ---- Generated large ledgers ----------------------------------------------

export interface GeneratedLedger {
  item: CompleteActivity
  taskId: string
  // Decimal event IDs start above 2^53 so no exact JavaScript number holds them.
  firstEventId: bigint
}

const ledgerStart = Date.parse('2026-06-01T00:00:00Z')

// A task ledger shaped like the long-running production task this view was
// built for: a handful of lifecycle events and human entries among tens of
// thousands of `dispatch.reconciled` records. `human` sets how many human
// timeline entries (assignment changes and merge requests) the ledger holds,
// so the bounded view can hold all of them or page the rest.
export function generateLedger(options: {
  taskId: string
  total: number
  reconciled: number
  human: number
  title?: string
}): GeneratedLedger {
  const { taskId, total, reconciled, human } = options
  const firstEventId = 9_007_199_254_740_993n
  const events: CompleteEvent[] = []
  let sequence = 0
  const at = (index: number) => {
    const micros = BigInt(ledgerStart) * 1000n + BigInt(index) * 1_000_037n
    const millis = Number(micros / 1000n)
    const fraction = String(micros % 1_000_000n).padStart(6, '0')
    return `${new Date(millis).toISOString().slice(0, 19)}.${fraction}Z`
  }
  const push = (event: Omit<CompleteEvent, 'id' | 'at' | 'task_id'>) => {
    const index = sequence++
    // Odd decimal IDs above 2^53: no JavaScript number holds any of them exactly.
    events.push({ id: String(firstEventId + 2n * BigInt(index)), task_id: taskId, at: at(index), ...event })
  }
  const triage = { id: `${taskId}-triage`, stage: 'triage' }
  const implement = { id: `${taskId}-implement`, stage: 'implement' }
  const review = { id: `${taskId}-review`, stage: 'review' }
  const reviewOrder = `${taskId}-review-order`
  // Lifecycle and summary-bearing events come first, so the newest human page
  // and the newest technical page never contain them.
  const lifecycle: Array<Omit<CompleteEvent, 'id' | 'at' | 'task_id'>> = [
    { kind: 'task.created', actor_id: 'user:usr_operator', actor_role: 'human', payload: { source: 'cli' } },
    { kind: 'pipeline.dispatched', actor_id: 'conveyor', actor_role: 'system', payload: { stage: 'triage' } },
    { kind: 'job.created', job_id: triage.id, actor_id: 'conveyor', actor_role: 'system', payload: {} },
    {
      kind: 'triage.completed',
      job_id: triage.id,
      actor_id: 'agent:triage',
      actor_role: 'agent',
      payload: { summary: 'Triage accepted the task for planning.' },
    },
    { kind: 'job.created', job_id: implement.id, actor_id: 'conveyor', actor_role: 'system', payload: {} },
    {
      kind: 'work_order.claimed',
      job_id: implement.id,
      actor_id: 'agent:impl',
      actor_role: 'agent',
      payload: { stage: 'implement', claimant_id: 'agent:impl' },
    },
    {
      kind: 'job.summary',
      job_id: implement.id,
      actor_id: 'agent:impl',
      actor_role: 'agent',
      payload: { summary: 'Implemented the bounded history pager across every task surface.' },
    },
    {
      kind: 'pull_request.opened',
      actor_id: 'conveyor',
      actor_role: 'system',
      payload: {
        url: `https://github.com/acme/widgets/pull/4242`,
        number: 4242,
        repository: 'acme/widgets',
        head_sha: 'feedfacecafebeef00112233445566778899aabb',
        base_sha: '0011223344556677889900aabbccddeeff001122',
      },
    },
    { kind: 'job.created', job_id: review.id, actor_id: 'conveyor', actor_role: 'system', payload: {} },
    {
      kind: 'review.completed',
      job_id: review.id,
      actor_id: 'agent:review',
      actor_role: 'agent',
      payload: {
        verdict: 'approve',
        review_work_order_id: reviewOrder,
        summary: 'Seat one approves the bounded pager.',
        feedback: 'Bounded history pages keep the sheet responsive.',
      },
    },
    {
      kind: 'review.round_completed',
      actor_id: 'conveyor',
      actor_role: 'system',
      payload: {
        verdict: 'approve',
        review_round: 1,
        summary: 'The panel approved the bounded pager.',
        reviews: [{ review_work_order_id: reviewOrder, verdict: 'approve' }],
      },
    },
  ]
  for (const event of lifecycle) push(event)
  const others = total - reconciled - lifecycle.length - human
  if (others < 0) throw new Error('the ledger is too small for its fixed events')
  // Spread human entries and the remaining bookkeeping evenly through the
  // reconciliation stream, with one omitted-payload human event near the
  // middle and one in the newest page.
  const fillers = reconciled + others + human
  const humanEvery = human > 0 ? Math.floor(fillers / human) : Number.POSITIVE_INFINITY
  let humanLeft = human
  let othersLeft = others
  let reconciledLeft = reconciled
  for (let index = 0; index < fillers; index++) {
    const humanSlot = humanLeft > 0 && (index + 1) % humanEvery === 0
    if (humanSlot || (humanLeft > 0 && fillers - index === humanLeft)) {
      const ordinal = human - humanLeft
      humanLeft--
      const large = ordinal === Math.floor(human / 2) || ordinal === human - 2
      push(
        large
          ? {
              kind: 'dispatch.failed',
              actor_id: 'conveyor',
              actor_role: 'system',
              payload: {
                error: `Dispatch failed with a long diagnostic ${ordinal}`,
                attempt: ordinal,
                trace: 'x'.repeat(9_000),
              },
            }
          : ordinal % 2 === 0
            ? {
                kind: 'task.assignee.set',
                actor_id: 'user:usr_operator',
                actor_role: 'human',
                payload: { assignee_user_id: `usr_${ordinal}`, ordinal },
              }
            : {
                kind: 'merge.requested',
                actor_id: 'conveyor',
                actor_role: 'system',
                payload: { url: `https://github.com/acme/widgets/pull/4242#request-${ordinal}`, ordinal },
              },
      )
    } else if (othersLeft > 0 && index % 211 === 7) {
      othersLeft--
      push({
        kind: 'work_order.lease_renewed',
        actor_id: 'agent:impl',
        actor_role: 'agent',
        payload: { attempt: index },
      })
    } else if (reconciledLeft > 0) {
      reconciledLeft--
      push({
        kind: 'dispatch.reconciled',
        actor_id: 'conveyor',
        actor_role: 'system',
        payload: { reason: 'queue reconciliation', pass: index },
      })
    } else {
      othersLeft--
      push({
        kind: 'work_order.lease_renewed',
        actor_id: 'agent:impl',
        actor_role: 'agent',
        payload: { attempt: index },
      })
    }
  }
  if (events.length !== total) throw new Error(`generated ${events.length} events, expected ${total}`)
  const created = events[0].at
  const jobs: Job[] = [
    jobFixture(triage.id, taskId, 'triage', events[2].at, events[3].at),
    jobFixture(implement.id, taskId, 'implement', events[4].at, events[6].at),
    jobFixture(review.id, taskId, 'review', events[8].at, events[9].at),
  ]
  const item = {
    task: {
      id: taskId,
      workspace: 'demo',
      title: options.title ?? 'Long-running reconciliation task',
      body: 'A task whose ledger grew past twenty thousand events.',
      class: 'feature',
      level: '',
      spec_approval: false,
      merge_approval: true,
      policy_version: 1,
      repo: 'widgets',
      base_branch: 'main',
      branch: `conveyor/${taskId}`,
      state: 'awaiting_human',
      source: 'cli',
      created_at: created,
    },
    jobs,
    events,
    interventions: [],
    work_orders: [
      {
        id: reviewOrder,
        task_id: taskId,
        job_id: review.id,
        stage: 'review',
        state: 'completed',
        review_round: 1,
        review_seat: 1,
        queue_entered_at: events[8].at,
        created_at: events[8].at,
        model: 'claude-opus',
        tokens_in: 0,
        tokens_out: 0,
      },
    ],
    checkout_available: false,
    checkout_guidance: '',
    needs_attention: true,
    at_merge_gate: true,
    merge_readiness: { state: 'MERGEABLE', head_sha: 'feedfacecafebeef00112233445566778899aabb' },
  } as unknown as CompleteActivity
  return { item, taskId, firstEventId }
}

function jobFixture(id: string, taskId: string, stage: string, startedAt: string, endedAt: string): Job {
  return {
    id,
    task_id: taskId,
    stage,
    harness: 'claude-code',
    model_tier: 'standard',
    runner: 'external',
    confinement: 'none',
    tokens_in: 0,
    tokens_out: 0,
    state: 'done',
    started_at: startedAt,
    ended_at: endedAt,
  } as unknown as Job
}
