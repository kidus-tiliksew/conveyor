import { expect, type Locator, type Page, test } from '@playwright/test'
import type { TaskFlow, TaskFlowStage } from '../src/lib/types'
import { callerIdentity } from './helpers/caller-identity'
import { serveNextActionsFromActivity } from './helpers/next-action-stub'

// The task flow chart (feature-operator-next-action NA-7, AC-3.2, AC-4.2,
// AC-4.4). The flow route is mocked with server-shaped projections. Every
// assertion checks how the chart draws what the server folded. The client
// derives nothing from events.

const timestamp = '2026-10-12T10:00:00Z'
const taskId = 'flow-task'
const task = {
  id: taskId,
  workspace: 'demo',
  title: 'Draw the task flow',
  state: 'running',
  repo: 'conveyor',
  branch: 'conveyor/flow-task',
  created_at: timestamp,
}
const activity = {
  task,
  jobs: [],
  events: [],
  work_orders: [],
  reviews: [],
  interventions: [],
  latest_stage: 'implement',
  plan: { state: 'approved', version: 1 },
  last_event_at: timestamp,
  needs_attention: false,
}

// The server states every sentence on a stage. These mock sentences differ
// from any wording the client could carry, so an assertion on them proves the
// chart prints what it was sent.
function serverCopy(id: TaskFlowStage['id'], status: TaskFlowStage['status']) {
  return {
    description: `Server description of ${id}.`,
    status_label: `Server ${status} label`,
    status_text: `Server sentence: ${id} is ${status}.`,
    missed_check_in: '',
  }
}

function stage(id: TaskFlowStage['id'], status: TaskFlowStage['status'], extra: Partial<TaskFlowStage> = {}) {
  return {
    id,
    gate: null,
    status,
    ...serverCopy(id, status),
    attempts: [],
    current_attempt: null,
    ...extra,
  } satisfies TaskFlowStage
}

// The same stage in another status, restated by the server.
function restated(row: TaskFlowStage, status: TaskFlowStage['status'], extra: Partial<TaskFlowStage> = {}) {
  return { ...row, ...serverCopy(row.id, status), status, ...extra }
}

const missedCheckIn = 'Server says the lease ran out at 2026-10-12 10:20 UTC.'

// One task in implementation after review bounced it twice. The plan gate has
// passed, verify is off by policy, a prerequisite blocks it, and the server
// says the agent's claimed attempt has missed its check-in.
function claimedFlow(leaseExpiresAt = '2026-10-12T10:20:00Z', missed = missedCheckIn): TaskFlow {
  return {
    stages: [
      stage('triage', 'done'),
      stage('spec', 'done', {
        gate: 'plan_approval',
        attempts: [
          {
            order_id: 'flow-task-spec-1',
            attempt_id: 'attempt-spec-1',
            started_at: '2026-10-12T10:01:00Z',
            ended_at: '2026-10-12T10:09:00Z',
            outcome: 'submitted',
            superseded: false,
            auto: false,
          },
        ],
      }),
      stage('implement', 'in_progress', {
        missed_check_in: missed,
        attempts: [
          {
            order_id: 'flow-task-implement-1',
            attempt_id: 'attempt-impl-1',
            started_at: '2026-10-12T10:10:00Z',
            ended_at: '2026-10-12T10:12:00Z',
            outcome: 'retired',
            superseded: true,
            auto: true,
          },
          {
            order_id: 'flow-task-implement-2',
            attempt_id: 'attempt-impl-2',
            started_at: '2026-10-12T10:15:00Z',
            ended_at: null,
            outcome: '',
            superseded: false,
            auto: false,
          },
        ],
        current_attempt: {
          harness: 'omp',
          model: 'claude-opus-5-5',
          user: 'operator@example.test',
          claimed_by: 'run:usr_operator',
          session_id: 'session-7f3a',
          attempt_id: 'attempt-impl-2',
          claimed_at: '2026-10-12T10:15:00Z',
          last_check_in: '2026-10-12T10:16:00Z',
          lease_expires_at: leaseExpiresAt,
          progress: 'Wiring the chart into task detail',
          elapsed_seconds: 3723,
        },
      }),
      stage('verify', 'off'),
      stage('review', 'done'),
      stage('merge', 'upcoming', { gate: 'merge_approval' }),
    ],
    loops: [{ from: 'review', to: 'implement', count: 2 }],
    side: {
      blocked_by: [{ task_id: 'task-prereq', title: 'Prerequisite slice', state: 'running' }],
      retrying: null,
      parked: false,
      closed: false,
      agent_question: false,
    },
    next_action: {
      label: 'Agent implementing',
      actor: 'agent',
      kind: 'agent_working',
      summary: 'An agent holds the implementation attempt; no operator action is needed.',
    },
    raw: {
      state: 'running',
      next_stage: 'implement',
      recovery_stage: '',
      gate_kind: '',
      order_ids: ['flow-task-spec-1', 'flow-task-implement-1', 'flow-task-implement-2'],
    },
  }
}

