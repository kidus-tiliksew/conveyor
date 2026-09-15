import { expect, type Locator, type Page, test } from '@playwright/test'

const timestamp = '2026-09-14T11:00:00Z'
const reason = 'automatic retry is suppressed'
const task = {
  id: 'touch-task',
  workspace: 'demo',
  title: 'Read task details on touch',
  state: 'running',
  repo: 'conveyor',
  branch: 'conveyor/touch-task',
  assignee: { user_id: 'usr-viewer', display_name: 'Viewer User', email: 'viewer@example.test' },
  created_at: timestamp,
  context: {
    requirements: [
      { id: 'req-old', title: 'Archived outcome', version: 1, archived: true, superseded_by: ['req-new'] },
    ],
    designs: [
      {
        id: 'design-old',
        title: 'Archived guidance',
        version: 1,
        archived: true,
        superseded_by: ['design-new', 'design-replacement-with-a-long-identifier'],
      },
    ],
  },
}
const activity = {
  task,
  jobs: [],
  events: [],
  work_orders: [],
  reviews: [],
  interventions: [],
  latest_stage: 'implement',
  plan: { state: 'approved', version: 1 },
  last_event_at: timestamp,
  stalled: { needed: true, reason },
  needs_attention: true,
}
const designVersion = {
  document_id: 'design-old',
  version: 1,
  confirmed: true,
  content: '# Archived guidance',
  created_at: timestamp,
  origin: 'operator',
  governs: [],
}
const design = {
  document: {
    id: 'design-old',
    title: 'Archived guidance',
    category: 'Architecture',
    current_version: 1,
    archived: true,
    superseded_by: ['design-new'],
    created_at: timestamp,
    updated_at: timestamp,
  },
  current_version: designVersion,
  pending_versions: [],
  versions: [designVersion],
  drift: [],
  lineage: [],
  lineage_total: 0,
}

async function mockAPI(page: Page) {
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', (route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({ json: { id: 'viewer', email: 'viewer@example.test', role: 'viewer' } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 1, task_count: 1, pending_proposal_count: 0 } } })
    if (path === '/v1/tasks/touch-task/activity') return route.fulfill({ json: activity })
    if (path.endsWith('/stream')) return route.fulfill({ contentType: 'text/event-stream', body: '' })
    if (path === '/v1/activity' || path === '/v1/task-operations')
      return route.fulfill({
        json: [activity],
        headers: { 'X-Conveyor-Total': '1', 'X-Conveyor-Limit': '50', 'X-Conveyor-Offset': '0' },
      })
    if (path === '/v1/system-designs') return route.fulfill({ json: [design] })
    if (path === '/v1/system-designs/design-old') return route.fulfill({ json: design })
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
    return route.fulfill({ json: [] })
  })
}

async function contentFor(page: Page, trigger: Locator) {
  const id = await trigger.getAttribute('aria-describedby')
  expect(id).toBeTruthy()
  return page.locator(`[id="${id}"]`)
}

async function expectTouchTarget(trigger: Locator) {
  const bounds = await trigger.boundingBox()
  expect(bounds?.width).toBeGreaterThanOrEqual(40)
  expect(bounds?.height).toBeGreaterThanOrEqual(40)
}

const surfaces = [
  { name: 'assignee detail', route: '/tasks', trigger: 'Viewer User', index: 0, detail: 'usr-viewer' },
  { name: 'archived outcome', route: '/tasks/touch-task/full', trigger: 'Archived', index: 0, detail: 'req-new' },
  { name: 'archived guidance', route: '/tasks/touch-task/full', trigger: 'Archived', index: 1, detail: 'design-new' },
  {
    name: 'stalled status',
    route: '/tasks/touch-task/full',
    trigger: 'Task status: Stalled',
    index: 0,
    detail: reason,
  },
  { name: 'updated timestamp', route: '/tasks', trigger: /^Updated /, index: 0, detail: '9/14/2026, 11:00:00 AM' },
]

