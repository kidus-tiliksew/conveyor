import { expect, type Page, test } from '@playwright/test'
import type { TaskFlow, TaskFlowStage } from '../src/lib/types'
import { callerIdentity } from './helpers/caller-identity'

// The Board card (req-operator-next-action AC-3.1 to AC-3.5;
// feature-operator-next-action NA-6, NA-7; component-web-task-surfaces).
// Activity items carry the server's compact next action; every assertion
// checks that the card prints the server's label verbatim, tones its status
// mark from the server's actor and kind, and keeps the summary and blockers
// behind hover and focus. The stage stripe is opt-in and off by default.

const at = '2026-10-15T09:00:00Z'

function card(
  id: string,
  title: string,
  stage: string,
  nextAction: Record<string, unknown> | undefined,
  extra: Record<string, unknown> = {},
  task: Record<string, unknown> = {},
) {
  return {
    task: {
      id,
      workspace: 'demo',
      title,
      repo: 'conveyor',
      branch: `conveyor/${id}`,
      state: 'running',
      created_at: at,
      ...task,
    },
    latest_stage: stage,
    last_event_at: at,
    needs_attention: nextAction?.actor === 'operator',
    next_action: nextAction,
    ...extra,
  }
}

const proposal = {
  label: 'Document proposal',
  actor: 'operator',
  kind: 'document_proposal',
  judgment: true,
  summary: 'Server summary: a document proposal is withholding the next claim.',
  deadline: null,
}
const working = {
  label: 'Working',
  actor: 'agent',
  kind: 'working',
  judgment: false,
  summary: 'Server summary: an agent is working on this task.',
  deadline: null,
}
const merging = {
  label: 'Merging',
  actor: 'factory',
  kind: 'merging',
  judgment: false,
  summary: 'Server summary: Conveyor is merging the approved branch.',
  deadline: null,
}
const queued = {
  label: 'Queued',
  actor: 'agent',
  kind: 'queued',
  judgment: false,
  summary: 'Server summary: an order is waiting to be claimed.',
  deadline: null,
}
const blocked = {
  label: 'Blocked',
  actor: 'dependency',
  kind: 'blocked',
  judgment: false,
  summary: 'Server summary: this task is waiting on another task.',
  deadline: null,
}
const stalled = {
  label: 'Stalled',
  actor: 'operator',
  kind: 'stalled',
  judgment: false,
  summary: 'Server summary: the latest order cannot be claimed.',
  deadline: null,
}
const agentQuestion = {
  label: 'Cited revisions need a decision',
  actor: 'operator',
  kind: 'agent_question',
  judgment: true,
  summary:
    'The agent asks you to confirm or dismiss proposed revisions to cited documents, then recover the order with direction.',
  deadline: null,
}

const items = [
  card('task-proposal', 'Confirm the dashboard proposal', 'spec', proposal),
  card(
    'task-working',
    'Build the opt-in stripe',
    'implement',
    working,
    {},
    { assignee: { user_id: 'usr_kt', display_name: 'Kidus Tiliksew' } },
  ),
  card('task-merging', 'Ship touch tooltips', 'merge', merging, {}, { state: 'approved' }),
  card('task-queued', 'Paginate lineage reads', 'implement', queued),
  card(
    'task-blocked',
    'Wire the timeline attempt',
    'implement',
    blocked,
    {},
    {
      blocking_task_ids: ['task-working', 'task-queued'],
      dependencies: [
        { id: 'task-working', title: 'Build the opt-in stripe', state: 'running' },
        { id: 'task-queued', title: 'Paginate lineage reads', state: 'running' },
      ],
    },
  ),
  card(
    'task-unsatisfiable',
    'Retire the stage chart',
    'implement',
    blocked,
    { stalled: { blocking_task_ids: ['task-gone'], unsatisfiable_edge: true } },
    { dependencies: [{ id: 'task-gone', title: 'Abandoned prerequisite', state: 'closed' }] },
  ),
  card('task-failed', 'Recover the stalled order', 'implement', stalled, {
    stalled: { last_failure: 'claim refused: no live claimant' },
  }),
  // A deliberate checkpoint release also matches the stalled row, so the
  // activity item carries its release message (component-web-task-surfaces,
  // Board and Tasks list).
  card('task-checkpoint', 'Fix checkpoint presentation', 'implement', agentQuestion, {
    stalled: { needed: true, reason: 'retry suppressed', last_failure: 'operator checkpoint reached' },
  }),
  card('task-checkpoint-forge', 'Publish the checkpoint comment', 'implement', agentQuestion, {
    stalled: { needed: true, reason: 'retry suppressed', last_failure: 'operator checkpoint reached' },
    forge_failure: { surface: 'pull request comment', detail: 'GitHub refused the comment', category: 'forge' },
  }),
]

