// Live status on task detail and the crate page, and the deployment reports
// line of the GitHub App card (req-production-feedback AC-2.2, AC-3.2, AC-3.4,
// AC-3.6, AC-11.1; component-production-feedback invariants 12, 16 to 18, 22,
// and 23; component-web-dashboard invariant 15). A fake server answers in the
// shapes internal/httpapi writes: `environments` on `GET /v1/tasks/{id}` for a
// merged task only, `environments` and merged members' `live_in` on the crate
// read, the environment rows of `GET /v1/environments`, and
// `deployment_reports` on the GitHub App read. The page derives no status,
// count, or time. Every value is synthetic.
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { expect, type Page, type Route } from '@playwright/test'
import type {
  CrateDetail,
  CrateEnvironmentLine,
  CrateInsight,
  CrateTask,
  DeploymentReportsState,
  EnvironmentRow,
  TaskEnvironmentStatus,
} from '../src/lib/types'
import { test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'
import { mockSettingsPage, openSettingsRow, settingsGitHubApp } from './helpers/settings'

const createdAt = '2026-11-01T09:00:00Z'

// A row with `listening` undefined leaves the field out of the answer, as an
// answer that states no listening state would.
function row(id: string, name: string, listening: boolean | undefined, lastSignalAt: string | null): EnvironmentRow {
  const value: EnvironmentRow = {
    id,
    name,
    tracked_branches: { conveyor: 'main' },
    github_environments: [name],
    version_rule: null,
    timing_window_minutes: 60,
    archived: false,
    created_by: 'usr_operator',
    created_at: createdAt,
    keys: [],
    listening,
    last_signal_at: lastSignalAt,
    newest_unresolved: null,
  }
  if (listening === undefined) delete value.listening
  return value
}

const environmentRows = [
  row('env-production', 'production', true, '2026-11-26T09:58:00Z'),
  row('env-staging', 'staging', false, '2026-11-25T18:00:00Z'),
  row('env-qa', 'qa', false, null),
  row('env-preview', 'preview', true, '2026-11-26T09:50:00Z'),
  // The edge row states no listening state.
  row('env-edge', 'edge', undefined, '2026-11-26T09:40:00Z'),
]

function status(
  id: string,
  name: string,
  value: TaskEnvironmentStatus['status'],
  overrides: Partial<TaskEnvironmentStatus> = {},
): TaskEnvironmentStatus {
  return {
    environment: { id, name },
    status: value,
    services: [],
    live_since: null,
    on_tracked_branch: false,
    as_of: '2026-11-26T09:58:00Z',
    ...overrides,
  }
}

// Each of the four statuses, and Not live yet both with and without the merge
// on a tracked branch.
const taskStatuses: TaskEnvironmentStatus[] = [
  status('env-production', 'production', 'live', {
    services: ['api', 'worker'],
    live_since: '2026-11-25T14:00:00Z',
    on_tracked_branch: true,
  }),
  status('env-staging', 'staging', 'not_live_yet', { on_tracked_branch: true, as_of: '2026-11-25T18:00:00Z' }),
  status('env-preview', 'preview', 'not_live_yet', { on_tracked_branch: false, as_of: '2026-11-26T09:50:00Z' }),
  status('env-qa', 'qa', 'no_longer_live', { services: null, as_of: '2026-11-24T10:00:00Z' }),
  status('env-edge', 'edge', 'unknown', { as_of: null }),
]

const nextAction = {
  label: 'Finished',
  actor: 'none',
  kind: 'terminal',
  judgment: false,
  summary: 'This task is finished.',
  deadline: null,
}

function activity(id: string, state: string) {
  return {
    task: {
      id,
      workspace: 'demo',
      title: state === 'merged' ? 'Send invoices by email' : 'Add refunds',
      body: 'Send each invoice by email.',
      repo: 'conveyor',
      base_branch: 'main',
      state,
      branch: `conveyor/task-${id}`,
      created_at: createdAt,
    },
    jobs: [],
    events: [],
    interventions: [],
    work_orders: [],
    reviews: [],
    latest_stage: 'implement',
    next_action: nextAction,
    checkout_available: false,
    checkout_guidance: '',
    needs_attention: false,
  }
}

function crateTask(id: string, title: string, state: CrateTask['state'], liveIn?: string[]): CrateTask {
  return {
    id,
    title,
    state,
    branch: `conveyor/task-${id}`,
    pull_request: null,
    next_action: { label: state === 'merged' ? 'Finished' : 'Agent working', actor: 'agent', kind: 'working' },
    ...(liveIn ? { live_in: liveIn } : {}),
  } as CrateTask
}

const crateLines: CrateEnvironmentLine[] = [
  {
    environment: { id: 'env-production', name: 'production' },
    live_task_count: 1,
    merged_task_count: 2,
    newest_live_at: '2026-11-25T14:00:00Z',
    listening: true,
    last_signal_at: '2026-11-26T09:57:00Z',
    as_of: '2026-11-26T09:58:00Z',
  },
  {
    environment: { id: 'env-staging', name: 'staging' },
    live_task_count: 0,
    merged_task_count: 2,
    newest_live_at: null,
    listening: false,
    last_signal_at: '2026-11-25T18:00:00Z',
    as_of: '2026-11-25T18:00:00Z',
  },
]

function crate(environments: CrateEnvironmentLine[], liveIn: boolean): CrateDetail {
  return {
    id: 'crate-billing',
    name: 'Billing v2',
    target_date: null,
    state: 'open',
    shipped: false,
    created_by: 'usr_operator',
    created_at: createdAt,
    closed_at: null,
    open_task_count: 1,
    merged_task_count: 2,
    total_task_count: 3,
    requirements: [],
    undeclared_requirements: [],
    tasks: [
      crateTask('261120-a01', 'Send invoices by email', 'merged', liveIn ? ['production'] : []),
      crateTask('261120-b02', 'Store invoice numbers', 'merged', []),
      crateTask('261120-c03', 'Add refunds', 'running'),
    ],
    environments,
  }
}

function insight(detail: CrateDetail): CrateInsight {
  const start = '2026-11-10T09:00:00Z'
  return {
    tasks: detail.tasks.map((task) => ({
      id: task.id,
      phase: task.state === 'merged' ? 'merged' : 'building',
      station: task.state === 'merged' ? 'crate' : 'build',
      base_branch: 'main',
      created_at: start,
      pull_request_opened_at: [],
      changes_requested_at: [],
      review_rounds: 0,
      merged_at: null,
      closed_at: null,
    })),
    branches: ['main'],
    whereabouts: [],
    progress: {
      daily: [],
      forecast: null,
      weekly: [],
      delivered: 2,
      total: 3,
      open: 1,
      throughput_per_week: 0,
      median_cycle_seconds: null,
      status_mix: { delivered: 2, closed: 0, open: [] },
    },
    first_review: { approved: 0, reviewed: 0 },
    forecast_days_before_target: null,
    replay: { start, end: '2026-11-26T10:00:00Z', steps: [] },
  } as unknown as CrateInsight
}

interface Options {
  statuses?: TaskEnvironmentStatus[]
  // The served environments_pending of the task read. Undefined leaves the
  // field out.
  taskPending?: boolean
  crate?: CrateDetail
}

async function mockServer(page: Page, options: Options = {}) {
  const state = {
    statuses: options.statuses ?? taskStatuses,
    taskPending: options.taskPending,
    rows: environmentRows.map((item) => ({ ...item })),
    crate: options.crate ?? crate(crateLines, true),
    failCrate: false,
    failTask: false,
    gets: [] as string[],
  }
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    localStorage.setItem('conveyor-theme', 'light')
    localStorage.setItem('conveyor-task-activity-view', 'timeline')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (request.method() === 'GET') state.gets.push(`${path}${url.search}`)
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({ json: callerIdentity({ id: 'usr_operator', role: 'operator' }, request) })
    if (path === '/v1/workspace')
      return route.fulfill({
        json: { workspace: 'demo', database: 'postgres', max_bounces: 2, repos: [{ name: 'conveyor', base: 'main' }] },
      })
    if (path === '/v1/environments') return route.fulfill({ json: state.rows })
    const taskRead = path.match(/^\/v1\/tasks\/([^/]+)(?:\/(activity|next-action))?$/)
    if (taskRead) {
      const id = decodeURIComponent(taskRead[1])
      const taskState = id.startsWith('running') || id === '261120-c03' ? 'running' : 'merged'
      if (taskRead[2] === 'next-action') return route.fulfill({ json: { ...nextAction, choices: [], details: {} } })
      if (taskRead[2] === 'activity') return route.fulfill({ json: activity(id, taskState) })
      // GET /v1/tasks/{id}: a merged task always carries `environments`, []
      // when no environment is relevant; any other task has no field.
      const item = activity(id, taskState).task
      if (taskState !== 'merged') return route.fulfill({ json: item })
      if (state.failTask) return route.fulfill({ status: 500, json: { error: 'internal error' } })
      const pending = state.taskPending === undefined ? {} : { environments_pending: state.taskPending }
      return route.fulfill({ json: { ...item, environments: state.statuses, ...pending } })
    }
    if (path === '/v1/crates/crate-billing') {
      if (state.failCrate) return route.fulfill({ status: 500, json: { error: 'internal error' } })
      return route.fulfill({ json: state.crate })
    }
    if (path === '/v1/crates/crate-billing/insight') return route.fulfill({ json: insight(state.crate) })
    if (path === '/v1/crates/crate-billing/activity') return route.fulfill({ json: { events: [] } })
    if (path === '/v1/crates/crate-billing/briefing')
      return route.fulfill({ json: { briefing: null, fingerprint: 'none', can_generate: false } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    return route.fulfill({ json: [] })
  })
  return state
}

// Reads name their workspace in the query, so a count compares the path only.
const pathOf = (read: string) => read.split('?')[0]
const taskReads = (state: { gets: string[] }, id: string) =>
  state.gets.filter((read) => pathOf(read) === `/v1/tasks/${id}`).length
const crateReads = (state: { gets: string[] }) =>
  state.gets.filter((read) => pathOf(read) === '/v1/crates/crate-billing').length

async function expectTaskLines(page: Page) {
  const section = page.getByRole('region', { name: 'Live status' })
  await expect(section).toBeVisible()
  const line = (id: string) => section.locator(`[data-live-environment="${id}"]`)
  await expect(section.locator('[data-live-environment]')).toHaveCount(5)

  await expect(line('env-production')).toHaveAttribute('data-live-status', 'live')
  await expect(line('env-production')).toContainText('production')
  await expect(line('env-production')).toContainText('Live')
  await expect(line('env-production')).toContainText('Runs in api, worker.')
  await expect(line('env-production').locator('time').first()).toHaveAttribute('datetime', '2026-11-25T14:00:00Z')
  await expect(line('env-production').locator('[data-live-as-of] time')).toHaveAttribute(
    'datetime',
    '2026-11-26T09:58:00Z',
  )
  await expect(line('env-production').locator('[data-watcher-state]')).toHaveCount(0)

  await expect(line('env-staging')).toContainText('Not live yet')
  await expect(line('env-staging')).toContainText('A tracked branch has the merge.')
  // A line for an environment with no watcher says so, with its last signal.
  await expect(line('env-staging').locator('[data-watcher-state="not-listening"]')).toContainText(
    'No watcher is listening.',
  )
  await expect(line('env-staging').locator('[data-watcher-state] time')).toHaveAttribute(
    'datetime',
    '2026-11-25T18:00:00Z',
  )

  await expect(line('env-preview')).toContainText('Not live yet')
  await expect(line('env-preview')).toContainText('No tracked branch has the merge yet.')

  await expect(line('env-qa')).toContainText('No longer live')
  await expect(line('env-qa')).toContainText('A past deploy had this task. No service runs it now.')
  await expect(line('env-qa').locator('[data-watcher-state="not-listening"]')).toContainText(
    'No watcher is listening. No signal has arrived.',
  )

  await expect(line('env-edge')).toContainText('Unknown')
  await expect(line('env-edge')).toContainText('The repository history does not have the merge commit.')
  await expect(line('env-edge').locator('[data-live-as-of]')).toHaveCount(0)
  // A row without a listening state shows no watcher state, where the
  // explicit false of staging and qa shows the no-watcher message.
  await expect(line('env-edge').locator('[data-watcher-state]')).toHaveCount(0)
  await expect(line('env-edge')).not.toContainText('No watcher is listening.')

  // No line offers a delivery or gate control (req-production-feedback
  // AC-11.1).
  await expect(section.getByRole('button')).toHaveCount(0)
}

test('a merged task shows one status line per environment in the sheet and on the full page', async ({ page }) => {
  await mockServer(page)
  await page.goto('/w/demo/tasks/261120-a01')
  await expect(page.getByRole('dialog')).toBeVisible()
  await expectTaskLines(page)

  await page.goto('/w/demo/tasks/261120-a01/full')
  await expect(page.getByRole('heading', { name: 'Send invoices by email', level: 1 })).toBeVisible()
  await expectTaskLines(page)
})

test('a task that is not merged reads no live status and shows none', async ({ page }) => {
  const state = await mockServer(page)
  await page.goto('/w/demo/tasks/running-1/full')
  await expect(page.getByRole('heading', { name: 'Add refunds', level: 1 })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Live status' })).toHaveCount(0)
  expect(taskReads(state, 'running-1')).toBe(0)
  expect(state.gets.some((path) => path.startsWith('/v1/environments'))).toBe(false)
})

test('a merged task with no relevant environment shows the page as before and reads on no timer', async ({ page }) => {
  await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
  const state = await mockServer(page, { statuses: [] })
  await page.goto('/w/demo/tasks/261120-a01/full')
  await expect(page.getByRole('heading', { name: 'Send invoices by email', level: 1 })).toBeVisible()
  await expect.poll(() => taskReads(state, '261120-a01')).toBeGreaterThanOrEqual(1)
  await expect(page.getByRole('region', { name: 'Live status' })).toHaveCount(0)
  await expect(page.getByText('Live status')).toHaveCount(0)
  expect(state.gets.some((path) => path.startsWith('/v1/environments'))).toBe(false)
  const reads = taskReads(state, '261120-a01')
  await page.clock.runFor(130_000)
  expect(taskReads(state, '261120-a01')).toBe(reads)
})

test('task live status reads again every 60 seconds and stops when the sheet closes', async ({ page }) => {
  await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
  const state = await mockServer(page)
  await page.goto('/w/demo/tasks/261120-a01')
  await expectTaskLines(page)
  await page.clock.runFor(1_000)
  const base = taskReads(state, '261120-a01')
  const rows = state.gets.filter((path) => path.startsWith('/v1/environments')).length
  expect(base).toBeGreaterThanOrEqual(1)

  state.statuses = taskStatuses.map((item) =>
    item.environment.id === 'env-staging'
      ? { ...item, status: 'live', services: ['api'], live_since: '2026-11-26T10:00:20Z' }
      : item,
  )
  await page.clock.runFor(57_000)
  expect(taskReads(state, '261120-a01')).toBe(base)
  await page.clock.runFor(3_000)
  await expect.poll(() => taskReads(state, '261120-a01')).toBe(base + 1)
  await expect.poll(() => state.gets.filter((path) => path.startsWith('/v1/environments')).length).toBe(rows + 1)
  await expect(page.locator('[data-live-environment="env-staging"]')).toHaveAttribute('data-live-status', 'live')
  await expect(page.locator('[data-live-environment="env-staging"]')).toContainText('Runs in api.')

  // Closing the sheet ends both timed reads.
  await page.getByRole('button', { name: 'Close panel' }).click()
  await expect(page.getByRole('region', { name: 'Live status' })).toHaveCount(0)
  const after = state.gets.length
  await page.clock.runFor(180_000)
  expect(state.gets.slice(after).filter((read) => pathOf(read) === '/v1/tasks/261120-a01')).toHaveLength(0)
  expect(state.gets.slice(after).filter((path) => path.startsWith('/v1/environments'))).toHaveLength(0)
})

test('the crate page shows a line per environment and where each merged task is live', async ({ page }) => {
  await mockServer(page)
  await page.goto('/w/demo/crates/crate-billing')
  const section = page.getByRole('region', { name: 'Live status' })
  await expect(section).toBeVisible()
  const production = section.locator('[data-crate-live-environment="env-production"]')
  await expect(production).toContainText('production')
  await expect(production.locator('[data-crate-live-counts]')).toHaveText('1 of 2 merged tasks is live.')
  await expect(production).toContainText('Newest went live')
  await expect(production.locator('time').first()).toHaveAttribute('datetime', '2026-11-25T14:00:00Z')
  // A listening environment shows the served positive state and its served
  // last signal (req-production-feedback AC-3.4).
  await expect(production.locator('[data-watcher-state="listening"]')).toContainText(
    'A watcher is listening. Last signal',
  )
  await expect(production.locator('[data-watcher-state] time')).toHaveAttribute('datetime', '2026-11-26T09:57:00Z')
  await expect(production.locator('[data-live-as-of] time')).toHaveAttribute('datetime', '2026-11-26T09:58:00Z')

  const staging = section.locator('[data-crate-live-environment="env-staging"]')
  await expect(staging.locator('[data-crate-live-counts]')).toHaveText('0 of 2 merged tasks are live.')
  await expect(staging).toContainText('No task is live here yet.')
  await expect(staging.locator('[data-watcher-state="not-listening"]')).toContainText('No watcher is listening.')
  await expect(staging.locator('[data-watcher-state] time')).toHaveAttribute('datetime', '2026-11-25T18:00:00Z')

  // Each merged row names the served environments; a running row has none.
  await expect(page.locator('[data-crate-task="261120-a01"] [data-task-live-in]')).toHaveText('Live in production')
  await expect(page.locator('[data-crate-task="261120-b02"] [data-task-live-in]')).toHaveText(
    'Not live in an environment',
  )
  await expect(page.locator('[data-crate-task="261120-c03"] [data-task-live-in]')).toHaveCount(0)
})

test('the crate read repeats every 60 seconds while the page is open and stops after leaving it', async ({ page }) => {
  await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
  const state = await mockServer(page)
  await page.goto('/w/demo/crates/crate-billing')
  await expect(page.getByRole('region', { name: 'Live status' })).toBeVisible()
  await page.clock.runFor(1_000)
  const base = crateReads(state)
  const insightReads = state.gets.filter((read) => pathOf(read).endsWith('/insight')).length

  state.crate = crate(
    crateLines.map((line) => (line.environment.id === 'env-staging' ? { ...line, live_task_count: 2 } : line)),
    true,
  )
  await page.clock.runFor(57_000)
  expect(crateReads(state)).toBe(base)
  await page.clock.runFor(3_000)
  await expect.poll(() => crateReads(state)).toBe(base + 1)
  await expect(page.locator('[data-crate-live-environment="env-staging"] [data-crate-live-counts]')).toHaveText(
    '2 of 2 merged tasks are live.',
  )
  // Only the crate read is timed: the insight view reads on no timer.
  expect(state.gets.filter((read) => pathOf(read).endsWith('/insight')).length).toBe(insightReads)

  await page.getByRole('link', { name: 'Crates' }).first().click()
  await expect(page).toHaveURL(/\/w\/demo\/crates$/)
  const after = crateReads(state)
  await page.clock.runFor(180_000)
  expect(crateReads(state)).toBe(after)
})

test('a failed timed crate read keeps the page and its open dialog', async ({ page }) => {
  await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
  const state = await mockServer(page)
  await page.goto('/w/demo/crates/crate-billing')
  await expect(page.getByRole('region', { name: 'Live status' })).toBeVisible()
  await page.getByRole('button', { name: 'Crate actions' }).click()
  await page.getByRole('menuitem', { name: 'Edit crate' }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog).toBeVisible()
  const base = crateReads(state)
  state.failCrate = true
  // The timed read and the dashboard's one retry both fail. fastForward
  // reaches the 60-second read without running every animation frame of the
  // line's scene in between. Each failed answer arrives in real time, so the
  // poll waits between the short clock steps that cover the retry backoff.
  await page.clock.fastForward(60_000)
  await expect
    .poll(
      async () => {
        await page.clock.runFor(1_000)
        return crateReads(state)
      },
      { timeout: 20_000 },
    )
    .toBeGreaterThanOrEqual(base + 2)
  await page.clock.runFor(1_000)
  await expect(page.getByText('This crate did not load')).toHaveCount(0)
  await expect(dialog).toBeVisible()
  await expect(page.getByRole('region', { name: 'Live status' })).toBeVisible()
})

test('a crate with no environment shows the page as before and reads on no timer', async ({ page }) => {
  await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
  const state = await mockServer(page, { crate: crate([], false) })
  await page.goto('/w/demo/crates/crate-billing')
  await expect(page.locator('[data-crate-task="261120-a01"]')).toBeVisible()
  await expect(page.getByRole('region', { name: 'Live status' })).toHaveCount(0)
  await expect(page.locator('[data-task-live-in]')).toHaveCount(0)
  await page.clock.runFor(1_000)
  const reads = crateReads(state)
  await page.clock.runFor(130_000)
  expect(crateReads(state)).toBe(reads)
})

// The pending reads (component-production-feedback invariant 23;
// req-production-feedback AC-3.2). While an answer carries
// environments_pending true, the page reads again after 10 seconds, at most
// six times in a row. The clock runs at its natural pace between the steps,
// so each step jumps over a whole delay and then lets the answer settle.

// hideableTab lets a test hide and show the tab. The page reads the
// visibility state that the browser would report.
async function hideableTab(page: Page) {
  await page.addInitScript(() => {
    let hidden = false
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => (hidden ? 'hidden' : 'visible'),
    })
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden })
    ;(window as unknown as { setTabHidden: (value: boolean) => void }).setTabHidden = (value: boolean) => {
      hidden = value
      document.dispatchEvent(new Event('visibilitychange', { bubbles: true }))
    }
  })
}

