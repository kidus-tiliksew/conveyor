import { expect, test, type Page } from '@playwright/test'

// UI coverage for the list-first Tasks view. The projection is mocked so each
// assertion reads the rendering of durable authority rather than a live pipeline.
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
        // A real document title is written for the document, not for a table
        // column, so the fixture carries one long enough to press on the Context
        // cell the way the corpus does.
        designs: [{ id: 'design-lifecycle', title: 'Work-order lifecycle and MCP surface', version: 16 }],
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

// The confirmed corpora the shared filter family reads its requirement and
// design options from (AC-2.4). Only a confirmed document can be attached to a
// task, so only a confirmed document is offered.
const requirementCorpus = [
  { requirement: { id: 'req-tasks-view', title: 'Task-centric operations view' }, current_version: { version: 1 } },
  { requirement: { id: 'req-draft', title: 'Unconfirmed idea' }, current_version: null },
]
const designCorpus = [
  {
    document: { id: 'design-lifecycle', title: 'Work-order lifecycle and MCP surface' },
    current_version: { version: 16 },
  },
]

// The mock stands in for the server-side predicate, so a surface that filtered
// in the browser instead would return the whole fixture and fail (AC-2.3).
function matchOperations(url: string) {
  const params = new URL(url).searchParams
  // Each list member repeats its parameter and matches on any listed value,
  // exactly as parseTaskFilter reads it on the server (AC-2.4).
  const states = params.getAll('state')
  const repositories = params.getAll('repository')
  const requirements = params.getAll('serves_requirement')
  const designs = params.getAll('governing_design')
  const needle = (params.get('q') ?? '').toLowerCase()
  const from = params.get('created_from') ?? ''
  const to = params.get('created_to') ?? ''
  return operations
    .filter((item) => {
      if (states.length && !states.includes(item.task.state)) return false
      if (repositories.length && !repositories.includes(item.task.repo)) return false
      if (
        needle &&
        ![item.task.title, item.task.id, item.task.source, item.task.branch]
          .filter(Boolean)
          .some((field) => field.toLowerCase().includes(needle))
      ) {
        return false
      }
      if (from && item.task.created_at < from) return false
      // The upper bound is exclusive, exactly as the store evaluates it.
      if (to && item.task.created_at >= to) return false
      if (
        requirements.length &&
        !(item.task.context?.requirements ?? []).some((entry) => requirements.includes(entry.id))
      ) {
        return false
      }
      if (designs.length && !(item.task.context?.designs ?? []).some((entry) => designs.includes(entry.id))) {
        return false
      }
      return true
    })
    .sort((a, b) => {
      const createdOrder = new Date(b.task.created_at).getTime() - new Date(a.task.created_at).getTime()
      return createdOrder || a.task.id.localeCompare(b.task.id)
    })
}