function stage(id: TaskFlowStage['id'], status: TaskFlowStage['status'], gate: TaskFlowStage['gate'] = null) {
  return {
    id,
    gate,
    status,
    description: `Server description of ${id}.`,
    status_label: `Server ${status} label`,
    status_text: `Server sentence: ${id} is ${status}.`,
    missed_check_in: '',
    attempts: [],
    current_attempt: null,
  } satisfies TaskFlowStage
}

function flowFor(taskId: string): TaskFlow {
  const proposalWaiting = taskId === 'task-proposal'
  return {
    stages: proposalWaiting
      ? [
          stage('triage', 'done'),
          stage('spec', 'needs_operator', 'plan_approval'),
          stage('implement', 'upcoming'),
          stage('verify', 'off'),
          stage('review', 'upcoming'),
          stage('merge', 'upcoming', 'merge_approval'),
        ]
      : [
          stage('triage', 'done'),
          stage('spec', 'done', 'plan_approval'),
          stage('implement', 'in_progress'),
          stage('verify', 'off'),
          stage('review', 'waiting'),
          stage('merge', 'upcoming'),
        ],
    loops: [],
    side: { blocked_by: [], retrying: null, parked: false, closed: false, agent_question: false },
    next_action: { label: 'x', actor: 'agent', kind: 'working', summary: 'x' },
    raw: { state: 'running', next_stage: 'implement', recovery_stage: '', gate_kind: '', order_ids: [] },
  }
}

async function mockBoard(
  page: Page,
  {
    stageProgress,
    stream,
    flow: flowOverride,
  }: { stageProgress?: string; stream?: () => Promise<string | null>; flow?: (taskId: string) => TaskFlow } = {},
) {
  const counts = { flow: 0, detail: 0, flowTasks: new Set<string>(), activityQueries: [] as string[] }
  await page.addInitScript((value) => {
    localStorage.setItem('conveyor-workspace', 'demo')
    if (value !== undefined) localStorage.setItem('conveyor-show-stage-progress', value)
  }, stageProgress)
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'operator', email: 'operator@example.test', role: 'operator' }, route.request()),
      })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    const flow = path.match(/^\/v1\/tasks\/([^/]+)\/flow$/)
    if (flow) {
      counts.flow++
      counts.flowTasks.add(flow[1])
      return route.fulfill({ json: { flow: (flowOverride ?? flowFor)(flow[1]) } })
    }
    const action = path.match(/^\/v1\/tasks\/([^/]+)\/next-action$/)
    if (action) {
      const item = items.find((candidate) => candidate.task.id === action[1])
      return route.fulfill({ json: { ...item?.next_action, choices: [], details: {} } })
    }
    const detail = path.match(/^\/v1\/tasks\/([^/]+)\/activity$/)
    if (detail) {
      counts.detail++
      const item = items.find((candidate) => candidate.task.id === detail[1])
      return route.fulfill({
        json: {
          ...item,
          jobs: [],
          events: [],
          work_orders: [],
          reviews: [],
          interventions: [],
          attachments: [],
          plan: { state: 'pending', version: 1 },
        },
      })
    }
    if (path.endsWith('/stream')) {
      const body = stream ? await stream() : null
      if (body !== null) return route.fulfill({ contentType: 'text/event-stream', body })
      return route.fulfill({ status: 204 })
    }
    if (path === '/v1/activity') {
      const q = url.searchParams.get('q') ?? ''
      counts.activityQueries.push(q)
      const matching = items.filter((item) => !q || item.task.id.includes(q) || item.task.title.includes(q))
      return route.fulfill({
        json: matching,
        headers: {
          'X-Conveyor-Total': String(matching.length),
          'X-Conveyor-Limit': '100',
          'X-Conveyor-Offset': '0',
        },
      })
    }
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
    return route.fulfill({ json: [] })
  })
  return counts
}

