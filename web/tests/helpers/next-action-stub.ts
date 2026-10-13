import type { Page, Route } from '@playwright/test'
import type { ActivityItem, NextAction, NextActionChoice, WorkOrder } from '../../src/lib/types'

// A fake server for GET /v1/tasks/{id}/next-action in specs whose fixtures
// predate the next-action projection. It stands in for internal/nextaction:
// the kinds, labels, summaries and choices are the server catalogue's
// (catalogue.go, choices.go), chosen from the activity fixture the page was
// served. The dashboard itself never makes these decisions.

type ChoiceSpec = [id: string, label: string, method: string, path: string]

const review: ChoiceSpec[] = [
  ['approve', 'Approve', 'POST', '/v1/tasks/{id}/review'],
  ['redirect', 'Redirect', 'POST', '/v1/tasks/{id}/review'],
  ['reject', 'Reject', 'POST', '/v1/tasks/{id}/review'],
]
const stalledChoices: ChoiceSpec[] = [
  ['recover', 'Recover', 'POST', '/v1/work-orders/{order_id}/recover'],
  ['redispatch', 'Redispatch', 'POST', '/v1/tasks/{id}/redispatch'],
]

const catalogue: Record<string, { label: string; summary: string; question?: string; choices: ChoiceSpec[] }> = {
  terminal: { label: 'Finished', summary: 'This task is finished.', choices: [] },
  parked: {
    label: 'Parked',
    summary: 'This task is parked.',
    question: 'Put it back, or close it?',
    choices: [
      ['recover', 'Recover', 'POST', '/v1/tasks/{id}/redispatch'],
      ['close', 'Close', 'POST', '/v1/tasks/{id}/close'],
    ],
  },
  plan_approval: {
    label: 'Plan approval',
    summary: 'The execution plan is waiting for approval.',
    question: 'Approve this plan?',
    choices: review,
  },
  plan_revision: {
    label: 'Plan revision',
    summary: 'Implementation asked to revise the plan.',
    question: 'Accept the revised plan?',
    choices: [
      ['approve_revision', 'Approve revision', 'POST', '/v1/tasks/{id}/review'],
      ['decline', 'Decline', 'POST', '/v1/tasks/{id}/review'],
      ['reject', 'Reject', 'POST', '/v1/tasks/{id}/review'],
    ],
  },
  merge_approval: {
    label: 'Merge approval',
    summary: 'The reviewed branch is waiting for merge approval.',
    question: 'Approve the merge?',
    choices: [
      ['approve', 'Approve', 'POST', '/v1/tasks/{id}/review'],
      ['request_changes', 'Request changes', 'POST', '/v1/tasks/{id}/request-changes'],
    ],
  },
  review_bounce_limit: {
    label: 'Review bounce limit',
    summary: 'Review bounced until the limit.',
    question: 'Recover this review?',
    choices: review,
  },
  job_failure: { label: 'Job failure', summary: 'A job failed and needs a person.', choices: review },
  merge_conflict: {
    label: 'Merge conflict',
    summary: 'Automatic conflict fixes are exhausted.',
    choices: [['fix_merge_conflict', 'Fix merge conflict', 'POST', '/v1/tasks/{id}/merge-conflict-fix']],
  },
  merge_execution: {
    label: 'Merge',
    summary: 'The merge gate is on and the branch is ready to merge.',
    question: 'Merge this branch?',
    choices: [
      ['merge', 'Merge', 'POST', '/v1/tasks/{id}/merge'],
      ['request_changes', 'Request changes', 'POST', '/v1/tasks/{id}/request-changes'],
      ['close', 'Close', 'POST', '/v1/tasks/{id}/close'],
    ],
  },
  merging: { label: 'Merging', summary: 'Conveyor is merging this branch.', choices: [] },
  agent_question: {
    label: 'Agent question',
    summary: 'The agent stopped for a decision.',
    question: 'Answer the agent?',
    choices: [],
  },
  verification_disposition: {
    label: 'Verification disposition',
    summary: 'Verification needs a disposition.',
    question: 'Was the change applied?',
    choices: [],
  },
  dependency_unsatisfiable: {
    label: 'Dependency unsatisfiable',
    summary: 'A dependency ended without merging.',
    question: 'Remove the dependency, or close the task?',
    choices: [],
  },
  stalled: { label: 'Stalled', summary: 'This order cannot be claimed.', choices: stalledChoices },
  blocked: { label: 'Blocked', summary: 'This task is waiting on another task.', choices: [] },
  working: { label: 'Working', summary: 'An agent is working on this task.', choices: [] },
  retrying: { label: 'Retrying', summary: 'Conveyor will retry this automatically.', choices: [] },
}

