import { expect, type Page, type Route, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'
import { installQueryClientProbe, withQueryClient } from './helpers/query-client'
import { activityFrame, controlTaskStreams, readyFrame } from './helpers/task-streams'

// Task-sheet read scheduling, Board navigation, stream refresh precedence, and
// the slow-read cache policy (component-web-task-surfaces, Sheet read
// scheduling, Board navigation, Live updates; component-web-dashboard,
// Server-state discipline; feature-operator-next-action NA-6). Every wait is a
// route response barrier, a controlled stream, or Playwright's clock.

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-task-activity-view', 'timeline')
    if (!localStorage.getItem('conveyor-workspace')) localStorage.setItem('conveyor-workspace', 'demo')
  })
})

const at = (minutes: number) => new Date(Date.UTC(2026, 6, 15, 12, minutes)).toISOString()

type Fixture = {
  id: string
  title: string
  created: string
  stage?: 'implement' | 'review'
  verify?: boolean
  blocking?: string[]
}

function taskItem({ id, title, created, stage = 'implement', verify = false, blocking }: Fixture) {
  return {
    task: {
      id,
      workspace: 'demo',
      title,
      state: 'running',
      next_stage: stage,
      repo: 'conveyor',
      branch: `conveyor/${id}`,
      created_at: created,
      policy_contract: verify ? { verify_stage: true } : undefined,
      blocking_task_ids: blocking,
      dependencies: blocking?.map((dependency) => ({ id: dependency, title: dependency, state: 'running' })),
    },
    jobs: [],
    events: [],
    work_orders: [],
    reviews: [],
    interventions: [],
    latest_stage: stage,
    last_event_at: at(50),
    needs_attention: false,
    pending_authority: false,
    next_action: {
      label: 'Agent working',
      actor: 'agent',
      kind: 'working',
      judgment: false,
      summary: 'The agent is working.',
      deadline: null,
    },
  }
}

const nextAction = (summary: string) => ({
  label: 'Agent working',
  actor: 'agent',
  kind: 'working',
  judgment: false,
  summary,
  deadline: null,
  choices: [],
  details: {},
})

type Server = {
  requests: string[]
  // Route hits, including a request the page later aborts or one still held.
  hits(match: (path: string, search: URLSearchParams) => boolean): number
  // Completed requests: React StrictMode's development rehearsal aborts and
  // repeats a read whose signal the query function consumed, and an aborted
  // request never finishes.
  count(match: (path: string, search: URLSearchParams) => boolean): number
  hold(path: string): void
  release(path: string): Promise<void>
  fail(path: string, times: number, status?: number): void
  revise(id: string, title: string): void
}

