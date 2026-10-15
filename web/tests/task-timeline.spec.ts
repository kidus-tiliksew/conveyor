import { expect, type Page, test } from '@playwright/test'
import type { NextAction, WorkOrder } from '../src/lib/types'
import { callerIdentity } from './helpers/caller-identity'

// Task detail tells the task's story through its timeline
// (req-operator-next-action REQ-7; feature-operator-next-action NA-6;
// component-web-task-surfaces). There is no stage-flow chart: the next-action
// card sits directly above the timeline, the current attempt's facts render on
// the entry whose order and attempt the server names, and parked, closed,
// dependency waits, automatic retries, and bounces read as entries.

const t0 = '2026-10-15T08:00:00Z'
const taskId = 'timeline-task'
const task = {
  id: taskId,
  workspace: 'demo',
  title: 'Tell the story on the timeline',
  state: 'running',
  repo: 'conveyor',
  branch: `conveyor/${taskId}`,
  created_at: t0,
}

const minutes = (n: number) => new Date(Date.parse(t0) + n * 60_000).toISOString()

function order(id: string, extra: Partial<WorkOrder>): WorkOrder {
  return {
    id,
    task_id: taskId,
    job_id: id,
    stage: 'implement',
    state: 'claimed',
    claimable: false,
    queue_entered_at: minutes(5),
    created_at: minutes(5),
    updated_at: minutes(6),
    ...extra,
  } as WorkOrder
}

function job(id: string, stage: string, state: string, extra: Record<string, unknown> = {}) {
  return {
    id,
    task_id: taskId,
    stage,
    harness: 'external-mcp',
    model_tier: 'operator-owned',
    runner: 'external',
    confinement: 'none',
    tokens_in: 0,
    tokens_out: 0,
    state,
    started_at: minutes(6),
    ...extra,
  }
}

const attempt = {
  harness: 'claude-code',
  model: 'claude-opus-5-5',
  user: 'operator@example.test',
  claimed_by: 'run:usr_operator',
  session_id: 'sess-current',
  attempt_id: 'attempt-current',
  claimed_at: minutes(6),
  last_check_in: minutes(20),
  lease_expires_at: minutes(70),
  progress: 'Wiring the attempt into the timeline',
  elapsed_seconds: 3723,
}

function nextAction(orderId: string, stage = 'implement', extra: Partial<NextAction> = {}): NextAction {
  return {
    label: 'Working',
    actor: 'agent',
    kind: 'working',
    judgment: false,
    summary: 'Server summary: an agent is working on this task.',
    deadline: null,
    choices: [],
    details: {},
    current_order: {
      id: orderId,
      stage,
      state: 'claimed',
      queue_deadline: null,
      execution_deadline: null,
      lease_expires_at: attempt.lease_expires_at,
    },
    attempt,
    ...extra,
  }
}

function activity(extra: Record<string, unknown> = {}) {
  return {
    task,
    jobs: [],
    events: [],
    work_orders: [],
    reviews: [],
    interventions: [],
    latest_stage: 'implement',
    plan: { state: 'approved', version: 1 },
    last_event_at: minutes(20),
    needs_attention: false,
    ...extra,
  }
}

// An implementation job running under the claimed order the server names.
const jobItem = activity({
  jobs: [job(`${taskId}-implement-1`, 'implement', 'running')],
  work_orders: [order(`${taskId}-implement-1`, { attempt_id: 'attempt-current', session_id: 'sess-current' })],
})

async function mockAPI(page: Page, item: object, action: NextAction) {
  const counts = { flow: 0 }
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'operator', email: 'operator@example.test', role: 'operator' }),
      })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    if (path === `/v1/tasks/${taskId}/flow`) {
      counts.flow++
      return route.fulfill({ status: 500, json: {} })
    }
    if (path === `/v1/tasks/${taskId}/next-action`) return route.fulfill({ json: action })
    if (path === `/v1/tasks/${taskId}/activity`) return route.fulfill({ json: item })
    if (path.endsWith('/stream')) return route.fulfill({ status: 204 })
    if (path === '/v1/activity' || path === '/v1/task-operations')
      return route.fulfill({
        json: [{ ...item, next_action: { ...action, choices: undefined } }],
        headers: { 'X-Conveyor-Total': '1', 'X-Conveyor-Limit': '100', 'X-Conveyor-Offset': '0' },
      })
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
    return route.fulfill({ json: [] })
  })
  return counts
}

const surfaces = [
  { name: 'Board sheet', url: `/tasks/${taskId}` },
  { name: 'full task route', url: `/tasks/${taskId}/full` },
] as const

