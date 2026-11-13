import { expect, type Locator, type Page, type TestInfo, test } from '@playwright/test'
import { buildTimeline, type TimelineEntry } from '../src/lib/activity'
import type { ActivityItem, NextAction, WorkOrder } from '../src/lib/types'
import { callerIdentity } from './helpers/caller-identity'
import { activityFrame, controlTaskStreams } from './helpers/task-streams'

// Task detail tells the task's story through its timeline
// (req-operator-next-action REQ-7; feature-operator-next-action NA-6;
// component-web-task-surfaces). The Timeline tab is the default below the
// next-action card and reads no flow (AC-7.4; the Flow tab is covered by
// task-flow.spec.ts), no entry renders a Current attempt facts region, each
// stage entry keeps its own content, and parked, closed, automatic retries,
// and bounces read as entries. A dependency wait, a merge request, and its
// GitHub confirmation render no entry (task 261108-38aabb).

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
    if (path === '/v1/activity')
      return route.fulfill({
        json: [{ ...item, next_action: { ...action, choices: undefined } }],
        headers: { 'X-Conveyor-Total': '1', 'X-Conveyor-Limit': '100', 'X-Conveyor-Offset': '0' },
      })
    return route.fulfill({ json: [] })
  })
  return counts
}

const surfaces = [
  { name: 'Board sheet', url: `/w/demo/tasks/${taskId}` },
  { name: 'full task route', url: `/w/demo/tasks/${taskId}/full` },
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
  { name: 'Board sheet', url: `/w/demo/tasks/${taskId}` },
  { name: 'Tasks panel', url: `/w/demo/tasks?task=${taskId}` },
  { name: 'full route', url: `/w/demo/tasks/${taskId}/full` },
] as const

const elapsed = /^\d+(h \d{2}m|m \d{2}s|s)$/
const shortTime = /^[A-Z][a-z]{2} \d{1,2}, \d{1,2}:\d{2}\s?[AP]M$/

// A running job's footer shows "Running for …" and no clock time. The start
// sits behind the runtime, which focus opens (DEC-127(1)).
async function expectRunningTime(footer: Locator) {
  const time = footer.locator('[data-job-time]')
  await expect(time).toHaveText(/^Running for \d+(h \d{2}m|m \d{2}s|s)$/)
  await expect(time.locator('[data-job-duration]')).toHaveText(elapsed)
  await expect(time).toHaveAttribute('data-runtime-live', '')
  await expect(time).toHaveAccessibleName(/^Running for \S+( \S+)?\. Started [A-Z][a-z]{2} \d{1,2}, .*[AP]M\.$/)
  await time.focus()
  const details = footer.locator('[data-job-time-details]')
  await expect(details.locator('[data-job-started]')).toHaveText(
    /^Started [A-Z][a-z]{2} \d{1,2}, \d{1,2}:\d{2}\s?[AP]M$/,
  )
  await expect(details.locator('[data-job-running]')).toHaveText('Still running')
  await expect(details.locator('[data-job-finished]')).toHaveCount(0)
  await time.blur()
}

// A finished job's footer shows its duration and no clock time. The start
// and the finish sit behind it, which focus opens (DEC-127(1)).
async function expectFinishedTime(footer: Locator) {
  const time = footer.locator('[data-job-time]')
  await expect(time).toHaveText(elapsed)
  await expect(time.locator('[data-job-duration]')).toHaveText(elapsed)
  await expect(time).not.toHaveAttribute('data-runtime-live')
  await expect(time).toHaveAccessibleName(/^Took \S+( \S+)?\. Started .*[AP]M\. Finished .*[AP]M\.$/)
  await time.focus()
  const details = footer.locator('[data-job-time-details]')
  await expect(details.locator('[data-job-started]')).toHaveText(
    /^Started [A-Z][a-z]{2} \d{1,2}, \d{1,2}:\d{2}\s?[AP]M$/,
  )
  await expect(details.locator('[data-job-finished]')).toHaveText(
    /^Finished [A-Z][a-z]{2} \d{1,2}, \d{1,2}:\d{2}\s?[AP]M$/,
  )
  await expect(details.locator('[data-job-running]')).toHaveCount(0)
  await time.blur()
}

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
      // The header holds the stage and state. The footer holds the model and
      // the runtime, and the start time sits behind the runtime.
      const header = entry.locator('[data-job-header]')
      await expect(header.getByText('claude-opus-5-5', { exact: true })).toHaveCount(0)
      await expect(header.locator('[data-job-duration]')).toHaveCount(0)
      await expect(entry.locator('[data-job-duration]')).toHaveCount(1)
      const footer = entry.locator('[data-job-footer]')
      await expect(footer.locator('[data-job-model]').getByText('claude-opus-5-5', { exact: true })).toBeVisible()
      await expectRunningTime(footer)
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
      const entry = timeline.locator('li[data-order-entry="claimed"]')
      await expect(entry.getByText('Implementation', { exact: true })).toBeVisible()
      await expect(entry.getByText('In progress', { exact: true })).toBeVisible()
      await expect(entry.getByText('run:usr_operator · claude-code · claude-opus-5-5', { exact: true })).toBeVisible()
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

// A dependency wait reads from the Dependencies section, the Board card's
// dependency mark, and the flowchart's waiting side state, so the timeline
// renders no entry for it (req-operator-next-action AC-6.11, AC-3.4, AC-7.3,
// AC-7.5).
test('automatic retries, bounces, parking, and closing read as timeline entries and dependency waits do not', async ({
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
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('The agent’s run failed — retrying')).toHaveCount(1)
  await expect(timeline.getByText('The agent’s run failed — not retried automatically')).toHaveCount(0)
  await expect(timeline.getByText('Sent back to implementation')).toBeVisible()
  await expect(timeline.getByText('Bounce 2')).toBeVisible()
  await expect(timeline.getByText('Parked', { exact: true })).toBeVisible()
  await expect(timeline.getByText('Triage parked the task for an operator decision')).toBeVisible()
  await expect(timeline.getByText('Closed — started over as a new task')).toBeVisible()
  await expect(timeline.getByText('Closed', { exact: true })).toHaveCount(0)
  await expect(timeline.getByText('Waiting on another task')).toHaveCount(0)
  await expect(timeline.getByText('task-prereq')).toHaveCount(0)
})

// A task-context decision shows as the document under Attached context, so the
// human timeline renders no note for it, with or without target fields and
// with an omitted payload; technical activity keeps the recorded events
// (component-web-task-surfaces, Task detail composition).
test('task-context decisions render no timeline note', async ({ page }) => {
  const item = activity({
    events: [
      event(1, 'task.context_proposal_confirmed', minutes(1), {
        target_kind: 'requirement',
        target_id: 'req-review-gates-evidence',
        requirement_id: 'req-review-gates-evidence',
      }),
      event(2, 'task.context_proposal_dismissed', minutes(2), {
        target_kind: 'system_design',
        target_id: 'component-web-dashboard',
      }),
      event(3, 'task.context_proposal_confirmed', minutes(3), {}),
      {
        ...event(4, 'task.context_proposal_dismissed', minutes(4), {}),
        payload: null,
        payload_omitted: true,
        payload_bytes: 9000,
        audit_available: true,
      },
      event(5, 'task.dependency_added', minutes(5), { depends_on_task_id: 'task-dependency' }),
      event(6, 'task.dependency_removed', minutes(6), { reason: 'operator removed the dependency' }),
    ],
  })
  await mockAPI(page, item, nextAction(`${taskId}-implement-1`))
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('Dependency removed')).toHaveCount(0)
  await expect(timeline.getByText('operator removed the dependency')).toHaveCount(0)
  await expect(timeline.getByText('Waiting on another task')).toHaveCount(0)
  await expect(timeline.getByText('task-dependency')).toHaveCount(0)
  await expect(timeline).not.toContainText('Context confirmed')
  await expect(timeline).not.toContainText('Context dismissed')
  await expect(timeline.getByText('Details omitted from this page')).toHaveCount(0)
  await expect(timeline).not.toContainText('undefined')
})

// A dependency removal, like a dependency addition, reads from the
// Dependencies section, the Board card's dependency mark, and the flowchart's
// waiting side state, so the timeline renders no row for any removal: neither
// the removal half of a rewire nor a lone removal. A dependency that closed
// without merging stays an alarm, and technical activity keeps every recorded
// removal (req-operator-next-action AC-6.11, AC-3.4, AC-7.5;
// component-web-task-surfaces, One human list; task 261112-436fab).
const dependencyRemovals = () =>
  activity({
    events: [
      event(1, 'task.dependency_removed', minutes(1), {
        depends_on_task_id: 'task-first',
        reason: 'the operator dropped the first dependency',
      }),
      event(2, 'task.dependency_added', minutes(2), { depends_on_task_id: 'task-second-successor' }),
      event(3, 'task.dependency_removed', minutes(2), {
        depends_on_task_id: 'task-second',
        reason: 'replace dependency task-second with successor task-second-successor',
      }),
      event(4, 'task.dependency_added', minutes(2), { depends_on_task_id: 'task-third-successor' }),
      event(5, 'task.dependency_removed', minutes(2), {
        depends_on_task_id: 'task-third',
        reason: 'replace dependency task-third with successor task-third-successor',
      }),
      event(6, 'task.dependency_removed', minutes(3), { depends_on_task_id: 'task-fourth' }),
      event(7, 'task.dependency_unsatisfiable', minutes(4), { depends_on_task_id: 'task-closed' }),
    ],
  })

async function openDependencyRemovals(page: Page) {
  const item = dependencyRemovals()
  await mockAPI(page, item, nextAction(`${taskId}-implement-1`))
  await page.route(`**/v1/tasks/${taskId}/history*`, (route) =>
    route.fulfill({
      json: {
        selection: 'technical',
        limit: 50,
        events: (item.events as { id: number }[]).map((entry) => ({ ...entry, id: String(entry.id) })),
        boundary: { max_id: '7', count: item.events.length },
        has_older: false,
        has_newer: false,
      },
    }),
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('Dependency needs attention')).toBeVisible()
  return timeline
}

test('dependency removals render no timeline row and stay in technical activity', async ({ page }) => {
  const timeline = await openDependencyRemovals(page)
  await expect(timeline.getByText('task-closed closed without merging')).toBeVisible()
  await expect(timeline.getByText('Dependency removed')).toHaveCount(0)
  await expect(timeline.getByText(/dependency task-(second|third) with successor/)).toHaveCount(0)
  await expect(timeline.getByText('the operator dropped the first dependency')).toHaveCount(0)
  await expect(timeline).not.toContainText('undefined')
  await page.getByRole('button', { name: 'Show technical activity' }).click()
  const technical = page.getByRole('list', { name: 'Technical activity events' })
  await expect(technical.getByText('task.dependency_removed', { exact: true })).toHaveCount(4)
  await expect(technical.getByText('task.dependency_unsatisfiable', { exact: true })).toBeVisible()
})

