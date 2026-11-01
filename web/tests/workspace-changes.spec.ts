import { expect, type Page, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'
import { controlWorkspaceChanges, type WorkspaceChangeControl } from './helpers/workspace-changes'

// The workspace change stream drives the shell's query refresh instead of
// timers (component-web-dashboard, Workspace change stream;
// feature-quiet-idle-runtime, "Pushed task streams and `await_review`";
// req-quiet-idle-runtime AC-1.1). Every wait is Playwright's clock or the fake
// EventSource; no assertion depends on wall time.

const at = (minutes: number) => new Date(Date.UTC(2026, 10, 1, 12, minutes)).toISOString()

type Fixture = { id: string; title: string; blocking?: string[] }

const blocked: Fixture = { id: 'task-blocked', title: 'Blocked task', blocking: ['task-dependency'] }
const dependency: Fixture = { id: 'task-dependency', title: 'Dependency task' }
const unrelated: Fixture = { id: 'task-unrelated', title: 'Unrelated task' }
const fixtures = [blocked, dependency, unrelated]

function taskItem({ id, title, blocking }: Fixture, titles: Map<string, string>) {
  return {
    task: {
      id,
      workspace: 'demo',
      title: titles.get(id) ?? title,
      state: 'running',
      next_stage: 'implement',
      repo: 'conveyor',
      branch: `conveyor/${id}`,
      created_at: at(1),
      blocking_task_ids: blocking,
      dependencies: blocking?.map((blocker) => ({ id: blocker, title: blocker, state: 'running' })),
    },
    jobs: [],
    events: [],
    work_orders: [],
    reviews: [],
    interventions: [],
    latest_stage: 'implement',
    last_event_at: at(30),
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

// The reads this spec watches, by family.
const families = {
  activity: (path: string) => path === '/v1/activity',
  proposals: (path: string) => path === '/v1/pending-proposals',
  workspace: (path: string) => path === '/v1/workspace',
  inbox: (path: string) => path === '/v1/next-actions',
  map: (path: string) => path === '/v1/task-map',
  detail: (taskId: string) => (path: string) => path === `/v1/tasks/${taskId}/activity`,
  history: (path: string) => /^\/v1\/tasks\/[^/]+\/history$/.test(path),
}

type Server = {
  hits(match: (path: string) => boolean): number
  revise(id: string, title: string): void
}

// One fake server for the shell, Board, sheet, inbox, and map reads. Each GET
// is counted when the route receives it.
async function serve(page: Page): Promise<Server> {
  const requests: string[] = []
  const titles = new Map<string, string>()
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname
    if (route.request().method() === 'GET') requests.push(path)
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity(
          { id: 'usr_ada', email: 'ada@example.test', display_name: 'Ada', role: 'operator' },
          route.request(),
        ),
      })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    if (path === '/v1/activity') {
      const limit = url.searchParams.get('limit') ?? '100'
      return route.fulfill({
        json: fixtures.map((fixture) => taskItem(fixture, titles)),
        headers: { 'X-Conveyor-Total': String(fixtures.length), 'X-Conveyor-Limit': limit, 'X-Conveyor-Offset': '0' },
      })
    }
    if (path === '/v1/next-actions')
      return route.fulfill({ json: { items: [], acts: [] }, headers: { 'X-Conveyor-Total': '0' } })
    if (path === '/v1/task-map') return route.fulfill({ json: { tasks: [], requirements: [] } })
    const detail = path.match(/^\/v1\/tasks\/([^/]+)\/activity$/)
    if (detail) {
      const fixture = fixtures.find((candidate) => candidate.id === detail[1]) ?? fixtures[0]
      return route.fulfill({ json: taskItem(fixture, titles) })
    }
    if (/^\/v1\/tasks\/[^/]+\/next-action$/.test(path))
      return route.fulfill({
        json: {
          label: 'Agent working',
          actor: 'agent',
          kind: 'working',
          judgment: false,
          summary: 'The agent is working.',
          deadline: null,
          choices: [],
          details: {},
        },
      })
    if (/^\/v1\/tasks\/[^/]+\/verification$/.test(path))
      return route.fulfill({
        json: { head_sha: '', current_context_id: '', contexts: { items: [], next_cursor: '' }, overview: {} },
      })
    // The task stream finds no stream here and backs off on its own clock.
    if (path.endsWith('/events/stream')) return route.fulfill({ status: 204 })
    return route.fulfill({ json: [] })
  })
  return {
    hits: (match) => requests.filter(match).length,
    revise: (id, title) => {
      titles.set(id, title)
    },
  }
}

// Replaces the tab's visibility so a spec can hide and show it.
async function controlVisibility(page: Page) {
  await page.addInitScript(() => {
    let visible = true
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => (visible ? 'visible' : 'hidden'),
    })
    ;(window as unknown as { setTabVisible: (next: boolean) => void }).setTabVisible = (next) => {
      visible = next
      document.dispatchEvent(new Event('visibilitychange'))
    }
  })
  return (visible: boolean) =>
    page.evaluate(
      (next) => (window as unknown as { setTabVisible: (next: boolean) => void }).setTabVisible(next),
      visible,
    )
}