async function setTabHidden(page: Page, hidden: boolean) {
  await page.evaluate(
    (value) => (window as unknown as { setTabHidden: (value: boolean) => void }).setTabHidden(value),
    hidden,
  )
}

// settle lets a served answer render and the page set its next timer.
async function settle(page: Page) {
  await page.evaluate(() => new Promise((resolve) => setTimeout(resolve, 200)))
}

type MockState = Awaited<ReturnType<typeof mockServer>>

interface Surface {
  name: string
  path: string
  readPath: string
  reads: (state: MockState) => number
  serve: (state: MockState, shown: boolean, pending: boolean | undefined) => void
  lines: (page: Page) => ReturnType<Page['locator']>
  leave: (page: Page) => Promise<void>
}

const pendingTaskLine = status('env-production', 'production', 'live', {
  services: ['api'],
  live_since: '2026-11-26T10:00:05Z',
  as_of: '2026-11-26T10:00:05Z',
})

const surfaces: Surface[] = [
  {
    name: 'task',
    path: '/w/demo/tasks/261120-a01',
    readPath: '/v1/tasks/261120-a01',
    reads: (state) => taskReads(state, '261120-a01'),
    serve: (state, shown, pending) => {
      state.statuses = shown ? [pendingTaskLine] : []
      state.taskPending = pending
    },
    lines: (page) => page.locator('[data-live-environment]'),
    leave: async (page) => {
      await page.getByRole('button', { name: 'Close panel' }).click()
      await expect(page.getByRole('dialog')).toHaveCount(0)
    },
  },
  {
    name: 'crate',
    path: '/w/demo/crates/crate-billing',
    readPath: '/v1/crates/crate-billing',
    reads: crateReads,
    serve: (state, shown, pending) => {
      const detail = crate(shown ? [crateLines[0]] : [], shown)
      state.crate = pending === undefined ? detail : { ...detail, environments_pending: pending }
    },
    lines: (page) => page.locator('[data-crate-live-environment]'),
    leave: async (page) => {
      await page.getByRole('link', { name: 'Crates' }).first().click()
      await expect(page).toHaveURL(/\/w\/demo\/crates$/)
    },
  },
]