// Visual record for the pull request: the human timeline and technical
// activity of a task with dependency removals, at 375 px and a desktop width.
for (const width of [375, 1280]) {
  test(`dependency removal screenshots at ${width} px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 1400 })
    await openDependencyRemovals(page)
    await page.evaluate(() => document.fonts.ready)
    await capture(page, testInfo, `dependency-removals-timeline-${width}.png`)
    await page.getByRole('button', { name: 'Show technical activity' }).click()
    const technical = page.getByRole('list', { name: 'Technical activity events' })
    await expect(technical.getByText('task.dependency_removed', { exact: true })).toHaveCount(4)
    await capture(page, testInfo, `dependency-removals-technical-${width}.png`)
  })
}

// A merge request and its GitHub confirmation only repeat the task's finished
// state, so the timeline renders no entry for either, with or without an
// omitted payload, while an out-of-pipeline merge the system reconciled still
// reads as an entry; technical activity keeps all three recorded events
// (component-web-task-surfaces, One human list; task 261108-38aabb).
test('merge requests and confirmations render no timeline entry while a reconciled merge does', async ({ page }) => {
  const mergeURL = 'https://github.com/kidus-tiliksew/conveyor/pull/1600'
  const item = activity({
    task: { ...task, state: 'merged' },
    events: [
      event(1, 'merge.requested', minutes(1), { url: mergeURL }),
      event(2, 'merge.confirmed', minutes(2), { url: mergeURL }),
      {
        ...event(3, 'merge.confirmed', minutes(3), {}),
        payload: null,
        payload_omitted: true,
        payload_bytes: 9000,
        audit_available: true,
      },
      event(4, 'merge.reconciled', minutes(4), { url: mergeURL, result: 'merged_outside_conveyor' }),
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
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('Already-merged pull request reconciled')).toBeVisible()
  await expect(timeline.getByText('Pull request merge requested')).toHaveCount(0)
  await expect(timeline.getByText('Pull request merge confirmed by GitHub')).toHaveCount(0)
  await expect(timeline.getByText('Details omitted from this page')).toHaveCount(0)
  await expect(timeline.getByText(/merge (requested|confirmed)/i)).toHaveCount(0)
  await page.route(`**/v1/tasks/${taskId}/history*`, (route) =>
    route.fulfill({
      json: {
        selection: 'technical',
        limit: 50,
        events: (item.events as { id: number }[]).map((entry) => ({ ...entry, id: String(entry.id) })),
        boundary: { max_id: '4', count: item.events.length },
        has_older: false,
        has_newer: false,
      },
    }),
  )
  await page.getByRole('button', { name: 'Show technical activity' }).click()
  const technical = page.getByRole('list', { name: 'Technical activity events' })
  for (const kind of ['merge.requested', 'merge.confirmed', 'merge.reconciled'])
    await expect(technical.getByText(kind, { exact: true }).first()).toBeVisible()
})

// A plan stage shaped like task 261015-3721c6's: claimed at 06:31:37Z, lease
// renewed at 06:32:22Z, and the plan drafted and the job ended at 06:34:18Z
// (req-operator-next-action AC-6.10; component-web-task-surfaces).
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

// The plan section and the approval intervention tell a plan stage, so its
// "Plan vN drafted" note stays technical (component-web-task-surfaces, Task
// detail composition; task 261028-5c2ae2).
test('a completed plan entry sits at its end and its plan version note stays out of the story', () => {
  for (const drafted of [planEnd, '2026-10-15T06:34:17.900Z']) {
    const titles = timelineOf(planStage({ draftedAt: drafted })).map(titleOf)
    expect(titles).toEqual(['spec job'])
  }
  const [entry] = timelineOf(planStage()).filter((item) => item.type === 'job')
  expect(entry.at).toBe(planEnd)
})

// An implementation that records an unabsorbed note at the instant it ends
// follows that note: an order without a head shows no delivery line, so its
// pull request keeps its own note (component-web-task-surfaces).
test('a finished implementation entry follows the note it recorded at the same instant', () => {
  const id = `${taskId}-implement-1`
  for (const opened of [planEnd, '2026-10-15T06:34:17.900Z']) {
    const item = activity({
      jobs: [job(id, 'implement', 'done', { started_at: planStart, ended_at: planEnd })],
      work_orders: [order(id, { state: 'submitted', queue_entered_at: planStart })],
      events: [{ ...event(779762, 'pull_request.opened', opened, { number: 7, url: prURL }), job_id: id }],
      summaries: { pull_request: { url: prURL, number: 7, event_id: '779762', at: opened } },
    })
    expect(timelineOf(item).map(titleOf)).toEqual(['Pull request opened', 'implement job'])
  }
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

// A live order whose progress report is printed as its running job entry's
// summary: the report reads once, in the job entry.
const runningJobId = `${taskId}-implement-1`
const runningReport = 'Round 1 fixes are pushed; waiting for exact-head CI.'
const runningProgress = {
  last_agent_activity_at: minutes(7),
  last_agent_activity_label: 'Progress: Round 1 fixes are pushed',
  progress: runningReport,
}
const progressShownItem = activity({
  jobs: [job(runningJobId, 'implement', 'running')],
  work_orders: [
    order(runningJobId, {
      attempt_id: 'attempt-current',
      session_id: 'sess-current',
      agent: 'claude-code',
      model: 'claude-opus-5-5',
      ...runningProgress,
    }),
  ],
})

test('a running job entry that prints its order progress adds no progress note for that order', () => {
  const entries = timelineOf(progressShownItem)
  const jobEntry = entries.find((entry) => entry.type === 'job')
  expect(jobEntry).toMatchObject({ summary: runningReport })
  expect(agentActivity(entries)).toEqual([])
})

test('a progress note stays wherever no job entry prints that order progress', () => {
  const note = (entries: TimelineEntry[]) => agentActivity(entries).map((entry) => [entry.at, titleOf(entry)])
  const expected = [[minutes(7), 'Agent activity — Progress: Round 1 fixes are pushed']]
  // A claimed order without a job, and a queued order.
  for (const state of ['claimed', 'queued'] as const) {
    const jobless = activity({ work_orders: [order(runningJobId, { state, ...runningProgress })] })
    expect(note(timelineOf(jobless)), state).toEqual(expected)
    expect(agentActivity(timelineOf(jobless))[0]).toMatchObject({ fullProgress: runningReport })
  }
  // A job that has not started renders no job entry.
  const pending = activity({
    jobs: [job(runningJobId, 'implement', 'pending', { started_at: undefined })],
    work_orders: [order(runningJobId, runningProgress)],
  })
  expect(note(timelineOf(pending))).toEqual(expected)
  // A job entry keeping its own narration does not print the progress.
  const narrated = activity({
    jobs: [job(runningJobId, 'implement', 'running')],
    work_orders: [order(runningJobId, runningProgress)],
    summaries: { job_results: { [runningJobId]: { summary: { text: 'Harness narration of the attempt.' } } } },
  })
  const narratedEntries = timelineOf(narrated)
  expect(narratedEntries.find((entry) => entry.type === 'job')).toMatchObject({
    summary: 'Harness narration of the attempt.',
  })
  expect(note(narratedEntries)).toEqual(expected)
  expect(agentActivity(narratedEntries)[0]).toMatchObject({ fullProgress: runningReport })
})

test('suppression covers only Progress labels of the order whose job entry prints the progress', () => {
  // A non-bookkeeping label that is not a progress report keeps its note.
  const pushed = activity({
    jobs: [job(runningJobId, 'implement', 'running')],
    work_orders: [order(runningJobId, { ...runningProgress, last_agent_activity_label: 'Branch pushed' })],
  })
  expect(agentActivity(timelineOf(pushed)).map(titleOf)).toEqual(['Agent activity — Branch pushed'])
  // Another live order without a job entry keeps its own progress note.
  const planOrderId = `${taskId}-spec-1`
  const twoOrders = activity({
    jobs: [job(runningJobId, 'implement', 'running')],
    work_orders: [
      order(runningJobId, runningProgress),
      order(planOrderId, {
        stage: 'spec',
        last_agent_activity_at: minutes(8),
        last_agent_activity_label: 'Progress: drafting the plan',
        progress: 'Drafting the plan in full detail.',
      }),
    ],
  })
  const notes = agentActivity(timelineOf(twoOrders))
  expect(notes.map(titleOf)).toEqual(['Agent activity — Progress: drafting the plan'])
  expect(notes[0]).toMatchObject({
    key: `agent-activity-${planOrderId}`,
    fullProgress: 'Drafting the plan in full detail.',
  })
})

for (const surface of surfaces) {
  test(`the ${surface.name} prints a running job's progress report once, with no agent-activity row`, async ({
    page,
  }) => {
    await mockAPI(page, progressShownItem, nextAction(runningJobId))
    await page.goto(surface.url)
    const timeline = page.getByRole('region', { name: 'Execution event timeline' })
    const entry = timeline.locator('li article').filter({ hasText: runningReport })
    await expect(entry).toHaveCount(1)
    await expect(entry.getByText('Running', { exact: true })).toBeVisible()
    await expect(timeline.getByText(runningReport)).toHaveCount(1)
    await expect(timeline.getByText(/Agent activity/)).toHaveCount(0)
    await expect(timeline.getByText('Show full progress report')).toHaveCount(0)
  })
}

for (const surface of surfaces) {
  test(`the ${surface.name} shows no row for a placeholder plan and keeps its draft and lease renewals technical`, async ({
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
    await expect(timeline.getByText('Activity', { exact: true })).toBeVisible()
    await expect(timeline.getByText('Plan v1 drafted')).toHaveCount(0)
    await expect(timeline.getByText('Plan completed')).toHaveCount(0)
    await expect(timeline.getByText(/Claim lease renewed/)).toHaveCount(0)
    await page.getByRole('button', { name: 'Show technical activity' }).click()
    const technical = page.getByRole('list', { name: 'Technical activity events' })
    await expect(technical.getByText('work_order.lease_renewed')).toBeVisible()
    await expect(technical.getByText('spec.version_created')).toBeVisible()
  })
}

// A narrated, failed, or rejected plan job keeps its job entry; only the
// finished placeholder row is gone, and other stages keep their one-line row.
test('narrated, failed, and rejected plan jobs keep their entries and other stages keep the completed row', async ({
  page,
}) => {
  const spec = (n: number) => `${taskId}-spec-${n}`
  const item = activity({
    jobs: [
      job(spec(1), 'spec', 'done', { started_at: minutes(1), ended_at: minutes(2) }),
      job(spec(2), 'spec', 'failed', { started_at: minutes(3), ended_at: minutes(4) }),
      job(spec(3), 'spec', 'done', { started_at: minutes(5), ended_at: minutes(6) }),
      job(spec(4), 'spec', 'done', { started_at: minutes(7), ended_at: minutes(8) }),
      job(`${taskId}-triage-1`, 'triage', 'done', { started_at: minutes(0), ended_at: minutes(1) }),
    ],
    summaries: {
      job_results: {
        [spec(1)]: { stage_summary: { text: 'Plan v1 splits the card from the history change.', event_id: '11' } },
        [spec(2)]: { failure_error: { text: 'The planner exited before submitting.', event_id: '12' } },
        [spec(3)]: { output_rejection: { event_id: '13', error: 'plan missing done criteria' } },
      },
    },
  })
  await mockAPI(page, item, { ...nextAction(`${taskId}-implement-1`), current_order: undefined, attempt: undefined })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('Plan v1 splits the card from the history change.')).toBeVisible()
  await expect(timeline.getByText('The planner exited before submitting.')).toBeVisible()
  await expect(timeline.getByText(/Output rejected by the pipeline — plan missing done criteria/)).toBeVisible()
  await expect(timeline.getByText('Triage completed')).toBeVisible()
  await expect(timeline.getByText('Plan completed')).toHaveCount(0)
  await expect(timeline.locator('li article')).toHaveCount(3)
})

// Each started implement job renders as its own Implementation card, the way
// verify and review do: its status, its attempt number when the task has more
// than one, the agent's text, and the pull request with the head its own order
// submitted. A pull_request.opened the card shows is absorbed; the plan
// version and publication bookkeeping notes stay technical
// (component-web-task-surfaces v41, Work-order timeline entries;
// component-http-api v62, Bounded detail view; task 261028-5c2ae2).
const prURL = 'https://github.com/kidus-tiliksew/conveyor/pull/1431'
const firstHead = '4a136fe1e6efae140758830baacedef02f1b76d8'
const secondHead = '61b4fa3ed1dac18b6f36a8ac756e198ac6b1e411'
const implementID = (n: number) => `${taskId}-implement-${n}`

function deliveredTask(extraEvents: object[] = []) {
  return activity({
    task: { ...task, state: 'merged' },
    latest_stage: 'merge',
    jobs: [
      // Response order is not attempt order.
      job(implementID(2), 'implement', 'done', { started_at: minutes(40), ended_at: minutes(60) }),
      job(`${taskId}-spec-1`, 'spec', 'done', { started_at: minutes(1), ended_at: minutes(3) }),
      job(implementID(1), 'implement', 'done', { started_at: minutes(5), ended_at: minutes(30) }),
    ],
    work_orders: [
      order(implementID(1), {
        state: 'cancelled',
        head_sha: firstHead,
        agent: 'claude-code',
        model: 'claude-opus-5-5',
        progress: 'Submitted at head 4a136fe1 (PR #1431).',
      }),
      order(implementID(2), {
        state: 'cancelled',
        queue_entered_at: minutes(35),
        head_sha: secondHead,
        agent: 'claude-code',
        model: 'claude-opus-5-5',
        progress: 'Resubmitted at head 61b4fa3e (PR #1431).',
      }),
    ],
    events: [
      event(10, 'spec.version_created', minutes(3), { version: 1 }),
      event(11, 'github_issue.publication_queued', minutes(4), {}),
      event(12, 'github_issue.associated', minutes(4), {
        issue_url: 'https://github.com/kidus-tiliksew/conveyor/issues/9',
      }),
      event(13, 'github_issue.publication_published', minutes(4), {
        issue_url: 'https://github.com/kidus-tiliksew/conveyor/issues/9',
      }),
      {
        ...event(20, 'pull_request.opened', minutes(30), { url: prURL, number: 1431, head_sha: firstHead }),
        job_id: implementID(1),
      },
      event(21, 'review.publication_retry', minutes(31), { attempt: 1 }),
      {
        ...event(30, 'pull_request.opened', minutes(60), { url: prURL, number: 1431, head_sha: secondHead }),
        job_id: implementID(2),
      },
      event(31, 'review.publication_retry', minutes(61), { attempt: 1 }),
      ...extraEvents,
    ],
    summaries: {
      pull_request: {
        url: prURL,
        number: 1431,
        repository: 'kidus-tiliksew/conveyor',
        head_sha: secondHead,
        event_id: '30',
        at: minutes(60),
      },
    },
  })
}

const removedTitles = [
  'Plan v1 drafted',
  'Plan completed',
  'GitHub issue publication queued',
  'GitHub issue associated',
  'Implementation completed',
  'GitHub review publication retrying',
  'Pull request opened',
]

test('implement attempts are numbered by start time with their own delivery heads', () => {
  const entries = timelineOf(deliveredTask())
  const implement = entries.filter((entry) => entry.type === 'job' && entry.job.stage === 'implement')
  expect(implement.map((entry) => entry.type === 'job' && [entry.job.id, entry.attempt, entry.delivery])).toEqual([
    [implementID(1), 1, { number: 1431, url: prURL, headSha: firstHead }],
    [implementID(2), 2, { number: 1431, url: prURL, headSha: secondHead }],
  ])
  const notes = entries.filter((entry) => entry.type === 'note').map(titleOf)
  for (const title of removedTitles) expect(notes).not.toContain(title)
})

