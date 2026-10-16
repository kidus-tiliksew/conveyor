import { expect, type Locator, type Page, test } from '@playwright/test'
import { buildTimeline, type TimelineEntry } from '../src/lib/activity'
import type { ActivityItem, NextAction, WorkOrder } from '../src/lib/types'
import { callerIdentity } from './helpers/caller-identity'

// Task detail tells the task's story through its timeline
// (req-operator-next-action REQ-7; feature-operator-next-action NA-6;
// component-web-task-surfaces). The Timeline tab is the default below the
// next-action card and reads no flow (AC-7.4; the Flow tab is covered by
// task-flow.spec.ts), no entry renders a Current attempt facts region, each
// stage entry keeps its own content, and parked, closed, dependency waits,
// automatic retries, and bounces read as entries.

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
        json: callerIdentity({ id: 'operator', email: 'operator@example.test', role: 'operator' }, route.request()),
      })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    if (path === `/v1/tasks/${taskId}/flow`) {
      counts.flow++
      return route.fulfill({ status: 500, json: {} })
    }
    if (path === `/v1/tasks/${taskId}/next-action`) return route.fulfill({ json: action })
    if (path === `/v1/tasks/${taskId}/verification`)
      return route.fulfill({
        json: { head_sha: '', current_context_id: '', contexts: { items: [], next_cursor: '' }, overview: {} },
      })
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
  test(`the ${surface.name} shows the Timeline tab by default below the next action and reads no flow`, async ({
    page,
  }) => {
    const counts = await mockAPI(page, jobItem, nextAction(`${taskId}-implement-1`))
    await page.goto(surface.url)
    const timeline = page.getByRole('region', { name: 'Execution event timeline' })
    await expect(timeline).toBeVisible()
    await expect(page.getByRole('tab', { name: 'Timeline', exact: true })).toHaveAttribute('aria-selected', 'true')
    await expect(page.getByRole('tab', { name: 'Flow', exact: true })).toHaveAttribute('aria-selected', 'false')
    await expect(page.getByRole('region', { name: 'Task flow' })).toHaveCount(0)
    await expect(page.locator('[data-flow-node]')).toHaveCount(0)
    const card = page.getByRole('region', { name: 'Next action' })
    await expect(card).toBeVisible()
    // The timeline sits in the selected tab panel, which follows the
    // next-action card.
    const panel = page.getByRole('tabpanel')
    await expect(panel.getByRole('region', { name: 'Execution event timeline' })).toBeVisible()
    const follows = await card.evaluate(
      (element, other) => Boolean(other && element.compareDocumentPosition(other) & Node.DOCUMENT_POSITION_FOLLOWING),
      await panel.elementHandle(),
    )
    expect(follows).toBe(true)
    expect(counts.flow).toBe(0)
  })
}

// Timeline entries carry no attempt facts panel (req-operator-next-action
// REQ-7, AC-7.1, AC-7.3). With a next action whose attempt matches the
// entry's own order and attempt, no entry renders a Current attempt region,
// and each entry keeps the stage content it already carried. Each case first
// waits for its entry, so the absence check never runs against an unloaded
// timeline.
const detailSurfaces = [
  { name: 'Board sheet', url: `/tasks/${taskId}` },
  { name: 'Tasks panel', url: `/tasks?task=${taskId}` },
  { name: 'full route', url: `/tasks/${taskId}/full` },
] as const

const elapsed = /^\d+(h \d{2}m|m \d{2}s|s)$/

interface AbsenceCase {
  entryKind: string
  item: object
  action: NextAction
  fullRouteOnly?: boolean
  retained: (timeline: Locator) => Promise<void>
}

