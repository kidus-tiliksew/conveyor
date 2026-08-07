import { expect, test, type Page } from '@playwright/test'

// UI coverage for the list-first Tasks view (spec §21.58, REQ-1 and
// AC-1.1 through AC-1.5). The projection is mocked so each assertion reads the
// rendering of durable authority rather than a live pipeline.
const operations = [
  {
    task: {
      id: 'task-blocked',
      workspace: 'demo',
      source: 'planning_bundle:bundle-1',
      title: 'Wire the Tasks view',
      repo: 'conveyor',
      branch: 'conveyor/task-blocked',
      state: 'queued',
      next_stage: 'implement',
      created_at: '2026-08-06T10:00:00Z',
      blocking_task_ids: ['task-dead', 'task-open'],
      dependencies: [
        { id: 'task-dead', title: 'Abandoned prerequisite', state: 'closed' },
        { id: 'task-done', title: 'Delivered prerequisite', state: 'merged' },
        { id: 'task-open', title: 'Running prerequisite', state: 'running' },
      ],
      context: {
        requirements: [{ id: 'req-tasks-view', title: 'Task-centric operations view', version: 1 }],
        designs: [{ id: 'design-lifecycle', title: 'Work-order lifecycle', version: 16 }],
      },
    },
    latest_stage: 'spec',
    last_event_at: '2026-08-06T11:00:00Z',
    needs_attention: false,
    unsatisfiable_task_ids: ['task-dead'],
    plan: { state: 'pending_gate', version: 2 },
  },
  {
    task: {
      id: 'task-anchor',
      workspace: 'demo',
      source: 'cli',
      title: 'Historical anchor',
      repo: 'conveyor',
      branch: 'conveyor/task-anchor',
      state: 'running',
      created_at: '2026-08-05T10:00:00Z',
    },
    latest_stage: 'implement',
    last_event_at: '2026-08-06T09:00:00Z',
    needs_attention: false,
    child_rollup: { total: 3, merged: 1, closed: 1, open: 1 },
    plan: { state: 'approved', version: 1 },
  },
  {
    task: {
      id: 'task-shipped',
      workspace: 'demo',
      source: 'cli',
      title: 'Shipped web change',
      repo: 'web',
      branch: 'web/task-shipped',
      state: 'merged',
      created_at: '2026-08-04T10:00:00Z',
    },
    last_event_at: '2026-08-05T09:00:00Z',
    needs_attention: false,
    plan: { state: 'none' },
  },
  {
    task: {
      id: 'task-stuck',
      workspace: 'demo',
      source: 'cli',
      title: 'Stuck conveyor change',
      repo: 'conveyor',
      branch: 'conveyor/task-stuck',
      state: 'queued',
      next_stage: 'implement',
      created_at: '2026-08-02T10:00:00Z',
    },
    latest_stage: 'implement',
    last_event_at: '2026-08-03T09:00:00Z',
    // The list-scoped summary: the reason, never the work order (§21.58
    // change 7). The detail surfaces are where the order itself renders.
    stalled: {
      needed: true,
      reason: 'dispatch is failing repeatedly',
      last_failure: 'harness exited before completing work order',
    },
    needs_attention: true,
    plan: { state: 'approved', version: 1 },
  },
  {
    task: {
      id: 'task-bounced',
      workspace: 'demo',
      source: 'cli',
      title: 'Bounced web plan',
      repo: 'web',
      branch: 'web/task-bounced',
      state: 'queued',
      next_stage: 'spec',
      created_at: '2026-08-03T10:00:00Z',
    },
    last_event_at: '2026-08-04T09:00:00Z',
    needs_attention: false,
    plan: { state: 'redirected', version: 1 },
  },
]