test('a pull request record stays a note unless its own card shows that pull request', () => {
  const unmatched = [
    // Another pull request number on a delivered job.
    { ...event(40, 'pull_request.opened', minutes(62), { url: `${prURL}0`, number: 14310 }), job_id: implementID(2) },
    // No job, so no card can show it.
    event(41, 'pull_request.opened', minutes(63), { url: prURL, number: 1431 }),
    // An omitted payload whose index carries no number.
    {
      ...event(42, 'pull_request.opened', minutes(64), {}),
      job_id: implementID(2),
      payload: null,
      payload_omitted: true,
      payload_bytes: 70000,
      audit_available: true,
    },
    // An omitted payload whose index names the shown number is absorbed.
    {
      ...event(43, 'pull_request.opened', minutes(65), {}),
      job_id: implementID(1),
      payload: null,
      payload_omitted: true,
      payload_bytes: 70000,
      payload_index: { number: 1431 },
      audit_available: true,
    },
  ]
  const notes = timelineOf(deliveredTask(unmatched)).filter((entry) => entry.type === 'note')
  expect(notes.map((entry) => entry.type === 'note' && [entry.key, entry.title])).toEqual([
    ['event-40', 'Pull request opened'],
    ['event-41', 'Pull request opened'],
    ['event-42', 'Pull request opened'],
  ])
  // A single attempt carries no attempt number.
  const single = activity({
    jobs: [job(implementID(1), 'implement', 'done', { ended_at: minutes(9) })],
    work_orders: [order(implementID(1), { state: 'submitted', head_sha: firstHead })],
  })
  const [entry] = timelineOf(single).filter((item) => item.type === 'job')
  expect(entry.type === 'job' && [entry.attempt, entry.delivery]).toEqual([undefined, undefined])
})

for (const surface of surfaces) {
  test(`the ${surface.name} renders each implementation attempt as a card with its pull request and head`, async ({
    page,
  }) => {
    const item = deliveredTask([event(50, 'github_issue.publication_failed', minutes(70), { last_error: 'forbidden' })])
    await mockAPI(page, item, {
      ...nextAction(implementID(2)),
      label: 'Finished',
      actor: 'none',
      kind: 'terminal',
      current_order: undefined,
      attempt: undefined,
    })
    await page.goto(surface.url)
    const timeline = page.getByRole('region', { name: 'Execution event timeline' })
    const first = timeline.getByRole('article', { name: 'Implementation attempt 1' })
    const second = timeline.getByRole('article', { name: 'Implementation attempt 2' })
    await expect(first).toBeVisible()
    await expect(second).toBeVisible()
    for (const [card, text, head] of [
      [first, 'Submitted at head 4a136fe1 (PR #1431).', '4a136fe'],
      [second, 'Resubmitted at head 61b4fa3e (PR #1431).', '61b4fa3'],
    ] as const) {
      await expect(card.getByText('Submitted', { exact: true })).toBeVisible()
      await expect(card.getByText(text)).toBeVisible()
      const header = card.locator('[data-job-header]')
      await expect(header.getByText('claude-opus-5-5', { exact: true })).toHaveCount(0)
      await expect(card.getByText(/^Attempt \d/)).toHaveCount(0)
      const footer = card.locator('[data-job-footer]')
      await expect(footer.locator('[data-job-model]').getByText('claude-opus-5-5', { exact: true })).toBeVisible()
      await expectFinishedTime(footer)
      // The header carries the attempt's own pull request. The chip names the
      // full head at every width and shows seven characters on a wide card.
      const fullHead = head === '4a136fe' ? firstHead : secondHead
      const delivery = header.locator('[data-implementation-delivery]')
      await expect(delivery).toHaveAttribute('href', prURL)
      await expect(delivery).toHaveAccessibleName(`Pull request #1431, head ${fullHead}`)
      await expect(delivery).toHaveAttribute('title', `Pull request #1431, head ${fullHead}`)
      await expect(delivery).toContainText('#1431')
      const wide = (await card.evaluate((element) => element.getBoundingClientRect().width)) >= 448
      const shortHead = delivery.locator('[data-implementation-head]')
      if (wide) await expect(shortHead).toHaveText(` · ${head}`)
      else await expect(shortHead).toBeHidden()
    }
    await expect(second.locator('[data-implementation-title]')).toHaveText('Implementation 2')
    // Attempt 1 precedes attempt 2 whatever the response order.
    const follows = await first.evaluate(
      (element, other) => Boolean(other && element.compareDocumentPosition(other) & Node.DOCUMENT_POSITION_FOLLOWING),
      await second.elementHandle(),
    )
    expect(follows).toBe(true)
    for (const title of removedTitles) await expect(timeline.getByText(title, { exact: true })).toHaveCount(0)
    await expect(timeline.getByText('GitHub issue publication failed')).toBeVisible()
    // The hidden kinds stay readable in technical activity.
    await page.route(`**/v1/tasks/${taskId}/history*`, (route) =>
      route.fulfill({
        json: {
          selection: 'technical',
          limit: 50,
          events: (item.events as { id: number }[]).map((entry) => ({ ...entry, id: String(entry.id) })),
          boundary: { max_id: '50', count: item.events.length },
          has_older: false,
          has_newer: false,
        },
      }),
    )
    await page.getByRole('button', { name: 'Show technical activity' }).click()
    const technical = page.getByRole('list', { name: 'Technical activity events' })
    for (const kind of ['spec.version_created', 'github_issue.associated', 'review.publication_retry'])
      await expect(technical.getByText(kind).first()).toBeVisible()
  })
}

const implementationCases = [
  {
    name: 'running',
    jobState: 'running',
    extra: {},
    order: { state: 'claimed', progress: 'Writing the implementation card.' },
    badge: 'Running',
    text: 'Writing the implementation card.',
    delivery: false,
  },
  {
    name: 'failed',
    jobState: 'failed',
    extra: { ended_at: minutes(9) },
    order: { state: 'cancelled' },
    results: { failure_error: { text: 'The agent exited 1 before submitting.', event_id: '61' } },
    badge: 'Failed',
    text: 'The agent exited 1 before submitting.',
    delivery: false,
  },
  {
    name: 'queued',
    jobState: 'pending',
    extra: {},
    order: { state: 'claimed' },
    badge: 'Queued',
    text: 'Queued for an operator-owned agent over MCP.',
    delivery: false,
  },
  {
    name: 'rejected output',
    jobState: 'done',
    extra: { ended_at: minutes(9) },
    order: { state: 'submitted', head_sha: firstHead },
    results: { output_rejection: { event_id: '62', error: 'missing head' } },
    badge: 'Submitted',
    text: 'Output rejected by the pipeline — missing head. Retrying with this feedback.',
    delivery: true,
    warning: true,
  },
  {
    name: 'no pull request',
    jobState: 'done',
    extra: { ended_at: minutes(9) },
    order: { state: 'submitted', head_sha: firstHead, progress: 'Submitted without a forge.' },
    noPullRequest: true,
    badge: 'Submitted',
    text: 'Submitted without a forge.',
    delivery: false,
  },
  {
    name: 'no head',
    jobState: 'done',
    extra: { ended_at: minutes(9) },
    order: { state: 'submitted', progress: 'Submitted before heads were recorded.' },
    badge: 'Submitted',
    text: 'Submitted before heads were recorded.',
    delivery: false,
  },
  {
    name: 'shortened summary',
    jobState: 'done',
    extra: { ended_at: minutes(9) },
    order: { state: 'submitted', head_sha: firstHead },
    results: { summary: { text: 'Implemented the **card** and its tests.', event_id: '63', truncated: true } },
    badge: 'Submitted',
    text: 'Implemented the card and its tests.',
    delivery: true,
  },
] as const

for (const scenario of implementationCases) {
  test(`a ${scenario.name} implementation renders as one card without an attempt number`, async ({ page }) => {
    const id = implementID(1)
    const item = activity({
      jobs: [job(id, 'implement', scenario.jobState, scenario.extra)],
      work_orders: [order(id, { agent: 'claude-code', model: 'claude-opus-5-5', ...scenario.order })],
      summaries: {
        ...('noPullRequest' in scenario
          ? {}
          : { pull_request: { url: prURL, number: 1431, head_sha: firstHead, event_id: '1', at: minutes(9) } }),
        ...('results' in scenario ? { job_results: { [id]: scenario.results } } : {}),
      },
    })
    await mockAPI(page, item, { ...nextAction(id), current_order: undefined, attempt: undefined })
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const card = page
      .getByRole('region', { name: 'Execution event timeline' })
      .getByRole('article', { name: 'Implementation', exact: true })
    await expect(card).toBeVisible()
    await expect(card.getByText(scenario.badge, { exact: true })).toBeVisible()
    await expect(card).toContainText(scenario.text)
    await expect(card.getByText(/^Attempt \d/)).toHaveCount(0)
    await expect(card.locator('[data-implementation-delivery]')).toHaveCount(scenario.delivery ? 1 : 0)
    if ('warning' in scenario) await expect(card).toHaveClass(/bg-attention-soft/)
    if (scenario.name === 'shortened summary') {
      await expect(card.locator('strong', { hasText: 'card' })).toBeVisible()
      await expect(card.getByText('Full text (shortened here)')).toBeVisible()
    }
  })
}

// One order per work-order entry state, plus a claimed order without a
// claimant, agent, or model, and a queued order that adds no entry. Every
// order reports usage or none, and no entry prints it
// (component-web-task-surfaces, Work-order timeline entries).
const orderStatesItem = activity({
  work_orders: [
    order(`${taskId}-implement-1`, {
      state: 'timed_out',
      queue_entered_at: minutes(1),
      execution_deadline: minutes(9),
      usage_reported: true,
      tokens_in: 12_345,
      tokens_out: 6_789,
    }),
    order(`${taskId}-implement-2`, { state: 'stale', queue_entered_at: minutes(10) }),
    order(`${taskId}-implement-3`, {
      state: 'claimed',
      queue_entered_at: minutes(11),
      claimed_by: 'run:usr_operator_with_a_long_claimant_identifier',
      agent: 'claude-code',
      model: 'claude-opus-5-5',
      usage_reported: true,
      tokens_in: 45_678,
      tokens_out: 1_234,
    }),
    order(`${taskId}-spec-1`, {
      stage: 'spec',
      job_id: `${taskId}-spec-1`,
      state: 'claimed',
      queue_entered_at: minutes(12),
    }),
    order(`${taskId}-implement-4`, { state: 'queued', claimable: true, queue_entered_at: minutes(13) }),
  ],
})

const orderStateRows = [
  {
    state: 'timed_out',
    stage: 'Implementation',
    badge: 'Timed out',
    detail: 'The execution deadline elapsed before the agent finished; the retry policy applies.',
  },
  {
    state: 'stale',
    stage: 'Implementation',
    badge: 'Stale',
    detail: 'No agent claimed it before queue retention elapsed. Redispatch to offer it again.',
  },
  {
    state: 'claimed',
    stage: 'Implementation',
    badge: 'In progress',
    detail: 'run:usr_operator_with_a_long_claimant_identifier · claude-code · claude-opus-5-5',
  },
  { state: 'claimed', stage: 'Plan', badge: 'In progress', detail: undefined },
] as const

test.describe('at 390 px', () => {
  test.use({ viewport: { width: 390, height: 844 } })

  for (const surface of surfaces) {
    test(`the ${surface.name} work-order entries read stage, badge, and detail without usage or horizontal scroll`, async ({
      page,
    }) => {
      await mockAPI(page, orderStatesItem, nextAction(`${taskId}-implement-3`))
      await page.goto(surface.url)
      const timeline = page.getByRole('region', { name: 'Execution event timeline' })
      const entries = timeline.locator('li[data-order-entry]')
      await expect(entries).toHaveCount(orderStateRows.length)
      await expect(timeline.locator('li[data-order-entry="queued"]')).toHaveCount(0)
      for (const [index, row] of orderStateRows.entries()) {
        const entry = entries.nth(index)
        await expect(entry).toHaveAttribute('data-order-entry', row.state)
        await expect(entry.locator(':scope > div > div > span').first()).toHaveText(row.stage)
        await expect(entry.getByText(row.badge, { exact: true })).toBeVisible()
        if (row.detail) await expect(entry.locator('p')).toHaveText(row.detail)
        else await expect(entry.locator('p')).toHaveCount(0)
        const size = await entry.evaluate((element) => [element.scrollWidth, element.clientWidth])
        expect(size[0]).toBeLessThanOrEqual(size[1])
      }
      await expect(entries.filter({ hasText: 'Usage unavailable' })).toHaveCount(0)
      await expect(entries.filter({ hasText: / in \/ .* out/ })).toHaveCount(0)
      await expect(entries.filter({ hasText: '—' })).toHaveCount(0)
      const viewport = await page.evaluate(() => [
        document.documentElement.scrollWidth,
        document.documentElement.clientWidth,
      ])
      expect(viewport[0]).toBeLessThanOrEqual(viewport[1])
    })
  }

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

  for (const surface of surfaces) {
    test(`the ${surface.name} implementation cards have no horizontal scroll`, async ({ page }) => {
      await mockAPI(page, deliveredTask(), {
        ...nextAction(implementID(2)),
        current_order: undefined,
        attempt: undefined,
      })
      await page.goto(surface.url)
      const timeline = page.getByRole('region', { name: 'Execution event timeline' })
      const cards = timeline.locator('[data-implementation-entry]')
      await expect(cards).toHaveCount(2)
      for (const card of await cards.all()) {
        await expect(card).toBeVisible()
        const size = await card.evaluate((element) => [element.scrollWidth, element.clientWidth])
        expect(size[0]).toBeLessThanOrEqual(size[1])
      }
      const viewport = await page.evaluate(() => [
        document.documentElement.scrollWidth,
        document.documentElement.clientWidth,
      ])
      expect(viewport[0]).toBeLessThanOrEqual(viewport[1])
    })
  }
})

