import { expect, type Page, test } from '@playwright/test'

// The Board half of the shared Tasks/Board filter family (AC-2.4). What each
// member means is asserted against the server in Go; what this covers is that
// the Board sends that family through one workspace activity cache, opens on
// the last month of activity, and remembers the operator's own adjustment.

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

function matchingActivity(url: string) {
  const params = new URL(url).searchParams
  const values = (key: string) => params.getAll(key)
  return activity.filter(({ task }) => {
    const query = params.get('q')?.toLocaleLowerCase()
    if (query && !`${task.id} ${task.title}`.toLocaleLowerCase().includes(query)) return false
    if (values('state').length > 0 && !values('state').includes(task.state)) return false
    if (values('repository').length > 0 && !values('repository').includes(task.repo)) return false
    const from = params.get('created_from')
    if (from && task.created_at < from) return false
    const to = params.get('created_to')
    if (to && task.created_at >= to) return false
    const assigneeFilter = params.get('assignee')
    const assignee = (task as typeof task & { assignee?: { user_id: string } }).assignee
    if (assigneeFilter === 'unassigned' && assignee) return false
    if (assigneeFilter && assigneeFilter !== 'unassigned' && assignee?.user_id !== assigneeFilter) {
      return false
    }
    return true
  })
}

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
    const limit = Number(params.get('limit') ?? 100)
    const offset = Number(params.get('offset') ?? 0)
    const matching = matchingActivity(url)
    if (!Number.isInteger(limit) || limit < 1 || limit > 200) {
      return route.fulfill({ status: 400, body: 'limit must be between 1 and 200\n' })
    }
    return route.fulfill({
      headers: {
        'X-Conveyor-Total': String(matching.length),
        'X-Conveyor-Limit': String(limit),
        'X-Conveyor-Offset': String(offset),
      },
      json: matching.slice(offset, offset + limit),
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

test('running cards use the in-flight stage when latest stage is stale', async ({ page }) => {
  const seen: string[] = []
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'test-token')
  })
  await routeBoard(page, seen)
  await page.route('**/v1/activity?**', (route) =>
    route.fulfill({
      headers: { 'X-Conveyor-Total': '1', 'X-Conveyor-Limit': '100', 'X-Conveyor-Offset': '0' },
      json: [
        {
          task: {
            id: 'task-running-review',
            workspace: 'demo',
            source: 'operator',
            title: 'Running external review',
            repo: 'conveyor',
            branch: 'conveyor/task-running-review',
            state: 'running',
            next_stage: 'review',
            created_at: '2026-08-16T10:00:00Z',
          },
          latest_stage: 'spec',
          last_event_at: '2026-08-16T11:00:00Z',
          needs_attention: false,
        },
      ],
    }),
  )
  await page.goto('/')
  await expect(page.getByRole('region', { name: 'Reviewing' })).toContainText('Running external review')
  await expect(page.getByRole('region', { name: 'Plan' })).not.toContainText('Running external review')
})

