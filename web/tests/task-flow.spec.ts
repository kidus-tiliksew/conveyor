import { expect, type Locator, type Page, type Route, test } from '@playwright/test'
import type { NextAction, TaskFlow, TaskFlowAttempt, TaskFlowSideName, TaskFlowStage } from '../src/lib/types'
import { callerIdentity } from './helpers/caller-identity'
import { waitForSheetSettled } from './helpers/sheet'
import { activityFrame, controlTaskStreams } from './helpers/task-streams'

// Task detail's Timeline and Flow tabs and the task flowchart
// (req-operator-next-action AC-7.4 to AC-7.7; feature-operator-next-action
// NA-6, NA-7; component-web-task-surfaces, Activity tabs and the task flow
// view). `/flow` and `/next-action` are route-intercepted with server-shaped
// bodies whose words differ from anything the client could carry, so every
// assertion proves the chart draws what the server stated and derives nothing.

const t0 = '2026-10-16T08:00:00Z'
const at = (minutes: number) => new Date(Date.parse(t0) + minutes * 60_000).toISOString()
const taskId = 'flow-task'
const otherId = 'flow-other'

function activity(id: string, title: string) {
  return {
    task: {
      id,
      workspace: 'demo',
      title,
      state: 'running',
      repo: 'conveyor',
      branch: `conveyor/${id}`,
      created_at: id === taskId ? at(1) : at(0),
    },
    jobs: [],
    events: [],
    work_orders: [],
    reviews: [],
    interventions: [],
    latest_stage: 'implement',
    plan: { state: 'approved', version: 1 },
    last_event_at: at(30),
    needs_attention: false,
  }
}

function nextAction(extra: Partial<NextAction> = {}): NextAction {
  return {
    label: 'Server working label',
    actor: 'agent',
    kind: 'working',
    judgment: false,
    summary: 'Server summary: the agent is implementing the change.',
    deadline: null,
    choices: [],
    details: {},
    ...extra,
  }
}

const statusWords = {
  done: 'Server done',
  in_progress: 'Server running',
  waiting: 'Server queued',
  needs_operator: 'Server needs you',
  upcoming: 'Server later',
  off: 'Server off',
} as const

function stage(id: TaskFlowStage['id'], status: TaskFlowStage['status'], extra: Partial<TaskFlowStage> = {}) {
  const gated = id === 'spec' || id === 'merge'
  return {
    id,
    label: `Server ${id}`,
    gate: gated ? (id === 'spec' ? 'plan_approval' : 'merge_approval') : null,
    gate_label: gated ? `Server ${id} gate` : '',
    gate_status: gated ? 'upcoming' : null,
    gate_status_label: gated ? statusWords.upcoming : '',
    gate_status_text: gated ? `Server ${id} gate sentence.` : '',
    status,
    description: `Server description of ${id}.`,
    status_label: statusWords[status],
    status_text: `Server sentence: ${id} is ${status}.`,
    missed_check_in: '',
    traversed: status !== 'upcoming' && status !== 'off',
    gate_traversed: false,
    attempts: [],
    current_attempt: null,
    ...extra,
  } satisfies TaskFlowStage
}

function gate(status: TaskFlowStage['status'], traversed = status !== 'upcoming' && status !== 'off') {
  return { gate_status: status, gate_status_label: statusWords[status], gate_traversed: traversed }
}

function attempt(n: number, extra: Partial<TaskFlowAttempt> = {}): TaskFlowAttempt {
  return {
    order_id: `${taskId}-implement-${n}`,
    attempt_id: `attempt-${n}`,
    started_at: at(10 * n),
    ended_at: at(10 * n + 5),
    outcome: 'timed_out',
    superseded: false,
    auto: n > 1,
    ...extra,
  }
}

const sideLabels: Record<TaskFlowSideName, string> = {
  blocked_by: 'Server waiting on another task',
  retrying: 'Server auto-retrying',
  parked: 'Server parked',
  closed: 'Server closed',
  agent_question: 'Server agent asks you',
}