function counter(server: Server, names: Record<string, (path: string) => boolean>) {
  return () => Object.fromEntries(Object.entries(names).map(([name, match]) => [name, server.hits(match)]))
}

function plus(base: Record<string, number>, delta: Record<string, number>) {
  return Object.fromEntries(Object.entries(base).map(([name, count]) => [name, count + (delta[name] ?? 0)]))
}

// Freezes the installed clock a second ahead, so only `runFor` moves time.
async function pauseClock(page: Page) {
  const now = await page.evaluate(() => Date.now())
  await page.clock.pauseAt(now + 1_000)
}

// Freezes the clock, sends `ready`, and lets its connect refresh land, so
// later counts start from a settled page.
async function connect(page: Page, changes: WorkspaceChangeControl, settled: () => Record<string, number>) {
  await pauseClock(page)
  const before = settled()
  await changes.ready()
  await page.clock.runFor(250)
  await expect.poll(settled).not.toEqual(before)
  await page.clock.runFor(1_000)
  return settled()
}

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    localStorage.setItem('conveyor-task-activity-view', 'timeline')
  })
})

test('an activity frame refreshes the Board, sidebar, and named task detail once', async ({ page }) => {
  const changes = await controlWorkspaceChanges(page)
  await page.clock.install({ time: new Date(at(40)) })
  const server = await serve(page)
  await page.goto(`/w/demo/tasks/${blocked.id}`)
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toContainText(blocked.title)
  await expect.poll(() => server.hits(families.activity)).toBeGreaterThan(0)
  expect(await changes.connections()).toContainEqual({ url: '/v1/workspaces/demo/changes', closed: false })
  expect(await changes.open()).toBe(1)

  const counts = counter(server, {
    activity: families.activity,
    proposals: families.proposals,
    workspace: families.workspace,
    detail: families.detail(blocked.id),
    history: families.history,
  })
  const settled = await connect(page, changes, counts)

  // A frame naming the open task refreshes each family once, after 250 ms.
  await changes.activity(blocked.id)
  await page.clock.runFor(249)
  expect(counts()).toEqual(settled)
  await page.clock.runFor(1)
  const named = plus(settled, { activity: 1, proposals: 1, workspace: 1, detail: 1 })
  await expect.poll(counts).toEqual(named)
  await page.clock.runFor(1_000)
  expect(counts()).toEqual(named)

  // Frames inside the delay join one refresh.
  await changes.activity(blocked.id)
  await changes.activity(unrelated.id)
  await page.clock.runFor(250)
  const joined = plus(named, { activity: 1, proposals: 1, workspace: 1, detail: 1 })
  await expect.poll(counts).toEqual(joined)
  await page.clock.runFor(1_000)
  expect(counts()).toEqual(joined)

  // A frame naming a blocking task refreshes its dependent's detail.
  server.revise(blocked.id, 'Blocked task, unblocked')
  await changes.activity(dependency.id)
  await page.clock.runFor(250)
  const blocker = plus(joined, { activity: 1, proposals: 1, workspace: 1, detail: 1 })
  await expect.poll(counts).toEqual(blocker)
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toContainText('Blocked task, unblocked')

  // A frame naming an unrelated task leaves the open detail alone.
  await changes.activity(unrelated.id)
  await page.clock.runFor(250)
  const other = plus(blocker, { activity: 1, proposals: 1, workspace: 1 })
  await expect.poll(counts).toEqual(other)
  await page.clock.runFor(1_000)
  expect(counts()).toEqual(other)

  // A folded notice names no task, so every open detail refreshes.
  await changes.activity()
  await page.clock.runFor(250)
  const folded = plus(other, { activity: 1, proposals: 1, workspace: 1, detail: 1 })
  await expect.poll(counts).toEqual(folded)
  await page.clock.runFor(1_000)
  expect(counts()).toEqual(folded)
  expect(counts().history).toBe(0)
})

test('an activity frame refreshes the inbox and the task map on their pages', async ({ page }) => {
  const changes = await controlWorkspaceChanges(page)
  await page.clock.install({ time: new Date(at(40)) })
  const server = await serve(page)

  await page.goto('/w/demo/inbox')
  await expect.poll(() => server.hits(families.inbox)).toBeGreaterThan(0)
  const inbox = counter(server, { inbox: families.inbox })
  const inboxSettled = await connect(page, changes, inbox)
  await changes.activity(unrelated.id)
  await page.clock.runFor(250)
  await expect.poll(inbox).toEqual(plus(inboxSettled, { inbox: 1 }))
  await page.clock.runFor(60_000)
  expect(inbox()).toEqual(plus(inboxSettled, { inbox: 1 }))

  // The connect helper paused the clock; the map's lazy chunk loads on a
  // flowing one.
  await page.clock.resume()
  await page.goto('/w/demo/?view=map')
  await expect.poll(() => server.hits(families.map)).toBeGreaterThan(0)
  const map = counter(server, { map: families.map })
  const mapSettled = await connect(page, changes, map)
  // No timer rereads the map.
  await page.clock.runFor(120_000)
  expect(map()).toEqual(mapSettled)
  await changes.activity(unrelated.id)
  await page.clock.runFor(250)
  await expect.poll(map).toEqual(plus(mapSettled, { map: 1 }))
  await page.clock.runFor(1_000)
  expect(map()).toEqual(plus(mapSettled, { map: 1 }))
})

