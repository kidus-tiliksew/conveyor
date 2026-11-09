import { expect, type Page, type Route } from '@playwright/test'
import type { PlanActs, PlanOrigin, PlanState } from '../src/lib/types'
import { test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'

// The dashboard's plan-state surfaces against mocks in the server's served
// shapes (component-web-task-surfaces invariants 57 to 65; component-plan-state
// invariants 1, 3 to 6; req-task-plan-state AC-1.4, AC-3.1, AC-3.2). Activity
// rows carry `task.plan_state`; the task detail read carries `task.plan_state`,
// `plan_acts`, and the plan version's `origin` and `base_commit`. The browser
// proves presentation and request composition; the Go suites own the acts'
// admission and effects.

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('conveyor-task-activity-view', 'timeline'))
})

const at = '2026-11-09T10:00:00Z'
const approvedAt = '2026-11-09T11:30:00Z'
const commit = '0123456789abcdef0123456789abcdef01234567'
const noActs: PlanActs = { set_needed: false, set_not_required: false, attach: false }
const labels: Record<PlanState, string> = {
  not_required: 'No plan required',
  needed: 'Planning needed',
  awaiting_approval: 'Plan awaiting approval',
  completed: 'Planning completed',
}
const working = {
  label: 'Working',
  actor: 'agent',
  kind: 'working',
  judgment: false,
  summary: 'An agent is working on this task.',
  deadline: null,
}

type SpecFixture = { origin?: PlanOrigin; base_commit?: string | null; approved?: boolean }
type EventFixture = Record<string, unknown>

function taskRow(id: string, planState: PlanState | undefined, title = `Task ${id}`) {
  return {
    id,
    workspace: 'demo',
    title,
    body: `Body of ${id}.`,
    class: 'feature',
    level: '',
    ...(planState ? { plan_state: planState } : {}),
    merge_approval: false,
    policy_version: 1,
    repo: 'conveyor',
    base_branch: 'main',
    branch: `conveyor/task-${id}`,
    state: 'running',
    next_stage: 'implement',
    source: 'api',
    created_at: at,
  }
}

function detail(
  id: string,
  planState: PlanState,
  options: {
    spec?: SpecFixture
    acts?: PlanActs
    events?: EventFixture[]
    nextAction?: Record<string, unknown>
  } = {},
) {
  const spec = options.spec
  return {
    task: taskRow(id, planState),
    jobs: [],
    events: options.events ?? [],
    interventions: [],
    work_orders: [],
    checkout_available: false,
    checkout_guidance: '',
    needs_attention: false,
    at_merge_gate: false,
    attachments: [],
    verification_evidence: [],
    plan_acts: options.acts ?? noActs,
    next_action: options.nextAction ?? working,
    ...(spec
      ? {
          spec: {
            task_id: id,
            version: 2,
            content: '## Approach\n\nSplit the reader.\n\n## Done criteria\n\n- The reader is split.',
            acceptance_count: 0,
            acceptance: null,
            decomposition: null,
            approved: spec.approved === true,
            created_at: at,
            ...(spec.approved ? { approved_at: approvedAt } : {}),
            origin: spec.origin ?? 'plan_order',
            base_commit: spec.base_commit ?? null,
          },
        }
      : {}),
  }
}

type Detail = ReturnType<typeof detail>
type PostReply = { status: number; json: Record<string, unknown> }