for (const surface of surfaces) {
  test(`the ${surface.name} has no stage-flow chart and puts the next action directly above the timeline`, async ({
    page,
  }) => {
    const counts = await mockAPI(page, jobItem, nextAction(`${taskId}-implement-1`))
    await page.goto(surface.url)
    const timeline = page.getByRole('region', { name: 'Execution event timeline' })
    await expect(timeline).toBeVisible()
    await expect(page.getByRole('region', { name: 'Task flow' })).toHaveCount(0)
    await expect(page.locator('[data-flow-stage]')).toHaveCount(0)
    const card = page.getByRole('region', { name: 'Next action' })
    await expect(card).toBeVisible()
    // The next-action card is the timeline's immediate predecessor.
    const adjacent = await card.evaluate(
      (element, other) => element.nextElementSibling === other,
      await timeline.elementHandle(),
    )
    expect(adjacent).toBe(true)
    expect(counts.flow).toBe(0)
  })

  test(`the ${surface.name} shows the current attempt on its running job entry`, async ({ page, context }) => {
    await context.grantPermissions(['clipboard-read', 'clipboard-write'])
    await mockAPI(page, jobItem, nextAction(`${taskId}-implement-1`))
    await page.goto(surface.url)
    const current = page.getByRole('region', { name: 'Current attempt' })
    await expect(current).toHaveCount(1)
    await expect(current).toHaveAttribute('data-current-attempt', `${taskId}-implement-1`)
    const field = (term: string) =>
      current.locator('dt', { hasText: new RegExp(`^${term}$`) }).locator('xpath=following-sibling::dd[1]')
    await expect(field('Harness')).toHaveText('claude-code')
    await expect(field('Model')).toHaveText('claude-opus-5-5')
    await expect(field('User')).toHaveText('operator@example.test')
    await expect(field('Claimed')).not.toHaveText('')
    await expect(field('Last check-in')).toContainText('ago')
    await expect(field('Progress')).toHaveText('Wiring the attempt into the timeline')
    await expect(field('Elapsed')).toHaveText('1h 02m')
    // The lease is the server's to judge; the entry prints no lease field.
    await expect(current.locator('dt', { hasText: /^Lease$/ })).toHaveCount(0)
    // Identifiers sit behind Show IDs.
    await expect(current.getByText('sess-current')).toBeHidden()
    await current.getByText('Show IDs').click()
    await expect(current.getByText('sess-current')).toBeVisible()
    await expect(current.getByText('attempt-current')).toBeVisible()
    await current.getByRole('button', { name: 'Copy attempt ID' }).click()
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe('attempt-current')
  })
}

test('a claimed order without a job carries the current attempt on its own entry', async ({ page }) => {
  const item = activity({
    work_orders: [order(`${taskId}-implement-2`, { attempt_id: 'attempt-current', agent: 'claude-code' })],
  })
  await mockAPI(page, item, nextAction(`${taskId}-implement-2`))
  await page.goto(`/tasks/${taskId}/full`)
  const current = page.getByRole('region', { name: 'Current attempt' })
  await expect(current).toHaveAttribute('data-current-attempt', `${taskId}-implement-2`)
  await expect(current).toContainText('Wiring the attempt into the timeline')
})

test('a review seat carries the current attempt, and its sibling seat does not', async ({ page }) => {
  const seat = (n: number, attemptId: string) =>
    order(`${taskId}-review-1-seat-${n}`, {
      stage: 'review',
      job_id: `${taskId}-review-1-seat-${n}`,
      review_round: 1,
      review_seat: n,
      attempt_id: attemptId,
      model: 'gpt-6',
      agent: 'codex',
    })
  const item = activity({
    latest_stage: 'review',
    work_orders: [seat(1, 'attempt-current'), seat(2, 'attempt-other')],
  })
  await mockAPI(page, item, nextAction(`${taskId}-review-1-seat-1`, 'review'))
  await page.goto(`/tasks/${taskId}/full`)
  const current = page.getByRole('region', { name: 'Current attempt' })
  await expect(current).toHaveCount(1)
  await expect(current).toHaveAttribute('data-current-attempt', `${taskId}-review-1-seat-1`)
})