// One fake server for every /v1 read the shell, Board, and sheet send. Each
// request is recorded as path plus query before any barrier applies, and again
// once it finishes.
async function serve(page: Page, { board, others = [] }: { board: Fixture[][]; others?: Fixture[] }): Promise<Server> {
  const requests: string[] = []
  const finished: string[] = []
  page.on('requestfinished', (request) => {
    const url = new URL(request.url())
    if (url.pathname.startsWith('/v1/')) finished.push(`${request.method()} ${url.pathname}${url.search}`)
  })
  const held = new Map<string, Route[]>()
  const holding = new Set<string>()
  const failures = new Map<string, { times: number; status: number }>()
  const titles = new Map<string, string>()
  const all = [...board.flat(), ...others]
  const total = board.flat().length
  const respond = async (route: Route, path: string, search: URLSearchParams) => {
    if (path === '/v1/workspaces')
      return route.fulfill({
        json: [
          { id: 'demo', name: 'Demo' },
          { id: 'other', name: 'Other' },
        ],
      })
    if (path === '/v1/me') {
      return route.fulfill({
        json: callerIdentity(
          { id: 'usr_ada', email: 'ada@example.test', display_name: 'Ada', role: 'operator' },
          route.request(),
        ),
      })
    }
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    if (/^\/v1\/workspaces\/[^/]+\/members$/.test(path))
      return route.fulfill({
        json: [{ user_id: 'usr_ada', email: 'ada@example.test', display_name: 'Ada', role: 'operator' }],
      })
    if (path === '/v1/activity') {
      const limit = Number(search.get('limit') ?? '100')
      const offset = Number(search.get('offset') ?? '0')
      const page = board[Math.floor(offset / 100)] ?? []
      return route.fulfill({
        json: page.slice(0, limit).map(taskItem),
        headers: {
          'X-Conveyor-Total': String(total),
          'X-Conveyor-Limit': String(limit),
          'X-Conveyor-Offset': String(offset),
        },
      })
    }
    const detail = path.match(/^\/v1\/tasks\/([^/]+)\/activity$/)
    if (detail) {
      const fixture = all.find((candidate) => candidate.id === detail[1]) ?? all[0]
      const item = taskItem(fixture)
      item.task.title = titles.get(fixture.id) ?? fixture.title
      return route.fulfill({ json: item })
    }
    const action = path.match(/^\/v1\/tasks\/([^/]+)\/next-action$/)
    if (action) return route.fulfill({ json: nextAction(`Server statement for ${action[1]}.`) })
    if (/^\/v1\/tasks\/[^/]+\/verification$/.test(path))
      return route.fulfill({
        json: { head_sha: '', current_context_id: '', contexts: { items: [], next_cursor: '' }, overview: {} },
      })
    if (/^\/v1\/tasks\/[^/]+\/assignee$/.test(path)) return route.fulfill({ json: {} })
    if (path.endsWith('/events/stream')) return route.fulfill({ status: 204 })
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
    if (path === '/v1/sign-out') return route.fulfill({ status: 204 })
    return route.fulfill({ json: [] })
  }
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname
    requests.push(`${route.request().method()} ${path}${url.search}`)
    const failure = failures.get(path + url.search) ?? failures.get(path)
    if (failure && failure.times > 0) {
      failure.times--
      return route.fulfill({ status: failure.status, json: { error: 'unavailable' } })
    }
    if (holding.has(path)) {
      held.set(path, [...(held.get(path) ?? []), route])
      return
    }
    await respond(route, path, url.searchParams)
  })
  const tally = (list: string[], match: (path: string, search: URLSearchParams) => boolean) =>
    list.filter((request) => {
      const [method, target] = request.split(' ')
      const url = new URL(target, 'http://localhost')
      return method === 'GET' && match(url.pathname, url.searchParams)
    }).length
  return {
    requests,
    hits: (match) => tally(requests, match),
    count: (match) => tally(finished, match),
    hold: (path) => {
      holding.add(path)
    },
    release: async (path) => {
      holding.delete(path)
      for (const route of held.get(path) ?? []) {
        const url = new URL(route.request().url())
        // A navigation may already have discarded the held request.
        await respond(route, url.pathname, url.searchParams).catch(() => undefined)
      }
      held.delete(path)
    },
    fail: (path, times, status = 503) => {
      failures.set(path, { times, status })
    },
    revise: (id, title) => {
      titles.set(id, title)
    },
  }
}

// The independent reads the sheet body needs (component-web-task-surfaces,
// Sheet read scheduling).
const reads = {
  identity: (path: string, search: URLSearchParams) => path === '/v1/me' && search.get('workspace_id') === 'demo',
  members: (path: string) => path === '/v1/workspaces/demo/members',
  blueprints: (path: string) => path === '/v1/blueprints',
  nextAction: (taskId: string) => (path: string) => path === `/v1/tasks/${taskId}/next-action`,
  systemDesigns: (path: string, search: URLSearchParams) =>
    path === '/v1/system-designs' && !search.has('include_archived'),
  summary: (taskId: string) => (path: string) => path === `/v1/tasks/${taskId}/verification`,
  detail: (taskId: string) => (path: string) => path === `/v1/tasks/${taskId}/activity`,
  activity: (path: string) => path === '/v1/activity',
  unfilteredActivity: (path: string, search: URLSearchParams) =>
    path === '/v1/activity' && !search.has('updated_from') && !search.has('updated_to'),
}