// A task implementing after one review and one verification bounce, with the
// plan gate passed and the merge gate ahead.
function baseFlow(prefix = 'Server'): TaskFlow {
  return {
    stages: [
      stage('triage', 'done'),
      stage('spec', 'done', gate('done')),
      stage('implement', 'in_progress', {
        label: `${prefix} implement`,
        missed_check_in: 'Server says the lease ran out at 2026-10-16 09:10 UTC.',
        attempts: [
          attempt(1, { superseded: true, outcome: 'retired' }),
          attempt(2),
          attempt(3, { ended_at: null, outcome: '' }),
        ],
        current_attempt: {
          harness: 'claude-code',
          model: 'claude-opus-5-5',
          user: 'operator@example.test',
          claimed_by: 'run:usr_operator',
          session_id: 'sess-flow-current',
          attempt_id: 'attempt-3',
          claimed_at: at(30),
          last_check_in: at(60),
          lease_expires_at: at(70),
          progress: 'Server progress: wiring the stage panel',
          elapsed_seconds: 3900,
        },
      }),
      stage('verify', 'done'),
      stage('review', 'done'),
      stage('merge', 'upcoming'),
    ],
    loops: [
      { from: 'review', to: 'implement', count: 2, label: 'Server sent back ×2' },
      { from: 'verify', to: 'implement', count: 1, label: 'Server sent back ×1' },
    ],
    side: { blocked_by: [], retrying: null, parked: false, closed: false, agent_question: false, labels: {} },
    current_stage: 'implement',
    as_of: at(95),
    display: {
      legend: [
        { status: 'done', label: 'Server legend done' },
        { status: 'in_progress', label: 'Server legend in progress' },
        { status: 'waiting', label: 'Server legend waiting' },
        { status: 'needs_operator', label: 'Server legend approval' },
        { status: 'off', label: 'Server legend skipped' },
      ],
      side_labels: sideLabels,
      skipped_label: 'server skipped',
      plan_bypass_label: 'Server bypass',
      from_any_step_label: 'Server from any step',
    },
    next_action: { label: 'Server working label', actor: 'agent', kind: 'working', summary: 'unused' },
    raw: { state: 'running', next_stage: 'implement', recovery_stage: '', gate_kind: '', order_ids: [] },
  }
}

type Api = {
  flowCalls: string[]
  detailCalls: string[]
  // The served task title, so a test can observe a refreshed detail land.
  titles: Record<string, string>
  hold: (id: string) => void
  release: (id: string) => Promise<void>
}

async function mockAPI(
  page: Page,
  { flows, action = nextAction() }: { flows: Record<string, TaskFlow>; action?: NextAction },
): Promise<Api> {
  const flowCalls: string[] = []
  const held = new Map<string, Route[]>()
  const holding = new Set<string>()
  const titles: Record<string, string> = { [taskId]: 'Draw the task flow', [otherId]: 'Another task' }
  const detailCalls: string[] = []
  const items = [activity(taskId, titles[taskId]), activity(otherId, titles[otherId])]
  await page.addInitScript(() => {
    if (!localStorage.getItem('conveyor-workspace')) localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname
    if (path === '/v1/workspaces')
      return route.fulfill({
        json: [
          { id: 'demo', name: 'Demo' },
          { id: 'other', name: 'Other' },
        ],
      })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'operator', email: 'operator@example.test', role: 'operator' }, route.request()),
      })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    const flow = path.match(/^\/v1\/tasks\/([^/]+)\/flow$/)
    if (flow) {
      flowCalls.push(`${flow[1]}@${url.searchParams.get('workspace_id')}`)
      if (holding.has(flow[1])) {
        held.set(flow[1], [...(held.get(flow[1]) ?? []), route])
        return
      }
      return route.fulfill({ json: { flow: flows[flow[1]] ?? baseFlow() } })
    }
    if (/^\/v1\/tasks\/[^/]+\/next-action$/.test(path)) return route.fulfill({ json: action })
    const detail = path.match(/^\/v1\/tasks\/([^/]+)\/activity$/)
    if (detail) {
      detailCalls.push(detail[1])
      const item = items.find((candidate) => candidate.task.id === detail[1]) ?? items[0]
      return route.fulfill({ json: { ...item, task: { ...item.task, title: titles[item.task.id] } } })
    }
    if (path.endsWith('/verification'))
      return route.fulfill({
        json: { head_sha: '', current_context_id: '', contexts: { items: [], next_cursor: '' }, overview: {} },
      })
    if (path.endsWith('/stream')) return route.fulfill({ status: 204 })
    if (path === '/v1/activity')
      return route.fulfill({
        json: items.map((item) => ({ ...item, next_action: { ...action, choices: undefined } })),
        headers: { 'X-Conveyor-Total': '2', 'X-Conveyor-Limit': '100', 'X-Conveyor-Offset': '0' },
      })
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
    return route.fulfill({ json: [] })
  })
  return {
    flowCalls,
    detailCalls,
    titles,
    hold: (id) => {
      holding.add(id)
    },
    release: async (id) => {
      holding.delete(id)
      // A navigation may already have discarded the held request.
      for (const route of held.get(id) ?? [])
        await route.fulfill({ json: { flow: flows[id] ?? baseFlow() } }).catch(() => undefined)
      held.delete(id)
    },
  }
}

const surfaces = [
  { name: 'Board sheet', url: (id: string) => `/w/demo/tasks/${id}` },
  { name: 'full task route', url: (id: string) => `/w/demo/tasks/${id}/full` },
] as const

