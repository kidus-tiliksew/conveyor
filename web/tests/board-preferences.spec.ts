import { expect, type Page, test } from '@playwright/test'
import type { TaskFlow } from '../src/lib/types'
import { callerIdentity } from './helpers/caller-identity'

// The Board preferences (component-web-dashboard, Board preferences;
// req-operator-next-action AC-3.3). "Show stage progress on cards" is a
// per-browser choice: off by default, kept across reloads and workspaces,
// never sent to the server, and shared by every page of this browser.

const at = '2026-10-15T09:00:00Z'
const workspaces = [
  { id: 'demo', name: 'Demo' },
  { id: 'other', name: 'Other' },
]

function activity(workspace: string) {
  return [
    {
      task: {
        id: `${workspace}-task`,
        workspace,
        title: `Card in ${workspace}`,
        repo: 'conveyor',
        branch: `conveyor/${workspace}-task`,
        state: 'running',
        created_at: at,
      },
      latest_stage: 'implement',
      last_event_at: at,
      needs_attention: false,
      next_action: {
        label: 'Working',
        actor: 'agent',
        kind: 'working',
        judgment: false,
        summary: 'Working.',
        deadline: null,
      },
    },
  ]
}

const flow: TaskFlow = {
  stages: (['triage', 'spec', 'implement', 'verify', 'review', 'merge'] as const).map((id, index) => ({
    id,
    gate: id === 'spec' ? 'plan_approval' : null,
    status: index < 2 ? 'done' : index === 2 ? 'in_progress' : 'upcoming',
    description: '',
    status_label: 'label',
    status_text: '',
    missed_check_in: '',
    attempts: [],
    current_attempt: null,
  })),
  loops: [],
  side: { blocked_by: [], retrying: null, parked: false, closed: false, agent_question: false },
  next_action: { label: 'Working', actor: 'agent', kind: 'working', summary: 'Working.' },
  raw: { state: 'running', next_stage: 'implement', recovery_stage: '', gate_kind: '', order_ids: [] },
}

async function mockAPI(page: Page, { stored }: { stored?: string } = {}) {
  const writes: string[] = []
  await page.addInitScript((value) => {
    if (sessionStorage.getItem('seeded')) return
    sessionStorage.setItem('seeded', 'true')
    localStorage.setItem('conveyor-workspace', 'demo')
    if (value !== undefined) localStorage.setItem('conveyor-show-stage-progress', value)
  }, stored)
  await page.route('**/v1/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (request.method() !== 'GET') writes.push(`${request.method()} ${path}`)
    if (path === '/v1/workspaces') return route.fulfill({ json: workspaces })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'operator', email: 'operator@example.test', role: 'operator' }, route.request()),
      })
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: url.searchParams.get('workspace_id') ?? 'demo', repos: [] } })
    if (path === '/v1/tokens') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    if (/^\/v1\/tasks\/[^/]+\/flow$/.test(path)) return route.fulfill({ json: { flow } })
    if (path === '/v1/activity') {
      const items = activity(url.searchParams.get('workspace_id') ?? 'demo')
      return route.fulfill({
        json: items,
        headers: { 'X-Conveyor-Total': '1', 'X-Conveyor-Limit': '100', 'X-Conveyor-Offset': '0' },
      })
    }
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
    return route.fulfill({ json: [] })
  })
  return writes
}

const toggle = (page: Page) => page.getByRole('switch', { name: 'Show stage progress on cards' })

test('the stage progress switch is off by default and persists across reloads without a server write', async ({
  page,
}) => {
  const writes = await mockAPI(page)
  await page.goto('/w/demo/settings')
  await expect(page.getByRole('heading', { name: 'Board' })).toBeVisible()
  await expect(toggle(page)).toHaveAttribute('aria-checked', 'false')
  await toggle(page).click()
  await expect(toggle(page)).toHaveAttribute('aria-checked', 'true')
  expect(await page.evaluate(() => localStorage.getItem('conveyor-show-stage-progress'))).toBe('true')
  await page.reload()
  await expect(toggle(page)).toHaveAttribute('aria-checked', 'true')
  await toggle(page).click()
  await page.reload()
  await expect(toggle(page)).toHaveAttribute('aria-checked', 'false')
  expect(writes).toEqual([])
})

test('a malformed stored value reads as off', async ({ page }) => {
  await mockAPI(page, { stored: '1' })
  await page.goto('/w/demo/settings')
  await expect(toggle(page)).toHaveAttribute('aria-checked', 'false')
})

test('the preference holds across workspaces', async ({ page }) => {
  await mockAPI(page, { stored: 'true' })
  await page.goto('/')
  await expect(page.getByRole('link', { name: /^Card in demo/ }).locator('[data-stage-stripe]')).toBeVisible()
  await page.getByRole('button', { name: 'Switch to Other' }).click()
  const other = page.getByRole('link', { name: /^Card in other/ })
  await expect(other.locator('[data-stage-stripe]')).toBeVisible()
  await expect(other.locator('[data-stage]')).toHaveCount(6)
})

test('turning the switch on updates the open Board in the same page', async ({ page }) => {
  await mockAPI(page)
  await page.goto('/w/demo/settings')
  await toggle(page).click()
  await page.getByRole('navigation', { name: 'Primary' }).getByRole('link', { name: 'Board' }).click()
  await expect(page.getByRole('link', { name: /^Card in demo/ }).locator('[data-stage]')).toHaveCount(6)
})

test('a change in another tab reaches an open Board', async ({ context }) => {
  const board = await context.newPage()
  await mockAPI(board)
  await board.goto('/')
  const card = board.getByRole('link', { name: /^Card in demo/ })
  await expect(card).toBeVisible()
  await expect(card.locator('[data-stage-stripe]')).toHaveCount(0)

  const settings = await context.newPage()
  await mockAPI(settings)
  await settings.goto('/w/demo/settings')
  await toggle(settings).click()
  await expect(card.locator('[data-stage]')).toHaveCount(6)
  await toggle(settings).click()
  await expect(card.locator('[data-stage-stripe]')).toHaveCount(0)
})