async function openTasks(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'test-token')
  })
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  // The repository filter reads the configured repositories, so this fixture
  // carries the real config.Repo shape rather than bare names.
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
  await page.route('**/v1/activity?**', (route) => route.fulfill({ json: [] }))
  await page.route('**/v1/task-operations?**', (route) => {
    expect(route.request().headers().authorization).toBe('Bearer test-token')
    // Mirror the server contract: state and repository narrow the set before
    // paging, and the page bounds travel in the additive response headers.
    const params = new URL(route.request().url()).searchParams
    const state = params.get('state') ?? ''
    const repository = params.get('repository') ?? ''
    const matched = operations.filter(
      (item) => (!state || item.task.state === state) && (!repository || item.task.repo === repository),
    )
    const offset = Number(params.get('offset') ?? '0')
    const limit = Number(params.get('limit') ?? String(matched.length))
    return route.fulfill({
      body: JSON.stringify(matched.slice(offset, offset + limit)),
      headers: {
        'content-type': 'application/json',
        'X-Conveyor-Total': String(matched.length),
        'X-Conveyor-Limit': String(limit),
        'X-Conveyor-Offset': String(offset),
      },
    })
  })
  await page.goto('/tasks')
  await expect(page.getByRole('heading', { name: 'Tasks' })).toBeVisible()
}

function rows(page: Page) {
  return page.getByRole('list', { name: 'Tasks' }).getByRole('listitem')
}

// AC-1.1: the view is a filterable list, not stage columns, and filters by
// state, repository, and free text.
test('tasks view lists every task and filters by state, repository, and free text', async ({ page }) => {
  await openTasks(page)
  await expect(rows(page)).toHaveCount(5)
  // The board's stage columns are not this surface.
  await expect(page.getByRole('heading', { name: 'Needs operator' })).toHaveCount(0)

  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('bounced')
  await expect(rows(page)).toHaveCount(1)
  await expect(page.getByRole('link', { name: 'Bounced web plan' })).toBeVisible()

  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('')
  await page.getByLabel('Filter by repository').selectOption('web')
  await expect(rows(page)).toHaveCount(2)

  await page.getByLabel('Filter by state').selectOption('merged')
  await expect(rows(page)).toHaveCount(1)
  await expect(page.getByRole('link', { name: 'Shipped web change' })).toBeVisible()

  await page.getByLabel('Filter by state').selectOption('running')
  await expect(rows(page)).toHaveCount(0)
  await expect(page.getByRole('heading', { name: 'No tasks match these filters' })).toBeVisible()
})

// AC-1.2: dependencies, blocking state, and child rollups render from durable
// authority, and their absence renders as absence.
test('tasks view shows dependency, blocking, and child rollup state', async ({ page }) => {
  await openTasks(page)
  const blocked = rows(page).filter({ hasText: 'Wire the Tasks view' })
  await expect(blocked.getByText('Blocked by 2 of 3')).toBeVisible()
  await expect(blocked.getByText('Abandoned prerequisite · unsatisfiable')).toBeVisible()
  await expect(blocked.getByText('Running prerequisite · blocking')).toBeVisible()
  // A merged dependency is satisfied, so it is listed without a blocking mark.
  await expect(blocked.getByText('Delivered prerequisite', { exact: true })).toBeVisible()

  const anchor = rows(page).filter({ hasText: 'Historical anchor' })
  await expect(anchor.getByText('3 child tasks · 1 merged · 1 closed · 1 open')).toBeVisible()
  await expect(anchor.getByText('No dependencies')).toBeVisible()

  // A task with no children states that rather than tallying zeroes.
  const shipped = rows(page).filter({ hasText: 'Shipped web change' })
  await expect(shipped.getByText('child task')).toHaveCount(0)
})

// AC-1.3: attached context links through the existing document routes, and an
// unattached task says so.
test('tasks view links attached requirements and design documents', async ({ page }) => {
  await openTasks(page)
  const blocked = rows(page).filter({ hasText: 'Wire the Tasks view' })
  await expect(blocked.getByRole('link', { name: 'Task-centric operations view v1' })).toHaveAttribute(
    'href',
    '/requirements?requirement=req-tasks-view',
  )
  await expect(blocked.getByRole('link', { name: 'Work-order lifecycle v16' })).toHaveAttribute(
    'href',
    '/system-design?document=design-lifecycle',
  )
  // Each row hands off to the task's own detail route rather than restating it.
  await expect(blocked.getByRole('link', { name: 'Wire the Tasks view' })).toHaveAttribute(
    'href',
    '/tasks/task-blocked/full',
  )

  await expect(rows(page).filter({ hasText: 'Shipped web change' }).getByText('No attached context')).toBeVisible()
})