async function mockServer(
  page: Page,
  options: {
    details?: Detail[]
    cards?: Record<string, unknown>[]
    role?: 'viewer' | 'maintainer' | 'operator'
    replies?: PostReply[]
  } = {},
) {
  const details = new Map((options.details ?? []).map((item) => [item.task.id, item]))
  const posts: { path: string; body: Record<string, unknown> }[] = []
  const detailReads = new Map<string, number>()
  const activityQueries: URLSearchParams[] = []
  const replies = [...(options.replies ?? [])]
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'usr_person', role: options.role ?? 'operator' }, request),
      })
    if (path === '/v1/workspace')
      return route.fulfill({
        json: { workspace: 'demo', max_bounces: 3, database: 'postgres', repos: [], default_plan_state: 'needed' },
      })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    if (path === '/v1/activity') {
      activityQueries.push(url.searchParams)
      const cards = options.cards ?? []
      return route.fulfill({
        json: cards,
        headers: { 'X-Conveyor-Total': String(cards.length), 'X-Conveyor-Limit': '100', 'X-Conveyor-Offset': '0' },
      })
    }
    const act = path.match(/^\/v1\/tasks\/([^/]+)\/(policy|plan)$/)
    if (act && request.method() === 'POST') {
      posts.push({ path, body: request.postDataJSON() as Record<string, unknown> })
      const reply = replies.shift() ?? { status: 200, json: {} }
      return route.fulfill({ status: reply.status, json: reply.json })
    }
    const detailPath = path.match(/^\/v1\/tasks\/([^/]+)\/activity$/)
    if (detailPath) {
      const id = decodeURIComponent(detailPath[1])
      detailReads.set(id, (detailReads.get(id) ?? 0) + 1)
      const item = details.get(id)
      if (!item) return route.fulfill({ status: 404, json: { error: 'not_found' } })
      return route.fulfill({ json: item })
    }
    const nextAction = path.match(/^\/v1\/tasks\/([^/]+)\/next-action$/)
    if (nextAction) {
      const item = details.get(decodeURIComponent(nextAction[1]))
      return route.fulfill({ json: { ...(item?.next_action ?? working), choices: [], details: {} } })
    }
    if (path.endsWith('/stream')) return route.fulfill({ status: 204 })
    return route.fulfill({ json: [] })
  })
  return {
    posts: () => posts,
    detailReads: (id: string) => detailReads.get(id) ?? 0,
    activityQueries: () => activityQueries,
    setDetail: (item: Detail) => details.set(item.task.id, item),
  }
}

function card(id: string, planState: PlanState | undefined) {
  return {
    task: taskRow(id, planState, `Card ${planState ?? 'unstated'}`),
    latest_stage: 'implement',
    last_event_at: at,
    needs_attention: false,
    next_action: working,
  }
}

const boardCard = (page: Page, planState: string) => page.getByRole('link', { name: new RegExp(`^Card ${planState}`) })
const planSection = (page: Page) => page.getByRole('region', { name: 'Execution plan' })

// ---- Board card mark (invariants 7, 58; AC-1.4) ----

test('a board card marks needed, awaiting, and completed plans beside the title and none without a plan', async ({
  page,
}) => {
  await mockServer(page, {
    cards: [
      card('t-needed', 'needed'),
      card('t-awaiting', 'awaiting_approval'),
      card('t-completed', 'completed'),
      card('t-none', 'not_required'),
    ],
  })
  await page.goto('/w/demo/')
  for (const state of ['needed', 'awaiting_approval', 'completed'] as const) {
    const item = boardCard(page, state)
    const mark = item.locator('[data-plan-state-mark]')
    await expect(mark).toHaveAttribute('data-plan-state-mark', state)
    await expect(mark).toHaveAccessibleName(labels[state])
    // The mark sits beside the title, and the next-action label keeps its own
    // row and text.
    await expect(item.locator('[data-next-action-label]')).toHaveText('Working')
    const markBox = await mark.boundingBox()
    const titleBox = await item.getByText(`Card ${state}`, { exact: true }).boundingBox()
    const labelBox = await item.locator('[data-next-action-label]').boundingBox()
    if (!markBox || !titleBox || !labelBox) throw new Error('the card is not rendered')
    expect(markBox.y).toBeLessThan(titleBox.y + titleBox.height)
    expect(labelBox.y).toBeGreaterThanOrEqual(titleBox.y + titleBox.height)
  }
  const none = boardCard(page, 'not_required')
  await expect(none).toBeVisible()
  await expect(none.locator('[data-plan-state-mark]')).toHaveCount(0)
  await expect(none.locator('[data-next-action-label]')).toHaveText('Working')
})