const absenceCases: AbsenceCase[] = [
  {
    entryKind: 'claimed running job',
    item: activity({
      jobs: [job(`${taskId}-implement-1`, 'implement', 'running')],
      work_orders: [
        order(`${taskId}-implement-1`, {
          attempt_id: 'attempt-current',
          session_id: 'sess-current',
          agent: 'claude-code',
          model: 'claude-opus-5-5',
          progress: 'Removing the facts panel from the stage card',
        }),
      ],
    }),
    action: nextAction(`${taskId}-implement-1`),
    retained: async (timeline) => {
      const entry = timeline.locator('li article').filter({ hasText: 'Removing the facts panel from the stage card' })
      await expect(entry).toHaveCount(1)
      await expect(entry.getByText('Running', { exact: true })).toBeVisible()
      // Model and duration print in the entry's header line, with no footer.
      const header = entry.locator('[data-job-header]')
      await expect(entry.locator('footer')).toHaveCount(0)
      await expect(header.getByText('claude-opus-5-5', { exact: true })).toBeVisible()
      await expect(header.locator('[data-job-duration]')).toHaveText(elapsed)
    },
  },
  {
    entryKind: 'claimed order without a job',
    item: activity({
      work_orders: [
        order(`${taskId}-implement-2`, {
          attempt_id: 'attempt-current',
          claimed_by: 'run:usr_operator',
          agent: 'claude-code',
          model: 'claude-opus-5-5',
          last_agent_activity_at: minutes(7),
          last_agent_activity_label: 'Progress: removing the facts panel',
          progress: 'Removing the facts panel in full detail.',
        }),
      ],
    }),
    action: nextAction(`${taskId}-implement-2`),
    retained: async (timeline) => {
      const title = timeline.getByText('Implementation — in progress', { exact: true })
      await expect(title).toBeVisible()
      await expect(
        title.locator('..').getByText('run:usr_operator · claude-code · claude-opus-5-5', { exact: true }),
      ).toBeVisible()
      await expect(timeline.getByText('Agent activity — Progress: removing the facts panel')).toBeVisible()
    },
  },
  {
    entryKind: 'review seat',
    item: activity({
      latest_stage: 'review',
      work_orders: [1, 2].map((n) =>
        order(`${taskId}-review-1-seat-${n}`, {
          stage: 'review',
          job_id: `${taskId}-review-1-seat-${n}`,
          review_round: 1,
          review_seat: n,
          attempt_id: n === 1 ? 'attempt-current' : 'attempt-other',
          model: n === 1 ? 'gpt-6' : 'gpt-6-mini',
          agent: 'codex',
          progress: n === 1 ? 'Reading the timeline diff' : undefined,
        }),
      ),
    }),
    action: nextAction(`${taskId}-review-1-seat-1`, 'review'),
    retained: async (timeline) => {
      const seat = timeline.getByText('Seat 1', { exact: true }).locator('..')
      await expect(seat.getByText('gpt-6', { exact: true })).toBeVisible()
      await expect(seat.getByText('Deliberating')).toBeVisible()
      await expect(seat.getByText('Reading the timeline diff')).toBeVisible()
    },
  },
  {
    entryKind: 'claimed verification job',
    fullRouteOnly: true,
    item: activity({
      latest_stage: 'verify',
      jobs: [job(`${taskId}-verify-1`, 'verify', 'running')],
      work_orders: [
        order(`${taskId}-verify-1`, {
          stage: 'verify',
          attempt_id: 'attempt-current',
          agent: 'claude-code',
          model: 'claude-opus-5-5',
        }),
      ],
    }),
    action: nextAction(`${taskId}-verify-1`, 'verify'),
    retained: async (timeline) => {
      const entry = timeline.getByRole('article', { name: 'Verification' })
      await expect(entry).toBeVisible()
      const footer = entry.locator('footer')
      await expect(footer.getByText('claude-opus-5-5', { exact: true })).toBeVisible()
      await expect(footer.locator('[data-job-duration]')).toHaveText(elapsed)
    },
  },
]

for (const surface of detailSurfaces) {
  for (const { entryKind, item, action, fullRouteOnly, retained } of absenceCases) {
    if (fullRouteOnly && surface.name !== 'full route') continue
    test(`the ${surface.name} omits attempt facts for a ${entryKind} and retains its stage content`, async ({
      page,
    }) => {
      expect(action.attempt?.attempt_id).toBe('attempt-current')
      await mockAPI(page, item, action)
      await page.goto(surface.url)
      const timeline = page.getByRole('region', { name: 'Execution event timeline' })
      await expect(timeline).toBeVisible()
      await retained(timeline)
      await expect(page.getByRole('region', { name: 'Current attempt', exact: true })).toHaveCount(0)
    })
  }
}

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