test.describe('touch disclosures', () => {
  test.use({
    viewport: { width: 390, height: 844 },
    isMobile: true,
    hasTouch: true,
    locale: 'en-US',
    timezoneId: 'UTC',
  })

  for (const surface of surfaces) {
    test(`${surface.name} opens by tap and supports all dismissals`, async ({ page }) => {
      await mockAPI(page)
      await page.goto(surface.route)
      const trigger = page.getByRole('button', { name: surface.trigger, exact: true }).nth(surface.index)
      const content = await contentFor(page, trigger)
      await expect(trigger).toHaveAttribute('aria-expanded', 'false')
      await expectTouchTarget(trigger)
      await trigger.tap()
      await expect(trigger).toHaveAttribute('aria-expanded', 'true')
      await expect(content).toBeVisible()
      await expect(content).toContainText(surface.detail)
      const bounds = await content.boundingBox()
      expect(bounds?.x).toBeGreaterThanOrEqual(0)
      expect((bounds?.x ?? 0) + (bounds?.width ?? 0)).toBeLessThanOrEqual(390)
      await trigger.tap()
      await expect(content).toBeHidden()
      await expect(trigger).toHaveAttribute('aria-expanded', 'false')
      await trigger.tap()
      await page.keyboard.press('Escape')
      await expect(content).toBeHidden()
      await trigger.tap()
      await page.getByRole('heading', { name: surface.route === '/tasks' ? 'Tasks' : task.title, exact: true }).tap()
      await expect(content).toBeHidden()
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390)
    })
  }

  test('assignee details do not navigate the containing board card', async ({ page }) => {
    await mockAPI(page)
    await page.goto('/')
    const trigger = page.getByRole('button', { name: 'Viewer User', exact: true })
    await trigger.tap()
    await expect(await contentFor(page, trigger)).toContainText('usr-viewer')
    await expect(page).toHaveURL(/\/$/)
    await expect(page.getByRole('dialog', { name: 'Task detail' })).toBeHidden()
  })

  test('successor links remain actionable by tap', async ({ page }) => {
    await mockAPI(page)
    await page.goto('/tasks/touch-task/full')
    await page.getByRole('button', { name: 'Archived', exact: true }).first().tap()
    await page.getByRole('link', { name: 'req-new', exact: true }).tap()
    await expect(page).toHaveURL(/requirements\?requirement=req-new/)
  })

  test('Escape dismisses the disclosure before its task sheet', async ({ page }) => {
    await mockAPI(page)
    await page.goto('/tasks/touch-task')
    const sheet = page.getByRole('dialog', { name: 'Task detail' })
    const trigger = sheet.getByRole('button', { name: 'Task status: Stalled' })
    const content = await contentFor(page, trigger)
    await trigger.tap()
    await page.keyboard.press('Escape')
    await expect(content).toBeHidden()
    await expect(sheet).toBeVisible()
    for (const name of ['Previous task', 'Next task', 'Close panel']) {
      await expectTouchTarget(sheet.getByRole('button', { name }))
    }
    await page.keyboard.press('Escape')
    await expect(sheet).toBeHidden()
  })

  test('document icon reveals successors without closing the drawer', async ({ page }) => {
    await mockAPI(page)
    await page.goto('/system-design?document=design-old')
    await page.getByRole('button', { name: 'Documents', exact: true }).tap()
    const drawer = page.getByRole('dialog', { name: 'Document tree' })
    await drawer.locator('summary').filter({ hasText: 'Archived' }).tap()
    const trigger = drawer.getByRole('button', { name: 'Document details' })
    await expectTouchTarget(trigger)
    await trigger.tap()
    const content = await contentFor(page, trigger)
    await expect(content.getByRole('link', { name: 'design-new' })).toBeVisible()
    await page.keyboard.press('Escape')
    await expect(content).toBeHidden()
    await expect(drawer).toBeVisible()
    await drawer
      .getByRole('button', { name: /Archived guidance/, exact: false })
      .first()
      .tap()
    await expect(drawer).toBeHidden()
  })
})

test.describe('desktop disclosures', () => {
  test.use({ locale: 'en-US', timezoneId: 'UTC' })

  for (const surface of surfaces) {
    test(`${surface.name} still opens with hover and keyboard focus`, async ({ page }) => {
      await mockAPI(page)
      await page.goto(surface.route)
      const trigger = page.getByRole('button', { name: surface.trigger, exact: true }).nth(surface.index)
      const content = await contentFor(page, trigger)
      await trigger.hover()
      await expect(content).toBeVisible()
      await expect(content).toContainText(surface.detail)
      await page.mouse.move(0, 0)
      await expect(content).toBeHidden()
      await trigger.focus()
      await expect(content).toBeVisible()
      await page.keyboard.press('Escape')
      await expect(content).toBeHidden()
      await trigger.press('Enter')
      await expect(content).toBeVisible()
      await trigger.press('Enter')
      await expect(content).toBeHidden()
    })
  }
})