// A task waiting on the operator's plan approval, with merge approval turned
// off by policy, so the server sends no merge gate.
function gateFlow(): TaskFlow {
  return {
    stages: [
      stage('triage', 'done'),
      stage('spec', 'needs_operator', { gate: 'plan_approval' }),
      stage('implement', 'upcoming'),
      stage('verify', 'upcoming'),
      stage('review', 'upcoming'),
      stage('merge', 'upcoming'),
    ],
    loops: [],
    side: { blocked_by: [], retrying: { attempt: 2, of: 3 }, parked: false, closed: false, agent_question: false },
    next_action: {
      label: 'Approve the plan',
      actor: 'operator',
      kind: 'gate',
      summary: 'The plan waits for your approval.',
    },
    raw: {
      state: 'awaiting_approval',
      next_stage: 'implement',
      recovery_stage: '',
      gate_kind: 'plan_approval',
      order_ids: [],
    },
  }
}

async function mockAPI(page: Page, flow: () => TaskFlow, stream?: () => Promise<string | null>) {
  const counts = { flow: 0, detail: 0 }
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await serveNextActionsFromActivity(page)
  await page.route('**/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({ json: callerIdentity({ id: 'operator', email: 'operator@example.test', role: 'admin' }) })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    if (path === `/v1/tasks/${taskId}/flow`) {
      counts.flow++
      return route.fulfill({ json: { flow: flow() } })
    }
    if (path === `/v1/tasks/${taskId}/activity`) {
      counts.detail++
      return route.fulfill({ json: activity })
    }
    if (path.endsWith('/stream')) {
      const body = stream ? await stream() : null
      if (body !== null) return route.fulfill({ contentType: 'text/event-stream', body })
      return route.fulfill({ status: 204 })
    }
    if (path === '/v1/activity' || path === '/v1/task-operations')
      return route.fulfill({
        json: [activity],
        headers: { 'X-Conveyor-Total': '1', 'X-Conveyor-Limit': '50', 'X-Conveyor-Offset': '0' },
      })
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
    return route.fulfill({ json: [] })
  })
  return counts
}

const surfaces = [
  { name: 'board sheet', url: `/tasks/${taskId}`, inDialog: true },
  { name: 'Tasks panel', url: `/tasks?task=${taskId}`, inDialog: true },
  { name: 'full page', url: `/tasks/${taskId}/full`, inDialog: false },
] as const

async function openFlow(page: Page, url: string) {
  await page.goto(url)
  const flow = page.getByRole('region', { name: 'Task flow' })
  await expect(flow.locator('[data-flow-stage]')).toHaveCount(6)
  return flow
}

const node = (flow: Locator, id: string) => flow.locator(`[data-flow-stage="${id}"]`)

for (const surface of surfaces) {
  test(`renders the flow above the timeline on the ${surface.name}`, async ({ page }) => {
    await mockAPI(page, () => claimedFlow())
    const flow = await openFlow(page, surface.url)
    if (surface.inDialog) await expect(page.getByRole('dialog', { name: 'Task detail' }).locator(flow)).toHaveCount(1)
    const timeline = page.getByRole('region', { name: 'Execution event timeline' })
    await expect(timeline).toHaveCount(1)
    // The chart sits immediately above the timeline in document order.
    const precedes = await flow.evaluate(
      (chart, other) => Boolean(chart.compareDocumentPosition(other as Node) & Node.DOCUMENT_POSITION_FOLLOWING),
      await timeline.elementHandle(),
    )
    expect(precedes).toBe(true)
  })
}