// A settled round's current summary renders inside its review panel above the
// seat rows, while seat rows, verdicts, and feedback render unchanged and a
// round without a summary shows no summary block (req-review-gates-evidence
// AC-10.5; DEC-79; feature-review-round-summary SU-6).
const seatID = (round: number, seat: number) => `${taskId}-review-${round}-seat-${seat}`

function seatOrder(round: number, seat: number) {
  return order(seatID(round, seat), {
    stage: 'review',
    state: 'completed',
    job_id: seatID(round, seat),
    review_round: round,
    review_seat: seat,
    agent: 'codex',
    model: seat === 1 ? 'gpt-6' : 'gpt-6-mini',
    queue_entered_at: minutes(round * 10),
  })
}

const roundOneSummary = {
  summary: 'Seat 1 found a missing regression test.\nSeat 2 approved with a naming nit.',
  findings: [
    {
      severity: 'blocking',
      problem: 'The refresh path has no regression test.',
      location: 'internal/workorder/service.go:983',
      fix: 'Add a test that records a summary after the claim.',
      seats: [1],
    },
    { severity: 'non-blocking', problem: 'Rename the helper for clarity.', seats: [1, 2] },
  ],
  version: 2,
  version_count: 2,
  author: {
    actor_id: 'agent:agt_coordinator',
    actor_role: 'agent',
    actor_owner_user_id: 'usr_coordinator_owner',
    model: 'claude-opus-5-5',
    model_enforcement: 'self-reported',
  },
  event_id: '150',
  recorded_at: minutes(18),
  truncated: false,
}

function summarizedPanels(extraSummaries: Record<string, unknown> = {}) {
  return activity({
    latest_stage: 'implement',
    work_orders: [seatOrder(1, 1), seatOrder(1, 2), seatOrder(2, 1), order(`${taskId}-implement-2`, {})],
    summaries: {
      review_results: {
        by_order: {
          [seatID(1, 1)]: {
            verdict: 'changes_requested',
            summary: 'Changes needed',
            feedback: 'Seat 1 feedback: add the missing regression test.',
            at: minutes(15),
            event_id: '120',
          },
          [seatID(1, 2)]: { verdict: 'approve', summary: 'Fine', feedback: '', at: minutes(16), event_id: '121' },
          [seatID(2, 1)]: {
            verdict: 'approve',
            summary: 'Fine',
            feedback: 'Round 2 seat 1: the test is in place.',
            at: minutes(25),
            event_id: '220',
          },
        },
        by_job: {},
      },
      review_rounds: {
        by_round: {
          '1': {
            event_id: '130',
            verdict: 'changes_requested',
            summary: 'The review panel requested changes.',
            at: minutes(17),
          },
          '2': { event_id: '230', verdict: 'approve', summary: 'All review panel seats approved.', at: minutes(26) },
        },
        by_order: {},
      },
      review_bounces: { '1': 1 },
      review_round_summaries: { '1': roundOneSummary, ...extraSummaries },
    },
  })
}

test('round summaries attach only to their matching review panels', () => {
  const item = summarizedPanels()
  // No review.summary_recorded event is on the bounded page: the summary
  // comes from summaries alone.
  expect((item.events as unknown[]).length).toBe(0)
  const panels = timelineOf(item).filter((entry) => entry.type === 'panel')
  expect(panels.map((panel) => panel.round)).toEqual([1, 2])
  expect(panels[0].roundSummary).toEqual(roundOneSummary)
  expect(panels[1].roundSummary).toBeUndefined()
  // A round-0 panel predates rounds and never takes a summary.
  const legacy = timelineOf(
    activity({
      work_orders: [order(`${taskId}-review-legacy`, { stage: 'review', review_seat: 1, review_round: 0 })],
      summaries: { review_round_summaries: { '0': roundOneSummary } },
    }),
  ).filter((entry) => entry.type === 'panel')
  expect(legacy).toHaveLength(1)
  expect(legacy[0].roundSummary).toBeUndefined()
})

// component-review invariant 39 and DEC-128: a seat shows its current reported
// model with no self-reported label, the panel never shows a recorded
// implementer match, and the panel states the frozen policy.
function seatLabelPanel(distinctModels: boolean) {
  const seat = (n: number, extra: Partial<WorkOrder>) =>
    order(`${taskId}-review-1-seat-${n}`, {
      stage: 'review',
      state: 'completed',
      job_id: `${taskId}-review-1-seat-${n}`,
      review_round: 1,
      review_seat: n,
      ...extra,
    })
  const verdict = (n: number, same?: string) => ({
    verdict: 'approve',
    summary: `seat ${n}`,
    feedback: '',
    at: minutes(8),
    event_id: String(n),
    ...(same ? { same_model_as_implementer: same } : {}),
  })
  return activity({
    task: { ...task, policy_contract: { review: { seats: [{}, {}, {}, {}], distinct_models: distinctModels } } },
    latest_stage: 'review',
    work_orders: [
      seat(1, { model: 'claude-sonnet-5-5', agent: 'claude-code', model_enforcement: 'self-reported' }),
      seat(2, { model: 'gpt-6', agent: 'codex', model_enforcement: 'self-reported' }),
      seat(3, { model: 'pinned-model', agent: 'codex', model_enforcement: 'worker-pinned' }),
      seat(4, { state: 'queued', required_model: 'required-model' }),
      seat(5, { state: 'queued' }),
    ],
    // A released or expired seat's order has no model; its job keeps the earlier one.
    jobs: [job(`${taskId}-review-1-seat-5`, 'review', 'pending', { model_tier: 'earlier-model' })],
    summaries: {
      review_results: {
        by_order: {
          [`${taskId}-review-1-seat-1`]: verdict(1, 'true'),
          [`${taskId}-review-1-seat-2`]: verdict(2, 'false'),
          [`${taskId}-review-1-seat-3`]: verdict(3, 'unknown'),
        },
      },
    },
  })
}

test('review seats label reported models and never show the recorded implementer match', async ({ page }) => {
  await mockAPI(page, seatLabelPanel(true), nextAction(`${taskId}-review-1-seat-4`, 'review'))
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  const seat = (n: number) => timeline.getByText(`Seat ${n}`, { exact: true }).locator('..')

  await expect(timeline.getByText('· task policy: distinct seat models')).toBeVisible()
  await expect(seat(1).getByText('claude-sonnet-5-5', { exact: true })).toBeVisible()
  await expect(seat(1).getByText('self-reported')).toHaveCount(0)
  // Seat 1's verdict recorded `true`, and the row still shows no match.
  await expect(seat(1).getByText('same model as implementer')).toHaveCount(0)
  await expect(seat(2).getByText('gpt-6', { exact: true })).toBeVisible()
  await expect(seat(2).getByText('self-reported')).toHaveCount(0)
  await expect(seat(2).getByText('same model as implementer')).toHaveCount(0)
  await expect(seat(3).getByText('self-reported')).toHaveCount(0)
  await expect(seat(3).getByText('same model as implementer')).toHaveCount(0)
  await expect(seat(4).getByText('required-model', { exact: true })).toBeVisible()
  await expect(seat(4).getByText('self-reported')).toHaveCount(0)
  await expect(seat(5).getByText('earlier-model', { exact: true })).toBeVisible()
  await expect(seat(5).getByText('self-reported')).toHaveCount(0)
  await expect(seat(4).getByText('same model as implementer')).toHaveCount(0)
  await expect(seat(5).getByText('same model as implementer')).toHaveCount(0)
  await expect(timeline.getByText('same model as implementer')).toHaveCount(0)
})

test('the review panel states the distinct-model policy only when the frozen policy is on', async ({ page }) => {
  await mockAPI(page, seatLabelPanel(false), nextAction(`${taskId}-review-1-seat-4`, 'review'))
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('Seat 1', { exact: true })).toBeVisible()
  await expect(timeline.getByText('· task policy: distinct seat models')).toHaveCount(0)
})

test('review panel shows its current round summary above unchanged seat feedback', async ({ page }, testInfo) => {
  await mockAPI(page, summarizedPanels(), nextAction(`${taskId}-implement-2`))
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  const summary = page.getByRole('region', { name: 'Round 1 summary' })
  await expect(timeline.getByRole('region', { name: 'Round 1 summary' })).toBeVisible()
  await expect(timeline.locator('[data-review-round-summary]')).toHaveCount(1)
  const summarized = timeline.locator('li article').filter({ has: summary })
  const unsummarized = timeline.locator('li article').filter({ hasText: 'Round 2 seat 1: the test is in place.' })
  await expect(summarized).toHaveCount(1)
  await expect(unsummarized).toHaveCount(1)

  await expect(summary.getByText('Seat 1 found a missing regression test.')).toBeVisible()
  await expect(summary.getByText('version 2 of 2')).toBeVisible()
  const author = summary.locator('[data-review-summary-author]')
  await expect(author).toContainText('By an agent')
  await expect(author.locator('[data-review-summary-owner]')).toHaveText(' (owned by an operator)')
  await expect(author).toContainText('claude-opus-5-5')
  await expect(author).toContainText('(self-reported)')
  const blocking = summary.locator('[data-review-summary-severity="blocking"]')
  await expect(blocking.getByRole('heading')).toHaveText('Blocking 1')
  await expect(blocking.getByText('The refresh path has no regression test.')).toBeVisible()
  await expect(blocking.getByText('internal/workorder/service.go:983')).toBeVisible()
  await expect(blocking.getByText('Fix: Add a test that records a summary after the claim.')).toBeVisible()
  await expect(blocking.getByText('Seat 1', { exact: true })).toBeVisible()
  const nonBlocking = summary.locator('[data-review-summary-severity="non-blocking"]')
  await expect(nonBlocking.getByRole('heading')).toHaveText('Non-blocking 1')
  await expect(nonBlocking.getByText('Seats 1, 2', { exact: true })).toBeVisible()
  await expect(nonBlocking.getByText(/^Fix:/)).toHaveCount(0)

  // The summary sits above the seat rows; seats, verdicts, and feedback are unchanged.
  const seatRows = summarized.locator('div.divide-y > div')
  await expect(seatRows).toHaveCount(2)
  const above = await summary.evaluate(
    (element, other) => Boolean(other && element.compareDocumentPosition(other) & Node.DOCUMENT_POSITION_FOLLOWING),
    await seatRows.first().elementHandle(),
  )
  expect(above).toBe(true)
  await expect(summarized.locator('[data-review-verdict]')).toHaveText('Changes requested')
  await expect(summarized.getByText('Seat 1 feedback: add the missing regression test.')).toBeVisible()
  await expect(summarized.locator('[data-review-banner]')).toHaveText(
    'Sent back for changes · 1 of 2 seats asked for changes · 1 bounce used',
  )
  await expect(unsummarized.locator('[data-review-round-summary]')).toHaveCount(0)
  await expect(unsummarized.locator('[data-review-verdict]')).toHaveText('Approved')
  await expect(unsummarized.locator('div.divide-y > div')).toHaveCount(1)
  await expect(timeline).not.toContainText('undefined')

  await summarized.screenshot({ path: testInfo.outputPath('review-round-summary-panel.png') })
  await unsummarized.screenshot({ path: testInfo.outputPath('review-round-without-summary-panel.png') })
})

test('a single-version summary with no findings shows no version counter or finding groups', async ({ page }) => {
  const item = summarizedPanels({
    '2': {
      ...roundOneSummary,
      summary: 'Approved.\n\nNo findings this round.',
      findings: [],
      version: 1,
      version_count: 1,
      author: { actor_id: 'user:usr_operator', actor_role: 'user', model_enforcement: 'self-reported' },
      event_id: '250',
    },
  })
  await mockAPI(page, item, nextAction(`${taskId}-implement-2`))
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const summary = page.getByRole('region', { name: 'Round 2 summary' })
  await expect(summary).toBeVisible()
  await expect(summary.locator('p').first()).toHaveText('Approved.\n\nNo findings this round.', { useInnerText: false })
  await expect(summary.locator('[data-review-summary-version]')).toHaveCount(0)
  await expect(summary.locator('[data-review-summary-severity]')).toHaveCount(0)
  await expect(summary.locator('[data-review-summary-author]')).toContainText('By an operator')
  await expect(summary.locator('[data-review-summary-author]')).not.toContainText('self-reported')
})

