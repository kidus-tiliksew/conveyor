import { type BrowserContext, expect, type Page, type Request, type Route, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'

// Workspace-scoped dashboard addresses (req-workspace-addresses REQ-1;
// component-web-dashboard, Route tree and Shell and workspace scoping). Two
// mocked workspaces, Alpha and Bravo, each hold one task. A third workspace,
// Private, exists on the server but the caller holds no binding there, so the
// mock answers it exactly as it answers a workspace that does not exist.

const alpha = { id: 'alpha', name: 'Alpha' }
const bravo = { id: 'bravo', name: 'Bravo' }
const recent = new Date(Date.now() - 60 * 60 * 1000).toISOString()

const tasks: Record<string, { id: string; title: string }> = {
  alpha: { id: 'alpha-1', title: 'Alpha only change' },
  bravo: { id: 'bravo-1', title: 'Bravo only change' },
  private: { id: 'private-1', title: 'Private change' },
}

function task(workspace: string) {
  const { id, title } = tasks[workspace]
  return {
    id,
    workspace,
    source: 'operator',
    title,
    repo: 'conveyor',
    branch: `conveyor/${id}`,
    state: 'running',
    next_stage: 'implement',
    created_at: recent,
  }
}

function detail(workspace: string) {
  return {
    task: task(workspace),
    jobs: [],
    events: [],
    work_orders: [],
    reviews: [],
    interventions: [],
    latest_stage: 'implement',
    last_event_at: recent,
    needs_attention: false,
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

type Server = {
  memberships: Array<{ id: string; name: string }>
  // Every /v1 request, with the workspace it names ('' when it names none).
  requests: Array<{ method: string; path: string; workspace: string }>
}

async function mockServer(page: Page, server: Server) {
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const workspace = url.searchParams.get('workspace_id') ?? request.headers()['x-workspace-id'] ?? ''
    server.requests.push({ method: request.method(), path: url.pathname, workspace })
    const member = server.memberships.some((item) => item.id === workspace)
    // Membership-scoped visibility: a workspace without a binding answers
    // not found, exactly like one that does not exist (DEC-19).
    if (workspace && !member) return route.fulfill({ status: 404, json: { error: 'workspace_not_found' } })
    if (url.pathname === '/v1/workspaces') return route.fulfill({ json: server.memberships })
    if (url.pathname === '/v1/me') {
      return route.fulfill({
        json: callerIdentity({ id: 'usr_operator', email: 'operator@example.test', role: 'operator' }, request),
      })
    }
    if (url.pathname === '/v1/workspace') {
      return route.fulfill({ json: { workspace, repos: [{ name: 'conveyor', base: 'main' }] } })
    }
    if (url.pathname === '/v1/pending-proposals') {
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    }
    if (url.pathname === '/v1/activity') {
      const items = [{ ...detail(workspace), task: task(workspace) }]
      return route.fulfill({
        headers: { 'X-Conveyor-Total': '1', 'X-Conveyor-Limit': '100', 'X-Conveyor-Offset': '0' },
        json: items,
      })
    }
    if (url.pathname === '/v1/workers') {
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
    }
    if (url.pathname === '/v1/tasks' && request.method() === 'POST') {
      return route.fulfill({ status: 201, json: { ...task(workspace), id: `${workspace}-created` } })
    }
    const taskRead = url.pathname.match(/^\/v1\/tasks\/([^/]+)(\/activity)?$/)
    if (taskRead) {
      // A task answers only in the workspace that holds it.
      if (tasks[workspace]?.id !== taskRead[1]) return route.fulfill({ status: 404, json: { error: 'not_found' } })
      return route.fulfill({ json: taskRead[2] ? detail(workspace) : task(workspace) })
    }
    if (url.pathname.endsWith('/next-action')) {
      return route.fulfill({ json: { ...detail(workspace).next_action, choices: [], details: {} } })
    }
    if (url.pathname.endsWith('/verification')) {
      return route.fulfill({
        json: { head_sha: '', current_context_id: '', contexts: { items: [], next_cursor: '' }, overview: {} },
      })
    }
    if (url.pathname.endsWith('/events/stream')) return route.fulfill({ status: 204 })
    if (/^\/v1\/workspaces\/[^/]+\/members$/.test(url.pathname)) return route.fulfill({ json: [] })
    if (url.pathname === '/v1/next-actions') return route.fulfill({ json: { items: [], total: 0 } })
    return route.fulfill({ json: [] })
  })
}

async function open(context: BrowserContext | Page, stored?: string) {
  const server: Server = { memberships: [alpha, bravo], requests: [] }
  const page = 'newPage' in context ? await context.newPage() : context
  await page.addInitScript((selected) => {
    localStorage.setItem('conveyor-theme', 'dark')
    if (selected && !sessionStorage.getItem('seeded')) {
      localStorage.setItem('conveyor-workspace', selected)
      sessionStorage.setItem('seeded', 'yes')
    }
  }, stored)
  await mockServer(page, server)
  return { page, server }
}

const stored = (page: Page) => page.evaluate(() => localStorage.getItem('conveyor-workspace'))
const scopedTo = (server: Server, workspace: string) =>
  server.requests.filter((request) => request.workspace === workspace)

test('every sidebar entry and Board card link names the page workspace (AC-1.1, AC-1.2)', async ({ page }) => {
  await open(page, 'alpha')
  await page.goto('/w/bravo/')
  await expect(page.getByText('Bravo only change', { exact: true })).toBeVisible()
  await expect(page).toHaveURL(/\/w\/bravo\/$/)

  const nav = page.getByRole('navigation', { name: 'Primary' })
  const hrefs = await nav.getByRole('link').evaluateAll((links) => links.map((link) => link.getAttribute('href')))
  expect(hrefs).toEqual([
    '/w/bravo/',
    '/w/bravo/inbox',
    '/w/bravo/workspace',
    '/w/bravo/requirements',
    '/w/bravo/system-design',
    '/w/bravo/decisions',
    '/w/bravo/monitor',
    '/w/bravo/settings',
  ])
  await expect(nav.getByRole('link', { name: 'Board' })).toHaveAttribute('aria-current', 'page')
  await expect(page.getByRole('link', { name: /Bravo only change/ })).toHaveAttribute('href', '/w/bravo/tasks/bravo-1')

  // Every page keeps its own path below the prefix.
  await nav.getByRole('link', { name: 'Settings' }).click()
  await expect(page).toHaveURL(/\/w\/bravo\/settings$/)
  await expect(nav.getByRole('link', { name: 'Settings' })).toHaveAttribute('aria-current', 'page')
  await expect(nav.getByRole('link', { name: 'Board' })).not.toHaveAttribute('aria-current', 'page')
})

test("an address opens its workspace's task whatever the browser stored, then stores it (AC-1.3)", async ({ page }) => {
  const { server } = await open(page, 'alpha')
  await page.goto('/w/bravo/tasks/bravo-1')
  const sheet = page.getByRole('dialog', { name: 'Task detail' })
  await expect(sheet.getByRole('heading', { name: 'Bravo only change' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Switch to Bravo' })).toHaveAttribute('aria-current', 'true')
  await expect.poll(() => stored(page)).toBe('bravo')
  // No read named the workspace the browser had stored.
  expect(scopedTo(server, 'alpha')).toEqual([])
})

test('a workspace without a binding and an unknown one show the same not-found page (AC-1.4)', async ({ page }) => {
  const { server } = await open(page, 'alpha')
  const pages: string[] = []
  for (const address of ['/w/unknown/', '/w/private/', '/w/private/tasks/private-1']) {
    await page.goto(address)
    const main = page.getByRole('main')
    await expect(main.getByRole('heading', { name: 'Page not found' })).toBeVisible()
    pages.push(await main.innerText())
    // The workspace sidebar stays away; the rail still offers the memberships.
    await expect(page.getByRole('navigation', { name: 'Primary' })).toHaveCount(0)
    await expect(page.getByRole('button', { name: 'Switch to Alpha' })).not.toHaveAttribute('aria-current', 'true')
  }
  expect(new Set(pages).size).toBe(1)
  expect(scopedTo(server, 'unknown')).toEqual([])
  expect(scopedTo(server, 'private')).toEqual([])
  expect(await stored(page)).toBe('alpha')
})

test("the switcher opens the chosen workspace's Board and Back returns (AC-1.5)", async ({ page }) => {
  await open(page, 'alpha')
  await page.goto('/w/alpha/inbox?band=decisions')
  await expect(page.getByRole('heading', { name: 'Inbox' })).toBeVisible()

  await page.getByRole('navigation', { name: 'Workspaces' }).getByRole('button', { name: 'Switch to Bravo' }).click()
  await expect(page).toHaveURL(/\/w\/bravo\/$/)
  await expect(page.getByText('Bravo only change', { exact: true })).toBeVisible()
  await expect.poll(() => stored(page)).toBe('bravo')

  await page.goBack()
  await expect(page).toHaveURL(/\/w\/alpha\/inbox\?band=decisions$/)
  await expect(page.getByRole('heading', { name: 'Inbox' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Switch to Alpha' })).toHaveAttribute('aria-current', 'true')
})

test('two tabs on different workspaces each read and act on their own workspace (AC-1.7)', async ({ context }) => {
  const first = await open(context, 'alpha')
  await first.page.goto('/w/alpha/?create=true')
  await expect(first.page.getByRole('dialog', { name: 'New task' })).toBeVisible()
  // The second tab opens last, so the browser's remembered workspace is Bravo.
  const second = await open(context)
  await second.page.goto('/w/bravo/?create=true')
  await expect(second.page.getByRole('dialog', { name: 'New task' })).toBeVisible()
  await expect.poll(() => stored(first.page)).toBe('bravo')

  const create = async ({ page }: { page: Page }) => {
    const posted = page.waitForRequest(
      (request: Request) => new URL(request.url()).pathname === '/v1/tasks' && request.method() === 'POST',
    )
    await page.locator('textarea').first().fill('Ship the change')
    await page.getByRole('button', { name: 'Create task' }).click()
    return new URL((await posted).url()).searchParams.get('workspace_id')
  }
  expect(await create(first)).toBe('alpha')
  expect(await create(second)).toBe('bravo')

  const reads = (server: Server) =>
    new Set(server.requests.filter((request) => request.path === '/v1/activity').map((request) => request.workspace))
  expect(reads(first.server)).toEqual(new Set(['alpha']))
  expect(reads(second.server)).toEqual(new Set(['bravo']))
})

test('a task address without a workspace redirects to the membership holding the task (AC-1.8)', async ({ page }) => {
  const { server } = await open(page, 'alpha')
  await page.goto('/tasks/bravo-1/full?tab=history')
  await expect(page).toHaveURL(/\/w\/bravo\/tasks\/bravo-1\/full\?tab=history$/)
  await expect(page.getByRole('heading', { name: 'Bravo only change' })).toBeVisible()
  // The stored workspace is probed first, then the other membership. React
  // StrictMode's development rehearsal may repeat an aborted probe.
  const probes = server.requests.filter((request) => request.path === '/v1/tasks/bravo-1')
  expect([...new Set(probes.map((request) => request.workspace))]).toEqual(['alpha', 'bravo'])

  // A task no membership holds shows the not-found page and probes nothing
  // outside the memberships.
  await page.goto('/tasks/private-1')
  await expect(page.getByRole('heading', { name: 'Page not found' })).toBeVisible()
  await expect(page).toHaveURL(/\/tasks\/private-1$/)
  expect(scopedTo(server, 'private')).toEqual([])
})

test('another address without a workspace redirects under the remembered or only workspace (AC-1.9)', async ({
  browser,
}) => {
  // The remembered workspace.
  const remembered = await browser.newContext()
  const withStored = await open(remembered, 'bravo')
  await withStored.page.goto('/inbox?band=decisions')
  await expect(withStored.page).toHaveURL(/\/w\/bravo\/inbox\?band=decisions$/)
  await remembered.close()

  // No remembered workspace and one membership.
  const single = await browser.newContext()
  const only = await open(single)
  only.server.memberships = [alpha]
  await only.page.goto('/inbox?band=decisions')
  await expect(only.page).toHaveURL(/\/w\/alpha\/inbox\?band=decisions$/)
  await single.close()

  // Neither: the user chooses, and the choice keeps the path and search.
  const neither = await browser.newContext()
  const chooser = await open(neither)
  await chooser.page.goto('/inbox?band=decisions')
  const choices = chooser.page.getByRole('list', { name: 'Choose a workspace' })
  await expect(choices.getByRole('link')).toHaveText(['Alpha', 'Bravo'])
  await expect(chooser.page).toHaveURL(/\/inbox\?band=decisions$/)
  await choices.getByRole('link', { name: 'Bravo' }).click()
  await expect(chooser.page).toHaveURL(/\/w\/bravo\/inbox\?band=decisions$/)
  await neither.close()
})
