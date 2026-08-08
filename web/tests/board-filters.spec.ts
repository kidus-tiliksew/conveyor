import { expect, test, type Page } from '@playwright/test'

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
  await page.route('**/v1/task-operations?**', (route) => route.fulfill({ json: [] }))
  await page.route('**/v1/activity?**', (route) => {
    const url = route.request().url()
    seen.push(url)
    const params = new URL(url).searchParams
    const from = params.get('updated_from') ?? ''
    const needle = (params.get('q') ?? '').toLowerCase()
    // List members repeat their parameter and mean "any of" (AC-2.4).
    const repositories = params.getAll('repository')
    return route.fulfill({
      json: activity.filter((item) => {
        if (from && (item.last_event_at || item.task.created_at) < from) return false
        if (repositories.length && !repositories.includes(item.task.repo)) return false
        return !needle || item.task.title.toLowerCase().includes(needle)
      }),
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
  await expect(page.getByRole('heading', { name: 'Board' })).toBeVisible()
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

test('board opens on the last month and remembers the operator adjustment per workspace', async ({ page }) => {
  const seen: string[] = []
  await openBoard(page, seen)

  // The default is a starting point the operator can see the effect of: the
  // board asks the server for recent activity, and the ancient task is gone.
  await expect(cards(page).filter({ hasText: 'Recent conveyor change' })).toHaveCount(1)
  await expect(cards(page).filter({ hasText: 'Ancient web change' })).toHaveCount(0)
  // The bound went to the server rather than being applied to a fully-loaded
  // workspace. The rail's attention badge speaks for the whole workspace and
  // keeps reading the unfiltered feed, so this is `some`, not `every`.
  expect(seen.some((url) => url.includes('updated_from='))).toBe(true)

  // Adjusting it is what gets remembered — including across a reload.
  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Updated' }).click()
  await expect(page.getByRole('option', { name: 'Last month' })).toHaveAttribute('aria-selected', 'true')
  await page.getByRole('option', { name: 'Any time' }).click()
  await expect(cards(page).filter({ hasText: 'Ancient web change' })).toHaveCount(1)
  await page.reload()
  await expect(cards(page).filter({ hasText: 'Ancient web change' })).toHaveCount(1)
  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Updated' }).click()
  await expect(page.getByRole('option', { name: 'Any time' })).toHaveAttribute('aria-selected', 'true')

  // Persistence is scoped to the workspace it was set in, so another workspace
  // opens on its own default rather than inheriting a repository filter or a
  // window from a workspace it has nothing in common with.
  expect(await page.evaluate(() => localStorage.getItem('conveyor-task-filters:board:demo'))).toContain(
    '"updated":"any"',
  )
  await page.evaluate(() => localStorage.setItem('conveyor-workspace', 'other'))
  await page.reload()
  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Updated' }).click()
  await expect(page.getByRole('option', { name: 'Last month' })).toHaveAttribute('aria-selected', 'true')
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

  // AC-1.5 stands on this surface too: no barred field is offered as a filter.
  await expect(page.getByRole('tab', { name: 'Priority' })).toHaveCount(0)
  await expect(page.getByRole('tab', { name: 'Assignee' })).toHaveCount(0)
})

test('board no longer offers task creation', async ({ page }) => {
  const seen: string[] = []
  await openBoard(page, seen)
  await expect(page.getByRole('link', { name: 'New task' })).toHaveCount(0)
})