test('draws stages top to bottom with the server status word, and both approval gates in place', async ({ page }) => {
  await mockAPI(page, () => claimedFlow())
  const flow = await openFlow(page, `/tasks/${taskId}/full`)
  const order = await flow
    .locator('[data-flow-stage], [data-flow-gate]')
    .evaluateAll((items) =>
      items.map((item) => item.getAttribute('data-flow-stage') ?? `gate:${item.getAttribute('data-flow-gate')}`),
    )
  // The plan approval follows the plan, and the merge approval precedes the merge.
  expect(order).toEqual([
    'triage',
    'spec',
    'gate:plan_approval',
    'implement',
    'verify',
    'review',
    'gate:merge_approval',
    'merge',
  ])
  await expect(node(flow, 'implement')).toContainText('Server in_progress label')
  await expect(node(flow, 'merge')).toContainText('Server upcoming label')
  const boxes = await flow
    .locator('[data-flow-stage]')
    .evaluateAll((items) => items.map((item) => item.getBoundingClientRect().top))
  expect(boxes).toEqual([...boxes].sort((a, b) => a - b))
})

test('marks the waiting gate and omits a gate the policy turned off', async ({ page }) => {
  await mockAPI(page, () => gateFlow())
  const flow = await openFlow(page, `/tasks/${taskId}/full`)
  const gate = flow.locator('[data-flow-gate="plan_approval"]')
  await expect(gate).toBeVisible()
  await expect(gate.getByText('plan_approval')).toHaveClass(/bg-attention-soft/)
  await expect(flow.locator('[data-flow-gate="merge_approval"]')).toHaveCount(0)
  await expect(node(flow, 'spec')).toContainText('Server needs_operator label')
  await expect(flow.locator('[data-flow-side="retrying"]')).toHaveText('retrying2/3')
  // Nothing past the gate has been reached, so every edge after it is dashed.
  await expect(flow.locator('[data-flow-edge]')).toHaveCount(6)
  await expect(flow.locator('[data-flow-edge="visited"]')).toHaveCount(2)
})

test('draws the review bounce as a left back-edge labelled with its count', async ({ page }) => {
  await mockAPI(page, () => claimedFlow())
  const flow = await openFlow(page, `/tasks/${taskId}/full`)
  const loop = flow.locator('[data-flow-loop="review-implement"]')
  await expect(loop).toContainText('×2')
  const [edge, implement, review] = await Promise.all([
    loop.boundingBox(),
    node(flow, 'implement').boundingBox(),
    node(flow, 'review').boundingBox(),
  ])
  if (!edge || !implement || !review) throw new Error('flow geometry missing')
  // The edge runs from the review row back up to the implement row, left of the stages.
  expect(edge.x + edge.width).toBeLessThanOrEqual(implement.x + 1)
  expect(edge.y).toBeGreaterThanOrEqual(implement.y)
  expect(edge.y).toBeLessThanOrEqual(implement.y + implement.height)
  expect(edge.y + edge.height).toBeGreaterThanOrEqual(review.y)
  expect(edge.y + edge.height).toBeLessThanOrEqual(review.y + review.height)
})

test('gives the longer of two loops the outer gutter so the back-edges do not cross', async ({ page }) => {
  await mockAPI(page, () => ({
    ...claimedFlow(),
    loops: [
      { from: 'verify', to: 'implement', count: 1 },
      { from: 'merge', to: 'implement', count: 3 },
    ],
  }))
  const flow = await openFlow(page, `/tasks/${taskId}/full`)
  const [outer, inner] = await Promise.all([
    flow.locator('[data-flow-loop="merge-implement"]').boundingBox(),
    flow.locator('[data-flow-loop="verify-implement"]').boundingBox(),
  ])
  if (!outer || !inner) throw new Error('flow geometry missing')
  expect(outer.x + outer.width).toBeLessThanOrEqual(inner.x + 1)
  expect(outer.y + outer.height).toBeGreaterThan(inner.y + inner.height)
})