async function openSurface(page: Page, surface: Surface, state: MockState) {
  await page.goto(surface.path)
  await expect.poll(() => surface.reads(state)).toBeGreaterThanOrEqual(1)
  await settle(page)
  return surface.reads(state)
}

// step jumps the clock and checks how many reads of the surface it caused.
async function step(page: Page, surface: Surface, state: MockState, ms: number, reads: number) {
  const before = surface.reads(state)
  await page.clock.fastForward(ms)
  if (reads > 0) await expect.poll(() => surface.reads(state)).toBe(before + reads)
  await settle(page)
  expect(surface.reads(state)).toBe(before + reads)
}

for (const surface of surfaces) {
  test(`the ${surface.name} page shows a line that arrives on the second read without reopening`, async ({ page }) => {
    await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
    const state = await mockServer(page)
    surface.serve(state, false, true)
    await openSurface(page, surface, state)
    await expect(page.getByRole('region', { name: 'Live status' })).toHaveCount(0)

    surface.serve(state, true, false)
    await step(page, surface, state, 9_000, 0)
    await step(page, surface, state, 1_000, 1)
    await expect(surface.lines(page)).toHaveCount(1)
    await expect(surface.lines(page).first()).toContainText('production')

    // The answer is not pending and shows a line, so the 60-second rule
    // applies again.
    await step(page, surface, state, 50_000, 0)
    await step(page, surface, state, 10_000, 1)

    // Leaving the page stops the timer.
    surface.serve(state, false, true)
    await step(page, surface, state, 60_000, 1)
    await surface.leave(page)
    await step(page, surface, state, 130_000, 0)
  })

  test(`six pending ${surface.name} reads in a row end the reads for an empty answer`, async ({ page }) => {
    await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
    const state = await mockServer(page)
    surface.serve(state, false, true)
    await openSurface(page, surface, state)
    for (let read = 1; read <= 6; read++) {
      await step(page, surface, state, 9_000, 0)
      await step(page, surface, state, 1_000, 1)
    }
    // No seventh timed read, before or after 60 seconds.
    await step(page, surface, state, 10_000, 0)
    await step(page, surface, state, 130_000, 0)
    await expect(page.getByRole('region', { name: 'Live status' })).toHaveCount(0)
  })

  test(`a pending ${surface.name} answer with a line reads six times, then every 60 seconds`, async ({ page }) => {
    await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
    const state = await mockServer(page)
    surface.serve(state, true, true)
    await openSurface(page, surface, state)
    await expect(surface.lines(page)).toHaveCount(1)
    for (let read = 1; read <= 6; read++) await step(page, surface, state, 10_000, 1)
    // The six pending reads are spent, and the page shows a line, so the
    // 60-second rule applies.
    await step(page, surface, state, 50_000, 0)
    await step(page, surface, state, 10_000, 1)
    // A pending answer from a 60-second read starts no new run of 10-second
    // reads.
    await step(page, surface, state, 10_000, 0)
    await step(page, surface, state, 40_000, 0)
    await step(page, surface, state, 10_000, 1)
    // An answer that is not pending ends the run, and a later pending answer
    // from a 60-second read still starts none.
    surface.serve(state, true, false)
    await step(page, surface, state, 60_000, 1)
    surface.serve(state, true, true)
    await step(page, surface, state, 60_000, 1)
    await step(page, surface, state, 10_000, 0)
    await step(page, surface, state, 50_000, 1)
    await expect(surface.lines(page)).toHaveCount(1)
  })

  test(`an empty ${surface.name} answer that is not pending sets no timer`, async ({ page }) => {
    await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
    const state = await mockServer(page)
    surface.serve(state, false, false)
    await openSurface(page, surface, state)
    await step(page, surface, state, 10_000, 0)
    await step(page, surface, state, 130_000, 0)
  })

  test(`a hidden tab makes no pending ${surface.name} read and no catch-up read`, async ({ page }) => {
    await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
    await hideableTab(page)
    const state = await mockServer(page)
    surface.serve(state, false, true)
    await openSurface(page, surface, state)
    await setTabHidden(page, true)
    await step(page, surface, state, 60_000, 0)
    // Showing the tab makes no read at once. The page waits the whole delay.
    await setTabHidden(page, false)
    await step(page, surface, state, 9_000, 0)
    await step(page, surface, state, 1_000, 1)
    // The hidden time used none of the six reads.
    for (let read = 2; read <= 6; read++) await step(page, surface, state, 10_000, 1)
    await step(page, surface, state, 70_000, 0)
  })
}