test('an entry whose attempt is not the current one shows no attempt facts', async ({ page }) => {
  const item = activity({
    jobs: [job(`${taskId}-implement-1`, 'implement', 'running')],
    work_orders: [order(`${taskId}-implement-1`, { attempt_id: 'attempt-previous' })],
  })
  await mockAPI(page, item, nextAction(`${taskId}-implement-1`))
  await page.goto(`/tasks/${taskId}/full`)
  await expect(page.getByRole('region', { name: 'Execution event timeline' })).toBeVisible()
  await expect(page.getByText('Tell the story on the timeline').first()).toBeVisible()
  await expect(page.getByRole('region', { name: 'Current attempt' })).toHaveCount(0)
})

test('a next action without an attempt shows no attempt facts', async ({ page }) => {
  await mockAPI(page, jobItem, nextAction(`${taskId}-implement-1`, 'implement', { attempt: undefined }))
  await page.goto(`/tasks/${taskId}/full`)
  await expect(page.getByRole('region', { name: 'Execution event timeline' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Current attempt' })).toHaveCount(0)
})

function event(id: number, kind: string, at: string, payload: Record<string, unknown>) {
  return { id, task_id: taskId, kind, actor_id: 'system', actor_role: 'system', payload, at }
}

test('dependency waits, automatic retries, bounces, parking, and closing read as timeline entries', async ({
  page,
}) => {
  const item = activity({
    task: { ...task, state: 'closed' },
    events: [
      event(1, 'task.dependency_added', minutes(1), { task_id: taskId, depends_on_task_id: 'task-prereq' }),
      event(2, 'work_order.child_failed', minutes(2), { reason: 'child exited 1', retry_suppressed: false }),
      // A retry the factory will not take stays out of the story.
      event(3, 'work_order.child_failed', minutes(3), { reason: 'child exited 2', retry_suppressed: true }),
      event(4, 'pipeline.bounced', minutes(4), { from: 'verify', to: 'implement', count: 2, source: 'verify' }),
      event(5, 'task.state_changed', minutes(5), { from: 'running', to: 'parked', command: 'triage.park' }),
      event(6, 'task.state_changed', minutes(6), { from: 'parked', to: 'closed', command: 'task.start_over' }),
      // A rejection is told by its intervention entry, so no second Closed entry.
      event(7, 'task.state_changed', minutes(7), { from: 'running', to: 'closed', command: 'intervention.reject' }),
      // Unrelated state changes stay out.
      event(8, 'task.state_changed', minutes(0), { from: 'queued', to: 'running', command: 'dispatch.start' }),
    ],
  })
  await mockAPI(page, item, {
    ...nextAction(`${taskId}-implement-1`),
    label: 'Finished',
    actor: 'none',
    kind: 'terminal',
    current_order: undefined,
    attempt: undefined,
  })
  await page.goto(`/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('Waiting on another task')).toBeVisible()
  await expect(timeline.getByText('task-prereq')).toBeVisible()
  await expect(timeline.getByText('The agent’s run failed — retrying')).toHaveCount(1)
  await expect(timeline.getByText('The agent’s run failed — not retried automatically')).toHaveCount(0)
  await expect(timeline.getByText('Sent back to implementation')).toBeVisible()
  await expect(timeline.getByText('Bounce 2')).toBeVisible()
  await expect(timeline.getByText('Parked', { exact: true })).toBeVisible()
  await expect(timeline.getByText('Triage parked the task for an operator decision')).toBeVisible()
  await expect(timeline.getByText('Closed — started over as a new task')).toBeVisible()
  await expect(timeline.getByText('Closed', { exact: true })).toHaveCount(0)
})

for (const theme of ['light', 'dark'] as const) {
  test(`the current attempt renders in the ${theme} theme`, async ({ page }) => {
    await page.addInitScript((value) => localStorage.setItem('conveyor-theme', value), theme)
    await mockAPI(page, jobItem, nextAction(`${taskId}-implement-1`))
    await page.goto(`/tasks/${taskId}/full`)
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
    await expect(page.getByRole('region', { name: 'Current attempt' })).toBeVisible()
  })
}

test.describe('at 390 px', () => {
  test.use({ viewport: { width: 390, height: 844 } })

  for (const surface of surfaces) {
    test(`the ${surface.name} timeline with an attempt has no horizontal scroll`, async ({ page }) => {
      await mockAPI(page, jobItem, nextAction(`${taskId}-implement-1`))
      await page.goto(surface.url)
      await expect(page.getByRole('region', { name: 'Current attempt' })).toBeVisible()
      const viewport = await page.evaluate(() => [
        document.documentElement.scrollWidth,
        document.documentElement.clientWidth,
      ])
      expect(viewport[0]).toBeLessThanOrEqual(viewport[1])
    })
  }
})
