import { expect, test, type Page, type Route } from '@playwright/test'

const createdAt = '2026-07-15T12:00:00Z'

function activity(taskId: string, overflowing: boolean) {
  return {
    task: {
      id: taskId,
      workspace: 'demo',
      source: 'mcp',
      title: overflowing ? 'Overflowing task' : 'Short task',
      body: overflowing ? Array.from({ length: 80 }, (_, index) => `Description line ${index + 1}`).join('\n') : 'A short description.',
      class: 'bug',
      level: 'L2',
      repo: 'conveyor',
      base_branch: 'main',
      branch: `conveyor/task-${taskId}`,
      state: 'running',
      next_stage: 'implement',
      created_at: createdAt,
    },
    jobs: [],
    events: [],
    interventions: [],
    checkout_available: false,
    checkout_guidance: 'Use the assigned worktree.',
    needs_attention: false,
    spec: {
      task_id: taskId,
      version: 1,
      content: '## Specification\n\nRegression marker at the bottom of the task content.',
      acceptance_count: 0,
      acceptance: [],
      decomposition: [],
      approved: true,
      created_at: createdAt,
      approved_at: createdAt,
    },
    work_orders: [],
  }
}

async function mockTaskAPIs(page: Page) {
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route: Route) => {
    const url = new URL(route.request().url())
    const taskMatch = url.pathname.match(/^\/v1\/tasks\/([^/]+)\/activity$/)
    if (taskMatch) {
      const taskId = decodeURIComponent(taskMatch[1])
      await route.fulfill({ json: activity(taskId, taskId === 'overflowing') })
      return
    }
    if (url.pathname.endsWith('/events/stream')) {
      await route.fulfill({ status: 204 })
      return
    }
    await route.fulfill({ json: [] })
  })
}

test.beforeEach(async ({ page }) => {
  await mockTaskAPIs(page)
})

test('overflowing full-screen task content scrolls from top to bottom', async ({ page }) => {
  await page.goto('/tasks/overflowing/full')

  const content = page.getByRole('region', { name: 'Task content' })
  const marker = page.getByText('Regression marker at the bottom of the task content.')
  await expect(content).toBeVisible()

  const initial = await content.evaluate((element) => ({
    clientHeight: element.clientHeight,
    overflowY: getComputedStyle(element).overflowY,
    scrollHeight: element.scrollHeight,
    scrollTop: element.scrollTop,
    touchAction: getComputedStyle(element).touchAction,
  }))
  expect(initial.overflowY).toBe('auto')
  expect(initial.scrollHeight).toBeGreaterThan(initial.clientHeight)
  expect(initial.scrollTop).toBe(0)
  expect(initial.touchAction).not.toBe('none')
  await expect(marker).not.toBeInViewport()

  await content.hover()
  await page.mouse.wheel(0, 500)
  await expect.poll(() => content.evaluate((element) => element.scrollTop)).toBeGreaterThan(0)

  await content.focus()
  await page.keyboard.press('End')
  await expect(marker).toBeInViewport()
  await expect.poll(() => content.evaluate((element) => element.scrollTop + element.clientHeight)).toBeGreaterThanOrEqual(
    await content.evaluate((element) => element.scrollHeight - 1),
  )
})

test('short full-screen task content does not create a nested vertical scrollbar', async ({ page }) => {
  await page.goto('/tasks/short/full')

  const content = page.getByRole('region', { name: 'Task content' })
  await expect(content).toBeVisible()
  const dimensions = await content.evaluate((element) => ({
    clientHeight: element.clientHeight,
    scrollHeight: element.scrollHeight,
  }))
  expect(dimensions.scrollHeight).toBeLessThanOrEqual(dimensions.clientHeight)
})