test('a failed pending crate read keeps the page, counts as one of the six, and is not retried', async ({ page }) => {
  await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
  const state = await mockServer(page)
  const surface = surfaces[1]
  surface.serve(state, true, true)
  await openSurface(page, surface, state)
  state.failCrate = true
  await step(page, surface, state, 10_000, 1)
  // No retry read follows the failure, and the page keeps the served line.
  await step(page, surface, state, 5_000, 0)
  await expect(page.getByText('This crate did not load')).toHaveCount(0)
  await expect(surface.lines(page)).toHaveCount(1)
  state.failCrate = false
  await step(page, surface, state, 5_000, 1)
  for (let read = 3; read <= 6; read++) await step(page, surface, state, 10_000, 1)
  await step(page, surface, state, 50_000, 0)
  await step(page, surface, state, 10_000, 1)
})

test('a failed pending task read keeps the served line and counts as one of the six', async ({ page }) => {
  await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
  const state = await mockServer(page)
  const surface = surfaces[0]
  surface.serve(state, true, true)
  await openSurface(page, surface, state)
  state.failTask = true
  await step(page, surface, state, 10_000, 1)
  await expect(surface.lines(page)).toHaveCount(1)
  state.failTask = false
  for (let read = 2; read <= 6; read++) await step(page, surface, state, 10_000, 1)
  await step(page, surface, state, 50_000, 0)
  await step(page, surface, state, 10_000, 1)
})