async function routeTasksSurface(page: Page) {
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
  await page.route('**/v1/requirements**', (route) => route.fulfill({ json: requirementCorpus }))
  await page.route('**/v1/system-designs**', (route) => route.fulfill({ json: designCorpus }))
  await page.route('**/v1/activity?**', (route) => route.fulfill({ json: [] }))
  await page.route('**/v1/tasks/*/activity**', (route) => {
    const taskId = /\/v1\/tasks\/([^/]+)\/activity/.exec(new URL(route.request().url()).pathname)?.[1] ?? ''
    const match = operations.find((item) => item.task.id === taskId)
    return route.fulfill({
      json: {
        task: match?.task ?? { id: taskId, workspace: 'demo', title: taskId, state: 'queued' },
        jobs: [],
        events: [],
        interventions: [],
        work_orders: [],
        checkout_available: false,
        checkout_guidance: '',
        needs_attention: false,
      },
    })
  })
  await page.route('**/v1/task-operations?**', (route) => {
    expect(route.request().headers().authorization).toBe('Bearer test-token')
    // Mirror the server contract: every filter narrows the set before paging,
    // and the page bounds travel in the additive response headers.
    const params = new URL(route.request().url()).searchParams
    const matched = matchOperations(route.request().url())
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
}

async function openTasks(page: Page) {
  await routeTasksSurface(page)
  await page.goto('/tasks')
  await expect(page.getByRole('heading', { name: 'Tasks' })).toBeVisible()
}

function rows(page: Page) {
  return page.getByRole('list', { name: 'Tasks' }).getByRole('listitem')
}

test('tasks view preserves newest-created-first API order', async ({ page }) => {
  await openTasks(page)
  await expect(rows(page)).toContainText([
    'Wire the Tasks view',
    'Historical anchor',
    'Shipped web change',
    'Bounced web plan',
    'Stuck conveyor change',
  ])
})

// AC-1.1: the view is a filterable list, not stage columns, and filters by
// state, repository, and free text. State and repository are multi-select:
// checking a second value widens the member rather than replacing it.
test('tasks view lists every task and filters by state, repository, and free text', async ({ page }) => {
  await openTasks(page)
  await expect(rows(page)).toHaveCount(5)
  // The board's stage columns are not this surface.
  await expect(page.getByRole('heading', { name: 'Needs operator' })).toHaveCount(0)

  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('bounced')
  await expect(rows(page)).toHaveCount(1)
  await expect(page.getByRole('link', { name: 'Bounced web plan' })).toBeVisible()

  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('')
  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Repository' }).click()
  await page.getByRole('option', { name: 'web' }).click()
  await expect(rows(page)).toHaveCount(2)

  await page.getByRole('tab', { name: 'Status' }).click()
  await page.getByRole('option', { name: 'Merged' }).click()
  await expect(rows(page)).toHaveCount(1)
  await expect(page.getByRole('link', { name: 'Shipped web change' })).toBeVisible()

  // Both checks hold at once: merged-or-queued over the web repository is the
  // two web tasks, and the menu stays open for the next adjustment.
  await page.getByRole('option', { name: 'Queued' }).click()
  await expect(rows(page)).toHaveCount(2)
  await expect(page.getByRole('option', { name: 'Merged' })).toHaveAttribute('aria-selected', 'true')

  // Narrowing to a pair no seeded task satisfies empties the list rather than
  // falling back to the unfiltered workspace.
  await page.getByRole('option', { name: 'Merged' }).click()
  await page.getByRole('option', { name: 'Queued' }).click()
  await page.getByRole('option', { name: 'Running' }).click()
  await expect(rows(page)).toHaveCount(0)
  await expect(page.getByRole('heading', { name: 'No tasks match these filters' })).toBeVisible()
})

// AC-1.2: child rollups render from durable authority, and their absence
// renders as absence. The table redesign folded the per-dependency breakdown
// out of the row — the task's own detail panel is where it renders now.
test('tasks view shows child rollup state', async ({ page }) => {
  await openTasks(page)
  const anchor = rows(page).filter({ hasText: 'Historical anchor' })
  await expect(anchor.getByText('3 child tasks · 1 merged · 1 closed · 1 open')).toBeVisible()

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
  await expect(blocked.getByRole('link', { name: 'Work-order lifecycle and MCP surface v16' })).toHaveAttribute(
    'href',
    '/system-design?document=design-lifecycle',
  )
  // Each row hands off to the task's own detail composition rather than
  // restating it, and does so at an address that can be shared (AC-2.2).
  await expect(blocked.getByRole('link', { name: 'Wire the Tasks view' })).toHaveAttribute(
    'href',
    '/tasks?task=task-blocked',
  )

  await expect(rows(page).filter({ hasText: 'Shipped web change' }).getByText('No attached context')).toBeVisible()
})

// AC-1.4: every plan-era row reports the plan outcome an operator can act on.
// An approved plan is the state a task passes through on the way to being
// implemented, so the Stage column reports the stage and stays quiet about it;
// the outcomes still waiting on someone, and the absence of a plan, keep saying
// so.
test('tasks view reports actionable plan status and stays quiet about approved plans', async ({ page }) => {
  await openTasks(page)
  await expect(
    rows(page).filter({ hasText: 'Wire the Tasks view' }).getByText('Plan awaiting approval v2'),
  ).toBeVisible()
  await expect(rows(page).filter({ hasText: 'Bounced web plan' }).getByText('Plan changes requested v1')).toBeVisible()
  await expect(rows(page).filter({ hasText: 'Shipped web change' }).getByText('No plan')).toBeVisible()

  const approved = rows(page).filter({ hasText: 'Historical anchor' })
  await expect(approved.getByText('Plan approved', { exact: false })).toHaveCount(0)
  // The stage itself is what the column is for, and it stays.
  await expect(approved.getByText('Implement', { exact: true })).toBeVisible()
})

// The one group over the list is static, so it carries no expand affordance to
// suggest a collapse that does not exist.
test('the all-tasks group header offers no expand control', async ({ page }) => {
  await openTasks(page)
  const group = page.getByText('All tasks', { exact: true }).locator('xpath=..')
  await expect(group).toBeVisible()
  await expect(group.getByRole('button')).toHaveCount(0)
  // Only the group's own list icon remains beside the label.
  await expect(group.locator('svg')).toHaveCount(1)
})

// The table is read at the width the navigation leaves beside it, so its
// columns have to hold there: header and rows stay on the same tracks, an
// overlong attached-context title stops at its own column instead of running
// under the timestamp, and nothing is cut off past the table's edge.
test('tasks table stays aligned and readable at a constrained width', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 })
  await openTasks(page)
  const table = page.getByRole('region', { name: 'Tasks table' })
  const row = rows(page).filter({ hasText: 'Wire the Tasks view' })

  // Header and row read one track definition: the Stage column starts at the
  // same offset in both.
  const headerStage = await table.getByText('Stage', { exact: true }).boundingBox()
  const rowStage = await row.getByText('Implement', { exact: true }).boundingBox()
  expect(Math.abs((headerStage?.x ?? 0) - (rowStage?.x ?? 0))).toBeLessThan(1)

  // Attached context is held inside its own column rather than overlapping the
  // timestamp beside it, and keeps the full title reachable.
  const context = row.getByRole('link', { name: 'Work-order lifecycle and MCP surface v16' })
  const contextBox = await context.boundingBox()
  const updatedBox = await row.getByText('Updated', { exact: false }).boundingBox()
  expect((contextBox?.x ?? 0) + (contextBox?.width ?? 0)).toBeLessThanOrEqual(updatedBox?.x ?? 0)
  await expect(context.getByTitle('Work-order lifecycle and MCP surface v16')).toBeVisible()

  // The Stage column holds its own widest badge rather than spilling the plan
  // outcome across the Context cell beside it.
  const planBadge = await row.getByText('Plan awaiting approval v2').boundingBox()
  expect((planBadge?.x ?? 0) + (planBadge?.width ?? 0)).toBeLessThanOrEqual(contextBox?.x ?? 0)

  // The timestamp is fully inside the table rather than clipped past its edge.
  const tableBox = await table.boundingBox()
  expect((updatedBox?.x ?? 0) + (updatedBox?.width ?? 0)).toBeLessThanOrEqual(
    (tableBox?.x ?? 0) + (tableBox?.width ?? 0),
  )
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
// it says why the task cannot move. It sits beside the
// needs-operator badge rather than replacing it: a task can hold at a gate and
// carry a stalled order at once, and a row that hides one of those misreads.
test('tasks view reports why a stalled task cannot move on its own', async ({ page }) => {
  await openTasks(page)
  const stuck = rows(page).filter({ hasText: 'Stuck conveyor change' })
  await expect(
    stuck.getByText('Stalled — dispatch is failing repeatedly: harness exited before completing work order'),
  ).toBeVisible()
  await expect(stuck.getByLabel('Needs operator')).toBeVisible()

  const healthy = rows(page).filter({ hasText: 'Shipped web change' })
  await expect(healthy.getByText('Stalled', { exact: false })).toHaveCount(0)
  await expect(healthy.getByLabel('Needs operator')).toHaveCount(0)
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

// AC-2.1: intake lives on the surface where delivery is managed. The legacy
// address and the Board entry point both hand off to that single surface.
test('tasks view is where a task is created', async ({ page }) => {
  await openTasks(page)
  await page.getByRole('link', { name: 'New task' }).click()
  await expect(page.getByRole('dialog', { name: 'New task' })).toBeVisible()
  expect(new URL(page.url()).pathname).toBe('/tasks')
  // The list it files into stays behind the sheet rather than being replaced.
  await expect(page.getByRole('heading', { name: 'Tasks' })).toBeVisible()

  await page.getByRole('button', { name: 'Close', exact: true }).click()
  await expect(page.getByRole('dialog', { name: 'New task' })).toHaveCount(0)

  await routeTasksSurface(page)
  await page.goto('/new')
  await expect(page.getByRole('dialog', { name: 'New task' })).toBeVisible()
  expect(new URL(page.url()).pathname).toBe('/tasks')

  // The board mounts the same intake composition locally instead of handing
  // off to the Tasks route.
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Board' })).toBeVisible()
  await page.getByRole('button', { name: 'New task' }).click()
  await expect(page.getByRole('dialog', { name: 'New task' })).toBeVisible()
  expect(new URL(page.url()).pathname).toBe('/')
})

// AC-2.2: a row opens the task's own detail composition beside the list, at an
// address that survives being shared, while the full route stays the deep link.
test('selecting a row opens the task detail panel with a permalink', async ({ page }) => {
  await openTasks(page)
  await page.getByRole('link', { name: 'Shipped web change' }).click()
  const panel = page.getByRole('dialog', { name: 'Task detail' })
  await expect(panel).toBeVisible()
  expect(new URL(page.url()).search).toBe('?task=task-shipped')
  // The list stays behind the panel — this is a panel on the Tasks view, not a
  // navigation away from it.
  await expect(page.getByRole('heading', { name: 'Tasks' })).toBeVisible()
  await expect(panel.getByRole('button', { name: 'Copy link to this task' })).toBeVisible()
  // Exactly the open row is announced as current — the rows all point at the
  // same path, so the search value is what distinguishes them.
  await expect(page.getByRole('list', { name: 'Tasks' }).locator('a[aria-current]')).toHaveCount(1)
  await expect(panel.getByRole('link', { name: 'Open full task page' })).toHaveAttribute(
    'href',
    '/tasks/task-shipped/full',
  )

  // Stepping through neighbours moves the panel along the list that is showing.
  await panel.getByRole('button', { name: 'Previous task' }).click()
  await expect(page).toHaveURL(/\?task=task-anchor$/)

  await panel.getByRole('button', { name: 'Close panel' }).click()
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toHaveCount(0)
  expect(new URL(page.url()).search).toBe('')

  // The panel's address is a permalink: opening it cold restores the panel.
  await routeTasksSurface(page)
  await page.goto('/tasks?task=task-bounced')
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toBeVisible()

  // …and the full route remains reachable for the deep links that already
  // point at it.
  await page.goto('/tasks/task-bounced/full')
  await expect(page.getByRole('link', { name: 'Back to board' })).toBeVisible()
  expect(new URL(page.url()).pathname).toBe('/tasks/task-bounced/full')
})

// A blueprint anchor keeps its one canonical home. The panel
// hosts the task's own detail composition rather than a copy of it, so the rule
// arrives with the composition instead of being restated on this surface.
test('an anchor opened in the panel is sent to its canonical blueprint route', async ({ page }) => {
  await routeTasksSurface(page)
  await page.route('**/v1/tasks/task-anchor/activity**', (route) =>
    route.fulfill({
      json: {
        task: {
          id: 'task-anchor',
          workspace: 'demo',
          title: 'Historical anchor',
          state: 'running',
          children: [{ id: 'child-one', title: 'A child task', state: 'merged' }],
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
  await page.route('**/v1/blueprints**', (route) => route.fulfill({ json: [] }))
  await page.goto('/tasks?task=task-anchor')
  await expect(page).toHaveURL(/\/blueprints\/task-anchor$/)
})

// AC-2.3: the list pages through the server rather than loading the workspace.
test('tasks view pages through server-side results', async ({ page }) => {
  await openTasks(page)
  const requests: string[] = []
  page.on('request', (request) => {
    if (request.url().includes('/v1/task-operations')) requests.push(request.url())
  })
  // The fixture is smaller than a page, so pagination is exercised by asking
  // the server for a page it must honour.
  await page.route('**/v1/task-operations?**', (route) => {
    const params = new URL(route.request().url()).searchParams
    const matched = matchOperations(route.request().url())
    const offset = Number(params.get('offset') ?? '0')
    return route.fulfill({
      body: JSON.stringify(matched.slice(offset, offset + 2)),
      headers: {
        'content-type': 'application/json',
        'X-Conveyor-Total': String(matched.length),
        'X-Conveyor-Limit': '2',
        'X-Conveyor-Offset': String(offset),
      },
    })
  })
  await page.reload()
  await expect(rows(page)).toHaveCount(2)
  await expect(page.getByText('1–2 of 5')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Previous' })).toBeDisabled()

  await page.getByRole('button', { name: 'Next' }).click()
  await expect(page.getByText('3–4 of 5')).toBeVisible()
  await expect(rows(page).filter({ hasText: 'Shipped web change' })).toHaveCount(1)
  // Every page came from the server with its own offset: nothing was sliced in
  // the browser out of one whole-workspace read.
  expect(requests.some((url) => url.includes('offset=2'))).toBe(true)
})

// AC-2.4: the shared filter family — created-at range, served requirement, and
// governing design — is applied by the server on the Tasks surface.
test('tasks view filters by created-at range, served requirement, and governing design', async ({ page }) => {
  await openTasks(page)
  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Requirement' }).click()
  await page.getByRole('option', { name: 'Task-centric operations view' }).click()
  await expect(rows(page)).toHaveCount(1)
  await expect(page.getByRole('link', { name: 'Wire the Tasks view' })).toBeVisible()

  // An unconfirmed document is not an option: only confirmed documents can be
  // attached to a task, so only confirmed documents can narrow the list.
  await expect(page.getByRole('option', { name: 'Unconfirmed idea' })).toHaveCount(0)

  await page.getByRole('option', { name: 'Any requirement' }).click()
  await page.getByRole('tab', { name: 'System design' }).click()
  await page.getByRole('option', { name: 'Work-order lifecycle' }).click()
  await expect(rows(page)).toHaveCount(1)

  await page.getByRole('option', { name: 'Any system design' }).click()
  await page.getByRole('tab', { name: 'Created' }).click()
  await page.getByRole('option', { name: 'Custom range' }).click()
  // Fixed dates rather than a preset, so the assertion does not depend on when
  // the suite runs. The end date is inclusive of its own day.
  await page.getByLabel('Created from').fill('2026-08-04')
  await page.getByLabel('Created to').fill('2026-08-04')
  await expect(rows(page)).toHaveCount(1)
  await expect(page.getByRole('link', { name: 'Shipped web change' })).toBeVisible()

  await page.getByLabel('Created to').fill('2026-08-03')
  await expect(page.getByText('Choose an end date on or after the start date.')).toBeVisible()

  await page.getByRole('button', { name: 'Reset filters' }).click()
  await expect(rows(page)).toHaveCount(5)
})
