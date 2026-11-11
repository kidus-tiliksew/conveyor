import { expect, type Page, type Route, type TestInfo } from '@playwright/test'
import type { PlanActs, PlanOrigin, PlanState, RiskTier, TaskRisk } from '../src/lib/types'
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
  summary: 'An agent is implementing this task.',
  deadline: null,
}

type SpecFixture = {
  origin?: PlanOrigin
  base_commit?: string | null
  approved?: boolean
  risk_tier?: RiskTier
  risk_reason?: string
  content?: string
}
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
    risk?: TaskRisk
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
    ...(options.risk ? { risk: options.risk } : {}),
    ...(spec
      ? {
          spec: {
            task_id: id,
            version: 2,
            content: spec.content ?? '## Approach\n\nSplit the reader.\n\n## Done criteria\n\n- The reader is split.',
            acceptance_count: 0,
            acceptance: null,
            decomposition: null,
            approved: spec.approved === true,
            created_at: at,
            ...(spec.approved ? { approved_at: approvedAt } : {}),
            origin: spec.origin ?? 'plan_order',
            base_commit: spec.base_commit ?? null,
            ...(spec.risk_tier ? { risk_tier: spec.risk_tier } : {}),
            ...(spec.risk_reason ? { risk_reason: spec.risk_reason } : {}),
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
  let detailFails = false
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
      if (detailFails) return route.fulfill({ status: 503, json: { error: 'detail unavailable' } })
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
    failDetail: (fails: boolean) => {
      detailFails = fails
    },
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

// ---- No Board mark (invariant 58; AC-1.4) ----

test('a board card carries no plan-state mark for any plan state', async ({ page }) => {
  await mockServer(page, {
    cards: [
      card('t-none', 'not_required'),
      card('t-needed', 'needed'),
      card('t-awaiting', 'awaiting_approval'),
      card('t-completed', 'completed'),
    ],
  })
  await page.goto('/w/demo/')
  for (const state of ['not_required', 'needed', 'awaiting_approval', 'completed'] as const) {
    const item = boardCard(page, state)
    await expect(item).toBeVisible()
    await expect(item.locator('[data-plan-state-mark]')).toHaveCount(0)
    await expect(item.locator('[data-next-action-label]')).toHaveText('Working')
  }
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

test('the plan section shows no origin, the base commit or None recorded, and the approval per state', async ({
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
    // The read keeps spec.origin, and the section shows no origin (DEC-127(3)).
    await expect(section.locator('[data-plan-origin]')).toHaveCount(0)
    await expect(section.getByText(entry.origin)).toHaveCount(0)
    await expect(section.getByText('Origin', { exact: true })).toHaveCount(0)
    const status = section.locator('[data-plan-status]')
    await expect(status.getByText(entry.approval, { exact: true })).toBeVisible()
    // The approval or draft time sits behind the badge: hover and keyboard
    // focus open it, and the badge's accessible name carries it.
    await expect(status).not.toContainText('Nov 9')
    const time = entry.approval === 'Approved' ? /^Approved Nov 9, / : /^Drafted Nov 9, /
    const badge = status.getByRole('button', { name: new RegExp(`^${entry.approval}\\. `) })
    await expect(badge).toHaveAccessibleName(new RegExp(`^${entry.approval}\\. ${time.source.slice(1)}`))
    await badge.hover()
    await expect(status.locator('[data-plan-approval-time]')).toHaveText(time)
    await page.mouse.move(0, 0)
    await expect(status.locator('[data-plan-approval-time]')).toHaveCount(0)
    await badge.focus()
    await expect(status.locator('[data-plan-approval-time]')).toHaveText(time)
    await badge.blur()
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

// ---- Plan risk tier (req-risk-based-review AC-4.4) ----

const planRiskReason = 'Approval-time ratings span three stores, so a wrong write could weaken the review floor.'
const pathRisk: TaskRisk = {
  mode: 'record',
  tier: 'medium',
  source: 'path_rule',
  reason: 'matched path rules: GitHub Actions workflows (medium)',
  required_agent_reviews: 1,
  required_human_approvals: 0,
  calculated_agent_reviews: 1,
  calculated_human_approvals: 1,
  recorded_approvals: 0,
  author_approval_needed: false,
  ratings: [],
}
const riskPlanContent = `## Approach\n\nRecord the plan tier.\n\n## Files touched\n\n- internal/store/risk.go\n\n## Ordering\n\n1. Parse.\n\n## Risks\n\n- Three stores.\n\nRisk tier: high — ${planRiskReason}\n\n## Done criteria\n\n- The plan is rated.`

test('at plan approval the plan card shows the version tier and reason on both task-detail variants', async ({
  page,
}) => {
  await mockServer(page, {
    details: [
      detail('risk-awaiting', 'awaiting_approval', {
        spec: {
          origin: 'plan_order',
          base_commit: commit,
          risk_tier: 'high',
          risk_reason: planRiskReason,
          content: riskPlanContent,
        },
        risk: pathRisk,
      }),
      detail('risk-legacy', 'awaiting_approval', { spec: { origin: 'plan_order' } }),
    ],
  })
  for (const path of ['/w/demo/tasks/risk-awaiting', '/w/demo/tasks/risk-awaiting/full']) {
    await page.goto(path)
    const plan = planSection(page).locator('[data-plan-risk]')
    await expect(plan).toHaveAttribute('data-plan-risk', 'high')
    await expect(plan.getByText('Plan risk tier', { exact: true })).toBeVisible()
    await expect(plan.locator('[data-risk-tier]')).toHaveText('High')
    // The chip shows alone; its reason sits behind it.
    await expect(plan.locator('[data-plan-risk-reason]')).toHaveCount(0)
    await expect(plan.getByText(planRiskReason)).toHaveCount(0)
    const chip = plan.getByRole('button', { name: `Plan risk tier High. ${planRiskReason}` })
    await chip.focus()
    await expect(plan.locator('[data-plan-risk-reason]')).toHaveText(planRiskReason)
    await chip.blur()
    await expect(plan.locator('[data-plan-risk-reason]')).toHaveCount(0)
    await expect(
      planSection(page).locator('[data-plan-status]').getByText('Awaiting approval', { exact: true }),
    ).toBeVisible()
    // The task's own tier stays the server's aggregate, apart from the plan's.
    await expect(page.locator('[data-task-risk]').first().locator('[data-risk-tier]')).toHaveText('Medium')
  }
  // A version that states no tier shows no invented one.
  for (const path of ['/w/demo/tasks/risk-legacy', '/w/demo/tasks/risk-legacy/full']) {
    await page.goto(path)
    await expect(planSection(page).getByRole('heading', { name: 'Execution plan' })).toBeVisible()
    await expect(planSection(page).locator('[data-plan-risk]')).toHaveCount(0)
  }
})

// The screenshots attach to the Playwright report, which CI uploads as an
// artifact, and are also written to CONVEYOR_SCREENSHOT_DIR when it is set.
async function capture(page: Page, testInfo: TestInfo, name: string) {
  const shots = process.env.CONVEYOR_SCREENSHOT_DIR
  const image = await page.screenshot({ path: shots ? `${shots}/${name}` : undefined, fullPage: true })
  await testInfo.attach(name, { body: image, contentType: 'image/png' })
}

const riskCompleted = detail('risk-completed', 'completed', {
  spec: {
    origin: 'plan_order',
    approved: true,
    base_commit: commit,
    risk_tier: 'low',
    risk_reason: planRiskReason,
    content: riskPlanContent,
  },
})

// The plan facts' term column and value column, measured on the page.
async function planFactLayout(page: Page) {
  return planSection(page)
    .locator('[data-plan-facts]')
    .evaluate((list) => {
      const box = (selector: string) => list.querySelector(selector)!.getBoundingClientRect()
      const terms = Array.from(list.querySelectorAll('dt')).map((term) => term.getBoundingClientRect())
      const values = Array.from(list.querySelectorAll('dd')).map((value) => value.getBoundingClientRect())
      return {
        header: list.parentElement!.getBoundingClientRect().width,
        termLefts: terms.map((term) => Math.round(term.left)),
        valueLefts: values.map((value) => Math.round(value.left)),
        termWidths: terms.map((term) => Math.round(term.width)),
        rows: terms.map((term) => Math.round(term.top)),
        commitTop: Math.round(box('[data-plan-base-commit]').top),
        riskTop: Math.round(box('[data-plan-risk] dd').top),
      }
    })
}

test.describe('at phone width on a touch screen', () => {
  test.use({
    viewport: { width: 375, height: 800 },
    isMobile: true,
    hasTouch: true,
    locale: 'en-US',
    timezoneId: 'UTC',
  })

  test('the plan tier stays beside an approved plan and opens its reason by tap', async ({ page }, testInfo) => {
    await mockServer(page, { details: [riskCompleted] })
    await page.goto('/w/demo/tasks/risk-completed/full')
    const plan = planSection(page).locator('[data-plan-risk]')
    await expect(plan.locator('[data-risk-tier]')).toHaveText('Low')
    await expect(plan.locator('[data-plan-risk-reason]')).toHaveCount(0)
    const chip = plan.getByRole('button', { name: `Plan risk tier Low. ${planRiskReason}` })
    await chip.tap()
    await expect(plan.locator('[data-plan-risk-reason]')).toHaveText(planRiskReason)
    await expect(planSection(page).locator('[data-plan-origin]')).toHaveCount(0)
    // Below 40rem the facts form two columns with a 6rem term column.
    const layout = await planFactLayout(page)
    expect(layout.header).toBeLessThan(640)
    expect(new Set(layout.termLefts).size).toBe(1)
    expect(new Set(layout.valueLefts).size).toBe(1)
    expect(layout.valueLefts[0] - layout.termLefts[0]).toBeGreaterThanOrEqual(96)
    expect(layout.riskTop).toBeGreaterThan(layout.commitTop)
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
    expect(overflow).toBeLessThanOrEqual(0)
    await capture(page, testInfo, 'plan-risk-open-375.png')
    // The approval time opens by tap.
    const status = planSection(page).locator('[data-plan-status]')
    await status.getByRole('button', { name: /^Approved\. Approved Nov 9, / }).tap()
    await expect(status.locator('[data-plan-approval-time]')).toHaveText(/^Approved Nov 9, 11:30/)
  })
})

test('the plan facts share one line on a header at least 40rem wide', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await mockServer(page, { details: [riskCompleted] })
  await page.goto('/w/demo/tasks/risk-completed/full')
  await expect(planSection(page).locator('[data-plan-risk] [data-risk-tier]')).toHaveText('Low')
  const layout = await planFactLayout(page)
  expect(layout.header).toBeGreaterThanOrEqual(640)
  expect(new Set(layout.rows).size).toBe(1)
  expect(layout.termLefts[1]).toBeGreaterThan(layout.valueLefts[0])
  await capture(page, testInfo, 'plan-1440.png')
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
  server.setDetail(detail('act-409', 'needed', { acts: noActs }))
  await page.getByRole('button', { name: 'Attach plan' }).click()
  const dialog = page.getByRole('dialog', { name: 'Attach plan' })
  await dialog.getByLabel('Plan (Markdown)').fill('## Approach\n\nA plan.')
  await dialog.getByLabel('Reason (required)').fill('Attach it')
  await dialog.getByRole('button', { name: 'Attach plan' }).click()
  await expect(dialog.getByRole('alert')).toHaveText('an implementation order of task act-409 is claimed')
  await expect.poll(() => server.detailReads('act-409')).toBeGreaterThan(readsBefore)
  // The page's own offer follows the refetched plan_acts, while the open
  // dialog keeps the refusal in place.
  await expect(planSection(page).getByRole('button', { name: 'Attach plan' })).toHaveCount(0)
  await expect(planSection(page).getByRole('button', { name: 'Change plan state' })).toHaveCount(0)
  await expect(dialog.getByRole('alert')).toHaveText('an implementation order of task act-409 is claimed')
  // The withdrawn act cannot be submitted again from the open dialog.
  await expect(dialog.getByRole('button', { name: 'Attach plan' })).toBeDisabled()
  await expect(dialog.getByText('The server no longer accepts an attached plan for this task.')).toBeVisible()
  await page.waitForTimeout(500)
  expect(server.posts()).toHaveLength(1)
})

test('a recorded act whose detail refresh fails stays blocked and offers a read-only retry', async ({ page }) => {
  const server = await mockServer(page, {
    details: [detail('act-refresh', 'needed', { acts: { set_needed: false, set_not_required: true, attach: false } })],
  })
  await page.goto('/w/demo/tasks/act-refresh/full')
  await page.getByRole('button', { name: 'Change plan state' }).click()
  const dialog = page.getByRole('dialog', { name: 'Change plan state' })
  await dialog.getByLabel('Reason (required)').fill('No plan needed')
  server.failDetail(true)
  await dialog.getByRole('button', { name: 'Change plan state' }).click()
  await expect(dialog.getByRole('alert')).toContainText('The change was recorded, but the task could not refresh.')
  await expect(dialog.getByRole('button', { name: 'Change plan state' })).toBeDisabled()
  expect(server.posts()).toHaveLength(1)

  // The retry reads detail again and sends no act.
  server.setDetail(detail('act-refresh', 'not_required'))
  server.failDetail(false)
  await dialog.getByRole('button', { name: 'Retry refresh' }).click()
  await expect(dialog).toHaveCount(0)
  await expect(planSection(page).locator('[data-plan-state-label]')).toHaveText('No plan required')
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

// A plan-state move reads from the plan section and its label, so the timeline
// renders no entry for an intake, plan-submission, or operator move, with or
// without an omitted payload, while technical activity still lists each
// recorded move (component-web-task-surfaces invariants 24 and 65;
// component-plan-state invariant 3).
test('plan-state moves render no timeline entry and stay in technical activity', async ({ page }) => {
  const id = 'timeline-plan'
  const move = (eventID: number, minute: number, payload: Record<string, unknown>) => ({
    id: eventID,
    task_id: id,
    kind: 'task.plan_state_changed',
    actor_id: 'user:usr_person',
    actor_role: 'human',
    payload,
    at: `2026-11-09T10:0${minute}:00Z`,
  })
  const events: EventFixture[] = [
    move(1, 0, { from: null, to: 'needed', cause: 'intake' }),
    move(2, 2, { from: 'needed', to: 'awaiting_approval', cause: 'plan_submitted' }),
    move(3, 4, { from: 'awaiting_approval', to: 'not_required', cause: 'operator' }),
    {
      ...move(4, 6, {}),
      payload: null,
      payload_omitted: true,
      payload_bytes: 9000,
      payload_index: { from: 'not_required', to: 'needed', cause: 'operator' },
      audit_available: true,
    },
  ]
  await mockServer(page, { details: [detail(id, 'needed', { events })] })
  await page.route(`**/v1/tasks/${id}/history*`, (route) =>
    route.fulfill({
      json: {
        selection: 'technical',
        limit: 50,
        events: events.map((entry) => ({ ...entry, id: String(entry.id) })),
        boundary: { max_id: '4', count: events.length },
        has_older: false,
        has_newer: false,
      },
    }),
  )
  await page.goto(`/w/demo/tasks/${id}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline).toBeVisible()
  await expect(timeline.getByText(/Plan state/)).toHaveCount(0)
  for (const label of Object.values(labels)) await expect(timeline.getByText(label)).toHaveCount(0)
  await expect(
    timeline.getByText(/Set when the task was filed|A plan was submitted|Changed by an operator/),
  ).toHaveCount(0)
  await expect(timeline.getByText('Details omitted from this page')).toHaveCount(0)
  await expect(planSection(page).locator('[data-plan-state-label]')).toHaveText('Planning needed')
  await page.getByRole('button', { name: 'Show technical activity' }).click()
  const technical = page.getByRole('list', { name: 'Technical activity events' })
  await expect(technical.getByText('task.plan_state_changed', { exact: true })).toHaveCount(4)
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

test('Escape in a plan dialog over the Board sheet closes only the dialog', async ({ page }) => {
  await mockServer(page, {
    cards: [card('sheet-escape', 'needed')],
    details: [detail('sheet-escape', 'needed', { acts: { set_needed: false, set_not_required: true, attach: true } })],
  })
  await page.goto('/w/demo/tasks/sheet-escape')
  const sheet = page.getByRole('dialog', { name: /Task/ }).first()
  await expect(planSection(page).locator('[data-plan-state-label]')).toHaveText('Planning needed')
  await planSection(page).getByRole('button', { name: 'Attach plan' }).click()
  const dialog = page.getByRole('dialog', { name: 'Attach plan' })
  await expect(dialog).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(dialog).toHaveCount(0)
  await expect(sheet).toBeVisible()
  await expect(planSection(page)).toBeVisible()
  // With no dialog open, Escape closes the sheet as before.
  await page.keyboard.press('Escape')
  await expect(planSection(page)).toHaveCount(0)
})

test('at phone width the filter panel stays on screen after a change and shows whole plan-state labels', async ({
  page,
}) => {
  await page.setViewportSize({ width: 375, height: 812 })
  await mockServer(page, { cards: [card('t-needed', 'needed')] })
  await page.goto('/w/demo/')
  await page.getByRole('button', { name: 'Filters' }).click()
  await page.getByRole('tab', { name: 'Plan state' }).click()
  const options = page.getByRole('listbox', { name: 'Plan state' }).getByRole('option')
  // The first change shows Reset beside the trigger and moves it; the panel
  // is measured again and stays inside the page.
  await options.filter({ hasText: 'Planning needed' }).click()
  await expect(page.getByRole('button', { name: 'Reset filters' })).toBeVisible()
  const panel = await page.locator('[data-filter-panel]').boundingBox()
  if (!panel) throw new Error('the filter panel is not rendered')
  expect(panel.x).toBeGreaterThanOrEqual(0)
  expect(panel.x + panel.width).toBeLessThanOrEqual(375)
  for (const label of Object.values(labels)) {
    const text = options.filter({ hasText: label }).locator('span.truncate')
    expect(await text.evaluate((node) => node.scrollWidth <= node.clientWidth)).toBe(true)
  }
  for (const category of ['Requirement', 'System Design']) {
    const text = page.getByRole('tab', { name: category }).locator('span.truncate')
    expect(await text.evaluate((node) => node.scrollWidth <= node.clientWidth)).toBe(true)
  }
})