const deploymentReportsText: Record<DeploymentReportsState, string[]> = {
  on: ['Deployment reports: on. GitHub sends deployment reports to Conveyor.'],
  missing_permission: [
    'The App does not have read access to Deployments.',
    'The App owner must give the App read access to Deployments on GitHub.',
  ],
  missing_event: [
    'The App does not subscribe to the Deployment status event.',
    'The App owner must subscribe the App to the Deployment status event on GitHub.',
  ],
  missing_permission_and_event: [
    'The App does not have read access to Deployments and does not subscribe to the Deployment status event.',
    'The App owner must give the App read access to Deployments and subscribe it to the Deployment status event on GitHub.',
  ],
}

for (const [served, texts] of Object.entries(deploymentReportsText)) {
  test(`the GitHub App card states deployment reports when the read serves ${served}`, async ({ page }) => {
    const state = await mockSettingsPage(page)
    state.githubApp = { ...settingsGitHubApp, deployment_reports: served }
    await page.goto('/w/demo/settings')
    const region = await openSettingsRow(page, 'GitHub App')
    const line = region.locator(`[data-deployment-reports="${served}"]`)
    await expect(line).toBeVisible()
    for (const text of texts) await expect(line).toContainText(text)
    if (served !== 'on')
      await expect(line).toContainText('Then an owner of the installation account must approve the change')
  })
}