// ---- Board filter (invariant 59) ----

test('the Plan state filter sends each checked value as a repeated plan_state', async ({ page }) => {
  const server = await mockServer(page, { cards: [card('t-needed', 'needed')] })
  await page.goto('/w/demo/')
  await expect(boardCard(page, 'needed')).toBeVisible()
  expect(server.activityQueries().every((query) => !query.has('plan_state'))).toBe(true)

  await page.getByRole('button', { name: 'Filters' }).click()
  await page.getByRole('tab', { name: 'Plan state' }).click()
  const options = page.getByRole('listbox', { name: 'Plan state' }).getByRole('option')
  await expect(options).toHaveText(['Any plan state', ...Object.values(labels)])
  await options.filter({ hasText: 'Planning needed' }).click()
  await options.filter({ hasText: 'Plan awaiting approval' }).click()
  await expect
    .poll(() => server.activityQueries().at(-1)?.getAll('plan_state').sort())
    .toEqual(['awaiting_approval', 'needed'])
  await expect(page.getByRole('tab', { name: 'Plan state' }).locator('.rounded-full')).toHaveCount(1)

  // Any plan state clears the member; the unfiltered page may come from the
  // cache, so the member's own state is what reads inactive.
  await options.filter({ hasText: 'Any plan state' }).click()
  await expect(page.getByRole('tab', { name: 'Plan state' }).locator('.rounded-full')).toHaveCount(0)
})

test('a saved filter without the Plan state member reads inactive', async ({ page }) => {
  await page.addInitScript(() =>
    localStorage.setItem(
      'conveyor-task-filters:board:demo',
      JSON.stringify({
        query: '',
        states: [],
        repositories: [],
        updated: '30d',
        updatedFrom: '',
        updatedTo: '',
        requirements: [],
        designs: [],
        assignee: '',
      }),
    ),
  )
  const server = await mockServer(page, { cards: [card('t-needed', 'needed')] })
  await page.goto('/w/demo/')
  await expect(boardCard(page, 'needed')).toBeVisible()
  expect(server.activityQueries().length).toBeGreaterThan(0)
  expect(server.activityQueries().every((query) => !query.has('plan_state'))).toBe(true)
  await expect(page.getByTestId('filter-count')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Reset filters' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Filters' }).click()
  await page.getByRole('tab', { name: 'Plan state' }).click()
  await expect(page.getByRole('tab', { name: 'Plan state' }).locator('.rounded-full')).toHaveCount(0)
  await expect(
    page.getByRole('listbox', { name: 'Plan state' }).getByRole('option', { name: 'Any plan state' }),
  ).toHaveAttribute('aria-selected', 'true')
})

// ---- Plan section (invariant 60; AC-1.4) ----

test('the plan section renders for every plan state without a plan version, headed by its label', async ({ page }) => {
  const states = Object.keys(labels) as PlanState[]
  await mockServer(page, { details: states.map((state) => detail(`bare-${state}`, state)) })
  for (const state of states) {
    await page.goto(`/w/demo/tasks/bare-${state}/full`)
    const section = planSection(page)
    await expect(section.locator('[data-plan-state-label]')).toHaveText(labels[state])
    await expect(section).toHaveAttribute('data-plan-state', state)
    await expect(section.getByRole('heading', { name: 'Execution plan' })).toHaveCount(0)
    await expect(section.locator('[data-plan-provenance]')).toHaveCount(0)
  }
  // The Board sheet renders the same section.
  await page.goto('/w/demo/tasks/bare-not_required')
  await expect(planSection(page).locator('[data-plan-state-label]')).toHaveText('No plan required')
})

