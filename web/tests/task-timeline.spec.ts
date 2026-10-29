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
    if (path === '/v1/activity')
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
  await page.goto(`/w/demo/tasks/${taskId}/full`)
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

// A task-context decision is the operator's audited act, so it reads as a note
// naming the document; a payload without target fields, or an omitted payload,
// keeps a readable title and the audit disclosure (req-operator-next-action
// AC-2.2; component-web-task-surfaces, Work-order timeline entries).
test('task-context decisions read as Context confirmed and Context dismissed notes', async ({ page }) => {
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
    ],
  })
  await mockAPI(page, item, nextAction(`${taskId}-implement-1`))
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('Context confirmed: requirement req-review-gates-evidence')).toBeVisible()
  await expect(timeline.getByText('Context dismissed: System Design component-web-dashboard')).toBeVisible()
  await expect(timeline.getByText('Context confirmed', { exact: true })).toHaveCount(1)
  await expect(timeline.getByText('Context dismissed', { exact: true })).toHaveCount(1)
  await expect(timeline.getByText('Details omitted from this page')).toHaveCount(1)
  await expect(timeline).not.toContainText('undefined')
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
      await expect(header.getByText('claude-opus-5-5', { exact: true })).toBeVisible()
      await expect(header.locator('[data-job-duration]')).toHaveText(elapsed)
      const delivery = card.locator('[data-implementation-delivery]')
      await expect(delivery.getByRole('link', { name: '#1431' })).toHaveAttribute('href', prURL)
      await expect(delivery.getByText(head, { exact: true })).toBeVisible()
    }
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
    name: 'shortened summary with a transcript',
    jobState: 'done',
    extra: { ended_at: minutes(9) },
    order: {
      state: 'submitted',
      head_sha: firstHead,
      transcript_captures: [
        {
          attempt_id: 'attempt-impl',
          content: 'transcript body',
          termination_reason: 'completed',
          truncated: false,
          captured_at: minutes(9),
        },
      ],
    },
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
    if (scenario.name === 'shortened summary with a transcript') {
      await expect(card.locator('strong', { hasText: 'card' })).toBeVisible()
      await expect(card.getByText('Full text (shortened here)')).toBeVisible()
      await expect(card.getByText('Attempt attempt-impl transcript')).toBeVisible()
    }
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
  await expect(author).toContainText('By agent:agt_coordinator')
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
  await expect(summarized.getByText('Changes requested — 1 of 2 seats')).toBeVisible()
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
  await expect(summary.locator('[data-review-summary-author]')).toContainText('By user:usr_operator')
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