test('draws visited edges solid and upcoming edges dashed', async ({ page }) => {
  await mockAPI(page, () => claimedFlow())
  const flow = await openFlow(page, `/tasks/${taskId}/full`)
  const edges = await flow
    .locator('[data-flow-edge]')
    .evaluateAll((items) =>
      items.map((item) => [item.getAttribute('data-flow-edge'), getComputedStyle(item).borderLeftStyle]),
    )
  // Into: spec, plan gate, implement, verify (off), review, merge gate, merge.
  expect(edges).toEqual([
    ['visited', 'solid'],
    ['visited', 'solid'],
    ['visited', 'solid'],
    ['upcoming', 'dashed'],
    ['visited', 'solid'],
    ['upcoming', 'dashed'],
    ['upcoming', 'dashed'],
  ])
})

// Edge kinds into: spec, plan gate, implement, verify, review, merge gate, merge.
async function edgeKinds(flow: Locator) {
  return flow
    .locator('[data-flow-edge]')
    .evaluateAll((items) => items.map((item) => item.getAttribute('data-flow-edge')))
}

test('a stage whose order waits to be claimed has been reached', async ({ page }) => {
  const flow = claimedFlow()
  await mockAPI(page, () => ({
    ...flow,
    stages: flow.stages.map((row) =>
      row.id === 'implement'
        ? restated(row, 'done', { current_attempt: null })
        : row.id === 'review'
          ? restated(row, 'waiting')
          : row,
    ),
  }))
  const chart = await openFlow(page, `/tasks/${taskId}/full`)
  await expect(node(chart, 'review')).toContainText('Server waiting label')
  expect(await edgeKinds(chart)).toEqual([
    'visited',
    'visited',
    'visited',
    'upcoming',
    'visited',
    'upcoming',
    'upcoming',
  ])
})

test('a merge waiting on its approval has reached the gate but not the merge', async ({ page }) => {
  const flow = claimedFlow()
  await mockAPI(page, () => ({
    ...flow,
    stages: flow.stages.map((row) =>
      row.id === 'implement'
        ? restated(row, 'done', { current_attempt: null })
        : row.id === 'merge'
          ? restated(row, 'needs_operator')
          : row,
    ),
  }))
  const chart = await openFlow(page, `/tasks/${taskId}/full`)
  await expect(chart.locator('[data-flow-gate="merge_approval"]').getByText('merge_approval')).toHaveClass(
    /bg-attention-soft/,
  )
  expect(await edgeKinds(chart)).toEqual([
    'visited',
    'visited',
    'visited',
    'upcoming',
    'visited',
    'visited',
    'upcoming',
  ])
})

test('greys an off stage and prints the label the server sent for it', async ({ page }) => {
  await mockAPI(page, () => claimedFlow())
  const flow = await openFlow(page, `/tasks/${taskId}/full`)
  const verify = node(flow, 'verify')
  await expect(verify).toContainText('Server off label')
  await expect(flow).not.toContainText('off for this task')
  expect(await verify.evaluate((item) => Number(getComputedStyle(item).opacity))).toBeLessThan(1)
  expect(await node(flow, 'review').evaluate((item) => Number(getComputedStyle(item).opacity))).toBe(1)
})

test('shows a blocking prerequisite as a side node that links to it', async ({ page }) => {
  await mockAPI(page, () => claimedFlow())
  const flow = await openFlow(page, `/tasks/${taskId}/full`)
  const blocked = flow.locator('[data-flow-side="blocked_by"]')
  await expect(blocked).toContainText('task-prereq')
  await expect(blocked.getByRole('link', { name: 'Prerequisite slice' })).toHaveAttribute('href', '/tasks/task-prereq')
  const [side, implement] = await Promise.all([blocked.boundingBox(), node(flow, 'implement').boundingBox()])
  if (!side || !implement) throw new Error('flow geometry missing')
  expect(side.x).toBeGreaterThanOrEqual(implement.x + implement.width)
})