const tab = (page: Page, name: 'Timeline' | 'Flow') => page.getByRole('tab', { name, exact: true })
const chart = (page: Page) => page.locator('[data-flow-chart]')
const node = (page: Page, id: string) => page.locator(`[data-flow-node="${id}"]`)
const gateNode = (page: Page, id: string) => page.locator(`[data-flow-gate="${id}"]`)
const sideNode = (scope: Page | Locator, name: TaskFlowSideName) => scope.locator(`[data-flow-side="${name}"]`)

async function openFlow(page: Page, url: string) {
  await page.goto(url)
  await tab(page, 'Flow').click()
  await expect(chart(page)).toBeVisible()
}

for (const surface of surfaces) {
  test(`the ${surface.name} shows Timeline by default below the next-action card and reads no flow`, async ({
    page,
  }) => {
    const api = await mockAPI(page, { flows: {} })
    await page.goto(surface.url(taskId))
    await expect(page.getByRole('region', { name: 'Execution event timeline' })).toBeVisible()
    await expect(tab(page, 'Timeline')).toHaveAttribute('aria-selected', 'true')
    await expect(tab(page, 'Flow')).toHaveAttribute('aria-selected', 'false')
    const card = page.getByRole('region', { name: 'Next action' })
    await expect(card).toBeVisible()
    // The tablist follows the next-action card.
    const follows = await card.evaluate(
      (element, other) => Boolean(other && element.compareDocumentPosition(other) & Node.DOCUMENT_POSITION_FOLLOWING),
      await page.getByRole('tablist', { name: 'Task activity view' }).elementHandle(),
    )
    expect(follows).toBe(true)
    await expect(chart(page)).toHaveCount(0)
    expect(api.flowCalls).toEqual([])
  })

  test(`the ${surface.name} reads the flow only while the Flow tab is shown`, async ({ page }) => {
    const api = await mockAPI(page, { flows: { [taskId]: baseFlow() } })
    await openFlow(page, surface.url(taskId))
    expect(api.flowCalls.length).toBeGreaterThan(0)
    expect(api.flowCalls.every((call) => call === `${taskId}@demo`)).toBe(true)
    await expect(page.getByRole('region', { name: 'Execution event timeline' })).toHaveCount(0)
    await tab(page, 'Timeline').click()
    await expect(page.getByRole('region', { name: 'Execution event timeline' })).toBeVisible()
    await expect(chart(page)).toHaveCount(0)
    const shown = api.flowCalls.length
    // Detail refreshes while Timeline is shown reach no flow reader, and the
    // persisted Timeline choice reads none after a reload either.
    await page.evaluate(() => window.dispatchEvent(new Event('focus')))
    await page.reload()
    await expect(page.getByRole('region', { name: 'Execution event timeline' })).toBeVisible()
    await expect(tab(page, 'Timeline')).toHaveAttribute('aria-selected', 'true')
    expect(api.flowCalls).toHaveLength(shown)
  })
}

test('the tab choice persists per browser across reloads and surfaces', async ({ page }) => {
  const api = await mockAPI(page, { flows: { [taskId]: baseFlow() } })
  await openFlow(page, `/w/demo/tasks/${taskId}`)
  expect(await page.evaluate(() => localStorage.getItem('conveyor-task-activity-view'))).toBe('flow')
  await page.reload()
  await expect(tab(page, 'Flow')).toHaveAttribute('aria-selected', 'true')
  await expect(chart(page)).toBeVisible()
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await expect(tab(page, 'Flow')).toHaveAttribute('aria-selected', 'true')
  await expect(chart(page)).toBeVisible()
  await tab(page, 'Timeline').click()
  const shown = api.flowCalls.length
  await page.goto(`/w/demo/tasks/${taskId}`)
  await expect(tab(page, 'Timeline')).toHaveAttribute('aria-selected', 'true')
  await expect(page.getByRole('region', { name: 'Execution event timeline' })).toBeVisible()
  expect(api.flowCalls).toHaveLength(shown)
})

test('malformed and inaccessible storage fall back to Timeline', async ({ page }) => {
  const api = await mockAPI(page, { flows: {} })
  await page.addInitScript(() => localStorage.setItem('conveyor-task-activity-view', '{"view":"flow"}'))
  await page.goto(`/w/demo/tasks/${taskId}`)
  await expect(tab(page, 'Timeline')).toHaveAttribute('aria-selected', 'true')

  const blocked = await page.context().newPage()
  await mockAPI(blocked, { flows: {} })
  await blocked.addInitScript(() => {
    const getItem = Storage.prototype.getItem
    const setItem = Storage.prototype.setItem
    Storage.prototype.getItem = function (key: string) {
      if (key === 'conveyor-task-activity-view') throw new DOMException('denied', 'SecurityError')
      return getItem.call(this, key)
    }
    Storage.prototype.setItem = function (key: string, value: string) {
      if (key === 'conveyor-task-activity-view') throw new DOMException('denied', 'QuotaExceededError')
      return setItem.call(this, key, value)
    }
  })
  await blocked.goto(`/w/demo/tasks/${taskId}/full`)
  await expect(tab(blocked, 'Timeline')).toHaveAttribute('aria-selected', 'true')
  // A refused write keeps the choice for the page.
  await tab(blocked, 'Flow').click()
  await expect(chart(blocked)).toBeVisible()
  expect(api.flowCalls).toEqual([])
})