const operatorKinds: Record<string, true> = {
  parked: true,
  plan_approval: true,
  plan_revision: true,
  merge_approval: true,
  review_bounce_limit: true,
  job_failure: true,
  merge_conflict: true,
  merge_execution: true,
  agent_question: true,
  verification_disposition: true,
  dependency_unsatisfiable: true,
  stalled: true,
}

function activityTime(order: WorkOrder) {
  return new Date(
    order.updated_at ?? order.last_failure_at ?? order.execution_started_at ?? order.queue_entered_at,
  ).getTime()
}

// The order the server would name as current, and the kind it would state.
function orderKind(item: ActivityItem): { kind: string; order?: WorkOrder } | undefined {
  const orders = item.work_orders ?? []
  const newestPerStage = new Map<string, WorkOrder>()
  for (const order of orders) {
    if (order.state === 'cancelled') continue
    const seen = newestPerStage.get(order.stage)
    if (!seen || (order.created_at ?? '') > (seen.created_at ?? '')) newestPerStage.set(order.stage, order)
  }
  const candidates = orders
    .filter((order) => !(order.stage === 'review' && (order.review_round ?? 0) > 0))
    .filter((order) => order.state === 'claimed' || ['queued', 'stale', 'timed_out'].includes(order.state))
    .filter((order) => order.state === 'claimed' || newestPerStage.get(order.stage)?.id === order.id)
    .sort((a, b) => activityTime(b) - activityTime(a))
  const claimed = candidates.find((order) => order.state === 'claimed')
  if (claimed) return { kind: 'working', order: claimed }
  const blockingIDs = item.task.blocking_task_ids ?? []
  const unsatisfiable = orders.find(
    (order) =>
      order.stage === 'implement' &&
      order.state === 'queued' &&
      !order.claimable &&
      blockingIDs.length > 0 &&
      (item.stalled?.unsatisfiable_edge === true || (order.unsatisfiable_task_ids?.length ?? 0) > 0),
  )
  const paused = candidates.find(
    (order) =>
      order.state !== 'queued' || Boolean(order.last_attempt_outcome || order.retry_suppressed || order.next_retry_at),
  )
  if (!paused) {
    if (unsatisfiable) return { kind: 'dependency_unsatisfiable', order: unsatisfiable }
    const waiting = orders.find(
      (order) => order.stage === 'implement' && order.state === 'queued' && !order.claimable && blockingIDs.length > 0,
    )
    if (waiting) return { kind: 'blocked', order: waiting }
    return undefined
  }
  if (paused.next_retry_at && !paused.retry_suppressed) return { kind: 'retrying', order: paused }
  if (paused.last_failure_message === 'operator checkpoint reached') return { kind: 'agent_question', order: paused }
  if (paused.stage === 'verify' && paused.checkpoint?.verification)
    return { kind: 'verification_disposition', order: paused }
  return { kind: 'stalled', order: paused }
}

function gateKind(item: ActivityItem): string {
  // A failed triage holds the task without a gate kind: the server's job_failure.
  if (item.task.recovery_stage === 'triage') return 'job_failure'
  const events = item.events ?? []
  for (let i = events.length - 1; i >= 0; i--) {
    const kind = events[i].kind
    if (kind.startsWith('intervention.')) break
    if (kind === 'work_order.plan_revision_requested') return 'plan_revision'
  }
  if (item.at_merge_gate) return 'merge_approval'
  for (let i = events.length - 1; i >= 0; i--) {
    const kind = events[i].kind
    if (kind === 'pipeline.bounce_limit') return 'review_bounce_limit'
    if (kind === 'job.timeout') return 'job_failure'
    if (kind === 'job.created' || kind.startsWith('intervention.')) break
  }
  return 'plan_approval'
}