test('board pages beyond the 200th activity item and walks back without blanking', async ({ page }) => {
  const seen: string[] = []
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'test-token')
  })
  await routeBoard(page, seen)
  const many = Array.from({ length: 323 }, (_, index) => ({
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
    seen.push(route.request().url())
    const params = new URL(route.request().url()).searchParams
    const limit = Number(params.get('limit') ?? 100)
    const offset = Number(params.get('offset') ?? 0)
    if (!Number.isInteger(limit) || limit < 1 || limit > 200) {
      return route.fulfill({ status: 400, body: 'limit must be between 1 and 200\n' })
    }
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
  await expect(page.getByText('Showing 1–100 of 323')).toBeVisible()
  await expect(cards(page)).toHaveCount(100)
  await page.getByRole('button', { name: 'Next' }).click()
  await expect(page.getByText('Showing 101–200 of 323')).toBeVisible()
  await expect(cards(page)).toHaveCount(100)
  await page.getByRole('button', { name: 'Next' }).click()
  await expect(page.getByText('Showing 201–300 of 323')).toBeVisible()
  await expect(cards(page)).toHaveCount(100)
  await page.getByRole('button', { name: 'Next' }).click()
  await expect(page.getByText('Showing 301–323 of 323')).toBeVisible()
  await expect(cards(page)).toHaveCount(23)
  await expect(page.getByRole('button', { name: 'Next' })).toBeDisabled()
  await page.getByRole('button', { name: 'Previous' }).click()
  await expect(page.getByText('Showing 201–300 of 323')).toBeVisible()
  await expect(cards(page)).toHaveCount(100)
  expect(seen.some((url) => new URL(url).searchParams.get('offset') === '200')).toBe(true)
  expect(seen.some((url) => new URL(url).searchParams.get('offset') === '300')).toBe(true)
})

test('board opens on the last month and remembers the operator adjustment per workspace', async ({ page }) => {
  const seen: string[] = []
  const requests: string[] = []
  page.on('request', (request) => requests.push(new URL(request.url()).pathname))
  await openBoard(page, seen)

  // The default is a starting point the operator can see the effect of: the
  // board asks the server for recently created tasks across the workspace.
  await expect(cards(page).filter({ hasText: 'Recent conveyor change' })).toHaveCount(1)
  await expect(cards(page).filter({ hasText: 'Ancient web change' })).toHaveCount(0)
  // The Board owns the only activity refresh lifecycle; the rail reads the
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

test('board sends the shared filter family to the whole-workspace activity query', async ({ page }) => {
  const seen: string[] = []
  await openBoard(page, seen)

  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('ancient')
  await expect(cards(page)).toHaveCount(0)
  expect(seen.some((url) => url.includes('q=ancient'))).toBe(true)
  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('')

  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Created' }).click()
  await page.getByRole('option', { name: 'Any time' }).click()
  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('ancient')
  await expect(cards(page).filter({ hasText: 'Ancient web change' })).toHaveCount(1)
  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('')
  await page.getByRole('tab', { name: 'Repository' }).click()
  await page.getByRole('option', { name: 'web' }).click()
  await expect(cards(page).filter({ hasText: 'Ancient web change' })).toHaveCount(1)

  // Checking a second value keeps the first: the member travels as one
  // repeated parameter, and the server reads it as a disjunction (AC-2.4).
  await page.getByRole('option', { name: 'conveyor' }).click()
  await expect(cards(page).filter({ hasText: 'Ancient web change' })).toHaveCount(1)

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
  await expect(cards(page)).toHaveCount(0)

  // Unassigned is its own choice, not the absence of one, and it replaces the
  // member rather than accumulating beside it.
  await page.getByRole('option', { name: 'Unassigned' }).click()
  await expect(cards(page).filter({ hasText: 'Ancient web change' })).toHaveCount(1)
  expect(seen.some((url) => url.includes('assignee=usr_bo'))).toBe(true)
  expect(seen.some((url) => url.includes('assignee=unassigned'))).toBe(true)

  // AC-1.5 stands on this surface too: no barred field is offered as a filter.
  await expect(page.getByRole('tab', { name: 'Priority' })).toHaveCount(0)
  await expect(page.getByRole('tab', { name: 'Phase' })).toHaveCount(0)
})

test('activity polling is serialized, pauses while hidden, and cold-refreshes on resume', async ({ page }) => {
  const seen: string[] = []
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    localStorage.setItem('conveyor-task-filters:board:demo', JSON.stringify({ created: 'any' }))
    sessionStorage.setItem('conveyor-token', 'test-token')
    let visible = true
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => (visible ? 'visible' : 'hidden'),
    })
    ;(window as unknown as { setActivityVisible: (next: boolean) => void }).setActivityVisible = (next) => {
      visible = next
      document.dispatchEvent(new Event('visibilitychange'))
    }
    const nativeSetInterval = window.setInterval.bind(window)
    window.setInterval = ((handler: TimerHandler, timeout?: number, ...args: unknown[]) =>
      nativeSetInterval(handler, timeout === 15_000 ? 50 : timeout, ...args)) as typeof window.setInterval
  })
  await routeBoard(page, seen)
  let requests = 0
  let active = 0
  let maxActive = 0
  const urls: string[] = []
  await page.route('**/v1/activity?**', async (route) => {
    const url = new URL(route.request().url())
    const limit = Number(url.searchParams.get('limit') ?? 100)
    requests++
    active++
    maxActive = Math.max(maxActive, active)
    urls.push(url.toString())
    if (!Number.isInteger(limit) || limit < 1 || limit > 200) {
      active--
      await route.fulfill({ status: 400, body: 'limit must be between 1 and 200\n' })
      return
    }
    await new Promise((resolve) => setTimeout(resolve, 120))
    active--
    await route.fulfill({
      headers: {
        ETag: `"activity-${requests}"`,
        'X-Conveyor-Cursor': `cursor-${requests}`,
        'X-Conveyor-Total': '1',
        'X-Conveyor-Limit': String(limit),
        'X-Conveyor-Offset': '0',
      },
      json: [activity[0]],
    })
  })

  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Board' })).toBeVisible()
  await expect(page.getByText(/Activity feed unavailable:/)).toHaveCount(0)
  await expect.poll(() => requests).toBeGreaterThanOrEqual(3)
  expect(maxActive).toBe(1)
  expect(urls.every((url) => Number(new URL(url).searchParams.get('limit')) === 100)).toBe(true)
  expect(urls.some((url) => new URL(url).searchParams.has('since'))).toBe(true)
  await page.evaluate(() =>
    (window as unknown as { setActivityVisible: (next: boolean) => void }).setActivityVisible(false),
  )
  await expect.poll(() => active).toBe(0)
  const hiddenCount = requests
  await page.waitForTimeout(200)
  expect(requests).toBe(hiddenCount)
  await page.evaluate(() =>
    (window as unknown as { setActivityVisible: (next: boolean) => void }).setActivityVisible(true),
  )
  await expect.poll(() => requests).toBeGreaterThan(hiddenCount)
  expect(urls.every((url) => Number(new URL(url).searchParams.get('limit')) === 100)).toBe(true)
  expect(new URL(urls.at(-1) ?? '').searchParams.has('since')).toBe(false)
})