test('the tabs follow the keyboard', async ({ page }) => {
  await mockAPI(page, { flows: {} })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await tab(page, 'Timeline').focus()
  await page.keyboard.press('ArrowRight')
  await expect(tab(page, 'Flow')).toBeFocused()
  await expect(tab(page, 'Flow')).toHaveAttribute('aria-selected', 'true')
  await expect(tab(page, 'Flow')).toHaveAttribute('tabindex', '0')
  await expect(tab(page, 'Timeline')).toHaveAttribute('tabindex', '-1')
  await page.keyboard.press('Home')
  await expect(tab(page, 'Timeline')).toBeFocused()
  await page.keyboard.press('End')
  await expect(tab(page, 'Flow')).toHaveAttribute('aria-selected', 'true')
  await page.keyboard.press('ArrowLeft')
  await expect(tab(page, 'Timeline')).toHaveAttribute('aria-selected', 'true')
  const panel = page.getByRole('tabpanel')
  await expect(panel).toHaveAttribute('aria-labelledby', (await tab(page, 'Timeline').getAttribute('id')) ?? '')
})

test('the chart draws the served path, statuses, gate marks, legend, and loops', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  const flow = baseFlow()
  flow.stages[5] = stage('merge', 'waiting', { ...gate('needs_operator'), traversed: false })
  flow.stages[3] = stage('verify', 'off', { traversed: true })
  await mockAPI(page, { flows: { [taskId]: flow } })
  await openFlow(page, `/w/demo/tasks/${taskId}/full`)
  const legend = page.getByRole('list', { name: 'Legend' })
  await expect(legend.getByRole('listitem')).toHaveText([
    'Server legend done',
    'Server legend in progress',
    'Server legend waiting',
    'Server legend approval',
    'Server legend skipped',
  ])
  // The eight-node main path, in order, named by the server.
  const order = await page
    .locator('[data-flow-node], [data-flow-gate]')
    .evaluateAll((elements) =>
      elements.map((element) => `${element.getAttribute('data-flow-node') ? 'stage' : 'gate'}:${element.textContent}`),
    )
  expect(order.map((entry) => entry.replace(/Server (done|running|queued|needs you|later|off)$/, ''))).toEqual([
    'stage:Server triage',
    'stage:Server spec',
    'gate:Server spec gate',
    'stage:Server implement',
    'stage:Server verifyserver skipped',
    'stage:Server review',
    'gate:Server merge gate',
    'stage:Server merge',
  ])
  const statuses = {
    triage: 'done',
    spec: 'done',
    implement: 'in_progress',
    verify: 'off',
    review: 'done',
    merge: 'waiting',
  }
  for (const [id, status] of Object.entries(statuses))
    await expect(node(page, id)).toHaveAttribute('data-status', status)
  await expect(node(page, 'implement')).toHaveAccessibleName('Server implement — Server running')
  await expect(gateNode(page, 'spec')).toHaveAttribute('data-status', 'done')
  await expect(gateNode(page, 'merge')).toHaveAttribute('data-status', 'needs_operator')
  await expect(gateNode(page, 'merge')).toHaveAccessibleName('Server merge gate — Server needs you')
  // Done nodes carry a check badge; others do not.
  await expect(node(page, 'triage').locator('[data-flow-done]')).toHaveCount(1)
  await expect(node(page, 'implement').locator('[data-flow-done]')).toHaveCount(0)
  // A skipped Verify the task has passed reads as traversed.
  await expect(page.locator('[data-flow-edge="stage-implement:stage-verify"]')).toHaveAttribute(
    'data-traversed',
    'true',
  )
  await expect(page.locator('[data-flow-edge="stage-review:gate-merge"]')).toHaveAttribute('data-traversed', 'true')
  await expect(page.locator('[data-flow-edge="gate-merge:stage-merge"]')).toHaveAttribute('data-traversed', 'false')
  // Both loop origins, each in its own lane with its served pill.
  await expect(page.locator('[data-flow-loop="review-implement"]')).toContainText('Server sent back ×2')
  await expect(page.locator('[data-flow-loop="verify-implement"]')).toContainText('Server sent back ×1')
  await expect(page.locator('[data-flow-bypass]')).toHaveCount(0)
  // No next-action summary inside the chart (DEC-61), and no typical duration.
  await expect(page.locator('[data-task-flow]')).not.toContainText('Server summary')
})