test('the GitHub App card shows no deployment reports line when the read omits it', async ({ page }) => {
  const state = await mockSettingsPage(page)
  state.githubApp = { ...settingsGitHubApp, installation_account: '' }
  await page.goto('/w/demo/settings')
  const region = await openSettingsRow(page, 'GitHub App')
  await expect(region.getByText('Not installed yet.')).toBeVisible()
  await expect(region.locator('[data-deployment-reports]')).toHaveCount(0)
  await expect(region.getByText('Deployment reports')).toHaveCount(0)
})

// The visual check of the task sheet, the full task page, the crate page, and
// the GitHub App card at 375 px and at a desktop width (AGENTS.md, UI
// changes). It runs only when CONVEYOR_VISUAL_CHECK_DIR names a directory
// outside the checkout, so ordinary runs and CI skip it and no screenshot
// reaches the repository.
test.describe('visual check', () => {
  const outputDir = process.env.CONVEYOR_VISUAL_CHECK_DIR
  test.skip(!outputDir, 'set CONVEYOR_VISUAL_CHECK_DIR to capture the visual check')

  async function capture(page: Page, name: string, width: number) {
    const dir = outputDir as string
    mkdirSync(dir, { recursive: true })
    await page.evaluate(() =>
      Promise.all(
        document
          .getAnimations()
          .filter((animation) => animation.effect?.getComputedTiming().iterations !== Number.POSITIVE_INFINITY)
          .map((animation) => animation.finished.catch(() => undefined)),
      ),
    )
    await page.screenshot({ path: join(dir, `${name}-${width}.png`), fullPage: true })
  }

  for (const width of [375, 1280]) {
    test(`task, crate, and GitHub App surfaces at ${width} px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 })
      await mockServer(page)
      await page.goto('/w/demo/tasks/261120-a01')
      await expect(page.getByRole('region', { name: 'Live status' })).toBeVisible()
      await page.getByRole('region', { name: 'Live status' }).scrollIntoViewIfNeeded()
      await capture(page, 'task-sheet-live-status', width)

      await page.goto('/w/demo/tasks/261120-a01/full')
      await expect(page.getByRole('region', { name: 'Live status' })).toBeVisible()
      await capture(page, 'task-full-live-status', width)

      await page.goto('/w/demo/crates/crate-billing')
      await expect(page.getByRole('region', { name: 'Live status' })).toBeVisible()
      await capture(page, 'crate-live-status', width)
      // The member rows sit below the first screen, so the task list is
      // captured on its own.
      const tasks = page.locator('#crate-task-list')
      await tasks.scrollIntoViewIfNeeded()
      await tasks.screenshot({ path: join(outputDir as string, `crate-member-live-in-${width}.png`) })
    })

    test(`a status line that arrived on the second read at ${width} px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 })
      await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
      const state = await mockServer(page)
      for (const surface of surfaces) {
        surface.serve(state, false, true)
        await openSurface(page, surface, state)
        await expect(page.getByRole('region', { name: 'Live status' })).toHaveCount(0)
        surface.serve(state, true, false)
        await step(page, surface, state, 10_000, 1)
        const region = page.getByRole('region', { name: 'Live status' })
        await expect(surface.lines(page)).toHaveCount(1)
        await region.scrollIntoViewIfNeeded()
        await capture(page, `${surface.name}-second-read-live-status`, width)
      }
    })

    test(`GitHub App deployment reports at ${width} px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 })
      const state = await mockSettingsPage(page)
      state.githubApp = { ...settingsGitHubApp, deployment_reports: 'missing_permission_and_event' }
      await page.goto('/w/demo/settings')
      // The Settings page scrolls inside itself, so the open row is captured
      // as an element.
      let region = await openSettingsRow(page, 'GitHub App')
      await region.locator('[data-deployment-reports]').scrollIntoViewIfNeeded()
      await capture(page, 'github-app-deployment-reports-missing', width)
      await region.screenshot({ path: join(outputDir as string, `github-app-card-missing-${width}.png`) })
      state.githubApp = { ...settingsGitHubApp, deployment_reports: 'on' }
      await page.reload()
      region = await openSettingsRow(page, 'GitHub App')
      await region.locator('[data-deployment-reports]').scrollIntoViewIfNeeded()
      await region.screenshot({ path: join(outputDir as string, `github-app-card-on-${width}.png`) })
    })
  }
})