// Freezes the installed clock a second ahead, with no stream timer pending.
async function pauseClock(page: Page) {
  const now = await page.evaluate(() => Date.now())
  await page.clock.pauseAt(now + 1_000)
}

const sheet = (page: Page) => page.getByRole('dialog', { name: 'Task detail' })
const nextActionRegion = (page: Page) => sheet(page).getByRole('region', { name: 'Next action' })

const sheetTask: Fixture = { id: 'sheet-verify', title: 'Verified sheet task', created: at(1), verify: true }
const plainTask: Fixture = { id: 'sheet-plain', title: 'Plain sheet task', created: at(2) }

function independentCounts(server: Server, taskId: string) {
  return {
    identity: server.count(reads.identity),
    members: server.count(reads.members),
    blueprints: server.count(reads.blueprints),
    nextAction: server.count(reads.nextAction(taskId)),
    systemDesigns: server.count(reads.systemDesigns),
    summary: server.count(reads.summary(taskId)),
  }
}

test('opening a Board sheet starts its independent reads before the detail returns and reuses them', async ({
  page,
}) => {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  const server = await serve(page, { board: [[sheetTask, plainTask]] })
  server.hold(`/v1/tasks/${sheetTask.id}/activity`)
  await page.goto(`/w/demo/tasks/${sheetTask.id}`)
  await expect(sheet(page)).toBeVisible()

  // With the detail still held, every detail-independent read is already out.
  await expect
    .poll(() => independentCounts(server, sheetTask.id))
    .toEqual({ identity: 1, members: 1, blueprints: 1, nextAction: 1, systemDesigns: 1, summary: 1 })
  expect(server.hits(reads.detail(sheetTask.id))).toBe(1)
  expect(server.count(reads.detail(sheetTask.id))).toBe(0)
  await expect(nextActionRegion(page)).toHaveCount(0)

  // Releasing the detail mounts the body, whose observers reuse each result.
  await server.release(`/v1/tasks/${sheetTask.id}/activity`)
  await expect(nextActionRegion(page)).toContainText(`Server statement for ${sheetTask.id}.`)
  expect(independentCounts(server, sheetTask.id)).toEqual({
    identity: 1,
    members: 1,
    blueprints: 1,
    nextAction: 1,
    systemDesigns: 1,
    summary: 1,
  })
  expect(server.count(reads.detail(sheetTask.id))).toBe(1)
  expect(errors).toEqual([])
})

test('a failed prefetch leaves its consumer to retry and never blocks the detail', async ({ page }) => {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  const server = await serve(page, { board: [[sheetTask, plainTask]] })
  server.fail(`/v1/tasks/${plainTask.id}/next-action`, 1)
  server.hold(`/v1/tasks/${plainTask.id}/activity`)
  await page.goto(`/w/demo/tasks/${plainTask.id}`)
  await expect.poll(() => server.count(reads.nextAction(plainTask.id))).toBe(1)

  await server.release(`/v1/tasks/${plainTask.id}/activity`)
  await expect(sheet(page).getByRole('heading', { name: plainTask.title })).toBeVisible()
  // The card's own observer retries the failed read on mount and renders it.
  await expect(nextActionRegion(page)).toContainText(`Server statement for ${plainTask.id}.`)
  expect(server.count(reads.nextAction(plainTask.id))).toBe(2)
  expect(errors).toEqual([])
})

