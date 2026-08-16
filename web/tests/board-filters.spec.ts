import { expect, type Page, test } from '@playwright/test'

// The Board half of the shared Tasks/Board filter family (AC-2.4). What each
// member means is asserted against the server in Go; what this covers is that
// the Board sends the same family to the same route, opens on the last month of
// activity, and remembers the operator's own adjustment per workspace.

const activity = [
  {
    task: {
      id: 'task-recent',
      workspace: 'demo',
      source: 'operator',
      title: 'Recent conveyor change',
      repo: 'conveyor',
      branch: 'conveyor/task-recent',
      state: 'running',
      created_at: '2026-08-06T10:00:00Z',
    },
    latest_stage: 'implement',
    last_event_at: '2026-08-06T11:00:00Z',
    needs_attention: false,
  },
  {
    task: {
      id: 'task-ancient',
      workspace: 'demo',
      source: 'cli',
      title: 'Ancient web change',
      repo: 'web',
      branch: 'web/task-ancient',
      state: 'running',
      created_at: '2025-01-02T10:00:00Z',
    },
    latest_stage: 'implement',
    last_event_at: '2025-01-02T11:00:00Z',
    needs_attention: false,
  },
  {
    task: {
      id: 'task-recent-zeta',
      workspace: 'demo',
      source: 'operator',
      title: 'Same-time conveyor change',
      repo: 'conveyor',
      branch: 'conveyor/task-recent-zeta',
      state: 'running',
      created_at: '2026-08-06T10:00:00Z',
    },
    latest_stage: 'implement',
    last_event_at: '2026-08-08T11:00:00Z',
    needs_attention: false,
  },
  {
    task: {
      id: 'task-older-busy',
      workspace: 'demo',
      source: 'operator',
      title: 'Older busy conveyor change',
      repo: 'conveyor',
      branch: 'conveyor/task-older-busy',
      state: 'running',
      created_at: '2026-08-05T10:00:00Z',
    },
    latest_stage: 'implement',
    last_event_at: '2026-08-09T11:00:00Z',
    needs_attention: false,
  },
]

const requirementCorpus = [
  { requirement: { id: 'req-tasks-view', title: 'Task-centric operations view' }, current_version: { version: 1 } },
]
const designCorpus = [
  { document: { id: 'design-lifecycle', title: 'Work-order lifecycle' }, current_version: { version: 16 } },
]