test.describe('round summary at 390 px', () => {
  test.use({ viewport: { width: 390, height: 844 } })

  test('the summarized review panel has no horizontal scroll', async ({ page }, testInfo) => {
    await mockAPI(page, summarizedPanels(), nextAction(`${taskId}-implement-2`))
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const summary = page.getByRole('region', { name: 'Round 1 summary' })
    await expect(summary).toBeVisible()
    const panel = page.locator('li article').filter({ has: summary })
    const card = await panel.evaluate((element) => [element.scrollWidth, element.clientWidth])
    expect(card[0]).toBeLessThanOrEqual(card[1])
    const viewport = await page.evaluate(() => [
      document.documentElement.scrollWidth,
      document.documentElement.clientWidth,
    ])
    expect(viewport[0]).toBeLessThanOrEqual(viewport[1])
    await panel.screenshot({ path: testInfo.outputPath('review-round-summary-panel-390.png') })
  })
})

// The timeline's redispatch card never repeats a redispatch-path choice the
// server's next action already offers: that choice is task detail's one button
// for the act, under the server's label. The card keeps a closed task with a
// decided stage and a queued task the next action offers no such choice for
// (DEC-61; req-operator-next-action AC-1.4, AC-1.7; task 261108-1ae968).
const redispatchButtons = (page: Page) => page.getByRole('button', { name: /^(Recover|Redispatch|Resume task)$/ })

function recordPosts(page: Page) {
  const posts: string[] = []
  return page
    .route(`**/v1/tasks/${taskId}/redispatch*`, (route) => {
      posts.push(`${route.request().method()} ${new URL(route.request().url()).pathname}`)
      return route.fulfill({ json: {} })
    })
    .then(() => posts)
}

const parkedAction: NextAction = {
  ...nextAction(`${taskId}-triage-1`),
  label: 'Decide on a parked task',
  actor: 'operator',
  kind: 'parked',
  judgment: true,
  summary: 'This task is parked.',
  question: 'Put it back, or close it?',
  current_order: undefined,
  attempt: undefined,
  choices: [
    {
      id: 'recover',
      label: 'Recover',
      effect: 'Puts the task back in the queue.',
      method: 'POST',
      path: `/v1/tasks/${taskId}/redispatch`,
      capability: 'recover_work',
      allowed: true,
    },
    {
      id: 'close',
      label: 'Close',
      effect: 'Closes the task.',
      method: 'POST',
      path: `/v1/tasks/${taskId}/close`,
      capability: 'operate_gates',
      allowed: true,
    },
  ],
}

for (const surface of surfaces) {
  test(`the ${surface.name} offers a parked task's redispatch once, from the server's Recover choice`, async ({
    page,
  }) => {
    await mockAPI(
      page,
      activity({
        task: { ...task, state: 'parked', recovery_stage: 'triage' },
        needs_attention: true,
        summaries: {
          park_reason: {
            summary: 'Triage could not tell which surface the task changes.',
            questions: ['Which page shows the second button?'],
            risks: [],
            event_id: '41',
            at: minutes(4),
          },
        },
      }),
      parkedAction,
    )
    const posts = await recordPosts(page)
    await page.goto(surface.url)
    const card = page.getByRole('region', { name: 'Next action' })
    const reason = card.getByRole('region', { name: 'Why it is parked' })
    await expect(reason).toContainText('Triage could not tell which surface the task changes.')
    await expect(reason.getByRole('region', { name: 'Open questions' })).toContainText(
      'Which page shows the second button?',
    )
    await expect(reason.getByRole('region', { name: 'Risks' })).toHaveCount(0)

    await expect(redispatchButtons(page)).toHaveCount(1)
    const timeline = page.getByRole('region', { name: 'Execution event timeline' })
    await expect(timeline.getByRole('button')).toHaveCount(0)
    await expect(timeline.getByText(/Parked by triage — resume/)).toHaveCount(0)
    await card.getByRole('button', { name: 'Recover', exact: true }).click()
    await expect.poll(() => posts).toEqual([`POST /v1/tasks/${taskId}/redispatch`])
  })
}

test('a closed task with a decided stage keeps the timeline redispatch card', async ({ page }) => {
  await mockAPI(page, activity({ task: { ...task, state: 'closed', next_stage: 'implement' } }), {
    ...nextAction(`${taskId}-implement-1`),
    label: 'Finished',
    actor: 'none',
    kind: 'terminal',
    current_order: undefined,
    attempt: undefined,
  })
  const posts = await recordPosts(page)
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('Closed — redispatch resumes at the decided stage.')).toBeVisible()
  await expect(redispatchButtons(page)).toHaveCount(1)
  await timeline.getByRole('button', { name: 'Redispatch', exact: true }).click()
  await expect.poll(() => posts).toEqual([`POST /v1/tasks/${taskId}/redispatch`])
})

test('a closed task without a decided stage offers no redispatch', async ({ page }) => {
  await mockAPI(page, activity({ task: { ...task, state: 'closed', next_stage: '' } }), {
    ...nextAction(`${taskId}-implement-1`),
    label: 'Finished',
    actor: 'none',
    kind: 'terminal',
    current_order: undefined,
    attempt: undefined,
  })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(page.getByRole('heading', { name: 'Activity', exact: true })).toBeVisible()
  await expect(timeline.getByText('Closed — redispatch resumes at the decided stage.')).toHaveCount(0)
  await expect(redispatchButtons(page)).toHaveCount(0)
})