// The gate mark is whatever the server stated, never derived from the next
// action's kind or the row status (AC-7.5, AC-7.7).
const gateCases = [
  { name: 'plan approval', kind: 'plan_approval', plan: 'needs_operator', merge: 'upcoming' },
  { name: 'plan revision', kind: 'plan_revision', plan: 'needs_operator', merge: 'upcoming' },
  { name: 'merge approval', kind: 'merge_approval', plan: 'done', merge: 'needs_operator' },
  { name: 'context proposal', kind: 'context_proposal', plan: 'upcoming', merge: 'upcoming' },
  { name: 'agent question', kind: 'agent_question', plan: 'waiting', merge: 'upcoming' },
] as const

for (const gateCase of gateCases) {
  test(`gate marks for a ${gateCase.name} come from the server`, async ({ page }) => {
    const flow = baseFlow()
    flow.stages[1] = stage('spec', 'needs_operator', gate(gateCase.plan))
    flow.stages[5] = stage('merge', 'upcoming', gate(gateCase.merge))
    flow.next_action = { label: 'x', actor: 'operator', kind: gateCase.kind, summary: 'x' }
    await mockAPI(page, {
      flows: { [taskId]: flow },
      action: nextAction({ actor: 'operator', kind: gateCase.kind, label: `Server ${gateCase.name}` }),
    })
    await openFlow(page, `/w/demo/tasks/${taskId}`)
    await expect(node(page, 'spec')).toHaveAttribute('data-status', 'needs_operator')
    await expect(gateNode(page, 'spec')).toHaveAttribute('data-status', gateCase.plan)
    await expect(gateNode(page, 'merge')).toHaveAttribute('data-status', gateCase.merge)
  })
}

test('a plan-off task draws the bypass and skipped plan nodes', async ({ page }) => {
  const flow = baseFlow()
  flow.stages[1] = stage('spec', 'off', { gate: null, ...gate('off', true), traversed: true })
  flow.loops = []
  await mockAPI(page, { flows: { [taskId]: flow } })
  await openFlow(page, `/w/demo/tasks/${taskId}/full`)
  const bypass = page.locator('[data-flow-bypass]')
  await expect(bypass).toContainText('Server bypass')
  await expect(node(page, 'spec')).toContainText('server skipped')
  await expect(gateNode(page, 'spec')).toContainText('server skipped')
  await expect(gateNode(page, 'spec')).toHaveAttribute('data-status', 'off')
  // The bypass runs from Triage down to Implement.
  const [bypassBox, triageBox, implementBox] = await Promise.all([
    bypass.boundingBox(),
    node(page, 'triage').boundingBox(),
    node(page, 'implement').boundingBox(),
  ])
  if (!bypassBox || !triageBox || !implementBox) throw new Error('missing boxes')
  expect(Math.abs(bypassBox.y - (triageBox.y + triageBox.height / 2))).toBeLessThan(3)
  expect(Math.abs(bypassBox.y + bypassBox.height - (implementBox.y + implementBox.height / 2))).toBeLessThan(3)
  expect(bypassBox.x + bypassBox.width).toBeLessThanOrEqual(implementBox.x + 1)
})

const sideCases: { name: TaskFlowSideName; patch: (flow: TaskFlow) => void }[] = [
  {
    name: 'agent_question',
    patch: (flow) => {
      flow.side.agent_question = true
      flow.side.labels = { agent_question: 'Server agent asks you now' }
    },
  },
  {
    name: 'blocked_by',
    patch: (flow) => {
      flow.side.blocked_by = [
        { task_id: 'task-prereq', title: 'Prerequisite slice', state: 'running' },
        { task_id: 'task-second', title: 'Second prerequisite with a long name that wraps', state: 'queued' },
      ]
      flow.side.labels = { blocked_by: 'Server waiting on another task now' }
    },
  },
  {
    name: 'retrying',
    patch: (flow) => {
      flow.side.retrying = { attempt: 2, of: 3 }
      flow.side.labels = { retrying: 'Server auto-retrying · 2 of 3' }
    },
  },
  {
    name: 'parked',
    patch: (flow) => {
      flow.side.parked = true
      flow.side.labels = { parked: 'Server parked now' }
    },
  },
  {
    name: 'closed',
    patch: (flow) => {
      flow.side.closed = true
      flow.side.labels = { closed: 'Server closed now' }
    },
  },
]