// Every request the board makes for its feed, so the test can assert what the
// server was actually asked rather than only what came back.
async function routeBoard(page: Page, seen: string[]) {
  // Two workspaces, because the scoping case below switches between them and a
  // singleton install would simply reselect the only one there is.
  await page.route('**/v1/workspaces', (route) =>
    route.fulfill({
      json: [
        { id: 'demo', name: 'Demo' },
        { id: 'other', name: 'Other' },
      ],
    }),
  )
  await page.route('**/v1/workspace?**', (route) =>
    route.fulfill({
      json: {
        workspace: 'demo',
        repos: [
          { name: 'conveyor', url: 'https://example.test/conveyor', base: 'main' },
          { name: 'web', url: 'https://example.test/web', base: 'main' },
        ],
      },
    }),
  )
  await page.route('**/v1/requirements**', (route) => route.fulfill({ json: requirementCorpus }))
  await page.route('**/v1/system-designs**', (route) => route.fulfill({ json: designCorpus }))
  await page.route('**/v1/pending-proposals**', (route) =>
    route.fulfill({ json: { items: [], attention: { task_count: 0, pending_proposal_count: 0, total: 0 } } }),
  )
  // The assignee member offers workspace co-members, so the shared filter row
  // needs the membership read on this surface too.
  await page.route('**/v1/workspaces/*/members**', (route) =>
    route.fulfill({
      json: [
        {
          workspace_id: 'demo',
          user_id: 'usr_ada',
          display_name: 'Ada Owner',
          role: 'operator',
          created_at: '2026-08-06T10:00:00Z',
        },
        {
          workspace_id: 'demo',
          user_id: 'usr_bo',
          display_name: 'Bo Member',
          role: 'contributor',
          created_at: '2026-08-06T10:00:00Z',
        },
      ],
    }),
  )
  await page.route('**/v1/workers**', (route) =>
    route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false, setup_serviceability: {} } }),
  )
  await page.route('**/v1/tasks?**', (route) => {
    if (route.request().method() !== 'POST') return route.fallback()
    return route.fulfill({
      status: 201,
      json: {
        id: 'task-created-on-board',
        workspace: 'demo',
        title: 'Created on Board',
        body: 'Create this task without leaving the board',
        repo: 'conveyor',
        state: 'queued',
        created_at: '2026-08-10T12:00:00Z',
      },
    })
  })
  await page.route('**/v1/tasks/task-created-on-board/activity**', (route) =>
    route.fulfill({
      json: {
        task: {
          id: 'task-created-on-board',
          workspace: 'demo',
          title: 'Created on Board',
          body: 'Create this task without leaving the board',
          repo: 'conveyor',
          state: 'queued',
          created_at: '2026-08-10T12:00:00Z',
        },
        jobs: [],
        events: [],
        interventions: [],
        work_orders: [],
        checkout_available: false,
        checkout_guidance: '',
        needs_attention: false,
      },
    }),
  )
  await page.route('**/v1/task-operations?**', (route) => route.fulfill({ json: [] }))
  await page.route('**/v1/activity?**', (route) => {
    const url = route.request().url()
    seen.push(url)
    const params = new URL(url).searchParams
    const from = params.get('created_from') ?? ''
    const needle = (params.get('q') ?? '').toLowerCase()
    // List members repeat their parameter and mean "any of" (AC-2.4).
    const repositories = params.getAll('repository')
    const filtered = activity.filter((item) => {
      if (from && item.task.created_at < from) return false
      if (repositories.length && !repositories.includes(item.task.repo)) return false
      return !needle || item.task.title.toLowerCase().includes(needle)
    })
    const limit = Number(params.get('limit') ?? 100)
    const offset = Number(params.get('offset') ?? 0)
    return route.fulfill({
      headers: {
        'X-Conveyor-Total': String(filtered.length),
        'X-Conveyor-Limit': String(limit),
        'X-Conveyor-Offset': String(offset),
      },
      json: filtered.slice(offset, offset + limit),
    })
  })
}

async function openBoard(page: Page, seen: string[], workspace = 'demo') {
  // Seed the selection only once: the workspace-scoping case below switches
  // workspaces and reloads, and an unconditional init script would put the
  // original one back before the app read it.
  await page.addInitScript((selected) => {
    if (!localStorage.getItem('conveyor-workspace')) localStorage.setItem('conveyor-workspace', selected)
    sessionStorage.setItem('conveyor-token', 'test-token')
  }, workspace)
  await routeBoard(page, seen)
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Board', exact: true })).toBeVisible()
}

function cards(page: Page) {
  return page.getByRole('region', { name: 'Task board' }).getByRole('link')
}

test('board orders each stage by creation time rather than latest activity', async ({ page }) => {
  const seen: string[] = []
  await openBoard(page, seen)

  await expect(cards(page)).toContainText([
    'Recent conveyor change',
    'Same-time conveyor change',
    'Older busy conveyor change',
  ])
})