const cardFor = (page: Page, title: string) => page.getByRole('link', { name: new RegExp(`^${title}`) })

test('a card leads with the server label behind a status mark, and keeps the summary behind hover and focus', async ({
  page,
}) => {
  await mockBoard(page)
  await page.goto('/')
  const operator = cardFor(page, 'Confirm the dashboard proposal')
  await expect(operator.locator('[data-next-action-label]')).toHaveText('Document proposal')
  await expect(operator.locator('[data-status-mark]')).toHaveAttribute('data-status-mark', 'operator')
  // The card face prints no summary sentence, task ID, repository hue, or actor word.
  const summary = operator.locator('[data-next-action-summary]')
  await expect(summary).toHaveCSS('opacity', '0')
  await expect(operator).not.toContainText('task-proposal')
  await expect(operator.getByText('operator', { exact: true })).toHaveCount(0)
  await expect(operator.locator('[title="conveyor"]')).toHaveCount(0)
  // The summary is the card's description and opens on hover and on focus.
  await expect(operator).toHaveAccessibleDescription(proposal.summary)
  await operator.hover()
  await expect(summary).toHaveCSS('opacity', '1')
  await page.mouse.move(0, 0)
  await expect(summary).toHaveCSS('opacity', '0')
  await operator.focus()
  await expect(summary).toHaveCSS('opacity', '1')
})

test('status marks follow the server actor and kind, with a recorded failure first', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/')
  const tones: [string, string, string][] = [
    ['Confirm the dashboard proposal', 'operator', 'Document proposal'],
    ['Build the opt-in stripe', 'working', 'Working'],
    ['Ship touch tooltips', 'merging', 'Merging'],
    ['Paginate lineage reads', 'waiting', 'Queued'],
    ['Wire the timeline attempt', 'waiting', 'Blocked'],
    // An operator kind with a recorded failure reads as the failure.
    ['Recover the stalled order', 'failure', 'Stalled'],
  ]
  for (const [title, tone, label] of tones) {
    const item = cardFor(page, title)
    await expect(item.locator('[data-status-mark]')).toHaveAttribute('data-status-mark', tone)
    await expect(item.locator('[data-next-action-label]')).toHaveText(label)
  }
  await expect(cardFor(page, 'Recover the stalled order')).toContainText('claim refused: no live claimant')
  // An assigned card shows the assignee's initials, so the label keeps its room.
  const assigned = cardFor(page, 'Build the opt-in stripe')
  await expect(assigned.getByRole('button', { name: /Kidus Tiliksew/ })).toContainText('KT')
  const label = assigned.locator('[data-next-action-label]')
  expect(await label.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true)
})

test('an agent-question checkpoint card reads as the operator question, not a failure', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/')
  const checkpoint = cardFor(page, 'Fix checkpoint presentation')
  await expect(checkpoint.locator('[data-next-action-label]')).toHaveText(agentQuestion.label)
  await expect(checkpoint.locator('[data-status-mark]')).toHaveAttribute('data-status-mark', 'operator')
  await expect(checkpoint.locator('[data-next-action-label]')).toHaveClass(/text-attention/)
  await expect(checkpoint).not.toContainText('operator checkpoint reached')
  await expect(checkpoint.locator('.text-failure')).toHaveCount(0)
  await expect(checkpoint).toHaveAccessibleDescription(agentQuestion.summary)
  // A forge failure on the same task still prints and still sets the tone.
  const forge = cardFor(page, 'Publish the checkpoint comment')
  await expect(forge.locator('[data-status-mark]')).toHaveAttribute('data-status-mark', 'failure')
  await expect(forge.getByText(/GitHub refused the comment/)).toHaveClass(/text-failure/)
  await expect(forge).not.toContainText('operator checkpoint reached')
})

test('an operator card is not tinted; only the mark and the label carry the colour', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/')
  const operator = cardFor(page, 'Confirm the dashboard proposal')
  const calm = cardFor(page, 'Paginate lineage reads')
  const background = await calm.evaluate((element) => getComputedStyle(element).backgroundColor)
  await expect(operator).toHaveCSS('background-color', background)
  await expect(operator).toHaveCSS(
    'border-color',
    await calm.evaluate((element) => getComputedStyle(element).borderColor),
  )
})