export function projectNextAction(item: ActivityItem): NextAction {
  const task = item.task
  let kind: string
  let order: WorkOrder | undefined
  if (task.state === 'merged' || task.state === 'closed') kind = 'terminal'
  else if (task.state === 'parked') kind = 'parked'
  else if (task.state === 'awaiting_human') kind = gateKind(item)
  else if (task.state === 'approved') kind = task.merge_approval ? 'merge_execution' : 'merging'
  else {
    const current = orderKind(item)
    kind = current?.kind ?? 'working'
    order = current?.order
  }
  const entry = catalogue[kind]
  const choices: NextActionChoice[] = entry.choices.map(([id, label, method, path]) => ({
    id,
    label,
    effect: label,
    method,
    path: path.replace('{id}', task.id).replace('{order_id}', order?.id ?? ''),
    capability: '',
    allowed: true,
  }))
  return {
    kind,
    label: entry.label,
    actor: operatorKinds[kind]
      ? 'operator'
      : kind === 'terminal'
        ? 'none'
        : kind === 'blocked'
          ? 'dependency'
          : 'factory',
    judgment: operatorKinds[kind] === true,
    summary: entry.summary,
    question: entry.question,
    deadline: null,
    choices,
    current_order: order && {
      id: order.id,
      stage: order.stage,
      state: order.state,
      queue_deadline: null,
      execution_deadline: null,
      lease_expires_at: null,
    },
    details: {},
  }
}

function withCompact(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(withCompact)
  const item = value as ActivityItem | null
  if (!item || typeof item !== 'object' || !item.task || item.next_action || !Array.isArray(item.events)) return value
  const { label, actor, kind, judgment, summary, deadline } = projectNextAction(item)
  return { ...item, next_action: { label, actor, kind, judgment, summary, deadline } }
}

const nextActionURL = /\/v1\/tasks\/[^/]+\/next-action(\?|$)/

// Serves each task's next action from the newest activity body the page
// received for it, so per-test activity overrides drive the projection too,
// and adds the compact next action the server puts on every activity item.
// Install it before any other route: it wraps later `page.route` handlers so
// their activity bodies carry the projection and they never answer the
// next-action read themselves. Activity reads still in flight are awaited, so
// a refetch after an act sees the refreshed fixture.
export async function serveNextActionsFromActivity(page: Page) {
  const latest = new Map<string, ActivityItem>()
  const pending = new Map<string, Set<Promise<unknown>>>()
  const taskOf = (url: string) => new URL(url).pathname.match(/^\/v1\/tasks\/([^/]+)\/activity$/)?.[1]
  page.on('request', (request) => {
    const id = taskOf(request.url())
    if (!id) return
    const settle = request
      .response()
      .then(async (response) => {
        if (response?.ok()) {
          const body = (await response.json().catch(() => undefined)) as ActivityItem | undefined
          if (body?.task) latest.set(decodeURIComponent(id), body)
        }
      })
      .catch(() => undefined)
    const set = pending.get(id) ?? new Set()
    set.add(settle)
    pending.set(id, set)
    void settle.finally(() => set.delete(settle))
  })
  await page.route(nextActionURL, async (route: Route) => {
    const id = new URL(route.request().url()).pathname.split('/')[3]
    for (let tries = 0; tries < 100; tries++) {
      const inflight = [...(pending.get(id) ?? [])]
      if (inflight.length > 0) await Promise.all(inflight)
      else if (latest.has(decodeURIComponent(id))) break
      else {
        const { promise, resolve } = Promise.withResolvers<void>()
        setTimeout(resolve, 20)
        await promise
      }
    }
    const item = latest.get(decodeURIComponent(id))
    if (!item) return route.fulfill({ status: 404, json: { error: 'not found' } })
    return route.fulfill({ json: projectNextAction(item) })
  })
  const route = page.route.bind(page)
  page.route = ((url, handler, options) =>
    route(
      url,
      (target, request) => {
        if (nextActionURL.test(request.url())) return target.fallback()
        const fulfill = target.fulfill.bind(target)
        target.fulfill = (response) =>
          fulfill(response && 'json' in response ? { ...response, json: withCompact(response.json) } : response)
        return handler(target, request)
      },
      options,
    )) as Page['route']
}