test('simultaneous filtered Board and task-order consumers share one serialized refresh lifecycle', async ({
  page,
}) => {
  const seen: string[] = []
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'test-token')
  })
  await routeBoard(page, seen)
  await page.route('**/v1/tasks/task-recent/activity**', (route) =>
    route.fulfill({
      json: {
        task: activity[0].task,
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
  let active = 0
  let maxActive = 0
  const urls: string[] = []
  await page.route('**/v1/activity?**', async (route) => {
    const url = route.request().url()
    const params = new URL(url).searchParams
    const matching = matchingActivity(url)
    active++
    maxActive = Math.max(maxActive, active)
    urls.push(url)
    await new Promise((resolve) => setTimeout(resolve, 80))
    active--
    await route.fulfill({
      headers: {
        'X-Conveyor-Total': String(matching.length),
        'X-Conveyor-Limit': params.get('limit') ?? '100',
        'X-Conveyor-Offset': params.get('offset') ?? '0',
      },
      json: matching,
    })
  })

  await page.goto('/tasks/task-recent')
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toBeVisible()
  await expect.poll(() => urls.length).toBeGreaterThanOrEqual(2)
  expect(urls.some((url) => new URL(url).searchParams.has('created_from'))).toBe(true)
  expect(urls.some((url) => !new URL(url).searchParams.has('created_from'))).toBe(true)
  expect(maxActive).toBe(1)
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