// A plan stage shaped like task 261015-3721c6's: claimed at 06:31:37Z, lease
// renewed at 06:32:22Z, and the plan drafted and the job ended at 06:34:18Z
// (req-task-centric-operations-view AC-2.2; component-web-task-surfaces).
const planStart = '2026-10-15T06:31:37Z'
const planRenewed = '2026-10-15T06:32:22Z'
const planEnd = '2026-10-15T06:34:18Z'

function planStage(extra: { jobEnd?: string; draftedAt?: string } = {}) {
  const id = `${taskId}-spec-1`
  return activity({
    latest_stage: 'implement',
    jobs: [job(id, 'spec', 'done', { started_at: planStart, ended_at: extra.jobEnd ?? planEnd })],
    work_orders: [
      order(id, {
        stage: 'spec',
        state: 'completed',
        queue_entered_at: planStart,
        last_agent_activity_at: planRenewed,
        last_agent_activity_label: 'Claim lease renewed',
      }),
    ],
    events: [
      event(779742, 'work_order.claimed', planStart, { id, stage: 'spec' }),
      event(779755, 'work_order.lease_renewed', planRenewed, { lease_expires_at: '2026-10-15T07:31:37Z' }),
      event(779762, 'spec.version_created', extra.draftedAt ?? planEnd, { version: 1 }),
    ],
  })
}

const timelineOf = (item: object) => buildTimeline(item as ActivityItem)

function titleOf(entry: TimelineEntry) {
  if (entry.type === 'job') return `${entry.job.stage} job`
  return 'title' in entry ? entry.title : entry.type
}

const agentActivity = (entries: TimelineEntry[]) =>
  entries.filter((entry) => entry.type === 'note' && entry.title.startsWith('Agent activity'))

test('a completed plan entry sits at its end, after the plan version it drafted', () => {
  for (const drafted of [planEnd, '2026-10-15T06:34:17.900Z']) {
    const titles = timelineOf(planStage({ draftedAt: drafted })).map(titleOf)
    expect(titles).toEqual(['Plan v1 drafted', 'spec job'])
  }
  const [entry] = timelineOf(planStage()).filter((item) => item.type === 'job')
  expect(entry.at).toBe(planEnd)
})

test('a running job sits at its start, and a finished job without an end keeps its start', () => {
  const running = activity({ jobs: [job(`${taskId}-implement-1`, 'implement', 'running')] })
  expect(timelineOf(running).find((entry) => entry.type === 'job')?.at).toBe(minutes(6))
  const legacy = activity({ jobs: [job(`${taskId}-implement-1`, 'implement', 'failed')] })
  expect(timelineOf(legacy).find((entry) => entry.type === 'job')?.at).toBe(minutes(6))
  const failed = activity({
    jobs: [job(`${taskId}-implement-1`, 'implement', 'failed', { ended_at: minutes(9) })],
  })
  expect(timelineOf(failed).find((entry) => entry.type === 'job')?.at).toBe(minutes(9))
})

test('attempt bookkeeping and finished or superseded orders add no agent-activity note', () => {
  expect(agentActivity(timelineOf(planStage()))).toEqual([])
  for (const label of [
    'Work order claimed',
    'Claim lease renewed',
    'Attempt worktree checkpointed',
    'Usage telemetry reported',
    'Usage and rate-limit telemetry reported',
    'Transcript uploaded',
  ]) {
    const live = activity({
      work_orders: [
        order(`${taskId}-implement-1`, { last_agent_activity_at: minutes(7), last_agent_activity_label: label }),
      ],
    })
    expect(agentActivity(timelineOf(live)), label).toEqual([])
  }
  const progress = { last_agent_activity_at: minutes(7), last_agent_activity_label: 'Progress: wiring the timeline' }
  for (const state of ['submitted', 'completed', 'cancelled'] as const) {
    const finished = activity({ work_orders: [order(`${taskId}-implement-1`, { state, ...progress })] })
    expect(agentActivity(timelineOf(finished)), state).toEqual([])
  }
  const superseded = activity({
    work_orders: [
      order(`${taskId}-implement-1`, { state: 'stale', ...progress }),
      order(`${taskId}-implement-2`, { state: 'queued', queue_entered_at: minutes(30) }),
    ],
  })
  expect(agentActivity(timelineOf(superseded))).toEqual([])
})