test('the plan section shows each origin, the base commit or None recorded, and the approval per state', async ({
  page,
}) => {
  const cases: Array<{
    id: string
    state: PlanState
    spec: SpecFixture
    origin: string
    approval: string
  }> = [
    {
      id: 'v-awaiting',
      state: 'awaiting_approval',
      spec: { origin: 'intake', base_commit: commit },
      origin: 'Filed with the task',
      approval: 'Awaiting approval',
    },
    {
      id: 'v-completed',
      state: 'completed',
      spec: { origin: 'plan_order', approved: true },
      origin: 'Written by the plan order',
      approval: 'Approved',
    },
    {
      id: 'v-needed',
      state: 'needed',
      spec: { origin: 'operator', base_commit: commit },
      origin: 'Attached by an operator',
      approval: 'Not approved',
    },
    {
      id: 'v-not-required',
      state: 'not_required',
      spec: { origin: 'plan_order' },
      origin: 'Written by the plan order',
      approval: 'Not approved',
    },
  ]
  await mockServer(page, { details: cases.map((entry) => detail(entry.id, entry.state, { spec: entry.spec })) })
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'])
  for (const entry of cases) {
    await page.goto(`/w/demo/tasks/${entry.id}/full`)
    const section = planSection(page)
    await expect(section.locator('[data-plan-state-label]')).toHaveText(labels[entry.state])
    await expect(section.getByRole('heading', { name: 'Execution plan' })).toBeVisible()
    await expect(section.locator('[data-plan-origin]')).toHaveText(entry.origin)
    const status = section.locator('[data-plan-status]')
    await expect(status.getByText(entry.approval, { exact: true })).toBeVisible()
    if (entry.approval === 'Approved') await expect(status).toContainText('approved Nov 9')
    const base = section.locator('[data-plan-base-commit]')
    if (entry.spec.base_commit) {
      await expect(base.locator('code')).toHaveText(commit.slice(0, 12))
      await base.getByRole('button', { name: 'Copy base commit' }).click()
      expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(commit)
    } else {
      await expect(base).toHaveText('None recorded')
    }
  }
})

// ---- Operator acts (invariants 61, 62, 64; AC-3.1, AC-3.2) ----

const actCombinations: PlanActs[] = [
  noActs,
  { set_needed: true, set_not_required: false, attach: false },
  { set_needed: false, set_not_required: true, attach: false },
  { set_needed: true, set_not_required: true, attach: false },
  { set_needed: false, set_not_required: false, attach: true },
  { set_needed: true, set_not_required: false, attach: true },
  { set_needed: false, set_not_required: true, attach: true },
  { set_needed: true, set_not_required: true, attach: true },
]

test('the section offers exactly the acts plan_acts marks true', async ({ page }) => {
  await mockServer(page, {
    details: actCombinations.map((acts, index) => detail(`acts-${index}`, 'needed', { acts })),
  })
  for (const [index, acts] of actCombinations.entries()) {
    await page.goto(`/w/demo/tasks/acts-${index}/full`)
    const section = planSection(page)
    await expect(section.locator('[data-plan-state-label]')).toHaveText('Planning needed')
    const change = section.getByRole('button', { name: 'Change plan state' })
    const attach = section.getByRole('button', { name: 'Attach plan' })
    await expect(change).toHaveCount(acts.set_needed || acts.set_not_required ? 1 : 0)
    await expect(attach).toHaveCount(acts.attach ? 1 : 0)
    if (acts.set_needed || acts.set_not_required) {
      await change.click()
      const dialog = page.getByRole('dialog', { name: 'Change plan state' })
      const targets = [acts.set_needed && 'Planning needed', acts.set_not_required && 'No plan required'].filter(
        (value): value is string => Boolean(value),
      )
      await expect(dialog.getByRole('radio')).toHaveCount(targets.length)
      for (const target of targets) await expect(dialog.getByRole('radio', { name: target })).toBeVisible()
      await dialog.getByRole('button', { name: 'Cancel' }).click()
    }
  }
})