test('a hidden tab holds no stream and runs no interval refetch', async ({ page }) => {
  const setVisible = await controlVisibility(page)
  const changes = await controlWorkspaceChanges(page)
  await page.clock.install({ time: new Date(at(40)) })
  const server = await serve(page)
  await page.goto('/w/demo/')
  await expect(page.getByRole('link', { name: new RegExp(blocked.title) })).toBeVisible()
  const counts = counter(server, {
    activity: families.activity,
    proposals: families.proposals,
    workspace: families.workspace,
  })
  const settled = await connect(page, changes, counts)
  const opened = (await changes.connections()).length

  await setVisible(false)
  expect(await changes.open()).toBe(0)
  // Five minutes hidden: no stream opens and no family reads.
  await page.clock.runFor(300_000)
  expect(await changes.open()).toBe(0)
  expect((await changes.connections()).length).toBe(opened)
  expect(counts()).toEqual(settled)

  // Visible again: one stream opens at once, and its `ready` refreshes once.
  await setVisible(true)
  expect(await changes.open()).toBe(1)
  expect((await changes.connections()).length).toBe(opened + 1)
  await page.clock.runFor(60_000)
  expect(counts()).toEqual(settled)
  await changes.ready()
  await page.clock.runFor(250)
  const shown = plus(settled, { activity: 1, proposals: 1, workspace: 1 })
  await expect.poll(counts).toEqual(shown)
  await page.clock.runFor(1_000)
  expect(counts()).toEqual(shown)
})

test('a dropped stream reconnects with backoff capped at 30 seconds and refetches once', async ({ page }) => {
  const changes = await controlWorkspaceChanges(page)
  await page.clock.install({ time: new Date(at(40)) })
  const server = await serve(page)
  await page.goto(`/w/demo/tasks/${blocked.id}`)
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toContainText(blocked.title)
  const counts = counter(server, {
    activity: families.activity,
    proposals: families.proposals,
    workspace: families.workspace,
    detail: families.detail(blocked.id),
  })
  const settled = await connect(page, changes, counts)
  const total = async () => (await changes.connections()).length

  // A drop after `ready` reconnects after one second. Each later connection
  // that fails before `ready` doubles the wait up to the 30-second cap.
  for (const wait of [1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000]) {
    const before = await total()
    await changes.error()
    expect(await changes.open()).toBe(0)
    await page.clock.runFor(wait - 1)
    expect(await total()).toBe(before)
    await page.clock.runFor(1)
    expect(await total()).toBe(before + 1)
    expect(await changes.open()).toBe(1)
  }
  // No connection reached `ready`, so nothing refreshed.
  expect(counts()).toEqual(settled)

  // `online` during a wait reconnects at once.
  const offline = await total()
  await changes.error()
  await page.evaluate(() => window.dispatchEvent(new Event('online')))
  expect(await total()).toBe(offline + 1)

  // The reconnected stream's `ready` refreshes each family exactly once.
  await changes.ready()
  await page.clock.runFor(250)
  const reconnected = plus(settled, { activity: 1, proposals: 1, workspace: 1, detail: 1 })
  await expect.poll(counts).toEqual(reconnected)
  await page.clock.runFor(5_000)
  expect(counts()).toEqual(reconnected)

  // `ready` reset the backoff: the next drop reconnects after one second.
  const after = await total()
  await changes.error()
  await page.clock.runFor(1_000)
  expect(await total()).toBe(after + 1)
})

test('a change between the first read and the stream subscription is not missed', async ({ page }) => {
  const changes = await controlWorkspaceChanges(page)
  await page.clock.install({ time: new Date(at(40)) })
  const server = await serve(page)
  await page.goto(`/w/demo/tasks/${blocked.id}`)
  const sheet = page.getByRole('dialog', { name: 'Task detail' })
  await expect(sheet).toContainText(blocked.title)
  await expect(page.getByRole('link', { name: new RegExp(unrelated.title) })).toBeVisible()

  // The server records a change before the stream has subscribed, so no
  // frame announces it, and no timer finds it.
  server.revise(unrelated.id, 'Unrelated task, changed before subscribe')
  server.revise(blocked.id, 'Blocked task, changed before subscribe')
  await page.clock.runFor(60_000)
  await expect(page.getByRole('link', { name: /Unrelated task, changed before subscribe/ })).toHaveCount(0)
  await expect(sheet).not.toContainText('changed before subscribe')

  // The stream's `ready` refresh shows it on the Board and in the sheet.
  await changes.ready()
  await page.clock.runFor(250)
  await expect(page.getByRole('link', { name: /Unrelated task, changed before subscribe/ })).toBeVisible()
  await expect(sheet).toContainText('Blocked task, changed before subscribe')
})