for (const sideCase of sideCases) {
  test(`the side state ${sideCase.name} alone is drawn present and connected`, async ({ page }) => {
    // The Board sheet is half of a 1600 px viewport, wide enough for the
    // side column (640 px of chart width).
    await page.setViewportSize({ width: 1600, height: 1000 })
    const flow = baseFlow()
    sideCase.patch(flow)
    await mockAPI(page, { flows: { [taskId]: flow } })
    await openFlow(page, `/w/demo/tasks/${taskId}`)
    await expect(chart(page)).toHaveAttribute('data-flow-layout', 'column')
    for (const other of sideCases) {
      const state = sideNode(page, other.name)
      const present = other.name === sideCase.name
      await expect(state).toHaveAttribute('data-present', String(present))
      await expect(state).toContainText(present ? (flow.side.labels[other.name] ?? '') : sideLabels[other.name])
      await expect(page.locator(`[data-flow-connector="${other.name}"]`)).toHaveCount(present ? 1 : 0)
    }
    await expect(page.locator('[data-flow-connector="stage"]')).toHaveCount(1)
    await expect(chart(page)).toContainText('Server from any step')
    if (sideCase.name === 'blocked_by') {
      const blocker = page.locator('[data-flow-blocker="task-prereq"]')
      await expect(blocker).toHaveText('Prerequisite slice')
      await expect(blocker).toHaveAttribute('href', '/w/demo/tasks/task-prereq')
      await expect(page.locator('[data-flow-blocker="task-second"]')).toBeVisible()
    }
  })
}

test('combined side states and retry limits print the served labels', async ({ page }) => {
  await page.setViewportSize({ width: 1600, height: 1000 })
  const flow = baseFlow()
  flow.side = {
    blocked_by: [{ task_id: 'task-prereq', title: 'Prerequisite slice', state: 'running' }],
    retrying: { attempt: 4, of: null },
    parked: false,
    closed: false,
    agent_question: true,
    labels: {
      blocked_by: 'Server waiting on another task now',
      retrying: 'Server auto-retrying · 4',
      agent_question: 'Server agent asks you now',
    },
  }
  await mockAPI(page, { flows: { [taskId]: flow } })
  await openFlow(page, `/w/demo/tasks/${taskId}`)
  await expect(chart(page)).toHaveAttribute('data-flow-layout', 'column')
  await expect(sideNode(page, 'retrying')).toHaveText('Server auto-retrying · 4')
  await expect(sideNode(page, 'retrying')).not.toContainText(' of ')
  for (const name of ['blocked_by', 'retrying', 'agent_question'] as const)
    await expect(sideNode(page, name)).toHaveAttribute('data-present', 'true')
  for (const name of ['parked', 'closed'] as const)
    await expect(sideNode(page, name)).toHaveAttribute('data-present', 'false')
  await expect(page.locator('[data-flow-connector="bus"]')).toHaveCount(1)
})

for (const surface of surfaces) {
  test(`at 390 px the ${surface.name} folds side states into chips above an unscaled path`, async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    const flow = baseFlow()
    flow.side.blocked_by = [{ task_id: 'task-prereq', title: 'Prerequisite slice with a long title', state: 'running' }]
    flow.side.retrying = { attempt: 2, of: 3 }
    flow.side.labels = { blocked_by: 'Server waiting on another task now', retrying: 'Server auto-retrying · 2 of 3' }
    await mockAPI(page, { flows: { [taskId]: flow } })
    await openFlow(page, surface.url(taskId))
    await expect(chart(page)).toHaveAttribute('data-flow-layout', 'chips')
    const chips = page.locator('[data-flow-chips]')
    await expect(chips.locator('[data-flow-side]')).toHaveCount(5)
    await expect(sideNode(chips, 'retrying')).toHaveText('Server auto-retrying · 2 of 3')
    await expect(sideNode(chips, 'blocked_by')).toContainText('Prerequisite slice with a long title')
    await expect(page.locator('[data-flow-connector]')).toHaveCount(0)
    const [chipsBox, triageBox] = await Promise.all([chips.boundingBox(), node(page, 'triage').boundingBox()])
    if (!chipsBox || !triageBox) throw new Error('missing boxes')
    expect(chipsBox.y + chipsBox.height).toBeLessThanOrEqual(triageBox.y)
    // The main column keeps a readable size: never scaled, at least 8.5 rem.
    for (const id of ['triage', 'implement', 'merge']) {
      const box = await node(page, id).boundingBox()
      expect(box?.width ?? 0).toBeGreaterThanOrEqual(136)
      expect(await node(page, id).evaluate((element) => getComputedStyle(element).transform)).toBe('none')
    }
    // Nothing scrolls sideways: not the page, the sheet body, or the card.
    const overflow = await page.evaluate(() =>
      [document.scrollingElement, ...document.querySelectorAll('.task-sheet-body, [data-task-flow] > div')]
        .filter((element): element is Element => element !== null)
        .map((element) => element.scrollWidth - element.clientWidth),
    )
    expect(Math.max(...overflow)).toBeLessThanOrEqual(0)
  })
}