test('a task without a verify stage reads the summary only on a cold open', async ({ page }) => {
  const server = await serve(page, { board: [[sheetTask, plainTask]] })
  await page.goto(`/w/demo/tasks/${plainTask.id}`)
  await expect(nextActionRegion(page)).toBeVisible()
  // The cold open read the summary beside the detail; the detail then showed
  // no verify stage, so nothing reads it again.
  expect(server.count(reads.summary(plainTask.id))).toBe(1)

  // Reopening with the detail cached sends no summary read at all.
  await sheet(page).getByRole('button', { name: 'Close panel' }).click()
  await expect(sheet(page)).toHaveCount(0)
  await page
    .getByRole('link', { name: new RegExp(plainTask.title) })
    .first()
    .click()
  await expect(nextActionRegion(page)).toBeVisible()
  expect(server.count(reads.summary(plainTask.id))).toBe(1)
})

// Board page 2 holds four tasks across two columns. In Board order the
// Implement column lists newest creation first, with the task ID breaking the
// tie, and the Review column follows.
const firstPage: Fixture[] = Array.from({ length: 100 }, (_, index) => ({
  id: `page-one-${String(index).padStart(3, '0')}`,
  title: `Page one task ${index}`,
  created: at(index % 40),
}))
const secondPage: Fixture[] = [
  { id: 'page-two-b', title: 'Second page tie B', created: at(5) },
  { id: 'page-two-review', title: 'Second page review', created: at(9), stage: 'review' },
  { id: 'page-two-a', title: 'Second page tie A', created: at(5) },
  { id: 'page-two-newest', title: 'Second page newest', created: at(7) },
]

test('Board sheet navigation follows the visible filtered page without an unfiltered read', async ({ page }) => {
  await page.clock.install({ time: new Date('2026-07-15T13:00:00Z') })
  const server = await serve(page, { board: [firstPage, secondPage] })
  await page.goto('/')
  await expect(page.getByText('1–100 of 104')).toBeVisible()
  await page.getByRole('button', { name: 'Next', exact: true }).click()
  await expect(page.getByText('101–104 of 104')).toBeVisible()

  await page.getByRole('link', { name: /Second page newest/ }).click()
  const header = sheet(page).locator('header').first()
  await expect(header).toContainText('Second page newest')
  await expect(header.getByRole('note')).toHaveText('The previous task is on an earlier Board page.')
  // A clock-driven Board pass refreshes only the Board's own filtered page.
  await page.clock.runFor(15_000)
  await expect.poll(() => server.count(reads.activity)).toBeGreaterThanOrEqual(3)
  expect(server.count(reads.unfilteredActivity)).toBe(0)

  const order = ['Second page newest', 'Second page tie A', 'Second page tie B', 'Second page review']
  for (const title of order.slice(1)) {
    await header.getByRole('link', { name: 'Next task' }).click()
    await expect(header).toContainText(title)
  }
  // The last task of the last page has no later neighbour and no note.
  await expect(header.getByRole('button', { name: 'Next task' })).toBeDisabled()
  await expect(header.getByRole('note')).toHaveCount(0)
  for (const title of order.slice(0, -1).reverse()) {
    await header.getByRole('link', { name: 'Previous task' }).click()
    await expect(header).toContainText(title)
  }
  await expect(header.getByRole('button', { name: 'Previous task' })).toBeDisabled()
  expect(server.count(reads.unfilteredActivity)).toBe(0)
})