test('a queued task the next action offers no redispatch for keeps the card in plain words', async ({ page }) => {
  await mockAPI(
    page,
    activity({
      task: { ...task, state: 'queued' },
      work_orders: [order(`${taskId}-implement-1`, { state: 'queued', claimable: true })],
    }),
    {
      ...nextAction(`${taskId}-implement-1`),
      label: 'Waiting for an agent',
      kind: 'queued',
      summary: 'Waiting for an agent to claim this task.',
      attempt: undefined,
    },
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  const sentence = timeline.getByText('Waiting for an agent to pick this task up.', { exact: true })
  await expect(sentence).toBeVisible()
  // The sentence names no internal term; Redispatch stays the button label.
  await expect(sentence).not.toContainText(/enqueue|dispatch/i)
  await expect(timeline.getByRole('button', { name: 'Redispatch', exact: true })).toBeVisible()
  await expect(redispatchButtons(page)).toHaveCount(1)
})

test('a queued task keeps the timeline redispatch card when the next-action read fails', async ({ page }) => {
  await mockAPI(
    page,
    activity({
      task: { ...task, state: 'queued' },
      work_orders: [order(`${taskId}-implement-1`, { state: 'queued', claimable: true })],
    }),
    nextAction(`${taskId}-implement-1`),
  )
  // Registered after mockAPI, so it answers every next-action read, retries included.
  let nextActionReads = 0
  await page.route(`**/v1/tasks/${taskId}/next-action*`, (route) => {
    nextActionReads++
    return route.fulfill({ status: 500, json: { error: 'internal' } })
  })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  // With no stated next action no choice renders, so the card is the one button.
  await expect(timeline.getByRole('button', { name: 'Redispatch', exact: true })).toBeVisible()
  await expect(redispatchButtons(page)).toHaveCount(1)
  expect(nextActionReads).toBeGreaterThan(0)
})

test('a stalled queued task with a redispatch choice renders no timeline redispatch card', async ({ page }) => {
  await mockAPI(
    page,
    activity({
      task: { ...task, state: 'queued' },
      work_orders: [order(`${taskId}-implement-1`, { state: 'queued', claimable: false })],
    }),
    {
      ...nextAction(`${taskId}-implement-1`),
      label: 'Stalled',
      actor: 'operator',
      kind: 'stalled',
      summary: 'No agent claimed this task in time.',
      attempt: undefined,
      choices: [
        {
          id: 'redispatch',
          label: 'Redispatch',
          effect: 'Puts the task back in the queue.',
          method: 'POST',
          path: `/v1/tasks/${taskId}/redispatch`,
          capability: 'recover_work',
          allowed: true,
        },
      ],
      details: { stall_reason: 'unclaimable' },
    },
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  await expect(card.getByRole('button', { name: 'Redispatch', exact: true })).toBeVisible()
  await expect(redispatchButtons(page)).toHaveCount(1)
  await expect(
    page.getByRole('region', { name: 'Execution event timeline' }).getByRole('button', { name: 'Redispatch' }),
  ).toHaveCount(0)
})

// The review panel reads on a phone (task 261110-50771a;
// component-web-task-surfaces; req-review-gates-evidence AC-2.7;
// component-review invariant 39; DEC-128):
// each settled seat row keeps its label, model, verdict, and duration on one
// line, notes wrap inside the card, a one-seat panel's note has no seat
// gutter, and the settled banner states what happened next in short words.
type PanelSeatFixture = {
  model: string
  verdict: 'approve' | 'changes_requested'
  feedback?: string
  same?: 'true' | 'false' | 'unknown'
  risk?: 'low' | 'medium' | 'high'
}

// A 120-character unbroken token, such as a long code path or identifier.
const longToken = 'internal/dispatch/merge.go:949-990#mergeApprovedTaskLocked'.repeat(3).slice(0, 120)
const longNote = [
  `The merge path in ${longToken} skips the check-status read.`,
  'Read the approved head before the automatic merge, and hold the task on a failed check.',
  '```go\nif err := mergeApprovedTaskLocked(ctx, task, head, policy, checks, audit, recorder, clock); err != nil { return err }\n```',
  'Add a regression test that holds the merge on a pending check.',
  'Add a second test that records the deadline only on the first pending read.',
].join('\n\n')

function reviewPanel(seats: PanelSeatFixture[], extra: { bounce?: number; verdict?: string } = {}) {
  const id = (n: number) => `${taskId}-review-1-seat-${n}`
  const roundVerdict =
    extra.verdict ?? (seats.some((seat) => seat.verdict === 'changes_requested') ? 'changes_requested' : 'approve')
  return activity({
    task: { ...task, policy_contract: { review: { seats: seats.map(() => ({})) } } },
    latest_stage: roundVerdict === 'approve' ? 'review' : 'implement',
    work_orders: seats.map((seat, index) =>
      order(id(index + 1), {
        stage: 'review',
        state: 'completed',
        job_id: id(index + 1),
        review_round: 1,
        review_seat: index + 1,
        model: seat.model,
        agent: 'claude-code',
        model_enforcement: 'self-reported',
      }),
    ),
    jobs: seats.map((_, index) =>
      job(id(index + 1), 'review', 'done', { started_at: minutes(6), ended_at: minutes(14 + index) }),
    ),
    summaries: {
      review_results: {
        by_order: Object.fromEntries(
          seats.map((seat, index) => [
            id(index + 1),
            {
              verdict: seat.verdict,
              summary: seat.verdict === 'approve' ? 'Approved' : 'Changes needed',
              feedback: seat.feedback ?? '',
              at: minutes(14 + index),
              event_id: String(100 + index),
              ...(seat.same ? { same_model_as_implementer: seat.same } : {}),
              ...(seat.risk ? { risk_tier: seat.risk, risk_reason: 'Touches the merge path.' } : {}),
            },
          ]),
        ),
      },
      review_rounds: {
        by_round: {
          '1': {
            event_id: '200',
            verdict: roundVerdict,
            summary: roundVerdict === 'approve' ? 'All review panel seats approved.' : 'Changes requested.',
            at: minutes(20),
          },
        },
        by_order: {},
      },
      ...(extra.bounce === undefined ? {} : { review_bounces: { '1': extra.bounce } }),
      review_round_seats: {
        by_round: {
          '1': { review_round: 1, seat_count: seats.length, seat_count_source: 'minimum', minimum_seat_count: 1 },
        },
        by_order: {},
      },
    },
  })
}

// Realistic 24-character model names: the longest a seat row shows in full at 375 px.
const name24 = ['claude-sonnet-5-5-review', 'gpt-6.1-sol-extended-pro', 'gemini-3-pro-max-preview']
const oneSeatChanges = () =>
  reviewPanel([{ model: name24[0], verdict: 'changes_requested', feedback: longNote }], { bounce: 1 })
const threeSeatMixed = () =>
  reviewPanel(
    [
      { model: 'claude-opus-5-5', verdict: 'approve', same: 'true', risk: 'medium' },
      {
        model: 'gpt-6.1-sol-extended-reasoning-preview-2026-11-01',
        verdict: 'changes_requested',
        feedback: longNote,
        same: 'false',
      },
      { model: name24[2], verdict: 'changes_requested', feedback: 'Rename `mergeApprovedTaskLocked`.' },
    ],
    { bounce: 2 },
  )
const threeSeatApproved = () =>
  reviewPanel([
    { model: 'claude-opus-5-5', verdict: 'approve', feedback: 'Looks right.' },
    { model: 'gpt-6.1-sol', verdict: 'approve' },
    { model: 'gemini-3-pro', verdict: 'approve' },
  ])

async function openPanel(page: Page, item: object, width: number) {
  await page.setViewportSize({ width, height: 900 })
  await mockAPI(page, item, nextAction(`${taskId}-implement-2`))
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const panel = page.getByRole('region', { name: 'Execution event timeline' }).locator('li article')
  await expect(panel).toHaveCount(1)
  await page.evaluate(() => document.fonts.ready)
  return panel
}

type Box = { x: number; y: number; width: number; height: number }
async function box(locator: Locator): Promise<Box> {
  const bounds = await locator.boundingBox()
  expect(bounds).not.toBeNull()
  return bounds as Box
}

// The visible pill, not its larger disclosure touch target.
const seatRow = (panel: Locator, n: number) => panel.getByText(`Seat ${n}`, { exact: true }).first().locator('..')
const seatPill = (row: Locator) => row.locator('[data-seat-verdict]')

async function expectFullName(row: Locator, model: string) {
  const name = row.getByText(model, { exact: true })
  expect(await name.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true)
}

async function expectOneLine(row: Locator, n: number, model: string) {
  const label = await box(row.getByText(`Seat ${n}`, { exact: true }))
  const pill = await box(seatPill(row))
  const chip = await box(row.getByTitle(model, { exact: true }).first())
  const took = await box(row.getByText(/^\d+m \d{2}s$/))
  for (const other of [pill, chip, took]) expect(Math.abs(other.y - label.y)).toBeLessThanOrEqual(4)
}

async function expectInside(panel: Locator, inner: Locator) {
  const outer = await box(panel)
  const count = await inner.count()
  expect(count).toBeGreaterThan(0)
  for (let i = 0; i < count; i++) {
    const bounds = await box(inner.nth(i))
    expect(bounds.x).toBeGreaterThanOrEqual(outer.x - 0.5)
    expect(bounds.x + bounds.width).toBeLessThanOrEqual(outer.x + outer.width + 0.5)
  }
}

async function expectNoteContained(page: Page, panel: Locator) {
  await expectInside(panel, panel.locator('.markdown'))
  await expectInside(panel, panel.locator('.markdown pre'))
  await expectInside(panel, panel.getByRole('button', { name: /seat \d+ notes/ }))
  // Prose wraps inside its box; fenced code scrolls inside its own.
  for (const prose of await panel.locator('.markdown').all()) {
    expect(await prose.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true)
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(375)
}

test('a one-seat changes-requested panel reads on a 375 px screen', async ({ page }) => {
  expect(longToken).toHaveLength(120)
  const panel = await openPanel(page, oneSeatChanges(), 375)
  const row = seatRow(panel, 1)

  // Seat 1, a 24-character model name in full, the verdict pill, and the
  // duration share one line. The narrow pill keeps its word for screen readers.
  expect(name24.every((model) => model.length === 24)).toBe(true)
  await expectOneLine(row, 1, name24[0])
  await expectFullName(row, name24[0])
  await expect(seatPill(row)).toHaveText('Changes')
  await expect(row.getByText('self-reported')).toHaveCount(0)

  // One seat needs no attribution beside its note: "Seat 1" appears once.
  await expect(panel.getByText('Seat 1', { exact: true })).toHaveCount(1)
  await expect(panel.getByText(longToken, { exact: false })).toBeVisible()
  await expectNoteContained(page, panel)
  const code = panel.locator('.markdown pre')
  expect(await code.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(true)

  // The expanded note stays inside the card too.
  const more = panel.getByRole('button', { name: 'Expand seat 1 notes' })
  await expect(more).toHaveText('More')
  await more.click()
  await expect(panel.getByRole('button', { name: 'Collapse seat 1 notes' })).toHaveText('Less')
  await expectNoteContained(page, panel)

  await expect(panel.locator('[data-review-verdict]')).toHaveText('Changes requested')
  await expect(panel.locator('[data-review-banner]')).toHaveText('Sent back for changes · 1 bounce used')
  await expect(panel.getByText('Panel of 1 · minimum 1', { exact: true })).toBeVisible()
})

test('a three-seat panel keeps each seat row on one line at 375 px and attributes each note', async ({ page }) => {
  const panel = await openPanel(page, threeSeatMixed(), 375)
  const longModel = 'gpt-6.1-sol-extended-reasoning-preview-2026-11-01'

  await expectOneLine(seatRow(panel, 1), 1, 'claude-opus-5-5')
  await expectFullName(seatRow(panel, 1), 'claude-opus-5-5')
  // A 24-character name beside a "10m 00s" duration still shows in full.
  await expectOneLine(seatRow(panel, 3), 3, name24[2])
  await expectFullName(seatRow(panel, 3), name24[2])
  // A model name longer than the row truncates with an ellipsis, keeps its
  // full name in the chip's title, and leaves the pill on the seat's line.
  await expectOneLine(seatRow(panel, 2), 2, longModel)
  const name = seatRow(panel, 2).getByText(longModel, { exact: true })
  expect(await name.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(true)
  expect(await name.evaluate((element) => getComputedStyle(element).textOverflow)).toBe('ellipsis')

  // The risk tier stays readable. No seat shows a recorded implementer match,
  // although seat 1's verdict recorded `true`, and no seat says self-reported.
  await expect(seatRow(panel, 1).locator('[data-seat-risk-tier="medium"]')).toBeVisible()
  await expect(panel.getByText('same model as implementer')).toHaveCount(0)
  await expect(panel.getByText('self-reported')).toHaveCount(0)

  // Each note in a multi-seat panel keeps its seat label.
  const notes = panel.locator('div.grid').filter({ has: page.locator('.markdown') })
  await expect(notes).toHaveCount(2)
  await expect(notes.nth(0).getByText('Seat 2', { exact: true })).toBeVisible()
  await expect(notes.nth(1).getByText('Seat 3', { exact: true })).toBeVisible()
  await expectNoteContained(page, panel)

  await expect(panel.locator('[data-review-banner]')).toHaveText(
    'Sent back for changes · 2 of 3 seats asked for changes · 2 bounces used',
  )
})

const bannerCases: { name: string; item: () => object; text: string }[] = [
  {
    name: 'one seat without a recorded bounce',
    item: () => reviewPanel([{ model: 'm', verdict: 'changes_requested', feedback: 'Fix it.' }]),
    text: 'Sent back for changes',
  },
  {
    name: 'one seat with a recorded zero bounce',
    item: () => reviewPanel([{ model: 'm', verdict: 'changes_requested', feedback: 'Fix it.' }], { bounce: 0 }),
    text: 'Sent back for changes · 0 bounces used',
  },
  {
    name: 'two seats with one bounce',
    item: () =>
      reviewPanel(
        [
          { model: 'a', verdict: 'changes_requested', feedback: 'Fix it.' },
          { model: 'b', verdict: 'approve' },
        ],
        { bounce: 1 },
      ),
    text: 'Sent back for changes · 1 of 2 seats asked for changes · 1 bounce used',
  },
  {
    name: 'two seats with three bounces',
    item: () =>
      reviewPanel(
        [
          { model: 'a', verdict: 'changes_requested', feedback: 'Fix it.' },
          { model: 'b', verdict: 'changes_requested', feedback: 'Fix it too.' },
        ],
        { bounce: 3 },
      ),
    text: 'Sent back for changes · 2 of 2 seats asked for changes · 3 bounces used',
  },
  { name: 'one approving seat', item: () => reviewPanel([{ model: 'm', verdict: 'approve' }]), text: 'Approved' },
  { name: 'three approving seats', item: threeSeatApproved, text: 'The panel is unanimous — 3 of 3 approved' },
]

for (const { name, item, text } of bannerCases) {
  test(`the settled review banner for ${name} reads "${text}"`, async ({ page }) => {
    const panel = await openPanel(page, item(), 1280)
    await expect(panel.locator('[data-review-banner]')).toHaveText(text)
    // A wide card shows each seat's verdict word in its pill.
    const word = seatPill(seatRow(panel, 1)).getByText(/^(Approved|Changes)$/)
    expect((await box(word)).width).toBeGreaterThan(20)
  })
}

// Mocked screenshots for visual inspection (AGENTS.md UI changes). The HTML
// report carries each attachment, so CI uploads them with the report.
const screenshotCases = [
  { name: 'one-seat-changes', item: oneSeatChanges },
  { name: 'three-seat-mixed', item: threeSeatMixed },
  { name: 'three-seat-approved', item: threeSeatApproved },
]

for (const { name, item } of screenshotCases) {
  for (const width of [375, 1280]) {
    test(`review panel screenshot: ${name} at ${width} px`, async ({ page }, testInfo) => {
      const panel = await openPanel(page, item(), width)
      const path = testInfo.outputPath(`review-panel-${name}-${width}.png`)
      await panel.screenshot({ path })
      await testInfo.attach(`review-panel-${name}-${width}`, { path, contentType: 'image/png' })
      if (width === 375) expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(375)
    })
  }
}

// A queued order with no claimant adds no timeline entry for any stage. The
// next-action card alone states that the task waits for an agent, so the
// timeline repeats no current-state summary (component-web-task-surfaces,
// Presenting the statement invariant 2; req-operator-next-action AC-1.7,
// AC-7.1). A queued review seat keeps its waiting label inside its round's
// panel, and the redispatch card keeps its sentence (task 261111-11da2c).
function queuedCurrentOrder(id: string, stage: string): NextAction['current_order'] {
  return { id, stage, state: 'queued', queue_deadline: null, execution_deadline: null, lease_expires_at: null }
}

const queuedOrdersAction: NextAction = {
  ...nextAction(`${taskId}-implement-1`),
  label: 'Waiting for an agent',
  kind: 'queued',
  summary: 'Waiting for an agent to claim this task.',
  current_order: queuedCurrentOrder(`${taskId}-implement-1`, 'implement'),
  attempt: undefined,
}

const queuedPlanAndImplementation = activity({
  task: { ...task, state: 'queued' },
  latest_stage: 'implement',
  work_orders: [
    order(`${taskId}-spec-1`, { stage: 'spec', state: 'queued', claimable: true, queue_entered_at: minutes(1) }),
    order(`${taskId}-implement-1`, { state: 'queued', claimable: true, queue_entered_at: minutes(2) }),
  ],
})

for (const surface of surfaces) {
  test(`the ${surface.name} adds no timeline entry for queued Plan and Implementation orders`, async ({ page }) => {
    await mockAPI(page, queuedPlanAndImplementation, queuedOrdersAction)
    await page.goto(surface.url)
    const timeline = page.getByRole('region', { name: 'Execution event timeline' })
    // The redispatch card loads with the timeline, so the absence checks below
    // never run against an unloaded timeline.
    await expect(timeline.getByText('Waiting for an agent to pick this task up.', { exact: true })).toBeVisible()
    await expect(timeline.getByRole('button', { name: 'Redispatch', exact: true })).toBeVisible()

    await expect(timeline.locator('li[data-order-entry]')).toHaveCount(0)
    await expect(timeline.getByText('Waiting for an agent', { exact: true })).toHaveCount(0)
    await expect(timeline.getByText('Any agent connected over MCP can claim it.', { exact: false })).toHaveCount(0)

    // The next-action card still states the wait in the server's words.
    const card = page.getByRole('region', { name: 'Next action' })
    await expect(card.getByText('Waiting for an agent', { exact: true })).toBeVisible()
    await expect(card.getByText('Waiting for an agent to claim this task.', { exact: true })).toBeVisible()
  })
}

// Visual record for the pull request: a queued task's redispatch card on the
// task page and the full task page, at 375 px and at a desktop width.
for (const surface of surfaces) {
  for (const width of [375, 1280]) {
    test(`queued redispatch card screenshots on the ${surface.name} at ${width} px`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width, height: 900 })
      await mockAPI(page, queuedPlanAndImplementation, queuedOrdersAction)
      await page.goto(surface.url)
      const timeline = page.getByRole('region', { name: 'Execution event timeline' })
      const sentence = timeline.getByText('Waiting for an agent to pick this task up.', { exact: true })
      await expect(sentence).toBeVisible()
      await expect(timeline.getByRole('button', { name: 'Redispatch', exact: true })).toBeVisible()
      await page.evaluate(() => document.fonts.ready)
      await expectNoSidewaysScroll(page)
      // The card sits in a bounded scroll region, so bring it into view first.
      await sentence.scrollIntoViewIfNeeded()
      const slug = surface.name === 'Board sheet' ? 'sheet' : 'full'
      await capture(page, testInfo, `redispatch-queued-${slug}-${width}.png`)
    })
  }
}

const queuedVerifyAndReview = activity({
  latest_stage: 'review',
  work_orders: [
    order(`${taskId}-verify-1`, { stage: 'verify', state: 'queued', claimable: true, queue_entered_at: minutes(1) }),
    order(`${taskId}-review-1`, { stage: 'review', state: 'queued', claimable: true, queue_entered_at: minutes(2) }),
    order(`${taskId}-review-2-seat-1`, {
      stage: 'review',
      state: 'queued',
      claimable: true,
      review_round: 2,
      review_seat: 1,
      queue_entered_at: minutes(3),
    }),
  ],
})

test('queued Verification and Review orders add no order entry while a queued review seat keeps its waiting status', () => {
  const entries = timelineOf(queuedVerifyAndReview)
  expect(entries.filter((entry) => entry.type === 'order')).toEqual([])
  const panels = entries.filter((entry) => entry.type === 'panel')
  expect(panels).toHaveLength(1)
  expect(panels[0].seats.map((seat) => seat.status)).toEqual(['waiting'])
})

for (const surface of surfaces) {
  test(`the ${surface.name} keeps the queued review seat label and adds no queued Verification or Review entry`, async ({
    page,
  }) => {
    await mockAPI(page, queuedVerifyAndReview, {
      ...queuedOrdersAction,
      current_order: queuedCurrentOrder(`${taskId}-review-2-seat-1`, 'review'),
    })
    await page.goto(surface.url)
    const timeline = page.getByRole('region', { name: 'Execution event timeline' })
    const seatLabel = timeline.getByText('Waiting for an agent', { exact: true })
    await expect(seatLabel).toHaveCount(1)
    await expect(seatLabel).toBeVisible()
    await expect(seatLabel).toHaveAttribute(
      'title',
      'Any agent connected over MCP can claim this seat. The server URL is in Settings.',
    )
    await expect(timeline.locator('li[data-order-entry]')).toHaveCount(0)
    await expect(timeline.getByText('Any agent connected over MCP can claim it.', { exact: false })).toHaveCount(0)
  })
}

// ---- Narrow-screen layout (component-web-task-surfaces invariant 43; DEC-127) ----
//
// The job card header says what happened and its footer says who and when. The
// Implementation header carries the pull request chip, and summaries wrap
// inside the card. These cases measure the rendered page at 375 px and at a
// desktop width.

const longHashSummary = `Resubmitted at head ${secondHead} (PR #1431). CI: https://github.com/kidus-tiliksew/conveyor/actions/runs/19283746501/job/54839201938475?check_suite_focus=true`

function deliveredLongSummary() {
  const item = deliveredTask() as { work_orders: WorkOrder[] }
  item.work_orders[1] = { ...item.work_orders[1], progress: longHashSummary }
  return item
}

// Every child of the element sits on one line.
async function expectOneLineHeader(header: Locator) {
  const rows = await header.evaluate((element) =>
    Array.from(element.children).map((child) => {
      const bounds = child.getBoundingClientRect()
      return { top: bounds.top, bottom: bounds.bottom }
    }),
  )
  expect(rows.length).toBeGreaterThan(1)
  const lowestTop = Math.max(...rows.map((row) => row.top))
  const highestBottom = Math.min(...rows.map((row) => row.bottom))
  expect(lowestTop).toBeLessThan(highestBottom)
}

async function expectNoSidewaysScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)).toBeLessThanOrEqual(0)
  const overflowing = await page
    .getByRole('region', { name: 'Execution event timeline' })
    .locator('li article')
    .evaluateAll((cards) => cards.filter((card) => card.scrollWidth > card.clientWidth + 0.5).length)
  expect(overflowing).toBe(0)
}