test('pulses the in-progress stage, and reduced motion stops the pulse', async ({ page }) => {
  await mockAPI(page, () => claimedFlow())
  let flow = await openFlow(page, `/tasks/${taskId}/full`)
  const live = () => flow.locator('[data-flow-live]')
  await expect(live()).toHaveCount(1)
  await expect(node(flow, 'implement').locator('[data-flow-live]')).toHaveCount(1)
  expect(await live().evaluate((item) => getComputedStyle(item).animationName)).toBe('pulse')
  await page.emulateMedia({ reducedMotion: 'reduce' })
  flow = await openFlow(page, `/tasks/${taskId}/full`)
  expect(await live().evaluate((item) => getComputedStyle(item).animationName)).toBe('none')
})

test('opens the stage sheet with the current attempt, a missed check-in, and copyable IDs', async ({
  page,
  context,
}) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  await mockAPI(page, () => claimedFlow())
  const flow = await openFlow(page, `/tasks/${taskId}`)
  await node(flow, 'implement').click()
  const sheet = page.getByRole('dialog', { name: 'Implement stage' })
  await expect(sheet.getByRole('heading', { name: 'Implement' })).toBeVisible()
  await expect(sheet.locator('header')).toContainText('Server in_progress label')
  await expect(sheet.locator('[data-flow-description]')).toHaveText('Server description of implement.')
  await expect(sheet.locator('[data-flow-status-text]')).toHaveText('Server sentence: implement is in_progress.')
  const attempt = sheet.getByRole('region', { name: 'Current attempt' })
  const field = (term: string) =>
    attempt.locator('dt', { hasText: new RegExp(`^${term}$`) }).locator('xpath=following-sibling::dd[1]')
  await expect(field('Harness')).toHaveText('omp')
  await expect(field('Model')).toHaveText('claude-opus-5-5')
  await expect(field('User')).toHaveText('operator@example.test')
  await expect(field('Session')).toContainText('session-7f3a')
  await expect(field('Attempt')).toContainText('attempt-impl-2')
  await expect(field('Progress')).toHaveText('Wiring the chart into task detail')
  await expect(field('Elapsed')).toHaveText('1h 02m')
  await expect(field('Claimed at')).not.toHaveText('—')
  await expect(field('Last check-in').locator('[data-flow-missed-check-in]')).toHaveText(missedCheckIn)
  await attempt.getByRole('button', { name: 'Copy session ID' }).click()
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe('session-7f3a')
  await attempt.getByRole('button', { name: 'Copy attempt ID' }).click()
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe('attempt-impl-2')
  // Escape closes only the stage sheet; the task detail under it stays.
  await page.keyboard.press('Escape')
  await expect(sheet).toHaveCount(0)
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toBeVisible()
})

// The client judges no lease: a lease long past with no server sentence shows
// no note, and the off stage's sheet prints the server's words too.
test('a lapsed lease the server sent no sentence for shows no missed check-in note', async ({ page }) => {
  await mockAPI(page, () => claimedFlow('2026-10-12T10:20:00Z', ''))
  const flow = await openFlow(page, `/tasks/${taskId}/full`)
  await node(flow, 'implement').click()
  const sheet = page.getByRole('dialog', { name: 'Implement stage' })
  const attempt = sheet.getByRole('region', { name: 'Current attempt' })
  await expect(attempt).toContainText('session-7f3a')
  await expect(attempt.locator('[data-flow-missed-check-in]')).toHaveCount(0)
  await expect(attempt).not.toContainText(/missed check-in/i)
  await sheet.getByRole('button', { name: 'Close stage' }).click()
  await node(flow, 'verify').click()
  const off = page.getByRole('dialog', { name: 'Verify stage' })
  await expect(off.locator('header')).toContainText('Server off label')
  await expect(off.locator('[data-flow-status-text]')).toHaveText('Server sentence: verify is off.')
  await expect(off).not.toContainText('off for this task')
})