test('board pages a bounded activity window and reports its position', async ({ page }) => {
  const seen: string[] = []
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'test-token')
  })
  await routeBoard(page, seen)
  const many = Array.from({ length: 205 }, (_, index) => ({
    task: {
      id: `paged-${String(index).padStart(3, '0')}`,
      workspace: 'demo',
      source: 'operator',
      title: `Paged task ${String(index).padStart(3, '0')}`,
      repo: 'conveyor',
      branch: `conveyor/paged-${index}`,
      state: 'running',
      created_at: new Date(Date.UTC(2026, 7, 15, 12, 0, 0) - index * 1_000).toISOString(),
    },
    latest_stage: 'implement',
    last_event_at: '2026-08-15T12:00:00Z',
    needs_attention: false,
  }))
  await page.route('**/v1/activity?**', (route) => {
    const params = new URL(route.request().url()).searchParams
    const limit = Number(params.get('limit') ?? 100)
    const offset = Number(params.get('offset') ?? 0)
    return route.fulfill({
      headers: {
        'X-Conveyor-Total': String(many.length),
        'X-Conveyor-Limit': String(limit),
        'X-Conveyor-Offset': String(offset),
      },
      json: many.slice(offset, offset + limit),
    })
  })

  await page.goto('/')
  await expect(page.getByText('Showing 1–100 of 205')).toBeVisible()
  await expect(cards(page)).toHaveCount(100)
  await page.getByRole('button', { name: 'Next' }).click()
  await expect(page.getByText('Showing 101–200 of 205')).toBeVisible()
  await expect(cards(page)).toHaveCount(100)
  await page.getByRole('button', { name: 'Next' }).click()
  await expect(page.getByText('Showing 201–205 of 205')).toBeVisible()
  await expect(cards(page)).toHaveCount(5)
  await expect(page.getByRole('button', { name: 'Next' })).toBeDisabled()
})

test('board opens on the last month and remembers the operator adjustment per workspace', async ({ page }) => {
  const seen: string[] = []
  const requests: string[] = []
  page.on('request', (request) => requests.push(new URL(request.url()).pathname))
  await openBoard(page, seen)

  // The default is a starting point the operator can see the effect of: the
  // board asks the server for recently created tasks, and the ancient task is gone.
  await expect(cards(page).filter({ hasText: 'Recent conveyor change' })).toHaveCount(1)
  await expect(cards(page).filter({ hasText: 'Ancient web change' })).toHaveCount(0)
  // The Board owns the only activity query; the rail reads the bounded
  // pending-proposals attention projection instead of mounting a second,
  // unfiltered activity request.
  expect(seen).toHaveLength(1)
  expect(seen[0]).toContain('created_from=')
  expect(requests.filter((path) => path === '/v1/workspaces/demo/members')).toHaveLength(1)
  expect(requests.filter((path) => path === '/v1/pending-proposals')).toHaveLength(1)

  // Adjusting it is what gets remembered — including across a reload.
  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Created' }).click()
  await expect(page.getByRole('option', { name: 'Last month' })).toHaveAttribute('aria-selected', 'true')
  await page.getByRole('option', { name: 'Any time' }).click()
  await expect(cards(page).filter({ hasText: 'Ancient web change' })).toHaveCount(1)
  await page.reload()
  await expect(cards(page).filter({ hasText: 'Ancient web change' })).toHaveCount(1)
  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Created' }).click()
  await expect(page.getByRole('option', { name: 'Any time' })).toHaveAttribute('aria-selected', 'true')

  // Persistence is scoped to the workspace it was set in, so another workspace
  // opens on its own default rather than inheriting a repository filter or a
  // window from a workspace it has nothing in common with.
  expect(await page.evaluate(() => localStorage.getItem('conveyor-task-filters:board:demo'))).toContain(
    '"created":"any"',
  )
  await page.evaluate(() => localStorage.setItem('conveyor-workspace', 'other'))
  await page.reload()
  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Created' }).click()
  await expect(page.getByRole('option', { name: 'Last month' })).toHaveAttribute('aria-selected', 'true')
})

test('board migrates a saved Updated window and persists only the Created shape', async ({ page }) => {
  const seen: string[] = []
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'test-token')
    localStorage.setItem(
      'conveyor-task-filters:board:demo',
      JSON.stringify({ updated: '7d', updatedFrom: '', updatedTo: '' }),
    )
  })
  await routeBoard(page, seen)
  await page.goto('/')

  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Created' }).click()
  await expect(page.getByRole('option', { name: 'Last 7 days' })).toHaveAttribute('aria-selected', 'true')
  await page.getByRole('option', { name: 'Any time' }).click()

  const stored = await page.evaluate(() => JSON.parse(localStorage.getItem('conveyor-task-filters:board:demo') ?? '{}'))
  expect(stored).toMatchObject({ created: 'any', createdFrom: '', createdTo: '' })
  expect(stored).not.toHaveProperty('updated')
})