test('the stage panel states the current attempt and its history, and Escape closes only the panel', async ({
  page,
}, testInfo) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await mockAPI(page, { flows: { [taskId]: baseFlow() } })
  await openFlow(page, `/w/demo/tasks/${taskId}`)
  const taskSheet = page.getByRole('dialog', { name: 'Task detail' })
  await node(page, 'implement').click()
  const panel = page.getByRole('dialog', { name: 'Server implement stage' })
  await waitForSheetSettled(panel)
  await expect(panel).toContainText('Server running')
  await expect(panel).toContainText('Server description of implement.')
  await expect(panel.locator('[data-flow-missed-check-in]')).toHaveText(
    'Server says the lease ran out at 2026-10-16 09:10 UTC.',
  )
  await expect(panel.locator('[data-flow-attempt-who]')).toHaveText(
    'claude-code · claude-opus-5-5 · operator@example.test',
  )
  await expect(panel.locator('[data-flow-attempt-progress]')).toHaveText('Server progress: wiring the stage panel')
  await expect(panel.locator('[data-flow-attempt-elapsed]')).toHaveText('1h 05m')
  await expect(panel.getByText('Claimed', { exact: true })).toBeVisible()
  await expect(panel.getByText('Last check-in', { exact: true })).toBeVisible()
  // IDs wait behind a disclosure, each with its own copy button.
  const ids = panel.locator('[data-flow-attempt-ids]')
  await expect(ids.getByText('sess-flow-current')).toBeHidden()
  await ids.locator('summary').click()
  await expect(ids.getByText('sess-flow-current')).toBeVisible()
  await expect(ids.getByRole('button', { name: 'Copy session ID' })).toBeVisible()
  await expect(ids.getByRole('button', { name: 'Copy attempt ID' })).toBeVisible()
  // Newest first: the latest expanded, earlier attempts collapsed with their
  // superseded count.
  const attempts = panel.getByRole('region', { name: 'Attempts' })
  await expect(attempts.locator('[data-flow-attempt]').first()).toHaveAttribute('data-flow-attempt', '3')
  await expect(attempts.locator('[data-flow-attempt="3"]')).toBeVisible()
  const earlier = attempts.locator('[data-flow-earlier-attempts]')
  await expect(earlier.locator('summary')).toHaveText('2 earlier attempts, 1 superseded')
  await expect(earlier.locator('[data-flow-attempt="2"]')).toBeHidden()
  await earlier.locator('summary').click()
  await expect(earlier.locator('[data-flow-attempt]')).toHaveCount(2)
  await expect(earlier.locator('[data-flow-attempt]').first()).toHaveAttribute('data-flow-attempt', '2')
  await expect(earlier.locator('[data-flow-attempt="1"]')).toContainText('superseded')
  await page.screenshot({ path: testInfo.outputPath('task-flow-stage-panel.png') })

  await page.keyboard.press('Escape')
  await expect(panel).toHaveCount(0)
  await expect(taskSheet).toBeVisible()
  await expect(node(page, 'implement')).toBeFocused()
  await page.screenshot({ path: testInfo.outputPath('task-flow-sheet.png') })

  // A stage without a current attempt states the server's status sentence.
  await node(page, 'review').click()
  const review = page.getByRole('dialog', { name: 'Server review stage' })
  await expect(review.locator('[data-flow-status-text]')).toHaveText('Server sentence: review is done.')
  await expect(review.getByRole('region', { name: 'Attempts' })).toHaveCount(0)
})

test('a task switch drops the late flow of the previous task', async ({ page }) => {
  const flows = { [taskId]: baseFlow('First'), [otherId]: baseFlow('Second') }
  const api = await mockAPI(page, { flows })
  await page.goto(`/w/demo/tasks/${taskId}`)
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toContainText('Draw the task flow')
  api.hold(taskId)
  await tab(page, 'Flow').click()
  await expect.poll(() => api.flowCalls).toContain(`${taskId}@demo`)
  const next = page.getByRole('link', { name: 'Next task' }).or(page.getByRole('link', { name: 'Previous task' }))
  await next.first().click()
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toContainText('Another task')
  await expect(node(page, 'implement')).toHaveText(/Second implement/)
  await api.release(taskId)
  await expect(node(page, 'implement')).toHaveText(/Second implement/)
  await expect(page.locator('[data-flow-node]').filter({ hasText: 'First implement' })).toHaveCount(0)
})

test('a workspace switch leaves no flow read for the previous workspace', async ({ page }) => {
  const api = await mockAPI(page, { flows: { [taskId]: baseFlow() } })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  api.hold(taskId)
  await tab(page, 'Flow').click()
  await expect.poll(() => api.flowCalls).toContain(`${taskId}@demo`)
  const demoReads = api.flowCalls.length
  await page.getByRole('navigation', { name: 'Workspaces' }).getByRole('button', { name: 'Switch to Other' }).click()
  await expect.poll(() => page.evaluate(() => localStorage.getItem('conveyor-workspace'))).toBe('other')
  await api.release(taskId)
  // Every later flow read names the newly selected workspace.
  expect(api.flowCalls.slice(demoReads).every((call) => call.endsWith('@other'))).toBe(true)
})