test('a live order keeps its progress note and the full report', () => {
  for (const state of ['claimed', 'queued'] as const) {
    const live = activity({
      work_orders: [
        order(`${taskId}-implement-1`, {
          state,
          last_agent_activity_at: minutes(7),
          last_agent_activity_label: 'Progress: wiring the timeline',
          progress: 'Wiring the timeline in full detail.',
        }),
      ],
    })
    const notes = agentActivity(timelineOf(live))
    expect(notes).toHaveLength(1)
    expect(notes[0]).toMatchObject({
      at: minutes(7),
      title: 'Agent activity — Progress: wiring the timeline',
      fullProgress: 'Wiring the timeline in full detail.',
    })
  }
})

for (const surface of surfaces) {
  test(`the ${surface.name} lists the completed plan after its draft and keeps lease renewals technical`, async ({
    page,
  }) => {
    const item = planStage()
    await mockAPI(page, item, {
      ...nextAction(`${taskId}-implement-1`),
      current_order: undefined,
      attempt: undefined,
    })
    await page.route(`**/v1/tasks/${taskId}/history*`, (route) =>
      route.fulfill({
        json: {
          selection: 'technical',
          limit: 50,
          events: item.events.map((entry) => ({ ...(entry as object), id: String((entry as { id: number }).id) })),
          boundary: { max_id: '779762', count: 3 },
          has_older: false,
          has_newer: false,
        },
      }),
    )
    await page.goto(surface.url)
    const timeline = page.getByRole('region', { name: 'Execution event timeline' })
    const drafted = timeline.getByText('Plan v1 drafted')
    const completed = timeline.getByText('Plan completed')
    await expect(drafted).toBeVisible()
    await expect(completed).toBeVisible()
    const follows = await drafted.evaluate(
      (element, other) => Boolean(element.compareDocumentPosition(other as Node) & Node.DOCUMENT_POSITION_FOLLOWING),
      await completed.elementHandle(),
    )
    expect(follows).toBe(true)
    const row = completed.locator('..')
    await expect(row.getByText('2m 41s')).toBeVisible()
    const endTime = await page.evaluate(
      (value) =>
        new Date(value).toLocaleString('en', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }),
      planEnd,
    )
    await expect(row.locator('time')).toHaveText(endTime)
    await expect(timeline.getByText(/Claim lease renewed/)).toHaveCount(0)
    await page.getByRole('button', { name: 'Show technical activity' }).click()
    await expect(
      page.getByRole('list', { name: 'Technical activity events' }).getByText('work_order.lease_renewed'),
    ).toBeVisible()
  })
}

test.describe('at 390 px', () => {
  test.use({ viewport: { width: 390, height: 844 } })

  for (const surface of surfaces) {
    test(`the ${surface.name} running job entry has no horizontal scroll`, async ({ page }) => {
      // The next action still carries an attempt; the entry renders without it.
      await mockAPI(page, jobItem, nextAction(`${taskId}-implement-1`))
      await page.goto(surface.url)
      const timeline = page.getByRole('region', { name: 'Execution event timeline' })
      await expect(timeline).toBeVisible()
      const running = timeline.locator('li article').filter({ hasText: 'Running' })
      await expect(running).toHaveCount(1)
      await expect(running).toBeVisible()
      const card = await running.evaluate((element) => [element.scrollWidth, element.clientWidth])
      expect(card[0]).toBeLessThanOrEqual(card[1])
      const viewport = await page.evaluate(() => [
        document.documentElement.scrollWidth,
        document.documentElement.clientWidth,
      ])
      expect(viewport[0]).toBeLessThanOrEqual(viewport[1])
    })
  }
})