test('a selected card keeps the primary border', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/')
  const calm = cardFor(page, 'Paginate lineage reads')
  const resting = await calm.evaluate((element) => getComputedStyle(element).borderColor)
  await page.route('**/v1/tasks/task-queued/**', (route) => route.fulfill({ status: 404, json: {} }))
  await calm.click()
  await expect(calm).not.toHaveCSS('border-color', resting)
})

test('the dependency mark names every blocker on hover and focus, and reads attention for a closed blocker', async ({
  page,
}) => {
  await mockBoard(page)
  await page.goto('/')
  const waiting = cardFor(page, 'Wire the timeline attempt')
  const mark = waiting.locator('[data-dependency-mark="waiting"]')
  await expect(mark).toBeVisible()
  const tip = mark.locator('[role="tooltip"]')
  await expect(tip).toHaveCSS('opacity', '0')
  await mark.hover()
  await expect(tip).toHaveCSS('opacity', '1')
  await expect(tip).toContainText('Build the opt-in stripe')
  await expect(tip).toContainText('Paginate lineage reads')
  // The card's summary steps aside while the dependency tooltip is open.
  await expect(waiting.locator('[data-next-action-summary]')).toHaveCSS('opacity', '0')
  await page.mouse.move(0, 0)
  await waiting.focus()
  await expect(tip).toHaveCSS('opacity', '1')
  // No chip is left behind, and a task without blockers carries no mark.
  await expect(waiting.getByText('Waiting on dependencies')).toHaveCount(0)
  await expect(cardFor(page, 'Paginate lineage reads').locator('[data-dependency-mark]')).toHaveCount(0)

  const closed = cardFor(page, 'Retire the stage chart').locator('[data-dependency-mark="unsatisfiable"]')
  await expect(closed).toHaveAttribute('aria-label', 'Needs attention: Abandoned prerequisite closed without merging')
  await expect(closed).toHaveClass(/text-attention/)
})

test('Board search still matches a task ID', async ({ page }) => {
  const counts = await mockBoard(page)
  await page.goto('/')
  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('task-merging')
  await expect.poll(() => counts.activityQueries.at(-1)).toBe('task-merging')
  await expect(cardFor(page, 'Ship touch tooltips')).toBeVisible()
  await expect(cardFor(page, 'Paginate lineage reads')).toHaveCount(0)
})

test('with stage progress off, no card mounts a stripe and the Board sends no flow request', async ({ page }) => {
  const counts = await mockBoard(page)
  await page.goto('/')
  await expect(cardFor(page, 'Build the opt-in stripe')).toBeVisible()
  await expect(page.locator('[data-stage-stripe]')).toHaveCount(0)
  await page.waitForTimeout(300)
  expect(counts.flow).toBe(0)
})

test('a malformed stored preference reads as off', async ({ page }) => {
  const counts = await mockBoard(page, { stageProgress: 'yes please' })
  await page.goto('/')
  await expect(cardFor(page, 'Build the opt-in stripe')).toBeVisible()
  await expect(page.locator('[data-stage-stripe]')).toHaveCount(0)
  expect(counts.flow).toBe(0)
})