// The screenshots attach to the Playwright report, which CI uploads as an
// artifact, and are also written to CONVEYOR_SCREENSHOT_DIR when it is set.
async function capture(page: Page, testInfo: TestInfo, name: string) {
  const shots = process.env.CONVEYOR_SCREENSHOT_DIR
  const image = await page.screenshot({ path: shots ? `${shots}/${name}` : undefined, fullPage: true })
  await testInfo.attach(name, { body: image, contentType: 'image/png' })
}

async function openFinishedTask(page: Page, width: number) {
  await page.setViewportSize({ width, height: 1400 })
  await mockAPI(page, deliveredLongSummary(), {
    ...nextAction(implementID(2)),
    label: 'Finished',
    actor: 'none',
    kind: 'terminal',
    current_order: undefined,
    attempt: undefined,
  })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page
    .getByRole('region', { name: 'Execution event timeline' })
    .getByRole('article', { name: 'Implementation attempt 2' })
  await expect(card).toBeVisible()
  await page.evaluate(() => document.fonts.ready)
  return card
}

test('at 375 px an attempt-2 Implementation header fits one line and no card scrolls sideways', async ({
  page,
}, testInfo) => {
  const card = await openFinishedTask(page, 375)
  const header = card.locator('[data-job-header]')
  await expect(header.locator('[data-implementation-title]')).toHaveText('Implementation 2')
  await expect(header.getByText('Submitted', { exact: true })).toBeVisible()
  await expect(header.locator('[data-implementation-delivery]')).toBeVisible()
  await expectOneLineHeader(header)
  // A card narrower than 28rem shows the number and keeps the head in the
  // chip's accessible name and title.
  expect(await card.evaluate((element) => element.getBoundingClientRect().width)).toBeLessThan(448)
  const delivery = header.locator('[data-implementation-delivery]')
  await expect(delivery.locator('[data-implementation-head]')).toBeHidden()
  await expect(delivery).toHaveAccessibleName(`Pull request #1431, head ${secondHead}`)
  // The 40-character head and the long URL wrap inside the summary.
  await expect(card.getByText(secondHead, { exact: false })).toBeVisible()
  await expectNoSidewaysScroll(page)
  // A time disclosure open at the card's right edge stays inside the page.
  const time = card.locator('[data-job-footer] [data-job-time]')
  await time.focus()
  const details = card.locator('[data-job-time-details]')
  await expect(details).toBeVisible()
  const bounds = await details.boundingBox()
  expect(bounds!.x).toBeGreaterThanOrEqual(0)
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(375)
  await expectNoSidewaysScroll(page)
  await card.scrollIntoViewIfNeeded()
  await capture(page, testInfo, 'timeline-time-open-375.png')
})

test('at a desktop width the Implementation chip adds the seven-character head', async ({ page }, testInfo) => {
  const card = await openFinishedTask(page, 1280)
  const header = card.locator('[data-job-header]')
  expect(await card.evaluate((element) => element.getBoundingClientRect().width)).toBeGreaterThanOrEqual(448)
  await expect(header.locator('[data-implementation-head]')).toHaveText(` · ${secondHead.slice(0, 7)}`)
  await expect(header.locator('[data-implementation-head]')).toHaveClass(/font-mono/)
  await expectOneLineHeader(header)
  await expectNoSidewaysScroll(page)
  await capture(page, testInfo, 'timeline-1280.png')
  await card.locator('[data-job-footer] [data-job-time]').hover()
  await expect(card.locator('[data-job-time-details]')).toBeVisible()
  await capture(page, testInfo, 'timeline-time-open-1280.png')
})

test.describe('on a coarse pointer at 375 px', () => {
  test.use({ viewport: { width: 375, height: 1400 }, isMobile: true, hasTouch: true })

  test('the footer model and time are 40 px targets and the footer keeps its height', async ({ page }) => {
    await mockAPI(page, deliveredLongSummary(), {
      ...nextAction(implementID(2)),
      label: 'Finished',
      actor: 'none',
      kind: 'terminal',
      current_order: undefined,
      attempt: undefined,
    })
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const card = page
      .getByRole('region', { name: 'Execution event timeline' })
      .getByRole('article', { name: 'Implementation attempt 2' })
    const footer = card.locator('[data-job-footer]')
    await expect(footer).toBeVisible()
    expect(await page.evaluate(() => matchMedia('(pointer: coarse)').matches)).toBe(true)
    for (const target of [footer.locator('[data-job-model] button'), footer.locator('[data-job-time]')]) {
      const bounds = await target.boundingBox()
      expect(bounds!.height).toBeGreaterThanOrEqual(40)
      expect(bounds!.width).toBeGreaterThanOrEqual(40)
    }
    // One 16 px text line, 8 px padding above and below, and the top border:
    // the 40 px targets do not grow the footer.
    expect((await footer.boundingBox())!.height).toBeLessThanOrEqual(33.5)
    await footer.locator('[data-job-time]').tap()
    await expect(footer.locator('[data-job-started]')).toHaveText(/^Started /)
    await expect(footer.locator('[data-job-finished]')).toHaveText(/^Finished /)
    await expectNoSidewaysScroll(page)
  })
})

// An unsettled round whose last seat has no verdict yet. That seat's job is
// still running unless `lastJobEnded` gives it a recorded end.
function openRound(models = ['claude-opus-5-5', 'gpt-6.1-sol'], lastJobEnded = false) {
  const item = reviewPanel(models.map((model) => ({ model, verdict: 'approve' }))) as {
    summaries: Record<string, { by_order?: Record<string, unknown> }>
    work_orders: WorkOrder[]
    jobs: Record<string, unknown>[]
  }
  delete item.summaries.review_rounds
  const last = models.length - 1
  delete item.summaries.review_results.by_order?.[`${taskId}-review-1-seat-${models.length}`]
  item.work_orders[last] = { ...item.work_orders[last], state: 'claimed' }
  if (!lastJobEnded) {
    const { ended_at: _ended, ...running } = item.jobs[last]
    item.jobs[last] = { ...running, state: 'running' }
  }
  return item
}

// The open-round footer holds only the verdict count and the runtime. The
// runtime's disclosure wraps its trigger, so the strip's second child is the
// disclosure.
async function expectOpenFooter(panel: Locator, count: string) {
  const footer = panel.locator('[data-review-time]').locator('xpath=../..')
  await expect(footer.locator(':scope > *')).toHaveCount(2)
  await expect(footer.locator(':scope > span').first()).toHaveText(count)
  await expect(footer).not.toContainText('·')
  await expect(footer).not.toContainText('changes requested')
  await expect(footer).not.toContainText('sends the task back')
}

// A settled panel's runtime opens the convened and settled times on focus,
// and its accessible name states the runtime first.
async function expectSettledPanelTime(time: Locator, runtime: string) {
  await expect(time).toHaveText(runtime)
  await expect(time).not.toHaveAttribute('data-runtime-live')
  await expect(time).toHaveAccessibleName(
    new RegExp(
      `^Took ${runtime}\\. Convened [A-Z][a-z]{2} \\d{1,2}, .*[AP]M\\. Settled [A-Z][a-z]{2} \\d{1,2}, .*[AP]M\\.$`,
    ),
  )
  await time.focus()
  const details = time.locator('xpath=..').locator('[data-review-time-details]')
  await expect(details.locator('[data-review-convened]')).toHaveText(
    /^Convened [A-Z][a-z]{2} \d{1,2}, \d{1,2}:\d{2}\s?[AP]M$/,
  )
  await expect(details.locator('[data-review-settled]')).toHaveText(
    /^Settled [A-Z][a-z]{2} \d{1,2}, \d{1,2}:\d{2}\s?[AP]M$/,
  )
  await time.blur()
}

// An open panel's runtime reads "Open for …" and advances. The convened time
// sits behind it.
async function expectOpenPanelTime(time: Locator) {
  await expect(time).toHaveText(/^Open for \d+(h \d{2}m|m \d{2}s|s)$/)
  await expect(time).toHaveAttribute('data-runtime-live', '')
  await expect(time).toHaveAccessibleName(
    /^Open for \S+( \S+)?\. Convened [A-Z][a-z]{2} \d{1,2}, .*[AP]M\. Not settled yet\.$/,
  )
  await time.focus()
  const details = time.locator('xpath=..').locator('[data-review-time-details]')
  await expect(details.locator('[data-review-convened]')).toHaveText(
    /^Convened [A-Z][a-z]{2} \d{1,2}, \d{1,2}:\d{2}\s?[AP]M$/,
  )
  await expect(details.locator('[data-review-open]')).toHaveText('Not settled yet')
  await expect(details.locator('[data-review-settled]')).toHaveCount(0)
  await time.blur()
}

for (const width of [375, 1280]) {
  test(`the review panel keeps its time in the bottom strip at ${width} px`, async ({ page }, testInfo) => {
    const settled = await openPanel(page, threeSeatApproved(), width)
    const header = settled.locator('[data-review-header]')
    await expect(header.locator('time')).toHaveCount(0)
    await expect(header).not.toContainText('Settled')
    const banner = settled.locator('[data-review-banner]')
    const strip = banner.locator('xpath=..')
    // The settled panel shows its runtime from convened to settled, and no
    // clock time on its face (DEC-127(1)).
    await expectSettledPanelTime(strip.locator('[data-review-time]'), '15m 00s')
    await expect(strip.locator('[data-review-time]')).toHaveCSS('font-size', '11px')
    const [bannerBox, timeBox] = [await box(banner), await box(strip.locator('[data-review-time]'))]
    expect(timeBox.x).toBeGreaterThanOrEqual(bannerBox.x + bannerBox.width - 1)
    // Below 28rem the panel text and seat dots take their own line.
    const narrow = (await settled.evaluate((element) => element.getBoundingClientRect().width)) < 448
    const stageBox = await box(header.getByText('Code review', { exact: true }))
    const factsBox = await box(header.locator('[data-review-panel-facts]'))
    if (narrow) expect(factsBox.y).toBeGreaterThanOrEqual(stageBox.y + stageBox.height - 1)
    else expect(factsBox.y).toBeLessThan(stageBox.y + stageBox.height)
    expect(narrow).toBe(width === 375)
    await expectNoSidewaysScroll(page)
    await capture(page, testInfo, `review-panel-settled-${width}.png`)

    const open = await openPanel(page, openRound(), width)
    await expect(open.locator('[data-review-header] time')).toHaveCount(0)
    await expectOpenPanelTime(open.locator('[data-review-time]'))
    await expectOpenFooter(open, '1 of 2 verdicts in')
    await expectNoSidewaysScroll(page)
    await capture(page, testInfo, `review-panel-open-${width}.png`)
  })
}