test('board sends the shared filter family to the server rather than narrowing in the browser', async ({ page }) => {
  const seen: string[] = []
  await openBoard(page, seen)

  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('ancient')
  await expect.poll(() => seen.some((url) => url.includes('q=ancient')), { timeout: 5000 }).toBe(true)

  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Repository' }).click()
  await page.getByRole('option', { name: 'web' }).click()
  await expect.poll(() => seen.some((url) => url.includes('repository=web')), { timeout: 5000 }).toBe(true)

  // Checking a second value keeps the first: the member travels as one
  // repeated parameter, and the server reads it as a disjunction (AC-2.4).
  await page.getByRole('option', { name: 'conveyor' }).click()
  await expect
    .poll(() => seen.some((url) => url.includes('repository=web') && url.includes('repository=conveyor')), {
      timeout: 5000,
    })
    .toBe(true)

  // The same family the Tasks list offers, on the same surface, by the same
  // component — including the two document filters.
  await expect(page.getByRole('tab', { name: 'Requirement' })).toBeVisible()
  await expect(page.getByRole('tab', { name: 'System design' })).toBeVisible()

  // Assignee travels the same way, as one single-valued member: a task has one
  // holder, so there is no union to express. DEC-18 bars priority and phase and
  // permits assignee as a claim-eligibility constraint, so it belongs here while
  // the barred fields still do not.
  await page.getByRole('tab', { name: 'Assignee' }).click()
  await page.getByRole('option', { name: 'Bo Member' }).click()
  await expect.poll(() => seen.some((url) => url.includes('assignee=usr_bo')), { timeout: 5000 }).toBe(true)

  // Unassigned is its own choice, not the absence of one, and it replaces the
  // member rather than accumulating beside it.
  await page.getByRole('option', { name: 'Unassigned' }).click()
  await expect.poll(() => seen.some((url) => url.includes('assignee=unassigned')), { timeout: 5000 }).toBe(true)
  expect(seen.some((url) => url.includes('assignee=usr_bo') && url.includes('assignee=unassigned'))).toBe(false)

  // AC-1.5 stands on this surface too: no barred field is offered as a filter.
  await expect(page.getByRole('tab', { name: 'Priority' })).toHaveCount(0)
  await expect(page.getByRole('tab', { name: 'Phase' })).toHaveCount(0)
})

test('board opens and closes task creation without leaving the board', async ({ page }) => {
  const seen: string[] = []
  await openBoard(page, seen)
  await page.getByRole('button', { name: 'New task' }).click()
  await expect(page.getByRole('dialog', { name: 'New task' })).toBeVisible()
  expect(new URL(page.url()).pathname).toBe('/')
  await expect(page.getByRole('heading', { name: 'Board' })).toBeVisible()

  await page.getByRole('button', { name: 'Close', exact: true }).click()
  await expect(page.getByRole('dialog', { name: 'New task' })).toHaveCount(0)
  expect(new URL(page.url()).pathname).toBe('/')
})

test('board creation opens the new task in the board detail route', async ({ page }) => {
  const seen: string[] = []
  await openBoard(page, seen)

  await page.getByRole('button', { name: 'New task' }).click()
  await page.locator('textarea').fill('Create this task without leaving the board')
  await page.getByRole('button', { name: 'Create task' }).click()

  await expect(page).toHaveURL(/\/tasks\/task-created-on-board$/)
  await expect(page.getByRole('heading', { name: 'Board', exact: true })).toBeVisible()
  await expect(page.getByRole('dialog', { name: 'New task' })).toHaveCount(0)
  const detail = page.getByRole('dialog', { name: 'Task detail' })
  await expect(detail).toBeVisible()
  await expect(detail.getByRole('heading', { name: 'Created on Board', exact: true })).toBeVisible()
})