test('Board sheet notes a later page and a task outside the page; the full route has no neighbours', async ({
  page,
}) => {
  const outside: Fixture = { id: 'outside-window', title: 'Outside the window', created: at(3) }
  const server = await serve(page, { board: [firstPage, secondPage], others: [outside] })
  // The newest task on page one sorts first; the oldest one last.
  await page.goto('/')
  await expect(page.getByText('1–100 of 104')).toBeVisible()
  const last = firstPage.filter((task) => task.created === at(0)).sort((a, b) => b.id.localeCompare(a.id))[0]
  await page.getByRole('link', { name: new RegExp(last.title) }).click()
  const header = sheet(page).locator('header').first()
  await expect(header).toContainText(last.title)
  await expect(header.getByRole('note')).toHaveText('The next task is on a later Board page.')
  await expect(header.getByRole('button', { name: 'Next task' })).toBeDisabled()

  await page.goto(`/w/demo/tasks/${outside.id}`)
  await expect(sheet(page).locator('header').first()).toContainText(outside.title)
  await expect(sheet(page).locator('header').first().getByRole('note')).toHaveText(
    'This task is outside the loaded Board window.',
  )

  await page.goto(`/w/demo/tasks/${secondPage[0].id}/full`)
  await expect(page.getByRole('heading', { name: secondPage[0].title, level: 1 })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Previous task' })).toBeDisabled()
  await expect(page.getByRole('button', { name: 'Next task' })).toBeDisabled()
  await expect(page.getByRole('note')).toHaveCount(0)
  expect(server.count(reads.unfilteredActivity)).toBe(0)
})

test.describe('task stream refresh', () => {
  test('a ready frame skips fresh and in-flight reads; a stale ready and any activity frame refresh', async ({
    page,
  }) => {
    const streams = await controlTaskStreams(page)
    await installQueryClientProbe(page)
    await page.clock.install({ time: new Date('2026-07-15T13:00:00Z') })
    const server = await serve(page, { board: [[sheetTask, plainTask]] })
    const aborted: string[] = []
    page.on('requestfailed', (request) => aborted.push(new URL(request.url()).pathname))
    const detailPath = `/v1/tasks/${sheetTask.id}/activity`
    server.hold(detailPath)
    await page.goto(`/w/demo/tasks/${sheetTask.id}`)
    await expect.poll(() => streams.countFor(sheetTask.id)).toBeGreaterThan(0)
    await expect.poll(() => server.hits(reads.detail(sheetTask.id))).toBe(1)

    // A ready frame while the first detail read is held neither aborts nor
    // repeats it.
    await streams.open()
    await pauseClock(page)
    await streams.send(readyFrame)
    await page.clock.runFor(250)
    await page.clock.runFor(1_000)
    expect(server.hits(reads.detail(sheetTask.id))).toBe(1)
    await server.release(detailPath)
    await page.clock.resume()
    await expect(nextActionRegion(page)).toBeVisible()
    await pauseClock(page)
    expect(server.hits(reads.detail(sheetTask.id))).toBe(1)
    expect(aborted.filter((path) => path === detailPath)).toEqual([])

    // A ready frame inside the detail's stale window leaves fresh entries alone.
    const detail = () => server.count(reads.detail(sheetTask.id))
    const nextActions = () => server.count(reads.nextAction(sheetTask.id))
    const feed = server.count(reads.activity)
    await streams.send(readyFrame)
    await page.clock.runFor(250)
    await page.clock.runFor(1_000)
    expect(detail()).toBe(1)
    expect(nextActions()).toBe(1)
    expect(server.count(reads.activity)).toBe(feed)

    // After the stale window a ready frame refreshes the detail and the Board
    // page; the detail update refreshes the next action once.
    await page.clock.runFor(5_000)
    const feedBefore = server.count(reads.activity)
    await streams.send(readyFrame)
    await page.clock.runFor(250)
    await expect.poll(detail).toBe(2)
    await expect.poll(() => server.count(reads.activity)).toBe(feedBefore + 1)
    await expect.poll(nextActions).toBe(2)

    // An activity frame refreshes despite freshness, with one next-action read
    // for the batch, and ready plus activity in one batch counts as activity.
    for (const frames of [[activityFrame], [readyFrame, activityFrame]]) {
      const before = { detail: detail(), nextAction: nextActions(), feed: server.count(reads.activity) }
      for (const frame of frames) await streams.send(frame)
      await page.clock.runFor(250)
      await expect.poll(detail).toBe(before.detail + 1)
      await expect.poll(() => server.count(reads.activity)).toBe(before.feed + 1)
      await expect.poll(nextActions).toBe(before.nextAction + 1)
      await page.clock.runFor(1_000)
      expect(nextActions()).toBe(before.nextAction + 1)
      expect(detail()).toBe(before.detail + 1)
    }

    // Invalidation stays in the captured workspace and its governed families.
    const untouched = [
      ['activity', 'other', { filter: {}, limit: 100, offset: 0 }],
      ['task', 'other', sheetTask.id],
      ['task-history', 'demo', sheetTask.id, 'technical', ''],
      ['task-audit', 'demo', sheetTask.id, 'event', '1'],
      ['verification-page', 'demo', sheetTask.id, 'seeded-context', 'evidence', 0],
    ]
    await withQueryClient(
      page,
      (client, keys) => {
        for (const key of keys) client.setQueryData(key, { seeded: true })
      },
      untouched,
    )
    await streams.send(activityFrame)
    await page.clock.runFor(250)
    expect(
      await withQueryClient(
        page,
        (client, keys) => keys.map((key) => client.getQueryState(key)?.isInvalidated ?? null),
        untouched,
      ),
    ).toEqual([false, false, false, false, false])
    expect(server.count(reads.summary(sheetTask.id))).toBe(1)
  })

  test('next action still refreshes once after a dependency poll or a mutation changes the detail', async ({
    page,
  }) => {
    await controlTaskStreams(page)
    await page.clock.install({ time: new Date('2026-07-15T13:00:00Z') })
    const blocked: Fixture = { id: 'blocked-task', title: 'Blocked task', created: at(4), blocking: ['plain'] }
    const server = await serve(page, { board: [[blocked, plainTask]] })
    await page.goto(`/w/demo/tasks/${blocked.id}`)
    await expect(nextActionRegion(page)).toBeVisible()
    const detail = () => server.count(reads.detail(blocked.id))
    const nextActions = () => server.count(reads.nextAction(blocked.id))
    expect(nextActions()).toBe(1)

    // The 15 s blocker poll changes the detail without a stream frame.
    server.revise(blocked.id, 'Blocked task, polled')
    await page.clock.runFor(15_000)
    await expect.poll(detail).toBe(2)
    await expect(sheet(page).locator('header').first()).toContainText('Blocked task, polled')
    await expect.poll(nextActions).toBe(2)

    // Assignment refreshes the detail through its mutation.
    server.revise(blocked.id, 'Blocked task, assigned')
    await sheet(page).getByRole('button', { name: 'Task actions' }).click()
    await page.getByRole('menuitem', { name: 'Assign', exact: true }).click()
    await page.getByRole('listbox', { name: 'Workspace members' }).getByRole('option', { name: 'Ada' }).click()
    await expect(sheet(page).locator('header').first()).toContainText('Blocked task, assigned')
    await expect.poll(nextActions).toBe(3)
    await page.clock.runFor(1_000)
    expect(nextActions()).toBe(3)

    // Window focus sends no detail read; the blocker poll continues.
    const beforeFocus = detail()
    await page.clock.runFor(6_000)
    await page.evaluate(() => {
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await page.clock.runFor(1_000)
    expect(detail()).toBe(beforeFocus)
    await page.clock.runFor(8_000)
    await expect.poll(detail).toBe(beforeFocus + 1)
  })
})

test.describe('slow reads', () => {
  test('repeated sheet opens reuse slow reads and only Blueprints routes poll blueprints', async ({ page }) => {
    await page.clock.install({ time: new Date('2026-07-15T13:00:00Z') })
    const server = await serve(page, { board: [[sheetTask, plainTask]] })
    await page.goto(`/w/demo/tasks/${sheetTask.id}`)
    await expect(nextActionRegion(page)).toBeVisible()
    const slow = () => ({
      identity: server.count(reads.identity),
      members: server.count(reads.members),
      blueprints: server.count(reads.blueprints),
      systemDesigns: server.count(reads.systemDesigns),
    })
    const first = slow()
    expect(first).toEqual({ identity: 1, members: 1, blueprints: 1, systemDesigns: 1 })

    // A sheet left open sends no blueprint poll.
    await page.clock.runFor(31_000)
    expect(server.count(reads.blueprints)).toBe(1)

    // Opening other sheets inside five minutes reuses every slow read.
    await sheet(page).getByRole('button', { name: 'Close panel' }).click()
    await page.clock.runFor(60_000)
    await page
      .getByRole('link', { name: new RegExp(plainTask.title) })
      .first()
      .click()
    await expect(nextActionRegion(page)).toContainText(`Server statement for ${plainTask.id}.`)
    await page.clock.runFor(60_000)
    await sheet(page).getByRole('button', { name: 'Close panel' }).click()
    await page
      .getByRole('link', { name: new RegExp(sheetTask.title) })
      .first()
      .click()
    await expect(nextActionRegion(page)).toBeVisible()
    expect(slow()).toEqual(first)

    // The Blueprints route polls every 15 s.
    await page.goto('/w/demo/blueprints')
    await expect.poll(() => server.count(reads.blueprints)).toBeGreaterThan(0)
    const landed = server.count(reads.blueprints)
    await page.clock.runFor(15_000)
    await expect.poll(() => server.count(reads.blueprints)).toBe(landed + 1)
    await page.clock.runFor(15_000)
    await expect.poll(() => server.count(reads.blueprints)).toBe(landed + 2)
  })

  test('a failed identity read denies, and saving, switching, and signing out still refresh slow reads', async ({
    page,
  }) => {
    const server = await serve(page, { board: [[sheetTask, plainTask]] })
    // A failed scoped identity read denies every capability: no New task.
    server.fail('/v1/me?workspace_id=demo', 1)
    await page.goto('/')
    await expect(page.getByRole('link', { name: new RegExp(sheetTask.title) })).toBeVisible()
    await expect.poll(() => server.count(reads.identity)).toBeGreaterThan(0)
    await expect(page.getByRole('button', { name: 'New task' })).toHaveCount(0)

    // Saving the profile refreshes identity at once and marks members stale,
    // so the Board's cards read members again on return inside five minutes.
    await page.getByRole('link', { name: 'Settings' }).first().click()
    await expect(page.getByLabel('Display name')).toBeVisible()
    const beforeSave = { identity: server.count(reads.identity), members: server.count(reads.members) }
    await page.getByLabel('Display name').fill('Ada Lovelace')
    await page.getByRole('button', { name: 'Save profile' }).click()
    await expect.poll(() => server.count(reads.identity)).toBeGreaterThan(beforeSave.identity)
    await page.getByRole('link', { name: 'Board', exact: true }).first().click()
    await expect(page.getByRole('link', { name: new RegExp(sheetTask.title) })).toBeVisible()
    await expect.poll(() => server.count(reads.members)).toBeGreaterThan(beforeSave.members)

    // A workspace switch drops every slow read; the other workspace reads its own.
    await page.goto('/')
    await page.getByRole('button', { name: 'Switch to Other' }).click()
    await expect
      .poll(() => server.count((path, search) => path === '/v1/me' && search.get('workspace_id') === 'other'))
      .toBeGreaterThan(0)

    // Sign-out clears the cache, so the next session reads identity again.
    await page.getByRole('button', { name: 'Switch to Demo' }).click()
    await expect(page.getByRole('button', { name: 'New task' })).toBeVisible()
    const beforeSignOut = server.count(reads.identity)
    await page.getByRole('button', { name: 'Sign out' }).click()
    await expect(page).toHaveURL(/\/sign-in/)
    await page.evaluate(() => localStorage.setItem('conveyor-workspace', 'demo'))
    await page.goto('/')
    await expect.poll(() => server.count(reads.identity)).toBeGreaterThan(beforeSignOut)
  })
})