// AC-1.4: every plan-era row reports one of the four durable plan outcomes.
test('tasks view reports plan status for every task', async ({ page }) => {
  await openTasks(page)
  await expect(
    rows(page).filter({ hasText: 'Wire the Tasks view' }).getByText('Plan awaiting approval v2'),
  ).toBeVisible()
  await expect(rows(page).filter({ hasText: 'Historical anchor' }).getByText('Plan approved v1')).toBeVisible()
  await expect(rows(page).filter({ hasText: 'Bounced web plan' }).getByText('Plan changes requested v1')).toBeVisible()
  await expect(rows(page).filter({ hasText: 'Shipped web change' }).getByText('No plan')).toBeVisible()
})

// The list-first surface keeps the empty and error behavior the other document
// surfaces have, so an unreachable projection never reads as an empty factory.
test('tasks view distinguishes an empty workspace from a failed load', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'test-token')
  })
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  await page.route('**/v1/workspace?**', (route) =>
    route.fulfill({
      json: { workspace: 'demo', repos: [{ name: 'conveyor', url: 'https://example.test/conveyor', base: 'main' }] },
    }),
  )
  await page.route('**/v1/activity?**', (route) => route.fulfill({ json: [] }))
  await page.route('**/v1/task-operations?**', (route) => route.fulfill({ json: [] }))
  await page.goto('/tasks')
  await expect(page.getByRole('heading', { name: 'No tasks yet' })).toBeVisible()

  await page.unrouteAll({ behavior: 'ignoreErrors' })
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  await page.route('**/v1/workspace?**', (route) =>
    route.fulfill({
      json: { workspace: 'demo', repos: [{ name: 'conveyor', url: 'https://example.test/conveyor', base: 'main' }] },
    }),
  )
  await page.route('**/v1/activity?**', (route) => route.fulfill({ json: [] }))
  await page.route('**/v1/task-operations?**', (route) =>
    route.fulfill({ status: 500, body: 'task operations unavailable' }),
  )
  await page.reload()
  await expect(page.getByText('task operations unavailable')).toBeVisible()
})

// Staleness renders from the durable §21.34 state the projection carries, and
// it says why the task cannot move (spec §21.58 change 7). It sits beside the
// needs-operator badge rather than replacing it: a task can hold at a gate and
// carry a stalled order at once, and a row that hides one of those misreads.
test('tasks view reports why a stalled task cannot move on its own', async ({ page }) => {
  await openTasks(page)
  const stuck = rows(page).filter({ hasText: 'Stuck conveyor change' })
  await expect(
    stuck.getByText('Stalled — dispatch is failing repeatedly: harness exited before completing work order'),
  ).toBeVisible()
  await expect(stuck.getByText('Needs operator')).toBeVisible()

  const healthy = rows(page).filter({ hasText: 'Shipped web change' })
  await expect(healthy.getByText('Stalled', { exact: false })).toHaveCount(0)
  await expect(healthy.getByText('Needs operator')).toHaveCount(0)
})

// AC-1.5: no barred field appears on the surface, and none is offered as a
// filter or sort control.
test('tasks view exposes no priority, assignee, or declared-phase field', async ({ page }) => {
  await openTasks(page)
  const surface = (await page.locator('main').innerText()).toLowerCase()
  for (const barred of ['priority', 'assignee', 'assigned to', 'phase']) {
    expect(surface).not.toContain(barred)
  }
  await expect(page.getByLabel('Filter by priority')).toHaveCount(0)
  await expect(page.getByLabel('Filter by assignee')).toHaveCount(0)
})