test('a one-seat open review round keeps its footer of count and time', async ({ page }) => {
  const open = await openPanel(page, openRound(['claude-opus-5-5']), 375)
  const convened = open.locator('[data-review-time]')
  await expectOpenPanelTime(convened)
  await expectOpenFooter(open, '0 of 1 verdicts in')
  // The time keeps its place at the end of the strip.
  const count = await box(open.getByText('0 of 1 verdicts in', { exact: true }))
  const time = await box(convened)
  expect(time.x).toBeGreaterThanOrEqual(count.x + count.width)
  await expectNoSidewaysScroll(page)
})

// ---- Runtime on the card face (DEC-127(1); component-web-task-surfaces invariants 40 to 43) ----
//
// A card for work with a start and an end shows its runtime, and its clock
// times sit behind the runtime. A running runtime advances once a second from
// its own timer. The probe below counts the page's one-second timers that are
// still active; it wraps the clock's fake timers, so it installs after them.

async function installTimerProbe(page: Page) {
  await page.addInitScript(() => {
    const active = new Set<number>()
    const set = window.setInterval.bind(window)
    const clear = window.clearInterval.bind(window)
    window.setInterval = ((handler: TimerHandler, timeout?: number, ...rest: unknown[]) => {
      const id = set(handler, timeout, ...rest)
      if (timeout === 1000) active.add(id)
      return id
    }) as typeof window.setInterval
    window.clearInterval = ((id?: number) => {
      if (id !== undefined) active.delete(id)
      clear(id)
    }) as typeof window.clearInterval
    ;(window as unknown as { __secondTimers: () => number }).__secondTimers = () => active.size
  })
}

const secondTimers = (page: Page) =>
  page.evaluate(() => (window as unknown as { __secondTimers: () => number }).__secondTimers())

// A running Implementation card, a finished narrated plan card, a compact
// completed triage row, and an operator decision.
function runtimeItem() {
  return activity({
    jobs: [
      job(`${taskId}-triage-1`, 'triage', 'done', { started_at: minutes(0), ended_at: minutes(1) }),
      job(`${taskId}-spec-1`, 'spec', 'done', { started_at: minutes(1), ended_at: minutes(3) }),
      job(`${taskId}-implement-1`, 'implement', 'running', { started_at: minutes(6) }),
    ],
    work_orders: [
      order(`${taskId}-implement-1`, {
        attempt_id: 'attempt-current',
        session_id: 'sess-current',
        agent: 'claude-code',
        model: 'claude-opus-5-5',
        progress: 'Moving the clock times behind the runtime',
      }),
    ],
    interventions: [
      {
        id: 7,
        task_id: taskId,
        actor_id: 'user:usr_operator',
        actor_role: 'human',
        action: 'pull_to_local',
        reason_code: 'operator-pull',
        comment: 'Pulled to check the footer on a phone.',
        at: minutes(4),
      },
    ],
    summaries: {
      job_results: {
        [`${taskId}-spec-1`]: {
          stage_summary: { text: 'Plan v1 moves the times behind the runtime.', event_id: '11' },
        },
      },
    },
  }) as ReturnType<typeof activity> & { jobs: Record<string, unknown>[] }
}

async function openRuntimeTask(page: Page, item: object, width = 1280) {
  await page.setViewportSize({ width, height: 1400 })
  await page.clock.install({ time: new Date(minutes(9)) })
  await installTimerProbe(page)
  await mockAPI(page, item, nextAction(`${taskId}-implement-1`))
  // Data reads only. The mocked notify streams answer 204, so their
  // reconnects with backoff are not reads a tick could cause.
  const reads: string[] = []
  page.on('request', (request) => {
    const path = new URL(request.url()).pathname
    if (path.startsWith('/v1/') && !path.endsWith('/events/stream') && path !== '/v1/workspaces/demo/changes')
      reads.push(path)
  })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  const running = timeline.locator('li article').filter({ hasText: 'Moving the clock times behind the runtime' })
  await expect(running).toHaveCount(1)
  await page.clock.pauseAt(new Date(minutes(10)))
  return { timeline, running, reads }
}

test('a running Implementation card shows its runtime, which advances each second without a read', async ({ page }) => {
  const { timeline, running, reads } = await openRuntimeTask(page, runtimeItem())
  const face = running.locator('[data-job-footer] [data-job-time]')
  await expect(face).toHaveText('Running for 4m 00s')
  await expectRunningTime(running.locator('[data-job-footer]'))
  // Hover opens the start time too.
  await face.hover()
  await expect(running.locator('[data-job-started]')).toBeVisible()
  await page.mouse.move(0, 0)
  await expect(running.locator('[data-job-time-details]')).toHaveCount(0)

  const finished = timeline.locator('li article').filter({ hasText: 'Plan v1 moves the times behind the runtime.' })
  const finishedFace = finished.locator('[data-job-footer] [data-job-time]')
  await expect(finishedFace).toHaveText('2m 00s')
  await expectFinishedTime(finished.locator('[data-job-footer]'))

  // Only the running runtime holds a one-second timer. A tick sends no
  // request and leaves the finished runtimes as they were.
  expect(await secondTimers(page)).toBe(1)
  await expect(timeline.locator('[data-runtime-live]')).toHaveCount(1)
  const before = reads.length
  await page.clock.runFor(3_000)
  await expect(face).toHaveText('Running for 4m 03s')
  await expect(finishedFace).toHaveText('2m 00s')
  await page.clock.runFor(60_000)
  await expect(face).toHaveText('Running for 5m 03s')
  expect(reads.length).toBe(before)
  expect(await secondTimers(page)).toBe(1)

  // An operator decision is one instant, so it keeps its clock time.
  const decision = timeline.locator('li').filter({ hasText: 'Pulled to check the footer on a phone.' })
  await expect(decision.locator('time')).toHaveText(shortTime)

  // Leaving the timeline unmounts the runtime and clears its timer.
  await page.getByRole('tab', { name: 'Flow', exact: true }).click()
  await expect(timeline).toHaveCount(0)
  expect(await secondTimers(page)).toBe(0)
})

test('a page with only finished runtimes starts no timer', async ({ page }) => {
  await page.clock.install({ time: new Date(minutes(90)) })
  await installTimerProbe(page)
  await mockAPI(page, deliveredTask(), {
    ...nextAction(implementID(2)),
    label: 'Finished',
    actor: 'none',
    kind: 'terminal',
    current_order: undefined,
    attempt: undefined,
  })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page
    .getByRole('region', { name: 'Execution event timeline' })
    .getByRole('article', { name: 'Implementation attempt 2' })
  const face = card.locator('[data-job-footer] [data-job-time]')
  await expect(face).toHaveText('20m 00s')
  await page.clock.runFor(5_000)
  await expect(face).toHaveText('20m 00s')
  await expect(page.locator('[data-runtime-live]')).toHaveCount(0)
  expect(await secondTimers(page)).toBe(0)
})

test('a running runtime stops and shows its duration when the job finishes on a live refresh', async ({ page }) => {
  const streams = await controlTaskStreams(page)
  const item = runtimeItem()
  const { running } = await openRuntimeTask(page, item)
  const face = running.locator('[data-job-footer] [data-job-time]')
  await expect(face).toHaveText('Running for 4m 00s')
  expect(await secondTimers(page)).toBe(1)
  await expect.poll(() => streams.countFor(taskId)).toBeGreaterThan(0)
  await streams.open()
  Object.assign(item.jobs[2], { state: 'done', ended_at: minutes(10) })
  await streams.send(activityFrame)
  await page.clock.runFor(250)
  // Query results arrive through timers, so the clock runs again to render them.
  await page.clock.resume()
  await expect(face).toHaveText('4m 00s')
  await expect(face).not.toHaveAttribute('data-runtime-live')
  expect(await secondTimers(page)).toBe(0)
})

test('the compact completed row shows its duration and keeps its clock times behind it', async ({ page }) => {
  const { timeline } = await openRuntimeTask(page, runtimeItem())
  const row = timeline.locator('[data-job-completed]').filter({ hasText: 'Triage completed' })
  await expect(row).toHaveCount(1)
  const face = row.locator('[data-job-time]')
  await expect(face).toHaveText('1m 00s')
  // No clock time shows on the row's face.
  await expect(row.locator(':scope > *')).toHaveCount(2)
  await expect(row).not.toContainText(/[AP]M/)
  await expect(face).toHaveAccessibleName(/^Took 1m 00s\. Started .*[AP]M\. Finished .*[AP]M\.$/)
  await face.focus()
  await expect(row.locator('[data-job-started]')).toHaveText(/^Started [A-Z][a-z]{2} \d{1,2}, /)
  await expect(row.locator('[data-job-finished]')).toHaveText(/^Finished [A-Z][a-z]{2} \d{1,2}, /)
  await face.blur()
  await face.hover()
  await expect(row.locator('[data-job-time-details]')).toBeVisible()
})

test('an open review panel shows its runtime from convened, which advances each second', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.clock.install({ time: new Date(minutes(9)) })
  await installTimerProbe(page)
  await mockAPI(page, openRound(), nextAction(`${taskId}-review-1-seat-2`, 'review'))
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const panel = page.getByRole('region', { name: 'Execution event timeline' }).locator('li article')
  await expect(panel).toHaveCount(1)
  await page.clock.pauseAt(new Date(minutes(10)))
  const time = panel.locator('[data-review-time]')
  await expect(time).toHaveText('Open for 5m 00s')
  await expectOpenPanelTime(time)
  // The deliberating seat's elapsed time advances with the round.
  const seat = panel.locator('[data-seat-runtime]')
  await expect(seat).toHaveCount(1)
  await expect(seat).toHaveText('4m 00s')
  await page.clock.runFor(2_000)
  await expect(time).toHaveText('Open for 5m 02s')
  await expect(seat).toHaveText('4m 02s')
  // One timer for the round and one for the deliberating seat.
  expect(await secondTimers(page)).toBe(2)
  await time.hover()
  await expect(panel.locator('[data-review-convened]')).toBeVisible()
})

// A seat whose job ended before its verdict landed still reads as
// deliberating. Its elapsed time stops at the recorded end and holds no timer.
test('a deliberating seat whose job has ended shows its measured duration', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.clock.install({ time: new Date(minutes(19)) })
  await installTimerProbe(page)
  await mockAPI(page, openRound(undefined, true), nextAction(`${taskId}-review-1-seat-2`, 'review'))
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const panel = page.getByRole('region', { name: 'Execution event timeline' }).locator('li article')
  await expect(panel).toHaveCount(1)
  await page.clock.pauseAt(new Date(minutes(20)))
  await expect(panel.getByText('Deliberating')).toBeVisible()
  const seat = panel.locator('[data-seat-runtime]')
  await expect(seat).toHaveText('9m 00s')
  await page.clock.runFor(3_000)
  await expect(seat).toHaveText('9m 00s')
  await expect(panel.locator('[data-review-time]')).toHaveText('Open for 15m 03s')
  // Only the round's runtime holds a timer.
  expect(await secondTimers(page)).toBe(1)
})

test.describe('on a coarse pointer at 375 px, runtimes', () => {
  test.use({ viewport: { width: 375, height: 1400 }, isMobile: true, hasTouch: true })

  test('open their clock times on tap', async ({ page }) => {
    const { timeline, running } = await openRuntimeTask(page, runtimeItem(), 375)
    await running.locator('[data-job-footer] [data-job-time]').tap()
    await expect(running.locator('[data-job-started]')).toBeVisible()
    await expect(running.locator('[data-job-running]')).toHaveText('Still running')
    const row = timeline.locator('[data-job-completed]').filter({ hasText: 'Triage completed' })
    await row.locator('[data-job-time]').tap()
    await expect(row.locator('[data-job-finished]')).toBeVisible()
    await expectNoSidewaysScroll(page)
  })

  test('on a settled review panel open the convened and settled times on tap', async ({ page }) => {
    const settled = await openPanel(page, threeSeatApproved(), 375)
    const time = settled.locator('[data-review-time]')
    await time.tap()
    await expect(settled.locator('[data-review-convened]')).toBeVisible()
    await expect(settled.locator('[data-review-settled]')).toBeVisible()
    const bounds = await settled.locator('[data-review-time-details]').boundingBox()
    expect(bounds!.x).toBeGreaterThanOrEqual(0)
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(375)
  })
})

// Visual record for the pull request: a running Implementation card, a
// finished job card, and the compact completed row, at 375 px and at a desktop
// width. The clock is fixed so the runtimes read the same on every run.
for (const width of [375, 1280]) {
  test(`runtime screenshots at ${width} px`, async ({ page }, testInfo) => {
    const { running } = await openRuntimeTask(page, runtimeItem(), width)
    await expect(running.locator('[data-job-footer] [data-job-time]')).toHaveText('Running for 4m 00s')
    await page.evaluate(() => document.fonts.ready)
    await expectNoSidewaysScroll(page)
    await capture(page, testInfo, `runtime-running-${width}.png`)
    await running.locator('[data-job-footer] [data-job-time]').focus()
    await expect(running.locator('[data-job-time-details]')).toBeVisible()
    await capture(page, testInfo, `runtime-running-open-${width}.png`)
  })
}