test('with stage progress on, visible cards draw every served status and policy-kept gate', async ({ page }) => {
  const counts = await mockBoard(page, { stageProgress: 'true' })
  await page.goto('/')
  const gated = cardFor(page, 'Confirm the dashboard proposal')
  await expect(gated.locator('[data-stage]')).toHaveCount(6)
  await expect(gated.getByRole('img', { name: 'Plan — Server needs_operator label' })).toHaveAttribute(
    'data-status',
    'needs_operator',
  )
  await expect(gated.getByRole('img', { name: 'Plan approval — waiting for you' })).toHaveAttribute(
    'data-gate-state',
    'needs_operator',
  )
  await expect(gated.getByRole('img', { name: 'Merge approval — upcoming' })).toBeVisible()
  await expect(gated.locator('[data-stage="verify"]')).toHaveAttribute('data-status', 'off')

  // The stripe reads its flow once the card scrolls into view.
  const running = cardFor(page, 'Build the opt-in stripe')
  expect(counts.flowTasks.has('task-working')).toBe(false)
  await running.scrollIntoViewIfNeeded()
  await expect(running.getByRole('img', { name: 'Plan approval — passed' })).toBeVisible()
  await expect(running.locator('[data-stage="implement"]')).toHaveAttribute('data-status', 'in_progress')
  await expect(running.locator('[data-stage="review"]')).toHaveAttribute('data-status', 'waiting')
  // A policy that turned the merge gate off sends no gate, so none is drawn.
  await expect(running.locator('[data-gate="merge_approval"]')).toHaveCount(0)
  // The in-progress segment is wider than a finished one.
  const [done, active] = await Promise.all([
    running.locator('[data-stage="triage"]').boundingBox(),
    running.locator('[data-stage="implement"]').boundingBox(),
  ])
  expect(active?.width ?? 0).toBeGreaterThan(done?.width ?? 0)
  expect(counts.flow).toBeGreaterThan(0)
})

test('turning stage progress off unmounts every stripe and stops flow reads', async ({ page }) => {
  const counts = await mockBoard(page, { stageProgress: 'true' })
  await page.goto('/')
  await expect(page.locator('[data-stage-stripe]').first()).toBeVisible()
  await expect.poll(() => counts.flow).toBeGreaterThan(0)
  // Another tab turns it off: the storage event reaches this page.
  await page.evaluate(() => {
    localStorage.setItem('conveyor-show-stage-progress', 'false')
    window.dispatchEvent(new StorageEvent('storage', { key: 'conveyor-show-stage-progress', newValue: 'false' }))
  })
  await expect(page.locator('[data-stage-stripe]')).toHaveCount(0)
  // An activity refresh would refetch a mounted stripe's flow; none is mounted.
  const reads = counts.flow
  await page.waitForTimeout(1000)
  expect(counts.flow).toBe(reads)
})

// The stripe refreshes when the open task's detail does (useTaskFlow), so it
// never disagrees with the timeline beside it.
test('a detail refresh refreshes an opted-in card stripe', async ({ page }) => {
  let released = false
  let release = () => {}
  const frame = new Promise<void>((resolve) => {
    release = () => {
      released = true
      resolve()
    }
  })
  const counts = await mockBoard(page, {
    stageProgress: 'true',
    flow: (taskId) => {
      const flow = flowFor(taskId)
      if (!released || taskId !== 'task-proposal') return flow
      return { ...flow, stages: flow.stages.map((row) => (row.id === 'spec' ? { ...row, status: 'done' } : row)) }
    },
    stream: async () => {
      if (released) return null
      await frame
      return 'event: activity\ndata: {}\n\n'
    },
  })
  await page.goto('/tasks/task-proposal')
  // The open sheet makes the Board behind it inert, so the card is found by its link.
  const stripe = page.locator('a[href="/tasks/task-proposal"] [data-stage="spec"]')
  await expect(stripe).toHaveAttribute('data-status', 'needs_operator')
  const detailReads = counts.detail
  release()
  await expect.poll(() => counts.detail).toBeGreaterThan(detailReads)
  await expect(stripe).toHaveAttribute('data-status', 'done')
})

for (const theme of ['light', 'dark'] as const) {
  test(`cards render in the ${theme} theme`, async ({ page }) => {
    await page.addInitScript((value) => localStorage.setItem('conveyor-theme', value), theme)
    await mockBoard(page)
    await page.goto('/')
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
    const operator = cardFor(page, 'Confirm the dashboard proposal')
    await expect(operator.locator('[data-status-mark="operator"]')).toBeVisible()
    await expect(operator.locator('[data-next-action-label]')).toHaveClass(/text-attention/)
  })
}

test.describe('at 390 px', () => {
  test.use({ viewport: { width: 390, height: 844 } })

  test('a card fits its lane without horizontal overflow', async ({ page }) => {
    await mockBoard(page, { stageProgress: 'true' })
    await page.goto('/')
    const item = cardFor(page, 'Wire the timeline attempt')
    await expect(item).toBeVisible()
    const [scroll, client] = await item.evaluate((element) => [element.scrollWidth, element.clientWidth])
    expect(scroll).toBeLessThanOrEqual(client)
  })
})