// The flow refreshes with task detail while the Flow tab is shown, and a
// detail refresh on the Timeline tab reads no flow (AC-7.4, AC-7.6, AC-7.7).
// The Board stripe preference stays off, so every /flow read here is the
// detail chart's. A controlled task stream frame drives a real detail-cache
// refresh, and Playwright's clock holds the stream's 250 ms refresh delay.
async function pauseClock(page: Page) {
  const now = await page.evaluate(() => Date.now())
  await page.clock.pauseAt(now + 1_000)
}

function refreshedFlow(): TaskFlow {
  const flow = baseFlow()
  const implement = flow.stages[2]
  flow.stages[2] = {
    ...implement,
    current_attempt: implement.current_attempt && {
      ...implement.current_attempt,
      progress: 'Server progress: refreshed after the stream frame',
      elapsed_seconds: 4200,
    },
  }
  flow.stages[4] = stage('review', 'waiting')
  return flow
}

for (const surface of surfaces) {
  test(`an active Flow tab refreshes with task detail on the ${surface.name}, and Timeline reads no flow`, async ({
    page,
  }) => {
    const streams = await controlTaskStreams(page)
    await page.addInitScript(() => localStorage.setItem('conveyor-show-stage-progress', 'false'))
    const flows = { [taskId]: baseFlow() }
    const api = await mockAPI(page, { flows })
    await page.clock.install({ time: new Date(at(95)) })
    await page.goto(surface.url(taskId))
    await tab(page, 'Flow').click()
    await expect(node(page, 'review')).toHaveAttribute('data-status', 'done')
    await node(page, 'implement').click()
    const panel = page.getByRole('dialog', { name: 'Server implement stage' })
    await expect(panel.locator('[data-flow-attempt-progress]')).toHaveText('Server progress: wiring the stage panel')
    await expect(panel.locator('[data-flow-attempt-elapsed]')).toHaveText('1h 05m')
    await expect.poll(() => streams.countFor(taskId)).toBeGreaterThan(0)
    await streams.open()
    await pauseClock(page)
    expect(api.flowCalls.every((call) => call === `${taskId}@demo`)).toBe(true)

    // The server's next fold, announced by an activity frame.
    const flowReads = api.flowCalls.length
    const detailReads = api.detailCalls.length
    flows[taskId] = refreshedFlow()
    api.titles[taskId] = 'Draw the task flow, refreshed'
    await streams.send(activityFrame)
    await page.clock.runFor(250)
    await expect.poll(() => api.detailCalls.length).toBeGreaterThan(detailReads)
    await expect.poll(() => api.flowCalls.length).toBeGreaterThan(flowReads)
    // Query delivers results through zero-delay timers, which the paused
    // clock holds until it moves.
    await expect
      .poll(async () => {
        await page.clock.runFor(50)
        return panel.locator('[data-flow-attempt-progress]').textContent()
      })
      .toBe('Server progress: refreshed after the stream frame')
    await expect(panel.locator('[data-flow-attempt-progress]')).toHaveText(
      'Server progress: refreshed after the stream frame',
    )
    await expect(panel.locator('[data-flow-attempt-elapsed]')).toHaveText('1h 10m')
    await page.keyboard.press('Escape')
    await expect(panel).toHaveCount(0)
    await expect(node(page, 'review')).toHaveAttribute('data-status', 'waiting')

    // On the Timeline tab a refreshed detail reaches no flow reader.
    await tab(page, 'Timeline').click()
    await expect(page.getByRole('region', { name: 'Execution event timeline' })).toBeVisible()
    await expect(chart(page)).toHaveCount(0)
    const shown = api.flowCalls.length
    const timelineDetailReads = api.detailCalls.length
    api.titles[taskId] = 'Draw the task flow, refreshed twice'
    await streams.send(activityFrame)
    await page.clock.runFor(250)
    await expect.poll(() => api.detailCalls.length).toBeGreaterThan(timelineDetailReads)
    // The refreshed detail has rendered, so a mounted flow reader would have
    // been told to refetch by now.
    await expect
      .poll(async () => {
        await page.clock.runFor(50)
        return page.getByText('Draw the task flow, refreshed twice').count()
      })
      .toBeGreaterThan(0)
    await page.clock.runFor(1_000)
    await expect(page.getByRole('region', { name: 'Execution event timeline' })).toBeVisible()
    expect(api.flowCalls).toHaveLength(shown)
  })
}