test('the acts render for no caller without operate_gates', async ({ page }) => {
  await mockServer(page, { role: 'viewer', details: [detail('acts-viewer', 'needed', { acts: actCombinations[7] })] })
  await page.goto('/w/demo/tasks/acts-viewer/full')
  await expect(planSection(page).locator('[data-plan-state-label]')).toHaveText('Planning needed')
  await expect(page.getByRole('button', { name: 'Change plan state' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Attach plan' })).toHaveCount(0)
})

test('Change plan state sends one POST per submission and reuses its request_id on an unchanged retry', async ({
  page,
}) => {
  const server = await mockServer(page, {
    details: [detail('act-state', 'needed', { acts: { set_needed: false, set_not_required: true, attach: false } })],
    replies: [
      { status: 500, json: { error: 'The server could not record the change.' } },
      { status: 200, json: {} },
    ],
  })
  await page.goto('/w/demo/tasks/act-state/full')
  await page.getByRole('button', { name: 'Change plan state' }).click()
  const dialog = page.getByRole('dialog', { name: 'Change plan state' })
  const submit = dialog.getByRole('button', { name: 'Change plan state' })
  // A single target is preselected; the reason is required.
  await expect(dialog.getByRole('radio', { name: 'No plan required' })).toBeChecked()
  await expect(submit).toBeDisabled()
  await dialog.getByLabel('Reason (required)').fill('   ')
  await expect(submit).toBeDisabled()
  await dialog.getByLabel('Reason (required)').fill('  The change is a one-line copy fix. ')
  await submit.click()
  await expect(dialog.getByRole('alert')).toHaveText('The server could not record the change.')
  expect(server.posts()).toHaveLength(1)
  const readsBefore = server.detailReads('act-state')

  await submit.click()
  await expect(dialog).toHaveCount(0)
  const posts = server.posts()
  expect(posts).toHaveLength(2)
  expect(posts[0].path).toBe('/v1/tasks/act-state/policy')
  expect(Object.keys(posts[0].body).sort()).toEqual(['plan_state', 'reason', 'request_id'])
  expect(posts[0].body).toMatchObject({ plan_state: 'not_required', reason: 'The change is a one-line copy fix.' })
  expect(typeof posts[0].body.request_id).toBe('string')
  expect(posts[1].body).toEqual(posts[0].body)
  // Success refreshes task detail in the captured workspace.
  await expect.poll(() => server.detailReads('act-state')).toBeGreaterThan(readsBefore)
})

test('a changed input takes a fresh request_id, and a new dialog makes its own', async ({ page }) => {
  const server = await mockServer(page, {
    details: [detail('act-fresh', 'needed', { acts: { set_needed: false, set_not_required: true, attach: false } })],
    replies: [
      { status: 500, json: { error: 'Try again.' } },
      { status: 500, json: { error: 'Try again.' } },
    ],
  })
  await page.goto('/w/demo/tasks/act-fresh/full')
  await page.getByRole('button', { name: 'Change plan state' }).click()
  let dialog = page.getByRole('dialog', { name: 'Change plan state' })
  await dialog.getByLabel('Reason (required)').fill('First reason')
  await dialog.getByRole('button', { name: 'Change plan state' }).click()
  await expect(dialog.getByRole('alert')).toBeVisible()
  await dialog.getByLabel('Reason (required)').fill('Second reason')
  await dialog.getByRole('button', { name: 'Change plan state' }).click()
  await expect.poll(() => server.posts().length).toBe(2)
  expect(server.posts()[1].body.request_id).not.toBe(server.posts()[0].body.request_id)
  await dialog.getByRole('button', { name: 'Cancel' }).click()

  await page.getByRole('button', { name: 'Change plan state' }).click()
  dialog = page.getByRole('dialog', { name: 'Change plan state' })
  await dialog.getByLabel('Reason (required)').fill('Second reason')
  await dialog.getByRole('button', { name: 'Change plan state' }).click()
  await expect.poll(() => server.posts().length).toBe(3)
  expect(server.posts()[2].body.request_id).not.toBe(server.posts()[1].body.request_id)
})

test('Attach plan sends one POST with the plan, the trimmed base commit, the reason, and a request_id', async ({
  page,
}) => {
  const server = await mockServer(page, {
    details: [
      detail('act-attach', 'not_required', { acts: { set_needed: true, set_not_required: false, attach: true } }),
    ],
  })
  await page.goto('/w/demo/tasks/act-attach/full')
  await page.getByRole('button', { name: 'Attach plan' }).click()
  const dialog = page.getByRole('dialog', { name: 'Attach plan' })
  const submit = dialog.getByRole('button', { name: 'Attach plan' })
  // Approval stays on the gate card.
  await expect(dialog.getByText(/approve/i)).toHaveCount(0)
  const plan = '## Approach\n\nOne reader.\n\n## Done criteria\n\n- Done.'
  await dialog.getByLabel('Plan (Markdown)').fill(plan)
  await dialog.getByLabel('Reason (required)').fill('Planned with the operator')
  await dialog.getByLabel('Base commit (optional)').fill('abc123')
  await expect(dialog.getByRole('alert')).toHaveText(
    'Base commit must be a full commit ID of 40 lowercase hexadecimal characters.',
  )
  await expect(submit).toBeDisabled()
  await dialog.getByLabel('Base commit (optional)').fill(` ${commit} `)
  await submit.click()
  await expect(dialog).toHaveCount(0)
  const posts = server.posts()
  expect(posts).toHaveLength(1)
  expect(posts[0].path).toBe('/v1/tasks/act-attach/plan')
  expect(Object.keys(posts[0].body).sort()).toEqual(['base_commit', 'plan', 'reason', 'request_id'])
  expect(posts[0].body).toMatchObject({ plan, base_commit: commit, reason: 'Planned with the operator' })
})

test('Attach plan without a base commit sends none', async ({ page }) => {
  const server = await mockServer(page, {
    details: [
      detail('act-attach-bare', 'needed', { acts: { set_needed: false, set_not_required: true, attach: true } }),
    ],
  })
  await page.goto('/w/demo/tasks/act-attach-bare/full')
  await page.getByRole('button', { name: 'Attach plan' }).click()
  const dialog = page.getByRole('dialog', { name: 'Attach plan' })
  await dialog.getByLabel('Plan (Markdown)').fill('## Approach\n\nA plan.')
  await dialog.getByLabel('Reason (required)').fill('Attached')
  await dialog.getByRole('button', { name: 'Attach plan' }).click()
  await expect(dialog).toHaveCount(0)
  expect(Object.keys(server.posts()[0].body).sort()).toEqual(['plan', 'reason', 'request_id'])
})

test('a 409 shows the server message in place, refetches detail, and is never retried', async ({ page }) => {
  const server = await mockServer(page, {
    details: [detail('act-409', 'needed', { acts: { set_needed: false, set_not_required: true, attach: true } })],
    replies: [{ status: 409, json: { error: 'an implementation order of task act-409 is claimed' } }],
  })
  await page.goto('/w/demo/tasks/act-409/full')
  await expect(planSection(page).locator('[data-plan-state-label]')).toHaveText('Planning needed')
  const readsBefore = server.detailReads('act-409')
  // The refetch serves the moved task, whose acts the server no longer admits.
  server.setDetail(detail('act-409', 'needed', { acts: { set_needed: false, set_not_required: true, attach: false } }))
  await page.getByRole('button', { name: 'Attach plan' }).click()
  const dialog = page.getByRole('dialog', { name: 'Attach plan' })
  await dialog.getByLabel('Plan (Markdown)').fill('## Approach\n\nA plan.')
  await dialog.getByLabel('Reason (required)').fill('Attach it')
  await dialog.getByRole('button', { name: 'Attach plan' }).click()
  await expect(dialog.getByRole('alert')).toHaveText('an implementation order of task act-409 is claimed')
  await expect.poll(() => server.detailReads('act-409')).toBeGreaterThan(readsBefore)
  // The page's own offer follows the refetched plan_acts.
  await expect(planSection(page).getByRole('button', { name: 'Attach plan' })).toHaveCount(0)
  await page.waitForTimeout(500)
  expect(server.posts()).toHaveLength(1)
})

test('a 400 or 403 shows the server message and is never retried', async ({ page }) => {
  const server = await mockServer(page, {
    details: [detail('act-400', 'needed', { acts: { set_needed: false, set_not_required: true, attach: false } })],
    replies: [
      { status: 400, json: { error: 'nonblank reason and request_id are required' } },
      { status: 403, json: { error: 'operate_gates is required' } },
    ],
  })
  await page.goto('/w/demo/tasks/act-400/full')
  await page.getByRole('button', { name: 'Change plan state' }).click()
  const dialog = page.getByRole('dialog', { name: 'Change plan state' })
  await dialog.getByLabel('Reason (required)').fill('Reason')
  await dialog.getByRole('button', { name: 'Change plan state' }).click()
  await expect(dialog.getByRole('alert')).toHaveText('nonblank reason and request_id are required')
  await page.waitForTimeout(300)
  expect(server.posts()).toHaveLength(1)
  await dialog.getByRole('button', { name: 'Change plan state' }).click()
  await expect(dialog.getByRole('alert')).toHaveText('operate_gates is required')
  await page.waitForTimeout(300)
  expect(server.posts()).toHaveLength(2)
})

// ---- Timeline (invariant 65) and the planning request card ----

test('a plan-state move names both labels, the cause, and the actor', async ({ page }) => {
  await mockServer(page, {
    details: [
      detail('timeline-plan', 'not_required', {
        events: [
          {
            id: 1,
            task_id: 'timeline-plan',
            kind: 'task.plan_state_changed',
            actor_id: 'user:usr_person',
            actor_role: 'human',
            payload: { from: null, to: 'needed', cause: 'intake' },
            at,
          },
          {
            id: 2,
            task_id: 'timeline-plan',
            kind: 'task.plan_state_changed',
            actor_id: 'user:usr_person',
            actor_role: 'human',
            payload: { from: 'needed', to: 'not_required', cause: 'operator' },
            at: '2026-11-09T10:05:00Z',
          },
        ],
      }),
    ],
  })
  await page.goto('/w/demo/tasks/timeline-plan/full')
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('Plan state set to Planning needed')).toBeVisible()
  await expect(timeline.getByText('Set when the task was filed · By User · usr_person')).toBeVisible()
  await expect(timeline.getByText('Plan state changed from Planning needed to No plan required')).toBeVisible()
  await expect(timeline.getByText('Changed by an operator · By User · usr_person')).toBeVisible()
})

test('a planning request from a task that needs no plan shows its decision card', async ({ page }) => {
  await mockServer(page, {
    details: [
      {
        ...detail('planning-request', 'not_required', {
          nextAction: {
            label: 'Approve plan revision',
            actor: 'operator',
            kind: 'plan_revision',
            judgment: true,
            summary: 'Implementation asked to revise the plan.',
            deadline: null,
          },
          events: [
            {
              id: 1,
              task_id: 'planning-request',
              kind: 'work_order.plan_revision_requested',
              actor_id: 'user:usr_person',
              actor_role: 'human',
              payload: {
                work_order_id: 'planning-request-implement-1',
                attempt_id: 'attempt-1',
                session_id: 'session-1',
                rationale: 'The change spans three packages; it needs a plan first.',
              },
              at,
            },
          ],
        }),
        task: { ...taskRow('planning-request', 'not_required'), state: 'awaiting_human' },
      },
    ],
  })
  await page.goto('/w/demo/tasks/planning-request/full')
  const card = page.getByRole('region', { name: 'Human gate' })
  await expect(card.getByRole('heading', { name: 'Planning requested' })).toBeVisible()
  await expect(card.getByText('The change spans three packages; it needs a plan first.')).toBeVisible()
  await expect(card.getByText('Contested plan')).toHaveCount(0)
  await expect(card.getByRole('button', { name: 'Approve planning' })).toBeVisible()
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('The agent asked for a plan before implementing')).toBeVisible()
})