test('collapses superseded attempts, and Show raw state reveals the raw projection', async ({ page }) => {
  await mockAPI(page, () => claimedFlow())
  const flow = await openFlow(page, `/tasks/${taskId}/full`)
  await node(flow, 'implement').click()
  const sheet = page.getByRole('dialog', { name: 'Implement stage' })
  const attempts = sheet.getByRole('region', { name: 'Attempts' })
  await expect(attempts.getByText('flow-task-implement-2')).toBeVisible()
  await expect(attempts.getByText('flow-task-implement-1')).toBeHidden()
  await attempts.getByText('Superseded').click()
  await expect(attempts.getByText('flow-task-implement-1')).toBeVisible()
  await expect(sheet.locator('[data-flow-raw]')).toHaveCount(0)
  await sheet.getByRole('button', { name: 'Show raw state' }).click()
  const raw = sheet.locator('[data-flow-raw]')
  await expect(raw).toContainText('"order_ids"')
  await expect(raw).toContainText('flow-task-implement-2')
  await expect(sheet.getByRole('button', { name: 'Hide raw state' })).toHaveAttribute('aria-expanded', 'true')
})

test('shows the pending next action under the chart as the server states it', async ({ page }) => {
  await mockAPI(page, () => gateFlow())
  const flow = await openFlow(page, `/tasks/${taskId}/full`)
  const pending = flow.locator('[data-flow-next-action]')
  await expect(pending).toHaveText('The plan waits for your approval.')
  const [stages, line] = await Promise.all([
    flow.getByRole('list', { name: 'Stages' }).boundingBox(),
    pending.boundingBox(),
  ])
  if (!stages || !line) throw new Error('flow geometry missing')
  expect(line.y).toBeGreaterThanOrEqual(stages.y + stages.height)
})

test('a detail refresh refreshes the flow', async ({ page }) => {
  let released = false
  let release = () => {}
  const frame = new Promise<void>((resolve) => {
    release = () => {
      released = true
      resolve()
    }
  })
  // Connections opened before the release are held and then deliver one
  // activity frame each. Connections opened afterwards carry no stream, so
  // nothing else refreshes the detail.
  const counts = await mockAPI(
    page,
    () =>
      released
        ? {
            ...claimedFlow(),
            stages: claimedFlow().stages.map((row) =>
              row.id === 'implement' ? restated(row, 'done', { current_attempt: null }) : row,
            ),
          }
        : claimedFlow(),
    async () => {
      if (released) return null
      await frame
      return 'event: activity\ndata: {}\n\n'
    },
  )
  const flow = await openFlow(page, `/tasks/${taskId}/full`)
  await expect(node(flow, 'implement')).toHaveAttribute('data-status', 'in_progress')
  const reads = { detail: counts.detail, flow: counts.flow }
  release()
  await expect.poll(() => counts.detail).toBeGreaterThan(reads.detail)
  await expect(node(flow, 'implement')).toHaveAttribute('data-status', 'done')
  expect(counts.flow).toBeGreaterThan(reads.flow)
})

test.describe('at 390 px', () => {
  test.use({ viewport: { width: 390, height: 844 } })

  for (const surface of surfaces) {
    test(`the ${surface.name} chart has no horizontal scroll`, async ({ page }) => {
      await mockAPI(page, () => claimedFlow())
      const flow = await openFlow(page, surface.url)
      await expect(flow.locator('[data-flow-side="blocked_by"]')).toBeVisible()
      const card = flow.locator(':scope > div').first()
      const widths = await card.evaluate((item) => [item.scrollWidth, item.clientWidth])
      expect(widths[0]).toBeLessThanOrEqual(widths[1])
      const viewport = await page.evaluate(() => [
        document.documentElement.scrollWidth,
        document.documentElement.clientWidth,
      ])
      expect(viewport[0]).toBeLessThanOrEqual(viewport[1])
      // Side nodes stack under the stages instead of squeezing beside them.
      const [side, merge] = await Promise.all([
        flow.locator('[data-flow-side="blocked_by"]').boundingBox(),
        node(flow, 'merge').boundingBox(),
      ])
      if (!side || !merge) throw new Error('flow geometry missing')
      expect(side.y).toBeGreaterThanOrEqual(merge.y + merge.height)
    })
  }
})
